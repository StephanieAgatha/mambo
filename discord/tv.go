package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"mambo/config"
	"mambo/exchange"
	"mambo/filter"
	"mambo/ta"
	aiPkg "mambo/ai"
)

// coinAnalysisRaw holds the nested coin_analysis tool response.
type coinAnalysisRaw struct {
	RSI  struct{ Value float64 `json:"value"` }          `json:"rsi"`
	MACD struct {
		MacdLine   float64 `json:"macd_line"`
		SignalLine float64 `json:"signal_line"`
		Histogram  float64 `json:"histogram"`
	} `json:"macd"`
	EMA struct {
		EMA10  float64 `json:"ema10"`
		EMA20  float64 `json:"ema20"`
		EMA50  float64 `json:"ema50"`
		EMA200 float64 `json:"ema200"`
	} `json:"ema"`
	BB struct {
		Upper  float64 `json:"upper"`
		Middle float64 `json:"middle"`
		Lower  float64 `json:"lower"`
	} `json:"bollinger_bands"`
	ATR  struct{ Value float64 `json:"value"` } `json:"atr"`
	Bias  string  `json:"bias"`
	Error string  `json:"error"`

	PriceData struct {
		CurrentPrice float64 `json:"current_price"`
	} `json:"price_data"`
}

// parseCoinAnalysis unmarshals the raw tool result into coinAnalysisRaw.
func parseCoinAnalysis(raw json.RawMessage) (coinAnalysisRaw, error) {
	var resp coinAnalysisRaw
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("tv: unmarshal coin_analysis: %w", err)
	}
	return resp, nil
}

// taEmbedFields builds standard TA embed fields from a TAResult.
func taEmbedFields(r ta.TAResult) []*discordgo.MessageEmbedField {
	return []*discordgo.MessageEmbedField{
		{Name: "Price", Value: fmt.Sprintf("$%.4f", r.CurrentPrice), Inline: true},
		{Name: "RSI", Value: fmt.Sprintf("%.2f (%s)", r.RSI, r.RSIZone), Inline: true},
		{Name: "EMA Spread", Value: fmt.Sprintf("%.3f%%", r.EMASpread), Inline: true},
		{Name: "EMA200", Value: fmt.Sprintf("$%.4f", r.EMA200), Inline: true},
		{Name: "Ribbon", Value: r.RibbonStatus, Inline: true},
		{Name: "Support", Value: fmt.Sprintf("$%.4f (%s)", r.NearestSupport, r.SupportStrength), Inline: true},
		{Name: "Resistance", Value: fmt.Sprintf("$%.4f (%s)", r.NearestResistance, r.ResistanceStrength), Inline: true},
	}
}

// handleTV runs the /tv command: TradingView MCP coin_analysis + AI scoring + confirm buttons.
func (b *Bot) handleTV(s *discordgo.Session, i *discordgo.InteractionCreate) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	b.handleTVWithLocalTA(s, i, ctx, coin, interval)
}

// handleTVWithLocalTA fetches OHLCV, calculates TA, scores with AI, and shows results.
func (b *Bot) handleTVWithLocalTA(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, coin, interval string) {
	var taResult ta.TAResult
	var taSource string
	var tvSignal string

	if b.mcpTA != nil {
		taRaw, err := b.mcpTA.CallTool(ctx, "coin_analysis", map[string]any{
			"symbol":    coin + "USDT",
			"exchange":  "Binance",
			"timeframe": interval,
		})
		if err != nil {
			slog.Warn("tv: coin_analysis RPC failed, falling back to local TA", "coin", coin, "err", err)
		} else {
			coinResp, parseErr := parseCoinAnalysis(taRaw)
			if parseErr != nil {
				slog.Warn("tv: coin_analysis parse failed, falling back", "coin", coin, "err", parseErr)
			} else if coinResp.Error != "" {
				slog.Info("tv: coin_analysis returned no data, falling back to local TA", "coin", coin, "tv_error", coinResp.Error)
			} else if coinResp.PriceData.CurrentPrice <= 0 {
				slog.Warn("tv: coin_analysis returned zero price, falling back", "coin", coin, "price", coinResp.PriceData.CurrentPrice)
			} else {
				taResult = mapCoinAnalysisToTA(coinResp)
				taSource = fmt.Sprintf("📡 TradingView MCP · %s", strings.ToUpper(interval))
				tvSignal = coinResp.Bias
			}
		}
	}

	if taSource == "" {
		bars, err := b.fetcher.FetchOHLCV(ctx, coin, interval, 200)
		if err != nil {
			editErrorEmbed(s, i, coin, "Both MCP and local TA failed: "+err.Error())
			return
		}
		taResult, err = ta.Calculate(bars)
		if err != nil {
			editErrorEmbed(s, i, coin, "Local TA failed: "+err.Error())
			return
		}
		taSource = fmt.Sprintf("⚠️ Legacy TA Mode · %s", strings.ToUpper(interval))
	}

	mc, _ := b.fetcher.FetchMarketContext(ctx, coin)
	balance, _ := b.exClient.FetchBalance(ctx)
	lossUSD, winUSD, consecLosses, _ := b.exClient.FetchDailyPnL(ctx)
	state := filter.BotState{
		Balance:           balance,
		DailyLossUSD:      lossUSD,
		DailyWinUSD:       winUSD,
		ConsecutiveLosses: consecLosses,
	}

	var prefilterReason string
	filterResult := filter.ApplyPreFilter(coin, taResult, state)
	if !filterResult.Pass {
		prefilterReason = filterResult.Reason
	}

	score, err := b.scorer.Score(ctx, coin, taResult, mc, state)
	if err != nil {
		editErrorEmbed(s, i, coin, "AI scoring failed: "+err.Error())
		return
	}

	slog.Info("tv: analysis complete",
		"coin", coin, "action", score.Action, "confidence", score.Confidence, "ta_source", taSource,
	)

	isTradeAction := score.Action == "open_long" || score.Action == "open_short"
	embed := buildTVResultEmbed(coin, score, taResult, tvSignal, taSource, prefilterReason)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
	})

	if isTradeAction && score.Confidence >= config.MinConfidence {
		b.sendTVConfirmButtons(s, i, coin, score, taResult)
	}
}

func (b *Bot) executeTVTrade(ctx context.Context, coin string, score aiPkg.ScoreResult, taResult ta.TAResult) {
	positions, err := b.exClient.FetchPositions(ctx)
	if err == nil {
		for _, pos := range positions {
			if pos.Pair == coin {
				b.SendEmbed(&discordgo.MessageEmbed{
					Title:       fmt.Sprintf("⏭️ %s — Already in Position", coin),
					Description: "Open position exists, skipping execution.",
					Color:       ColorBlue,
				})
				return
			}
		}
	}

	side := exchange.OrderSideLong
	if score.Action == "open_short" {
		side = exchange.OrderSideShort
	}

	result, err := b.exClient.PlaceBracketOrder(
		ctx, coin, side, score.PositionSizeUSD,
		taResult.CurrentPrice, score.TakeProfit, score.StopLoss, score.Leverage,
	)
	if err != nil {
		b.SendEmbed(&discordgo.MessageEmbed{
			Title:       fmt.Sprintf("❌ Bracket Order Failed — %s", coin),
			Description: err.Error(), Color: ColorRed,
		})
		return
	}

	b.StartMonitor(ctx, coin, side,
		taResult.CurrentPrice, score.PositionSizeUSD, score.Leverage,
		score.StopLoss, score.TakeProfit, score.Confidence,
		score.Strategy, score.Reasoning, result.OrderID)

	b.SendEmbed(&discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🚀 Trade Executed — %s", coin),
		Description: fmt.Sprintf("**%s** | Entry: $%.4f | Size: $%.2f | %dx\nSL: $%.4f | TP: $%.4f\nStrategy: %s",
			side, taResult.CurrentPrice, score.PositionSizeUSD, score.Leverage,
			score.StopLoss, score.TakeProfit, score.Strategy),
		Color: ColorGreen, Footer: &discordgo.MessageEmbedFooter{Text: "📡 TradingView MCP · PlaceBracketOrder"},
		Timestamp: time.Now().Format(time.RFC3339),
	})

	slog.Info("tv: trade executed", "coin", coin, "side", side, "entry", taResult.CurrentPrice, "size_usd", score.PositionSizeUSD, "order_id", result.OrderID)
}

func (b *Bot) sendTVConfirmButtons(s *discordgo.Session, i *discordgo.InteractionCreate, coin string, score aiPkg.ScoreResult, taResult ta.TAResult) {
	s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
		Embeds: []*discordgo.MessageEmbed{{
			Title:       fmt.Sprintf("▶️ Execute %s %s?", coin, score.Action),
			Description: fmt.Sprintf("**%s** | Confidence: %.0f%% | Size: $%.2f | %dx\nSL: $%.4f | TP: $%.4f",
				score.Action, score.Confidence, score.PositionSizeUSD, score.Leverage,
				score.StopLoss, score.TakeProfit),
			Color: ColorGreen,
		}},
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{Label: "Accept", Style: discordgo.SuccessButton, CustomID: fmt.Sprintf("tv_accept_%s", coin)},
					discordgo.Button{Label: "Decline", Style: discordgo.DangerButton, CustomID: fmt.Sprintf("tv_decline_%s", coin)},
				},
			},
		},
	})
}

// mapCoinAnalysisToTA maps coin_analysis response to internal ta.TAResult.
func mapCoinAnalysisToTA(tv coinAnalysisRaw) ta.TAResult {
	price := tv.PriceData.CurrentPrice
	ema10 := tv.EMA.EMA10
	ema20 := tv.EMA.EMA20
	ema50 := tv.EMA.EMA50
	ema200 := tv.EMA.EMA200
	rsiVal := tv.RSI.Value
	atrVal := tv.ATR.Value

	emaSpread := 0.0
	if price > 0 && ema20 > 0 {
		emaSpread = math.Abs(ema10-ema20) / price * 100
	}

	histogram := tv.MACD.Histogram
	macdCross := "none"
	if histogram > 0 {
		macdCross = "bullish histogram"
	} else if histogram < 0 {
		macdCross = "bearish histogram"
	}

	bbWidth := "unknown"
	bbMiddle := tv.BB.Middle
	if bbMiddle > 0 {
		bw := (tv.BB.Upper - tv.BB.Lower) / bbMiddle * 100
		switch {
		case bw < 3.0:
			bbWidth = "squeeze"
		case bw < 8.0:
			bbWidth = "normal"
		default:
			bbWidth = "expanded"
		}
	}

	bbPosition := "at middle band"
	switch {
	case price > tv.BB.Upper:
		bbPosition = "above upper band"
	case price < tv.BB.Lower:
		bbPosition = "below lower band"
	case price >= tv.BB.Upper*0.98:
		bbPosition = "at upper band"
	case price <= tv.BB.Lower*1.02:
		bbPosition = "at lower band"
	}

	atrLevel := "unknown"
	if price > 0 {
		pct := atrVal / price * 100
		switch {
		case pct < 1.5:
			atrLevel = "low"
		case pct < 3.0:
			atrLevel = "medium"
		default:
			atrLevel = "high"
		}
	}

	return ta.TAResult{
		CurrentPrice:     price,
		EMA9:             ema10,
		EMA21:            ema20,
		EMA50:            ema50,
		EMA200:           ema200,
		EMASpread:        emaSpread,
		RibbonStatus:     tvRibbon(price, ema10, ema20, ema50, ema200),
		DailyTrend:       tvDailyTrend(ema50, ema200),
		RSI:              rsiVal,
		RSIZone:          tvRSIZone(rsiVal),
		ATR:              atrVal,
		ATRLevel:         atrLevel,
		SL1ATR:           price - atrVal,
		SL15ATR:          price - atrVal*1.5,
		MACDValue:        tv.MACD.MacdLine,
		MACDSignal:       tv.MACD.SignalLine,
		MACDHistogram:    histogram,
		MACDCross:        macdCross,
		BBUpper:          tv.BB.Upper,
		BBMiddle:         bbMiddle,
		BBLower:          tv.BB.Lower,
		BBWidth:          bbWidth,
		BBPosition:       bbPosition,
		VolumeMultiplier: 1.0,
		VolumeConfirms:   false,
	}
}

func buildTVResultEmbed(coin string, score aiPkg.ScoreResult, taResult ta.TAResult, tvSignal, taSource, prefilterReason string) *discordgo.MessageEmbed {
	var actionColor int
	switch score.Action {
	case "open_long":
		actionColor = ColorGreen
	case "open_short":
		actionColor = ColorRed
	case "wait", "hold":
		actionColor = ColorYellow
	default:
		actionColor = ColorBlue
	}

	actionEmoji := map[string]string{
		"open_long": "📈 LONG", "open_short": "📉 SHORT",
		"hold": "⏸️ HOLD", "wait": "⏳ WAIT",
	}[score.Action]
	if actionEmoji == "" {
		actionEmoji = score.Action
	}

	tvSignalLine := ""
	if tvSignal != "" {
		tvSignalLine = fmt.Sprintf("\n**TradingView Bias:** %s", tvSignal)
	}

	description := fmt.Sprintf("## %s — %s\n%s\n\n**Confidence:** %.0f%% | **R:R:** %.2f | **Confluence:** %d/9\n**Strategy:** %s\n\n**Entry:** $%.4f | **Size:** $%.2f | **Leverage:** %dx\n**SL:** $%.4f | **TP:** $%.4f",
		coin, actionEmoji, tvSignalLine,
		score.Confidence, score.RRRatio, score.ConfluenceCount, score.Strategy,
		taResult.CurrentPrice, score.PositionSizeUSD, score.Leverage,
		score.StopLoss, score.TakeProfit,
	)

	if prefilterReason != "" {
		description += fmt.Sprintf("\n\n⚠️ **Pre-filter warning:** %s", prefilterReason)
	}
	if score.Reasoning != "" {
		description += fmt.Sprintf("\n\n```\n%s\n```", score.Reasoning)
	}

	return &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🔍 %s Analysis", coin),
		Description: description,
		Color:       actionColor,
		Fields:      taEmbedFields(taResult),
		Footer:      &discordgo.MessageEmbedFooter{Text: taSource},
		Timestamp:   time.Now().Format(time.RFC3339),
	}
}

func editErrorEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, coin, msg string) {
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{{
			Title:       fmt.Sprintf("❌ Error — %s", coin),
			Description: msg, Color: ColorRed,
			Timestamp: time.Now().Format(time.RFC3339),
		}},
	})
}

func tvRibbon(price, ema10, ema20, ema50, ema200 float64) string {
	switch {
	case price > ema10 && ema10 > ema20 && ema20 > ema50 && ema50 > ema200:
		return "fully aligned bullish"
	case price < ema10 && ema10 < ema20 && ema20 < ema50 && ema50 < ema200:
		return "fully aligned bearish"
	case price > ema20 && price > ema50:
		return "partial bullish"
	case price < ema20 && price < ema50:
		return "partial bearish"
	default:
		return "mixed"
	}
}

func tvDailyTrend(ema50, ema200 float64) string {
	if ema50 > ema200 {
		return "Golden Cross (bullish macro)"
	}
	if ema50 < ema200 {
		return "Death Cross (bearish macro)"
	}
	return "Neutral"
}

func tvRSIZone(rsi float64) string {
	switch {
	case rsi > 70:
		return "overbought >70"
	case rsi < 30:
		return "oversold <30"
	case rsi >= 40 && rsi <= 60:
		return "valid 40-60"
	default:
		return "valid 30-70"
	}
}
