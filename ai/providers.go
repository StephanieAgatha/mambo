package ai

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openaicompat"

	"mambo/config"
)

// NewProvider creates a Fantasy provider from Mambo config.
// Grok/OpenAI/DeepSeek use openaicompat (OpenAI-compatible API with custom base URL).
// Anthropic uses the native Anthropic provider.
func NewProvider(ctx context.Context, cfg *config.Config) (fantasy.Provider, error) {
	switch cfg.AIProvider {
	case config.ProviderAnthropic:
		return anthropic.New(
			anthropic.WithBaseURL(cfg.AIBaseURL),
			anthropic.WithAPIKey(cfg.AIAPIKey),
		)
	default:
		// Grok, OpenAI, DeepSeek — all OpenAI-compatible
		return openaicompat.New(
			openaicompat.WithBaseURL(cfg.AIBaseURL),
			openaicompat.WithAPIKey(cfg.AIAPIKey),
			openaicompat.WithName(cfg.AIProvider),
		)
	}
}

// NewModel creates a Fantasy LanguageModel from Mambo config.
func NewModel(ctx context.Context, cfg *config.Config) (fantasy.LanguageModel, error) {
	provider, err := NewProvider(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("ai: create provider %s: %w", cfg.AIProvider, err)
	}

	model, err := provider.LanguageModel(ctx, cfg.AIModel)
	if err != nil {
		return nil, fmt.Errorf("ai: get model %s from provider %s: %w", cfg.AIModel, cfg.AIProvider, err)
	}

	slog.Info("ai model initialized via Fantasy",
		"provider", model.Provider(),
		"model", model.Model(),
	)

	return model, nil
}