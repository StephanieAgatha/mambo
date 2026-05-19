# ai-changes.md — AI Package Changes for v2

## Overview

Changes needed in `ai/scorer.go` and `ai/position_manager.go` to support:
1. New divergence fields from `ta/divergence.go`
2. New prompt fields from updated `verify_trade.md` and `suggest_trade.md`
3. New `ScoreResult` fields for richer trade context

**Dependency:** `ta/divergence.go` must be implemented first.
New `TAResult` fields (`RSIDivergence`, `MACDDivergence`, `RSIDivBarsAgo`, `MACDDivBarsAgo`, `DoubleDivergence`, `DoubleDivergenceType`) must exist before these changes compile.

---

## 1. `ai/scorer.go`

### 1.1 `ScoreResult` — Add 5 New Fields

Current struct only has core trade fields. Add context fields that the updated prompts now return:

```go
// ScoreResult holds the parsed AI decision for a trade setup.
type ScoreResult struct {
    Symbol          string
    Action          string  // "open_long" / "open_short" / "hold" / "wait"
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
```

---

### 1.2 `parseDecision()` — Parse New Fields

Current `parseDecision` only parses 10 fields. Add the 5 new ones:

```go
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
```

---

### 1.3 `parseSuggestion()` — Parse New Fields

`suggest_trade.md` now returns `trend`, `entry_quality`, `sl_placement`, `tp_placement` in the `<suggestion>` block:

```go
func parseSuggestion(raw, pair string) (ScoreResult, error) {
    cleaned := strings.ReplaceAll(raw, "```json", "")
    cleaned = strings.ReplaceAll(cleaned, "```", "")

    re := regexp.MustCompile(`(?s)<suggestion>(.*?)</suggestion>`)
    matches := re.FindStringSubmatch(cleaned)
    if len(matches) < 2 {
        slog.Warn("scorer: no <suggestion> block found — returning fallback",
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

    jsonStr := strings.TrimSpace(matches[1])

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

        // ── New in v2 ─────────────────────────────────────
        Trend        string `json:"trend"`
        EntryQuality string `json:"entry_quality"`
        SLPlacement  string `json:"sl_placement"`  // suggest_trade uses sl_placement
        TPPlacement  string `json:"tp_placement"`  // suggest_trade uses tp_placement
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

        // New — note: sl_placement maps to SLReasoning for consistency
        Trend:        d.Trend,
        EntryQuality: d.EntryQuality,
        SLReasoning:  d.SLPlacement,
        TPReasoning:  d.TPPlacement,
    }, nil
}
```

---

### 1.4 `fillTemplate()` — Add Divergence Placeholders

Current `fillTemplate` already handles `{{RSI_DIVERGENCE}}` and `{{MACD_DIVERGENCE}}` as plain strings.
Update these and add 5 new placeholders for the new divergence fields.

Add the following to the `strings.NewReplacer(...)` call in `fillTemplate()`:

```go
// Replace these existing entries:
// OLD:  "{{RSI_DIVERGENCE}}",  r.RSIDivergence,   // was a plain string field
// OLD:  "{{MACD_DIVERGENCE}}", r.MACDDivergence,  // was a plain string field

// NEW — after ta/divergence.go is implemented:
"{{RSI_DIVERGENCE}}",    boolToStr(r.RSIDivergence != ta.DivNone),
"{{RSI_DIV_TYPE}}",      string(r.RSIDivergence),
"{{RSI_DIV_BARS}}",      strconv.Itoa(r.RSIDivBarsAgo),
"{{MACD_DIVERGENCE}}",   boolToStr(r.MACDDivergence != ta.DivNone),
"{{MACD_DIV_TYPE}}",     string(r.MACDDivergence),
"{{MACD_DIV_BARS}}",     strconv.Itoa(r.MACDDivBarsAgo),
"{{DIVERGENCE_SIGNAL}}", divergenceSignalStr(r),

// Also add SR_ENTRY_SIGNAL for the confluence table row:
"{{SR_ENTRY_SIGNAL}}", srEntrySignal(r),
```

**Note on imports:** Add `strconv` to the imports in `scorer.go`.

---

### 1.5 New Helper Functions

Add these private helpers at the bottom of `scorer.go`:

```go
// divergenceSignalStr returns the confluence table value for the divergence row.
// Used to fill {{DIVERGENCE_SIGNAL}} in verify_trade.md.
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
// AT_STRUCTURE = price at support (long) or resistance (short) — highest quality.
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
```

> `boolToStr` may already exist in your codebase under a different name — check before adding.

---

### 1.6 Log Divergence in `Score()`

After the `slog.Info("AI score complete", ...)` call, add divergence context:

```go
slog.Info("AI score complete",
    "provider", s.cfg.AIProvider,
    "pair", pair,
    "action", result.Action,
    "confidence", result.Confidence,
    "size_usd", result.PositionSizeUSD,
    "leverage", result.Leverage,
    "rr_ratio", result.RRRatio,
    "strategy", result.Strategy,
    "trend", result.Trend,           // new
    "entry_quality", result.EntryQuality, // new
    "rsi_div", string(taResult.RSIDivergence),  // new
    "macd_div", string(taResult.MACDDivergence), // new
    "double_div", taResult.DoubleDivergence,     // new
)
```

> `Score()` signature already receives `taResult ta.TAResult` so these fields are available.

---

## 2. `ai/position_manager.go`

### 2.1 `buildPrompt()` — Add Divergence to TA Snapshot

Current prompt already includes support/resistance and RSI/MACD. Add divergence lines to the `LIVE TA SNAPSHOT` section:

```go
// Current buildPrompt format string — add these lines after the resistance block:
"- RSI Divergence   : %s (%d bars ago)\n" +
"- MACD Divergence  : %s (%d bars ago)\n" +
"- Double Divergence: %v\n",

// Corresponding args — add after snap.NearResistance args:
string(snap.RSIDivergence),  snap.RSIDivBarsAgo,
string(snap.MACDDivergence), snap.MACDDivBarsAgo,
snap.DoubleDivergence,
```

### 2.2 `buildPrompt()` — Add Divergence to DECISION RULES

The position manager AI needs to know how to USE divergence when deciding whether to hold or close:

```go
// Add to DECISION RULES section in buildPrompt:
"- Hidden bullish divergence forming + long position in drawdown → hold, momentum recovering\n" +
"- Hidden bearish divergence forming + short position in drawdown → hold, momentum recovering\n" +
"- Regular bearish divergence + long position near TP + PnL > 3%% → consider closing early\n" +
"- Regular bullish divergence + short position near TP + PnL > 3%% → consider closing early\n" +
"- Double divergence (RSI + MACD same type) → high conviction signal, weight heavily\n",
```

Full updated `buildPrompt` format string:

```go
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
    // New divergence args:
    string(snap.RSIDivergence),  snap.RSIDivBarsAgo,
    string(snap.MACDDivergence), snap.MACDDivBarsAgo,
    snap.DoubleDivergence,
)
```

---

## 3. Modified Files Summary

| File | Changes |
|---|---|
| `ai/scorer.go` | `ScoreResult` +5 fields; `parseDecision` +5 fields; `parseSuggestion` +4 fields; `fillTemplate` +7 placeholders; 3 new helper functions; `Score()` log updated |
| `ai/position_manager.go` | `buildPrompt` +3 divergence lines in TA snapshot; +5 divergence rules in DECISION RULES |

## 4. New Imports Needed

```go
// ai/scorer.go — add:
"strconv"
"mambo/ta" // already imported via ta.TAResult, but DivNone needs ta.DivNone
```

---

## 5. Implementation Order

```
1. ta/divergence.go          ← implement DetectDivergence, types (see ta-divergence.md)
2. ta/indicators.go          ← add new TAResult fields, wire DetectDivergence into Calculate()
3. ai/scorer.go              ← add ScoreResult fields, fillTemplate placeholders, helpers
4. ai/position_manager.go    ← add divergence lines to buildPrompt
5. prompts/verify_trade.md   ← already done ✅
6. prompts/suggest_trade.md  ← already done ✅
```

> Do NOT update scorer.go before ta/divergence.go compiles —
> `ta.DivNone`, `r.RSIDivergence`, `r.DoubleDivergence` won't exist yet.