package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// Config holds Discord bot credentials.
type Config struct {
	BotToken    string
	MaxReplyLen int
}

// discordBotHealCooldown rate-limits the lazy bot-identity self-heal so group
// traffic while botID is unknown cannot hammer the REST API (#2009).
const discordBotHealCooldown = time.Minute

// Discord implements Platform and RunnablePlatform via WebSocket gateway.
type Discord struct {
	cfg     Config
	session *discordgo.Session
	handler platform.MessageHandler
	startMu sync.Mutex
	started bool
	// botID is written after the gateway connects and read concurrently by
	// onMessageCreate goroutines; a plain string would be a torn-read data
	// race (#1814).
	botID atomic.Pointer[string]
	// botHealAt is the next time the self-heal may run; guarded by botHealMu.
	botHealMu sync.Mutex
	botHealAt time.Time

	stopCtx    context.Context
	stopCancel context.CancelFunc
	// dispatch bounds concurrent handler goroutines — each may download up to
	// maxDiscordAttachmentsPerMessage × 10 MB — and tracks the self-heal for Stop().
	dispatch platform.BoundedDispatch
}

// New creates a Discord platform adapter.
func New(cfg Config) *Discord {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = platform.DiscordMaxReplyLen // Discord's actual API limit
	}
	return &Discord{cfg: cfg, dispatch: platform.BoundedDispatch{Name: "discord"}}
}

// getBotID returns the bot's user ID, or "" before the gateway populated it.
func (d *Discord) getBotID() string {
	if p := d.botID.Load(); p != nil {
		return *p
	}
	return ""
}

// setBotID stores the bot's user ID; shared by Start() and the late-READY backfill.
func (d *Discord) setBotID(id string) {
	if id == "" {
		return
	}
	d.botID.Store(&id)
}

// maybeHealBotID kicks one rate-limited background identity fetch while botID
// is unknown (Open() can return without a READY frame), restoring exact
// mention filtering (#2009).
func (d *Discord) maybeHealBotID() {
	if d.getBotID() != "" {
		return
	}
	d.botHealMu.Lock()
	if d.getBotID() != "" || time.Now().Before(d.botHealAt) {
		d.botHealMu.Unlock()
		return
	}
	d.botHealAt = time.Now().Add(discordBotHealCooldown)
	d.botHealMu.Unlock()

	sess := d.session
	if sess == nil {
		return
	}
	d.dispatch.Go("discord bot heal", func() {
		u, err := sess.User("@me")
		if err != nil || u == nil {
			slog.Warn("discord bot identity self-heal failed; staying fail-open", "err", err)
			return
		}
		d.setBotID(u.ID)
		slog.Info("discord bot identity recovered",
			"bot_id", u.ID,
			"bot_name", osutil.SanitizeForLog(u.Username, 128))
	})
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

	// discordgo's default client follows 3xx while keeping the Authorization
	// header — SSRF / token leakage via a hostile redirect. Stop at hop one.
	sess.Client = &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	sess.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	sess.AddHandler(d.onMessageCreate)
	// Open() can return before READY (Op 9 / Op 1 first packets), leaving
	// State.User nil; backfill botID when READY arrives (#2009).
	sess.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		if r != nil && r.User != nil {
			d.setBotID(r.User.ID)
			slog.Info("discord ready: bot identity set",
				"bot_id", r.User.ID,
				"bot_name", osutil.SanitizeForLog(r.User.Username, 128))
		}
	})

	// Assigned BEFORE Open() so handlers never see a nil d.session.
	d.session = sess

	if err := sess.Open(); err != nil {
		d.session = nil
		return fmt.Errorf("open discord gateway: %w", err)
	}

	if sess.State != nil && sess.State.User != nil {
		d.setBotID(sess.State.User.ID)
		slog.Info("discord gateway connected",
			"bot_id", sess.State.User.ID,
			"bot_name", osutil.SanitizeForLog(sess.State.User.Username, 128))
	} else {
		// Not fatal: READY or maybeHealBotID backfills; group messages fail-open meanwhile.
		slog.Warn("discord gateway connected but bot identity unavailable; will backfill on READY")
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
	go func() { d.dispatch.Wait(); close(done) }()
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
		return platform.EncodeMessageRef(msg.ChatID, m.ID), nil
	}

	m, err := d.session.ChannelMessageSend(msg.ChatID, msg.Text, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("discord send: %w", err)
	}
	return platform.EncodeMessageRef(msg.ChatID, m.ID), nil
}

// EditMessage updates an existing Discord message.
func (d *Discord) EditMessage(ctx context.Context, msgID string, text string) error {
	channel, id, ok := platform.DecodeMessageRef(msgID)
	if !ok {
		return fmt.Errorf("invalid discord msgID format: %q", msgID)
	}
	if _, err := d.session.ChannelMessageEdit(channel, id, text, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("discord edit message %s: %w", msgID, err)
	}
	return nil
}

// reactionEmoji maps ReactionType to a raw unicode emoji; "" = unsupported.
func reactionEmoji(r platform.ReactionType) string {
	switch r {
	case platform.ReactionQueued:
		return "\u23F3" // ⏳ hourglass
	}
	return ""
}

// AddReaction implements platform.Reactor on a composite "channel:msg" id.
func (d *Discord) AddReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return fmt.Errorf("discord AddReaction: empty messageID")
	}
	emoji := reactionEmoji(r)
	if emoji == "" {
		return fmt.Errorf("discord AddReaction: unsupported reaction %q", r)
	}
	channel, id, ok := platform.DecodeMessageRef(messageID)
	if !ok {
		return fmt.Errorf("invalid discord msgID format: %q", messageID)
	}
	if err := d.session.MessageReactionAdd(channel, id, emoji, discordgo.WithContext(ctx)); err != nil {
		// Idempotent: swallow the "already reacted" variants so dispatch does
		// not fall back to a text notice on retry.
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
	channel, id, ok := platform.DecodeMessageRef(messageID)
	if !ok {
		return fmt.Errorf("invalid discord msgID format: %q", messageID)
	}
	if err := d.session.MessageReactionRemove(channel, id, emoji, "@me", discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("discord remove reaction: %w", err)
	}
	return nil
}

func (d *Discord) onMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil {
		return
	}
	botID := d.getBotID()
	if m.Author.ID == botID {
		return
	}
	if m.Author.Bot {
		return
	}

	text := m.Content
	mentionMe := false

	if botID != "" {
		for _, u := range m.Mentions {
			if u.ID == botID {
				mentionMe = true
				text = strings.ReplaceAll(text, "<@"+botID+">", "")
				text = strings.ReplaceAll(text, "<@!"+botID+">", "")
				break
			}
		}
	} else {
		// botID unknown: mentionMe=false would fail CLOSED (dispatch's group
		// gate drops every guild message until restart). Fail open and kick a
		// rate-limited self-heal instead (#2009).
		mentionMe = true
		d.maybeHealBotID()
	}
	text = strings.TrimSpace(text)
	// API-posted messages can exceed the 2000-char UX limit; cap before dispatch.
	const maxDiscordInboundBytes = platform.DefaultMaxIncomingBytes
	if len(text) > maxDiscordInboundBytes {
		slog.Warn("discord message exceeds inbound text cap, dropping",
			"len", len(text), "channel", m.ChannelID)
		return
	}

	// Attachment metadata is collected here; downloads happen asynchronously.
	type pendingImage struct {
		url         string
		contentType string
	}
	// Discord allows 10 × 10 MB attachments per message; cap the in-flight
	// footprint a hostile client can pin.
	const maxDiscordAttachmentsPerMessage = 5
	var pending []pendingImage
	for _, att := range m.Attachments {
		if !isImageContentType(att.ContentType) {
			continue
		}
		if len(pending) >= maxDiscordAttachmentsPerMessage {
			slog.Warn("discord attachments truncated",
				"channel", m.ChannelID,
				"kept", maxDiscordAttachmentsPerMessage,
				"total", len(m.Attachments))
			break
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

	// Downloads run in the bounded goroutine, not discordgo's event dispatch.
	d.dispatch.TryGo("discord", func() {
		var total int
		for _, p := range pending {
			data, mime, err := downloadURL(p.url)
			if err != nil {
				slog.Warn("discord download attachment failed",
					"err", err, "url", osutil.SanitizeForLog(p.url, 256))
				continue
			}
			if !aggregateAttachmentBytesAllow(total, len(data)) {
				slog.Warn("discord attachments aggregate cap reached",
					"channel", m.ChannelID,
					"kept", len(msg.Images),
					"cap_bytes", maxDiscordTotalAttachmentBytes,
					"so_far_bytes", total,
					"next_bytes", len(data))
				break
			}
			total += len(data)
			msg.Images = append(msg.Images, platform.Image{Data: data, MimeType: mime})
		}
		d.handler(d.stopCtx, msg)
	}, "channel", m.ChannelID, "user", m.Author.ID)
}

// maxDiscordTotalAttachmentBytes caps aggregate bytes per inbound message on
// top of the per-image 10 MB cap: 32 MiB fits ordinary screenshots while
// bounding heap pinned until the dispatcher consumes the message.
const maxDiscordTotalAttachmentBytes = 32 * 1024 * 1024

// aggregateAttachmentBytesAllow reports whether adding next bytes to soFar
// stays within maxDiscordTotalAttachmentBytes.
func aggregateAttachmentBytesAllow(soFar, next int) bool {
	if next < 0 {
		return false
	}
	return soFar+next <= maxDiscordTotalAttachmentBytes
}

func isImageContentType(ct string) bool {
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

// discordDialTestBypass disables the private-IP dial guard for loopback
// httptest servers. Set ONLY by discord_test.go; production MUST leave it false.
var discordDialTestBypass bool

// blockPrivateDial returns a DialContext that resolves the host and refuses
// reserved IPs (loopback, link-local, private, unspecified), closing the DNS
// rebinding vector where an allowlisted CDN host later resolves to IMDS. The
// validated IP is dialed directly so the resolver is not consulted twice.
func blockPrivateDial() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("discord: malformed dial address %q: %w", addr, err)
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("discord: DNS lookup %q: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("discord: no addresses for %q", host)
		}
		for _, ia := range addrs {
			ip := ia.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsUnspecified() {
				if discordDialTestBypass {
					continue
				}
				return nil, fmt.Errorf("discord: refused connection to reserved IP %s (DNS rebinding guard)", ip)
			}
		}
		// Dial the validated IP directly (no second DNS lookup / TOCTOU).
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
	}
}

// discordHTTPClient disables redirects (a 302 could bypass the CDN allowlist
// into an internal address) and dials through blockPrivateDial.
var discordHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           blockPrivateDial(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
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
	// CDN URLs are always https; plaintext would let a MITM substitute bytes
	// that are then forwarded as a trusted attachment.
	if u.Scheme != "https" {
		return nil, "", fmt.Errorf("attachment URL must be https, got %q", u.Scheme)
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
	ct, err := resolveImageContentType(data, headerCT, u.Hostname())
	if err != nil {
		return nil, "", err
	}
	return data, ct, nil
}

// resolveImageContentType derives the forwarded Content-Type from the bytes,
// never the CDN-controlled header (a malicious edge could claim text/html for
// XSS on IM clients). An empty body is an error, not a header fallback.
func resolveImageContentType(data []byte, headerCT, host string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("download: empty body from %s", host)
	}
	sniffed := stripMIMEParams(http.DetectContentType(data))
	if !strings.HasPrefix(sniffed, "image/") {
		return "", fmt.Errorf("download: mime mismatch (header=%s sniffed=%s)", headerCT, sniffed)
	}
	return sniffed, nil
}

func stripMIMEParams(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}
