package discord

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"

	"mambo/config"
	"mambo/exchange"
	"mambo/journal"
	"mambo/market"
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
	mode     string // "auto" | "manual"
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
			},
		},
		{
			Name:        "status",
			Description: "Show open positions + live PnL",
		},
		{
			Name:        "journal",
			Description: "Show trade history",
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
			Description: "Scan the market for a good trade setup",
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
					Name:        "bypass",
					Description: "Skip prefilter + AI — execute immediately with defaults (true/false)",
					Type:        discordgo.ApplicationCommandOptionBoolean,
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
		case "scan":
			b.handleScan(s, i)
		case "execute":
			b.handleExecute(s, i)
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