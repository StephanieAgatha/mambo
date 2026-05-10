package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mambo/config"
)

// Client is a provider-agnostic AI chat client.
// Supports Grok, OpenAI, DeepSeek (OpenAI-compatible) and Anthropic (custom format).
// Scorer and PositionManager both use this — no direct HTTP calls elsewhere.
type Client struct {
	cfg        *config.Config
	httpClient *http.Client
}

// NewClient creates an AI client for the provider configured in .env.
func NewClient(cfg *config.Config, timeout time.Duration) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Chat sends a system + user prompt to the configured AI provider
// and returns the raw text response.
func (c *Client) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	switch c.cfg.AIProvider {
	case config.ProviderAnthropic:
		// Anthropic uses a different API format — not OpenAI-compatible
		return c.chatAnthropic(ctx, systemPrompt, userPrompt)
	default:
		// Grok, OpenAI, DeepSeek are all OpenAI-compatible
		return c.chatOpenAICompatible(ctx, systemPrompt, userPrompt)
	}
}

// chatOpenAICompatible handles Grok, OpenAI, and DeepSeek.
// All three use the same /chat/completions format with Bearer token auth.
func (c *Client) chatOpenAICompatible(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	reqBody := map[string]any{
		"model": c.cfg.AIModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"max_tokens": 4096,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ai/client: marshal request: %w", err)
	}

	url := strings.TrimRight(c.cfg.AIBaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ai/client: build request provider=%s: %w", c.cfg.AIProvider, err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.AIAPIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai/client: http request provider=%s: %w", c.cfg.AIProvider, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ai/client: read response provider=%s: %w", c.cfg.AIProvider, err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai/client: provider=%s status=%d body=%s",
			c.cfg.AIProvider, resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("ai/client: parse response provider=%s: %w", c.cfg.AIProvider, err)
	}

	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("ai/client: no choices returned by provider=%s", c.cfg.AIProvider)
	}

	return parsed.Choices[0].Message.Content, nil
}

// chatAnthropic handles Anthropic's Claude models.
// Anthropic uses a different API format:
//   - Endpoint: POST /v1/messages (not /chat/completions)
//   - Auth: x-api-key header (not Bearer)
//   - Requires: anthropic-version header
//   - System prompt: top-level field (not inside messages array)
func (c *Client) chatAnthropic(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	reqBody := map[string]any{
		"model":      c.cfg.AIModel,
		"max_tokens": 4096,
		"system":     systemPrompt, // top-level, not in messages
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ai/client: anthropic marshal request: %w", err)
	}

	url := strings.TrimRight(c.cfg.AIBaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ai/client: anthropic build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.cfg.AIAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai/client: anthropic http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ai/client: anthropic read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai/client: anthropic status=%d body=%s",
			resp.StatusCode, string(respBody))
	}

	// Anthropic response format differs from OpenAI
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("ai/client: anthropic parse response: %w", err)
	}

	// find the first text block
	for _, block := range parsed.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}

	return "", fmt.Errorf("ai/client: anthropic returned no text content")
}
