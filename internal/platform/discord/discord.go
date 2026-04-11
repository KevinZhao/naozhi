package discord

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
	cfg        Config
	session    *discordgo.Session
	handler    platform.MessageHandler
	startMu    sync.Mutex
	started    bool
	botID      string
	stopCtx    context.Context
	stopCancel context.CancelFunc
	handlerWg  sync.WaitGroup
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

func (d *Discord) SupportsInterimMessages() bool { return true }

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

	ctx, cancel := context.WithCancel(context.Background())
	d.stopCtx = ctx
	d.stopCancel = cancel

	sess, err := discordgo.New("Bot " + d.cfg.BotToken)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}

	sess.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	sess.AddHandler(d.onMessageCreate)

	// Assign session BEFORE Open() so handlers don't hit nil d.session.
	// If Open() fails, nil it out.
	d.session = sess

	if err := sess.Open(); err != nil {
		d.session = nil
		return fmt.Errorf("open discord gateway: %w", err)
	}

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
	if d.stopCancel != nil {
		d.stopCancel()
	}
	if d.session != nil {
		if err := d.session.Close(); err != nil {
			return fmt.Errorf("close discord session: %w", err)
		}
	}
	done := make(chan struct{})
	go func() { d.handlerWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("discord: timed out waiting for handler goroutines")
	}
	return nil
}

// Reply sends a message to a Discord channel. Handles text and/or images.
func (d *Discord) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	// If images, send as file attachments
	if len(msg.Images) > 0 {
		var files []*discordgo.File
		for i, img := range msg.Images {
			ext := ".png"
			switch img.MimeType {
			case "image/jpeg":
				ext = ".jpg"
			case "image/gif":
				ext = ".gif"
			case "image/webp":
				ext = ".webp"
			}
			files = append(files, &discordgo.File{
				Name:        fmt.Sprintf("image_%d%s", i, ext),
				ContentType: img.MimeType,
				Reader:      bytes.NewReader(img.Data),
			})
		}
		ms := &discordgo.MessageSend{
			Content: msg.Text,
			Files:   files,
		}
		m, err := d.session.ChannelMessageSendComplex(msg.ChatID, ms, discordgo.WithContext(ctx))
		if err != nil {
			return "", fmt.Errorf("discord send with images: %w", err)
		}
		return msg.ChatID + ":" + m.ID, nil
	}

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

	// Collect image attachment metadata; download happens asynchronously
	type pendingImage struct {
		url         string
		contentType string
	}
	var pending []pendingImage
	for _, att := range m.Attachments {
		if !isImageContentType(att.ContentType) {
			continue
		}
		pending = append(pending, pendingImage{url: att.URL, contentType: att.ContentType})
	}

	if text == "" && len(pending) == 0 {
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

	// Download images in the async goroutine, not in discordgo's event dispatch
	d.handlerWg.Add(1)
	go func() {
		defer d.handlerWg.Done()
		defer platform.RecoverHandler("discord")
		for _, p := range pending {
			data, mime, err := downloadURL(p.url)
			if err != nil {
				slog.Warn("discord download attachment failed", "err", err, "url", p.url)
				continue
			}
			msg.Images = append(msg.Images, platform.Image{Data: data, MimeType: mime})
		}
		d.handler(d.stopCtx, msg)
	}()
}

func isImageContentType(ct string) bool {
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

var discordHTTPClient = &http.Client{Timeout: 15 * time.Second}

// discordCDNHosts is the set of trusted Discord CDN domains for attachment downloads.
var discordCDNHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

func downloadURL(rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("invalid attachment URL: %w", err)
	}
	if !discordCDNHosts[u.Hostname()] {
		return nil, "", fmt.Errorf("attachment URL host not in whitelist: %s", u.Hostname())
	}
	resp, err := discordHTTPClient.Get(rawURL)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, "", err
	}
	ct := stripMIMEParams(resp.Header.Get("Content-Type"))
	if ct == "" {
		ct = "image/png"
	}
	return data, ct, nil
}

func stripMIMEParams(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}
