# Mambo v2 — `/tv` Command + Bracket Order Fix

## Overview

Two independent changes bundled into v2:

1. **`/tv` slash command** — replaces local techan TA pipeline with `tradingview-mcp` for real-time TradingView indicators. Execution still uses the existing `go-hyperliquid` SDK (no hyperliquid-mcp).
2. **Bracket order fix** — `PlaceTriggerOrder` (called after `waitForFill`) is replaced with atomic `PlaceBracketOrder` using `grouping: "normalTpsl"`. This fixes the bug where TP/SL were not appearing on Hyperliquid UI when the entry order hadn't filled yet.

**MCP Server (TA only):**
- [`atilaahmettaner/tradingview-mcp`](https://github.com/atilaahmettaner/tradingview-mcp) — 30+ indicators. Tool: `get_technical_analysis`. Package: `tradingview-mcp-server` (PyPI). No API key required.

> **hyperliquid-mcp status:** Not used in v2. Execution remains native Go via `go-hyperliquid` SDK — more reliable, no subprocess overhead, no asset index lookup needed. May be revisited in a future phase.

**Prerequisites on VPS:**
```bash
# Install uv
curl -LsSf https://astral.sh/uv/install.sh | sh && source ~/.bashrc

# Verify tradingview-mcp is callable
uvx --from tradingview-mcp-server tradingview-mcp --help
```

---

## Change 1: Bracket Order Fix (`exchange/hyperliquid.go`)

### Root Cause

In v1, `PlaceLimitOrder` places the entry order, then `waitForFill` polls until filled, then `PlaceTriggerOrder` is called separately for TP and SL. The problem: Hyperliquid requires a position to exist before accepting `reduce-only` trigger orders. If `waitForFill` fires the triggers while the entry order is still resting (not yet matched), Hyperliquid silently rejects or ignores them — TP/SL never appear on the UI.

### Fix: `grouping: "normalTpsl"`

Per [Privy/Hyperliquid trading patterns docs](https://docs.privy.io/recipes/hyperliquid/trading-patterns.md), the correct approach is to send all 3 orders (entry + TP + SL) **atomically in one call** using `grouping: "normalTpsl"`. Hyperliquid holds the TP/SL orders and activates them automatically once the entry fills. If either TP or SL triggers, the other is auto-cancelled (OCO behavior).

### New Function: `PlaceBracketOrder`

Add to `exchange/hyperliquid.go`, replacing the `PlaceLimitOrder` + `PlaceTriggerOrder` pattern:

```go
// PlaceBracketOrder places entry + TP + SL atomically using grouping "normalTpsl".
// All 3 orders are submitted in one call. Hyperliquid activates TP/SL once entry fills.
// If TP or SL triggers, the other is automatically cancelled (OCO).
// This replaces the old PlaceLimitOrder + waitForFill + PlaceTriggerOrder pattern.
func (c *Client) PlaceBracketOrder(
    ctx context.Context,
    pair string,
    side OrderSide,
    sizeUSD float64,
    entryPrice float64,
    takeProfitPrice float64,
    stopLossPrice float64,
    leverage int,
) (OrderResult, error) {
    // Set cross margin leverage before placing orders
    if _, err := c.ex.UpdateLeverage(ctx, leverage, pair, true); err != nil {
        return OrderResult{}, fmt.Errorf("exchange: set leverage %dx failed pair=%s: %w", leverage, pair, err)
    }

    isBuy := side == OrderSideLong

    // Round size to pair's szDecimals — same logic as PlaceLimitOrder
    decimals := c.szDecimalsForPair(ctx, pair)
    roundFactor := math.Pow(10, float64(decimals))
    sizeCoins := math.Round((sizeUSD/entryPrice)*roundFactor) / roundFactor

    orders := []hyperliquid.CreateOrderRequest{
        // (A) Entry — limit GTC, opens position
        {
            Coin:       pair,
            IsBuy:      isBuy,
            Size:       sizeCoins,
            Price:      entryPrice,
            ReduceOnly: false,
            OrderType:  hyperliquid.OrderType{Limit: &hyperliquid.LimitOrderType{Tif: "Gtc"}},
        },
        // (B) Stop-loss — trigger market, reduce-only, opposite side
        {
            Coin:       pair,
            IsBuy:      !isBuy,
            Size:       sizeCoins,
            Price:      0, // ignored when IsMarket: true
            ReduceOnly: true,
            OrderType: hyperliquid.OrderType{
                Trigger: &hyperliquid.TriggerOrderType{
                    TriggerPx: stopLossPrice,
                    IsMarket:  true,
                    Tpsl:      hyperliquid.StopLoss,
                },
            },
        },
        // (C) Take-profit — trigger market, reduce-only, opposite side
        {
            Coin:       pair,
            IsBuy:      !isBuy,
            Size:       sizeCoins,
            Price:      0,
            ReduceOnly: true,
            OrderType: hyperliquid.OrderType{
                Trigger: &hyperliquid.TriggerOrderType{
                    TriggerPx: takeProfitPrice,
                    IsMarket:  true,
                    Tpsl:      hyperliquid.TakeProfit,
                },
            },
        },
    }

    grouping := hyperliquid.GroupingNormalTpsl
    resp, err := c.ex.BulkOrder(ctx, orders, &grouping)
    if err != nil {
        return OrderResult{}, fmt.Errorf("exchange: bracket order failed pair=%s: %w", pair, err)
    }

    // Entry order ID is the first response
    var orderID uint64
    if len(resp) > 0 && resp[0].Resting != nil {
        orderID = uint64(resp[0].Resting.Oid)
    }

    slog.Info("bracket order placed",
        "pair", pair,
        "side", side,
        "entry_px", entryPrice,
        "tp_px", takeProfitPrice,
        "sl_px", stopLossPrice,
        "size_coins", sizeCoins,
        "size_usd", sizeUSD,
        "leverage", leverage,
        "entry_order_id", orderID,
        "network", c.cfg.NetworkLabel(),
    )

    return OrderResult{
        OrderID:  orderID,
        Pair:     pair,
        Side:     side,
        Price:    entryPrice,
        SizeUSD:  sizeUSD,
        Leverage: leverage,
        PlacedAt: time.Now(),
    }, nil
}
```

### Cascade Changes to `monitor/position.go`

Because TP/SL are now placed atomically at order time, `waitForFill` no longer needs to place trigger orders. Simplify it:

```go
// waitForFill polls until the entry order is confirmed as a position on the exchange.
// TP/SL are already live (placed atomically via PlaceBracketOrder) — no trigger placement here.
// On timeout: clean up position record and return false.
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

**Removed from `waitForFill`:**
- `PlaceTriggerOrder` calls for TP and SL
- Error handling around trigger placement failures

### Modified Files (Bracket Fix)

| File | Change |
|---|---|
| `exchange/hyperliquid.go` | Add `PlaceBracketOrder`; keep `PlaceLimitOrder` + `PlaceTriggerOrder` but mark deprecated |
| `monitor/position.go` | Remove `PlaceTriggerOrder` calls from `waitForFill`; poll-only |
| All callers of `PlaceLimitOrder` | Update to `PlaceBracketOrder` with `score.TakeProfit` / `score.StopLoss` |

---

## Change 2: `/tv` Command (`mcp/client.go` + `discord/tv.go`)

### 2.1 New File: `mcp/client.go`

Stdio-based MCP client — launches `tradingview-mcp` as a Python subprocess and communicates via JSON-RPC 2.0 over stdin/stdout.

```go
package mcp

import (
    "bufio"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "os/exec"
    "sync"
    "time"
)

type StdioConfig struct {
    Command string   // "uvx"
    Args    []string // ["--from", "tradingview-mcp-server", "tradingview-mcp"]
    Env     []string // extra env vars in "KEY=VALUE" format
}

type Client struct {
    cfg         StdioConfig
    cmd         *exec.Cmd
    stdin       io.WriteCloser
    stdout      *bufio.Scanner
    mu          sync.Mutex
    reqID       int
    CallTimeout time.Duration // default 30s
}

func NewClient(cfg StdioConfig) (*Client, error) { /* spawn subprocess, MCP initialize handshake */ }
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) { /* JSON-RPC tools/call */ }
func (c *Client) Close() error { /* SIGTERM subprocess */ }
```

**Key decisions:**
- Mutex-protected — stdio MCP is serial, one call at a time
- Auto-restart on subprocess crash, exponential backoff (max 3 retries)
- Stderr → `io.Discard` to prevent subprocess stderr from blocking
- `CallTimeout` default 30s

### 2.2 Command Registration (`discord/bot.go`)

```go
// In registerCommands():
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

// In interaction switch:
case "tv":
    b.handleTV(s, i)
```

Bot struct gets one new field:

```go
type Bot struct {
    // ... existing fields ...
    mcpTA *mcp.Client // tradingview-mcp subprocess; nil = fallback mode
}
```

### 2.3 Handler Pipeline (`discord/tv.go`)

```go
func (b *Bot) handleTV(s *discordgo.Session, i *discordgo.InteractionCreate) {
    data := i.ApplicationCommandData()
    coin := strings.ToUpper(data.Options[0].StringValue())

    s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
    })

    ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
    defer cancel()

    // ── Step 1: TradingView MCP ───────────────────────────────────────
    var taResult ta.TAResult
    var taSource string
    var tvSignal string

    if b.mcpTA != nil {
        taRaw, err := b.mcpTA.CallTool(ctx, "get_technical_analysis", map[string]any{
            "symbol":   coin + "USDT",
            "exchange": "BINANCE",
            "screener": "crypto",
            "interval": "4h",
        })
        if err == nil {
            taSummary, parseErr := parseTechnicalAnalysis(taRaw)
            if parseErr == nil {
                taResult = mapTVToTAResult(taSummary, coin)
                taSource = "📡 TradingView MCP · 4H"
                tvSignal = taSummary.TVSignal
            } else {
                slog.Warn("tv: parse failed, falling back", "coin", coin, "err", parseErr)
            }
        } else {
            slog.Warn("tv: tradingview-mcp failed, falling back", "coin", coin, "err", err)
        }
    }

    // Fallback: local techan TA
    if taSource == "" {
        bars, err := b.fetcher.FetchOHLCV(ctx, coin, 200)
        if err != nil {
            editErrorEmbed(s, i, coin, "Both MCP and local TA failed: "+err.Error())
            return
        }
        taResult = ta.Calculate(bars)
        taSource = "⚠️ Legacy TA Mode · 4H"
    }

    // ── Step 2: Market context (non-fatal) ────────────────────────────
    mc, _ := b.fetcher.FetchMarketContext(ctx, coin)

    // ── Step 3: Bot state ─────────────────────────────────────────────
    balance, _ := b.exClient.FetchBalance(ctx)
    lossUSD, winUSD, consecLosses, _ := b.exClient.FetchDailyPnL(ctx)
    state := filter.BotState{
        Balance:           balance,
        DailyLossUSD:      lossUSD,
        DailyWinUSD:       winUSD,
        ConsecutiveLosses: consecLosses,
    }

    // ── Step 4: Pre-filter ────────────────────────────────────────────
    filterResult := filter.ApplyPreFilter(coin, taResult, state)
    if !filterResult.Pass {
        s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
            Embeds: &[]*discordgo.MessageEmbed{{
                Title:       fmt.Sprintf("⏭️ %s — Pre-filter Rejected", coin),
                Description: filterResult.Reason,
                Color:       ColorYellow,
                Fields:      buildTAFields(taResult),
                Footer:      &discordgo.MessageEmbedFooter{Text: taSource},
                Timestamp:   time.Now().Format(time.RFC3339),
            }},
        })
        return
    }

    // ── Step 5: AI scoring ────────────────────────────────────────────
    score, err := b.scorer.Score(ctx, coin, taResult, mc, state)
    if err != nil {
        editErrorEmbed(s, i, coin, "AI scoring failed: "+err.Error())
        return
    }

    slog.Info("tv: analysis complete",
        "coin", coin,
        "action", score.Action,
        "confidence", score.Confidence,
        "ta_source", taSource,
    )

    // ── Step 6: Result embed ──────────────────────────────────────────
    isTradeAction := score.Action == "open_long" || score.Action == "open_short"
    embed := buildTVResultEmbed(coin, score, taResult, mc, tvSignal, taSource)
    s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
        Embeds: &[]*discordgo.MessageEmbed{embed},
    })

    // ── Step 7: Confirm buttons ───────────────────────────────────────
    // Accept → PlaceBracketOrder via go-hyperliquid SDK
    // Decline → "Trade declined" embed
    if isTradeAction && score.Confidence >= float64(config.MinConfidence) {
        b.sendTVConfirmButtons(s, i, coin, score, taResult)
    }
}
```

### 2.4 Execution on Accept

```go
func (b *Bot) executeTVTrade(ctx context.Context, coin string, score aiPkg.ScoreResult, taResult ta.TAResult) {
    // Guard: skip if already in position
    positions, err := b.exClient.FetchPositions(ctx)
    if err == nil {
        for _, pos := range positions {
            if pos.Pair == coin {
                b.sendInfoEmbed(fmt.Sprintf("⏭️ %s — Already in Position", coin),
                    "Open position exists, skipping execution.")
                return
            }
        }
    }

    side := exchange.OrderSideLong
    if score.Action == "open_short" {
        side = exchange.OrderSideShort
    }

    // Atomic bracket order — entry + TP + SL in one call
    result, err := b.exClient.PlaceBracketOrder(
        ctx,
        coin,
        side,
        score.PositionSizeUSD,
        taResult.CurrentPrice,
        score.TakeProfit,
        score.StopLoss,
        score.Leverage,
    )
    if err != nil {
        b.notifyError(coin, "Bracket order failed: "+err.Error())
        return
    }

    b.StartMonitor(ctx, coin, side,
        taResult.CurrentPrice, score.PositionSizeUSD, score.Leverage,
        score.StopLoss, score.TakeProfit, score.Confidence,
        score.Strategy, score.Reasoning, result.OrderID)

    b.notifyTradeExecuted(coin, score, taResult.CurrentPrice, "tradingview-mcp")
}
```

---

## 3. Data Mapping: `get_technical_analysis` → `ta.TAResult`

```go
// TechnicalAnalysisResponse is the parsed output of get_technical_analysis.
// Field keys follow tradingview-ta library conventions.
type TechnicalAnalysisResponse struct {
    CurrentPrice float64 `json:"close"`
    EMA9         float64 `json:"EMA9"`
    EMA21        float64 `json:"EMA21"`
    EMA50        float64 `json:"EMA50"`
    EMA200       float64 `json:"EMA200"`
    RSI          float64 `json:"RSI"`
    MACDLine     float64 `json:"MACD.macd"`
    MACDSignal   float64 `json:"MACD.signal"`
    BBUpper      float64 `json:"BB.upper"`
    BBMiddle     float64 `json:"BB.middle"`
    BBLower      float64 `json:"BB.lower"`
    BBWidth      float64 `json:"BBW"`
    ATR          float64 `json:"ATR"`
    Volume       float64 `json:"volume"`
    AvgVol       float64 `json:"volume_avg"`

    // Values: "BUY" / "SELL" / "NEUTRAL" / "STRONG_BUY" / "STRONG_SELL"
    TVSignal string `json:"Recommend.All"`
}

func mapTVToTAResult(tv TechnicalAnalysisResponse, coin string) ta.TAResult {
    macdHistogram := tv.MACDLine - tv.MACDSignal
    emaSpread := 0.0
    if tv.CurrentPrice > 0 {
        emaSpread = math.Abs(tv.EMA9-tv.EMA21) / tv.CurrentPrice * 100
    }
    return ta.TAResult{
        CurrentPrice:  tv.CurrentPrice,
        EMA9:          tv.EMA9,
        EMA21:         tv.EMA21,
        EMA50:         tv.EMA50,
        EMA200:        tv.EMA200,
        EMASpread:     emaSpread,
        RSI:           tv.RSI,
        RSIZone:       classifyRSIZone(tv.RSI),
        ATR:           tv.ATR,
        MACDValue:     tv.MACDLine,
        MACDSignal:    tv.MACDSignal,
        MACDHistogram: macdHistogram,
        MACDCross:     classifyMACDCross(macdHistogram),
        BBUpper:       tv.BBUpper,
        BBMiddle:      tv.BBMiddle,
        BBLower:       tv.BBLower,
        Volume:        tv.Volume,
        AboveAvgVol:   tv.Volume > tv.AvgVol,
        // S/R: not available from tradingview-mcp — zero-valued
        // scorer handles missing S/R gracefully (no AtSupport/NearResistance bonus)
    }
}
```

---

## 4. All Modified Files

| File | Change |
|---|---|
| `exchange/hyperliquid.go` | Add `PlaceBracketOrder` (atomic entry+TP+SL via `normalTpsl`) |
| `monitor/position.go` | Remove `PlaceTriggerOrder` calls from `waitForFill`; poll-only |
| `discord/bot.go` | Add `mcpTA *mcp.Client`; register `/tv` + handler route; update `New()` |
| `discord/tv.go` | **New** — `/tv` handler, TV→TA mapping, fallback, confirm buttons, `executeTVTrade` |
| `mcp/client.go` | **New** — stdio JSON-RPC 2.0 MCP client |
| `main.go` | Init `mcpTA` client (non-fatal); pass to `discord.New()` |
| `config/config.go` | Add optional `MCP_TA_COMMAND` env override |
| `.env.example` | Document `MCP_TA_COMMAND` var |

---

## 5. New `.env` Variable

```env
# ── TradingView MCP (optional) ───────────────────────────
# Only needed if uvx is not in PATH
MCP_TA_COMMAND=uvx
# default args: --from tradingview-mcp-server tradingview-mcp
```

---

## 6. Fallback Strategy

| Failure Point | Behaviour |
|---|---|
| `tradingview-mcp` won't start at boot | Log warning; `b.mcpTA = nil`; `/tv` always uses local TA |
| `get_technical_analysis` times out (30s) | Retry once; fall back to local `ta.Calculate()` |
| `get_technical_analysis` parse error | Fall back to local `ta.Calculate()` |
| Local TA also fails | Red error embed — cannot proceed |
| AI scorer fails | Red error embed; no execution |
| `PlaceBracketOrder` SDK error | Red error embed with reason |
| Existing position for coin | Blue info embed; skip silently |
| Pre-filter rejects | Yellow skipped embed with reason |

---

## 7. Execution Flow

```
User: /tv SOL
  │
  ▼
Deferred response ("Bot is thinking…")
  │
  ▼
tradingview-mcp :: get_technical_analysis("SOLUSDT", screener="crypto", exchange="BINANCE", interval="4h")
  │ → 23+ indicators + STRONG_BUY / BUY / NEUTRAL / SELL / STRONG_SELL
  │ ✗ fail/timeout → local ta.Calculate() · "⚠️ Legacy TA Mode" footer
  ▼
mapTVToTAResult() → ta.TAResult (S/R fields zero-valued, handled gracefully)
  │
  ▼
Pre-filter (EMA spread, RSI, daily limits, budget)
  │ → PASS / yellow skip embed
  ▼
AI scorer :: b.scorer.Score(coin, taResult, mc, state)
  │ → open_long / open_short / hold / wait + confidence + size + leverage + TP/SL
  ▼
Result embed posted (green = trade, yellow = wait/hold)
  │
  ▼ (EXECUTE + confidence ≥ 55%)
Accept / Decline buttons
  │
  ▼ (Accept clicked)
FetchPositions → skip if already open
  │
  ▼
exchange.PlaceBracketOrder(coin, side, sizeUSD, entryPx, tpPx, slPx, leverage)
  │ → grouping: "normalTpsl"
  │ → 3 orders atomic:
  │     (A) entry limit GTC
  │     (B) SL trigger market reduce-only  ─┐ activate on fill
  │     (C) TP trigger market reduce-only  ─┘ OCO: one fills → other cancelled ✅
  ▼
b.StartMonitor(…) — existing two-ticker goroutine
b.notifyTradeExecuted(…) — green Discord embed
```

---

## 8. Key Differences: `/tv` vs `/scan`

| | `/scan` | `/tv` |
|---|---|---|
| TA source | Local techan + 200 OHLCV candles | TradingView MCP real-time screener |
| Timeframe | 4H | 4H |
| S/R detection | ✅ Swing point detection | ❌ Not available (zero-valued, graceful) |
| Execution | `PlaceBracketOrder` (after v2 fix) | `PlaceBracketOrder` (same) |
| TP/SL placement | Atomic at order time ✅ | Atomic at order time ✅ |
| Pair selection | Random from `pairs.json` | User-specified |
| Fallback | N/A | Local techan TA |
| Confirmation | Accept/Decline buttons | Accept/Decline buttons |

---

> *"I do not guess. I do not hope. I see my setup, I execute, I accept the outcome."*
> — Mambo AI Trade v2