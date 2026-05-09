package discord

import (
	"bytes"
	"context"
	"errors"
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
	// NewTimer + Stop: fast path (handlers exit cleanly) must not leave a
	// 30s timer goroutine parked until the timeout elapses.
	timer := time.NewTimer(30 * time.Second)
	select {
	case <-done:
		timer.Stop()
	case <-timer.C:
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
			ext := platform.ImageExt(img.MimeType)
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

// reactionEmoji maps platform-agnostic ReactionType to a Discord unicode
// emoji. Discord's reaction API accepts raw unicode strings directly — no
// aliasing needed. Empty return means unsupported.
func reactionEmoji(r platform.ReactionType) string {
	switch r {
	case platform.ReactionQueued:
		return "\u23F3" // ⏳ hourglass
	}
	return ""
}

// AddReaction implements platform.Reactor. messageID is our composite
// "channel:msg" format. We use the bot's own identity (@me) to add on behalf
// of the bot account.
func (d *Discord) AddReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return fmt.Errorf("discord AddReaction: empty messageID")
	}
	emoji := reactionEmoji(r)
	if emoji == "" {
		return fmt.Errorf("discord AddReaction: unsupported reaction %q", r)
	}
	parts := strings.SplitN(messageID, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid discord msgID format: %q", messageID)
	}
	if err := d.session.MessageReactionAdd(parts[0], parts[1], emoji, discordgo.WithContext(ctx)); err != nil {
		// AddReaction is idempotent from the platform's perspective; swallow
		// discord's "unknown emoji" / "already reacted" variants so dispatch
		// does not fall back to a text notice on a queue-drain retry. This
		// mirrors slack.go which already swallows "already_reacted".
		var restErr *discordgo.RESTError
		if errors.As(err, &restErr) && restErr.Message != nil {
			switch restErr.Message.Code {
			case discordgo.ErrCodeUnknownEmoji, discordgo.ErrCodeReactionBlocked:
				return nil
			}
		}
		return fmt.Errorf("discord add reaction: %w", err)
	}
	return nil
}

// RemoveReaction implements platform.Reactor. Passes "@me" as the userID
// so only the bot's own reaction is cleared (Discord REST convention).
func (d *Discord) RemoveReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return nil
	}
	emoji := reactionEmoji(r)
	if emoji == "" {
		return nil
	}
	parts := strings.SplitN(messageID, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid discord msgID format: %q", messageID)
	}
	if err := d.session.MessageReactionRemove(parts[0], parts[1], emoji, "@me", discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("discord remove reaction: %w", err)
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
	// Match the Feishu/Slack inbound cap: bots posting via the Discord API
	// can exceed the 2000-char user UX limit, and any oversized prompt would
	// bypass the HTTP-surface maxWSSendTextBytes guard. The shim's 12 MB
	// line ceiling and the dispatch queue's 4 MB coalesce cap are final
	// backstops, not the intended security boundary. R71-SEC-M4.
	const maxDiscordInboundBytes = 8 * 1024
	if len(text) > maxDiscordInboundBytes {
		slog.Warn("discord message exceeds inbound text cap, dropping",
			"len", len(text), "channel", m.ChannelID)
		return
	}

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
		MessageID: m.ChannelID + ":" + m.ID,
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

// discordHTTPClient disables redirects so the CDN host allowlist cannot be
// bypassed via a 302 to an internal address (SSRF). Discord's CDN serves
// attachments directly and never requires cross-host redirects.
var discordHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

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
	headerCT := stripMIMEParams(resp.Header.Get("Content-Type"))
	// Prefer the sniffed type over the CDN header: the header is upstream-
	// controlled and can carry parameters, arbitrary media types, or
	// deliberate mismatches. The sniffed value is derived from the bytes
	// we just read and is safer to forward downstream.
	ct := headerCT
	if len(data) > 0 {
		sniffed := stripMIMEParams(http.DetectContentType(data))
		if !strings.HasPrefix(sniffed, "image/") {
			return nil, "", fmt.Errorf("download: mime mismatch (header=%s sniffed=%s)", headerCT, sniffed)
		}
		ct = sniffed
	}
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
