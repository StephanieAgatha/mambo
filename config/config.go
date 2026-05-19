package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Guardrail constants — never hardcode dollar amounts, always use percentages
const (
	// Position sizing
	MinSizePct          = 0.05 // 5% of balance per trade minimum
	MaxSizePct          = 0.20 // 20% of balance per trade maximum
	MaxCapitalAtRiskPct = 0.60 // 60% of balance max deployed across all positions

	// Leverage
	MinLeverageX = 1  // 1x cross minimum
	MaxLeverageX = 10 // 10x cross maximum

	// Daily limits
	DailyLossLimitPct = 0.15 // 15% of balance → pause all trading
	DailyWinLimitPct  = 0.30 // 30% of balance → lock profits, pause

	// Trade rules
	MaxConsecutiveLosses = 2   // consecutive losses → 24h mandatory break
	MinConfidence        = 50  // AI confidence minimum to proceed
	MinRiskReward        = 2.0 // R:R minimum — hard rejected below this
	OrderTimeoutMin      = 10  // minutes before unfilled limit order is cancelled

	// EMA trend gate
	EMASpreadThreshold = 0.002 // 0.2% — below this = sideways market → skip pair

	// Hyperliquid API URLs
	HyperliquidMainnetURL = "https://api.hyperliquid.xyz"
	HyperliquidTestnetURL = "https://api.hyperliquid-testnet.xyz"

	// Free market context APIs
	FearGreedURL  = "https://api.alternative.me/fng/?limit=1"
	BinanceFutURL = "https://fapi.binance.com"
)

// Supported AI providers
const (
	ProviderGrok      = "grok"
	ProviderOpenAI    = "openai"
	ProviderDeepSeek  = "deepseek"
	ProviderAnthropic = "anthropic"
)

// providerBaseURLs maps provider name → OpenAI-compatible base URL.
// Anthropic is handled separately (different API format).
var providerBaseURLs = map[string]string{
	ProviderGrok:     "https://api.x.ai/v1",
	ProviderOpenAI:   "https://ai.sumopod.com/v1",
	ProviderDeepSeek: "https://hyper.charm.land/v1",
	// Anthropic uses a different API format — handled in ai/client.go
	ProviderAnthropic: "https://api.anthropic.com",
}

// defaultModels maps provider name → recommended default model.
var defaultModels = map[string]string{
	ProviderGrok:      "grok-4.20-0309-reasoning",
	ProviderOpenAI:    "gpt-5.4",
	ProviderDeepSeek:  "deepseek-v4-pro",
	ProviderAnthropic: "claude-opus-4-5",
}

// Config holds all runtime configuration loaded from .env
type Config struct {
	// Hyperliquid
	AgentPrivateKey   string
	AccountAddress    string
	Testnet           bool
	HyperliquidAPIURL string // derived from Testnet flag

	// AI Provider (configurable)
	AIProvider string // "grok" | "openai" | "deepseek" | "anthropic"
	AIModel    string // model name — defaults to provider's recommended model
	AIBaseURL  string // derived from AIProvider
	AIAPIKey   string // the active provider's API key

	// Individual API keys (only the active provider's key is required)
	XAIAPIKey       string // Grok (xAI)
	OpenAIAPIKey    string // OpenAI
	DeepSeekAPIKey  string // DeepSeek
	AnthropicAPIKey string // Anthropic

	// Discord
	DiscordBotToken         string
	DiscordGuildID          string
	DiscordChannelID        string
	DiscordAuthorizedRoleID string

	// Monitor intervals
	MonitorPriceSec int    // price + hard rules check interval (default 10s)
	MonitorAISec    int    // Grok position analysis interval (default 1200s = 20min)
	MonitorAIIntvl  string // timeframe for monitor TA snapshots (default 4h)

	// Feature toggles
	EnableAI bool // enable/disable AI scoring (default: true)

	// MCP (optional — defaults to uvx)
	MCPTACommand string // env: MCP_TA_COMMAND, default "uvx"
}

var PreFilterEnabled bool = true

// Load reads .env then validates all required environment variables.
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil {
		fmt.Println("warn: .env file not found — expecting env vars set externally")
	}

	cfg := &Config{}
	var err error

	// ── Hyperliquid ──────────────────────────────────────────────────────────

	cfg.AgentPrivateKey = os.Getenv("HL_AGENT_PRIVATE_KEY")
	if cfg.AgentPrivateKey == "" {
		return nil, fmt.Errorf("config: HL_AGENT_PRIVATE_KEY is required")
	}

	cfg.AccountAddress = os.Getenv("HL_ACCOUNT_ADDRESS")
	if cfg.AccountAddress == "" {
		return nil, fmt.Errorf("config: HL_ACCOUNT_ADDRESS is required")
	}

	testnetStr := os.Getenv("HL_TESTNET")
	if testnetStr == "" {
		testnetStr = "true" // default to testnet for safety
	}
	isTestnet, err := strconv.ParseBool(testnetStr)
	if err != nil {
		return nil, fmt.Errorf("config: HL_TESTNET must be 'true' or 'false', got %q: %w", testnetStr, err)
	}
	cfg.Testnet = isTestnet

	if cfg.Testnet {
		cfg.HyperliquidAPIURL = HyperliquidTestnetURL
	} else {
		cfg.HyperliquidAPIURL = HyperliquidMainnetURL
	}

	// ── Pre‑filter toggle ──────────────────────────────────────────────────
	preFilterStr := os.Getenv("ENABLE_PREFILTER")
	if preFilterStr == "" {
		PreFilterEnabled = true // default menyala
	} else {
		enabled, err := strconv.ParseBool(preFilterStr)
		if err != nil {
			PreFilterEnabled = true
		} else {
			PreFilterEnabled = enabled
		}
	}

	// ── AI Provider ───────────────────────────────────────────────────────────

	cfg.AIProvider = os.Getenv("AI_PROVIDER")
	if cfg.AIProvider == "" {
		cfg.AIProvider = ProviderGrok // default to Grok
	}

	// validate provider
	baseURL, ok := providerBaseURLs[cfg.AIProvider]
	if !ok {
		return nil, fmt.Errorf("config: AI_PROVIDER %q is not supported. Valid: grok, openai, deepseek, anthropic", cfg.AIProvider)
	}
	cfg.AIBaseURL = baseURL

	// allow override via AI_BASE_URL env (useful for custom proxies / agent routers)
	if override := os.Getenv("AI_BASE_URL"); override != "" {
		cfg.AIBaseURL = override
	}

	// model — use custom if set, otherwise use provider default
	cfg.AIModel = os.Getenv("AI_MODEL")
	if cfg.AIModel == "" {
		cfg.AIModel = defaultModels[cfg.AIProvider]
	}

	// load all provider keys (only the active one is strictly required)
	cfg.XAIAPIKey = os.Getenv("XAI_API_KEY")
	cfg.OpenAIAPIKey = os.Getenv("OPENAI_API_KEY")
	cfg.DeepSeekAPIKey = os.Getenv("DEEPSEEK_API_KEY")
	cfg.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")

	// set the active API key based on selected provider
	switch cfg.AIProvider {
	case ProviderGrok:
		cfg.AIAPIKey = cfg.XAIAPIKey
	case ProviderOpenAI:
		cfg.AIAPIKey = cfg.OpenAIAPIKey
	case ProviderDeepSeek:
		cfg.AIAPIKey = cfg.DeepSeekAPIKey
	case ProviderAnthropic:
		cfg.AIAPIKey = cfg.AnthropicAPIKey
	}

	if cfg.AIAPIKey == "" {
		return nil, fmt.Errorf("config: API key for provider %q is required (check your .env)", cfg.AIProvider)
	}

	// ── Discord ───────────────────────────────────────────────────────────────

	cfg.DiscordBotToken = os.Getenv("DISCORD_BOT_TOKEN")
	if cfg.DiscordBotToken == "" {
		fmt.Println("warn: DISCORD_BOT_TOKEN not set — Discord features disabled")
	}

	cfg.DiscordGuildID = os.Getenv("DISCORD_GUILD_ID")
	if cfg.DiscordGuildID == "" {
		fmt.Println("config: DISCORD_GUILD_ID is required")
	}

	cfg.DiscordChannelID = os.Getenv("DISCORD_CHANNEL_ID")
	if cfg.DiscordChannelID == "" {
		fmt.Println("config: DISCORD_CHANNEL_ID is required")
	}

	cfg.DiscordAuthorizedRoleID = os.Getenv("DISCORD_AUTHORIZED_ROLE_ID")
	if cfg.DiscordAuthorizedRoleID == "" {
		fmt.Println("config: DISCORD_AUTHORIZED_ROLE_ID is required")
	}

	// ── AI toggle ────────────────────────────────────────────────────────────
	enableAIStr := os.Getenv("ENABLE_AI")
	if enableAIStr == "" {
		cfg.EnableAI = true // default enabled
	} else {
		enabled, err := strconv.ParseBool(enableAIStr)
		if err != nil {
			cfg.EnableAI = true
		} else {
			cfg.EnableAI = enabled
		}
	}

	// ── MCP TA command ──────────────────────────────────────────────────────
	cfg.MCPTACommand = os.Getenv("MCP_TA_COMMAND")
	if cfg.MCPTACommand == "" {
		cfg.MCPTACommand = "uvx"
	}

	// ── Monitor intervals ─────────────────────────────────────────────────────

	cfg.MonitorPriceSec, err = parseInt(os.Getenv("MONITOR_PRICE_SEC"), 10)
	if err != nil {
		return nil, fmt.Errorf("config: MONITOR_PRICE_SEC must be an integer: %w", err)
	}
	if cfg.MonitorPriceSec < 5 {
		return nil, fmt.Errorf("config: MONITOR_PRICE_SEC must be ≥ 5 seconds, got %d", cfg.MonitorPriceSec)
	}

	cfg.MonitorAISec, err = parseInt(os.Getenv("MONITOR_AI_SEC"), 1200)
	if err != nil {
		return nil, fmt.Errorf("config: MONITOR_AI_SEC must be an integer: %w", err)
	}
	if cfg.MonitorAISec < 60 {
		return nil, fmt.Errorf("config: MONITOR_AI_SEC must be ≥ 60 seconds, got %d", cfg.MonitorAISec)
	}

	cfg.MonitorAIIntvl = os.Getenv("MONITOR_AI_INTERVAL")
	if cfg.MonitorAIIntvl == "" {
		cfg.MonitorAIIntvl = "4h"
	}

	return cfg, nil
}

// NetworkLabel returns a human-readable network name.
func (c *Config) NetworkLabel() string {
	if c.Testnet {
		return "TESTNET"
	}
	return "MAINNET"
}

// AILabel returns a human-readable AI provider + model string for logging.
func (c *Config) AILabel() string {
	return fmt.Sprintf("%s/%s", c.AIProvider, c.AIModel)
}

// parseInt parses an env string as int, returning defaultVal if empty.
func parseInt(s string, defaultVal int) (int, error) {
	if s == "" {
		return defaultVal, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return v, nil
}
