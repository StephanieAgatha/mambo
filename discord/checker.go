package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"mambo/filter"
	"mambo/ta"
)

// handleCheck performs TA + AI analysis for a requested coin.
// Deferred response so the user sees "thinking..." while AI processes.
func (b *Bot) handleCheck(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	coin := strings.ToUpper(data.Options[0].StringValue())
	interval := "4h"
	for _, opt := range data.Options {
		if opt.Name == "interval" {
			interval = opt.StringValue()
		}
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	ctx := context.Background()

	candles, err := b.fetcher.FetchOHLCV(ctx, coin, interval, 200)
	if err != nil {
		slog.Error("discord: /check fetch OHLCV failed", "coin", coin, "err", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("❌ %s — Data Error", coin),
				Description: fmt.Sprintf("Failed to fetch OHLCV data: %s", err.Error()),
				Color:       ColorRed,
			}},
		})
		return
	}

	taResult, err := ta.Calculate(candles)
	if err != nil {
		slog.Error("discord: /check TA failed", "coin", coin, "err", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("❌ %s — TA Error", coin),
				Description: fmt.Sprintf("Failed to calculate indicators: %s", err.Error()),
				Color:       ColorRed,
			}},
		})
		return
	}

	mc, err := b.fetcher.FetchMarketContext(ctx, coin)
	if err != nil {
		slog.Warn("discord: /check market context unavailable", "coin", coin, "err", err)
	}

	balance, err := b.exClient.FetchBalance(ctx)
	if err != nil {
		slog.Warn("discord: /check fetch balance failed", "err", err)
	}

	lossUSD, winUSD, consecLosses, err := b.exClient.FetchDailyPnL(ctx)
	if err != nil {
		slog.Warn("discord: /check daily PnL failed", "err", err)
	}

	state := filter.BotState{
		Balance:           balance,
		DailyLossUSD:      lossUSD,
		DailyWinUSD:       winUSD,
		ConsecutiveLosses: consecLosses,
		TotalAtRiskUSD:    0,
	}

	filterResult := filter.ApplyPreFilter(coin, taResult, state)
	if !filterResult.Pass {
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("⏭️ %s — Skipped", coin),
				Description: filterResult.Reason,
				Color:       ColorYellow,
				Fields: []*discordgo.MessageEmbedField{
					{Name: "Price", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
					{Name: "RSI", Value: fmt.Sprintf("%.2f (%s)", taResult.RSI, taResult.RSIZone), Inline: true},
					{Name: "EMA Spread", Value: fmt.Sprintf("%.3f%%", taResult.EMASpread), Inline: true},
					{Name: "EMA200", Value: fmt.Sprintf("$%.4f", taResult.EMA200), Inline: true},
					{Name: "Ribbon", Value: taResult.RibbonStatus, Inline: true},
					{Name: "Support", Value: fmt.Sprintf("$%.4f (%s)", taResult.NearestSupport, taResult.SupportStrength), Inline: true},
					{Name: "Resistance", Value: fmt.Sprintf("$%.4f (%s)", taResult.NearestResistance, taResult.ResistanceStrength), Inline: true},
				},
				Footer:    &discordgo.MessageEmbedFooter{Text: "All rules checked — no exceptions."},
				Timestamp: time.Now().Format(time.RFC3339),
			}},
		})
		return
	}

	score, err := b.scorer.Score(ctx, coin, taResult, mc, state)
	if err != nil {
		slog.Error("discord: /check AI scoring failed", "coin", coin, "err", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("❌ %s — AI Error", coin),
				Description: fmt.Sprintf("AI scoring failed: %s", err.Error()),
				Color:       ColorRed,
			}},
		})
		return
	}

	slog.Info("discord: /check complete",
		"coin", coin,
		"action", score.Action,
		"confidence", score.Confidence,
	)

	isTradeAction := score.Action == "open_long" || score.Action == "open_short"

	var color int
	var title string
	if isTradeAction {
		color = ColorGreen
		title = fmt.Sprintf("🚀 %s — %s EXECUTE", coin, strings.ToUpper(strings.TrimPrefix(score.Action, "open_")))
	} else if score.Action == "wait" || score.Action == "hold" {
		color = ColorYellow
		title = fmt.Sprintf("⏸️ %s — %s", coin, strings.ToUpper(score.Action))
	} else {
		color = ColorYellow
		title = fmt.Sprintf("⏸️ %s — %s", coin, score.Action)
	}

	actionLabel := "ABORT"
	if isTradeAction {
		actionLabel = "EXECUTE"
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Price", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
		{Name: "RSI", Value: fmt.Sprintf("%.2f (%s)", taResult.RSI, taResult.RSIZone), Inline: true},
		{Name: "EMA Spread", Value: fmt.Sprintf("%.3f%%", taResult.EMASpread), Inline: true},
		{Name: "EMA9", Value: fmt.Sprintf("$%.4f", taResult.EMA9), Inline: true},
		{Name: "EMA21", Value: fmt.Sprintf("$%.4f", taResult.EMA21), Inline: true},
		{Name: "EMA200", Value: fmt.Sprintf("$%.4f", taResult.EMA200), Inline: true},
		{Name: "MACD", Value: fmt.Sprintf("%.4f (%s)", taResult.MACDValue, taResult.MACDCross), Inline: true},
		{Name: "ATR", Value: fmt.Sprintf("%.4f (%s)", taResult.ATR, taResult.ATRLevel), Inline: true},
		{Name: "Volume", Value: fmt.Sprintf("%.2fx (%v)", taResult.VolumeMultiplier, taResult.VolumeConfirms), Inline: true},
		{Name: "Ribbon", Value: taResult.RibbonStatus, Inline: true},
		{Name: "Daily Trend", Value: taResult.DailyTrend, Inline: true},
		{Name: "BB Position", Value: taResult.BBPosition, Inline: true},
		{Name: "Support", Value: fmt.Sprintf("$%.4f (%s)", taResult.NearestSupport, taResult.SupportStrength), Inline: true},
		{Name: "Resistance", Value: fmt.Sprintf("$%.4f (%s)", taResult.NearestResistance, taResult.ResistanceStrength), Inline: true},
		{Name: "At Support", Value: fmt.Sprintf("%v", taResult.AtSupport), Inline: true},
		{Name: "Near Resistance", Value: fmt.Sprintf("%v", taResult.NearResistance), Inline: true},
	}

	if isTradeAction {
		fields = append(fields,
			&discordgo.MessageEmbedField{Name: "Entry", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
			&discordgo.MessageEmbedField{Name: "SL", Value: fmt.Sprintf("$%.4f", score.StopLoss), Inline: true},
			&discordgo.MessageEmbedField{Name: "TP", Value: fmt.Sprintf("$%.4f", score.TakeProfit), Inline: true},
			&discordgo.MessageEmbedField{Name: "Size", Value: fmt.Sprintf("$%.2f", score.PositionSizeUSD), Inline: true},
			&discordgo.MessageEmbedField{Name: "Leverage", Value: fmt.Sprintf("%dx cross", score.Leverage), Inline: true},
			&discordgo.MessageEmbedField{Name: "R:R", Value: fmt.Sprintf("%.2f", score.RRRatio), Inline: true},
		)
	}

	fields = append(fields,
		&discordgo.MessageEmbedField{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", score.Confidence), Inline: true},
		&discordgo.MessageEmbedField{Name: "Action", Value: fmt.Sprintf("%s", actionLabel), Inline: true},
		&discordgo.MessageEmbedField{Name: "Strategy", Value: score.Strategy, Inline: true},
		&discordgo.MessageEmbedField{Name: "Confluence", Value: fmt.Sprintf("%d signals", score.ConfluenceCount), Inline: true},
		&discordgo.MessageEmbedField{Name: "Decision", Value: score.Reasoning, Inline: false},
	)

	if mc.FearGreedValue > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "Market", Value: fmt.Sprintf(
				"Fear&Greed: %d (%s) | Funding: %.6f (%s) | L/S: %.2f (%s)",
				mc.FearGreedValue, mc.FearGreedZone,
				mc.FundingRate, mc.FundingBias,
				mc.LongShortRatio, mc.LongShortBias,
			), Inline: false,
		})
	}

	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{{
			Title:       title,
			Description: fmt.Sprintf("**Decision**: %s", score.Reasoning),
			Color:       color,
			Fields:      fields,
			Footer:      &discordgo.MessageEmbedFooter{Text: randomQuote()},
			Timestamp:   time.Now().Format(time.RFC3339),
		}},
	})
}
