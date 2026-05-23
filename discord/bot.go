package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"

	"mambo/config"
	"mambo/exchange"
	"mambo/journal"
	"mambo/market"
	"mambo/monitor"
	aiPkg "mambo/ai"
)

// Bot holds all the dependencies needed by Discord slash commands.
type Bot struct {
	session  *discordgo.Session
	cfg      *config.Config
	scorer   *aiPkg.Scorer
	fetcher  *market.Fetcher
	jl       *journal.Logger
	exClient *exchange.Client
	mon      *monitor.Monitor
	mode     string // "auto" | "manual"

	scanMu        sync.Mutex
	activeScan    *scanResult // non-nil = waiting for user accept/decline
	scanMessageID string      // the message with buttons
	scanChannelID string      // channel where buttons were sent
}

// hasAuthorizedRole checks if a Discord member has the authorized role.
// Returns false silently — no response to unauthorized users.
func hasAuthorizedRole(member *discordgo.Member, authorizedRoleID string) bool {
	if authorizedRoleID == "" || member == nil {
		return false
	}
	for _, roleID := range member.Roles {
		if roleID == authorizedRoleID {
			return true
		}
	}
	return false
}

// New creates a Discord bot, opens the WebSocket connection, and registers slash commands.
func New(
	cfg *config.Config,
	scorer *aiPkg.Scorer,
	fetcher *market.Fetcher,
	jl *journal.Logger,
	exClient *exchange.Client,
	mon *monitor.Monitor,
) (*Bot, error) {
	if cfg.DiscordBotToken == "" {
		return nil, fmt.Errorf("discord: DISCORD_BOT_TOKEN not set")
	}

	session, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		return nil, fmt.Errorf("discord: create session: %w", err)
	}

	session.Identify.Intents = discordgo.IntentsGuildMessages

	if err := session.Open(); err != nil {
		return nil, fmt.Errorf("discord: open websocket: %w", err)
	}

	bot := &Bot{
		session:  session,
		cfg:      cfg,
		scorer:   scorer,
		fetcher:  fetcher,
		jl:       jl,
		exClient: exClient,
		mon:      mon,
		mode:     "auto",
	}

	if err := bot.registerCommands(); err != nil {
		return nil, fmt.Errorf("discord: register commands: %w", err)
	}

	slog.Info("discord bot connected",
		"guild_id", cfg.DiscordGuildID,
		"channel_id", cfg.DiscordChannelID,
		"username", session.State.User.Username,
	)

	return bot, nil
}

// GetMode returns the current trading mode.
func (b *Bot) GetMode() string {
	return b.mode
}

// SetMonitor wires the position monitor into the bot for auto-starting on /execute.
func (b *Bot) SetMonitor(mon *monitor.Monitor) {
	b.mon = mon
}

// StartMonitor saves the position and spawns the monitor goroutine.
func (b *Bot) StartMonitor(ctx context.Context, pair string, direction exchange.OrderSide, entryPrice, sizeUSD float64, leverage int, stopLoss, takeProfit, confidence float64, strategy, aiReason string, orderID uint64) {
	if b.mon == nil {
		slog.Warn("discord: monitor not wired — position not monitored", "pair", pair)
		return
	}

	pos := journal.OpenPosition{
		ID:         journal.GeneratePositionID(),
		Pair:       pair,
		Direction:  direction,
		EntryPrice: entryPrice,
		SizeUSD:    sizeUSD,
		Leverage:   leverage,
		StopLoss:   stopLoss,
		TakeProfit: takeProfit,
		Confidence: confidence,
		Strategy:   strategy,
		AIReason:   aiReason,
		OrderID:    orderID,
		OpenedAt:   journal.Now(),
		Status:     "open",
	}

	if err := b.jl.SavePosition(pos); err != nil {
		slog.Error("discord: save position failed", "pos_id", pos.ID, "pair", pair, "err", err)
		return
	}

	b.mon.Start(ctx, pos)

	slog.Info("discord: position monitor started",
		"pos_id", pos.ID,
		"pair", pair,
		"direction", direction,
		"entry", entryPrice,
		"sl", stopLoss,
		"tp", takeProfit,
	)
}

// Close gracefully shuts down the Discord session.
func (b *Bot) Close() {
	if b.session != nil {
		b.session.Close()
		slog.Info("discord bot disconnected")
	}
}

// SendEmbed sends an embed message to the configured Discord channel.
func (b *Bot) SendEmbed(embed *discordgo.MessageEmbed) {
	if b.session == nil || b.cfg.DiscordChannelID == "" {
		return
	}
	_, err := b.session.ChannelMessageSendEmbed(b.cfg.DiscordChannelID, embed)
	if err != nil {
		slog.Warn("discord: failed to send embed", "err", err)
	}
}

// SendDM sends a private embed message to a specific user.
func (b *Bot) SendDM(userID string, embed *discordgo.MessageEmbed) {
	if b.session == nil || userID == "" {
		return
	}
	ch, err := b.session.UserChannelCreate(userID)
	if err != nil {
		slog.Warn("discord: failed to create DM channel", "user_id", userID, "err", err)
		return
	}
	_, err = b.session.ChannelMessageSendEmbed(ch.ID, embed)
	if err != nil {
		slog.Warn("discord: failed to send DM", "user_id", userID, "err", err)
	}
}

// registration creates all slash commands on the configured guild.
func (b *Bot) registerCommands() error {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "check",
			Description: "TA + AI analysis for a coin",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "coin",
					Description: "Ticker symbol (e.g. SOL, BTC, ETH)",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    true,
				},
				{
					Name:        "interval",
					Description: "Timeframe for analysis",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "1m", Value: "1m"},
						{Name: "5m", Value: "5m"},
						{Name: "15m", Value: "15m"},
						{Name: "30m", Value: "30m"},
						{Name: "1h", Value: "1h"},
						{Name: "4h", Value: "4h"},
						{Name: "1d", Value: "1d"},
						{Name: "1w", Value: "1w"},
					},
				},
			},
		},
		{
			Name:        "status",
			Description: "Show open positions + live PnL",
		},
		{
			Name:        "journal",
			Description: "Show trade history from exchange",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "range",
					Description: "Time range (today or week)",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "today", Value: "today"},
						{Name: "week", Value: "week"},
					},
				},
				{
					Name:        "page",
					Description: "Page number (10 trades per page, default: 1)",
					Type:        discordgo.ApplicationCommandOptionInteger,
					Required:    false,
				},
			},
		},
		{
			Name:        "pnl",
			Description: "Total PnL + win rate",
		},
		{
			Name:        "mode",
			Description: "Toggle trading mode (auto / manual)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "mode",
					Description: "Trading mode",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "auto", Value: "auto"},
						{Name: "manual", Value: "manual"},
					},
				},
			},
		},
		{
			Name:        "capital",
			Description: "Show balance + limits + remaining budget",
		},
		{
			Name:        "pairs",
			Description: "Show active trading pairs",
		},
		{
			Name:        "scan",
			Description: "Scan random pairs with AI and get a setup (pause+confirm flow)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "interval",
					Description: "Timeframe for scanning",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "1m", Value: "1m"},
						{Name: "5m", Value: "5m"},
						{Name: "15m", Value: "15m"},
						{Name: "30m", Value: "30m"},
						{Name: "1h", Value: "1h"},
						{Name: "4h", Value: "4h"},
						{Name: "1d", Value: "1d"},
						{Name: "1w", Value: "1w"},
					},
				},
			},
		},
		{
			Name:        "hunt",
			Description: "Auto-scan + execute + monitor — full autonomous trade hunt",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "interval",
					Description: "Timeframe for scanning",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "1m", Value: "1m"},
						{Name: "5m", Value: "5m"},
						{Name: "15m", Value: "15m"},
						{Name: "30m", Value: "30m"},
						{Name: "1h", Value: "1h"},
						{Name: "4h", Value: "4h"},
						{Name: "1d", Value: "1d"},
						{Name: "1w", Value: "1w"},
					},
				},
			},
		},
		{
			Name:        "verify",
			Description: "Compare OHLCV candle from Hyperliquid vs Altfins",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "coin",
					Description: "Ticker symbol (e.g. SOL, BTC, ETH)",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    true,
				},
				{
					Name:        "interval",
					Description: "Timeframe (Altfins: DAILY/HOURLY/WEEKLY/MONTHLY)",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "HOURLY", Value: "HOURLY"},
						{Name: "DAILY", Value: "DAILY"},
						{Name: "WEEKLY", Value: "WEEKLY"},
						{Name: "MONTHLY", Value: "MONTHLY"},
					},
				},
			},
		},
		{
			Name:        "execute",
			Description: "Execute a trade — with or without AI analysis",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "coin",
					Description: "Ticker symbol (e.g. SOL, BTC, ETH)",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    true,
				},
				{
					Name:        "leverage",
					Description: "Leverage (1-10x cross). Required for bypass mode.",
					Type:        discordgo.ApplicationCommandOptionInteger,
					Required:    true,
					MinValue:    func() *float64 { v := 1.0; return &v }(),
					MaxValue:    10,
				},
				{
					Name:        "interval",
					Description: "Timeframe for analysis",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "1m", Value: "1m"},
						{Name: "5m", Value: "5m"},
						{Name: "15m", Value: "15m"},
						{Name: "30m", Value: "30m"},
						{Name: "1h", Value: "1h"},
						{Name: "4h", Value: "4h"},
						{Name: "1d", Value: "1d"},
						{Name: "1w", Value: "1w"},
					},
				},
				{
					Name:        "bypass",
					Description: "Skip prefilter + AI — execute immediately with your params (true/false)",
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Required:    false,
				},
				{
					Name:        "tp",
					Description: "Take-profit price. Required when bypass=true.",
					Type:        discordgo.ApplicationCommandOptionNumber,
					Required:    false,
				},
				{
					Name:        "sl",
					Description: "Stop-loss price. Required when bypass=true.",
					Type:        discordgo.ApplicationCommandOptionNumber,
					Required:    false,
				},
			},
		},
	}

	b.session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}

		if !hasAuthorizedRole(i.Member, b.cfg.DiscordAuthorizedRoleID) {
			return
		}

		switch i.ApplicationCommandData().Name {
		case "check":
			b.handleCheck(s, i)
		case "status":
			b.handleStatus(s, i)
		case "journal":
			b.handleJournal(s, i)
		case "pnl":
			b.handlePnl(s, i)
		case "mode":
			b.handleMode(s, i)
		case "capital":
			b.handleCapital(s, i)
		case "pairs":
			b.handlePairs(s, i)
		case "hunt":
			b.handleHunt(s, i)
		case "scan":
			b.handleScan(s, i)
		case "execute":
			b.handleExecute(s, i)
		case "verify":
			b.handleVerify(s, i)
		}
	})

	// Handle message component interactions (accept/decline buttons from /scan)
	b.session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionMessageComponent {
			return
		}
		if !hasAuthorizedRole(i.Member, b.cfg.DiscordAuthorizedRoleID) {
			return
		}
		switch i.MessageComponentData().CustomID {
		case "scan_accept":
			b.handleScanAccept(s, i)
		case "scan_decline":
			b.handleScanDecline(s, i)
		case "suggest_execute":
			b.handleSuggestExecute(s, i)
		case "suggest_skip":
			b.handleSuggestSkip(s, i)
		}
	})

	// Guild-specific commands for faster registration (no 1-hour global cache)
	_, err := b.session.ApplicationCommandBulkOverwrite(
		b.session.State.User.ID,
		b.cfg.DiscordGuildID,
		commands,
	)
	if err != nil {
		return fmt.Errorf("discord: bulk overwrite commands: %w", err)
	}

	slog.Info("discord slash commands registered", "count", len(commands))
	return nil
}