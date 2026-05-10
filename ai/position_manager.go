package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"mambo/config"
	"mambo/journal"
	"mambo/ta"
)

// PositionDecision is the AI's decision for an open position.
type PositionDecision struct {
	Action    string  // "hold" / "close" / "move_sl" / "move_tp"
	NewSL     float64 // only used when Action == "move_sl"
	NewTP     float64 // only used when Action == "move_tp"
	Reasoning string
}

// PositionManager sends live position state to the configured AI and parses its decision.
type PositionManager struct {
	agentPrompt string
	client      *Client
	cfg         *config.Config
}

// NewPositionManager loads AGENT.md once at startup.
func NewPositionManager(cfg *config.Config) (*PositionManager, error) {
	data, err := os.ReadFile("AGENT.md")
	if err != nil {
		return nil, fmt.Errorf("position_manager: read AGENT.md: %w", err)
	}

	// position decisions are simpler — shorter timeout than trade scoring
	client := NewClient(cfg, 60*time.Second)

	slog.Info("position manager initialized",
		"provider", cfg.AIProvider,
		"model", cfg.AIModel,
	)

	return &PositionManager{
		agentPrompt: string(data),
		client:      client,
		cfg:         cfg,
	}, nil
}

// Decide sends position state to the configured AI provider and returns a validated decision.
// Hard rules are enforced by monitor/position.go before this is called.
func (pm *PositionManager) Decide(
	ctx context.Context,
	pos journal.OpenPosition,
	currentPrice float64,
	currentPnLPct float64,
	snap ta.TAResult,
) (PositionDecision, error) {
	prompt := pm.buildPrompt(pos, currentPrice, currentPnLPct, snap)

	slog.Debug("position manager: calling AI",
		"provider", pm.cfg.AIProvider,
		"model", pm.cfg.AIModel,
		"pos_id", pos.ID,
		"pair", pos.Pair,
		"pnl_pct", fmt.Sprintf("%.2f%%", currentPnLPct),
	)

	raw, err := pm.client.Chat(ctx, pm.agentPrompt, prompt)
	if err != nil {
		return PositionDecision{}, fmt.Errorf("position_manager: AI call failed pos=%s: %w", pos.ID, err)
	}

	decision, err := parsePositionDecision(raw)
	if err != nil {
		// parse failure → safe fallback: hold
		slog.Warn("position_manager: parse failed — defaulting to hold",
			"pos_id", pos.ID,
			"provider", pm.cfg.AIProvider,
			"err", err,
		)
		return PositionDecision{Action: "hold", Reasoning: "parse error — holding"}, nil
	}

	// validate — AI cannot bypass hard constraints
	decision = pm.validate(decision, pos)

	slog.Debug("position decision",
		"pos_id", pos.ID,
		"pair", pos.Pair,
		"provider", pm.cfg.AIProvider,
		"action", decision.Action,
		"new_sl", decision.NewSL,
		"new_tp", decision.NewTP,
		"reasoning", decision.Reasoning,
	)

	return decision, nil
}

// buildPrompt constructs a concise position state prompt for the AI.
func (pm *PositionManager) buildPrompt(
	pos journal.OpenPosition,
	currentPrice float64,
	currentPnLPct float64,
	snap ta.TAResult,
) string {
	openedAt, _ := time.Parse(time.RFC3339, pos.OpenedAt)
	holdMinutes := time.Since(openedAt).Minutes()

	return fmt.Sprintf(`You are managing an open Hyperliquid futures position.
Evaluate the current state and decide what to do next.

POSITION STATE:
- Pair        : %s
- Direction   : %s
- Entry       : $%.4f
- Current     : $%.4f
- PnL         : %.2f%%
- Peak PnL    : %.2f%%
- Hold time   : %.0f minutes
- SL          : $%.4f
- TP          : $%.4f
- Leverage    : %dx cross
- Strategy    : %s

LIVE TA SNAPSHOT:
- Price vs EMA9/21 : $%.4f / $%.4f (%s)
- RSI              : %.2f (%s)
- MACD histogram   : %.4f
- ATR              : $%.4f (%s)
- Support          : $%.4f (%s)
- Resistance       : $%.4f (%s)
- At support       : %v
- Near resistance  : %v

DECISION RULES:
- Never move SL below entry (long) or above entry (short)
- Never move TP beyond 2x original TP distance from entry
- RSI > 74 + PnL > 5%% → consider moving SL up to lock profit
- MACD histogram shrinking + PnL > 3%% → consider closing early
- Price approaching strong resistance + PnL > 3%% → consider closing or moving TP down
- Price bouncing off support again → hold, can tighten SL above support
- If in noise zone (-1.5%% to +1.5%%) → hold unless very high confidence

Respond ONLY in this exact format:
<decision>
{
  "action": "hold | close | move_sl | move_tp",
  "new_sl": 0.0,
  "new_tp": 0.0,
  "reasoning": "one sentence, direct"
}
</decision>`,
		pos.Pair, pos.Direction,
		pos.EntryPrice, currentPrice,
		currentPnLPct, pos.PeakPnLPct,
		holdMinutes,
		pos.StopLoss, pos.TakeProfit,
		pos.Leverage, pos.Strategy,
		snap.EMA9, snap.EMA21, snap.RibbonStatus,
		snap.RSI, snap.RSIZone,
		snap.MACDHistogram,
		snap.ATR, snap.ATRLevel,
		snap.NearestSupport, snap.SupportStrength,
		snap.NearestResistance, snap.ResistanceStrength,
		snap.AtSupport, snap.NearResistance,
	)
}

// parsePositionDecision extracts the <decision> JSON block from AI response.
func parsePositionDecision(raw string) (PositionDecision, error) {
	re := regexp.MustCompile(`(?s)<decision>(.*?)</decision>`)
	matches := re.FindStringSubmatch(raw)
	if len(matches) < 2 {
		return PositionDecision{}, fmt.Errorf("no <decision> block in response")
	}

	jsonStr := strings.TrimSpace(matches[1])

	var parsed struct {
		Action    string  `json:"action"`
		NewSL     float64 `json:"new_sl"`
		NewTP     float64 `json:"new_tp"`
		Reasoning string  `json:"reasoning"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return PositionDecision{}, fmt.Errorf("unmarshal decision JSON: %w", err)
	}

	return PositionDecision{
		Action:    parsed.Action,
		NewSL:     parsed.NewSL,
		NewTP:     parsed.NewTP,
		Reasoning: parsed.Reasoning,
	}, nil
}

// validate enforces hard constraints on AI position decisions.
func (pm *PositionManager) validate(d PositionDecision, pos journal.OpenPosition) PositionDecision {
	if d.Action == "move_sl" && d.NewSL > 0 {
		if pos.Direction == "long" && d.NewSL < pos.EntryPrice {
			slog.Warn("position_manager: SL below entry rejected",
				"pos_id", pos.ID,
				"suggested_sl", d.NewSL,
				"entry", pos.EntryPrice,
				"provider", pm.cfg.AIProvider,
			)
			d.Action = "hold"
			d.Reasoning = "SL below entry rejected — holding"
		}
		if pos.Direction == "short" && d.NewSL > pos.EntryPrice {
			slog.Warn("position_manager: SL above entry rejected for short",
				"pos_id", pos.ID,
				"suggested_sl", d.NewSL,
				"entry", pos.EntryPrice,
			)
			d.Action = "hold"
			d.Reasoning = "SL above entry rejected for short — holding"
		}
	}

	if d.Action == "move_tp" && d.NewTP > 0 {
		originalDist := pos.TakeProfit - pos.EntryPrice
		maxTP := pos.EntryPrice + (originalDist * 2)
		if pos.Direction == "long" && d.NewTP > maxTP {
			slog.Warn("position_manager: TP beyond 2x clamped",
				"suggested_tp", d.NewTP,
				"max_tp", maxTP,
			)
			d.NewTP = maxTP
		}
	}

	return d
}
