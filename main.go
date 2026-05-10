package main

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

	// ── Phase G+H: Scan loop (AI-only, no execution) ──────────────────────────

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

	// refreshPairData fetches candles + TA + market context for a given pair
	refreshPairData := func(pair string) (ta.TAResult, market.MarketContext, error) {
		candles, err := fetcher.FetchOHLCV(ctx, pair, "4h", 200)
		if err != nil {
			return ta.TAResult{}, market.MarketContext{}, fmt.Errorf("fetch OHLCV: %w", err)
		}

		taResult, err := ta.Calculate(candles)
		if err != nil {
			return ta.TAResult{}, market.MarketContext{}, fmt.Errorf("calculate TA: %w", err)
		}

		mc, err := fetcher.FetchMarketContext(ctx, pair)
		if err != nil {
			slog.Warn("market context unavailable", "pair", pair, "err", err)
		}

		return taResult, mc, nil
	}

	// track already-seen pairs to avoid infinite loops
	seenPairs := make(map[string]bool)

	const maxAttempts = 20
	attempt := 0

scanLoop:
	for attempt < maxAttempts {
		attempt++

		// pick a random pair we haven't analysed yet this cycle
		pair, err := market.GetRandomPair()
		if err != nil {
			slog.Error("failed to get random pair", "err", err)
			break
		}
		if seenPairs[pair] {
			continue
		}
		seenPairs[pair] = true

		slog.Info("scanning pair", "attempt", attempt, "max", maxAttempts, "pair", pair)

		taResult, mc, err := refreshPairData(pair)
		if err != nil {
			slog.Error("failed to refresh pair data", "pair", pair, "err", err)
			continue
		}

		slog.Info("✅ TA calculated",
			"pair", pair,
			"price", taResult.CurrentPrice,
			"rsi", fmt.Sprintf("%.2f", taResult.RSI),
			"ribbon", taResult.RibbonStatus,
		)

		// ── Pre‑filter ────────────────────────────────────────────────────────
		filterResult := filter.ApplyPreFilter(pair, taResult, state)
		if !filterResult.Pass {
			slog.Warn("pre-filter: skip", "pair", pair, "reason", filterResult.Reason)
			if filterResult.SkipAll {
				slog.Warn("global limit hit — stopping scan")
				break
			}
			continue
		}

		slog.Info("✅ pre-filter passed", "pair", pair)

		if !cfg.EnableAI {
			slog.Info("AI disabled — skipping scoring", "pair", pair)
			continue
		}

		// ── AI Scoring ───────────────────────────────────────────────────────
		score, err := scorer.Score(ctx, pair, taResult, mc, state)
		if err != nil {
			slog.Error("AI scoring failed", "pair", pair, "err", err)
			// log the TA even if AI call failed
			jl.AppendAnalysisLog(journal.AnalysisLogEntry{
				Timestamp: journal.Now(),
				Pair:      pair,
				TA:        taResult,
				Market:    mc,
				AIError:   err.Error(),
			})
			continue
		}

		// ── Build AI decision log ─────────────────────────────────────────────
		aiLog := &journal.AIDecisionLog{
			Action:          score.Action,
			Leverage:        score.Leverage,
			PositionSizeUSD: score.PositionSizeUSD,
			StopLoss:        score.StopLoss,
			TakeProfit:      score.TakeProfit,
			Confidence:      score.Confidence,
			Strategy:        score.Strategy,
			ConfluenceCount: score.ConfluenceCount,
			RRRatio:         score.RRRatio,
			Reasoning:       score.Reasoning,
		}

		// Append full TA + AI decision to JSONL
		if err := jl.AppendAnalysisLog(journal.AnalysisLogEntry{
			Timestamp:  journal.Now(),
			Pair:       pair,
			TA:         taResult,
			Market:     mc,
			AIDecision: aiLog,
		}); err != nil {
			slog.Warn("failed to write analysis log", "pair", pair, "err", err)
		}

		// ── Decide whether to accept or retry ──────────────────────────────────
		switch score.Action {
		case "open_long", "open_short":
			slog.Info("✅ AI accepted trade",
				"pair", pair,
				"action", score.Action,
				"confidence", fmt.Sprintf("%.0f", score.Confidence),
				"entry_price", fmt.Sprintf("%.4f", taResult.CurrentPrice),
				"stop_loss", fmt.Sprintf("%.4f", score.StopLoss),
				"take_profit", fmt.Sprintf("%.4f", score.TakeProfit),
				"size_usd", fmt.Sprintf("%.2f", score.PositionSizeUSD),
				"leverage", score.Leverage,
				"rr_ratio", fmt.Sprintf("%.2f", score.RRRatio),
				"strategy", score.Strategy,
				"reasoning", score.Reasoning,
			)
			// TODO: Phase H — execute on Hyperliquid
			break scanLoop

		case "wait", "hold":
			slog.Warn("AI chose to abort — retrying with different pair",
				"pair", pair,
				"action", score.Action,
				"reasoning", score.Reasoning,
			)
			continue

		default:
			slog.Warn("AI returned unknown action — retrying",
				"pair", pair,
				"action", score.Action,
			)
			continue
		}
	}

	slog.Info("scan complete",
		"attempts", attempt,
		"pairs_tried", len(seenPairs),
	)

	slog.Info("✅ Phase G+H ready — all systems running")
	slog.Info("Mambo AI Trade ready — press Ctrl+C to stop")

	sig := <-quit
	slog.Info("shutdown signal received", "signal", sig)
	cancel()
	slog.Info("Mambo AI Trade stopped. Goodbye.")
}
