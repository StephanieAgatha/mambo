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
		"ai_model", config.GrokModel,
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

	// ── Test Phase B+C+D for SOL ──────────────────────────────────────────────
	candles, err := fetcher.FetchOHLCV(ctx, "SOL", "4h", 200)
	if err != nil {
		slog.Error("failed to fetch OHLCV", "err", err)
		os.Exit(1)
	}

	mc, err := fetcher.FetchMarketContext(ctx, "SOL")
	if err != nil {
		slog.Warn("market context unavailable", "err", err)
	}

	taResult, err := ta.Calculate(candles)
	if err != nil {
		slog.Error("failed to calculate TA", "err", err)
		os.Exit(1)
	}
	slog.Info("✅ TA calculated",
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

	filterResult := filter.ApplyPreFilter("SOL", taResult, state)
	if !filterResult.Pass {
		slog.Warn("pre-filter: skip", "pair", "SOL", "reason", filterResult.Reason)
	} else {
		slog.Info("✅ pre-filter passed", "pair", "SOL")

		score, err := scorer.Score(ctx, "SOL", taResult, mc, state)
		if err != nil {
			slog.Error("AI scoring failed", "err", err)
		} else {
			slog.Info("✅ AI score",
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