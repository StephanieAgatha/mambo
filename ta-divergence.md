# ta-divergence.md — Divergence Detection Implementation

## Overview

Implement divergence detection as a new file `ta/divergence.go`.
Results are added to `TAResult` in `ta/indicators.go` and consumed by `ai/scorer.go` and `ai/position_manager.go`.

**Dependency chain:**
```
ta/divergence.go → ta/indicators.go (TAResult) → ai/scorer.go → ai/position_manager.go
```

---

## 1. Divergence Types

| Constant | Price | Oscillator | Meaning | Signal |
|---|---|---|---|---|
| `DivRegularBullish` | Lower Low | Higher Low | Downtrend exhaustion → reversal up | Counter-trend LONG, small size |
| `DivRegularBearish` | Higher High | Lower High | Uptrend exhaustion → reversal down | Counter-trend SHORT, small size |
| `DivHiddenBullish` | Higher Low | Lower Low | Uptrend continuation, pullback over | Trend LONG, size up 1 tier |
| `DivHiddenBearish` | Lower High | Higher High | Downtrend continuation, bounce over | Trend SHORT, size up 1 tier |
| `DivNone` | — | — | No divergence detected | Neutral |

---

## 2. New File: `ta/divergence.go`

```go
package ta

// DivergenceType classifies the type of divergence detected between price and an oscillator.
type DivergenceType string

const (
    DivNone           DivergenceType = "none"
    DivRegularBullish DivergenceType = "regular_bullish"
    DivRegularBearish DivergenceType = "regular_bearish"
    DivHiddenBullish  DivergenceType = "hidden_bullish"
    DivHiddenBearish  DivergenceType = "hidden_bearish"
)

// DivergenceResult holds the detected divergence for a single oscillator.
type DivergenceResult struct {
    Type    DivergenceType
    BarsAgo int     // how many candles ago the divergence pair was formed
    PriceA  float64 // older swing point price
    PriceB  float64 // newer swing point price (closer to current)
    OscA    float64 // oscillator value at PriceA swing
    OscB    float64 // oscillator value at PriceB swing
}

// swingPoint is a detected pivot high or pivot low.
type swingPoint struct {
    Index int
    Price float64
}

// DetectDivergence scans the last `lookback` candles for RSI and MACD histogram divergence.
// Returns DivNone if fewer than 2 swing points of the required type are found.
func DetectDivergence(
    closes []float64,   // close prices, index 0 = oldest
    highs []float64,    // high prices
    lows []float64,     // low prices
    rsi []float64,      // RSI series, same length as closes
    macdHist []float64, // MACD histogram series, same length as closes
    lookback int,
) (rsiDiv, macdDiv DivergenceResult) {
    if lookback <= 0 {
        lookback = DivLookbackBars
    }

    n := len(closes)
    if n < lookback*2 {
        return DivergenceResult{Type: DivNone}, DivergenceResult{Type: DivNone}
    }

    start := n - lookback

    // Swing lows → check for bullish divergence types
    swingLows := findSwingLows(lows, start, n)
    rsiDiv = checkDivergence(swingLows, closes, rsi, "low", n)
    if rsiDiv.Type == DivNone {
        // Swing highs → check for bearish divergence types
        swingHighs := findSwingHighs(highs, start, n)
        rsiDiv = checkDivergence(swingHighs, closes, rsi, "high", n)
    }

    // Same for MACD histogram
    swingLows2 := findSwingLows(lows, start, n)
    macdDiv = checkDivergence(swingLows2, closes, macdHist, "low", n)
    if macdDiv.Type == DivNone {
        swingHighs2 := findSwingHighs(highs, start, n)
        macdDiv = checkDivergence(swingHighs2, closes, macdHist, "high", n)
    }

    return rsiDiv, macdDiv
}

// findSwingLows returns pivot lows using a 3-candle pivot:
// lows[i] < lows[i-1] AND lows[i] < lows[i+1]
func findSwingLows(lows []float64, start, end int) []swingPoint {
    var points []swingPoint
    // need i-1 and i+1 so range is start+1 to end-2
    for i := start + 1; i < end-1; i++ {
        if lows[i] < lows[i-1] && lows[i] < lows[i+1] {
            points = append(points, swingPoint{Index: i, Price: lows[i]})
        }
    }
    return points
}

// findSwingHighs returns pivot highs using a 3-candle pivot:
// highs[i] > highs[i-1] AND highs[i] > highs[i+1]
func findSwingHighs(highs []float64, start, end int) []swingPoint {
    var points []swingPoint
    for i := start + 1; i < end-1; i++ {
        if highs[i] > highs[i-1] && highs[i] > highs[i+1] {
            points = append(points, swingPoint{Index: i, Price: highs[i]})
        }
    }
    return points
}

// checkDivergence compares the most recent 2 swing points of swingType
// against the oscillator values at those same indices.
func checkDivergence(
    swings []swingPoint,
    closes []float64,
    osc []float64,
    swingType string,
    totalBars int,
) DivergenceResult {
    // Need at least 2 swing points to form a pair
    if len(swings) < 2 {
        return DivergenceResult{Type: DivNone}
    }

    // Use the two most recent swing points
    // swings is ordered oldest→newest, so take last two
    a := swings[len(swings)-2]
    b := swings[len(swings)-1]

    // Enforce minimum gap between swings to reduce noise
    if b.Index-a.Index < DivMinSwingGap {
        return DivergenceResult{Type: DivNone}
    }

    oscA := osc[a.Index]
    oscB := osc[b.Index]

    // Skip if oscillator values are zero/invalid (techan may return 0 for early bars)
    if oscA == 0 || oscB == 0 {
        return DivergenceResult{Type: DivNone}
    }

    divType := classifyDivergence(a.Price, b.Price, oscA, oscB, swingType)
    if divType == DivNone {
        return DivergenceResult{Type: DivNone}
    }

    barsAgo := totalBars - 1 - b.Index

    return DivergenceResult{
        Type:    divType,
        BarsAgo: barsAgo,
        PriceA:  a.Price,
        PriceB:  b.Price,
        OscA:    oscA,
        OscB:    oscB,
    }
}

// classifyDivergence determines the divergence type from price and oscillator swing pairs.
// priceA/oscA = older swing, priceB/oscB = newer swing (closer to current bar).
func classifyDivergence(
    priceA, priceB float64,
    oscA, oscB float64,
    swingType string,
) DivergenceType {
    switch swingType {
    case "low":
        priceLowerLow  := priceB < priceA
        priceHigherLow := priceB > priceA
        oscHigherLow   := oscB > oscA
        oscLowerLow    := oscB < oscA

        if priceLowerLow && oscHigherLow {
            return DivRegularBullish // price LL, osc HL → downtrend exhausting
        }
        if priceHigherLow && oscLowerLow {
            return DivHiddenBullish  // price HL, osc LL → uptrend continuing
        }

    case "high":
        priceHigherHigh := priceB > priceA
        priceLowerHigh  := priceB < priceA
        oscLowerHigh    := oscB < oscA
        oscHigherHigh   := oscB > oscA

        if priceHigherHigh && oscLowerHigh {
            return DivRegularBearish // price HH, osc LH → uptrend exhausting
        }
        if priceLowerHigh && oscHigherHigh {
            return DivHiddenBearish  // price LH, osc HH → downtrend continuing
        }
    }

    return DivNone
}
```

---

## 3. Constants File Addition

Add to `ta/divergence.go` (or `ta/indicators.go`):

```go
const (
    DivLookbackBars   = 14 // candles to scan for swing points
    DivMinSwingGap    = 3  // minimum bars between two swing points (noise filter)
    DivStaleThreshold = 10 // BarsAgo > 10 = stale, AI instructed to discount
)
```

---

## 4. `ta/indicators.go` — TAResult Changes

### 4.1 Add New Fields to TAResult

Current `TAResult` has `RSIDivergence string` and `MACDDivergence string` as plain strings.
Replace these with the typed versions and add the new fields:

```go
type TAResult struct {
    // ── existing fields (unchanged) ───────────────────────────────────
    CurrentPrice       float64
    EMA9               float64
    EMA21              float64
    EMA50              float64
    EMA200             float64
    EMASpread          float64
    RibbonStatus       string
    DailyTrend         string
    RSI                float64
    RSIZone            string
    ATR                float64
    ATRLevel           string
    SL1ATR             float64
    SL15ATR            float64
    MACDValue          float64
    MACDSignal         float64
    MACDHistogram      float64
    MACDCross          string
    BBUpper            float64
    BBMiddle           float64
    BBLower            float64
    BBWidth            string
    BBPosition         string
    VolumeMultiplier   float64
    VolumeConfirms     bool
    OBVTrend           string
    NearestSupport     float64
    NearestResistance  float64
    SupportStrength    string
    ResistanceStrength string
    AtSupport          bool
    NearResistance     bool

    // ── New in v2: divergence ─────────────────────────────────────────
    // RSI divergence — replaces old RSIDivergence string
    RSIDivergence  DivergenceType // "none" | "regular_bullish" | "regular_bearish" | "hidden_bullish" | "hidden_bearish"
    RSIDivBarsAgo  int            // 0 = not detected

    // MACD histogram divergence
    MACDDivergence DivergenceType
    MACDDivBarsAgo int

    // DoubleDivergence is true when RSI and MACD show the same divergence type
    DoubleDivergence     bool
    DoubleDivergenceType DivergenceType
}
```

### 4.2 Wire `DetectDivergence` into `Calculate()`

At the end of `Calculate()`, after RSI and MACD values are computed, extract the series and run detection:

```go
// In Calculate(), after all indicators are computed:

// Extract series for divergence detection.
// techan stores values in a TimeSeries — extract close/high/low/RSI/macdHist for the last N bars.
rsiSeries    := extractFloatSeries(rsiIndicator, len(closeSeries))
macdHistSeries := extractFloatSeries(macdHistIndicator, len(closeSeries))
closes       := extractFloatSeries(closeIndicator, len(closeSeries))
highs        := extractFloatSeriesFromCandles(candles, "high")
lows         := extractFloatSeriesFromCandles(candles, "low")

rsiDiv, macdDiv := DetectDivergence(closes, highs, lows, rsiSeries, macdHistSeries, DivLookbackBars)

result.RSIDivergence  = rsiDiv.Type
result.RSIDivBarsAgo  = rsiDiv.BarsAgo
result.MACDDivergence = macdDiv.Type
result.MACDDivBarsAgo = macdDiv.BarsAgo

// Double divergence: both oscillators detected the same type
if rsiDiv.Type != DivNone && rsiDiv.Type == macdDiv.Type {
    result.DoubleDivergence     = true
    result.DoubleDivergenceType = rsiDiv.Type
}
```

> **Note on techan series extraction:** techan uses `techan.TimeSeries` internally. You already extract values for RSI, MACD, EMA in the existing `Calculate()` function. Use the same pattern to build `[]float64` slices for divergence detection. The exact method depends on how your current code iterates the series — match the existing pattern in `ta/indicators.go`.

---

## 5. Modified Files Summary

| File | Change |
|---|---|
| `ta/divergence.go` | **New** — `DivergenceType`, constants, `DivergenceResult`, `DetectDivergence`, helpers |
| `ta/indicators.go` | Update `TAResult`: replace `RSIDivergence string` + `MACDDivergence string` with typed fields; add `RSIDivBarsAgo`, `DoubleDivergence`, `DoubleDivergenceType`; call `DetectDivergence` at end of `Calculate()` |

---

## 6. Edge Cases

| Case | Handling |
|---|---|
| Fewer than `lookback×2` bars | Return `DivNone` for both |
| Only 1 swing point in window | Cannot form a pair → `DivNone` |
| Swing gap < `DivMinSwingGap` | Skip pair, too noisy |
| Oscillator value == 0 at swing | Skip — techan returns 0 for early uncomputed bars |
| `BarsAgo > DivStaleThreshold` | Still returned — AI prompt instructs to discount stale divergence |
| RSI and MACD detect different types | Both returned independently, `DoubleDivergence = false` |

---

## 7. Divergence in Downstream Consumers

After `TAResult` has divergence fields, these are consumed by:

**`ai/scorer.go` `fillTemplate()`** — 7 new placeholders:
```
{{RSI_DIVERGENCE}}    → boolToStr(r.RSIDivergence != DivNone)
{{RSI_DIV_TYPE}}      → string(r.RSIDivergence)
{{RSI_DIV_BARS}}      → strconv.Itoa(r.RSIDivBarsAgo)
{{MACD_DIVERGENCE}}   → boolToStr(r.MACDDivergence != DivNone)
{{MACD_DIV_TYPE}}     → string(r.MACDDivergence)
{{MACD_DIV_BARS}}     → strconv.Itoa(r.MACDDivBarsAgo)
{{DIVERGENCE_SIGNAL}} → divergenceSignalStr(r)
```

**`ai/position_manager.go` `buildPrompt()`** — 3 new lines in TA snapshot:
```
RSI Divergence   : %s (%d bars ago)
MACD Divergence  : %s (%d bars ago)
Double Divergence: %v
```

See `ai-changes.md` for full implementation details.

---

## 8. Implementation Order

```
Step 1: ta/divergence.go        ← new file, no dependencies
Step 2: ta/indicators.go        ← update TAResult + wire Calculate()
Step 3: ai/scorer.go            ← add placeholders + helpers (see ai-changes.md)
Step 4: ai/position_manager.go  ← add divergence to buildPrompt (see ai-changes.md)
```

> Divergence doesn't predict the future. It tells you momentum is shifting.
> Use it as confirmation at structure — never as a standalone signal.