# 🤖 Mambo AI Trade

> Autonomous AI-powered futures trading bot for Hyperliquid.  
> Built with Go. Disciplined. Emotionless. Balance-aware. Multi-provider AI.

---

## 📌 Overview

Mambo AI Trade autonomously scans crypto pairs, performs technical analysis using the **techan** library + custom support/resistance detection, runs an AI-powered **news sentiment scan** (CryptoPanic, CoinDesk, The Block, etc.), consults an AI trading agent, and executes cross margin limit orders on Hyperliquid Futures — all within a dynamic, balance-aware rule set. Supports multiple AI providers (Grok, OpenAI, DeepSeek, Anthropic) switchable via `.env`. You monitor and control everything through a private Discord bot with role-based access control.

---

## 🗂️ Project Structure

```
mambo/
├── main.go                     # Entry point — wires all modules, Discord bot, monitor
├── pairs.json                  # Legacy trading universe
├── common_pairs.json           # Expanded trading universe (174+ pairs, 4H timeframe)
├── analysis_log.json           # TA analysis log (per-cycle diagnostic dump)
├── AGENT.md                    # AI trader identity & rules (system prompt)
├── CLAUDE.md                   # Coding rules (auto-read by Claude CLI)
├── news_research_agent.md      # News sentiment scan agent prompt + JSON spec
├── prompts/
│   └── verify_trade.md         # Per-trade AI verification template
├── .env                        # All secrets + feature flags
├── .env.example                # Template
├── config/
│   └── config.go               # Env loader, constants, multi-provider AI config
├── market/
│   ├── fetcher.go              # OHLCV + sentiment (alternative.me, Binance)
│   └── pairs.go                # Load / get random pairs from pairs.json
├── ta/
│   └── indicators.go           # techan wrapper + swing S/R detection
├── filter/
│   └── rules.go                # Hard-coded pre-AI filter rules
├── ai/
│   ├── client.go               # Provider router (Grok/OpenAI/DeepSeek/Anthropic)
│   ├── scorer.go               # Trade scoring — EXECUTE/ABORT + size + leverage
│   └── position_manager.go     # Position management — hold/close/move SL/TP
├── exchange/
│   └── hyperliquid.go          # go-hyperliquid SDK wrapper
├── monitor/
│   └── position.go             # Two-ticker goroutine (price + AI intervals)
├── discord/
│   ├── bot.go                  # Init, slash commands, role auth
│   ├── notify.go               # Embed notifications + motivational quotes
│   ├── commands.go             # Slash command handlers
│   └── checker.go              # /check coin:SOL handler
├── journal/
│   └── logger.go               # positions.json + journal.json
├── logger/
│   └── logger.go               # Color slog handler
├── positions.json              # Open positions (recovery on restart)
└── journal.json                # Closed trades history
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
AI_PROVIDER=grok

# Optional: override default model for selected provider
# AI_MODEL=grok-4.20-0309-reasoning

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
```

---

## 🤖 AI Providers

Mambo supports multiple AI providers switchable via `AI_PROVIDER` in `.env`. Only the API key for the selected provider is required.

| Provider | Value | Default Model | API Format |
|---|---|---|---|
| **Grok (xAI)** | `grok` | `grok-4.20-0309-reasoning` | OpenAI-compatible |
| **OpenAI** | `openai` | `gpt-4o` | OpenAI-compatible |
| **DeepSeek** | `deepseek` | `deepseek-reasoner` | OpenAI-compatible |
| **Anthropic** | `anthropic` | `claude-opus-4-5` | Custom (handled internally) |

To switch provider: change `AI_PROVIDER` and set the matching API key. No code changes needed.

---

## 📰 News Research Agent

Before AI trade scoring, Mambo runs a **deep news sentiment scan** on the selected pair. The `news_research_agent.md` prompt instructs the AI to:

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
| RSI 14 + divergence | techan |
| MACD (12,26,9) + divergence | techan |
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
   Init AI client (provider from AI_PROVIDER env var)
   Register Discord slash commands
   Recover positions.json → spawn monitor goroutines

2. FETCH BALANCE → recalculate dynamic limits

3. FETCH OHLCV (200 candles, 4H) per pair

4. TECHNICAL ANALYSIS
   EMA 9/21/50/200, RSI 14, MACD, BB, ATR, OBV (techan)
   Support/Resistance swing point detection (custom)

5. PRE-FILTER (hard rules, no AI)
   ❌ EMA spread < 0.2%       → skip (sideways)
   ❌ Price < EMA200           → skip (bear market)
   ❌ RSI > 70 or < 30         → skip pair
   ❌ Volume < average         → skip pair
   ❌ 2 consecutive losses     → skip all
   ❌ Daily loss ≥ 15%         → skip all
   ❌ Capital at risk ≥ 60%    → skip all

6. NEWS RESEARCH (AI-powered deep scan)
   CryptoPanic, CoinDesk, The Block, Decrypt, CoinTelegraph, social
   → PASS / WARN / SKIP gate decision

7. ENRICH CONTEXT (free APIs)
   Fear & Greed, Funding rate, OI, Long/Short ratio

8. AI SCORING (configured provider)
   Input: all TA + S/R + news gate + market context + portfolio state
   Output: EXECUTE/ABORT + size + leverage + strategy

9. VALIDATE + CLAMP (Go code — AI cannot bypass)
   confidence < 55% → reject
   R:R < 2.0 → reject
   size: 5–20%, leverage: 1–10x

10. ORDER EXECUTION
   Limit order, cross margin, 10min auto-cancel

11. MONITOR (two tickers)
    priceTicker (MonitorPriceSec) → TP/SL/hard rules
    aiTicker    (MonitorAISec)    → AI position analysis
```

---

## 👁️ Position Monitor

```
priceTicker (fast — default 10s):
  TP/SL hit, drawdown ≥ 40%, hold > 240min, smart loss cut

aiTicker (slow — default 20min, configurable):
  Fetch fresh TA + S/R → send to AI → hold/close/move_sl/move_tp
  AI is S/R aware:
    Near resistance + PnL > 3% → consider closing
    Bouncing off support → hold, tighten SL
```

---

## 🤖 Discord Bot

### Role-Based Access
Only Discord members with `DISCORD_AUTHORIZED_ROLE_ID` can interact.

### Slash Commands
```
/check  coin:SOL           → TA + AI analysis with S/R levels (read-only)
/execute coin:SOL          → full pipeline: prefilter → AI → execute if approved
/execute coin:SOL bypass:true → skip prefilter + AI, execute immediately with defaults
/scan                      → scan random pairs for trade setup (auto-retry 3×, 3min delay)
/status                    → open positions + live PnL
/journal                   → today's trades
/journal week              → last 7 days
/pnl                       → total PnL + win rate
/mode  auto|manual         → toggle mode
/capital                   → balance + limits + budget
/pairs                     → active pairs
```

| Mode | Direction | Size | Leverage | Entry |
|---|---|---|---|---|
| `bypass:false` | AI decides | AI decides (5–20%) | AI decides (1–10x) | Current price (limit) |
| `bypass:true` | LONG if > EMA200, else SHORT | 10% balance | 5x | Current price (limit) |

---

## 🛡️ Safety Layers

```
Layer 1  Trend + S/R Gate (Go)         EMA spread < 0.2% → skip
Layer 2  Pre-filter Rules (Go)          RSI, EMA, volume, budget
Layer 3  News Sentiment Gate (AI)       SKIP on exploits, hacks, SEC; WARN on unlocks, whales
Layer 4  AI Trade Scoring               confidence ≥ 55%, R:R ≥ 2.0, S/R aware
Layer 5  Output Clamp (Go)             size 5–20%, leverage 1–10x
Layer 6  Order Safety (Go)             limit only, 10min auto-cancel
Layer 7  Position Hard Rules (Go)      drawdown, max hold, smart loss cut
Layer 8  AI Position Management        hold/close/move SL/TP, S/R aware
Layer 9  Discord Role Auth (Go)        only authorized role can interact
```

---

## 📦 Tech Stack

| Component | Technology |
|---|---|
| Language | Go |
| Exchange SDK | `github.com/sonirico/go-hyperliquid` |
| AI Providers | Grok / OpenAI / DeepSeek / Anthropic (switchable) |
| TA Library | `github.com/sdcoffey/techan` + custom S/R |
| Discord | `github.com/bwmarrin/discordgo` |
| Env loading | `github.com/joho/godotenv` |
| Sentiment | alternative.me + Binance Public API (free) |
| News Research | AI-powered multi-source scan (CryptoPanic, CoinDesk, The Block, etc.) |

---

## 🚀 Getting Started

```bash
git clone https://github.com/yourname/mambo
cd mambo
go mod tidy
cp .env.example .env
# Fill in: HL keys, AI_PROVIDER + matching API key, Discord config
# Keep HL_TESTNET=true until everything works
go run main.go
```

---

## 📋 Phase Roadmap

```
Phase A  ✅  Foundation
Phase B  ✅  Hyperliquid SDK + market fetcher
Phase C  ✅  TA (techan) + S/R detection
Phase D  ✅  Pre-filter + AI scorer (multi-provider)
Phase E  ✅  Journal
Phase F  ✅  Position monitor (two-ticker)
Phase G  ✅  Discord bot (slash commands, embeds, role auth)
Phase H  ✅  Main scan loop (full wiring)

Extras  ✅  News research agent (sentiment scan + gate decision)
         ✅  Expanded pairs universe (174+ coins in common_pairs.json)
         ✅  Analysis log (per-cycle TA dump to analysis_log.json)

Phase 2 (later):
  → Top Gainers/Losers 24h (Asterdex API)
  → Chart generation (go-chart PNG to Discord)
```

---

> *"I do not guess. I do not hope. I see my setup, I execute, I accept the outcome. Greed is my only enemy."*  
> — Mambo AI Trade