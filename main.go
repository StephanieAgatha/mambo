package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	aiPkg "mambo/ai"
	"mambo/config"
	"mambo/discord"
	"mambo/exchange"
	"mambo/journal"
	"mambo/logger"
	"mambo/market"
	"mambo/monitor"
)

func main() {
	slog.SetDefault(logger.New())
	fmt.Println("Starting Mambo Discord Bot...")

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	jl, err := journal.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "journal error: %v\n", err)
		os.Exit(1)
	}

	openPositions, err := jl.LoadPositions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load positions error: %v\n", err)
		os.Exit(1)
	}

	exClient, err := exchange.New(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "exchange error: %v\n", err)
		os.Exit(1)
	}

	fetcher := market.New(ctx, cfg)

	scorer, err := aiPkg.NewScorer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scorer error: %v\n", err)
		os.Exit(1)
	}

	posMgr, err := aiPkg.NewPositionManager(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "position manager error: %v\n", err)
		os.Exit(1)
	}

	discordBot, err := discord.New(cfg, scorer, fetcher, jl, exClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discord error: %v\n", err)
		os.Exit(1)
	}
	defer discordBot.Close()

	// monitor callbacks → Discord
	onClose := func(pos journal.OpenPosition, exitPrice float64, reason monitor.CloseReason, pnlUSD, pnlPct float64) {
		discordBot.NotifyPositionClosed(pos.Pair, string(pos.Direction), pos.EntryPrice, exitPrice, pnlUSD, pnlPct, string(reason), pos.Strategy)
	}

	onUpdate := func(pos journal.OpenPosition, action string, newSL, newTP float64, reasoning string) {
		discordBot.NotifyPositionUpdated(pos.Pair, action, newSL, newTP, reasoning)
	}

	mon := monitor.New(exClient, fetcher, posMgr, jl, cfg, onClose, onUpdate)
	for _, pos := range openPositions {
		mon.Start(ctx, pos)
	}

	fmt.Println("Mambo Discord Bot is running. Press Ctrl+C to stop.")
	discordBot.SendEmbed(&discordgo.MessageEmbed{
		Title:       "🟢 Mambo is online",
		Description: "Bot started. All systems nominal.",
		Color:       discord.ColorGreen,
		Timestamp:   time.Now().Format(time.RFC3339),
	})

	<-quit
	fmt.Println("\nShutting down...")
	cancel()
}
