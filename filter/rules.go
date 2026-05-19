package filter

import (
	"mambo/config"
	"mambo/ta"
)

// BotState holds the current daily trading state needed for pre-filter decisions.
type BotState struct {
	Balance           float64 // live balance from Hyperliquid
	DailyLossUSD      float64 // total loss realized today
	DailyWinUSD       float64 // total profit realized today
	ConsecutiveLosses int     // consecutive losses without a win
	TotalAtRiskUSD    float64 // total USD currently deployed in open positions
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
	return FilterResult{Pass: true, SkipAll: false, Reason: "always pass"}
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
