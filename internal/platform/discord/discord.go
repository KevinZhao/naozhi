package discord

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/naozhi/naozhi/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// Config holds Discord bot credentials.
type Config struct {
	BotToken    string
	MaxReplyLen int
}

// Discord implements Platform and RunnablePlatform via WebSocket gateway.
type Discord struct {
	cfg     Config
	session *discordgo.Session
	handler platform.MessageHandler
	startMu sync.Mutex
	started bool
	botID   string
}

// New creates a Discord platform adapter.
func New(cfg Config) *Discord {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = 2000 // Discord's actual limit
	}
	return &Discord{cfg: cfg}
}

func (d *Discord) Name() string { return "discord" }

func (d *Discord) MaxReplyLength() int { return d.cfg.MaxReplyLen }

// RegisterRoutes is a no-op for Discord (WebSocket gateway, no inbound HTTP).
func (d *Discord) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

// Start implements RunnablePlatform. Opens Discord WebSocket gateway.
// Note: IntentMessageContent is a privileged intent that must be enabled
// in the Discord Developer Portal under "Privileged Gateway Intents".
func (d *Discord) Start(handler platform.MessageHandler) error {
	d.startMu.Lock()
	if d.started {
		d.startMu.Unlock()
		return fmt.Errorf("discord platform already started")
	}
	d.started = true
	d.startMu.Unlock()

	d.handler = handler

	sess, err := discordgo.New("Bot " + d.cfg.BotToken)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}

	sess.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	sess.AddHandler(d.onMessageCreate)

	if err := sess.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}

	d.session = sess

	if sess.State != nil && sess.State.User != nil {
		d.botID = sess.State.User.ID
		slog.Info("discord gateway connected", "bot_id", d.botID, "bot_name", sess.State.User.Username)
	} else {
		slog.Warn("discord gateway connected but bot identity unavailable")
	}

	return nil
}

// Stop implements RunnablePlatform. Closes Discord WebSocket gateway.
func (d *Discord) Stop() error {
	if d.session != nil {
		if err := d.session.Close(); err != nil {
			return fmt.Errorf("close discord session: %w", err)
		}
	}
	return nil
}

// Reply sends a message to a Discord channel.
func (d *Discord) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	m, err := d.session.ChannelMessageSend(msg.ChatID, msg.Text, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("discord send: %w", err)
	}
	return msg.ChatID + ":" + m.ID, nil
}

// EditMessage updates an existing Discord message.
func (d *Discord) EditMessage(ctx context.Context, msgID string, text string) error {
	parts := strings.SplitN(msgID, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid discord msgID format: %q", msgID)
	}
	if _, err := d.session.ChannelMessageEdit(parts[0], parts[1], text, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("discord edit message %s: %w", msgID, err)
	}
	return nil
}

func (d *Discord) onMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil {
		return
	}
	if m.Author.ID == d.botID {
		return
	}
	if m.Author.Bot {
		return
	}

	text := m.Content
	mentionMe := false

	for _, u := range m.Mentions {
		if u.ID == d.botID {
			mentionMe = true
			text = strings.ReplaceAll(text, "<@"+d.botID+">", "")
			text = strings.ReplaceAll(text, "<@!"+d.botID+">", "")
			break
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	chatType := "direct"
	if m.GuildID != "" {
		chatType = "group"
	}

	msg := platform.IncomingMessage{
		Platform:  "discord",
		EventID:   m.ID,
		UserID:    m.Author.ID,
		ChatID:    m.ChannelID,
		ChatType:  chatType,
		Text:      text,
		MentionMe: mentionMe,
	}

	go d.handler(context.Background(), msg)
}
