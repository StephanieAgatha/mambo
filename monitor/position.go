package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	hyperliquid "github.com/sonirico/go-hyperliquid"

	"mambo/ai"
	"mambo/config"
	"mambo/exchange"
	"mambo/journal"
	"mambo/market"
	"mambo/ta"
)

const (
	// maxHoldDuration: force close after 240 minutes (1 trading session)
	maxHoldDuration = 240 * time.Minute

	// maxWaitFillDuration: max time to wait for limit order to fill before giving up
	maxWaitFillDuration = 120 * time.Minute

	// fillPollInterval: how often to check if position has filled
	fillPollInterval = 15 * time.Second

	// smartLossCutDuration: how long a position must be losing before smart cut kicks in
	smartLossCutDuration = 30 * time.Minute

	// smartLossCutThreshold: minimum loss % to trigger smart loss cut
	smartLossCutThreshold = 1.0

	// drawdownProtectionPeak: minimum peak PnL% before drawdown protection activates
	drawdownProtectionPeak = 5.0

	// drawdownProtectionDrop: drop from peak % that triggers force close
	drawdownProtectionDrop = 40.0
)

// CloseReason documents why a position was closed.
type CloseReason string

const (
	CloseReasonTP       CloseReason = "TAKE_PROFIT"
	CloseReasonSL       CloseReason = "STOP_LOSS"
	CloseReasonAI       CloseReason = "AI_CLOSE"
	CloseReasonDrawdown CloseReason = "HARD_RULE_DRAWDOWN"
	CloseReasonTimeout  CloseReason = "HARD_RULE_TIMEOUT"
	CloseReasonSmartCut CloseReason = "HARD_RULE_SMART_LOSS_CUT"
)

// OnCloseFn is called whenever a position is closed.
// Wired to Discord notify in Phase G.
type OnCloseFn func(pos journal.OpenPosition, exitPrice float64, reason CloseReason, pnlUSD, pnlPct float64)

// OnUpdateFn is called when AI adjusts SL or TP.
// Wired to Discord notify in Phase G.
type OnUpdateFn func(pos journal.OpenPosition, action string, newSL, newTP float64, reasoning string)

// Monitor watches open positions and manages them on two separate intervals.
type Monitor struct {
	exClient *exchange.Client
	fetcher  *market.Fetcher
	posMgr   *ai.PositionManager
	jl       *journal.Logger
	cfg      *config.Config
	onClose  OnCloseFn
	onUpdate OnUpdateFn
}

// New creates a Monitor with all dependencies injected.
func New(
	exClient *exchange.Client,
	fetcher *market.Fetcher,
	posMgr *ai.PositionManager,
	jl *journal.Logger,
	cfg *config.Config,
	onClose OnCloseFn,
	onUpdate OnUpdateFn,
) *Monitor {
	return &Monitor{
		exClient: exClient,
		fetcher:  fetcher,
		posMgr:   posMgr,
		jl:       jl,
		cfg:      cfg,
		onClose:  onClose,
		onUpdate: onUpdate,
	}
}

// Start spawns a goroutine that monitors a single position until closed or ctx cancelled.
func (m *Monitor) Start(ctx context.Context, pos journal.OpenPosition) {
	go m.watch(ctx, pos)
}

// watch is the monitoring loop for a single position.
// Two separate tickers:
//   - priceTicker: cfg.MonitorPriceSec  → check price + hard rules (fast)
//   - aiTicker   : cfg.MonitorAISec     → call Grok for analysis (slower, configurable)
//
// Before entering the loop, waits for the position to actually fill on the exchange.
func (m *Monitor) watch(ctx context.Context, pos journal.OpenPosition) {
	slog.Info("monitor: waiting for position to fill",
		"pos_id", pos.ID,
		"pair", pos.Pair,
		"direction", pos.Direction,
		"entry", pos.EntryPrice,
		"size_usd", pos.SizeUSD,
	)

	filled := m.waitForFill(ctx, pos)
	if !filled {
		// position was placed on startup but not yet filled
		// clean up the position record since order didn't fill
		slog.Warn("monitor: position fill timed out — cleaning up",
			"pos_id", pos.ID, "pair", pos.Pair)
		// IMPORTANT: Do NOT log this as a trade — order never filled
		if err := m.jl.RemovePosition(pos.ID); err != nil {
			slog.Error("monitor: remove unfilled position failed", "pos_id", pos.ID, "err", err)
		}
		return
	}

	// Position is now filled — ready for monitoring
	pos.Status = "filled"
	if err := m.jl.SavePosition(pos); err != nil {
		slog.Warn("monitor: update position status to filled failed", "pos_id", pos.ID, "err", err)
	}

	// Place TP/SL trigger orders now that position exists on exchange
	coinSize := pos.SizeUSD / pos.EntryPrice
	m.placeTPandSL(ctx, pos, coinSize)

	slog.Info("monitor: position confirmed filled — starting monitoring",
		"pos_id", pos.ID,
		"pair", pos.Pair,
		"entry", pos.EntryPrice,
	)

	priceTicker := time.NewTicker(time.Duration(m.cfg.MonitorPriceSec) * time.Second)
	aiTicker := time.NewTicker(time.Duration(m.cfg.MonitorAISec) * time.Second)
	defer priceTicker.Stop()
	defer aiTicker.Stop()

	slog.Info("monitor started",
		"pos_id", pos.ID,
		"pair", pos.Pair,
		"direction", pos.Direction,
		"entry", pos.EntryPrice,
		"sl", pos.StopLoss,
		"tp", pos.TakeProfit,
		"price_check_sec", m.cfg.MonitorPriceSec,
		"ai_check_sec", m.cfg.MonitorAISec,
	)

	// track when position started losing for smart loss cut
	losingStart := time.Time{}

	for {
		select {
		case <-ctx.Done():
			slog.Info("monitor stopped — context cancelled", "pos_id", pos.ID)
			return

		// ── Fast tick: price check + hard rules ───────────────────────────────
		case <-priceTicker.C:
			currentPrice, err := m.exClient.FetchCurrentPrice(ctx, pos.Pair)
			if err != nil {
				// non-fatal — retry next tick
				slog.Warn("monitor: fetch price failed — retrying",
					"pos_id", pos.ID,
					"pair", pos.Pair,
					"err", err,
				)
				continue
			}

			pnlPct := calcPnLPct(pos.Direction, pos.EntryPrice, currentPrice)
			pnlUSD := pos.SizeUSD * (pnlPct / 100)

			slog.Debug("monitor price tick",
				"pos_id", pos.ID,
				"pair", pos.Pair,
				"price", currentPrice,
				"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
			)

			// update peak PnL if new high
			if pnlPct > pos.PeakPnLPct {
				pos.PeakPnLPct = pnlPct
				if err := m.jl.UpdatePeakPnL(pos.ID, pnlPct); err != nil {
					slog.Warn("monitor: update peak PnL failed",
						"pos_id", pos.ID, "err", err)
				}
			}

			// hard rule: TP hit
			if hitTP(pos.Direction, currentPrice, pos.TakeProfit) {
				m.closePosition(ctx, pos, currentPrice, CloseReasonTP, pnlUSD, pnlPct)
				return
			}

			// hard rule: SL hit
			if hitSL(pos.Direction, currentPrice, pos.StopLoss) {
				m.closePosition(ctx, pos, currentPrice, CloseReasonSL, pnlUSD, pnlPct)
				return
			}

			// hard rule: drawdown protection
			// peak ≥ +5% AND current dropped ≥ 40% from peak → force close
			if pos.PeakPnLPct >= drawdownProtectionPeak {
				dropFromPeak := (pos.PeakPnLPct - pnlPct) / pos.PeakPnLPct * 100
				if dropFromPeak >= drawdownProtectionDrop {
					slog.Warn("monitor: drawdown protection triggered",
						"pos_id", pos.ID,
						"peak_pnl", pos.PeakPnLPct,
						"current_pnl", pnlPct,
						"drop_from_peak_pct", fmt.Sprintf("%.1f%%", dropFromPeak),
					)
					m.closePosition(ctx, pos, currentPrice, CloseReasonDrawdown, pnlUSD, pnlPct)
					return
				}
			}

			// hard rule: max hold duration (240 min)
			openedAt, _ := time.Parse(time.RFC3339, pos.OpenedAt)
			holdDuration := time.Since(openedAt)
			if holdDuration >= maxHoldDuration {
				slog.Warn("monitor: max hold duration reached",
					"pos_id", pos.ID,
					"hold_min", fmt.Sprintf("%.0f", holdDuration.Minutes()),
				)
				m.closePosition(ctx, pos, currentPrice, CloseReasonTimeout, pnlUSD, pnlPct)
				return
			}

			// hard rule: smart loss cut
			// losing > 30 min continuously AND loss > 1%
			if pnlPct < 0 {
				if losingStart.IsZero() {
					losingStart = time.Now()
				}
				if time.Since(losingStart) >= smartLossCutDuration &&
					pnlPct < -smartLossCutThreshold {
					slog.Warn("monitor: smart loss cut triggered",
						"pos_id", pos.ID,
						"losing_min", fmt.Sprintf("%.0f", time.Since(losingStart).Minutes()),
						"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
					)
					m.closePosition(ctx, pos, currentPrice, CloseReasonSmartCut, pnlUSD, pnlPct)
					return
				}
			} else {
				// position recovered — reset losing timer
				losingStart = time.Time{}
			}

		// ── Slow tick: AI position analysis ───────────────────────────────────
		case <-aiTicker.C:
			currentPrice, err := m.exClient.FetchCurrentPrice(ctx, pos.Pair)
			if err != nil {
				slog.Warn("monitor: fetch price for AI failed — skipping",
					"pos_id", pos.ID, "err", err)
				continue
			}

			pnlPct := calcPnLPct(pos.Direction, pos.EntryPrice, currentPrice)
			pnlUSD := pos.SizeUSD * (pnlPct / 100)

			slog.Info("monitor: calling Grok for position analysis",
				"pos_id", pos.ID,
				"pair", pos.Pair,
				"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
				"interval_min", m.cfg.MonitorAISec/60,
			)

			taSnap, err := m.fetchTASnapshot(ctx, pos.Pair)
			if err != nil {
				slog.Warn("monitor: TA snapshot failed — skipping AI decision",
					"pos_id", pos.ID, "err", err)
				continue
			}

			decision, err := m.posMgr.Decide(ctx, pos, currentPrice, pnlPct, taSnap)
			if err != nil {
				slog.Warn("monitor: AI decision failed — holding",
					"pos_id", pos.ID, "err", err)
				continue
			}

			switch decision.Action {
			case "close":
				slog.Info("monitor: AI decided to close",
					"pos_id", pos.ID,
					"reasoning", decision.Reasoning,
				)
				m.closePosition(ctx, pos, currentPrice, CloseReasonAI, pnlUSD, pnlPct)
				return

			case "move_sl":
				if decision.NewSL > 0 {
					pos.StopLoss = decision.NewSL
					if err := m.jl.SavePosition(pos); err != nil {
						slog.Warn("monitor: save after SL update failed",
							"pos_id", pos.ID, "err", err)
					}
					slog.Info("monitor: SL moved by AI",
						"pos_id", pos.ID,
						"new_sl", decision.NewSL,
						"reasoning", decision.Reasoning,
					)
					if m.onUpdate != nil {
						m.onUpdate(pos, "move_sl", decision.NewSL, 0, decision.Reasoning)
					}
				}

			case "move_tp":
				if decision.NewTP > 0 {
					pos.TakeProfit = decision.NewTP
					if err := m.jl.SavePosition(pos); err != nil {
						slog.Warn("monitor: save after TP update failed",
							"pos_id", pos.ID, "err", err)
					}
					slog.Info("monitor: TP moved by AI",
						"pos_id", pos.ID,
						"new_tp", decision.NewTP,
						"reasoning", decision.Reasoning,
					)
					if m.onUpdate != nil {
						m.onUpdate(pos, "move_tp", 0, decision.NewTP, decision.Reasoning)
					}
				}

			case "hold":
				slog.Info("monitor: AI holding position",
					"pos_id", pos.ID,
					"reasoning", decision.Reasoning,
				)
			}
		}
	}
}

// closePosition executes close order, logs trade, and fires onClose callback.
func (m *Monitor) closePosition(
	ctx context.Context,
	pos journal.OpenPosition,
	exitPrice float64,
	reason CloseReason,
	pnlUSD, pnlPct float64,
) {
	slog.Info("monitor: closing position",
		"pos_id", pos.ID,
		"pair", pos.Pair,
		"reason", reason,
		"exit_price", exitPrice,
		"pnl_usd", fmt.Sprintf("%.2f", pnlUSD),
		"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
	)

	if err := m.exClient.ClosePosition(ctx, pos.Pair, pos.Direction); err != nil {
		// log error but still update journal — position may have closed on exchange
		slog.Error("monitor: close order failed",
			"pos_id", pos.ID, "pair", pos.Pair, "err", err)
	}

	result := "WIN"
	if pnlUSD < 0 {
		result = "LOSS"
	}

	trade := journal.ClosedTrade{
		ID:          pos.ID,
		Pair:        pos.Pair,
		Direction:   pos.Direction,
		EntryPrice:  pos.EntryPrice,
		ExitPrice:   exitPrice,
		SizeUSD:     pos.SizeUSD,
		Leverage:    pos.Leverage,
		PnLUSD:      pnlUSD,
		PnLPct:      pnlPct,
		Result:      result,
		CloseReason: string(reason),
		Strategy:    pos.Strategy,
		Confidence:  pos.Confidence,
		AIReason:    pos.AIReason,
		OpenedAt:    pos.OpenedAt,
		ClosedAt:    journal.Now(),
	}

	if err := m.jl.LogTrade(trade); err != nil {
		slog.Error("monitor: log trade failed", "pos_id", pos.ID, "err", err)
	}

	if err := m.jl.RemovePosition(pos.ID); err != nil {
		slog.Error("monitor: remove position failed", "pos_id", pos.ID, "err", err)
	}

	if m.onClose != nil {
		m.onClose(pos, exitPrice, reason, pnlUSD, pnlPct)
	}
}

// fetchTASnapshot fetches fresh candles and calculates a TA snapshot for AI decision.
func (m *Monitor) fetchTASnapshot(ctx context.Context, pair string) (ta.TAResult, error) {
	candles, err := m.fetcher.FetchOHLCV(ctx, pair, "4h", 200)
	if err != nil {
		return ta.TAResult{}, fmt.Errorf("monitor: fetch OHLCV pair=%s: %w", pair, err)
	}
	return ta.Calculate(candles)
}

// waitForFill polls the exchange until the position appears or timeout is reached.
// Returns true if the position filled, false on timeout.
func (m *Monitor) waitForFill(ctx context.Context, pos journal.OpenPosition) bool {
	deadline := time.Now().Add(maxWaitFillDuration)

	// immediate check — don't wait 15s on fresh placements or restarts
	if m.hasPositionOnExchange(ctx, pos) {
		slog.Info("monitor: position already filled on exchange",
			"pos_id", pos.ID, "pair", pos.Pair)
		return true
	}

	poll := time.NewTicker(fillPollInterval)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-poll.C:
			if m.hasPositionOnExchange(ctx, pos) {
				slog.Info("monitor: position filled on exchange",
					"pos_id", pos.ID, "pair", pos.Pair)
				return true
			}

			slog.Debug("monitor: position not yet filled — waiting",
				"pos_id", pos.ID, "pair", pos.Pair)
		}

		if time.Now().After(deadline) {
			slog.Warn("monitor: position fill timeout",
				"pos_id", pos.ID,
				"pair", pos.Pair,
				"elapsed_min", fmt.Sprintf("%.0f", maxWaitFillDuration.Minutes()),
			)
			return false
		}
	}
}

// hasPositionOnExchange checks if a matching position exists on Hyperliquid.
func (m *Monitor) hasPositionOnExchange(ctx context.Context, pos journal.OpenPosition) bool {
	positions, err := m.exClient.FetchPositions(ctx)
	if err != nil {
		slog.Warn("monitor: fetch positions failed — assuming not filled",
			"pos_id", pos.ID, "pair", pos.Pair, "err", err)
		return false
	}

	for _, p := range positions {
		if p.Pair == pos.Pair && p.Side == pos.Direction {
			return true
		}
	}
	return false
}

// placeTPandSL submits TP and SL trigger orders after position is confirmed filled.
// Doesn't return errors — logs at ERROR level so failures are visible.
func (m *Monitor) placeTPandSL(ctx context.Context, pos journal.OpenPosition, coinSize float64) {
	if pos.TakeProfit > 0 {
		if err := m.exClient.PlaceTriggerOrder(ctx, pos.Pair, pos.Direction, coinSize, pos.TakeProfit, hyperliquid.TakeProfit); err != nil {
			slog.Error("monitor: TP trigger order FAILED — will not appear on HL",
				"pos_id", pos.ID, "pair", pos.Pair, "tp", pos.TakeProfit, "err", err)
		}
	}
	if pos.StopLoss > 0 {
		if err := m.exClient.PlaceTriggerOrder(ctx, pos.Pair, pos.Direction, coinSize, pos.StopLoss, hyperliquid.StopLoss); err != nil {
			slog.Error("monitor: SL trigger order FAILED — will not appear on HL",
				"pos_id", pos.ID, "pair", pos.Pair, "sl", pos.StopLoss, "err", err)
		}
	}
}

// ── PnL + TP/SL Helpers ───────────────────────────────────────────────────────

func calcPnLPct(direction exchange.OrderSide, entryPrice, currentPrice float64) float64 {
	if entryPrice == 0 {
		return 0
	}
	if direction == exchange.OrderSideLong {
		return (currentPrice - entryPrice) / entryPrice * 100
	}
	return (entryPrice - currentPrice) / entryPrice * 100
}

func hitTP(direction exchange.OrderSide, currentPrice, tp float64) bool {
	if direction == exchange.OrderSideLong {
		return currentPrice >= tp
	}
	return currentPrice <= tp
}

func hitSL(direction exchange.OrderSide, currentPrice, sl float64) bool {
	if direction == exchange.OrderSideLong {
		return currentPrice <= sl
	}
	return currentPrice >= sl
}
