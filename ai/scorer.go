package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
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
	Action          string // "open_long" / "open_short" / "hold" / "wait"
	Leverage        int
	PositionSizeUSD float64
	StopLoss        float64
	TakeProfit      float64
	Confidence      float64
	Strategy        string
	ConfluenceCount int
	RRRatio         float64
	Reasoning       string
}

// Scorer sends trade setups to the configured AI provider and parses the response.
type Scorer struct {
	agentPrompt string
	template    string
	client      *Client
	cfg         *config.Config
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

	// reasoning models can take longer — generous timeout
	client := NewClient(cfg, 120*time.Second)

	slog.Info("scorer initialized",
		"provider", cfg.AIProvider,
		"model", cfg.AIModel,
		"agent_prompt_len", len(agentPrompt),
		"template_len", len(template),
	)

	return &Scorer{
		agentPrompt: string(agentPrompt),
		template:    string(template),
		client:      client,
		cfg:         cfg,
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
	)

	return result, nil
}

// parseDecision extracts the <decision> JSON block from AI response.
func parseDecision(raw, pair string) (ScoreResult, error) {
	re := regexp.MustCompile(`(?s)<decision>(.*?)</decision>`)
	matches := re.FindStringSubmatch(raw)
	if len(matches) < 2 {
		slog.Warn("scorer: no <decision> block found — defaulting to wait", "pair", pair)
		return ScoreResult{
			Symbol:    pair,
			Action:    "wait",
			Reasoning: "no decision block in AI response",
		}, nil
	}

	jsonStr := strings.TrimSpace(matches[1])

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
	}, nil
}

// validateAndClamp enforces all guardrails on AI output.
// AI cannot bypass these — they are enforced in Go code.
func (s *Scorer) validateAndClamp(result ScoreResult, state filter.BotState) ScoreResult {
	// confidence below minimum → force wait
	if result.Confidence < config.MinConfidence {
		slog.Warn("scorer: confidence below minimum — forcing wait",
			"confidence", result.Confidence,
			"min", config.MinConfidence,
			"provider", s.cfg.AIProvider,
		)
		result.Action = "wait"
		result.Reasoning = fmt.Sprintf("confidence %.0f < minimum %d — aborted",
			result.Confidence, config.MinConfidence)
		return result
	}

	// R:R below minimum → force wait
	if result.RRRatio < config.MinRiskReward {
		slog.Warn("scorer: R:R below minimum — forcing wait",
			"rr_ratio", result.RRRatio,
			"min", config.MinRiskReward,
		)
		result.Action = "wait"
		result.Reasoning = fmt.Sprintf("R:R %.2f < minimum %.1f — aborted",
			result.RRRatio, config.MinRiskReward)
		return result
	}

	// non-trade actions don't need size/leverage clamping
	if result.Action == "wait" || result.Action == "hold" {
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
		"{{RSI_DIVERGENCE}}", r.RSIDivergence,

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
		"{{MACD_DIVERGENCE}}", r.MACDDivergence,

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
