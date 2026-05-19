package discord

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
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
			maxScanCycles      = 5
			prescreenBatchSize = 20 // pairs per Altfins batch call
			maxFullTA          = 6  // pairs that get full OHLCV+TA+AI scoring
			maxAIScores        = 6
			retryDelay         = 2 * time.Minute
		)

		ctx := context.Background()
		totalScanned := 0
		globalSeen := make(map[string]bool)

		updateHunting := func() {
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("🏹 Hunting for trade… (%d pairs screened)", totalScanned),
					Description: "Screening with Altfins batch, scoring top candidates with AI.",
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

			// ── Phase 1: Batch pre-screen via Altfins ───────────────────────
			sample := pickRandomSample(prescreenBatchSize, globalSeen)
			if len(sample) == 0 {
				slog.Info("hunt: no new pairs to sample")
				break
			}
			for _, p := range sample {
				globalSeen[p] = true
			}
			totalScanned += len(sample)
			updateHunting()

			altfinsIntvl := mapHuntToAltfins(interval)
			afSnapshot, afErr := b.fetcher.FetchAltfinsBatchSnapshot(ctx, sample, altfinsIntvl)
			if afErr != nil {
				slog.Warn("hunt: Altfins batch failed, falling back to sequential", "err", afErr)
				afSnapshot = nil
			}

			// ── Phase 2: Rank candidates by pre-screen heuristics ──────────
			type candidate struct {
				pair       string
				closePrice float64
				volume     float64
				score      float64
			}
			var ranked []candidate

			for _, pair := range sample {
				afCandle, hasAF := afSnapshot[pair]
				if !hasAF {
					// not in Altfins — try with Hyperliquid directly
					ranked = append(ranked, candidate{pair: pair, score: 0})
					continue
				}
				c := candidate{
					pair:       pair,
					closePrice: float64(afCandle.Close),
					volume:     float64(afCandle.Volume),
				}
				// Heuristic: prefer coins with volume > 0 and reasonable price
				// Rank by volume descending — higher volume = more interest
				c.score = c.volume
				if c.closePrice <= 0 || c.volume <= 0 {
					c.score = -1 // dead coin
				}
				ranked = append(ranked, c)
			}

			// Sort by score desc, trim to maxFullTA
			sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
			if len(ranked) > maxFullTA {
				ranked = ranked[:maxFullTA]
			}

			// ── Phase 3: Full OHLCV + TA + AI for top candidates (parallel) ──
			type scoredResult struct {
				pair     string
				taResult ta.TAResult
				mc       market.MarketContext
				score    aiPkg.ScoreResult
				err      error
			}

			var wg sync.WaitGroup
			sem := make(chan struct{}, 2) // max 2 concurrent HL+AI calls (avoid rate limits)
			results := make(chan scoredResult, len(ranked))

			for _, cand := range ranked {
				wg.Add(1)
				go func(c candidate) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					var sr scoredResult
					sr.pair = c.pair

					bars, err := b.fetcher.FetchOHLCV(ctx, c.pair, interval, 200)
					if err != nil {
						sr.err = err
						results <- sr
						return
					}
					taResult, err := ta.Calculate(bars)
					if err != nil {
						sr.err = err
						results <- sr
						return
					}
					mc, _ := b.fetcher.FetchMarketContext(ctx, c.pair)

					sr.taResult = taResult
					sr.mc = mc
					sr.score, sr.err = b.scorer.Score(ctx, c.pair, taResult, mc, state)
					results <- sr
				}(cand)
				time.Sleep(300 * time.Millisecond) // stagger launches to avoid burst rate limits
			}

			go func() {
				wg.Wait()
				close(results)
			}()

			var bestScore aiPkg.ScoreResult
			var bestPair string
			var bestTA ta.TAResult

			for sr := range results {
				// log ALL scan results to analysis_log.json for audit
				aiLog := &journal.AIDecisionLog{}
				aiErr := ""
				if sr.err != nil {
					aiErr = sr.err.Error()
				} else {
					aiLog = &journal.AIDecisionLog{
						Action:          sr.score.Action,
						Leverage:        sr.score.Leverage,
						PositionSizeUSD: sr.score.PositionSizeUSD,
						StopLoss:        sr.score.StopLoss,
						TakeProfit:      sr.score.TakeProfit,
						Confidence:      sr.score.Confidence,
						Strategy:        sr.score.Strategy,
						ConfluenceCount: sr.score.ConfluenceCount,
						RRRatio:         sr.score.RRRatio,
						Reasoning:       sr.score.Reasoning,
					}
				}
				b.jl.AppendAnalysisLog(journal.AnalysisLogEntry{
					Timestamp:  journal.Now(),
					Pair:       sr.pair,
					TA:         sr.taResult,
					Market:     sr.mc,
					AIDecision: aiLog,
					AIError:    aiErr,
				})

				if sr.err != nil {
					continue
				}

				isTrade := sr.score.Action == "open_long" || sr.score.Action == "open_short"
				if !isTrade {
					continue
				}

				if sr.score.Confidence > bestScore.Confidence {
					bestScore = sr.score
					bestPair = sr.pair
					bestTA = sr.taResult
				}
			}

			if bestPair != "" {
				executeHuntTrade(ctx, b, s, i, bestPair, bestScore, bestTA, balance, cycle+1, totalScanned)
				return
			}

			updateHunting()
		}

		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       "🏹 Hunt Complete — No Trade",
				Description: fmt.Sprintf("Screened **%d** unique pairs across %d cycles — no qualifying setup met execution threshold.", totalScanned, maxScanCycles),
				Color:       ColorYellow,
				Footer:      &discordgo.MessageEmbedFooter{Text: "Try again later — market may be consolidating."},
				Timestamp:   time.Now().Format(time.RFC3339),
			}},
		})
	}()
}

// pickRandomSample returns n unseen random pairs from the full pool.
func pickRandomSample(n int, seen map[string]bool) []string {
	all, err := market.LoadPairs()
	if err != nil {
		return nil
	}

	var result []string
	// shuffle the slice
	for i := range all {
		j := rand.Intn(i + 1)
		all[i], all[j] = all[j], all[i]
	}

	for _, p := range all {
		if seen[p] {
			continue
		}
		result = append(result, p)
		if len(result) >= n {
			break
		}
	}
	return result
}

// mapHuntToAltfins converts mambo timeframe to Altfins timeInterval.
func mapHuntToAltfins(interval string) string {
	switch interval {
	case "1d", "1w":
		return "DAILY"
	default:
		return "HOURLY"
	}
}

// executeHuntTrade places the bracket order and starts monitoring for the best hunt candidate.
func executeHuntTrade(ctx context.Context, b *Bot, s *discordgo.Session, i *discordgo.InteractionCreate,
	pair string, score aiPkg.ScoreResult, taResult ta.TAResult, balance float64, cycle, totalScanned int) {

	slog.Info("hunt: trade found — executing",
		"pair", pair,
		"action", score.Action,
		"confidence", score.Confidence,
		"cycle", cycle,
	)

	side := exchange.OrderSideLong
	if score.Action == "open_short" {
		side = exchange.OrderSideShort
	}

	sizeUSD := score.PositionSizeUSD
	if sizeUSD <= 0 {
		sizeUSD = balance * 0.05
	}

	stopLoss := score.StopLoss
	takeProfit := score.TakeProfit
	if stopLoss <= 0 || takeProfit <= 0 {
		atrMult := taResult.ATR * 3
		if side == exchange.OrderSideLong {
			stopLoss = taResult.CurrentPrice - atrMult
			takeProfit = taResult.CurrentPrice + atrMult*2
		} else {
			stopLoss = taResult.CurrentPrice + atrMult
			takeProfit = taResult.CurrentPrice - atrMult*2
		}
	}

	orderResult, err := b.exClient.PlaceBracketOrder(
		ctx, pair, side, sizeUSD,
		taResult.CurrentPrice,
		takeProfit,
		stopLoss,
		score.Leverage,
	)
	if err != nil {
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("❌ Hunt Order Failed — %s", pair),
				Description: err.Error(),
				Color:       ColorRed,
			}},
		})
		return
	}

	b.StartMonitor(ctx, pair, side, taResult.CurrentPrice, sizeUSD,
		score.Leverage, stopLoss, takeProfit,
		score.Confidence, score.Strategy, score.Reasoning, orderResult.OrderID)

	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{{
			Title: fmt.Sprintf("🏹 HUNT SUCCESS — %s", pair),
			Description: fmt.Sprintf("**%s** | Confidence: **%.0f%%** | R:R: **%.2f**\nEntry: **$%.4f** | Size: **$%.2f** | **%dx**\nSL: **$%.4f** | TP: **$%.4f**\n\n%s",
				strings.ToUpper(string(side)), score.Confidence, score.RRRatio,
				taResult.CurrentPrice, sizeUSD, score.Leverage,
				stopLoss, takeProfit, score.Reasoning),
			Color: ColorGreen,
			Fields: []*discordgo.MessageEmbedField{
				{Name: "Strategy", Value: score.Strategy, Inline: true},
				{Name: "Confluence", Value: fmt.Sprintf("%d/9", score.ConfluenceCount), Inline: true},
				{Name: "Screened", Value: fmt.Sprintf("%d pairs", totalScanned), Inline: true},
			},
			Footer:    &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Hunt cycle %d · Altfins prescreen · Monitoring active", cycle)},
			Timestamp: time.Now().Format(time.RFC3339),
		}},
	})

	b.SendEmbed(&discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🏹 Trade Executed — %s", pair),
		Description: fmt.Sprintf("**%s** | Entry: $%.4f | Size: $%.2f | %dx | SL: $%.4f | TP: $%.4f",
			strings.ToUpper(string(side)), taResult.CurrentPrice, sizeUSD,
			score.Leverage, stopLoss, takeProfit),
		Color:     ColorGreen,
		Footer:    &discordgo.MessageEmbedFooter{Text: "Auto-hunt · Altfins batch + PlaceBracketOrder"},
		Timestamp: time.Now().Format(time.RFC3339),
	})
}
