package ai

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/object"

	"mambo/config"
	"mambo/filter"
	"mambo/market"
	"mambo/ta"
)

// ScoreResult holds the parsed AI decision for a trade setup.
// JSON tags enable auto-schema generation by Fantasy's object.Generate.
type ScoreResult struct {
	Symbol          string  `json:"symbol"`
	Action          string  `json:"action"`           // "open_long" / "open_short" / "hold" / "wait"
	Direction       string  `json:"direction"`        // "long" / "short" — used by /execute flow
	Leverage        int     `json:"leverage"`
	PositionSizeUSD float64 `json:"position_size_usd"`
	StopLoss        float64 `json:"stop_loss"`
	TakeProfit      float64 `json:"take_profit"`
	Confidence      float64 `json:"confidence"`
	Strategy        string  `json:"strategy"`
	ConfluenceCount int     `json:"confluence_count"`
	RRRatio         float64 `json:"rr_ratio"`
	Reasoning       string  `json:"reasoning"`

	// ── New in v2 ─────────────────────────────────────────────────────
	Trend        string `json:"trend"`         // "UPTREND" | "DOWNTREND" | "RANGING"
	EntryQuality string `json:"entry_quality"` // "AT_STRUCTURE" | "PULLBACK" | "BREAKOUT" | "CHASE" | "WAIT"
	SLReasoning  string `json:"sl_reasoning"`  // structural justification for SL placement
	TPReasoning  string `json:"tp_reasoning"`  // structural justification for TP placement
	Invalidation string `json:"invalidation"`  // price or action that immediately invalidates the trade
}

// Scorer sends trade setups to the configured AI provider and returns structured decisions.
type Scorer struct {
	agentPrompt   string
	template      string
	suggestPrompt string
	executePrompt string
	model         fantasy.LanguageModel
	cfg           *config.Config
}

// NewScorer loads AGENT.md and prompt files once at startup.
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

	model, err := NewModel(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("scorer: create fantasy model: %w", err)
	}

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
		model:         model,
		cfg:           cfg,
	}, nil
}

// Score fills the verify_trade.md template, sends to AI via structured output,
// and returns a validated + clamped ScoreResult.
func (s *Scorer) Score(
	ctx context.Context,
	pair string,
	taResult ta.TAResult,
	mc market.MarketContext,
	state filter.BotState,
) (ScoreResult, error) {
	prompt := s.fillTemplate(pair, taResult, mc, state)

	slog.Debug("sending to AI via structured output",
		"provider", s.cfg.AIProvider,
		"model", s.cfg.AIModel,
		"pair", pair,
	)

	result, err := object.Generate[ScoreResult](ctx, s.model, fantasy.ObjectCall{
		Prompt:            fantasy.Prompt{fantasy.NewSystemMessage(s.agentPrompt), fantasy.NewUserMessage(prompt)},
		SchemaName:        "trade_decision",
		SchemaDescription: "A trading decision with action, sizing, and structured reasoning",
	})
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: AI call failed pair=%s: %w", pair, err)
	}

	clamped := s.validateAndClamp(result.Object, state)

	slog.Info("AI score complete",
		"provider", s.cfg.AIProvider,
		"pair", pair,
		"action", clamped.Action,
		"confidence", clamped.Confidence,
		"size_usd", clamped.PositionSizeUSD,
		"leverage", clamped.Leverage,
		"rr_ratio", clamped.RRRatio,
		"strategy", clamped.Strategy,
		"trend", clamped.Trend,
		"entry_quality", clamped.EntryQuality,
		"rsi_div", string(taResult.RSIDivergence),
		"macd_div", string(taResult.MACDDivergence),
		"double_div", taResult.DoubleDivergence,
	)

	return clamped, nil
}

// Suggest runs the advisory-only prompt — no clamping, no guardrails.
func (s *Scorer) Suggest(
	ctx context.Context,
	pair string,
	taResult ta.TAResult,
	mc market.MarketContext,
	state filter.BotState,
	prefilterReason string,
) (ScoreResult, error) {
	prompt := s.fillTemplate(pair, taResult, mc, state)
	prompt = strings.ReplaceAll(prompt, "{{PREFILTER_REASON}}", prefilterReason)

	slog.Debug("sending suggestion request to AI",
		"provider", s.cfg.AIProvider,
		"model", s.cfg.AIModel,
		"pair", pair,
	)

	result, err := object.Generate[ScoreResult](ctx, s.model, fantasy.ObjectCall{
		Prompt:            fantasy.Prompt{fantasy.NewSystemMessage(s.suggestPrompt), fantasy.NewUserMessage(prompt)},
		SchemaName:        "trade_suggestion",
		SchemaDescription: "An advisory trade suggestion with reasoning",
	})
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: AI suggest call failed pair=%s: %w", pair, err)
	}

	slog.Info("AI suggestion complete",
		"provider", s.cfg.AIProvider,
		"pair", pair,
		"direction", result.Object.Action,
		"confidence", result.Object.Confidence,
		"size_usd", result.Object.PositionSizeUSD,
		"leverage", result.Object.Leverage,
		"rr_ratio", result.Object.RRRatio,
		"strategy", result.Object.Strategy,
	)

	return result.Object, nil
}

// Execute runs the direct execution prompt — forces a directional decision.
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

	result, err := object.Generate[ScoreResult](ctx, s.model, fantasy.ObjectCall{
		Prompt:            fantasy.Prompt{fantasy.NewSystemMessage(s.executePrompt), fantasy.NewUserMessage(prompt)},
		SchemaName:        "trade_execution",
		SchemaDescription: "A forced directional trade execution decision with sizing",
	})
	if err != nil {
		return ScoreResult{}, fmt.Errorf("scorer: AI execute call failed pair=%s: %w", pair, err)
	}

	clamped := s.clampExecute(result.Object, state)

	if clamped.Direction == "" {
		if clamped.Action == "open_long" {
			clamped.Direction = "long"
		} else if clamped.Action == "open_short" {
			clamped.Direction = "short"
		} else {
			clamped.Direction = "long"
		}
	}

	slog.Info("AI execute complete",
		"provider", s.cfg.AIProvider,
		"pair", pair,
		"direction", clamped.Direction,
		"confidence", clamped.Confidence,
		"size_usd", clamped.PositionSizeUSD,
		"leverage", clamped.Leverage,
		"rr_ratio", clamped.RRRatio,
		"strategy", clamped.Strategy,
		"entry_quality", clamped.EntryQuality,
	)

	return clamped, nil
}

// validateAndClamp enforces all guardrails on AI output.
func (s *Scorer) validateAndClamp(result ScoreResult, state filter.BotState) ScoreResult {
	isTradeAction := result.Action == "open_long" || result.Action == "open_short"

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

	if !isTradeAction {
		return result
	}

	if result.Leverage < config.MinLeverageX {
		slog.Debug("scorer: clamp leverage up", "from", result.Leverage, "to", config.MinLeverageX)
		result.Leverage = config.MinLeverageX
	}
	if result.Leverage > config.MaxLeverageX {
		slog.Debug("scorer: clamp leverage down", "from", result.Leverage, "to", config.MaxLeverageX)
		result.Leverage = config.MaxLeverageX
	}

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

	remaining := filter.RemainingBudget(state)
	if result.PositionSizeUSD > remaining {
		slog.Debug("scorer: clamp to remaining budget",
			"from", result.PositionSizeUSD, "to", remaining)
		result.PositionSizeUSD = remaining
	}

	result.PositionSizeUSD = math.Round(result.PositionSizeUSD*100) / 100
	return result
}

// clampExecute forces sane values for /execute (never changes action to "wait").
func (s *Scorer) clampExecute(result ScoreResult, state filter.BotState) ScoreResult {
	if result.Leverage < config.MinLeverageX {
		slog.Debug("execute: clamp leverage up", "from", result.Leverage, "to", config.MinLeverageX)
		result.Leverage = config.MinLeverageX
	}
	if result.Leverage > config.MaxLeverageX {
		slog.Debug("execute: clamp leverage down", "from", result.Leverage, "to", config.MaxLeverageX)
		result.Leverage = config.MaxLeverageX
	}

	minSize := state.Balance * config.MinSizePct
	if result.PositionSizeUSD < minSize {
		slog.Debug("execute: clamp size up", "from", result.PositionSizeUSD, "to", minSize)
		result.PositionSizeUSD = minSize
	}
	maxSize := state.Balance * config.MaxSizePct
	if result.PositionSizeUSD > maxSize {
		slog.Debug("execute: clamp size down", "from", result.PositionSizeUSD, "to", maxSize)
		result.PositionSizeUSD = maxSize
	}

	remaining := filter.RemainingBudget(state)
	if result.PositionSizeUSD > remaining {
		slog.Debug("execute: clamp to remaining budget", "from", result.PositionSizeUSD, "to", remaining)
		result.PositionSizeUSD = remaining
	}

	result.PositionSizeUSD = math.Round(result.PositionSizeUSD*100) / 100

	if result.Confidence <= 0 {
		result.Confidence = 50
	}

	return result
}

// fillTemplate replaces all {{PLACEHOLDERS}} in the prompt template with real values.
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
		"{{EMA9}}", fmt.Sprintf("%.4f", r.EMA9),
		"{{EMA21}}", fmt.Sprintf("%.4f", r.EMA21),
		"{{EMA50}}", fmt.Sprintf("%.4f", r.EMA50),
		"{{EMA200}}", fmt.Sprintf("%.4f", r.EMA200),
		"{{EMA_SPREAD}}", fmt.Sprintf("%.3f", r.EMASpread),
		"{{RIBBON_STATUS}}", r.RibbonStatus,
		"{{DAILY_TREND}}", r.DailyTrend,
		"{{RSI}}", fmt.Sprintf("%.2f", r.RSI),
		"{{RSI_ZONE}}", r.RSIZone,
		"{{MACD_VALUE}}", fmt.Sprintf("%.4f", r.MACDValue),
		"{{MACD_SIGNAL}}", fmt.Sprintf("%.4f", r.MACDSignal),
		"{{MACD_HISTOGRAM}}", fmt.Sprintf("%.4f", r.MACDHistogram),
		"{{MACD_CROSS}}", r.MACDCross,
		"{{ATR}}", fmt.Sprintf("%.4f", r.ATR),
		"{{ATR_LEVEL}}", r.ATRLevel,
		"{{BB_UPPER}}", fmt.Sprintf("%.4f", r.BBUpper),
		"{{BB_MIDDLE}}", fmt.Sprintf("%.4f", r.BBMiddle),
		"{{BB_LOWER}}", fmt.Sprintf("%.4f", r.BBLower),
		"{{BB_WIDTH}}", r.BBWidth,
		"{{BB_POSITION}}", r.BBPosition,
		"{{VOLUME_MULTIPLIER}}", fmt.Sprintf("%.2f", r.VolumeMultiplier),
		"{{VOLUME_CONFIRMS}}", boolToStr(r.VolumeConfirms),
		"{{NEAREST_SUPPORT}}", fmt.Sprintf("%.4f", r.NearestSupport),
		"{{SUPPORT_STRENGTH}}", r.SupportStrength,
		"{{AT_SUPPORT}}", boolToStr(r.AtSupport),
		"{{NEAREST_RESISTANCE}}", fmt.Sprintf("%.4f", r.NearestResistance),
		"{{RESISTANCE_STRENGTH}}", r.ResistanceStrength,
		"{{NEAR_RESISTANCE}}", boolToStr(r.NearResistance),
		"{{SL_1ATR}}", fmt.Sprintf("%.4f", r.SL1ATR),
		"{{SL_15ATR}}", fmt.Sprintf("%.4f", r.SL15ATR),
		"{{RSI_DIVERGENCE}}", string(r.RSIDivergence),
		"{{RSI_DIV_BARS_AGO}}", fmt.Sprintf("%d", r.RSIDivBarsAgo),
		"{{MACD_DIVERGENCE}}", string(r.MACDDivergence),
		"{{MACD_DIV_BARS_AGO}}", fmt.Sprintf("%d", r.MACDDivBarsAgo),
		"{{DOUBLE_DIVERGENCE}}", boolToStr(r.DoubleDivergence),
		"{{OBV_TREND}}", r.OBVTrend,

		// market context
		"{{FEAR_GREED}}", fmt.Sprintf("%d", mc.FearGreedValue),
		"{{FEAR_GREED_ZONE}}", mc.FearGreedZone,
		"{{FUNDING_RATE}}", fmt.Sprintf("%.6f", mc.FundingRate),
		"{{FUNDING_BIAS}}", mc.FundingBias,
		"{{LONG_SHORT_RATIO}}", fmt.Sprintf("%.2f", mc.LongShortRatio),
		"{{LONG_SHORT_BIAS}}", mc.LongShortBias,
		"{{OI_CHANGE}}", mc.OIChange,
	)

	return replacer.Replace(s.template)
}

// boolToStr converts bool to "true"/"false" string for template placeholders.
func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}