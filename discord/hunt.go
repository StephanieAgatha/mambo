package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"mambo/exchange"
	"mambo/filter"
	"mambo/journal"
	"mambo/market"
	"mambo/ta"
	aiPkg "mambo/ai"
)

// ── /hunt — Auto-Scan + Execute + Monitor ───────────────────────────────────

// handleHunt scans random pairs, scores with AI, picks the best trade,
// executes via PlaceBracketOrder, and starts monitoring. Full auto — no buttons.
func (b *Bot) handleHunt(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	interval := "4h"
	for _, opt := range data.Options {
		if opt.Name == "interval" {
			interval = opt.StringValue()
		}
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	go func() {
		const (
			maxScanCycles    = 5
			maxPairsPerCycle = 20
			maxAIScores      = 15
			retryDelay       = 2 * time.Minute
		)

		ctx := context.Background()
		totalScanned := 0
		globalSeen := make(map[string]bool)

		updateHunting := func() {
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("🏹 Hunting for trade… (%d pairs scanned)", totalScanned),
					Description: "Scanning market, scoring with AI. Auto-execute on valid setup.",
					Color:       ColorBlue,
				}},
			})
		}

		for cycle := 0; cycle < maxScanCycles; cycle++ {
			if cycle > 0 {
				slog.Info("hunt: next cycle", "cycle", cycle+1, "delay", retryDelay)
				time.Sleep(retryDelay)
			}

			balance, err := b.exClient.FetchBalance(ctx)
			if err != nil {
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds: &[]*discordgo.MessageEmbed{{
						Title:       "❌ Hunt Failed",
						Description: fmt.Sprintf("Balance fetch error: %s", err.Error()),
						Color:       ColorRed,
					}},
				})
				return
			}

			lossUSD, winUSD, consecLosses, err := b.exClient.FetchDailyPnL(ctx)
			if err != nil {
				slog.Warn("hunt: daily PnL fetch failed", "err", err)
				lossUSD, winUSD, consecLosses = 0, 0, 0
			}

			state := filter.BotState{
				Balance:           balance,
				DailyLossUSD:      lossUSD,
				DailyWinUSD:       winUSD,
				ConsecutiveLosses: consecLosses,
			}

			aiScored := 0
			var bestScore aiPkg.ScoreResult
			var bestPair string
			var bestTA ta.TAResult

			for attempt := 0; attempt < maxPairsPerCycle && aiScored < maxAIScores; attempt++ {
				pair, err := market.GetRandomPair()
				if err != nil {
					slog.Error("hunt: random pair failed", "err", err)
					break
				}
				if globalSeen[pair] {
					continue
				}
				globalSeen[pair] = true
				totalScanned++

				if totalScanned%10 == 0 {
					updateHunting()
				}

				bars, err := b.fetcher.FetchOHLCV(ctx, pair, interval, 200)
				if err != nil {
					continue
				}
				taResult, err := ta.Calculate(bars)
				if err != nil {
					continue
				}
				mc, _ := b.fetcher.FetchMarketContext(ctx, pair)

				filterResult := filter.ApplyPreFilter(pair, taResult, state)
				if !filterResult.Pass {
					if filterResult.SkipAll {
						slog.Warn("hunt: daily limit hit — stopping", "reason", filterResult.Reason)
						s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
							Embeds: &[]*discordgo.MessageEmbed{{
								Title:       "🛑 Hunt Halted",
								Description: filterResult.Reason,
								Color:       ColorRed,
							}},
						})
						return
					}
				}

				aiScored++
				score, err := b.scorer.Score(ctx, pair, taResult, mc, state)

				// log ALL scan results to analysis_log.json for audit
				aiLog := &journal.AIDecisionLog{}
				aiErr := ""
				if err != nil {
					aiErr = err.Error()
				} else {
					aiLog = &journal.AIDecisionLog{
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
				}
				b.jl.AppendAnalysisLog(journal.AnalysisLogEntry{
					Timestamp:  journal.Now(),
					Pair:       pair,
					TA:         taResult,
					Market:     mc,
					AIDecision: aiLog,
					AIError:    aiErr,
				})

				if err != nil {
					continue
				}

				isTrade := score.Action == "open_long" || score.Action == "open_short"
				if !isTrade {
					continue
				}

				if score.Confidence > bestScore.Confidence {
					bestScore = score
					bestPair = pair
					bestTA = taResult
				}
			}

			if bestPair != "" {
				slog.Info("hunt: trade found — executing",
					"pair", bestPair,
					"action", bestScore.Action,
					"confidence", bestScore.Confidence,
					"cycle", cycle+1,
				)

				side := exchange.OrderSideLong
				if bestScore.Action == "open_short" {
					side = exchange.OrderSideShort
				}

				sizeUSD := bestScore.PositionSizeUSD
				if sizeUSD <= 0 {
					sizeUSD = balance * 0.05
				}

				stopLoss := bestScore.StopLoss
				takeProfit := bestScore.TakeProfit
				if stopLoss <= 0 || takeProfit <= 0 {
					atrMult := bestTA.ATR * 3
					if side == exchange.OrderSideLong {
						stopLoss = bestTA.CurrentPrice - atrMult
						takeProfit = bestTA.CurrentPrice + atrMult*2
					} else {
						stopLoss = bestTA.CurrentPrice + atrMult
						takeProfit = bestTA.CurrentPrice - atrMult*2
					}
				}

				orderResult, err := b.exClient.PlaceBracketOrder(
					ctx, bestPair, side, sizeUSD,
					bestTA.CurrentPrice,
					takeProfit,
					stopLoss,
					bestScore.Leverage,
				)
				if err != nil {
					s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Embeds: &[]*discordgo.MessageEmbed{{
							Title:       fmt.Sprintf("❌ Hunt Order Failed — %s", bestPair),
							Description: err.Error(),
							Color:       ColorRed,
						}},
					})
					return
				}

				b.StartMonitor(ctx, bestPair, side, bestTA.CurrentPrice, sizeUSD,
					bestScore.Leverage, stopLoss, takeProfit,
					bestScore.Confidence, bestScore.Strategy, bestScore.Reasoning, orderResult.OrderID)

				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds: &[]*discordgo.MessageEmbed{{
						Title: fmt.Sprintf("🏹 HUNT SUCCESS — %s", bestPair),
						Description: fmt.Sprintf("**%s** | Confidence: **%.0f%%** | R:R: **%.2f**\nEntry: **$%.4f** | Size: **$%.2f** | **%dx**\nSL: **$%.4f** | TP: **$%.4f**\n\n%s",
							strings.ToUpper(string(side)), bestScore.Confidence, bestScore.RRRatio,
							bestTA.CurrentPrice, sizeUSD, bestScore.Leverage,
							stopLoss, takeProfit, bestScore.Reasoning),
						Color: ColorGreen,
						Fields: []*discordgo.MessageEmbedField{
							{Name: "Strategy", Value: bestScore.Strategy, Inline: true},
							{Name: "Confluence", Value: fmt.Sprintf("%d/9", bestScore.ConfluenceCount), Inline: true},
							{Name: "Scanned", Value: fmt.Sprintf("%d pairs", totalScanned), Inline: true},
						},
						Footer:    &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Hunt cycle %d · Monitoring active", cycle+1)},
						Timestamp: time.Now().Format(time.RFC3339),
					}},
				})

				b.SendEmbed(&discordgo.MessageEmbed{
					Title:       fmt.Sprintf("🏹 Trade Executed — %s", bestPair),
					Description: fmt.Sprintf("**%s** | Entry: $%.4f | Size: $%.2f | %dx | SL: $%.4f | TP: $%.4f",
						strings.ToUpper(string(side)), bestTA.CurrentPrice, sizeUSD,
						bestScore.Leverage, stopLoss, takeProfit),
					Color:     ColorGreen,
					Footer:    &discordgo.MessageEmbedFooter{Text: "Auto-hunt · PlaceBracketOrder"},
					Timestamp: time.Now().Format(time.RFC3339),
				})

				return
			}

			updateHunting()
		}

		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       "🏹 Hunt Complete — No Trade",
				Description: fmt.Sprintf("Scanned **%d** unique pairs across %d cycles — no qualifying setup met execution threshold.", totalScanned, maxScanCycles),
				Color:       ColorYellow,
				Footer:      &discordgo.MessageEmbedFooter{Text: "Try again later — market may be consolidating."},
				Timestamp:   time.Now().Format(time.RFC3339),
			}},
		})
	}()
}
