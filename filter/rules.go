package filter

import (
	"fmt"
	"log/slog"

	"mambo/config"
	"mambo/ta"
)

// BotState holds the current daily trading state needed for pre-filter decisions.
type BotState struct {
	Balance            float64 // live balance from Hyperliquid
	DailyLossUSD       float64 // total loss realized today
	DailyWinUSD        float64 // total profit realized today
	ConsecutiveLosses  int     // consecutive losses without a win
	TotalAtRiskUSD     float64 // total USD currently deployed in open positions
}

// FilterResult is the outcome of ApplyPreFilter.
type FilterResult struct {
	Pass    bool   // true = pair passed all rules, proceed to AI scoring
	SkipAll bool   // true = stop scanning ALL pairs this cycle (daily limit hit)
	Reason  string // human-readable reason for skip/fail
}

// ApplyPreFilter runs all hard-coded rules against TA results and bot state.
// No AI involved — these rules are deterministic and cannot be overridden.
// Returns FilterResult indicating whether to proceed, skip this pair, or skip all.
func ApplyPreFilter(pair string, result ta.TAResult, state BotState) FilterResult {
	// ── Global limits — skip ALL pairs if triggered ───────────────────────────

	// daily loss limit: 15% of balance
	dailyLossLimit := state.Balance * config.DailyLossLimitPct
	if state.DailyLossUSD >= dailyLossLimit {
		reason := fmt.Sprintf("daily loss limit hit: $%.2f / $%.2f (%.0f%% of balance)",
			state.DailyLossUSD, dailyLossLimit, config.DailyLossLimitPct*100)
		slog.Warn("pre-filter: SKIP ALL — daily loss limit",
			"pair", pair,
			"loss_usd", state.DailyLossUSD,
			"limit_usd", dailyLossLimit,
		)
		return FilterResult{Pass: false, SkipAll: true, Reason: reason}
	}

	// daily win limit: 30% of balance — lock profits
	dailyWinLimit := state.Balance * config.DailyWinLimitPct
	if state.DailyWinUSD >= dailyWinLimit {
		reason := fmt.Sprintf("daily win limit hit: $%.2f / $%.2f — locking profits",
			state.DailyWinUSD, dailyWinLimit)
		slog.Warn("pre-filter: SKIP ALL — daily win limit (lock profits)",
			"pair", pair,
			"win_usd", state.DailyWinUSD,
			"limit_usd", dailyWinLimit,
		)
		return FilterResult{Pass: false, SkipAll: true, Reason: reason}
	}

	// consecutive losses: 2 in a row → mandatory 24h break
	if state.ConsecutiveLosses >= config.MaxConsecutiveLosses {
		reason := fmt.Sprintf("consecutive losses: %d/%d — mandatory break",
			state.ConsecutiveLosses, config.MaxConsecutiveLosses)
		slog.Warn("pre-filter: SKIP ALL — consecutive loss limit",
			"pair", pair,
			"consecutive_losses", state.ConsecutiveLosses,
		)
		return FilterResult{Pass: false, SkipAll: true, Reason: reason}
	}

	// capital at risk: 60% of balance max deployed across all positions
	maxAtRisk := state.Balance * config.MaxCapitalAtRiskPct
	if state.TotalAtRiskUSD >= maxAtRisk {
		reason := fmt.Sprintf("capital at risk limit: $%.2f / $%.2f (%.0f%% of balance)",
			state.TotalAtRiskUSD, maxAtRisk, config.MaxCapitalAtRiskPct*100)
		slog.Warn("pre-filter: SKIP ALL — capital at risk limit",
			"pair", pair,
			"at_risk_usd", state.TotalAtRiskUSD,
			"max_usd", maxAtRisk,
		)
		return FilterResult{Pass: false, SkipAll: true, Reason: reason}
	}

	// remaining budget check: must have at least 5% of balance left to open a trade
	minTradeSize := state.Balance * config.MinSizePct
	remainingBudget := maxAtRisk - state.TotalAtRiskUSD
	if remainingBudget < minTradeSize {
		reason := fmt.Sprintf("remaining budget too small: $%.2f < min trade size $%.2f",
			remainingBudget, minTradeSize)
		slog.Warn("pre-filter: SKIP ALL — insufficient remaining budget",
			"pair", pair,
			"remaining", remainingBudget,
			"min_trade", minTradeSize,
		)
		return FilterResult{Pass: false, SkipAll: true, Reason: reason}
	}

	// ── Pair-level rules — skip THIS pair only if triggered ───────────────────

	// EMA trend gate: spread < 0.2% = sideways/choppy market
	if result.EMASpread < config.EMASpreadThreshold*100 {
		reason := fmt.Sprintf("EMA spread %.3f%% < %.1f%% threshold — sideways market",
			result.EMASpread, config.EMASpreadThreshold*100)
		slog.Debug("pre-filter: skip pair — sideways market",
			"pair", pair,
			"ema_spread", result.EMASpread,
		)
		return FilterResult{Pass: false, SkipAll: false, Reason: reason}
	}

	// price below EMA200 = macro downtrend — no longs in bear market
	if result.CurrentPrice < result.EMA200 {
		reason := fmt.Sprintf("price $%.4f below EMA200 $%.4f — bearish macro, skip longs",
			result.CurrentPrice, result.EMA200)
		slog.Debug("pre-filter: skip pair — price below EMA200",
			"pair", pair,
			"price", result.CurrentPrice,
			"ema200", result.EMA200,
		)
		return FilterResult{Pass: false, SkipAll: false, Reason: reason}
	}

	// RSI extremes: overbought or oversold — wait for confirmation
	if result.RSI > 70 {
		reason := fmt.Sprintf("RSI %.2f > 70 — overbought, avoid long entry", result.RSI)
		slog.Debug("pre-filter: skip pair — RSI overbought",
			"pair", pair,
			"rsi", result.RSI,
		)
		return FilterResult{Pass: false, SkipAll: false, Reason: reason}
	}
	if result.RSI < 30 {
		reason := fmt.Sprintf("RSI %.2f < 30 — oversold, wait for reversal confirmation", result.RSI)
		slog.Debug("pre-filter: skip pair — RSI oversold",
			"pair", pair,
			"rsi", result.RSI,
		)
		return FilterResult{Pass: false, SkipAll: false, Reason: reason}
	}

	// volume confirmation: current volume must exceed 20-period average
	if !result.VolumeConfirms {
		reason := fmt.Sprintf("volume %.2fx average — below threshold, fakeout risk",
			result.VolumeMultiplier)
		slog.Debug("pre-filter: skip pair — low volume",
			"pair", pair,
			"volume_mult", result.VolumeMultiplier,
		)
		return FilterResult{Pass: false, SkipAll: false, Reason: reason}
	}

	// all rules passed
	slog.Debug("pre-filter: PASS",
		"pair", pair,
		"ema_spread", fmt.Sprintf("%.3f%%", result.EMASpread),
		"rsi", fmt.Sprintf("%.2f", result.RSI),
		"volume_mult", fmt.Sprintf("%.2fx", result.VolumeMultiplier),
	)

	return FilterResult{Pass: true, SkipAll: false, Reason: "all rules passed"}
}

// RemainingBudget returns how much USD can still be deployed in new positions.
func RemainingBudget(state BotState) float64 {
	maxAtRisk := state.Balance * config.MaxCapitalAtRiskPct
	remaining := maxAtRisk - state.TotalAtRiskUSD
	if remaining < 0 {
		return 0
	}
	return remaining
}