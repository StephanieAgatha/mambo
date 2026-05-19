package discord

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"mambo/config"
	"mambo/exchange"
	"mambo/filter"
	"mambo/journal"
	"mambo/market"
	"mambo/ta"
	aiPkg "mambo/ai"
)

// scanResult holds a scored pair waiting for user accept/decline/suggest.
type scanResult struct {
	pair     string
	taResult ta.TAResult
	mc       market.MarketContext
	state    filter.BotState
	score    aiPkg.ScoreResult
	// suggestMode is true when this is a prefilter-skipped pair that needs AI opinion
	suggestMode bool
	// skipReason is the prefilter rejection reason (only set in suggestMode)
	skipReason string
}

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

// handleJournal shows today's fills or last 7 days fetched live from Hyperliquid.
// Paginated: 10 trades per page.
func (b *Bot) handleJournal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	rangeVal := "today"
	page := 1
	for _, opt := range data.Options {
		if opt.Name == "range" {
			rangeVal = opt.StringValue()
		}
		if opt.Name == "page" {
			page = int(opt.IntValue())
		}
	}
	if page < 1 {
		page = 1
	}

	ctx := context.Background()
	now := time.Now()
	wib := time.FixedZone("WIB", 7*60*60)
	var startTime int64
	label := "Today"

	if rangeVal == "week" {
		startTime = now.AddDate(0, 0, -7).UnixMilli()
		label = "7 Days"
	} else {
		nowWIB := now.In(wib)
		todayStart := time.Date(nowWIB.Year(), nowWIB.Month(), nowWIB.Day(), 0, 0, 0, 0, wib)
		startTime = todayStart.UnixMilli()
	}

	fills, err := b.exClient.FetchFilledTrades(ctx, startTime, nil)
	if err != nil {
		slog.Error("discord: fetch fills failed", "err", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       "❌ Error",
					Description: "Failed to fetch trades from exchange.",
					Color:       ColorRed,
				}},
			},
		})
		return
	}

	if len(fills) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("📖 Journal — %s", label),
					Description: "No filled trades found.",
					Color:       ColorBlue,
				}},
			},
		})
		return
	}

	// filter out zero-PnL fills (partial fills / open entries)
	filtered := make([]exchange.FilledTrade, 0, len(fills))
	for _, f := range fills {
		if f.ClosedPnL != 0 {
			filtered = append(filtered, f)
		}
	}
	fills = filtered

	if len(fills) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("📖 Journal — %s", label),
					Description: "No closed trades yet (all $0.00 fills are partial entries).",
					Color:       ColorBlue,
				}},
			},
		})
		return
	}

	// sort fills newest first
	for i := 0; i < len(fills)/2; i++ {
		j := len(fills) - 1 - i
		fills[i], fills[j] = fills[j], fills[i]
	}

	// calc totals from ALL fills
	totalPnL := 0.0
	wins := 0
	losses := 0
	for _, f := range fills {
		totalPnL += f.ClosedPnL
		if f.ClosedPnL > 0 {
			wins++
		} else if f.ClosedPnL < 0 {
			losses++
		}
	}

	const perPage = 10
	totalPages := (len(fills) + perPage - 1) / perPage
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * perPage
	end := start + perPage
	if end > len(fills) {
		end = len(fills)
	}
	pageFills := fills[start:end]

	winRate := 0.0
	if wins+losses > 0 {
		winRate = float64(wins) / float64(wins+losses) * 100
	}

	pnlLabel := fmt.Sprintf("$%.2f", totalPnL)
	if totalPnL > 0 {
		pnlLabel = "+" + pnlLabel
	}

	fields := make([]*discordgo.MessageEmbedField, 0, len(pageFills)+1)

	// top summary
	fields = append(fields, &discordgo.MessageEmbedField{
		Name: "━━━━━━━━━━━━━━━━━━━━",
		Value: fmt.Sprintf(
			"**Total PnL:** %s | **W:** %d | **L:** %d | **WR:** %.0f%%",
			pnlLabel, wins, losses, winRate,
		),
		Inline: false,
	})

	// per-fill detail
	for _, f := range pageFills {
		// side: "A" = open short/close long, "B" = open long/close short
		sideEmoji := "🟢"
		sideLabel := "LONG"
		if f.Side == "A" {
			sideEmoji = "🔴"
			sideLabel = "SHORT"
		}

		pnlEmoji := "✅"
		if f.ClosedPnL < 0 {
			pnlEmoji = "❌"
		}

		pnlLabel := fmt.Sprintf("$%.2f", f.ClosedPnL)
		if f.ClosedPnL > 0 {
			pnlLabel = "+" + pnlLabel
		}

		timeWIB := f.Time.In(wib)
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: fmt.Sprintf("%s %s %s  %s %s",
				sideEmoji, f.Coin, sideLabel, pnlEmoji, pnlLabel),
			Value: fmt.Sprintf(
				"```Price: $%.4f   Size: %.4f %s   Fee: $%.3f\n%s WIB```",
				f.Price, f.Size, f.Coin, f.Fee,
				timeWIB.Format("02 Jan · 15:04"),
			),
			Inline: false,
		})
	}

	footer := fmt.Sprintf("Page %d/%d  •  %d total fills  •  Live from Hyperliquid",
		page, totalPages, len(fills))

	embed := &discordgo.MessageEmbed{
		Title:     fmt.Sprintf("📖 Journal — %s", label),
		Color:     ColorBlue,
		Fields:    fields,
		Footer:    &discordgo.MessageEmbedFooter{Text: footer},
		Timestamp: now.Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

// handlePnl shows total PnL + per-coin breakdown from Hyperliquid fills (7 days).
func (b *Bot) handlePnl(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx := context.Background()
	startTime := time.Now().AddDate(0, 0, -7).UnixMilli()

	fills, err := b.exClient.FetchFilledTrades(ctx, startTime, nil)
	if err != nil {
		slog.Error("discord: fetch fills for PnL failed", "err", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "❌ Error", Description: "Failed to fetch PnL from exchange.",
					Color: ColorRed,
				}},
			},
		})
		return
	}

	if len(fills) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "💰 PnL (7 days)", Description: "No filled trades found.",
					Color: ColorBlue,
				}},
			},
		})
		return
	}

	// filter zero-PnL fills (partial fills / open entries)
	filtered := make([]exchange.FilledTrade, 0, len(fills))
	for _, f := range fills {
		if f.ClosedPnL != 0 {
			filtered = append(filtered, f)
		}
	}
	fills = filtered

	if len(fills) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title: "💰 PnL (7 days)", Description: "No closed trades found.",
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

	// per-coin aggregation
	type coinStats struct {
		pnl    float64
		wins   int
		losses int
	}
	coinMap := make(map[string]*coinStats)
	coinOrder := make([]string, 0) // insertion order

	for _, f := range fills {
		totalPnL += f.ClosedPnL
		if f.ClosedPnL > 0 {
			wins++
			if f.ClosedPnL > bestWin {
				bestWin = f.ClosedPnL
			}
		} else if f.ClosedPnL < 0 {
			losses++
			if f.ClosedPnL < worstLoss {
				worstLoss = f.ClosedPnL
			}
		}

		cs, ok := coinMap[f.Coin]
		if !ok {
			cs = &coinStats{}
			coinMap[f.Coin] = cs
			coinOrder = append(coinOrder, f.Coin)
		}
		cs.pnl += f.ClosedPnL
		if f.ClosedPnL > 0 {
			cs.wins++
		} else if f.ClosedPnL < 0 {
			cs.losses++
		}
	}

	winRate := 0.0
	if wins+losses > 0 {
		winRate = float64(wins) / float64(wins+losses) * 100
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Total PnL", Value: formatPnL(totalPnL), Inline: true},
		{Name: "Win Rate", Value: fmt.Sprintf("%.0f%%", winRate), Inline: true},
		{Name: "Trades", Value: fmt.Sprintf("%d", wins+losses), Inline: true},
		{Name: "Wins", Value: fmt.Sprintf("%d", wins), Inline: true},
		{Name: "Losses", Value: fmt.Sprintf("%d", losses), Inline: true},
		{Name: "Best Win", Value: fmt.Sprintf("$%.2f", bestWin), Inline: true},
		{Name: "Worst Loss", Value: fmt.Sprintf("$%.2f", worstLoss), Inline: true},
	}

	// per-coin breakdown
	if len(coinOrder) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "━━━━━━ Per Coin ━━━━━━",
			Value:  "",
			Inline: false,
		})
		for _, coin := range coinOrder {
			cs := coinMap[coin]
			wr := 0.0
			if cs.wins+cs.losses > 0 {
				wr = float64(cs.wins) / float64(cs.wins+cs.losses) * 100
			}
			fields = append(fields, &discordgo.MessageEmbedField{
				Name: fmt.Sprintf("%s", coin),
				Value: fmt.Sprintf(
					"%s  |  W:%d L:%d  |  WR: %.0f%%",
					formatPnL(cs.pnl), cs.wins, cs.losses, wr,
				),
				Inline: true,
			})
		}
	}

	embed := &discordgo.MessageEmbed{
		Title:     "💰 PnL Summary (7 days)",
		Color:     ColorBlue,
		Fields:    fields,
		Timestamp: time.Now().Format(time.RFC3339),
		Footer:    &discordgo.MessageEmbedFooter{Text: "Live data from Hyperliquid"},
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

// formatPnL returns a signed PnL string.
func formatPnL(amount float64) string {
	s := fmt.Sprintf("$%.2f", amount)
	if amount > 0 {
		s = "+" + s
	}
	return s
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

	lossUSD, winUSD, consecLosses, err := b.exClient.FetchDailyPnL(context.Background())
	if err != nil {
		slog.Warn("discord: fetch daily PnL failed", "err", err)
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
// Keeps displaying "🔍 Scanning best pair…" during the search.
// When a scored pair is found (trade, hold, or wait), shows the result with Accept/Decline buttons.
// On Accept → execute trade (or acknowledge hold/wait). On Decline → scan continues.
// If no trade is found after all cycles, shows "No Trade Found".
func (b *Bot) handleScan(s *discordgo.Session, i *discordgo.InteractionCreate) {
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
			maxScanCycles    = 3
			maxPairsPerCycle = 15
			maxAIScores      = 10 // max AI calls per cycle to save tokens
			retryDelay       = 3 * time.Minute
		)

		ctx := context.Background()
		totalScanned := 0
		globalSeen := make(map[string]bool)

		updateScanning := func() {
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("🔍 Scanning best pair… (%d scanned)", totalScanned),
					Description: "Searching for a qualifying trade setup across the market.",
					Color:       ColorBlue,
				}},
			})
		}

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

			lossUSD, winUSD, consecLosses, err := b.exClient.FetchDailyPnL(ctx)
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

				taResult, mc, err := b.refreshPairData(ctx, pair, interval)
				if err != nil {
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

					dummySL := taResult.CurrentPrice - taResult.ATR*3
					dummyTP := taResult.CurrentPrice + taResult.ATR*6
					b.StartMonitor(ctx, pair, exchange.OrderSideLong, orderResult.Price, balance*0.10, config.MaxLeverageX, dummySL, dummyTP, 0, "dummy", "AI disabled scan", orderResult.OrderID)

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

				// Periodically update the scanning message
				if totalScanned%5 == 0 {
					updateScanning()
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

				// Any scored pair (trade, hold, wait) — show result with buttons
				b.showScanResult(s, i, pair, taResult, mc, state, score)
				return
			}

			// cycle finished, update scanning status
			updateScanning()
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

// showScanResult displays the AI score decision with Accept/Decline buttons.
// Stores the result in Bot.activeScan so the button handlers can reference it.
func (b *Bot) showScanResult(s *discordgo.Session, i *discordgo.InteractionCreate, pair string, taResult ta.TAResult, mc market.MarketContext, state filter.BotState, score aiPkg.ScoreResult) {
	isTrade := score.Action == "open_long" || score.Action == "open_short"

	var title, desc string
	if isTrade {
		title = fmt.Sprintf("🚀 Scan Complete — %s", pair)
		desc = fmt.Sprintf("**%s** with confidence **%.0f%%**\nEntry: **$%.4f** | SL: **$%.4f** | TP: **$%.4f**\n%s",
			strings.ToUpper(score.Action),
			score.Confidence,
			taResult.CurrentPrice,
			score.StopLoss,
			score.TakeProfit,
			score.Reasoning,
		)
	} else {
		title = fmt.Sprintf("⏸️ Scan Complete — %s (%s)", pair, strings.ToUpper(score.Action))
		desc = fmt.Sprintf("**%s** — %s\nConfidence: **%.0f%%** | Strategy: **%s**",
			strings.ToUpper(score.Action),
			score.Reasoning,
			score.Confidence,
			score.Strategy,
		)
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Price", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
		{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", score.Confidence), Inline: true},
		{Name: "Strategy", Value: score.Strategy, Inline: true},
	}

	if isTrade {
		fields = append(fields,
			&discordgo.MessageEmbedField{Name: "Entry", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
			&discordgo.MessageEmbedField{Name: "SL", Value: fmt.Sprintf("$%.4f", score.StopLoss), Inline: true},
			&discordgo.MessageEmbedField{Name: "TP", Value: fmt.Sprintf("$%.4f", score.TakeProfit), Inline: true},
			&discordgo.MessageEmbedField{Name: "Size", Value: fmt.Sprintf("$%.2f", score.PositionSizeUSD), Inline: true},
			&discordgo.MessageEmbedField{Name: "Leverage", Value: fmt.Sprintf("%dx", score.Leverage), Inline: true},
			&discordgo.MessageEmbedField{Name: "R:R", Value: fmt.Sprintf("%.2f", score.RRRatio), Inline: true},
		)
	}

	fields = append(fields,
		&discordgo.MessageEmbedField{Name: "Confluence", Value: fmt.Sprintf("%d signals", score.ConfluenceCount), Inline: true},
	)

	channelID := i.ChannelID

	// Store active scan so button handlers can read it
	b.scanMu.Lock()
	b.activeScan = &scanResult{
		pair:     pair,
		taResult: taResult,
		mc:       mc,
		state:    state,
		score:    score,
	}
	b.scanChannelID = channelID
	b.scanMu.Unlock()

	acceptLabel := "✅ Accept"
	declineLabel := "❌ Decline"
	if !isTrade {
		acceptLabel = "💡 Suggest"
		declineLabel = "❌ Skip"
	}

	msg := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{
				Title:       title,
				Description: desc,
				Color:       ColorYellow,
				Fields:      fields,
				Footer:      &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Do you accept this %s?", score.Action)},
				Timestamp:   time.Now().Format(time.RFC3339),
			}},
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.Button{
							Label:    acceptLabel,
							Style:    discordgo.SuccessButton,
							CustomID: "scan_accept",
						},
						discordgo.Button{
							Label:    declineLabel,
							Style:    discordgo.DangerButton,
							CustomID: "scan_decline",
						},
					},
				},
			},
		},
	}

	// Send a new follow-up message with buttons instead of editing the deferred response.
	// First clear the "scanning" deferred message, then send the result.
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{{
			Title:       "🔍 Scan Ready",
			Description: fmt.Sprintf("Found a setup for **%s**. See below for details.", pair),
			Color:       ColorBlue,
		}},
	})

	// Send follow-up with buttons
	_, err := s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
		Embeds:     msg.Data.Embeds,
		Components: msg.Data.Components,
	})
	if err != nil {
		slog.Error("discord: scan follow-up with buttons failed", "pair", pair, "err", err)
	}
}

// handleScanAccept handles the Accept/Acknowledge button click.
func (b *Bot) handleScanAccept(s *discordgo.Session, i *discordgo.InteractionCreate) {
	b.scanMu.Lock()
	sr := b.activeScan
	b.scanMu.Unlock()

	if sr == nil {
		b.respondAck(s, i, "⚠️ No active scan — run `/scan` first.")
		return
	}

	ctx := context.Background()
	isTrade := sr.score.Action == "open_long" || sr.score.Action == "open_short"

	if sr.suggestMode {
		// Prefilter skipped pair — run AI Suggest for advisory opinion
		// Show "analyzing..." while AI processes
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("🔍 Analyzing %s... hold on…", sr.pair),
					Description: "AI is analyzing this pair and preparing a trade suggestion.",
					Color:       ColorBlue,
				}},
				Components: []discordgo.MessageComponent{}, // remove buttons
			},
		})

		sugg, err := b.scorer.Suggest(ctx, sr.pair, sr.taResult, sr.mc, sr.state, sr.skipReason)
		b.scanMu.Lock()
		b.activeScan = nil
		b.scanMu.Unlock()

		if err != nil {
			s.ChannelMessageEditComplex(&discordgo.MessageEdit{
				Channel: i.ChannelID,
				ID:      i.Message.ID,
				Embeds:  &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("❌ AI Suggestion Failed — %s", sr.pair),
					Description: err.Error(),
					Color:       ColorRed,
				}},
			})
			return
		}

		b.editSuggestionResult(s, i, sr.pair, sr.taResult, sugg, sr.skipReason)
		return
	}

	if !isTrade {
		// AI already scored this as hold/wait — run AI Suggest for advisory opinion
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("🔍 Analyzing %s... hold on…", sr.pair),
					Description: "AI is analyzing this pair and preparing a trade suggestion.",
					Color:       ColorBlue,
				}},
				Components: []discordgo.MessageComponent{},
			},
		})

		sugg, err := b.scorer.Suggest(ctx, sr.pair, sr.taResult, sr.mc, sr.state, fmt.Sprintf("AI scored: %s (confidence %.0f%%)", sr.score.Action, sr.score.Confidence))
		b.scanMu.Lock()
		b.activeScan = nil
		b.scanMu.Unlock()

		if err != nil {
			s.ChannelMessageEditComplex(&discordgo.MessageEdit{
				Channel: i.ChannelID,
				ID:      i.Message.ID,
				Embeds:  &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("❌ AI Suggestion Failed — %s", sr.pair),
					Description: err.Error(),
					Color:       ColorRed,
				}},
			})
			return
		}

		b.editSuggestionResult(s, i, sr.pair, sr.taResult, sugg, fmt.Sprintf("AI scored: %s (confidence %.0f%%)", sr.score.Action, sr.score.Confidence))
		return
	}

	// Trade action — place order
	var side exchange.OrderSide
	if sr.score.Action == "open_long" {
		side = exchange.OrderSideLong
	} else {
		side = exchange.OrderSideShort
	}

	orderResult, err := b.exClient.PlaceLimitOrder(ctx, sr.pair, side, sr.score.PositionSizeUSD, sr.taResult.CurrentPrice, sr.score.Leverage)
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("❌ Order Failed — %s", sr.pair),
					Description: err.Error(),
					Color:       ColorRed,
				}},
				Components: []discordgo.MessageComponent{},
			},
		})
		b.scanMu.Lock()
		b.activeScan = nil
		b.scanMu.Unlock()
		return
	}

	// TP/SL placed by monitor after position fills — not here

	b.StartMonitor(ctx, sr.pair, side, orderResult.Price, sr.score.PositionSizeUSD, sr.score.Leverage,
		sr.score.StopLoss, sr.score.TakeProfit, sr.score.Confidence, sr.score.Strategy, sr.score.Reasoning, orderResult.OrderID)

	b.scanMu.Lock()
	b.activeScan = nil
	b.scanMu.Unlock()

	// Send notify embed
	b.SendEmbed(&discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🚀 %s %s EXECUTED", sr.pair, strings.ToUpper(string(side))),
		Description: fmt.Sprintf("**%s**", sr.score.Reasoning),
		Color:       ColorGreen,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Entry", Value: fmt.Sprintf("$%.4f", orderResult.Price), Inline: true},
			{Name: "SL", Value: fmt.Sprintf("$%.4f", sr.score.StopLoss), Inline: true},
			{Name: "TP", Value: fmt.Sprintf("$%.4f", sr.score.TakeProfit), Inline: true},
			{Name: "Size", Value: fmt.Sprintf("$%.2f", sr.score.PositionSizeUSD), Inline: true},
			{Name: "Leverage", Value: fmt.Sprintf("%dx", sr.score.Leverage), Inline: true},
			{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", sr.score.Confidence), Inline: true},
			{Name: "Strategy", Value: sr.score.Strategy, Inline: true},
			{Name: "Order ID", Value: fmt.Sprintf("%d", orderResult.OrderID), Inline: true},
		},
		Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
		Timestamp: time.Now().Format(time.RFC3339),
	})

	// Update the button message
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("🚀 %s %s EXECUTED", sr.pair, strings.ToUpper(string(side))),
				Description: fmt.Sprintf("Order placed at **$%.4f** | Order ID: **%d**\n%s",
					orderResult.Price, orderResult.OrderID, sr.score.Reasoning),
				Color: ColorGreen,
				Fields: []*discordgo.MessageEmbedField{
					{Name: "Entry", Value: fmt.Sprintf("$%.4f", orderResult.Price), Inline: true},
					{Name: "SL", Value: fmt.Sprintf("$%.4f", sr.score.StopLoss), Inline: true},
					{Name: "TP", Value: fmt.Sprintf("$%.4f", sr.score.TakeProfit), Inline: true},
					{Name: "Size", Value: fmt.Sprintf("$%.2f", sr.score.PositionSizeUSD), Inline: true},
					{Name: "Leverage", Value: fmt.Sprintf("%dx", sr.score.Leverage), Inline: true},
					{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", sr.score.Confidence), Inline: true},
				},
				Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
				Timestamp: time.Now().Format(time.RFC3339),
			}},
			Components: []discordgo.MessageComponent{},
		},
	})

	slog.Info("scan: user accepted trade",
		"pair", sr.pair,
		"action", sr.score.Action,
		"entry", orderResult.Price,
		"size", sr.score.PositionSizeUSD,
		"order_id", orderResult.OrderID,
	)
}

// handleScanDecline handles the Decline/Skip button click.
// Clears the active scan and indicates a new scan should be started.
func (b *Bot) handleScanDecline(s *discordgo.Session, i *discordgo.InteractionCreate) {
	b.scanMu.Lock()
	sr := b.activeScan
	b.activeScan = nil
	b.scanMu.Unlock()

	if sr == nil {
		b.respondAck(s, i, "⚠️ No active scan — run `/scan` first.")
		return
	}

	slog.Info("scan: user declined",
		"pair", sr.pair,
		"action", sr.score.Action,
	)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("❌ Declined — %s", sr.pair),
				Description: "Run `/scan` again to search for a new setup.",
				Color:       ColorRed,
				Footer:      &discordgo.MessageEmbedFooter{Text: randomQuote()},
				Timestamp:   time.Now().Format(time.RFC3339),
			}},
			Components: []discordgo.MessageComponent{},
		},
	})
}

// handleSuggestExecute executes a trade based on the AI suggestion.
func (b *Bot) handleSuggestExecute(s *discordgo.Session, i *discordgo.InteractionCreate) {
	b.scanMu.Lock()
	sr := b.activeScan
	b.activeScan = nil
	b.scanMu.Unlock()

	if sr == nil {
		b.respondAck(s, i, "⚠️ No active suggestion — run `/scan` first.")
		return
	}

	ctx := context.Background()

	isTrade := sr.score.Action == "open_long" || sr.score.Action == "open_short"
	if !isTrade {
		b.respondAck(s, i, fmt.Sprintf("⚠️ AI says \"%s\" — not a trade setup. Wait for a better signal.", sr.score.Action))
		return
	}

	var side exchange.OrderSide
	if sr.score.Action == "open_long" {
		side = exchange.OrderSideLong
	} else {
		side = exchange.OrderSideShort
	}

	// Clamp suggestion values — AI may return 0 for leverage
	leverage := sr.score.Leverage
	if leverage < config.MinLeverageX {
		leverage = config.MinLeverageX
	}
	if leverage > config.MaxLeverageX {
		leverage = config.MaxLeverageX
	}
	sizeUSD := sr.score.PositionSizeUSD
	if sizeUSD <= 0 {
		// AI didn't suggest size — use 5% of balance as default
		sizeUSD = sr.state.Balance * 0.05
	}

	// Ensure stop loss and take profit exist — use ±3×ATR as fallback
	stopLoss := sr.score.StopLoss
	takeProfit := sr.score.TakeProfit
	if stopLoss <= 0 || takeProfit <= 0 {
		atrMult := sr.taResult.ATR * 3
		if side == exchange.OrderSideLong {
			stopLoss = sr.taResult.CurrentPrice - atrMult
			takeProfit = sr.taResult.CurrentPrice + atrMult*2
		} else {
			stopLoss = sr.taResult.CurrentPrice + atrMult
			takeProfit = sr.taResult.CurrentPrice - atrMult*2
		}
	}

	// v2: atomic bracket order — entry + TP + SL in one call
	orderResult, err := b.exClient.PlaceBracketOrder(
		ctx, sr.pair, side, sizeUSD,
		sr.taResult.CurrentPrice,
		takeProfit,
		stopLoss,
		leverage,
	)
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("❌ Order Failed — %s", sr.pair),
					Description: err.Error(),
					Color:       ColorRed,
				}},
				Components: []discordgo.MessageComponent{},
			},
		})
		return
	}

	b.StartMonitor(ctx, sr.pair, side, orderResult.Price, sizeUSD, leverage,
		stopLoss, takeProfit, sr.score.Confidence, sr.score.Strategy, sr.score.Reasoning, orderResult.OrderID)

	// Notify channel
	b.SendEmbed(&discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🚀 %s %s EXECUTED (suggestion)", sr.pair, strings.ToUpper(string(side))),
		Description: fmt.Sprintf("**%s**", sr.score.Reasoning),
		Color:       ColorGreen,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Entry", Value: fmt.Sprintf("$%.4f", orderResult.Price), Inline: true},
			{Name: "SL", Value: fmt.Sprintf("$%.4f", sr.score.StopLoss), Inline: true},
			{Name: "TP", Value: fmt.Sprintf("$%.4f", sr.score.TakeProfit), Inline: true},
			{Name: "Size", Value: fmt.Sprintf("$%.2f", sizeUSD), Inline: true},
			{Name: "Leverage", Value: fmt.Sprintf("%dx", leverage), Inline: true},
			{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", sr.score.Confidence), Inline: true},
			{Name: "Strategy", Value: sr.score.Strategy, Inline: true},
			{Name: "Order ID", Value: fmt.Sprintf("%d", orderResult.OrderID), Inline: true},
		},
		Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
		Timestamp: time.Now().Format(time.RFC3339),
	})

	// Update button message
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("🚀 %s %s EXECUTED (suggestion)", sr.pair, strings.ToUpper(string(side))),
				Description: fmt.Sprintf("Order placed at **$%.4f** | Order ID: **%d**\n%s",
					orderResult.Price, orderResult.OrderID, sr.score.Reasoning),
				Color: ColorGreen,
				Fields: []*discordgo.MessageEmbedField{
					{Name: "Entry", Value: fmt.Sprintf("$%.4f", orderResult.Price), Inline: true},
					{Name: "SL", Value: fmt.Sprintf("$%.4f", sr.score.StopLoss), Inline: true},
					{Name: "TP", Value: fmt.Sprintf("$%.4f", sr.score.TakeProfit), Inline: true},
					{Name: "Size", Value: fmt.Sprintf("$%.2f", sizeUSD), Inline: true},
					{Name: "Leverage", Value: fmt.Sprintf("%dx", leverage), Inline: true},
					{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", sr.score.Confidence), Inline: true},
				},
				Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
				Timestamp: time.Now().Format(time.RFC3339),
			}},
			Components: []discordgo.MessageComponent{},
		},
	})

	slog.Info("scan: user executed suggestion",
		"pair", sr.pair,
		"action", sr.score.Action,
		"entry", orderResult.Price,
		"size", sr.score.PositionSizeUSD,
		"order_id", orderResult.OrderID,
	)
}

// handleSuggestSkip skips the AI suggestion.
func (b *Bot) handleSuggestSkip(s *discordgo.Session, i *discordgo.InteractionCreate) {
	b.scanMu.Lock()
	sr := b.activeScan
	b.activeScan = nil
	b.scanMu.Unlock()

	pair := "unknown"
	if sr != nil {
		pair = sr.pair
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("❌ Skipped — %s", pair),
				Description: "Run `/scan` again to search for a new setup.",
				Color:       ColorRed,
				Footer:      &discordgo.MessageEmbedFooter{Text: randomQuote()},
				Timestamp:   time.Now().Format(time.RFC3339),
			}},
			Components: []discordgo.MessageComponent{},
		},
	})
}

// respondAck sends a quick ephemeral ACK to a component interaction.
func (b *Bot) respondAck(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

// showSkippedPair displays a prefilter-skipped pair with Suggest/Skip buttons.
func (b *Bot) showSkippedPair(s *discordgo.Session, i *discordgo.InteractionCreate, pair string, taResult ta.TAResult, mc market.MarketContext, state filter.BotState, reason string) {
	channelID := i.ChannelID

	// Store active scan so Suggest button handler can call AI
	b.scanMu.Lock()
	b.activeScan = &scanResult{
		pair:        pair,
		taResult:    taResult,
		mc:          mc,
		state:       state,
		score:       aiPkg.ScoreResult{Action: "hold", Reasoning: reason},
		suggestMode: true,
		skipReason:  reason,
	}
	b.scanChannelID = channelID
	b.scanMu.Unlock()

	fields := []*discordgo.MessageEmbedField{
		{Name: "Price", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
		{Name: "RSI", Value: fmt.Sprintf("%.2f", taResult.RSI), Inline: true},
		{Name: "EMA200", Value: fmt.Sprintf("$%.4f", taResult.EMA200), Inline: true},
		{Name: "Volume", Value: fmt.Sprintf("%.2fx", taResult.VolumeMultiplier), Inline: true},
		{Name: "Reason", Value: reason, Inline: false},
	}

	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{{
			Title:       "🔍 Scan Ready",
			Description: fmt.Sprintf("Found **%s** but prefilter skipped it. Want AI suggestion?", pair),
			Color:       ColorBlue,
		}},
	})

	_, err := s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{{
			Title:       fmt.Sprintf("⏭️ %s — Skipped by Prefilter", pair),
			Description: fmt.Sprintf("**Reason**: %s\n\nWant AI to analyze this pair anyway and give a trade suggestion?", reason),
			Color:       ColorYellow,
			Fields:      fields,
			Footer:      &discordgo.MessageEmbedFooter{Text: "AI Suggestion is advisory only — DYOR"},
			Timestamp:   time.Now().Format(time.RFC3339),
		}},
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "💡 Suggest",
						Style:    discordgo.PrimaryButton,
						CustomID: "scan_accept",
					},
					discordgo.Button{
						Label:    "❌ Skip",
						Style:    discordgo.DangerButton,
						CustomID: "scan_decline",
					},
				},
			},
		},
	})
	if err != nil {
		slog.Error("discord: scan follow-up with buttons failed", "pair", pair, "err", err)
	}
}

// editSuggestionResult edits the "analyzing..." message with the AI suggestion result + Execute/Skip buttons.
// Auto-skip fires after 20 seconds of no response.
func (b *Bot) editSuggestionResult(s *discordgo.Session, i *discordgo.InteractionCreate, pair string, taResult ta.TAResult, sugg aiPkg.ScoreResult, contextReason string) {
	isTrade := sugg.Action == "open_long" || sugg.Action == "open_short"
	color := ColorYellow
	title := fmt.Sprintf("💡 AI Suggestion — %s", pair)
	sideLabel := ""
	if isTrade {
		color = ColorGreen
		sideLabel = " " + strings.ToUpper(strings.TrimPrefix(sugg.Action, "open_"))
		title = fmt.Sprintf("💡 AI Suggestion — %s%s", pair, sideLabel)
	}

	desc := fmt.Sprintf("**%s**\n\nConfidence: **%.0f%%** | Strategy: **%s**",
		sugg.Reasoning,
		sugg.Confidence,
		sugg.Strategy,
	)

	fields := []*discordgo.MessageEmbedField{
		{Name: "Price", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
		{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", sugg.Confidence), Inline: true},
		{Name: "Strategy", Value: sugg.Strategy, Inline: true},
		{Name: "Context", Value: contextReason, Inline: false},
	}

	if isTrade && sugg.PositionSizeUSD > 0 {
		fields = append(fields,
			&discordgo.MessageEmbedField{Name: "Entry", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
			&discordgo.MessageEmbedField{Name: "SL", Value: fmt.Sprintf("$%.4f", sugg.StopLoss), Inline: true},
			&discordgo.MessageEmbedField{Name: "TP", Value: fmt.Sprintf("$%.4f", sugg.TakeProfit), Inline: true},
			&discordgo.MessageEmbedField{Name: "Size", Value: fmt.Sprintf("$%.2f", sugg.PositionSizeUSD), Inline: true},
			&discordgo.MessageEmbedField{Name: "Leverage", Value: fmt.Sprintf("%dx", sugg.Leverage), Inline: true},
			&discordgo.MessageEmbedField{Name: "R:R", Value: fmt.Sprintf("%.2f", sugg.RRRatio), Inline: true},
		)
	}

	// Store suggestion so button handlers can execute it
	b.scanMu.Lock()
	b.activeScan = &scanResult{
		pair:     pair,
		taResult: taResult,
		mc:       market.MarketContext{},
		state:    filter.BotState{},
		score:    sugg,
	}
	b.scanMu.Unlock()

	// Update the "analyzing..." message with the result + buttons
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: i.ChannelID,
		ID:      i.Message.ID,
		Embeds: &[]*discordgo.MessageEmbed{{
			Title:       title,
			Description: desc,
			Color:       color,
			Fields:      fields,
			Footer:      &discordgo.MessageEmbedFooter{Text: "Auto-skip in 20s if no response. AI suggestion — always DYOR."},
			Timestamp:   time.Now().Format(time.RFC3339),
		}},
		Components: &[]discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "🚀 Execute",
						Style:    discordgo.SuccessButton,
						CustomID: "suggest_execute",
					},
					discordgo.Button{
						Label:    "❌ Skip",
						Style:    discordgo.DangerButton,
						CustomID: "suggest_skip",
					},
				},
			},
		},
	})
	if err != nil {
		slog.Error("discord: edit suggestion result failed", "pair", pair, "err", err)
	}

	// Auto-skip after 20 seconds if no user response
	go func() {
		time.Sleep(20 * time.Second)
		b.scanMu.Lock()
		sr := b.activeScan
		// only auto-skip if this is still the active suggestion (user didn't click anything)
		if sr != nil && sr.pair == pair {
			b.activeScan = nil
			b.scanMu.Unlock()
			slog.Info("scan: auto-skipping suggestion after 20s timeout",
				"pair", pair,
			)
			// can't use InteractionResponseEdit on a message modified by FollowupMessageCreate
			// just clear the buttons to indicate timeout
			s.ChannelMessageEditComplex(&discordgo.MessageEdit{
				Channel: i.ChannelID,
				ID:      i.Message.ID,
				Components: &[]discordgo.MessageComponent{},
			})
		} else {
			b.scanMu.Unlock()
		}
	}()
}

// handleExecute places a real order — either through the full AI pipeline or directly.
// bypass=true: skip prefilter + AI, execute immediately with formula defaults.
// bypass=false (default): full prefilter → AI score → execute if AI approves.
func (b *Bot) handleExecute(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	coin := strings.ToUpper(data.Options[0].StringValue())
	interval := "4h"
	bypass := false
	for _, opt := range data.Options {
		if opt.Name == "interval" {
			interval = opt.StringValue()
		}
		if opt.Name == "bypass" {
			bypass = opt.BoolValue()
		}
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	go func() {
		ctx := context.Background()

		candles, err := b.fetcher.FetchOHLCV(ctx, coin, interval, 200)
		if err != nil {
			slog.Error("discord: /execute fetch OHLCV failed", "coin", coin, "err", err)
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
			slog.Error("discord: /execute TA failed", "coin", coin, "err", err)
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
			slog.Warn("discord: /execute market context unavailable", "coin", coin, "err", err)
		}

		balance, err := b.exClient.FetchBalance(ctx)
		if err != nil {
			slog.Warn("discord: /execute fetch balance failed", "err", err)
		}

		lossUSD, winUSD, consecLosses, err := b.exClient.FetchDailyPnL(ctx)
		if err != nil {
			slog.Warn("discord: /execute daily PnL failed", "err", err)
		}

		state := filter.BotState{
			Balance:           balance,
			DailyLossUSD:      lossUSD,
			DailyWinUSD:       winUSD,
			ConsecutiveLosses: consecLosses,
			TotalAtRiskUSD:    0,
		}

		if bypass {
			// bypass mode — execute immediately with formula defaults
			side := exchange.OrderSideLong
			if taResult.CurrentPrice < taResult.EMA200 {
				side = exchange.OrderSideShort
			}

			sizeUSD := balance * 0.10 // 10% of balance
			sizeUSD = math.Round(sizeUSD*100) / 100
			leverage := 5

			orderResult, err := b.exClient.PlaceLimitOrder(ctx, coin, side, sizeUSD, taResult.CurrentPrice, leverage)
			if err != nil {
				s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds: &[]*discordgo.MessageEmbed{{
						Title:       fmt.Sprintf("❌ %s — Order Failed", coin),
						Description: err.Error(),
						Color:       ColorRed,
					}},
				})
				return
			}

			// bypass SL/TP: 3x ATR stop, 6x ATR target
			bypassSL := taResult.CurrentPrice - taResult.ATR*3
			bypassTP := taResult.CurrentPrice + taResult.ATR*6
			if side == exchange.OrderSideShort {
				bypassSL = taResult.CurrentPrice + taResult.ATR*3
				bypassTP = taResult.CurrentPrice - taResult.ATR*6
			}

			b.StartMonitor(ctx, coin, side, orderResult.Price, sizeUSD, leverage, bypassSL, bypassTP, 0, "bypass", "direct execution", orderResult.OrderID)

			b.SendEmbed(&discordgo.MessageEmbed{
				Title:       fmt.Sprintf("⚡ %s %s EXECUTED (bypass)", coin, strings.ToUpper(string(side))),
				Description: "Direct execution — no prefilter, no AI.",
				Color:       ColorGreen,
				Fields: []*discordgo.MessageEmbedField{
					{Name: "Entry", Value: fmt.Sprintf("$%.4f", orderResult.Price), Inline: true},
					{Name: "SL", Value: fmt.Sprintf("$%.4f", bypassSL), Inline: true},
					{Name: "TP", Value: fmt.Sprintf("$%.4f", bypassTP), Inline: true},
					{Name: "Size", Value: fmt.Sprintf("$%.2f", orderResult.SizeUSD), Inline: true},
					{Name: "Leverage", Value: fmt.Sprintf("%dx cross", orderResult.Leverage), Inline: true},
					{Name: "Side", Value: strings.ToUpper(string(side)), Inline: true},
					{Name: "Order ID", Value: fmt.Sprintf("%d", orderResult.OrderID), Inline: true},
				},
				Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
				Timestamp: time.Now().Format(time.RFC3339),
			})

			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("⚡ %s %s EXECUTED (bypass)", coin, strings.ToUpper(string(side))),
					Description: "Direct execution — no prefilter, no AI. Monitor active.",
					Color:       ColorGreen,
					Fields: []*discordgo.MessageEmbedField{
						{Name: "Entry", Value: fmt.Sprintf("$%.4f", orderResult.Price), Inline: true},
						{Name: "SL", Value: fmt.Sprintf("$%.4f", bypassSL), Inline: true},
						{Name: "TP", Value: fmt.Sprintf("$%.4f", bypassTP), Inline: true},
						{Name: "Size", Value: fmt.Sprintf("$%.2f", orderResult.SizeUSD), Inline: true},
						{Name: "Leverage", Value: fmt.Sprintf("%dx cross", orderResult.Leverage), Inline: true},
						{Name: "Order ID", Value: fmt.Sprintf("%d", orderResult.OrderID), Inline: true},
					},
					Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
					Timestamp: time.Now().Format(time.RFC3339),
				}},
			})
			return
		}

		// execute mode — no prefilter, AI always gives a direction
		score, err := b.scorer.Execute(ctx, coin, taResult, mc, state)
		if err != nil {
			slog.Error("discord: /execute AI execute failed", "coin", coin, "err", err)
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("❌ %s — AI Error", coin),
					Description: fmt.Sprintf("AI execution failed: %s", err.Error()),
					Color:       ColorRed,
				}},
			})
			return
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
			Pair:       coin,
			TA:         taResult,
			Market:     mc,
			AIDecision: aiLog,
		})

		// Always execute — no wait/hold logic
		var side exchange.OrderSide
		if score.Direction == "short" || score.Action == "open_short" {
			side = exchange.OrderSideShort
		} else {
			side = exchange.OrderSideLong
		}

		orderResult, err := b.exClient.PlaceLimitOrder(ctx, coin, side, score.PositionSizeUSD, taResult.CurrentPrice, score.Leverage)
		if err != nil {
			slog.Error("discord: /execute order failed", "coin", coin, "err", err)
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("❌ %s — Order Failed", coin),
					Description: err.Error(),
					Color:       ColorRed,
				}},
			})
			return
		}

		// TP/SL placed by monitor after position fills — not here

		b.NotifyTradeExecuted(coin, score.Action, taResult.CurrentPrice, score.PositionSizeUSD, score.Leverage, score.Confidence, score.Strategy, score.Reasoning)

		b.StartMonitor(ctx, coin, side, orderResult.Price, score.PositionSizeUSD, score.Leverage, score.StopLoss, score.TakeProfit, score.Confidence, score.Strategy, score.Reasoning, orderResult.OrderID)

		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       fmt.Sprintf("🚀 %s %s EXECUTED", coin, strings.ToUpper(strings.TrimPrefix(score.Action, "open_"))),
				Description: fmt.Sprintf("**%s**", score.Reasoning),
				Color:       ColorGreen,
				Fields: []*discordgo.MessageEmbedField{
					{Name: "Entry", Value: fmt.Sprintf("$%.4f", taResult.CurrentPrice), Inline: true},
					{Name: "SL", Value: fmt.Sprintf("$%.4f", score.StopLoss), Inline: true},
					{Name: "TP", Value: fmt.Sprintf("$%.4f", score.TakeProfit), Inline: true},
					{Name: "Size", Value: fmt.Sprintf("$%.2f", score.PositionSizeUSD), Inline: true},
					{Name: "Leverage", Value: fmt.Sprintf("%dx cross", score.Leverage), Inline: true},
					{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", score.Confidence), Inline: true},
					{Name: "Strategy", Value: score.Strategy, Inline: true},
					{Name: "R:R", Value: fmt.Sprintf("%.2f", score.RRRatio), Inline: true},
					{Name: "Order ID", Value: fmt.Sprintf("%d", orderResult.OrderID), Inline: true},
				},
				Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
				Timestamp: time.Now().Format(time.RFC3339),
			}},
		})
	}()
}

// refreshPairData fetches candles, calculates TA, and fetches market context for a pair.
func (b *Bot) refreshPairData(ctx context.Context, pair, interval string) (ta.TAResult, market.MarketContext, error) {
	candles, err := b.fetcher.FetchOHLCV(ctx, pair, interval, 200)
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
// handlePairs shows the current list of active trading pairs.
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
