package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mambo/config"
	"mambo/filter"
	"mambo/market"
	"mambo/ta"
)

// ScoreResult holds the parsed AI decision for a trade setup.
type ScoreResult struct {
	Symbol          string
	Action          string  // "open_long" / "open_short" / "hold" / "wait"
	Direction       string  // "long" / "short" — used by /execute flow
	Leverage        int
	PositionSizeUSD float64
	StopLoss        float64
	TakeProfit      float64
	Confidence      float64
	Strategy        string
	ConfluenceCount int
	RRRatio         float64
	Reasoning       string

	// ── New in v2 ─────────────────────────────────────────────────────
	Trend        string // "UPTREND" | "DOWNTREND" | "RANGING"
	EntryQuality string // "AT_STRUCTURE" | "PULLBACK" | "BREAKOUT" | "CHASE" | "WAIT"
	SLReasoning  string // structural justification for SL placement
	TPReasoning  string // structural justification for TP placement
	Invalidation string // price or action that immediately invalidates the trade
}

// Scorer sends trade setups to the configured AI provider and parses the response.
type Scorer struct {
	agentPrompt   string
	template      string
	suggestPrompt string
	executePrompt string
	client        *Client
	cfg           *config.Config
}

// NewScorer loads AGENT.md and verify_trade.md once at startup.
func NewScorer(cfg *config.Config) (*Scorer, error) {
	agentPrompt, err := os.ReadFile("AGENT.md")
	if err != nil {
		return nil, fmt.Errorf("scorer: read AGENT.md: %w", err)
	}

	template, err := os.ReadFile("prompts/verify_trade.md")
	if err != nil {
		return nil, fmt.Errorf("scorer: read prompts/verify_trade.md: %w", err)
	}

	suggestPrompt, err := os.ReadFile("prompts/suggest_trade.md")
	if err != nil {
		return nil, fmt.Errorf("scorer: read prompts/suggest_trade.md: %w", err)
	}

	executePrompt, err := os.ReadFile("prompts/execute_trade.md")
	if err != nil {
		return nil, fmt.Errorf("scorer: read prompts/execute_trade.md: %w", err)
	}

	// reasoning models can take longer — generous timeout
	client := NewClient(cfg, 120*time.Second)

	slog.Info("scorer initialized",
		"provider", cfg.AIProvider,
		"model", cfg.AIModel,
		"agent_prompt_len", len(agentPrompt),
		"template_len", len(template),
	)

	return &Scorer{
		agentPrompt:   string(agentPrompt),
		template:      string(template),
		suggestPrompt: string(suggestPrompt),
		executePrompt: string(executePrompt),
		client:        client,
		cfg:           cfg,
	}, nil
}

// Score fills the verify_trade.md template with real data, sends to AI,
// and returns a validated + clamped ScoreResult.
func (s *Scorer) Score(
	ctx context.Context,
	pair string,
	taResult ta.TAResult,
	mc market.MarketContext,
	state filter.BotState,
) (ScoreResult, error) {
	prompt := s.fillTemplate(pair, taResult, mc, state)

	slog.Debug("sending to AI",
		"provider", s.cfg.AIProvider,
		"model", s.cfg.AIModel,
		"pair", pair,
	)

	raw, err := s.client.Chat(ctx, s.agentPrompt, prompt)
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: AI call failed pair=%s: %w", pair, err)
	}

	result, err := parseDecision(raw, pair)
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: parse decision failed pair=%s: %w", pair, err)
	}

	// validate and clamp — AI cannot bypass guardrails
	result = s.validateAndClamp(result, state)

	slog.Info("AI score complete",
		"provider", s.cfg.AIProvider,
		"pair", pair,
		"action", result.Action,
		"confidence", result.Confidence,
		"size_usd", result.PositionSizeUSD,
		"leverage", result.Leverage,
		"rr_ratio", result.RRRatio,
		"strategy", result.Strategy,
		"trend", result.Trend,
		"entry_quality", result.EntryQuality,
		"rsi_div", string(taResult.RSIDivergence),
		"macd_div", string(taResult.MACDDivergence),
		"double_div", taResult.DoubleDivergence,
	)

	return result, nil
}

// Suggest runs the advisory-only prompt — no clamping, no guardrails.
// Returns the AI's raw trade suggestion for user review (DYOR).
// prefilterReason is the prefilter rejection reason (empty if not prefilter-skipped).
func (s *Scorer) Suggest(
	ctx context.Context,
	pair string,
	taResult ta.TAResult,
	mc market.MarketContext,
	state filter.BotState,
	prefilterReason string,
) (ScoreResult, error) {
	prompt := s.fillTemplate(pair, taResult, mc, state)
	// Inject the prefilter reason into the template
	prompt = strings.ReplaceAll(prompt, "{{PREFILTER_REASON}}", prefilterReason)

	slog.Debug("sending suggestion request to AI",
		"provider", s.cfg.AIProvider,
		"model", s.cfg.AIModel,
		"pair", pair,
	)

	raw, err := s.client.Chat(ctx, s.suggestPrompt, prompt)
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: AI suggest call failed pair=%s: %w", pair, err)
	}

	// Log raw response at INFO so we can debug format issues
	slog.Info("scorer: AI suggest raw response",
		"pair", pair,
		"raw_len", len(raw),
		"raw_preview", raw[:min(len(raw), 500)],
	)

	result, err := parseSuggestion(raw, pair)
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: parse suggestion failed pair=%s: %w", pair, err)
	}

	slog.Info("AI suggestion complete",
		"provider", s.cfg.AIProvider,
		"pair", pair,
		"direction", result.Action,
		"confidence", result.Confidence,
		"size_usd", result.PositionSizeUSD,
		"leverage", result.Leverage,
		"rr_ratio", result.RRRatio,
		"strategy", result.Strategy,
	)

	return result, nil
}

// Execute runs the direct execution prompt — forces a directional decision (long/short).
// No "wait" or "hold" fallback. Always returns a trade with direction, TP, SL, and sizing.
func (s *Scorer) Execute(
	ctx context.Context,
	pair string,
	taResult ta.TAResult,
	mc market.MarketContext,
	state filter.BotState,
) (ScoreResult, error) {
	prompt := s.fillTemplate(pair, taResult, mc, state)

	slog.Debug("sending execute request to AI",
		"provider", s.cfg.AIProvider,
		"model", s.cfg.AIModel,
		"pair", pair,
	)

	raw, err := s.client.Chat(ctx, s.executePrompt, prompt)
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: AI execute call failed pair=%s: %w", pair, err)
	}

	// Log raw response
	slog.Info("scorer: AI execute raw response",
		"pair", pair,
		"raw_len", len(raw),
		"raw_preview", raw[:min(len(raw), 500)],
	)

	result, err := parseExecute(raw, pair)
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: parse execute failed pair=%s: %w", pair, err)
	}

	result = s.validateAndClamp(result, state)

	// Ensure direction is always set
	if result.Direction == "" {
		if result.Action == "open_long" {
			result.Direction = "long"
		} else if result.Action == "open_short" {
			result.Direction = "short"
		} else {
			result.Direction = "long" // default to long as last resort
		}
	}

	slog.Info("AI execute complete",
		"provider", s.cfg.AIProvider,
		"pair", pair,
		"direction", result.Direction,
		"confidence", result.Confidence,
		"size_usd", result.PositionSizeUSD,
		"leverage", result.Leverage,
		"rr_ratio", result.RRRatio,
		"strategy", result.Strategy,
		"entry_quality", result.EntryQuality,
	)

	return result, nil
}

// parseSuggestion extracts the <suggestion> JSON block from AI response.
// Falls back to <decision> if <suggestion> is not found.
// Handles markdown code fences that AI might wrap the block in.
func parseSuggestion(raw, pair string) (ScoreResult, error) {
	// Strip markdown code fences around the block
	cleaned := strings.ReplaceAll(raw, "```json", "")
	cleaned = strings.ReplaceAll(cleaned, "```", "")

	re := regexp.MustCompile(`(?s)<suggestion>(.*?)</suggestion>`)
	matches := re.FindStringSubmatch(cleaned)
	if len(matches) >= 2 {
		return parseSuggestBlock(matches[1], pair)
	}

	// Fallback: AI might have returned <decision> instead of <suggestion>
	re2 := regexp.MustCompile(`(?s)<decision>(.*?)</decision>`)
	matches2 := re2.FindStringSubmatch(cleaned)
	if len(matches2) >= 2 {
		slog.Info("scorer: AI returned <decision> instead of <suggestion> — using as fallback", "pair", pair)
		return parseDecisionFallback(matches2[1], pair)
	}

	slog.Warn("scorer: no <suggestion> or <decision> block found — returning fallback",
		"pair", pair,
		"raw_len", len(raw),
	)
	slog.Debug("scorer: raw AI suggest response", "pair", pair, "raw", raw)
	return ScoreResult{
		Symbol:    pair,
		Action:    "hold",
		Reasoning: "no suggestion block in AI response",
	}, nil
}

// parseSuggestBlock parses a <suggestion> JSON block.
func parseSuggestBlock(jsonStr, pair string) (ScoreResult, error) {
	jsonStr = strings.TrimSpace(jsonStr)

	var d struct {
		Symbol          string  `json:"symbol"`
		Direction       string  `json:"direction"`
		SizeUSD         float64 `json:"size_usd"`
		EntryPrice      float64 `json:"entry_price"`
		StopLoss        float64 `json:"stop_loss"`
		TakeProfit      float64 `json:"take_profit"`
		Leverage        int     `json:"leverage"`
		Confidence      float64 `json:"confidence"`
		Strategy        string  `json:"strategy"`
		ConfluenceCount int     `json:"confluence_count"`
		RRRatio         float64 `json:"rr_ratio"`
		Reasoning       string  `json:"reasoning"`

		Trend        string `json:"trend"`
		EntryQuality string `json:"entry_quality"`
		SLPlacement  string `json:"sl_placement"`
		TPPlacement  string `json:"tp_placement"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: unmarshal suggestion JSON: %w", err)
	}

	action := "hold"
	switch strings.ToUpper(d.Direction) {
	case "LONG":
		action = "open_long"
	case "SHORT":
		action = "open_short"
	}

	return ScoreResult{
		Symbol:          pair,
		Action:          action,
		Leverage:        d.Leverage,
		PositionSizeUSD: d.SizeUSD,
		StopLoss:        d.StopLoss,
		TakeProfit:      d.TakeProfit,
		Confidence:      d.Confidence,
		Strategy:        d.Strategy,
		ConfluenceCount: d.ConfluenceCount,
		RRRatio:         d.RRRatio,
		Reasoning:       d.Reasoning,

		Trend:        d.Trend,
		EntryQuality: d.EntryQuality,
		SLReasoning:  d.SLPlacement,
		TPReasoning:  d.TPPlacement,
	}, nil
}

// parseDecisionFallback parses a <decision> block as a suggest fallback.
// Maps decision fields to ScoreResult fields (action comes from "action" not "direction").
func parseDecisionFallback(jsonStr, pair string) (ScoreResult, error) {
	jsonStr = strings.TrimSpace(jsonStr)

	var d struct {
		Symbol          string  `json:"symbol"`
		Action          string  `json:"action"`
		Leverage        int     `json:"leverage"`
		PositionSizeUSD float64 `json:"position_size_usd"`
		StopLoss        float64 `json:"stop_loss"`
		TakeProfit      float64 `json:"take_profit"`
		Confidence      float64 `json:"confidence"`
		Strategy        string  `json:"strategy"`
		ConfluenceCount int     `json:"confluence_count"`
		RRRatio         float64 `json:"rr_ratio"`
		Reasoning       string  `json:"reasoning"`

		Trend        string `json:"trend"`
		EntryQuality string `json:"entry_quality"`
		SLReasoning  string `json:"sl_reasoning"`
		TPReasoning  string `json:"tp_reasoning"`
		Invalidation string `json:"invalidation"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: unmarshal decision fallback JSON: %w", err)
	}

	return ScoreResult{
		Symbol:          pair,
		Action:          d.Action,
		Leverage:        d.Leverage,
		PositionSizeUSD: d.PositionSizeUSD,
		StopLoss:        d.StopLoss,
		TakeProfit:      d.TakeProfit,
		Confidence:      d.Confidence,
		Strategy:        d.Strategy,
		ConfluenceCount: d.ConfluenceCount,
		RRRatio:         d.RRRatio,
		Reasoning:       d.Reasoning,

		Trend:        d.Trend,
		EntryQuality: d.EntryQuality,
		SLReasoning:  d.SLReasoning,
		TPReasoning:  d.TPReasoning,
		Invalidation: d.Invalidation,
	}, nil
}

// parseDecision extracts the <decision> JSON block from AI response.
// Falls back to raw JSON {…} block if tags are missing.
func parseDecision(raw, pair string) (ScoreResult, error) {
	re := regexp.MustCompile(`(?s)<decision>(.*?)</decision>`)
	matches := re.FindStringSubmatch(raw)
	if len(matches) >= 2 {
		return parseDecisionJSON(matches[1], pair)
	}

	// Fallback: AI returned raw JSON without <decision> tags
	reJSON := regexp.MustCompile(`(?s)\{[^{]*"action"\s*:\s*"[^"]+"[^}]*\}`)
	matchesJSON := reJSON.FindString(raw)
	if matchesJSON != "" {
		slog.Info("scorer: no <decision> tags — found raw JSON block", "pair", pair)
		return parseDecisionJSON(matchesJSON, pair)
	}

	slog.Warn("scorer: no <decision> block and no action JSON found — defaulting to wait", "pair", pair)
	return ScoreResult{
		Symbol:    pair,
		Action:    "wait",
		Reasoning: "no decision block in AI response",
	}, nil
}

// parseDecisionJSON unmarshals a JSON decision block into ScoreResult.
func parseDecisionJSON(jsonStr, pair string) (ScoreResult, error) {
	jsonStr = strings.TrimSpace(jsonStr)

	var d struct {
		Symbol          string  `json:"symbol"`
		Action          string  `json:"action"`
		Leverage        int     `json:"leverage"`
		PositionSizeUSD float64 `json:"position_size_usd"`
		StopLoss        float64 `json:"stop_loss"`
		TakeProfit      float64 `json:"take_profit"`
		Confidence      float64 `json:"confidence"`
		Strategy        string  `json:"strategy"`
		ConfluenceCount int     `json:"confluence_count"`
		RRRatio         float64 `json:"rr_ratio"`
		Reasoning       string  `json:"reasoning"`

		// ── New in v2 ─────────────────────────────────────
		Trend        string `json:"trend"`
		EntryQuality string `json:"entry_quality"`
		SLReasoning  string `json:"sl_reasoning"`
		TPReasoning  string `json:"tp_reasoning"`
		Invalidation string `json:"invalidation"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: unmarshal decision JSON: %w", err)
	}

	return ScoreResult{
		Symbol:          pair,
		Action:          d.Action,
		Leverage:        d.Leverage,
		PositionSizeUSD: d.PositionSizeUSD,
		StopLoss:        d.StopLoss,
		TakeProfit:      d.TakeProfit,
		Confidence:      d.Confidence,
		Strategy:        d.Strategy,
		ConfluenceCount: d.ConfluenceCount,
		RRRatio:         d.RRRatio,
		Reasoning:       d.Reasoning,

		// New
		Trend:        d.Trend,
		EntryQuality: d.EntryQuality,
		SLReasoning:  d.SLReasoning,
		TPReasoning:  d.TPReasoning,
		Invalidation: d.Invalidation,
	}, nil
}

// validateAndClamp enforces all guardrails on AI output.
// AI cannot bypass these — they are enforced in Go code.
func (s *Scorer) validateAndClamp(result ScoreResult, state filter.BotState) ScoreResult {
	isTradeAction := result.Action == "open_long" || result.Action == "open_short"

	// confidence below minimum → force wait (only if AI wanted to trade)
	if isTradeAction && result.Confidence < config.MinConfidence {
		slog.Warn("scorer: confidence below minimum — forcing wait",
			"confidence", result.Confidence,
			"min", config.MinConfidence,
			"provider", s.cfg.AIProvider,
		)
		result.Action = "wait"
		result.Reasoning = fmt.Sprintf("confidence %.0f < minimum %d — aborted. Original: %s",
			result.Confidence, config.MinConfidence, result.Reasoning)
		return result
	}

	// R:R below minimum → force wait (only if AI wanted to trade)
	if isTradeAction && result.RRRatio > 0 && result.RRRatio < config.MinRiskReward {
		slog.Warn("scorer: R:R below minimum — forcing wait",
			"rr_ratio", result.RRRatio,
			"min", config.MinRiskReward,
		)
		result.Action = "wait"
		result.Reasoning = fmt.Sprintf("R:R %.2f < minimum %.1f — aborted. Original: %s",
			result.RRRatio, config.MinRiskReward, result.Reasoning)
		return result
	}

	// non-trade actions don't need size/leverage clamping
	if !isTradeAction {
		return result
	}

	// clamp leverage
	if result.Leverage < config.MinLeverageX {
		slog.Debug("scorer: clamp leverage up", "from", result.Leverage, "to", config.MinLeverageX)
		result.Leverage = config.MinLeverageX
	}
	if result.Leverage > config.MaxLeverageX {
		slog.Debug("scorer: clamp leverage down", "from", result.Leverage, "to", config.MaxLeverageX)
		result.Leverage = config.MaxLeverageX
	}

	// clamp position size
	minSize := state.Balance * config.MinSizePct
	maxSize := state.Balance * config.MaxSizePct

	if result.PositionSizeUSD < minSize {
		slog.Debug("scorer: clamp size up", "from", result.PositionSizeUSD, "to", minSize)
		result.PositionSizeUSD = minSize
	}
	if result.PositionSizeUSD > maxSize {
		slog.Debug("scorer: clamp size down", "from", result.PositionSizeUSD, "to", maxSize)
		result.PositionSizeUSD = maxSize
	}

	// clamp to remaining risk budget
	remaining := filter.RemainingBudget(state)
	if result.PositionSizeUSD > remaining {
		slog.Debug("scorer: clamp to remaining budget",
			"from", result.PositionSizeUSD, "to", remaining)
		result.PositionSizeUSD = remaining
	}

	result.PositionSizeUSD = math.Round(result.PositionSizeUSD*100) / 100

	return result
}

// fillTemplate replaces all {{PLACEHOLDERS}} in verify_trade.md with real values.
func (s *Scorer) fillTemplate(
	pair string,
	r ta.TAResult,
	mc market.MarketContext,
	state filter.BotState,
) string {
	remaining := filter.RemainingBudget(state)
	minSize := state.Balance * config.MinSizePct
	maxSize := state.Balance * config.MaxSizePct
	maxAtRisk := state.Balance * config.MaxCapitalAtRiskPct
	dailyLossLimit := state.Balance * config.DailyLossLimitPct
	dailyWinLimit := state.Balance * config.DailyWinLimitPct
	atRiskPct := 0.0
	if state.Balance > 0 {
		atRiskPct = state.TotalAtRiskUSD / state.Balance * 100
	}

	replacer := strings.NewReplacer(
		// portfolio context
		"{{LIVE_BALANCE}}", fmt.Sprintf("%.2f", state.Balance),
		"{{MIN_SIZE}}", fmt.Sprintf("%.2f", minSize),
		"{{MAX_SIZE}}", fmt.Sprintf("%.2f", maxSize),
		"{{MAX_AT_RISK}}", fmt.Sprintf("%.2f", maxAtRisk),
		"{{CURRENT_AT_RISK}}", fmt.Sprintf("%.2f", state.TotalAtRiskUSD),
		"{{CURRENT_AT_RISK_PCT}}", fmt.Sprintf("%.1f", atRiskPct),
		"{{REMAINING_BUDGET}}", fmt.Sprintf("%.2f", remaining),
		"{{DAILY_LOSS_LIMIT}}", fmt.Sprintf("%.2f", dailyLossLimit),
		"{{DAILY_LOSS_USED}}", fmt.Sprintf("%.2f", state.DailyLossUSD),
		"{{DAILY_WIN_LIMIT}}", fmt.Sprintf("%.2f", dailyWinLimit),
		"{{DAILY_WIN_EARNED}}", fmt.Sprintf("%.2f", state.DailyWinUSD),
		"{{CONSEC_LOSSES}}", fmt.Sprintf("%d", state.ConsecutiveLosses),
		"{{NETWORK}}", s.cfg.NetworkLabel(),
		"{{AI_PROVIDER}}", s.cfg.AIProvider,
		"{{AI_MODEL}}", s.cfg.AIModel,

		// trade data
		"{{ASSET}}", pair,
		"{{ENTRY_PRICE}}", fmt.Sprintf("%.4f", r.CurrentPrice),
		"{{CURRENT_PRICE}}", fmt.Sprintf("%.4f", r.CurrentPrice),

		// EMA
		"{{EMA9}}", fmt.Sprintf("%.4f", r.EMA9),
		"{{EMA21}}", fmt.Sprintf("%.4f", r.EMA21),
		"{{EMA50}}", fmt.Sprintf("%.4f", r.EMA50),
		"{{EMA200}}", fmt.Sprintf("%.4f", r.EMA200),
		"{{EMA_SPREAD}}", fmt.Sprintf("%.3f", r.EMASpread),
		"{{TREND_GATE}}", trendGate(r.EMASpread),
		"{{RIBBON_STATUS}}", r.RibbonStatus,
		"{{DAILY_TREND}}", r.DailyTrend,

		// RSI
		"{{RSI_VALUE}}", fmt.Sprintf("%.2f", r.RSI),
		"{{RSI_ZONE}}", r.RSIZone,
		"{{RSI_DIVERGENCE}}", boolToStr(r.RSIDivergence != ta.DivNone),
		"{{RSI_DIV_TYPE}}", string(r.RSIDivergence),
		"{{RSI_DIV_BARS}}", strconv.Itoa(r.RSIDivBarsAgo),

		// ATR
		"{{ATR_VALUE}}", fmt.Sprintf("%.4f", r.ATR),
		"{{ATR_LEVEL}}", r.ATRLevel,
		"{{SL_1ATR}}", fmt.Sprintf("%.4f", r.SL1ATR),
		"{{SL_15ATR}}", fmt.Sprintf("%.4f", r.SL15ATR),
		"{{RECOMMENDED_SL}}", fmt.Sprintf("%.4f", r.SL15ATR),

		// MACD
		"{{MACD_VALUE}}", fmt.Sprintf("%.4f", r.MACDValue),
		"{{MACD_SIGNAL_VALUE}}", fmt.Sprintf("%.4f", r.MACDSignal),
		"{{MACD_HISTOGRAM}}", fmt.Sprintf("%.4f", r.MACDHistogram),
		"{{MACD_CROSS}}", r.MACDCross,
		"{{MACD_DIVERGENCE}}", boolToStr(r.MACDDivergence != ta.DivNone),
		"{{MACD_DIV_TYPE}}", string(r.MACDDivergence),
		"{{MACD_DIV_BARS}}", strconv.Itoa(r.MACDDivBarsAgo),

		// Bollinger Bands
		"{{BB_UPPER}}", fmt.Sprintf("%.4f", r.BBUpper),
		"{{BB_MIDDLE}}", fmt.Sprintf("%.4f", r.BBMiddle),
		"{{BB_LOWER}}", fmt.Sprintf("%.4f", r.BBLower),
		"{{BB_WIDTH}}", r.BBWidth,
		"{{BB_POSITION}}", r.BBPosition,

		// Volume + OBV
		"{{VOLUME_MULTIPLIER}}", fmt.Sprintf("%.2f", r.VolumeMultiplier),
		"{{VOLUME_CONFIRMS}}", fmt.Sprintf("%v", r.VolumeConfirms),
		"{{OBV_TREND}}", r.OBVTrend,

		// Support & Resistance
		"{{NEAREST_SUPPORT}}", fmt.Sprintf("%.4f", r.NearestSupport),
		"{{NEAREST_RESISTANCE}}", fmt.Sprintf("%.4f", r.NearestResistance),
		"{{SUPPORT_STRENGTH}}", r.SupportStrength,
		"{{RESISTANCE_STRENGTH}}", r.ResistanceStrength,
		"{{AT_SUPPORT}}", fmt.Sprintf("%v", r.AtSupport),
		"{{NEAR_RESISTANCE}}", fmt.Sprintf("%v", r.NearResistance),

		// Market context
		"{{FEAR_GREED_VALUE}}", fmt.Sprintf("%d", mc.FearGreedValue),
		"{{FEAR_GREED_ZONE}}", mc.FearGreedZone,
		"{{FUNDING_RATE}}", fmt.Sprintf("%.6f", mc.FundingRate),
		"{{FUNDING_BIAS}}", mc.FundingBias,
		"{{OI_CHANGE}}", mc.OIChange,
		"{{LONG_SHORT_RATIO}}", fmt.Sprintf("%.2f", mc.LongShortRatio),
		"{{LONG_SHORT_BIAS}}", mc.LongShortBias,

		// AI-filled placeholders
		"{{DIRECTION}}", "LONG",
		"{{EMERGENCY_MIN_BALANCE}}", fmt.Sprintf("%.2f", minSize),
		"{{CURRENT_PNL_PCT}}", "0",
		"{{NOISE_ZONE}}", "N/A (new trade)",
		"{{PEAK_PNL_PCT}}", "0",
		"{{ACTIVE_POSITIONS}}", "see portfolio context",
		"{{TAKE_PROFIT}}", "AI will calculate",
		"{{CONFIDENCE}}", "AI will calculate",
		"{{SUGGESTED_SIZE}}", "AI will calculate",
		"{{SUGGESTED_PCT}}", "AI will calculate",
		"{{SUGGESTED_LEVERAGE}}", "AI will calculate",
		"{{SIZING_REASONING}}", "AI will decide",
		"{{STRATEGY_TRIGGERED}}", "AI will identify",
		"{{CONFLUENCE_COUNT}}", "AI will count",
		"{{RR_RATIO}}", "AI will calculate",
		"{{EXECUTE_OR_ABORT}}", "AI will decide",
		"{{EMA_SIGNAL}}", "see EMA section",
		"{{RSI_SIGNAL}}", "see RSI section",
		"{{MACD_SIGNAL}}", "see MACD section",
		"{{BB_SIGNAL}}", "see BB section",
		"{{VOLUME_SIGNAL}}", "see volume section",
		"{{MARKET_SIGNAL}}", "see market context",
		"{{RISK_STATUS}}", "passed pre-filter",
		"{{RR_GATE}}", "AI will evaluate",
		"{{SR_ENTRY_SIGNAL}}", srEntrySignal(r),
		"{{DIVERGENCE_SIGNAL}}", divergenceSignalStr(r),
		"{{TIMESTAMP}}", time.Now().Format(time.RFC3339),
	)

	return replacer.Replace(s.template)
}

// trendGate returns PASS or SKIP based on EMA spread threshold.
func trendGate(spread float64) string {
	if spread >= config.EMASpreadThreshold*100 {
		return "PASS"
	}
	return "SKIP (sideways market)"
}

// divergenceSignalStr returns the confluence table value for the divergence row.
func divergenceSignalStr(r ta.TAResult) string {
	if r.DoubleDivergence {
		return "DOUBLE_CONFIRMED ⚡"
	}
	if r.RSIDivergence != ta.DivNone || r.MACDDivergence != ta.DivNone {
		return "DETECTED"
	}
	return "NEUTRAL"
}

// srEntrySignal returns the confluence table value for the S/R entry row.
func srEntrySignal(r ta.TAResult) string {
	if r.AtSupport {
		return "AT_SUPPORT ✅"
	}
	if r.NearResistance {
		return "NEAR_RESISTANCE ⚠️"
	}
	return "BETWEEN_LEVELS"
}

// boolToStr converts bool to "true"/"false" string for template placeholders.
func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// parseExecute extracts the <decision> JSON block from an execute AI response.
// Same format as parseDecisionJSON but also maps direction.
func parseExecute(raw, pair string) (ScoreResult, error) {
	// Strip markdown code fences
	cleaned := strings.ReplaceAll(raw, "```json", "")
	cleaned = strings.ReplaceAll(cleaned, "```", "")

	re := regexp.MustCompile(`(?s)<decision>(.*?)</decision>`)
	matches := re.FindStringSubmatch(cleaned)
	if len(matches) >= 2 {
		jsonStr := strings.TrimSpace(matches[1])
		return parseExecuteJSON(jsonStr, pair)
	}

	// Fallback: raw JSON without <decision> tags
	reJSON := regexp.MustCompile(`(?s)\{[^{]*"(?:direction|action)"\s*:\s*"[^"]+"[^}]*\}`)
	matchesJSON := reJSON.FindString(cleaned)
	if matchesJSON != "" {
		slog.Info("scorer: execute response has no <decision> tags — using raw JSON", "pair", pair)
		return parseExecuteJSON(matchesJSON, pair)
	}

	slog.Warn("scorer: no <decision> block in execute response — defaulting long", "pair", pair)
	return ScoreResult{
		Symbol:    pair,
		Action:    "open_long",
		Direction: "long",
		Reasoning: "no decision block in AI response, defaulted to long",
	}, nil
}

// parseExecuteJSON unmarshals a JSON decision block with direction field.
func parseExecuteJSON(jsonStr, pair string) (ScoreResult, error) {
	jsonStr = strings.TrimSpace(jsonStr)

	var d struct {
		Symbol          string  `json:"symbol"`
		Action          string  `json:"action"`
		Direction       string  `json:"direction"`
		Leverage        int     `json:"leverage"`
		PositionSizeUSD float64 `json:"position_size_usd"`
		EntryPrice      float64 `json:"entry_price"`
		StopLoss        float64 `json:"stop_loss"`
		TakeProfit      float64 `json:"take_profit"`
		Confidence      float64 `json:"confidence"`
		Strategy        string  `json:"strategy"`
		ConfluenceCount int     `json:"confluence_count"`
		RRRatio         float64 `json:"rr_ratio"`
		Reasoning       string  `json:"reasoning"`

		Trend        string `json:"trend"`
		EntryQuality string `json:"entry_quality"`
		SLReasoning  string `json:"sl_reasoning"`
		TPReasoning  string `json:"tp_reasoning"`
		Invalidation string `json:"invalidation"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: unmarshal execute JSON: %w", err)
	}

	// Map direction to action
	action := d.Action
	if d.Action == "" {
		switch strings.ToLower(d.Direction) {
		case "long":
			action = "open_long"
		case "short":
			action = "open_short"
		default:
			action = "open_long"
		}
	}

	direction := strings.ToLower(d.Direction)
	if direction == "" {
		if action == "open_short" {
			direction = "short"
		} else {
			direction = "long"
		}
	}

	return ScoreResult{
		Symbol:          pair,
		Action:          action,
		Direction:       direction,
		Leverage:        d.Leverage,
		PositionSizeUSD: d.PositionSizeUSD,
		StopLoss:        d.StopLoss,
		TakeProfit:      d.TakeProfit,
		Confidence:      d.Confidence,
		Strategy:        d.Strategy,
		ConfluenceCount: d.ConfluenceCount,
		RRRatio:         d.RRRatio,
		Reasoning:       d.Reasoning,

		Trend:        d.Trend,
		EntryQuality: d.EntryQuality,
		SLReasoning:  d.SLReasoning,
		TPReasoning:  d.TPReasoning,
		Invalidation: d.Invalidation,
	}, nil
}
