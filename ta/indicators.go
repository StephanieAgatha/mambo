package ta

import (
    "fmt"
    "log/slog"
    "math"
    "time"

    "github.com/sdcoffey/big"
    "github.com/sdcoffey/techan"

    "mambo/market"
)

// TAResult holds all computed indicator values for a single pair.
// All values are passed to Grok via verify_trade.md placeholders.
type TAResult struct {
    // Current price (last candle close)
    CurrentPrice float64

    // EMA Ribbon
    EMA9   float64
    EMA21  float64
    EMA50  float64
    EMA200 float64

    // EMA spread = |EMA9 - EMA21| / Price * 100
    // < 0.2% → sideways market → skip pair
    EMASpread float64

    // Ribbon and trend status
    RibbonStatus string // "fully aligned bullish" / "fully aligned bearish" / "mixed"
    DailyTrend   string // "Golden Cross (bullish macro)" / "Death Cross (bearish macro)" / "Neutral"

    // RSI
    RSI           float64
    RSIZone       string // "overbought >70" / "oversold <30" / "valid 40-60" / "valid 30-70"
    RSIDivergence string // "bullish" / "hidden_bullish" / "bearish" / "none"

    // ATR — used for dynamic SL calculation
    ATR      float64
    ATRLevel string  // "low" / "medium" / "high"
    SL1ATR   float64 // entry - (1.0 × ATR)
    SL15ATR  float64 // entry - (1.5 × ATR)

    // MACD (12, 26, 9)
    MACDValue      float64
    MACDSignal     float64
    MACDHistogram  float64
    MACDCross      string // "bullish cross" / "bearish cross" / "none"
    MACDDivergence string // "bullish" / "bearish" / "none"

    // Bollinger Bands (20, 2)
    BBUpper    float64
    BBMiddle   float64
    BBLower    float64
    BBWidth    string // "squeeze" / "normal" / "expanded"
    BBPosition string // "at lower" / "at upper" / "at middle" / "above upper" / "below lower"

    // Volume + OBV
    VolumeMultiplier float64 // current vol / 20-period avg
    VolumeConfirms   bool
    OBVTrend         string // "rising" / "falling" / "flat"

    // Support & Resistance (swing point detection)
    NearestSupport     float64 // closest swing low below current price
    NearestResistance  float64 // closest swing high above current price
    SupportStrength    string  // "strong" / "moderate" / "weak"
    ResistanceStrength string  // "strong" / "moderate" / "weak"
    AtSupport          bool    // price within 1% of nearest support → +1 confluence
    NearResistance     bool    // price within 1% of nearest resistance → size down
}

// Calculate computes all TA indicators from OHLCV bars using techan + custom S/R.
// bars must be sorted ascending (oldest → newest) — market.FetchOHLCV guarantees this.
func Calculate(bars []market.OHLCV) (TAResult, error) {
    if len(bars) < 200 {
        return TAResult{}, fmt.Errorf("ta: need at least 200 candles, got %d", len(bars))
    }

    series := buildTimeSeries(bars)
    lastIdx := series.LastIndex()

    if lastIdx < 0 {
        return TAResult{}, fmt.Errorf("ta: time series is empty after building")
    }

    closePrice := techan.NewClosePriceIndicator(series)
    result := TAResult{}

    // ── Current price ─────────────────────────────────────────────────────────
    result.CurrentPrice = closePrice.Calculate(lastIdx).Float()

    // ── EMA Ribbon ────────────────────────────────────────────────────────────
    ema9 := techan.NewEMAIndicator(closePrice, 9)
    ema21 := techan.NewEMAIndicator(closePrice, 21)
    ema50 := techan.NewEMAIndicator(closePrice, 50)
    ema200 := techan.NewEMAIndicator(closePrice, 200)

    result.EMA9 = ema9.Calculate(lastIdx).Float()
    result.EMA21 = ema21.Calculate(lastIdx).Float()
    result.EMA50 = ema50.Calculate(lastIdx).Float()
    result.EMA200 = ema200.Calculate(lastIdx).Float()

    if result.CurrentPrice > 0 {
        result.EMASpread = math.Abs(result.EMA9-result.EMA21) / result.CurrentPrice * 100
    }

    result.RibbonStatus = ribbonStatus(result)
    result.DailyTrend = dailyTrend(result.EMA50, result.EMA200)

    // ── RSI (14) ──────────────────────────────────────────────────────────────
    rsi := techan.NewRelativeStrengthIndexIndicator(closePrice, 14)
    result.RSI = rsi.Calculate(lastIdx).Float()
    result.RSIZone = rsiZone(result.RSI)
    result.RSIDivergence = detectRSIDivergence(closePrice, rsi, lastIdx, 5)

    // ── ATR (14) ──────────────────────────────────────────────────────────────
    atr := techan.NewAverageTrueRangeIndicator(series, 14)
    result.ATR = atr.Calculate(lastIdx).Float()
    result.ATRLevel = atrLevel(result.ATR, result.CurrentPrice)
    result.SL1ATR = result.CurrentPrice - (1.0 * result.ATR)
    result.SL15ATR = result.CurrentPrice - (1.5 * result.ATR)

    // ── MACD (12, 26, 9) ──────────────────────────────────────────────────────
    // techan's MACD: NewMACDIndicator(indicator, shortWindow, longWindow)
    // Signal line: EMA(9) of the MACD line
    // Histogram: NewMACDHistogramIndicator(indicator, shortWindow, longWindow, signalWindow)
    macdLine := techan.NewMACDIndicator(closePrice, 12, 26)
    macdSignal := techan.NewEMAIndicator(macdLine, 9)
   macdHist := techan.NewMACDHistogramIndicator(macdLine, 9)

    result.MACDValue = macdLine.Calculate(lastIdx).Float()
    result.MACDSignal = macdSignal.Calculate(lastIdx).Float()
    result.MACDHistogram = macdHist.Calculate(lastIdx).Float()
    result.MACDCross = macdCross(macdLine, macdSignal, lastIdx)
    result.MACDDivergence = detectMACDDivergence(closePrice, macdHist, lastIdx, 5)

    // ── Bollinger Bands (20, 2) ───────────────────────────────────────────────
    // techan API: NewBollinger{Upper,Lower}BandIndicator(indicator, window, sigma)
    bbUpper := techan.NewBollingerUpperBandIndicator(closePrice, 20, 2.0)
    bbLower := techan.NewBollingerLowerBandIndicator(closePrice, 20, 2.0)
    bbMiddle := techan.NewSimpleMovingAverage(closePrice, 20) // BB middle = SMA(20)

    result.BBUpper = bbUpper.Calculate(lastIdx).Float()
    result.BBMiddle = bbMiddle.Calculate(lastIdx).Float()
    result.BBLower = bbLower.Calculate(lastIdx).Float()
    result.BBWidth = bbWidth(result.BBUpper, result.BBLower, result.BBMiddle)
    result.BBPosition = bbPosition(result.CurrentPrice, result.BBUpper, result.BBLower, result.BBMiddle)

    // ── Volume ────────────────────────────────────────────────────────────────
    volumeInd := techan.NewVolumeIndicator(series)
    volumeSMA := techan.NewSimpleMovingAverage(volumeInd, 20)

    currentVol := volumeInd.Calculate(lastIdx).Float()
    avgVol := volumeSMA.Calculate(lastIdx).Float()

    if avgVol > 0 {
        result.VolumeMultiplier = currentVol / avgVol
    }
    result.VolumeConfirms = result.VolumeMultiplier >= 1.0

    // ── OBV (custom implementation — not in techan) ───────────────────────────
    result.OBVTrend = computeOBVTrend(bars, 5)

    // ── Support & Resistance (custom swing point detection) ───────────────────
    srResult := detectSupportResistance(bars, result.CurrentPrice)
    result.NearestSupport = srResult.support
    result.NearestResistance = srResult.resistance
    result.SupportStrength = srResult.supportStrength
    result.ResistanceStrength = srResult.resistanceStrength
    result.AtSupport = srResult.atSupport
    result.NearResistance = srResult.nearResistance

    slog.Debug("TA calculated",
        "price", result.CurrentPrice,
        "ema9", result.EMA9,
        "ema21", result.EMA21,
        "ema_spread_pct", fmt.Sprintf("%.3f", result.EMASpread),
        "rsi", fmt.Sprintf("%.2f", result.RSI),
        "atr", result.ATR,
        "support", result.NearestSupport,
        "resistance", result.NearestResistance,
        "at_support", result.AtSupport,
        "near_resistance", result.NearResistance,
    )

    return result, nil
}

// ── OBV (On-Balance Volume) — custom implementation ──────────────────────────

// computeOBVTrend calculates OBV from raw bars and returns the trend over
// the last `lookback` periods.
func computeOBVTrend(bars []market.OHLCV, lookback int) string {
    if len(bars) < lookback+1 {
        return "flat"
    }

    // compute OBV series
    obvValues := make([]float64, len(bars))
    obvValues[0] = bars[0].Volume

    for i := 1; i < len(bars); i++ {
        switch {
        case bars[i].Close > bars[i-1].Close:
            obvValues[i] = obvValues[i-1] + bars[i].Volume
        case bars[i].Close < bars[i-1].Close:
            obvValues[i] = obvValues[i-1] - bars[i].Volume
        default:
            obvValues[i] = obvValues[i-1]
        }
    }

    lastIdx := len(obvValues) - 1
    prevOBV := obvValues[lastIdx-lookback]
    currOBV := obvValues[lastIdx]
    diff := currOBV - prevOBV
    threshold := math.Abs(prevOBV) * 0.01

    switch {
    case diff > threshold:
        return "rising"
    case diff < -threshold:
        return "falling"
    default:
        return "flat"
    }
}

// ── Support & Resistance Detection ───────────────────────────────────────────

type srResult struct {
    support            float64
    resistance         float64
    supportStrength    string
    resistanceStrength string
    atSupport          bool
    nearResistance     bool
}

// detectSupportResistance finds swing lows (support) and swing highs (resistance)
// from the candle data using a 3-candle pivot point method.
func detectSupportResistance(bars []market.OHLCV, currentPrice float64) srResult {
    const (
        proximityPct = 0.01
        mergePct     = 0.005
        lookback     = 100
    )

    if len(bars) < 5 {
        return srResult{
            supportStrength:    "unknown",
            resistanceStrength: "unknown",
        }
    }

    start := len(bars) - lookback
    if start < 0 {
        start = 0
    }
    recent := bars[start:]

    swingLows := []float64{}
    for i := 1; i < len(recent)-1; i++ {
        if recent[i].Low < recent[i-1].Low && recent[i].Low < recent[i+1].Low {
            swingLows = append(swingLows, recent[i].Low)
        }
    }

    swingHighs := []float64{}
    for i := 1; i < len(recent)-1; i++ {
        if recent[i].High > recent[i-1].High && recent[i].High > recent[i+1].High {
            swingHighs = append(swingHighs, recent[i].High)
        }
    }

    mergedSupports := mergeLevels(swingLows, mergePct)
    mergedResistances := mergeLevels(swingHighs, mergePct)

    nearestSupport := 0.0
    for _, lvl := range mergedSupports {
        if lvl < currentPrice {
            if nearestSupport == 0 || lvl > nearestSupport {
                nearestSupport = lvl
            }
        }
    }

    nearestResistance := 0.0
    for _, lvl := range mergedResistances {
        if lvl > currentPrice {
            if nearestResistance == 0 || lvl < nearestResistance {
                nearestResistance = lvl
            }
        }
    }

    supportTests := countTests(bars, nearestSupport, mergePct)
    resistanceTests := countTests(bars, nearestResistance, mergePct)

    atSupport := false
    if nearestSupport > 0 {
        dist := math.Abs(currentPrice-nearestSupport) / currentPrice
        atSupport = dist <= proximityPct
    }

    nearResistance := false
    if nearestResistance > 0 {
        dist := math.Abs(nearestResistance-currentPrice) / currentPrice
        nearResistance = dist <= proximityPct
    }

    return srResult{
        support:            nearestSupport,
        resistance:         nearestResistance,
        supportStrength:    levelStrength(supportTests),
        resistanceStrength: levelStrength(resistanceTests),
        atSupport:          atSupport,
        nearResistance:     nearResistance,
    }
}

func mergeLevels(levels []float64, thresholdPct float64) []float64 {
    if len(levels) == 0 {
        return nil
    }

    merged := []float64{}
    used := make([]bool, len(levels))

    for i := 0; i < len(levels); i++ {
        if used[i] {
            continue
        }

        cluster := []float64{levels[i]}
        for j := i + 1; j < len(levels); j++ {
            if used[j] {
                continue
            }
            diff := math.Abs(levels[j]-levels[i]) / levels[i]
            if diff <= thresholdPct {
                cluster = append(cluster, levels[j])
                used[j] = true
            }
        }

        sum := 0.0
        for _, v := range cluster {
            sum += v
        }
        merged = append(merged, sum/float64(len(cluster)))
    }

    return merged
}

func countTests(bars []market.OHLCV, level float64, thresholdPct float64) int {
    if level == 0 {
        return 0
    }

    count := 0
    for _, bar := range bars {
        lowDist := math.Abs(bar.Low-level) / level
        if lowDist <= thresholdPct {
            count++
            continue
        }
        highDist := math.Abs(bar.High-level) / level
        if highDist <= thresholdPct {
            count++
        }
    }

    return count
}

func levelStrength(tests int) string {
    switch {
    case tests >= 3:
        return "strong"
    case tests == 2:
        return "moderate"
    case tests == 1:
        return "weak"
    default:
        return "unconfirmed"
    }
}

// ── Time Series Builder ───────────────────────────────────────────────────────

func buildTimeSeries(bars []market.OHLCV) *techan.TimeSeries {
    series := techan.NewTimeSeries()

    for _, bar := range bars {
        candle := techan.NewCandle(techan.TimePeriod{
            Start: bar.OpenTime,
            End:   bar.OpenTime.Add(time.Minute),
        })

        candle.OpenPrice = big.NewDecimal(bar.Open)
        candle.MaxPrice = big.NewDecimal(bar.High)
        candle.MinPrice = big.NewDecimal(bar.Low)
        candle.ClosePrice = big.NewDecimal(bar.Close)
        candle.Volume = big.NewDecimal(bar.Volume)

        series.AddCandle(candle)
    }

    return series
}

// ── Indicator Helpers ─────────────────────────────────────────────────────────

func ribbonStatus(r TAResult) string {
    price := r.CurrentPrice
    switch {
    case price > r.EMA9 && r.EMA9 > r.EMA21 && r.EMA21 > r.EMA50 && r.EMA50 > r.EMA200:
        return "fully aligned bullish"
    case price < r.EMA9 && r.EMA9 < r.EMA21 && r.EMA21 < r.EMA50 && r.EMA50 < r.EMA200:
        return "fully aligned bearish"
    case price > r.EMA21 && price > r.EMA50:
        return "partial bullish"
    case price < r.EMA21 && price < r.EMA50:
        return "partial bearish"
    default:
        return "mixed"
    }
}

func dailyTrend(ema50, ema200 float64) string {
    switch {
    case ema50 > ema200:
        return "Golden Cross (bullish macro)"
    case ema50 < ema200:
        return "Death Cross (bearish macro)"
    default:
        return "Neutral"
    }
}

func rsiZone(rsi float64) string {
    switch {
    case rsi > 70:
        return "overbought >70 — avoid long entry"
    case rsi < 30:
        return "oversold <30 — wait for reversal confirmation"
    case rsi >= 40 && rsi <= 60:
        return "valid 40-60 (strong trend range)"
    default:
        return "valid 30-70 (ranging market)"
    }
}

func atrLevel(atr, price float64) string {
    if price == 0 {
        return "unknown"
    }
    pct := atr / price * 100
    switch {
    case pct < 1.5:
        return "low"
    case pct < 3.0:
        return "medium"
    default:
        return "high"
    }
}

func bbWidth(upper, lower, middle float64) string {
    if middle == 0 {
        return "unknown"
    }
    bandwidth := (upper - lower) / middle * 100
    switch {
    case bandwidth < 3.0:
        return "squeeze"
    case bandwidth < 8.0:
        return "normal"
    default:
        return "expanded"
    }
}

func bbPosition(price, upper, lower, middle float64) string {
    switch {
    case price > upper:
        return "above upper band"
    case price < lower:
        return "below lower band"
    case price >= upper*0.98:
        return "at upper band"
    case price <= lower*1.02:
        return "at lower band"
    default:
        return "at middle band"
    }
}

func macdCross(macdLine, macdSignal techan.Indicator, lastIdx int) string {
    if lastIdx < 1 {
        return "none"
    }
    prevMACD := macdLine.Calculate(lastIdx - 1).Float()
    prevSignal := macdSignal.Calculate(lastIdx - 1).Float()
    currMACD := macdLine.Calculate(lastIdx).Float()
    currSignal := macdSignal.Calculate(lastIdx).Float()

    switch {
    case prevMACD <= prevSignal && currMACD > currSignal:
        return "bullish cross"
    case prevMACD >= prevSignal && currMACD < currSignal:
        return "bearish cross"
    default:
        return "none"
    }
}

func detectRSIDivergence(price, rsi techan.Indicator, lastIdx, lookback int) string {
    if lastIdx < lookback {
        return "none"
    }
    prevIdx := lastIdx - lookback
    prevPrice := price.Calculate(prevIdx).Float()
    currPrice := price.Calculate(lastIdx).Float()
    prevRSI := rsi.Calculate(prevIdx).Float()
    currRSI := rsi.Calculate(lastIdx).Float()

    switch {
    // Bullish divergence: price makes lower low, RSI makes higher low
    case currPrice < prevPrice && currRSI > prevRSI:
        return "bullish"
    // Bearish divergence: price makes higher high, RSI makes lower high (RSI > 50 zone)
    case currPrice > prevPrice && currRSI < prevRSI && currRSI >= 50:
        return "bearish"
    // Hidden bullish divergence: price makes higher low, RSI makes lower low (RSI < 50 zone)
    case currPrice > prevPrice && currRSI < prevRSI && currRSI < 50:
        return "hidden_bullish"
    default:
        return "none"
    }
}


func detectMACDDivergence(price, macdHist techan.Indicator, lastIdx, lookback int) string {
    if lastIdx < lookback {
        return "none"
    }
    prevIdx := lastIdx - lookback
    prevPrice := price.Calculate(prevIdx).Float()
    currPrice := price.Calculate(lastIdx).Float()
    prevHist := macdHist.Calculate(prevIdx).Float()
    currHist := macdHist.Calculate(lastIdx).Float()

    switch {
    case currPrice < prevPrice && currHist > prevHist:
        return "bullish"
    case currPrice > prevPrice && currHist < prevHist:
        return "bearish"
    default:
        return "none"
    }
}
