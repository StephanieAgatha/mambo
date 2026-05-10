package discord

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Embed color constants — always use these, never raw hex strings
const (
	ColorGreen  = 0x00C853 // WIN, EXECUTE, TP hit, order placed
	ColorRed    = 0xD50000 // LOSS, ABORT, SL hit, daily limit hit
	ColorYellow = 0xFFD600 // signal found (manual mode)
	ColorBlue   = 0x2979FF // position updated, info commands
)

// motivational quotes for embed footers on trade events
var quotes = []string{
	"I do not guess. I do not hope. I see my setup, I execute, I accept the outcome.",
	"Discipline is the bridge between goals and accomplishment.",
	"The market is a device for transferring money from the impatient to the patient.",
	"Risk comes from not knowing what you're doing.",
	"In trading, you have to be defensive and aggressive at the same time.",
	"The goal of a successful trader is to make the best trades. Money is secondary.",
	"Don't focus on making money; focus on protecting what you have.",
	"Greed is my only enemy.",
}

func randomQuote() string {
	return quotes[rand.Intn(len(quotes))]
}

// notifyTradeExecuted sends a green embed when a trade is opened (auto mode).
func (b *Bot) NotifyTradeExecuted(pair, side string, entryPrice, sizeUSD float64, leverage int, confidence float64, strategy, reasoning string) {
	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🚀 %s %s EXECUTED", pair, side),
		Description: fmt.Sprintf("**%s**", reasoning),
		Color:       ColorGreen,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Entry Price", Value: fmt.Sprintf("$%.4f", entryPrice), Inline: true},
			{Name: "Size", Value: fmt.Sprintf("$%.2f", sizeUSD), Inline: true},
			{Name: "Leverage", Value: fmt.Sprintf("%dx cross", leverage), Inline: true},
			{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", confidence), Inline: true},
			{Name: "Strategy", Value: strategy, Inline: true},
		},
		Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	b.SendEmbed(embed)
}

// notifyTradeAborted sends a red embed when AI aborts a trade.
func (b *Bot) NotifyTradeAborted(pair, reason string) {
	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("❌ %s ABORTED", pair),
		Description: reason,
		Color:       ColorRed,
		Footer:      &discordgo.MessageEmbedFooter{Text: randomQuote()},
		Timestamp:   time.Now().Format(time.RFC3339),
	}
	b.SendEmbed(embed)
}

// notifySignalFound sends a yellow embed when AI finds a signal (manual mode only).
func (b *Bot) NotifySignalFound(pair, direction string, entryPrice, confidence float64, signals string, strategy string) {
	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🔔 %s SIGNAL — %s", pair, direction),
		Description: fmt.Sprintf("**Signals**: %s\nUse `/execute %s` to execute", signals, pair),
		Color:       ColorYellow,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Entry", Value: fmt.Sprintf("$%.4f", entryPrice), Inline: true},
			{Name: "Confidence", Value: fmt.Sprintf("%.0f%%", confidence), Inline: true},
			{Name: "Strategy", Value: strategy, Inline: true},
		},
		Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	b.SendEmbed(embed)
}

// notifyPositionClosed sends a green or red embed when a position is closed.
func (b *Bot) NotifyPositionClosed(pair, direction string, entryPrice, exitPrice, pnlUSD, pnlPct float64, reason, strategy string) {
	result := "WIN"
	color := ColorGreen
	emoji := "✅"
	if pnlUSD < 0 {
		result = "LOSS"
		color = ColorRed
		emoji = "🔴"
	}

	pnlLabel := fmt.Sprintf("$%.2f", pnlUSD)
	if pnlUSD > 0 {
		pnlLabel = "+" + pnlLabel
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("%s %s CLOSED — %s", emoji, pair, result),
		Description: fmt.Sprintf("**%s** %s", strategy, direction),
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Entry", Value: fmt.Sprintf("$%.4f", entryPrice), Inline: true},
			{Name: "Exit", Value: fmt.Sprintf("$%.4f", exitPrice), Inline: true},
			{Name: "PnL", Value: fmt.Sprintf("%s (%.2f%%)", pnlLabel, pnlPct), Inline: true},
			{Name: "Reason", Value: string(reason), Inline: true},
		},
		Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	b.SendEmbed(embed)
}

// notifyPositionUpdated sends a blue embed when AI adjusts SL or TP.
func (b *Bot) NotifyPositionUpdated(pair, action string, newSL, newTP float64, reasoning string) {
	var title string
	if action == "move_sl" {
		title = fmt.Sprintf("🔄 %s SL MOVED", pair)
	} else {
		title = fmt.Sprintf("🔄 %s TP MOVED", pair)
	}

	embed := &discordgo.MessageEmbed{
		Title:     title,
		Color:     ColorBlue,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Reasoning", Value: reasoning, Inline: false},
		},
		Footer:    &discordgo.MessageEmbedFooter{Text: randomQuote()},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	b.SendEmbed(embed)
}

// notifyDailyLimit sends a red embed when daily loss or win limit is hit.
func (b *Bot) NotifyDailyLimit(reason string) {
	embed := &discordgo.MessageEmbed{
		Title:       "🛑 TRADING HALTED",
		Description: reason,
		Color:       ColorRed,
		Footer:      &discordgo.MessageEmbedFooter{Text: "All trading paused until next day."},
		Timestamp:   time.Now().Format(time.RFC3339),
	}
	b.SendEmbed(embed)
}
