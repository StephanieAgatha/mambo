package ta

import (
	"log/slog"
)

// DivergenceType classifies the type of divergence detected between price and an oscillator.
type DivergenceType string

const (
	DivNone           DivergenceType = "none"
	DivRegularBullish DivergenceType = "regular_bullish"
	DivRegularBearish DivergenceType = "regular_bearish"
	DivHiddenBullish  DivergenceType = "hidden_bullish"
	DivHiddenBearish  DivergenceType = "hidden_bearish"
)

const (
	DivLookbackBars   = 14 // candles to scan for swing points
	DivMinSwingGap    = 3  // minimum bars between two swing points (noise filter)
	DivStaleThreshold = 10 // BarsAgo > 10 = stale, AI instructed to discount
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
	closes []float64, // close prices, index 0 = oldest
	highs []float64,  // high prices
	lows []float64,   // low prices
	rsi []float64,    // RSI series, same length as closes
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

	slog.Debug("divergence: detection complete",
		"rsi_div", string(rsiDiv.Type),
		"rsi_bars_ago", rsiDiv.BarsAgo,
		"macd_div", string(macdDiv.Type),
		"macd_bars_ago", macdDiv.BarsAgo,
	)

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
		priceLowerLow := priceB < priceA
		priceHigherLow := priceB > priceA
		oscHigherLow := oscB > oscA
		oscLowerLow := oscB < oscA

		if priceLowerLow && oscHigherLow {
			return DivRegularBullish // price LL, osc HL → downtrend exhausting
		}
		if priceHigherLow && oscLowerLow {
			return DivHiddenBullish // price HL, osc LL → uptrend continuing
		}

	case "high":
		priceHigherHigh := priceB > priceA
		priceLowerHigh := priceB < priceA
		oscLowerHigh := oscB < oscA
		oscHigherHigh := oscB > oscA

		if priceHigherHigh && oscLowerHigh {
			return DivRegularBearish // price HH, osc LH → uptrend exhausting
		}
		if priceLowerHigh && oscHigherHigh {
			return DivHiddenBearish // price LH, osc HH → downtrend continuing
		}
	}

	return DivNone
}
