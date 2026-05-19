# Mambo v2 — Coding Agent Implementation Plan

## Context
You are working on **Mambo AI Trade** — an autonomous crypto futures trading bot in Go targeting Hyperliquid.
Repository: https://github.com/StephanieAgatha/mambo
Read `CLAUDE.md` before writing any code. It contains coding standards that must be followed.

## Reference Documents
All spec documents are in the `docs/` folder (or wherever you placed them):
- `ta-divergence.md` — divergence detection spec
- `ai-changes.md` — AI package changes spec
- `mambo-v2-tv-command.md` — /tv command + bracket order fix spec
- `prompts/suggest_trade.md` — already updated ✅ (replace existing file)
- `prompts/verify_trade.md` — already updated ✅ (replace existing file)

---

## Implementation Order (STRICT — do not reorder)

```
Phase 1: ta/divergence.go          (no dependencies)
Phase 2: ta/indicators.go          (depends on Phase 1)
Phase 3: exchange/hyperliquid.go   (independent — bracket order fix)
Phase 4: monitor/position.go       (depends on Phase 3)
Phase 5: ai/scorer.go              (depends on Phase 2)
Phase 6: ai/position_manager.go    (depends on Phase 2)
Phase 7: mcp/client.go             (independent — new package)
Phase 8: discord/tv.go             (depends on Phase 5, 7)
Phase 9: discord/bot.go            (depends on Phase 8)
Phase 10: main.go                  (depends on Phase 7, 9)
Phase 11: config/config.go         (independent — add MCP_TA_COMMAND)
Phase 12: .env.example             (independent — document new vars)
```

---

## Phase 1 — `ta/divergence.go` (NEW FILE)

**Spec:** `ta-divergence.md` Section 2 + 3

Create `ta/divergence.go` with:

```
- DivergenceType string type with 5 constants:
    DivNone / DivRegularBullish / DivRegularBearish / DivHiddenBullish / DivHiddenBearish

- Constants:
    DivLookbackBars   = 14
    DivMinSwingGap    = 3
    DivStaleThreshold = 10

- DivergenceResult struct:
    Type    DivergenceType
    BarsAgo int
    PriceA  float64
    PriceB  float64
    OscA    float64
    OscB    float64

- swingPoint struct (private):
    Index int
    Price float64

- DetectDivergence(closes, highs, lows, rsi, macdHist []float64, lookback int) (rsiDiv, macdDiv DivergenceResult)
- findSwingLows(lows []float64, start, end int) []swingPoint
- findSwingHighs(highs []float64, start, end int) []swingPoint
- checkDivergence(swings []swingPoint, closes, osc []float64, swingType string, totalBars int) DivergenceResult
- classifyDivergence(priceA, priceB, oscA, oscB float64, swingType string) DivergenceType
```

Full implementation code is in `ta-divergence.md` Section 2.

**Verify:** `go build ./ta/...` passes with no errors.

---

## Phase 2 — `ta/indicators.go` (MODIFY)

**Spec:** `ta-divergence.md` Section 4

### 2a. Update TAResult struct

Find the `TAResult` struct. Replace the existing plain string fields:
```go
// REMOVE these (currently plain strings):
RSIDivergence  string
MACDDivergence string
```

Replace with typed fields + add new ones:
```go
// ADD:
RSIDivergence         DivergenceType
RSIDivBarsAgo         int
MACDDivergence        DivergenceType
MACDDivBarsAgo        int
DoubleDivergence      bool
DoubleDivergenceType  DivergenceType
```

### 2b. Wire DetectDivergence into Calculate()

At the END of the `Calculate()` function, after all indicators are computed, add:

```go
// Extract series as []float64 from techan — match the extraction pattern
// already used for RSI/MACD/EMA in this function.
// Build: closes []float64, highs []float64, lows []float64
// Build: rsiSeries []float64 from rsiIndicator
// Build: macdHistSeries []float64 from macdHistIndicator

rsiDiv, macdDiv := DetectDivergence(closes, highs, lows, rsiSeries, macdHistSeries, DivLookbackBars)

result.RSIDivergence  = rsiDiv.Type
result.RSIDivBarsAgo  = rsiDiv.BarsAgo
result.MACDDivergence = macdDiv.Type
result.MACDDivBarsAgo = macdDiv.BarsAgo

if rsiDiv.Type != DivNone && rsiDiv.Type == macdDiv.Type {
    result.DoubleDivergence     = true
    result.DoubleDivergenceType = rsiDiv.Type
}
```

**Note:** Study the existing series extraction code in `Calculate()` for RSI and MACD. Use the same pattern — do NOT invent a new approach.

**Verify:** `go build ./ta/...` passes. Run existing tests if any.

---

## Phase 3 — `exchange/hyperliquid.go` (MODIFY)

**Spec:** `mambo-v2-tv-command.md` Change 1

### 3a. Add PlaceBracketOrder function

Add this new function. Do NOT remove `PlaceLimitOrder` or `PlaceTriggerOrder` — mark them as deprecated with a comment but keep them (other callers may exist).

```go
// PlaceBracketOrder places entry + TP + SL atomically using grouping "normalTpsl".
// Replaces the old PlaceLimitOrder + waitForFill + PlaceTriggerOrder pattern.
// grouping "normalTpsl": Hyperliquid activates TP/SL once entry fills. OCO behavior.
func (c *Client) PlaceBracketOrder(
    ctx context.Context,
    pair string,
    side OrderSide,
    sizeUSD float64,
    entryPrice float64,
    takeProfitPrice float64,
    stopLossPrice float64,
    leverage int,
) (OrderResult, error)
```

Full implementation in `mambo-v2-tv-command.md` Change 1 section.

Key details:
- Call `c.ex.UpdateLeverage(ctx, leverage, pair, true)` first (cross margin = true)
- Use `c.szDecimalsForPair(ctx, pair)` for rounding — same as existing PlaceLimitOrder
- Orders slice: [entry GTC, stop-loss trigger reduce-only, take-profit trigger reduce-only]
- `grouping := hyperliquid.GroupingNormalTpsl`
- Call `c.ex.BulkOrder(ctx, orders, &grouping)`
- Entry order ID = `resp[0].Resting.Oid` (guard nil check)

**Verify:** `go build ./exchange/...` passes.

---

## Phase 4 — `monitor/position.go` (MODIFY)

**Spec:** `mambo-v2-tv-command.md` Change 1 — "Cascade Changes"

Find `waitForFill` function. Remove all `PlaceTriggerOrder` calls for TP and SL.
`waitForFill` now ONLY polls for position confirmation. No order placement.

```go
// waitForFill polls until entry order is confirmed as a position.
// TP/SL are already live via PlaceBracketOrder — no trigger placement needed here.
func waitForFill(ctx context.Context, exClient *exchange.Client, pair string, orderID uint64, timeout time.Duration) bool {
    deadline := time.Now().Add(timeout)
    for time.Now().Before(deadline) {
        positions, err := exClient.FetchPositions(ctx)
        if err != nil {
            slog.Warn("monitor: waitForFill fetch positions failed", "pair", pair, "err", err)
            time.Sleep(15 * time.Second)
            continue
        }
        for _, pos := range positions {
            if pos.Pair == pair {
                slog.Info("monitor: position confirmed filled", "pair", pair, "order_id", orderID)
                return true
            }
        }
        time.Sleep(15 * time.Second)
    }
    slog.Warn("monitor: waitForFill timeout", "pair", pair, "order_id", orderID)
    return false
}
```

Also find all callers of `PlaceLimitOrder` in monitor or discord packages.
Update them to use `PlaceBracketOrder` — pass `score.TakeProfit` and `score.StopLoss`.

**Verify:** `go build ./monitor/...` passes.

---

## Phase 5 — `ai/scorer.go` (MODIFY)

**Spec:** `ai-changes.md` Sections 1.1 through 1.6

### 5a. Update ScoreResult struct — add 5 fields
```go
Trend        string
EntryQuality string
SLReasoning  string
TPReasoning  string
Invalidation string
```

### 5b. Update parseDecision() — parse 5 new JSON fields
```go
Trend        string `json:"trend"`
EntryQuality string `json:"entry_quality"`
SLReasoning  string `json:"sl_reasoning"`
TPReasoning  string `json:"tp_reasoning"`
Invalidation string `json:"invalidation"`
```

### 5c. Update parseSuggestion() — parse 4 new JSON fields
```go
Trend        string `json:"trend"`
EntryQuality string `json:"entry_quality"`
SLPlacement  string `json:"sl_placement"`
TPPlacement  string `json:"tp_placement"`
// SLPlacement maps to SLReasoning, TPPlacement maps to TPReasoning in ScoreResult
```

### 5d. Update fillTemplate() — add 7 new placeholder replacements

Add to the `strings.NewReplacer(...)` call:
```go
"{{RSI_DIVERGENCE}}",    boolToStr(r.RSIDivergence != ta.DivNone),
"{{RSI_DIV_TYPE}}",      string(r.RSIDivergence),
"{{RSI_DIV_BARS}}",      strconv.Itoa(r.RSIDivBarsAgo),
"{{MACD_DIVERGENCE}}",   boolToStr(r.MACDDivergence != ta.DivNone),
"{{MACD_DIV_TYPE}}",     string(r.MACDDivergence),
"{{MACD_DIV_BARS}}",     strconv.Itoa(r.MACDDivBarsAgo),
"{{DIVERGENCE_SIGNAL}}", divergenceSignalStr(r),
"{{SR_ENTRY_SIGNAL}}",   srEntrySignal(r),
```

Also update existing entries:
```go
// The old plain string entries for RSI_DIVERGENCE and MACD_DIVERGENCE
// are now handled by the new typed entries above. Remove or replace the old ones.
```

### 5e. Add 3 helper functions

```go
func divergenceSignalStr(r ta.TAResult) string
func srEntrySignal(r ta.TAResult) string
func boolToStr(b bool) string  // check if already exists first
```

Full implementation in `ai-changes.md` Section 1.5.

### 5f. Add strconv to imports

### 5g. Update Score() slog.Info call — add trend, entry_quality, rsi_div, macd_div, double_div

Full updated log in `ai-changes.md` Section 1.6.

**Verify:** `go build ./ai/...` passes.

---

## Phase 6 — `ai/position_manager.go` (MODIFY)

**Spec:** `ai-changes.md` Section 2

### 6a. Update buildPrompt() format string

Add 3 lines to LIVE TA SNAPSHOT section:
```
- RSI Divergence   : %s (%d bars ago)
- MACD Divergence  : %s (%d bars ago)
- Double Divergence: %v
```

### 6b. Add 5 divergence rules to DECISION RULES section

```
- Hidden bullish divergence + long in drawdown → hold, momentum likely recovering
- Hidden bearish divergence + short in drawdown → hold, momentum likely recovering
- Regular bearish divergence + long near TP + PnL > 3% → close early, momentum fading
- Regular bullish divergence + short near TP + PnL > 3% → close early, momentum fading
- Double divergence (RSI + MACD agree) → weight this signal heavily in your decision
```

### 6c. Add 3 new args to the Sprintf call

```go
string(snap.RSIDivergence),  snap.RSIDivBarsAgo,
string(snap.MACDDivergence), snap.MACDDivBarsAgo,
snap.DoubleDivergence,
```

Full updated buildPrompt in `ai-changes.md` Section 2.2.

**Verify:** `go build ./ai/...` passes.

---

## Phase 7 — `mcp/client.go` (NEW PACKAGE)

**Spec:** `mambo-v2-tv-command.md` Change 2, Section 2.1

Create `mcp/` directory and `mcp/client.go`.

```go
package mcp

type StdioConfig struct {
    Command string
    Args    []string
    Env     []string
}

type Client struct {
    cfg         StdioConfig
    cmd         *exec.Cmd
    stdin       io.WriteCloser
    stdout      *bufio.Scanner
    mu          sync.Mutex
    reqID       int
    CallTimeout time.Duration
}

func NewClient(cfg StdioConfig) (*Client, error)
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error)
func (c *Client) Close() error
```

Implementation details:
- `NewClient`: spawn subprocess via `exec.Command`, pipe stdin/stdout, stderr → `io.Discard`, run MCP initialize handshake (JSON-RPC method `"initialize"` with `protocolVersion: "2024-11-05"`), send `"notifications/initialized"` notification
- `CallTool`: acquire mutex, increment reqID, send `{"jsonrpc":"2.0","id":N,"method":"tools/call","params":{"name":name,"arguments":args}}`, read one line response, parse `toolResult.content[0].text`
- `Close`: send SIGTERM to subprocess, wait
- All calls use `context.WithTimeout(ctx, c.CallTimeout)`
- `CallTimeout` default: 30s

JSON-RPC envelope types (private):
```go
type rpcRequest  struct { JSONRPC string; ID int; Method string; Params map[string]any }
type rpcResponse struct { JSONRPC string; ID int; Result json.RawMessage; Error *rpcError }
type rpcError    struct { Code int; Message string }
type toolResult  struct { Content []struct{ Type string; Text string }; IsError bool }
```

**Verify:** `go build ./mcp/...` passes.

---

## Phase 8 — `discord/tv.go` (NEW FILE)

**Spec:** `mambo-v2-tv-command.md` Change 2, Sections 2.3 + 2.4 + 3

Create `discord/tv.go` with:

```go
func (b *Bot) handleTV(s *discordgo.Session, i *discordgo.InteractionCreate)
func (b *Bot) handleTVWithLocalTA(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, coin string)
func (b *Bot) executeTVTrade(ctx context.Context, coin string, score aiPkg.ScoreResult, taResult ta.TAResult)
func (b *Bot) sendTVConfirmButtons(s *discordgo.Session, i *discordgo.InteractionCreate, coin string, score aiPkg.ScoreResult, taResult ta.TAResult)

// Data mapping helpers:
type TechnicalAnalysisResponse struct { ... } // see spec Section 3
func parseTechnicalAnalysis(raw json.RawMessage) (TechnicalAnalysisResponse, error)
func mapTVToTAResult(tv TechnicalAnalysisResponse, coin string) ta.TAResult
func classifyRSIZone(rsi float64) string   // reuse logic from existing code
func classifyMACDCross(histogram float64) string
func buildTVResultEmbed(...) *discordgo.MessageEmbed
func editErrorEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, coin, msg string)
```

Key flow for `handleTV`:
1. Deferred response
2. Try `b.mcpTA.CallTool("get_technical_analysis", {symbol: coin+"USDT", exchange: "BINANCE", screener: "crypto", interval: "4h"})`
3. On fail → `handleTVWithLocalTA`
4. `filter.ApplyPreFilter` → yellow embed if rejected
5. `b.scorer.Score` → red embed if error
6. Post result embed with `taSource` footer (`"📡 TradingView MCP · 4H"` or `"⚠️ Legacy TA Mode · 4H"`)
7. If EXECUTE + confidence ≥ `config.MinConfidence` → `sendTVConfirmButtons`

`executeTVTrade`:
1. `FetchPositions` → skip if already open for coin
2. `PlaceBracketOrder` with score.TakeProfit / score.StopLoss
3. `b.StartMonitor(...)`
4. `b.notifyTradeExecuted(...)`

`TechnicalAnalysisResponse` field keys (JSON tags from tradingview-ta library):
```go
CurrentPrice float64 `json:"close"`
EMA9  float64 `json:"EMA9"`
EMA21 float64 `json:"EMA21"`
EMA50 float64 `json:"EMA50"`
EMA200 float64 `json:"EMA200"`
RSI   float64 `json:"RSI"`
MACDLine   float64 `json:"MACD.macd"`
MACDSignal float64 `json:"MACD.signal"`
BBUpper  float64 `json:"BB.upper"`
BBMiddle float64 `json:"BB.middle"`
BBLower  float64 `json:"BB.lower"`
BBWidth  float64 `json:"BBW"`
ATR      float64 `json:"ATR"`
Volume   float64 `json:"volume"`
AvgVol   float64 `json:"volume_avg"`
TVSignal string  `json:"Recommend.All"` // "BUY"/"SELL"/"NEUTRAL"/"STRONG_BUY"/"STRONG_SELL"
```

**Verify:** `go build ./discord/...` passes.

---

## Phase 9 — `discord/bot.go` (MODIFY)

**Spec:** `mambo-v2-tv-command.md` Change 2, Section 2.2

### 9a. Add mcpTA field to Bot struct
```go
type Bot struct {
    // ... existing fields ...
    mcpTA *mcp.Client // nil = fallback mode (tradingview-mcp unavailable)
}
```

### 9b. Update New() to accept mcpTA parameter
```go
func New(cfg *config.Config, scorer *ai.Scorer, fetcher *market.Fetcher,
    jl *journal.Logger, exClient *exchange.Client, mon *monitor.Monitor,
    mcpTA *mcp.Client, // new parameter
) (*Bot, error)
```

### 9c. Register /tv slash command

In `registerCommands()`, add:
```go
{
    Name:        "tv",
    Description: "TradingView MCP analysis + AI scoring for a specific coin",
    Options: []*discordgo.ApplicationCommandOption{
        {
            Name:        "coin",
            Description: "Ticker symbol (e.g. SOL, BTC, ETH)",
            Type:        discordgo.ApplicationCommandOptionString,
            Required:    true,
        },
    },
},
```

### 9d. Add handler route in interaction switch
```go
case "tv":
    b.handleTV(s, i)
```

**Verify:** `go build ./discord/...` passes.

---

## Phase 10 — `main.go` (MODIFY)

**Spec:** `mambo-v2-tv-command.md` Section 4.2

Add MCP client initialization after existing client setup:

```go
// tradingview-mcp — NON-FATAL: if unavailable, /tv uses local TA fallback
mcpTACommand := cfg.MCPTACommand // default "uvx" from config
mcpTAArgs    := []string{"--from", "tradingview-mcp-server", "tradingview-mcp"}

var mcpTAClient *mcp.Client
mcpTAClient, err = mcp.NewClient(mcp.StdioConfig{
    Command: mcpTACommand,
    Args:    mcpTAArgs,
})
if err != nil {
    slog.Warn("main: tradingview-mcp unavailable, /tv will use local TA fallback",
        "err", err)
    mcpTAClient = nil // bot handles nil gracefully
} else {
    defer mcpTAClient.Close()
}

// Pass mcpTAClient to discord.New()
discordBot, err := discord.New(cfg, scorer, fetcher, jl, exClient, mon, mcpTAClient)
```

**Verify:** `go build .` passes.

---

## Phase 11 — `config/config.go` (MODIFY)

Add optional override for MCP command:

```go
// MCP (optional — defaults to uvx)
MCPTACommand string // env: MCP_TA_COMMAND, default "uvx"
```

In the config loader (wherever you parse env vars):
```go
cfg.MCPTACommand = getEnvOrDefault("MCP_TA_COMMAND", "uvx")
```

**Verify:** `go build ./config/...` passes.

---

## Phase 12 — `.env.example` + Prompt Files (MODIFY)

### 12a. .env.example — add MCP var
```env
# ── TradingView MCP (optional) ───────────────────────────
# Only needed if uvx is not in PATH on your VPS
MCP_TA_COMMAND=uvx
```

### 12b. Replace prompt files
```
cp prompts/suggest_trade.md → replace with updated version (from artifacts)
cp prompts/verify_trade.md  → replace with updated version (from artifacts)
```

---

## Final Verification

```bash
# Full build
go build ./...

# Vet
go vet ./...

# Check no leftover PlaceTriggerOrder calls in execution path
grep -rn "PlaceTriggerOrder" --include="*.go" .

# Check new placeholders are all handled
grep -o '{{[A-Z_]*}}' prompts/verify_trade.md | sort -u
# Cross-check every {{PLACEHOLDER}} appears in scorer.go fillTemplate()

# Test MCP subprocess (requires uvx on PATH)
uvx --from tradingview-mcp-server tradingview-mcp --help

# Run on testnet first
HL_TESTNET=true go run .
```

---

## Summary of All Files Changed

| File | Type | Phase |
|---|---|---|
| `ta/divergence.go` | NEW | 1 |
| `ta/indicators.go` | MODIFY | 2 |
| `exchange/hyperliquid.go` | MODIFY | 3 |
| `monitor/position.go` | MODIFY | 4 |
| `ai/scorer.go` | MODIFY | 5 |
| `ai/position_manager.go` | MODIFY | 6 |
| `mcp/client.go` | NEW | 7 |
| `discord/tv.go` | NEW | 8 |
| `discord/bot.go` | MODIFY | 9 |
| `main.go` | MODIFY | 10 |
| `config/config.go` | MODIFY | 11 |
| `.env.example` | MODIFY | 12 |
| `prompts/verify_trade.md` | REPLACE | 12 |
| `prompts/suggest_trade.md` | REPLACE | 12 |

**3 new files, 11 modified files.**