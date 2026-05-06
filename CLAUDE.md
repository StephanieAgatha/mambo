# CLAUDE.MD – Mambo AI Trade
## Coding Standards + Build Order

Auto-read by Claude CLI from project root. Follow every rule here strictly.

---

## 🏗️ Build Order (Phase by Phase)

Work through these in order. Test each phase before moving to the next.

```
Phase A  ✅  config/config.go + main.go (skeleton)
Phase B  ✅  exchange/hyperliquid.go + market/fetcher.go
Phase C  ✅  ta/indicators.go (techan + S/R detection)
Phase D  ✅  filter/rules.go + ai/scorer.go
Phase E  ✅  journal/logger.go
Phase F  ✅  ai/position_manager.go + monitor/position.go
Phase G  🔜  discord/bot.go + notify.go + commands.go + checker.go
Phase H  🔜  main.go (final wire — scan loop + all modules)
```

---

## 📦 Project Context

```
Exchange SDK  : github.com/sonirico/go-hyperliquid
  - Auth      : Agent wallet private key (not main wallet)
  - Network   : HL_TESTNET=true → testnet | false → mainnet
  - Info      : hyperliquid.NewInfo(ctx, apiURL, true, nil, nil, nil)
  - Exchange  : hyperliquid.NewExchange(ctx, key, url, nil, "", accountAddr, nil, nil)

AI Providers  : grok | openai | deepseek | anthropic (via AI_PROVIDER env)
  - Router    : ai/client.go → Chat(ctx, system, user) string
  - OpenAI-compatible: grok, openai, deepseek → /chat/completions + Bearer token
  - Anthropic : custom format → /v1/messages + x-api-key header
  - Config    : cfg.AIProvider, cfg.AIModel, cfg.AIBaseURL, cfg.AIAPIKey
  - Label     : cfg.AILabel() → "grok/grok-4.20-0309-reasoning"

TA Library    : github.com/sdcoffey/techan (NEVER write indicator math manually)
  + Custom S/R: swing low/high detection in ta/indicators.go

Discord       : github.com/bwmarrin/discordgo
  - Auth      : role-based (DISCORD_AUTHORIZED_ROLE_ID)
  - All output: embed messages, never plain text
  - Public    : no ephemeral flags

Storage       : positions.json + journal.json (RFC3339 + WIB timezone)
Capital       : all % of live balance — never hardcode dollar amounts
Monitor       : two tickers — priceTicker (MonitorPriceSec) + aiTicker (MonitorAISec)
```

---

## 📁 Package Responsibilities

```
config/         → env loading, constants, guardrails, provider config
logger/         → color slog handler (New() returns *slog.Logger)
market/         → OHLCV (type: OHLCV not Candle), market context, free APIs
ta/             → techan wrapper + S/R swing detection → TAResult struct
filter/         → pre-AI hard rules → BotState + FilterResult structs
ai/
  client.go     → provider router, Chat() interface
  scorer.go     → trade scoring, fills verify_trade.md, calls client.Chat()
  position_manager.go → position decisions, calls client.Chat()
exchange/       → go-hyperliquid SDK wrapper (FetchBalance, FetchPositions,
                  FetchCurrentPrice, PlaceLimitOrder, CancelOrder, ClosePosition)
monitor/        → two-ticker goroutine per position (priceTicker + aiTicker)
journal/        → positions.json + journal.json read/write, WIB timestamps
discord/        → bot init, slash commands, embed notifications
```

---

## 1. Error Wrapping — Always Add Context

```go
// ❌ BAD
if err != nil {
    return err
}

// ✅ GOOD
if err != nil {
    return fmt.Errorf("fetchOHLCV %s: failed to decode candles: %w", pair, err)
}
```

Every error: **where** (func/package) + **what** (attempted) + **why** (`%w`).

---

## 2. Logging — slog, Structured, Colored

Use `slog` only. No `fmt.Println` in production paths.
Color handler is in `logger/logger.go` — init once in `main.go`.

```go
// ❌ BAD
fmt.Println("order placed")
log.Println("balance:", balance)

// ✅ GOOD
slog.Info("order placed",
    "pair", "SOL",
    "side", "long",
    "price", 140.50,
    "size_usd", 180.00,
    "leverage", 7,
    "network", cfg.NetworkLabel(),
    "provider", cfg.AIProvider,
)

slog.Error("hyperliquid: place order failed — will retry next cycle",
    "pair", pair,
    "err", err,
)
```

Log levels:
```
DEBUG → gray   (internal state, loop ticks, indicator values)
INFO  → green  (orders, signals, lifecycle, AI decisions)
WARN  → yellow (unexpected but recoverable, API fallbacks)
ERROR → red    (failed, needs attention)
```

---

## 3. Variable Naming — No Ambiguity

```go
// ❌ BAD
p := getPair()
b, e := fetch(p)

// ✅ GOOD
pair := getPair()
balance, err := fetchBalance(ctx)
```

- Boolean: `isTestnet`, `hasRemainingBudget`, `isOrderFilled`, `atSupport`, `nearResistance`
- No single letters except loop `i`
- Structs: noun — `OpenPosition`, `TAResult`, `ScoreResult`, `FilterResult`, `MarketContext`
- Functions: verb+noun — `fetchBalance`, `placeOrder`, `applyPreFilter`, `detectSupportResistance`
- Constants:

```go
const (
    EMASpreadThreshold  = 0.002  // 0.2% — below = sideways market → skip
    MaxLeverageX        = 10
    MinSizePct          = 0.05
    MaxCapitalAtRiskPct = 0.60
    OrderTimeoutMin     = 10
)
```

---

## 4. Comments — When to Write

```go
// ✅ Write when code makes an assumption
// assumes candles are sorted ascending (oldest → newest)
// techan requires this order for correct EMA/RSI calculation
series := buildTimeSeries(candles)

// ✅ Write when error path is non-obvious
// DeadlineExceeded = Hyperliquid did not respond within timeout
// safe to skip this cycle and retry on next tick
if errors.Is(err, context.DeadlineExceeded) {
    return nil, ErrHyperliquidTimeout
}

// ✅ Write when logic is intentionally counter-intuitive
// clamp AI output BEFORE placing order — AI can return values outside guardrails
// never trust AI output directly for order parameters
leverage = clamp(aiLeverage, MinLeverageX, MaxLeverageX)

// ✅ Write for magic numbers
// 0.2% minimum EMA spread — below this = sideways/choppy market
// EMA crossovers in this zone produce too many false signals
const EMASpreadThreshold = 0.002

// ❌ Never comment obvious code
i++           // increment
return err    // return error
```

---

## 5. General Rules

- No magic numbers → named constants
- No nesting > 3 levels → extract to function
- No function > 60 lines → split it
- Return early (guard clauses), flat happy path

```go
// ❌ BAD
func process() {
    if condA {
        if condB {
            if condC { /* buried logic */ }
        }
    }
}

// ✅ GOOD
func process() error {
    if !condA { return fmt.Errorf("process: condA not met") }
    if !condB { return fmt.Errorf("process: condB not met") }
    if !condC { return fmt.Errorf("process: condC not met") }
    // logic here, flat and readable
}
```

---

## 6. techan Rules — Never Write Indicator Math

```go
// ✅ CORRECT — always use techan
series := techan.NewTimeSeries()
// populate...
closePrices := techan.NewClosePriceIndicator(series)
ema9        := techan.NewEMAIndicator(closePrices, 9)
ema21       := techan.NewEMAIndicator(closePrices, 21)
ema50       := techan.NewEMAIndicator(closePrices, 50)
ema200      := techan.NewEMAIndicator(closePrices, 200)
rsi14       := techan.NewRelativeStrengthIndexIndicator(closePrices, 14)
macd        := techan.NewMACDIndicator(closePrices, 12, 26)
bb          := techan.NewBollingerBandsIndicator(closePrices, 20, 2)
atr         := techan.NewAverageTrueRangeIndicator(series, 14)
obv         := techan.NewOBVIndicator(series)

// ❌ WRONG — never implement formulas manually
func calculateEMA(prices []float64, period int) float64 { ... }
func calculateRSI(prices []float64) float64 { ... }
```

S/R detection uses raw OHLCV bars (not techan) — custom swing point logic in `ta/indicators.go`.

---

## 7. go-hyperliquid SDK Rules

```go
// ✅ Always use SDK, never raw HTTP to Hyperliquid
import hyperliquid "github.com/sonirico/go-hyperliquid"

// Init in exchange/hyperliquid.go — pass Client around, don't re-init
rawKey := strings.TrimPrefix(cfg.AgentPrivateKey, "0x")
privateKey, err := crypto.HexToECDSA(rawKey)

apiURL := hyperliquid.MainnetAPIURL
if cfg.Testnet {
    slog.Warn("⚠️  TESTNET MODE — no real funds at risk", "api_url", apiURL)
    apiURL = hyperliquid.TestnetAPIURL
}

// Info  = read-only (balance, positions, candles, prices)
info := hyperliquid.NewInfo(ctx, apiURL, true, nil, nil, nil)

// Exchange = write (orders, leverage, cancel)
ex := hyperliquid.NewExchange(ctx, privateKey, apiURL, nil, "", cfg.AccountAddress, nil, nil)

// All string fields from SDK need strconv.ParseFloat — they are NOT float64
balance, err := strconv.ParseFloat(state.MarginSummary.AccountValue, 64)
```

---

## 8. AI Client Rules

```go
// ✅ Always use ai/client.go — never direct HTTP to AI providers
client := ai.NewClient(cfg, 120*time.Second)
raw, err := client.Chat(ctx, systemPrompt, userPrompt)

// client.go handles all provider differences internally:
// grok/openai/deepseek → /chat/completions + Bearer token
// anthropic → /v1/messages + x-api-key + anthropic-version header

// Always log which provider was used
slog.Info("AI call complete",
    "provider", cfg.AIProvider,
    "model", cfg.AIModel,
    "pair", pair,
)

// Always clamp AI output — never trust raw AI numbers
leverage = clamp(aiResult.Leverage, config.MinLeverageX, config.MaxLeverageX)
size     = clamp(aiResult.PositionSizeUSD, minSize, math.Min(maxSize, remaining))
```

---

## 9. Discord Rules

```go
// Color constants — always use these, never raw hex strings
const (
    ColorGreen  = 0x00C853 // WIN, EXECUTE, TP hit, order placed
    ColorRed    = 0xD50000 // LOSS, ABORT, SL hit, daily limit hit
    ColorYellow = 0xFFD600 // signal found (manual mode)
    ColorBlue   = 0x2979FF // position updated, info commands
)

// Role auth — FIRST line of every command handler, before anything else
func handleCheck(s *discordgo.Session, i *discordgo.InteractionCreate) {
    if !hasAuthorizedRole(i.Member, cfg.DiscordAuthorizedRoleID) {
        return // silently ignore — no response to unauthorized users
    }
    // handler logic here
}

// /check thinking state — defer immediately, edit after AI responds
s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
    Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
    // no Flags → public message (visible to everyone in channel)
})
// ... fetch + TA + AI ...
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
    Embeds: &[]*discordgo.MessageEmbed{resultEmbed},
})

// Never use ephemeral — all messages public
// Never send plain text — always use embeds
// Always include motivational quote in embed footer for trade events
```

---

## 10. Import Groups

```go
import (
    // stdlib
    "context"
    "fmt"
    "log/slog"
    "os"
    "strconv"
    "time"

    // external
    "github.com/bwmarrin/discordgo"
    "github.com/ethereum/go-ethereum/crypto"
    hyperliquid "github.com/sonirico/go-hyperliquid"
    "github.com/sdcoffey/techan"
    "github.com/joho/godotenv"

    // internal
    "mambo/config"
    "mambo/exchange"
    "mambo/ta"
    "mambo/ai"
)
```

---

## 11. Journal + Timestamps

```go
// Always use WIB (UTC+7) for timestamps — Indonesian timezone
wib := time.FixedZone("WIB", 7*60*60)
timestamp := time.Now().In(wib).Format(time.RFC3339)

// Use journal.Now() helper — already handles WIB
pos.OpenedAt = journal.Now()

// Use journal.GeneratePositionID() for unique IDs
pos.ID = journal.GeneratePositionID() // "pos_<unix_milli>"
```

---

**End of CLAUDE.md — Mambo AI Trade**