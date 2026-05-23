# 🤖 Mambo AI Trade

> Autonomous AI-powered futures trading bot for Hyperliquid.  
> Built with Go. Disciplined. Emotionless. Balance-aware. Multi-provider AI.
<img width="558" height="540" alt="mamboooo" src="https://github.com/user-attachments/assets/f3f3f9c9-00d9-49a4-bb7f-d5e61782696c" />

---

## 📌 Overview

Mambo AI Trade autonomously scans crypto pairs, performs technical analysis using the **techan** library + custom support/resistance detection, consults an AI trading agent via **[Charmbracelet Fantasy](https://github.com/charmbracelet/fantasy)** for **structured, type-safe decisions** (no regex parsing), and executes bracket orders on Hyperliquid Futures — all within a dynamic, balance-aware rule set.

Supports multiple AI providers (Grok, OpenAI, DeepSeek, Anthropic) switchable via `.env`. You monitor and control everything through a private Discord bot with role-based access control and **private DMs** for per-pair AI decisions.

---

## 🗂️ Project Structure

```
mambo/
├── main.go                     # Entry point — wires all modules, Discord bot, monitor
├── pairs.json                  # Legacy trading universe
├── common_pairs.json           # Expanded trading universe (174+ pairs, 4H timeframe)
├── AGENT.md                    # AI trader identity & rules (system prompt)
├── CLAUDE.md                   # Coding rules (auto-read by Claude CLI)
├── prompts/
│   ├── execute_trade.md        # /execute AI prompt template
│   ├── verify_trade.md         # Per-trade AI verification template
│   └── suggest_trade.md        # AI advisory suggestion template (DYOR mode)
├── .env                        # All secrets + feature flags
├── .env.example                # Template
├── config/
│   └── config.go               # Env loader, constants, multi-provider AI config
├── market/
│   ├── fetcher.go              # OHLCV + sentiment (alternative.me, Binance, Altfins)
│   └── pairs.go                # Load / get random pairs from pairs.json
├── ta/
│   ├── indicators.go           # techan wrapper + swing S/R detection
│   └── divergence.go           # RSI + MACD divergence detection
├── filter/
│   └── rules.go                # Hard-coded pre-AI filter rules
├── ai/
│   ├── providers.go            # Fantasy provider factory (Grok/OpenAI/DeepSeek/Anthropic)
│   ├── client.go               # Legacy HTTP client (kept for backward compat)
│   ├── scorer.go               # Structured trade scoring via object.Generate[ScoreResult]
│   └── position_manager.go     # Structured position decisions via object.Generate[PositionDecision]
├── exchange/
│   └── hyperliquid.go          # go-hyperliquid SDK wrapper (PlaceBracketOrder, PlaceTriggerOrder)
├── monitor/
│   └── position.go             # Two-ticker goroutine (price + AI intervals)
├── discord/
│   ├── bot.go                  # Init, slash commands, role auth, StartMonitor, SendDM
│   ├── notify.go               # Embed notifications + motivational quotes
│   ├── commands.go             # Slash command handlers (/check, /execute, /scan, etc.)
│   ├── hunt.go                 # /hunt — auto-scan + execute (Altfins batch + AI scoring + DMs)
│   ├── verify.go               # /verify — HL vs Altfins candle comparison
│   └── checker.go              # /check coin:SOL handler
├── journal/
│   └── logger.go               # positions.json + journal.json + decision-log.json + executed-log.json
├── logger/
│   └── logger.go               # Colored slog handler (cyan=DEBUG, green=INFO, yellow=WARN, red=ERROR)
├── positions.json              # Open positions (recovery on restart)
├── journal.json                # Closed trades history (local backup, /journal uses live API)
├── decision-log.json           # Hunt decisions (cleared on startup, executed moves to executed-log)
└── executed-log.json           # Permanent record of all executed trades
```

---

## ⚙️ Environment Variables (`.env`)

```env
# ── Hyperliquid ──────────────────────────────────────────
HL_AGENT_PRIVATE_KEY=0x...   # Agent wallet private key (NOT main wallet)
HL_ACCOUNT_ADDRESS=0x...     # Your MAIN wallet address
HL_TESTNET=true              # true = testnet | false = mainnet

# ── AI Provider ──────────────────────────────────────────
# Choose: grok | openai | deepseek | anthropic
AI_PROVIDER=deepseek

# Optional: override default model for selected provider
# AI_MODEL=mimo-v2.5-pro

# All providers support custom base URLs for proxies / OpenRouter alternatives
# AI_BASE_URL=https://your-proxy.com/v1

# ── AI API Keys (only fill the one matching AI_PROVIDER) ─
XAI_API_KEY=xai-...          # Grok     → console.x.ai
OPENAI_API_KEY=              # OpenAI   → platform.openai.com
DEEPSEEK_API_KEY=            # DeepSeek → platform.deepseek.com
ANTHROPIC_API_KEY=           # Claude   → console.anthropic.com

# ── Discord ──────────────────────────────────────────────
DISCORD_BOT_TOKEN=
DISCORD_GUILD_ID=
DISCORD_CHANNEL_ID=
DISCORD_AUTHORIZED_ROLE_ID=

# ── Monitor Intervals ─────────────────────────────────────
MONITOR_PRICE_SEC=10         # Price + hard rules (min 5s, default 10s)
MONITOR_AI_SEC=1200          # AI position analysis (min 60s, default 20min)

# ── Altfins (optional) ───────────────────────────────────
ALTFINS_API_KEY=             # Altfins API key for batch OHLCV pre-screening
```

---

## 🤖 AI Providers & Structured Output

Mambo uses **[Charmbracelet Fantasy](https://github.com/charmbracelet/fantasy)** for all AI calls. Fantasy provides:

- **Unified provider interface** — Grok, OpenAI, DeepSeek, Anthropic via a single API
- **Structured output** — `object.Generate[ScoreResult]` returns a fully typed Go struct. Zero regex parsing. Zero "no decision block" errors.
- **Custom base URLs** — route through proxies, OpenRouter alternatives, or self-hosted models

### Supported Providers

| Provider | Value | Default Model | Fantasy Backend |
|---|---|---|---|
| **Grok (xAI)** | `grok` | `grok-4.20-0309-reasoning` | `openaicompat` |
| **OpenAI** | `openai` | `gpt-5.4` | `openaicompat` |
| **DeepSeek** | `deepseek` | `deepseek-v4-pro` | `openaicompat` |
| **Anthropic** | `anthropic` | `claude-opus-4-5` | `anthropic` (native) |

To switch provider: change `AI_PROVIDER` and set the matching API key. No code changes needed.

### How Structured Output Works

```
Before (regex parsing):
  AI text → regex hunt for <decision> → json.Unmarshal → ScoreResult
  ❌ "no decision block in AI response" when formatting drifts

After (Fantasy):
  object.Generate[ScoreResult] → ScoreResult (directly typed)
  ✅ Schema auto-generated from Go struct tags — AI always returns valid JSON
```

---

## 📰 News Research Agent

Before AI trade scoring, Mambo can run a **deep news sentiment scan** on the selected pair (if enabled). The `news_research_agent.md` prompt instructs the AI to:

- Scan **CryptoPanic, CoinDesk, The Block, Decrypt, CoinTelegraph** + financial/community sources
- Assign a weighted sentiment score (-1.0 to +1.0)
- Make a **gate decision**: `PASS` / `WARN` / `SKIP`
- Output a machine-readable JSON report

```
Gate logic:
  SKIP  → score ≤ -0.6, or any High-impact negative event (hack, SEC, delisting)
  WARN  → score -0.3 to -0.59, or token unlock / whale movement detected
  PASS  → score ≥ -0.29, no red flags
```

Hard overrides (auto-SKIP): exchange hack, smart contract exploit, SEC enforcement, delisting, founder exit, network outage > 1h.

---

## 📊 Technical Analysis

| Indicator | Source |
|---|---|
| EMA 9, 21, 50, 200 | `github.com/sdcoffey/techan` |
| RSI 14 + divergence | techan + custom divergence detection |
| MACD (12,26,9) + divergence | techan + custom divergence detection |
| Bollinger Bands (20,2) | techan |
| ATR 14 (dynamic SL) | techan |
| OBV | techan |
| Support / Resistance | Custom swing point detection |

### Support & Resistance
- **Support** = nearest swing low below price (3-candle pivot method)
- **Resistance** = nearest swing high above price
- **Strength** = `strong` (3+ tests), `moderate` (2), `weak` (1)
- `AtSupport = true` → +1 confluence, can size up
- `NearResistance = true` → size down automatically

### Divergence Detection
- **RSI Divergence**: Regular (trend reversal) / Hidden (trend continuation)
- **MACD Divergence**: Regular / Hidden
- **Double Divergence**: RSI + MACD agree — highest confidence signal

---

## 💰 Dynamic Capital Rules

| Rule | Formula | Example $20 |
|---|---|---|
| Min trade size | 5% | $1.00 |
| Max trade size | 20% | $4.00 |
| Max capital at risk | 60% | $12.00 |
| AI decides size | 5–20% | confluence + S/R aware |
| AI decides leverage | 1–10x cross | confidence + volatility |
| Daily loss limit | 15% | $3.00 → pause |
| Daily win limit | 30% | $6.00 → pause |

---

## 🔄 Full Trading Flow

```
1. STARTUP
   Load config + AGENT.md + pairs.json
   Init Fantasy provider → LanguageModel (structured output ready)
   Register Discord slash commands
   Recover positions.json → spawn monitor goroutines

2. FETCH BALANCE → recalculate dynamic limits

3. FETCH OHLCV (200 candles, 4H) per pair

4. TECHNICAL ANALYSIS
   EMA 9/21/50/200, RSI 14, MACD, BB, ATR, OBV (techan)
   Support/Resistance swing point detection (custom)
   RSI + MACD divergence detection

5. PRE-FILTER (hard rules, no AI)
   ❌ EMA spread < 0.2%       → skip (sideways)
   ❌ Price < EMA200           → skip (bear market)
   ❌ RSI > 70 or < 30         → skip pair
   ❌ Volume < average         → skip pair
   ❌ 2 consecutive losses     → skip all
   ❌ Daily loss ≥ 15%         → skip all
   ❌ Capital at risk ≥ 60%    → skip all

6. ENRICH CONTEXT (free APIs)
   Fear & Greed, Funding rate, OI, Long/Short ratio

7. AI SCORING (Fantasy structured output)
   object.Generate[ScoreResult] → schema-driven, type-safe
   Input: all TA + S/R + market context + portfolio state
   Output: open_long / open_short / hold / wait + size + leverage + strategy

8. VALIDATE + CLAMP (Go code — AI cannot bypass)
   confidence < 50% → force wait
   R:R < 2.0 → force wait
   size: 5–20%, leverage: 1–10x
   /execute always executes: clamps to user-specified leverage, min size

9. CONFIRMATION (Discord + decision log)
   /scan → shows "🔍 Scanning best pair…" with Accept/Decline buttons
   /hunt → screens 20 pairs/batch (Altfins), scores top 6 with AI, best wins
   All hunt decisions logged to decision-log.json (executed trades → executed-log.json)

10. ORDER EXECUTION
    PlaceBracketOrder: entry limit + TP trigger + SL trigger atomically
    All 3 orders via BulkOrders — TP/SL activated after fill
    Cross margin, leverage set before orders
    Auto-saves position → spawns monitor goroutine

11. MONITOR (two tickers, starts after position fills)
    WaitForFill → confirm position exists on exchange (polls every 15s, 120min timeout)
    TP/SL already placed by PlaceBracketOrder (visible on Hyperliquid UI)
    priceTicker (MonitorPriceSec) → TP/SL hit, hard rules
    aiTicker    (MonitorAISec)    → AI position analysis (Fantasy structured output)
    Auto-closes position → Discord notification → logged to journal.json
```

---

## 👁️ Position Monitor

```
waitForFill (up to 120min, polls every 15s):
  Confirms position exists on exchange before monitoring begins
  TP/SL already placed atomically by PlaceBracketOrder
  On timeout: cleans up position record (no trade logged)

priceTicker (fast — default 10s):
  TP/SL hit, drawdown ≥ 40%, hold > 240min, smart loss cut

aiTicker (slow — default 20min, configurable):
  Fetch fresh TA + S/R → AI via object.Generate[PositionDecision] → hold/close/move_sl/move_tp
  AI is S/R + divergence aware:
    Near resistance + PnL > 3% → consider closing
    Hidden bullish divergence + long in drawdown → hold
    Double divergence (RSI + MACD agree) → weight heavily
```

---

## 🤖 Discord Bot

### Role-Based Access
Only Discord members with `DISCORD_AUTHORIZED_ROLE_ID` can interact. Unauthorized users get no response.

### Slash Commands

| Command | Description |
|---|---|
| `/check coin:SOL [interval:4h]` | TA + AI analysis with S/R levels (read-only) |
| `/hunt [interval:4h]` | Auto-scan pairs, AI scores best, auto-execute + monitor. Wraps when exhausted. |
| `/execute coin:SOL leverage:5 [interval:4h] [bypass:false] [tp:120] [sl:95]` | Full pipeline (bypass=false) or direct execution (bypass=true) |
| `/scan [interval:4h]` | Scan random pairs for trade setup (Accept/Decline/Suggest/Skip buttons) |
| `/status` | Open positions + live PnL from exchange |
| `/journal [range:today] [page:1]` | Trade history from Hyperliquid (today or week, paginated) |
| `/pnl` | Total PnL + per-coin breakdown (live from Hyperliquid) |
| `/mode auto\|manual` | Toggle trading mode |
| `/capital` | Balance + limits + remaining budget |
| `/pairs` | Active trading pairs |
| `/verify coin:SOL [interval:1d]` | HL vs Altfins candle comparison with deviation % |

### /execute Parameters

| Parameter | Required | Description |
|---|---|---|
| `coin` | ✅ | Ticker symbol (e.g. SOL, BTC) |
| `leverage` | ✅ | 1–10x cross margin (clamped if out of range) |
| `interval` | ❌ | Timeframe (default: 4h) |
| `bypass` | ❌ | Skip prefilter + AI — execute immediately (default: false) |
| `tp` | ❌* | Take-profit price — **required when bypass=true** |
| `sl` | ❌* | Stop-loss price — **required when bypass=true** |

| Mode | Leverage | Direction | Size | TP/SL | Entry |
|---|---|---|---|---|---|
| `bypass:false` | User-specified (clamped 1-10x) | AI decides | AI decides (5-20%) | AI decides | Current price (BracketOrder) |
| `bypass:true` | User-specified (clamped 1-10x) | LONG if > EMA200, else SHORT | 10% balance | User-specified (required) | Current price (BracketOrder) |

---

## 🏹 /hunt — Autonomous Trade Hunting

/hunt automatically finds and executes the best trade across the entire pair universe:

```
1. Pick 20 random unseen pairs (wraps around when all exhausted)
2. Batch pre-screen via Altfins (OHLCV snapshot — no per-pair API calls)
3. Rank by volume → top 6 get full TA + AI scoring (parallel, sem=2, staggered 300ms)
4. AI scores each pair via object.Generate[ScoreResult] — type-safe structured output
5. All decisions logged to decision-log.json (audit trail)
6. Best trade (highest confidence >= MinConfidence) wins → PlaceBracketOrder
7. Executed trade also logged to executed-log.json (permanent record)
8. Monitor starts after fill (TP/SL already placed atomically)
```

**Log lifecycle**: On bot startup, `decision-log.json` is cleaned — executed entries move to `executed-log.json` (permanent), everything else is discarded.

**Performance**: 20 pairs batch-screened in ~3s, top 6 AI-scored in ~30s, 5 cycles max (~2.5min delay between).

---

## 🛡️ Safety Layers

```
Layer 1  Trend Gate (Go)               EMA spread < 0.2% → skip
Layer 2  Pre-filter Rules (Go)          RSI, EMA, volume, budget
Layer 3  Market Context (free APIs)     Fear & Greed, funding, OI, L/S ratio
Layer 4  AI Structured Scoring          object.Generate[ScoreResult] — schema-enforced, type-safe
Layer 5  Output Clamp (Go)             size 5–20%, leverage 1–10x
Layer 6  Order Safety (Go)             BracketOrder: entry limit + TP/SL triggers atomically
Layer 7  User Confirmation (Discord)   Accept/Decline buttons on /scan; /hunt auto-executes if confident
Layer 8  Position Fill Check (Go)      waitForFill before monitoring (TP/SL pre-placed)
Layer 9  Position Hard Rules (Go)      drawdown, max hold, smart loss cut
Layer 10 AI Position Management        object.Generate[PositionDecision] — hold/close/move SL/TP
Layer 11 Discord Role Auth (Go)        only authorized role can interact
```

---

## 📦 Tech Stack

| Component | Technology |
|---|---|
| Language | Go |
| Exchange SDK | `github.com/sonirico/go-hyperliquid` v0.36 |
| AI Framework | `charm.land/fantasy` v0.25 — structured output, multi-provider |
| AI Providers | Grok / OpenAI / DeepSeek / Anthropic (switchable via `.env`) |
| TA Library | `github.com/sdcoffey/techan` + custom S/R + divergence |
| Discord | `github.com/bwmarrin/discordgo` |
| Env loading | `github.com/joho/godotenv` |
| Sentiment | alternative.me + Binance Public API (free) |
| Pre-screening | Altfins batch OHLCV API |
| News Research | AI-powered multi-source scan (CryptoPanic, CoinDesk, etc.) |

---

## 🚀 Getting Started

```bash
git clone https://github.com/StephanieAgatha/mambo
cd mambo
go mod tidy
cp .env.example .env
# Fill in: HL keys, AI_PROVIDER + matching API key, Discord config
# Keep HL_TESTNET=true until everything works
go run main.go
```

---

## 📋 Changelog

### v3 — Fantasy Integration (current)
- Replaced regex-based AI response parsing with `object.Generate[T]` — **zero parsing errors**
- Added `ai/providers.go` — Fantasy provider factory with custom base URL support
- `/execute` now requires `leverage` parameter; `tp`/`sl` required for bypass mode
- TP/SL direction validation in `/execute` (rejects backwards levels for long/short)
- Unified order execution via `PlaceBracketOrder` (entry + TP + SL atomically)
- Price rounding to 5 significant figures + non-zero LimitPx on trigger orders (HL requirement)
- AI fallback chain: tool mode → text mode → JSON extraction from XML-wrapped responses
- OHLCV fetch retries up to 5x with backoff
- `/hunt` decisions logged to `decision-log.json` (replaces DMs); executed trades → `executed-log.json`
- Decision log cleaned on startup (non-executed entries purged, executed preserved)
- Removed TradingView MCP integration (`/tv`, `mcp/`, `discord/tv.go`)
- Colored logger rewrite — no more escaped ANSI codes
- RSI + MACD divergence detection (regular, hidden, double)

### v2 — May 2026
- `/hunt` — autonomous trade hunting with Altfins batch pre-screening
- Wrap-around pair exhaustion (resets seen pairs when all scanned)
- AI client retry on 5xx/timeout (up to 3 attempts with backoff)
- TP/SL trigger orders via `PlaceTriggerOrder` + `PlaceBracketOrder`
- `waitForFill` — monitor waits for position to fill before acting
- Altfins `FlexFloat` unmarshaling for both JSON strings and numbers
- `/verify` — Hyperliquid vs Altfins candle comparison command

### v1 — Initial Release
- Foundation: config, exchange SDK, market fetcher
- TA with techan + custom S/R detection
- Pre-filter hard rules + multi-provider AI scoring
- Discord bot with slash commands, embeds, role auth
- Two-ticker position monitor (price + AI intervals)
- Journal: positions.json + journal.json with WIB timestamps

---

> *"I do not guess. I do not hope. I see my setup, I execute, I accept the outcome. Greed is my only enemy."*  
> — Mambo AI Trade