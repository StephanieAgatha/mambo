package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// handleVerify fetches the latest OHLCV candle from both Hyperliquid and Altfins
// and shows a side-by-side comparison with deviation percentages.
func (b *Bot) handleVerify(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	coin := strings.ToUpper(data.Options[0].StringValue())
	interval := "1d"
	for _, opt := range data.Options {
		if opt.Name == "interval" {
			interval = opt.StringValue()
		}
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	go func() {
		ctx := context.Background()

		// map mambo interval to Altfins timeInterval
		altfinsIntvl := mapMamboToAltfins(interval)

		// Fetch from Hyperliquid (last candle)
		hlBars, err := b.fetcher.FetchOHLCV(ctx, coin, interval, 1)
		if err != nil {
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{{
					Title:       fmt.Sprintf("❌ Verify — %s", coin),
					Description: fmt.Sprintf("Hyperliquid fetch failed: %s", err.Error()),
					Color:       ColorRed,
				}},
			})
			return
		}

		hlBar := hlBars[0]

		// Fetch from Altfins
		afBar, afErr := b.fetcher.FetchAltfinsSnapshot(ctx, coin, altfinsIntvl)

		title := fmt.Sprintf("🔍 Verify — %s (%s)", coin, interval)
		color := ColorBlue
		desc := ""

		if afErr != nil {
			desc = fmt.Sprintf("**Altfins**: ⚠️ %s\n\n", afErr.Error())
			color = ColorYellow
		} else {
			oDev := pctDiff(float64(afBar.Open), hlBar.Open)
			hDev := pctDiff(float64(afBar.High), hlBar.High)
			lDev := pctDiff(float64(afBar.Low), hlBar.Low)
			cDev := pctDiff(float64(afBar.Close), hlBar.Close)
			vDev := pctDiff(float64(afBar.Volume), hlBar.Volume)

			maxDev := oDev
			for _, d := range []float64{hDev, lDev, cDev} {
				if d < 0 {
					d = -d
				}
				if d > maxDev {
					maxDev = d
				}
			}

			if maxDev > 3 {
				title = fmt.Sprintf("⚠️ Verify — %s (%s) — DIVERGENCE %.1f%%", coin, interval, maxDev)
				color = ColorRed
			} else if maxDev > 1 {
				title = fmt.Sprintf("🟡 Verify — %s (%s) — %.1f%% deviation", coin, interval, maxDev)
				color = ColorYellow
			} else {
				title = fmt.Sprintf("✅ Verify — %s (%s) — %.1f%% deviation", coin, interval, maxDev)
				color = ColorGreen
			}

			desc = fmt.Sprintf("```\n"+
				"        ALT S   │ HYPERLIQUID │  DEV %%  \n"+
				"───────┼─────────────┼───────────\n"+
				"Open  │ %12.4f │ %11.4f │ %+6.2f%%\n"+
				"High  │ %12.4f │ %11.4f │ %+6.2f%%\n"+
				"Low   │ %12.4f │ %11.4f │ %+6.2f%%\n"+
				"Close │ %12.4f │ %11.4f │ %+6.2f%%\n"+
				"Vol   │ %12.2f │ %11.2f │ %+6.2f%%\n"+
				"```\n"+
				"**Altfins candle**: %s\n"+
				"**Hyperliquid candle**: %s",
				afBar.Open, hlBar.Open, oDev,
				afBar.High, hlBar.High, hDev,
				afBar.Low, hlBar.Low, lDev,
				afBar.Close, hlBar.Close, cDev,
				afBar.Volume, hlBar.Volume, vDev,
				afBar.Time, hlBar.OpenTime.Format("2006-01-02 15:04"),
			)
		}

		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Title:       title,
				Description: desc,
				Color:       color,
				Timestamp:   time.Now().Format(time.RFC3339),
			}},
		})
	}()
}

// mapMamboToAltfins converts mambo timeframe to Altfins timeInterval.
// Altfins only supports: DAILY, WEEKLY, MONTHLY, HOURLY.
func mapMamboToAltfins(interval string) string {
	switch interval {
	case "1d":
		return "DAILY"
	case "1w":
		return "WEEKLY"
	case "1m", "5m", "15m", "30m", "1h", "4h":
		return "HOURLY"
	default:
		return "HOURLY"
	}
}

// pctDiff returns the percentage difference: (a - b) / b * 100.
func pctDiff(altfinsVal, hlVal float64) float64 {
	if hlVal == 0 {
		return 0
	}
	return (altfinsVal - hlVal) / hlVal * 100
}
