package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"mambo/config"
	"mambo/exchange"
	"mambo/filter"
	"mambo/journal"
	"mambo/market"
	"mambo/ta"
)

// handleStatus shows all open positions with live PnL.
func (b *Bot) handleStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	positions, err := b.exClient.FetchPositions(context.Background())
	if err != nil {
		slog.Error("discord: fetch positions failed", "err", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       "❌ Error",
					Description: "Failed to fetch positions from exchange.",
					Color:       ColorRed,
				}},
			},
		})
		return
	}

	if len(positions) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       "📊 Positions",
					Description: "No open positions.",
					Color:       ColorBlue,
				}},
			},
		})
		return
	}

	fields := make([]*discordgo.MessageEmbedField, 0, len(positions))
	for _, pos := range positions {
		sideEmoji := "🟢"
		if pos.Side == "short" {
			sideEmoji = "🔴"
		}

		pnlLabel := fmt.Sprintf("$%.2f", pos.UnrealizedPnL)
		if pos.UnrealizedPnL > 0 {
			pnlLabel = "+" + pnlLabel
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name: fmt.Sprintf("%s %s %s", sideEmoji, pos.Pair, strings.ToUpper(string(pos.Side))),
			Value: fmt.Sprintf(
				"Entry: $%.4f | Size: $%.2f | Lev: %dx\nPnL: %s | Liq: $%.2f",
				pos.EntryPrice, pos.SizeUSD, pos.Leverage,
				pnlLabel, pos.LiquidationPx,
			),
			Inline: false,
		})
	}

	embed := &discordgo.MessageEmbed{
		Title:     fmt.Sprintf("📊 Open Positions (%d)", len(positions)),
		Color:     ColorBlue,
		Fields:    fields,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

// handleJournal shows today's trades or last 7 days based on the "range" option.
func (b *Bot) handleJournal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	rangeVal := "today"
	for _, opt := range data.Options {
		if opt.Name == "range" {
			rangeVal = opt.StringValue()
		}
	}

	var trades []journal.ClosedTrade
	var err error

	if rangeVal == "week" {
		trades, err = b.jl.GetWeekTrades()
	} else {
		trades, err = b.jl.GetTodayTrades()
	}

	if err != nil {
		slog.Error("discord: fetch journal failed", "err", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "❌ Error", Description: "Failed to fetch journal.",
					Color: ColorRed,
				}},
			},
		})
		return
	}

	if len(trades) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("📖 Journal (%s)", rangeVal),
					Description: "No trades found.",
					Color:       ColorBlue,
				}},
			},
		})
		return
	}

	totalPnL := 0.0
	wins := 0
	losses := 0

	fields := make([]*discordgo.MessageEmbedField, 0, len(trades)+1)
	for _, t := range trades {
		totalPnL += t.PnLUSD
		if t.Result == "WIN" {
			wins++
		} else {
			losses++
		}

		emoji := "✅"
		if t.Result == "LOSS" {
			emoji = "🔴"
		}

		pnlLabel := fmt.Sprintf("$%.2f", t.PnLUSD)
		if t.PnLUSD > 0 {
			pnlLabel = "+" + pnlLabel
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name: fmt.Sprintf("%s %s — %s (%s)", emoji, t.Pair, t.Result, t.CloseReason),
			Value: fmt.Sprintf(
				"Entry: $%.4f | Exit: $%.4f | PnL: %s\nStrategy: %s",
				t.EntryPrice, t.ExitPrice, pnlLabel, t.Strategy,
			),
			Inline: false,
		})
	}

	winRate := 0.0
	if wins+losses > 0 {
		winRate = float64(wins) / float64(wins+losses) * 100
	}

	pnlLabel := fmt.Sprintf("$%.2f", totalPnL)
	if totalPnL > 0 {
		pnlLabel = "+" + pnlLabel
	}

	summary := fmt.Sprintf("Total PnL: %s | Wins: %d | Losses: %d | Win Rate: %.0f%%",
		pnlLabel, wins, losses, winRate)

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("📖 Journal — %s", rangeVal),
		Description: summary,
		Color:       ColorBlue,
		Fields:      fields,
		Timestamp:   time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

// handlePnl shows total PnL and win rate from journal.
func (b *Bot) handlePnl(s *discordgo.Session, i *discordgo.InteractionCreate) {
	trades, err := b.jl.GetWeekTrades()
	if err != nil {
		slog.Error("discord: fetch trades for PnL failed", "err", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "❌ Error", Description: "Failed to fetch PnL.",
					Color: ColorRed,
				}},
			},
		})
		return
	}

	if len(trades) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "💰 PnL (7 days)", Description: "No trades yet.",
					Color: ColorBlue,
				}},
			},
		})
		return
	}

	totalPnL := 0.0
	wins := 0
	losses := 0
	bestWin := 0.0
	worstLoss := 0.0

	for _, t := range trades {
		totalPnL += t.PnLUSD
		if t.Result == "WIN" {
			wins++
			if t.PnLUSD > bestWin {
				bestWin = t.PnLUSD
			}
		} else {
			losses++
			if t.PnLUSD < worstLoss {
				worstLoss = t.PnLUSD
			}
		}
	}

	winRate := 0.0
	if wins+losses > 0 {
		winRate = float64(wins) / float64(wins+losses) * 100
	}

	pnlLabel := fmt.Sprintf("$%.2f", totalPnL)
	if totalPnL > 0 {
		pnlLabel = "+" + pnlLabel
	}

	embed := &discordgo.MessageEmbed{
		Title: "💰 PnL Summary (7 days)",
		Color: ColorBlue,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Total PnL", Value: pnlLabel, Inline: true},
			{Name: "Win Rate", Value: fmt.Sprintf("%.0f%%", winRate), Inline: true},
			{Name: "Total Trades", Value: fmt.Sprintf("%d", wins+losses), Inline: true},
			{Name: "Wins", Value: fmt.Sprintf("%d", wins), Inline: true},
			{Name: "Losses", Value: fmt.Sprintf("%d", losses), Inline: true},
			{Name: "Best Win", Value: fmt.Sprintf("$%.2f", bestWin), Inline: true},
			{Name: "Worst Loss", Value: fmt.Sprintf("$%.2f", worstLoss), Inline: true},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

// handleMode toggles between auto and manual trading modes.
func (b *Bot) handleMode(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	mode := data.Options[0].StringValue()
	b.mode = mode

	emoji := "🟢"
	if mode == "manual" {
		emoji = "🟡"
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("%s Mode: %s", emoji, strings.ToUpper(mode)),
				Description: fmt.Sprintf("Trading mode set to **%s**.", mode),
				Color:       ColorBlue,
			}},
		},
	})

	slog.Info("discord: mode changed", "mode", mode)
}

// handleCapital shows live balance, limits, and remaining budget.
func (b *Bot) handleCapital(s *discordgo.Session, i *discordgo.InteractionCreate) {
	balance, err := b.exClient.FetchBalance(context.Background())
	if err != nil {
		slog.Error("discord: fetch balance failed", "err", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "❌ Error", Description: "Failed to fetch balance.",
					Color: ColorRed,
				}},
			},
		})
		return
	}

	lossUSD, winUSD, consecLosses, err := b.jl.GetDailyPnL()
	if err != nil {
		slog.Warn("discord: get daily PnL failed", "err", err)
	}

	positions, err := b.exClient.FetchPositions(context.Background())
	totalAtRisk := 0.0
	if err == nil {
		for _, p := range positions {
			totalAtRisk += p.SizeUSD
		}
	}

	minSize := balance * 0.05
	maxSize := balance * 0.20
	maxAtRisk := balance * 0.60
	remaining := maxAtRisk - totalAtRisk
	if remaining < 0 {
		remaining = 0
	}

	embed := &discordgo.MessageEmbed{
		Title: "💰 Capital Overview",
		Color: ColorBlue,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Balance", Value: fmt.Sprintf("$%.2f", balance), Inline: true},
			{Name: "Min Size", Value: fmt.Sprintf("$%.2f", minSize), Inline: true},
			{Name: "Max Size", Value: fmt.Sprintf("$%.2f", maxSize), Inline: true},
			{Name: "At Risk", Value: fmt.Sprintf("$%.2f / $%.2f", totalAtRisk, maxAtRisk), Inline: true},
			{Name: "Remaining Budget", Value: fmt.Sprintf("$%.2f", remaining), Inline: true},
			{Name: "Daily Loss", Value: fmt.Sprintf("$%.2f / $%.2f", lossUSD, balance*0.15), Inline: true},
			{Name: "Daily Win", Value: fmt.Sprintf("$%.2f / $%.2f", winUSD, balance*0.30), Inline: true},
			{Name: "Consec Losses", Value: fmt.Sprintf("%d / 2", consecLosses), Inline: true},
			{Name: "Mode", Value: strings.ToUpper(b.mode), Inline: true},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

// handleScan runs the full scan loop: fetches balance, picks random pairs, pre-filters, and scores with AI.
// Responds immediately with "searching..." then edits once a result is found or all pairs exhausted.
// If no trade is found, sleeps 3 minutes and retries up to 3 total scan cycles.
func (b *Bot) handleScan(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	go func() {
		const (
			maxScanCycles    = 3
			maxPairsPerCycle = 15
			maxAIScores      = 10 // max AI calls per cycle to save tokens
			retryDelay       = 3 * time.Minute
		)

		ctx := context.Background()
		totalScanned := 0
		globalSeen := make(map[string]bool)

		for cycle := 0; cycle < maxScanCycles; cycle++ {
			if cycle > 0 {
				slog.Info("scan: retrying after delay",
					"cycle", cycle+1,
					"delay", retryDelay,
				)
				time.Sleep(retryDelay)
			}

			balance, err := b.exClient.FetchBalance(ctx)
			if err != nil {
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds: &[]*discordgo.MessageEmbed{{
						Title:       "❌ Scan Error",
						Description: fmt.Sprintf("Failed to fetch balance: %s", err.Error()),
						Color:       ColorRed,
					}},
				})
				return
			}

			lossUSD, winUSD, consecLosses, err := b.jl.GetDailyPnL()
			if err != nil {
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds: &[]*discordgo.MessageEmbed{{
						Title:       "❌ Scan Error",
						Description: fmt.Sprintf("Failed to fetch PnL: %s", err.Error()),
						Color:       ColorRed,
					}},
				})
				return
			}

			state := filter.BotState{
				Balance:           balance,
				DailyLossUSD:      lossUSD,
				DailyWinUSD:       winUSD,
				ConsecutiveLosses: consecLosses,
			}

			aiScored := 0

			for attempt := 0; attempt < maxPairsPerCycle && aiScored < maxAIScores; attempt++ {
				pair, err := market.GetRandomPair()
				if err != nil {
					slog.Error("scan: random pair failed", "err", err)
					break
				}
				if globalSeen[pair] {
					continue
				}
				globalSeen[pair] = true
				totalScanned++

				taResult, mc, err := b.refreshPairData(ctx, pair)
				if err != nil {
					continue
				}

				filterResult := filter.ApplyPreFilter(pair, taResult, state)
				if !filterResult.Pass {
					if filterResult.SkipAll {
						b.NotifyDailyLimit(filterResult.Reason)
						s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
							Embeds: &[]*discordgo.MessageEmbed{{
								Title:       "🛑 Scan Halted",
								Description: filterResult.Reason,
								Color:       ColorRed,
							}},
						})
						return
					}
					continue
				}

				if !b.cfg.EnableAI {
					orderResult, err := b.exClient.PlaceLimitOrder(
						ctx,
						pair,
						exchange.OrderSideLong,
						balance*0.10,
						taResult.CurrentPrice,
						config.MaxLeverageX,
					)
					if err != nil {
						s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
							Embeds: &[]*discordgo.MessageEmbed{{
								Title:       fmt.Sprintf("❌ Order Failed — %s", pair),
								Description: err.Error(),
								Color:       ColorRed,
							}},
						})
						return
					}

					b.SendEmbed(&discordgo.MessageEmbed{
						Title:       fmt.Sprintf("🚀 DUMMY ORDER — %s", pair),
						Description: "AI disabled. Executed dummy LONG with 10% balance at 10x.",
						Color:       ColorGreen,
						Fields: []*discordgo.MessageEmbedField{
							{Name: "Entry", Value: fmt.Sprintf("$%.4f", orderResult.Price), Inline: true},
							{Name: "Size", Value: fmt.Sprintf("$%.2f", orderResult.SizeUSD), Inline: true},
							{Name: "Leverage", Value: fmt.Sprintf("%dx", orderResult.Leverage), Inline: true},
							{Name: "Order ID", Value: fmt.Sprintf("%d", orderResult.OrderID), Inline: true},
						},
						Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
						Timestamp: time.Now().Format(time.RFC3339),
					})

					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Embeds: &[]*discordgo.MessageEmbed{{
							Title:       fmt.Sprintf("🚀 DUMMY ORDER — %s", pair),
							Description: fmt.Sprintf("AI disabled. Executed dummy LONG at $%.4f.", orderResult.Price),
							Color:       ColorGreen,
							Fields: []*discordgo.MessageEmbedField{
								{Name: "Size", Value: fmt.Sprintf("$%.2f", orderResult.SizeUSD), Inline: true},
								{Name: "Leverage", Value: fmt.Sprintf("%dx", orderResult.Leverage), Inline: true},
								{Name: "Order ID", Value: fmt.Sprintf("%d", orderResult.OrderID), Inline: true},
							},
							Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
							Timestamp: time.Now().Format(time.RFC3339),
						}},
					})
					return
				}

				aiScored++
				score, err := b.scorer.Score(ctx, pair, taResult, mc, state)
				if err != nil {
					b.jl.AppendAnalysisLog(journal.AnalysisLogEntry{
						Timestamp: journal.Now(),
						Pair:      pair,
						TA:        taResult,
						Market:    mc,
						AIError:   err.Error(),
					})
					continue
				}

				aiLog := &journal.AIDecisionLog{
					Action:          score.Action,
					Leverage:        score.Leverage,
					PositionSizeUSD: score.PositionSizeUSD,
					StopLoss:        score.StopLoss,
					TakeProfit:      score.TakeProfit,
					Confidence:      score.Confidence,
					Strategy:        score.Strategy,
					ConfluenceCount: score.ConfluenceCount,
					RRRatio:         score.RRRatio,
					Reasoning:       score.Reasoning,
				}

				b.jl.AppendAnalysisLog(journal.AnalysisLogEntry{
					Timestamp:  journal.Now(),
					Pair:       pair,
					TA:         taResult,
					Market:     mc,
					AIDecision: aiLog,
				})

				switch score.Action {
				case "open_long", "open_short":
					b.NotifyTradeExecuted(pair, score.Action, taResult.CurrentPrice, score.PositionSizeUSD, score.Leverage, score.Confidence, score.Strategy, score.Reasoning)
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Embeds: &[]*discordgo.MessageEmbed{{
							Title:       fmt.Sprintf("🚀 Trade Found — %s", pair),
							Description: fmt.Sprintf("**%s** — %s", strings.ToUpper(score.Action), score.Reasoning),
							Color:       ColorGreen,
							Fields: []*discordgo.MessageEmbedField{
								{Name: "Entry", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
								{Name: "SL", Value: fmt.Sprintf("$%.4f", score.StopLoss), Inline: true},
								{Name: "TP", Value: fmt.Sprintf("$%.4f", score.TakeProfit), Inline: true},
								{Name: "Size", Value: fmt.Sprintf("$%.2f", score.PositionSizeUSD), Inline: true},
								{Name: "Leverage", Value: fmt.Sprintf("%dx", score.Leverage), Inline: true},
								{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", score.Confidence), Inline: true},
								{Name: "Strategy", Value: score.Strategy, Inline: true},
								{Name: "R:R", Value: fmt.Sprintf("%.2f", score.RRRatio), Inline: true},
							},
							Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
							Timestamp: time.Now().Format(time.RFC3339),
						}},
					})
					return

				default:
					continue
				}
			}
		}

		// all cycles exhausted — no trade found
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       "🔍 No Trade Found",
				Description: fmt.Sprintf("Scanned %d pairs across %d cycles — no qualifying setup found.", totalScanned, maxScanCycles),
				Color:       ColorYellow,
				Footer:      &discordgo.MessageEmbedFooter{Text: randomQuote()},
				Timestamp:   time.Now().Format(time.RFC3339),
			}},
		})
	}()
}

// refreshPairData fetches candles, calculates TA, and fetches market context for a pair.
func (b *Bot) refreshPairData(ctx context.Context, pair string) (ta.TAResult, market.MarketContext, error) {
	candles, err := b.fetcher.FetchOHLCV(ctx, pair, "4h", 200)
	if err != nil {
		return ta.TAResult{}, market.MarketContext{}, fmt.Errorf("fetch OHLCV: %w", err)
	}

	taResult, err := ta.Calculate(candles)
	if err != nil {
		return ta.TAResult{}, market.MarketContext{}, fmt.Errorf("calculate TA: %w", err)
	}

	mc, err := b.fetcher.FetchMarketContext(ctx, pair)
	if err != nil {
		slog.Warn("market context unavailable", "pair", pair, "err", err)
	}

	return taResult, mc, nil
}
func (b *Bot) handlePairs(s *discordgo.Session, i *discordgo.InteractionCreate) {
	pairs, err := market.LoadPairs()
	if err != nil {
		slog.Error("discord: load pairs failed", "err", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "❌ Error", Description: "Failed to load trading pairs.",
					Color: ColorRed,
				}},
			},
		})
		return
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🪙 Trading Pairs (%d)", len(pairs)),
		Description: strings.Join(pairs, ", "),
		Color:       ColorBlue,
		Timestamp:   time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}
