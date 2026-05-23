```markdown
# Mambo + Fantasy Integration Plan (v2)

## Overview
This document outlines a phased migration plan to integrate [Charmbracelet Fantasy](https://github.com/charmbracelet/fantasy) into the [Mambo trading bot](https://github.com/StephanieAgatha/mambo), replacing the custom AI client layer with Fantasy's unified API, **structured output**, and **streaming capabilities**.

### Goals
- Simplify AI provider management (Grok, OpenAI, DeepSeek, Anthropic) via a single interface.
- Eliminate custom HTTP client logic and provider-specific code.
- Replace fragile string parsing with type‑safe **structured output** (`object.Generate`).
- Enable **streaming** for real-time trade decision feedback (Discord, logs, terminal).
- Enable future agentic workflows (multi-step reasoning, tool use) without disrupting existing trading logic.
- Maintain full backward compatibility with current trading strategies and risk controls.

---

## Current Architecture (Mambo's `ai/` Package)

ai/
├── client.go          # Multi-provider HTTP client with retry/fallback
├── scorer.go           # Core AI decision-making + prompt templates
├── position_manager.go # Position sizing/risk management AI calls
├── AGENT.md            # System prompt template
├── prompts/            # Individual prompt templates
└── ...
**Current Flow:**
1. Application calls `ai.ScoringAgent.Score(pair, taResult, ...)`
2. Scorer builds prompt from templates, calls `client.Chat(ctx, systemPrompt, userPrompt)`
3. `client.Chat` routes to provider-specific implementation (OpenAI, Anthropic, etc.) with custom HTTP logic
4. Raw text response is parsed with `parseDecision()` (regex + string splitting)
5. Result validated and clamped by Go guardrails

**Pain Points:**
- Provider switching requires code changes (switch-case on provider name).
- Custom HTTP logic for each provider adds maintenance burden.
- **String parsing is fragile, breaking on slight output variations.**
- No ability to chain multiple AI calls into a single logical operation.
- **No streaming support** → users wait for the entire response before seeing anything.

---

## Proposed Architecture with Fantasy

ai/
├── agents/
│   └── scorer_agent.go      # Agent wrapping existing scoring logic
├── tools/
│   └── score_trade.go        # Tool definition for trade scoring
├── prompts/                 # Existing prompt templates (reused)
│   ├── AGENT.md
│   └── verify_trade.md
├── types.go                 # Shared structs (ScoreResult, etc.)
├── scorer.go                # (refactored) Uses Fantasy's structured output + streaming
└── ...
**New Flow:**
1. Fantasy provider/model configured once at startup (supports all providers, custom base URLs).
2. Agent equipped with a `score_trade` tool that calls the existing scoring logic.
3. Agent can be invoked with a simple prompt or as a direct `object.Generate` call.
4. **Structured Output:** AI returns a fully populated `ScoreResult` struct—no parsing needed.
5. **Streaming:** The same call can be streamed for real-time updates to Discord/terminal.
6. Fantasy handles provider differences, retries, and error recovery automatically.

---

## Custom Base URL Support

For routing through proxies, OpenRouter alternatives, or self-hosted models:

- **Native providers (OpenAI, Azure, Bedrock):** Use `WithBaseURL("https://your-proxy.com/v1")`
- **Any OpenAI‑compatible endpoint (OpenRouter, Ollama, etc.):** Use the `openaicompat` provider with `WithBaseURL`

Configuration via environment variables keeps it secure and flexible.

---

## Phased Migration Plan

### Phase 1: Foundation – Structured Output & Streaming
**Objective:** Replace `client.Chat()` + `parseDecision()` with Fantasy’s `object.Generate` (structured output) and add streaming support for real-time feedback.

**Tasks:**
1. Add Fantasy dependency to `go.mod`.
2. Create a `providers` package to initialize the desired provider (OpenAI, Anthropic, etc.) with optional custom base URL.
3. Define the `ScoreResult` struct with JSON tags and `description` fields (reuse existing structure).
4. Refactor `scorer.go`’s `Score()` method:
   - Replace `client.Chat` + `parseDecision` with `object.Generate[ScoreResult]`.
   - Keep the same prompt templates and system prompt (`AGENT.md`).
5. **Add a streaming variant** for live output (e.g., `ScoreStream()`):
   - Use `object.Stream[ScoreResult]` to progressively emit confidence, action, etc.
   - Integrate with Discord bot: send partial updates as the AI decides.
6. Remove `parseDecision()` and regex logic.
7. Update tests to cover both structured and streaming paths.

**Deliverables:**
- Type‑safe trade scoring with zero parsing errors.
- Real-time streaming of the AI decision (e.g., “Confidence so far: 67%”).
- All existing unit tests pass.
- Old client code kept behind a feature flag for quick rollback.

---

### Phase 2: Toolification – Expose Scoring as an Agent Tool
**Objective:** Wrap the scoring logic into a Fantasy tool so it can be used in agentic flows.

**Tasks:**
1. Define a `ScoreTradeInput` struct (pair, direction, optional parameters).
2. Create a `scoreTrade` tool using `fantasy.NewAgentTool`:
   - Handler calls the existing (now refactored) `Scorer.Score()`.
   - Returns marshaled `ScoreResult` as tool response.
3. Build a simple `Agent` with a system prompt and this tool.
4. Create a new command (e.g., `/scan`) that triggers the agent:
   - Agent may reason about market conditions, then invoke the tool.
   - Output can be streamed back to the user.
5. Ensure backward compatibility: direct scorer calls (non-agent) still work.

**Deliverables:**
- A functional agent that can autonomously decide to score a trade.
- Streaming agent responses visible in Discord.

---

### Phase 3: Agentic Workflows – Complex Decision Chains
**Objective:** Implement multi-step agentic loops using tools for autonomous portfolio management.

**Examples:**
- **Portfolio Auditor Agent:** Checks all open positions, asks AI to suggest adjustments, scores each suggestion, and outputs a summary (all streamed).
- **Trade Idea Generator:** Given market data, generates 3 trade ideas, scores them, and ranks them by confidence—streaming each stage.

**Tasks:**
1. Build higher-level agents that compose tools (scoring, risk assessment, market data fetchers).
2. Use `object.Stream` for compound responses (list of scored ideas).
3. Add observability (logging each tool call, intermediate thoughts).

**Deliverables:**
- New commands like `/audit` or `/ideas` with real-time streaming.
- Fully autonomous agents that use Fantasy’s agentic loop.

---

## Streaming Details

### Where Streaming Shines
- **Discord bot `/scan`:** Start showing the AI’s reasoning immediately instead of waiting 10 seconds.
- **Terminal UI:** Progressive display of confidence, strategy, and final decision.
- **Portfolio audit:** Stream each position’s assessment as it’s processed.

### Implementation Snippet (Phase 1)

// Existing structured call
result, _ := object.Generate[ScoreResult](ctx, model, prompt)

// New streaming variant
stream, _ := object.Stream[ScoreResult](ctx, model, prompt)
for partial := range stream.PartialObjectStream() {
    // Send partial to Discord/terminal
    sendLiveUpdate(partial.Action, partial.Confidence)
}
finalResult := stream.Object()


---

Risk Mitigation

· Provider Fallbacks: Fantasy’s unified interface makes provider switching trivial; fallback logic can be built using a simple wrapper.
· Prompt Drift: Keep existing templates in markdown files; Fantasy injects them unchanged.
· Rate Limiting & Retries: Fantasy providers support retry configuration; existing rate limiting in Mambo can be layered on top.
· Streaming Partial Data: Ensure UI handles incomplete structs gracefully; show only validated fields.

---

Expected Benefits After Full Migration

Area Current After Fantasy
Code complexity Custom HTTP for 4 providers Single provider interface
Output parsing Fragile regex/string splitting Type-safe struct generation
Real-time feedback None Streaming partial results
Provider switching Code change + rebuild Config change only
Agent capabilities None (linear call) Multi-step reasoning, tool use
Error handling Custom retry logic Built-in retry + partial stream handling
Extensibility Add new provider = new client file Add new provider = one import

---

Timeline & Dependencies

· Week 1: Phase 1 – structured output + streaming.
· Week 2: Phase 2 – toolification and /scan command with streaming.
· Week 3–4: Phase 3 – agentic workflows based on user feedback.

Dependencies: Fantasy v0.25.1+, Go 1.21+, existing Mambo configs.

---

Next Steps

1. Review this plan with the Mambo maintainers.
2. Set up a feature branch for Phase 1.
3. Implement the provider initializer (with custom base URL support) and the first structured + streaming call.

```
