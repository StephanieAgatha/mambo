package ai

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/object"

	"mambo/config"
	"mambo/journal"
	"mambo/ta"
)

// PositionDecision is the AI's decision for an open position.
type PositionDecision struct {
	Action    string  `json:"action"`    // "hold" / "close" / "move_sl" / "move_tp"
	NewSL     float64 `json:"new_sl"`     // only used when Action == "move_sl"
	NewTP     float64 `json:"new_tp"`     // only used when Action == "move_tp"
	Reasoning string  `json:"reasoning"`
}

// PositionManager sends live position state to the configured AI and parses its decision.
type PositionManager struct {
	agentPrompt string
	model       fantasy.LanguageModel
	cfg         *config.Config
}

// NewPositionManager loads AGENT.md once at startup.
func NewPositionManager(cfg *config.Config) (*PositionManager, error) {
	data, err := os.ReadFile("AGENT.md")
	if err != nil {
		return nil, fmt.Errorf("position_manager: read AGENT.md: %w", err)
	}

	model, err := NewModel(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("position_manager: create fantasy model: %w", err)
	}

	slog.Info("position manager initialized",
		"provider", cfg.AIProvider,
		"model", cfg.AIModel,
	)

	return &PositionManager{
		agentPrompt: string(data),
		model:       model,
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

	result, err := object.Generate[PositionDecision](ctx, pm.model, fantasy.ObjectCall{
		Prompt:            fantasy.Prompt{fantasy.NewSystemMessage(pm.agentPrompt), fantasy.NewUserMessage(prompt)},
		SchemaName:        "position_decision",
		SchemaDescription: "A position management decision: hold, close, move_sl, or move_tp",
	})
	if err != nil {
		slog.Warn("position_manager: AI call failed — defaulting to hold",
			"pos_id", pos.ID,
			"err", err,
		)
		return PositionDecision{Action: "hold", Reasoning: "AI call failed — holding"}, nil
	}

	decision := pm.validate(result.Object, pos)

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
- RSI Divergence   : %s (%d bars ago)
- MACD Divergence  : %s (%d bars ago)
- Double Divergence: %v

DECISION RULES:
- Never move SL below entry (long) or above entry (short)
- Never move TP beyond 2x original TP distance from entry
- RSI > 74 + PnL > 5%% → consider moving SL up to lock profit
- MACD histogram shrinking + PnL > 3%% → consider closing early
- Price approaching strong resistance + PnL > 3%% → consider closing or moving TP down
- Price bouncing off support again → hold, can tighten SL above support
- If in noise zone (-1.5%% to +1.5%%) → hold unless very high confidence
- Hidden bullish divergence + long in drawdown → hold, momentum likely recovering
- Hidden bearish divergence + short in drawdown → hold, momentum likely recovering
- Regular bearish divergence + long near TP + PnL > 3%% → close early, momentum fading
- Regular bullish divergence + short near TP + PnL > 3%% → close early, momentum fading
- Double divergence (RSI + MACD agree) → weight this signal heavily in your decision

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
		string(snap.RSIDivergence), snap.RSIDivBarsAgo,
		string(snap.MACDDivergence), snap.MACDDivBarsAgo,
		snap.DoubleDivergence,
	)
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
