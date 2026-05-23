package market

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	hyperliquid "github.com/sonirico/go-hyperliquid"

	"mambo/config"
)

// AltfinsOHLCV holds a single candle from the Altfins snapshot API.
// Altfins sometimes returns numeric fields as strings — FlexFloat handles both.
type AltfinsOHLCV struct {
	Symbol string    `json:"symbol"`
	Time   string    `json:"time"`
	Open   FlexFloat `json:"open"`
	High   FlexFloat `json:"high"`
	Low    FlexFloat `json:"low"`
	Close  FlexFloat `json:"close"`
	Volume FlexFloat `json:"volume"`
}

// FlexFloat unmarshals both JSON numbers and numeric strings into float64.
type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("flexfloat: parse %q: %w", s, err)
		}
		*f = FlexFloat(v)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("flexfloat: %w", err)
	}
	*f = FlexFloat(n)
	return nil
}

// AltfinsResponse is the raw API response array.
type AltfinsResponse []AltfinsOHLCV

// OHLCV represents a single candlestick bar.
// Named OHLCV (not Candle) to avoid conflict with the SDK's hyperliquid.Candle type.
// Candles are always sorted ascending by OpenTime (oldest → newest).
// techan requires this order for correct EMA/RSI/MACD calculation.
type OHLCV struct {
	OpenTime time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// MarketContext holds enriched market data from free APIs.
// Sent to Grok alongside TA values to improve AI decision quality.
type MarketContext struct {
	FearGreedValue int
	FearGreedZone  string // "Extreme Fear" / "Fear" / "Neutral" / "Greed" / "Extreme Greed"
	FundingRate    float64
	FundingBias    string  // "positive (longs pay)" / "negative (shorts pay)" / "neutral"
	OIChange       string  // "rising" / "falling" / "flat"
	LongShortRatio float64 // >1 = more longs, <1 = more shorts
	LongShortBias  string  // "long-heavy" / "short-heavy" / "balanced"
}

// Fetcher handles all external data retrieval for Mambo.
type Fetcher struct {
	info       *hyperliquid.Info
	httpClient *http.Client
	cfg        *config.Config
}

// New creates a Fetcher using a read-only Info client (no signing needed).
func New(ctx context.Context, cfg *config.Config) *Fetcher {
	apiURL := hyperliquid.MainnetAPIURL
	if cfg.Testnet {
		apiURL = hyperliquid.TestnetAPIURL
	}

	// skipWS=true — we use REST polling only, no WebSocket subscriptions
	// nil meta/spotMeta/perpDexs → SDK fetches them automatically on first use
	info := hyperliquid.NewInfo(ctx, apiURL, true, nil, nil, nil)

	return &Fetcher{
		info: info,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cfg: cfg,
	}
}

// FetchOHLCV retrieves candlestick data for a pair from Hyperliquid.
// timeframe: "1m", "5m", "15m", "1h", "4h", "1d"
// Returns OHLCV bars sorted ascending (oldest → newest) — required by techan.
// Retries up to 5 times on transient failures.
func (f *Fetcher) FetchOHLCV(ctx context.Context, pair, timeframe string, limit int) ([]OHLCV, error) {
	const maxRetries = 5
	endTime := time.Now()
	startTime := endTime.Add(-estimateDuration(timeframe, limit))

	var rawCandles []hyperliquid.Candle
	var err error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt*2) * time.Second
			slog.Warn("fetcher: retrying OHLCV fetch", "pair", pair, "attempt", attempt+1, "delay", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		rawCandles, err = f.info.CandlesSnapshot(
			ctx,
			pair,
			timeframe,
			startTime.UnixMilli(),
			endTime.UnixMilli(),
		)
		if err == nil {
			break
		}
		slog.Warn("fetcher: HL candles attempt failed", "pair", pair, "tf", timeframe, "attempt", attempt+1, "err", err.Error())
	}

	if err != nil {
		return nil, fmt.Errorf("fetcher: fetch OHLCV failed pair=%s tf=%s after %d attempts: %w", pair, timeframe, maxRetries, err)
	}

	if len(rawCandles) == 0 {
		return nil, fmt.Errorf("fetcher: no candles returned pair=%s tf=%s", pair, timeframe)
	}

	bars := make([]OHLCV, 0, len(rawCandles))
	for _, c := range rawCandles {
		// Use the actual exported fields from hyperliquid.Candle
		open, err := strconv.ParseFloat(c.Open, 64)
		if err != nil {
			return nil, fmt.Errorf("fetcher: parse open %q pair=%s: %w", c.Open, pair, err)
		}
		high, err := strconv.ParseFloat(c.High, 64)
		if err != nil {
			return nil, fmt.Errorf("fetcher: parse high %q pair=%s: %w", c.High, pair, err)
		}
		low, err := strconv.ParseFloat(c.Low, 64)
		if err != nil {
			return nil, fmt.Errorf("fetcher: parse low %q pair=%s: %w", c.Low, pair, err)
		}
		closeVal, err := strconv.ParseFloat(c.Close, 64)
		if err != nil {
			return nil, fmt.Errorf("fetcher: parse close %q pair=%s: %w", c.Close, pair, err)
		}
		vol, err := strconv.ParseFloat(c.Volume, 64)
		if err != nil {
			return nil, fmt.Errorf("fetcher: parse volume %q pair=%s: %w", c.Volume, pair, err)
		}

		bars = append(bars, OHLCV{
			OpenTime: time.UnixMilli(c.TimeOpen), // c.TimeOpen = 't' (open time in ms)
			Open:     open,
			High:     high,
			Low:      low,
			Close:    closeVal,
			Volume:   vol,
		})
	}

	// Sort ascending (oldest → newest) — required by techan
	sort.Slice(bars, func(i, j int) bool {
		return bars[i].OpenTime.Before(bars[j].OpenTime)
	})

	// Trim to requested limit, keeping the most recent N candles
	if len(bars) > limit {
		bars = bars[len(bars)-limit:]
	}

	slog.Debug("OHLCV fetched",
		"pair", pair,
		"timeframe", timeframe,
		"count", len(bars),
		"oldest", bars[0].OpenTime.Format(time.RFC3339),
		"newest", bars[len(bars)-1].OpenTime.Format(time.RFC3339),
		"latest_close", bars[len(bars)-1].Close,
	)

	return bars, nil
}

// FetchFundingRate returns the current funding rate for a pair from Hyperliquid.
// Uses MetaAndAssetCtxs which maps Meta.Universe[i] to Ctxs[i] by index.
func (f *Fetcher) FetchFundingRate(ctx context.Context, pair string) (float64, string, error) {
	result, err := f.info.MetaAndAssetCtxs(ctx, hyperliquid.MetaAndAssetCtxsParams{})
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: fetch MetaAndAssetCtxs failed pair=%s: %w", pair, err)
	}

	// Meta.Universe[i] corresponds to Ctxs[i] by index
	for i, asset := range result.Meta.Universe {
		if asset.Name != pair {
			continue
		}
		if i >= len(result.Ctxs) {
			break
		}

		ctx := result.Ctxs[i]
		rate, err := strconv.ParseFloat(ctx.Funding, 64)
		if err != nil {
			return 0, "", fmt.Errorf("fetcher: parse funding rate %q pair=%s: %w", ctx.Funding, pair, err)
		}

		bias := fundingBias(rate)

		slog.Debug("funding rate fetched",
			"pair", pair,
			"rate", rate,
			"bias", bias,
		)

		return rate, bias, nil
	}

	// pair not found — return neutral defaults (not all pairs have funding data)
	slog.Debug("funding rate not found for pair, using neutral", "pair", pair)
	return 0, "neutral", nil
}

// FetchOpenInterest returns the open interest direction for a pair.
// Uses MetaAndAssetCtxs — same call as FetchFundingRate.
func (f *Fetcher) FetchOpenInterest(ctx context.Context, pair string) (string, error) {
	result, err := f.info.MetaAndAssetCtxs(ctx, hyperliquid.MetaAndAssetCtxsParams{})
	if err != nil {
		return "", fmt.Errorf("fetcher: fetch OI failed pair=%s: %w", pair, err)
	}

	for i, asset := range result.Meta.Universe {
		if asset.Name != pair {
			continue
		}
		if i >= len(result.Ctxs) {
			break
		}

		assetCtx := result.Ctxs[i]
		oi, err := strconv.ParseFloat(assetCtx.OpenInterest, 64)
		if err != nil {
			return "", fmt.Errorf("fetcher: parse OI %q pair=%s: %w", assetCtx.OpenInterest, pair, err)
		}

		direction := classifyOI(oi)

		slog.Debug("open interest fetched",
			"pair", pair,
			"oi", oi,
			"direction", direction,
		)

		return direction, nil
	}

	return "flat", nil
}

// FetchFearGreed retrieves the current Crypto Fear & Greed Index from alternative.me.
// Free, no API key needed.
func (f *Fetcher) FetchFearGreed(ctx context.Context) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.FearGreedURL, nil)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: build fear & greed request: %w", err)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: fear & greed request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: read fear & greed response: %w", err)
	}

	var result struct {
		Data []struct {
			Value               string `json:"value"`
			ValueClassification string `json:"value_classification"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return 0, "", fmt.Errorf("fetcher: parse fear & greed response: %w", err)
	}

	if len(result.Data) == 0 {
		return 0, "", fmt.Errorf("fetcher: fear & greed response is empty")
	}

	value, err := strconv.Atoi(result.Data[0].Value)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: parse fear & greed value %q: %w", result.Data[0].Value, err)
	}

	zone := result.Data[0].ValueClassification

	slog.Debug("fear & greed fetched", "value", value, "zone", zone)

	return value, zone, nil
}

// FetchLongShortRatio retrieves the global long/short account ratio from Binance Futures.
// Public API — no key needed. Returns neutral defaults if pair is not on Binance.
func (f *Fetcher) FetchLongShortRatio(ctx context.Context, pair string) (float64, string, error) {
	// Binance format: "SOLUSDT" (no slash, no hyphen)
	binancePair := pair + "USDT"
	url := fmt.Sprintf("%s/futures/data/globalLongShortAccountRatio?symbol=%s&period=5m&limit=1",
		config.BinanceFutURL, binancePair)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: build L/S ratio request pair=%s: %w", pair, err)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: L/S ratio request failed pair=%s: %w", pair, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: read L/S ratio response pair=%s: %w", pair, err)
	}

	var result []struct {
		LongShortRatio string `json:"longShortRatio"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		// pair might not exist on Binance — return neutral instead of error
		slog.Debug("L/S ratio unavailable for pair — using neutral", "pair", pair)
		return 1.0, "balanced", nil
	}

	if len(result) == 0 {
		return 1.0, "balanced", nil
	}

	ratio, err := strconv.ParseFloat(result[0].LongShortRatio, 64)
	if err != nil {
		return 0, "", fmt.Errorf("fetcher: parse L/S ratio %q pair=%s: %w", result[0].LongShortRatio, pair, err)
	}

	bias := longShortBias(ratio)

	slog.Debug("L/S ratio fetched",
		"pair", pair,
		"ratio", ratio,
		"bias", bias,
	)

	return ratio, bias, nil
}

// FetchMarketContext fetches all market enrichment data in a single call.
// Non-fatal: if any individual source fails, it logs a warning and continues with defaults.
func (f *Fetcher) FetchMarketContext(ctx context.Context, pair string) (MarketContext, error) {
	mc := MarketContext{
		FundingBias:    "neutral",
		OIChange:       "flat",
		LongShortBias:  "balanced",
		LongShortRatio: 1.0,
	}

	fgValue, fgZone, err := f.FetchFearGreed(ctx)
	if err != nil {
		slog.Warn("fetcher: fear & greed unavailable", "err", err)
	} else {
		mc.FearGreedValue = fgValue
		mc.FearGreedZone = fgZone
	}

	rate, bias, err := f.FetchFundingRate(ctx, pair)
	if err != nil {
		slog.Warn("fetcher: funding rate unavailable", "pair", pair, "err", err)
	} else {
		mc.FundingRate = rate
		mc.FundingBias = bias
	}

	oiDir, err := f.FetchOpenInterest(ctx, pair)
	if err != nil {
		slog.Warn("fetcher: open interest unavailable", "pair", pair, "err", err)
	} else {
		mc.OIChange = oiDir
	}

	lsRatio, lsBias, err := f.FetchLongShortRatio(ctx, pair)
	if err != nil {
		slog.Warn("fetcher: L/S ratio unavailable", "pair", pair, "err", err)
	} else {
		mc.LongShortRatio = lsRatio
		mc.LongShortBias = lsBias
	}

	return mc, nil
}

// FetchAltfinsSnapshot fetches the latest OHLCV candle from Altfins for comparison.
// timeInterval: "DAILY" | "HOURLY" | "WEEKLY" | "MONTHLY"
// Requires ALTFINS_API_KEY in config.
func (f *Fetcher) FetchAltfinsSnapshot(ctx context.Context, coin, timeInterval string) (AltfinsOHLCV, error) {
	if f.cfg.AltfinsAPIKey == "" {
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: ALTFINS_API_KEY not configured")
	}

	body := map[string]any{
		"symbols":      []string{strings.ToUpper(coin)},
		"timeInterval": timeInterval,
	}

	b, err := json.Marshal(body)
	if err != nil {
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: marshal altfins request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.AltfinsAPIURL, bytes.NewReader(b))
	if err != nil {
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: build altfins request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-KEY", f.cfg.AltfinsAPIKey)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: altfins request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: read altfins response: %w", err)
	}

	if resp.StatusCode != 200 {
		limit := min(500, len(raw))
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: altfins returned %d: %s", resp.StatusCode, string(raw[:limit]))
	}

	var result AltfinsResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: parse altfins response: %w", err)
	}

	if len(result) == 0 {
		return AltfinsOHLCV{}, fmt.Errorf("fetcher: altfins returned empty array for %s/%s", coin, timeInterval)
	}

	entry := result[0]
	slog.Debug("altfins snapshot fetched",
		"coin", coin,
		"interval", timeInterval,
		"open", entry.Open,
		"high", entry.High,
		"low", entry.Low,
		"close", entry.Close,
		"volume", entry.Volume,
	)

	return entry, nil
}

// FetchAltfinsBatchSnapshot fetches latest OHLCV candles for multiple symbols in one API call.
// timeInterval: "DAILY" | "HOURLY" | "WEEKLY" | "MONTHLY"
// Returns a map of symbol → candle. Symbols not found (empty array) are omitted.
func (f *Fetcher) FetchAltfinsBatchSnapshot(ctx context.Context, symbols []string, timeInterval string) (map[string]AltfinsOHLCV, error) {
	if f.cfg.AltfinsAPIKey == "" {
		return nil, fmt.Errorf("fetcher: ALTFINS_API_KEY not configured")
	}

	body := map[string]any{
		"symbols":      symbols,
		"timeInterval": timeInterval,
	}

	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("fetcher: marshal altfins batch request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.AltfinsAPIURL, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("fetcher: build altfins batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-KEY", f.cfg.AltfinsAPIKey)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetcher: altfins batch request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetcher: read altfins batch response: %w", err)
	}

	if resp.StatusCode != 200 {
		limit := min(500, len(raw))
		return nil, fmt.Errorf("fetcher: altfins batch returned %d: %s", resp.StatusCode, string(raw[:limit]))
	}

	var result AltfinsResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("fetcher: parse altfins batch response: %w", err)
	}

	out := make(map[string]AltfinsOHLCV, len(result))
	for _, entry := range result {
		out[entry.Symbol] = entry
	}

	slog.Debug("altfins batch snapshot fetched",
		"symbols", len(symbols),
		"results", len(out),
		"interval", timeInterval,
	)

	return out, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// estimateDuration returns an approximate look-back window for N candles.
// Adds a 20% buffer to ensure we always receive at least `limit` candles.
func estimateDuration(timeframe string, limit int) time.Duration {
	var single time.Duration
	switch timeframe {
	case "1m":
		single = time.Minute
	case "5m":
		single = 5 * time.Minute
	case "15m":
		single = 15 * time.Minute
	case "1h":
		single = time.Hour
	case "4h":
		single = 4 * time.Hour
	case "1d":
		single = 24 * time.Hour
	default:
		// fallback to 4h — primary timeframe for Mambo
		single = 4 * time.Hour
	}
	return time.Duration(float64(single) * float64(limit) * 1.2)
}

// fundingBias classifies a funding rate into a human-readable description.
func fundingBias(rate float64) string {
	switch {
	case rate > 0.0005:
		return "positive (longs pay — market overleveraged long)"
	case rate < -0.0005:
		return "negative (shorts pay — market overleveraged short)"
	default:
		return "neutral"
	}
}

// classifyOI returns a simple open interest level label.
func classifyOI(oi float64) string {
	switch {
	case oi > 1_000_000:
		return "high"
	case oi > 100_000:
		return "moderate"
	default:
		return "low"
	}
}

// longShortBias classifies the long/short ratio into a human-readable label.
func longShortBias(ratio float64) string {
	switch {
	case ratio > 1.5:
		return "long-heavy (contrarian bearish signal)"
	case ratio < 0.67:
		return "short-heavy (contrarian bullish signal)"
	default:
		return "balanced"
	}
}
