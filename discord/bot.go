package discord

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	aiPkg "mambo/ai"
	"mambo/config"
	"mambo/exchange"
	"mambo/filter"
	"mambo/journal"
	"mambo/logger"
	"mambo/market"
	"mambo/monitor"
	"mambo/ta"
)

func main() {
	slog.SetDefault(logger.New())
	slog.Info("Mambo AI Trade starting...")

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	slog.Info("config loaded",
		"network", cfg.NetworkLabel(),
		"ai_model", cfg.AILabel(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// ── Phase E: Journal ──────────────────────────────────────────────────────
	jl, err := journal.New()
	if err != nil {
		slog.Error("failed to init journal", "err", err)
		os.Exit(1)
	}

	openPositions, err := jl.LoadPositions()
	if err != nil {
		slog.Error("failed to load positions", "err", err)
		os.Exit(1)
	}
	slog.Info("journal initialized", "recovered_positions", len(openPositions))

	// ── Phase B: Exchange + Fetcher ───────────────────────────────────────────
	exClient, err := exchange.New(ctx, cfg)
	if err != nil {
		slog.Error("failed to init exchange client", "err", err)
		os.Exit(1)
	}

	balance, err := exClient.FetchBalance(ctx)
	if err != nil {
		slog.Error("failed to fetch balance", "err", err)
		os.Exit(1)
	}
	slog.Info("✅ balance fetched", "balance_usd", balance)

	fetcher := market.New(ctx, cfg)

	// ── Phase D: AI clients ───────────────────────────────────────────────────
	scorer, err := aiPkg.NewScorer(cfg)
	if err != nil {
		slog.Error("failed to init AI scorer", "err", err)
		os.Exit(1)
	}

	posMgr, err := aiPkg.NewPositionManager(cfg)
	if err != nil {
		slog.Error("failed to init position manager", "err", err)
		os.Exit(1)
	}

	// ── Phase F: Monitor ──────────────────────────────────────────────────────

	// Discord notification callbacks — wired in Phase G
	// For now: log to terminal
	onClose := func(pos journal.OpenPosition, exitPrice float64, reason monitor.CloseReason, pnlUSD, pnlPct float64) {
		result := "WIN"
		if pnlUSD < 0 {
			result = "LOSS"
		}
		slog.Info("🔔 position closed",
			"pair", pos.Pair,
			"result", result,
			"reason", reason,
			"exit_price", exitPrice,
			"pnl_usd", fmt.Sprintf("%.2f", pnlUSD),
			"pnl_pct", fmt.Sprintf("%.2f%%", pnlPct),
		)
	}

	onUpdate := func(pos journal.OpenPosition, action string, newSL, newTP float64, reasoning string) {
		slog.Info("🔔 position updated by AI",
			"pair", pos.Pair,
			"action", action,
			"new_sl", newSL,
			"new_tp", newTP,
			"reasoning", reasoning,
		)
	}

	mon := monitor.New(exClient, fetcher, posMgr, jl, cfg, onClose, onUpdate)

	// recover open positions — spawn a monitor goroutine for each
	for _, pos := range openPositions {
		slog.Info("recovering position monitor",
			"pos_id", pos.ID,
			"pair", pos.Pair,
			"direction", pos.Direction,
		)
		mon.Start(ctx, pos)
	}
	slog.Info("✅ Phase F: monitor goroutines started",
		"monitoring", len(openPositions),
	)

	// ── Test Phase B+C+D for random pair from pairs.json ──────────────────────
	// Get random pair from pairs.json
	randomPair, err := market.GetRandomPair()
	if err != nil {
		slog.Error("failed to get random pair", "err", err)
		os.Exit(1)
	}

	slog.Info("Selected random pair for analysis", "pair", randomPair)

	candles, err := fetcher.FetchOHLCV(ctx, randomPair, "4h", 200)
	if err != nil {
		slog.Error("failed to fetch OHLCV", "pair", randomPair, "err", err)
		os.Exit(1)
	}

	mc, err := fetcher.FetchMarketContext(ctx, randomPair)
	if err != nil {
		slog.Warn("market context unavailable", "pair", randomPair, "err", err)
	}

	taResult, err := ta.Calculate(candles)
	if err != nil {
		slog.Error("failed to calculate TA", "pair", randomPair, "err", err)
		os.Exit(1)
	}
	slog.Info("✅ TA calculated",
		"pair", randomPair,
		"price", taResult.CurrentPrice,
		"rsi", fmt.Sprintf("%.2f", taResult.RSI),
		"ribbon", taResult.RibbonStatus,
	)

	lossUSD, winUSD, consecLosses, err := jl.GetDailyPnL()
	if err != nil {
		slog.Error("failed to get daily PnL", "err", err)
		os.Exit(1)
	}

	state := filter.BotState{
		Balance:           balance,
		DailyLossUSD:      lossUSD,
		DailyWinUSD:       winUSD,
		ConsecutiveLosses: consecLosses,
		TotalAtRiskUSD:    0,
	}

	filterResult := filter.ApplyPreFilter(randomPair, taResult, state)
	if !filterResult.Pass {
		slog.Warn("pre-filter: skip", "pair", randomPair, "reason", filterResult.Reason)
	} else {
		slog.Info("✅ pre-filter passed", "pair", randomPair)

		score, err := scorer.Score(ctx, randomPair, taResult, mc, state)
		if err != nil {
			slog.Error("AI scoring failed", "pair", randomPair, "err", err)
		} else {
			slog.Info("✅ AI score",
				"pair", randomPair,
				"action", score.Action,
				"confidence", score.Confidence,
				"size_usd", score.PositionSizeUSD,
				"leverage", score.Leverage,
				"reasoning", score.Reasoning,
			)
		}
	}

	slog.Info("✅ Phase F complete — all systems running")
	slog.Info("Mambo AI Trade ready — press Ctrl+C to stop")

	// ── TODO: Phase G — discord bot ──────────────────────────────────────────
	// ── TODO: Phase H — main scan loop ───────────────────────────────────────

	sig := <-quit
	slog.Info("shutdown signal received", "signal", sig)
	cancel()
	slog.Info("Mambo AI Trade stopped. Goodbye.")
}
