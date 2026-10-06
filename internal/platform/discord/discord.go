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

// discordCloseTimeout bounds how long Stop waits for the gateway close. A
// reconnect blocked on a handshake holds the session lock with no read
// deadline, and Close waits on that lock.
const discordCloseTimeout = 5 * time.Second

// Disconnect-probe cadence: the first REST check after a drop, the cap its
// doubling backs off to, and the bound on one request.
const (
	discordProbeDelay       = 5 * time.Second
	discordProbeMaxInterval = 5 * time.Minute
	discordProbeTimeout     = 10 * time.Second
)

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
	// connState is fed by the gateway's Connect/Ready/Resumed/Disconnect events.
	connState platform.ConnTracker
	// admit gates attachment downloads; nil admits everyone. Set by
	// SetAdmission before Start, read-only after.
	admit platform.AdmitFunc
	// restTransport replaces the REST client's transport; nil in production.
	restTransport http.RoundTripper
	// closeTimeout overrides discordCloseTimeout when non-zero.
	closeTimeout time.Duration
	// probing is set while a disconnect probe goroutine runs.
	probing atomic.Bool
	// probeDelay / probeMaxInterval / probeTimeout override the discordProbe*
	// defaults when non-zero.
	probeDelay       time.Duration
	probeMaxInterval time.Duration
	probeTimeout     time.Duration
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
	if sess == nil || d.stopped() {
		return
	}
	d.dispatch.Go("discord bot heal", func() {
		u, err := d.fetchSelf(sess)
		if d.stopped() {
			return
		}
		if err != nil {
			slog.Warn("discord bot identity self-heal failed; staying fail-open", "err", probeReason(err))
			return
		}
		if u == nil || u.ID == "" {
			slog.Warn("discord bot identity self-heal got no user ID; staying fail-open")
			return
		}
		d.setBotID(u.ID)
		slog.Info("discord bot identity recovered",
			"bot_id", u.ID,
			"bot_name", osutil.SanitizeForLog(u.Username, 128))
	})
}

// stopped reports whether Stop has been called.
func (d *Discord) stopped() bool {
	return d.stopCtx != nil && d.stopCtx.Err() != nil
}

// fetchSelf asks REST who the bot is, bounded by probeTimeout and by Stop.
// The 429 retry is off: discordgo would sleep it out ignoring ctx. Its
// pre-request wait on an exhausted rate-limit bucket still ignores ctx.
func (d *Discord) fetchSelf(sess *discordgo.Session) (*discordgo.User, error) {
	parent := d.stopCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, durationOr(d.probeTimeout, discordProbeTimeout))
	defer cancel()
	return sess.User("@me", discordgo.WithContext(ctx), discordgo.WithRetryOnRatelimit(false))
}

func (d *Discord) Name() string { return "discord" }

func (d *Discord) MaxReplyLength() int { return d.cfg.MaxReplyLen }

func (d *Discord) SupportsInterimMessages() bool { return true }

// ConnState implements platform.ConnStateReporter; ok=false until Start.
func (d *Discord) ConnState() (platform.ConnState, bool) { return d.connState.Snapshot() }

// SetAdmission implements platform.Admitter.
func (d *Discord) SetAdmission(fn platform.AdmitFunc) { d.admit = fn }

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

	d.configureSession(sess)

	// Assigned BEFORE Open() so handlers never see a nil d.session.
	d.session = sess

	d.connState.Set(platform.ConnConnecting)
	if err := sess.Open(); err != nil {
		d.session = nil
		// The server refuses to start without the gateway, so nothing retries.
		d.connState.Fail(platform.ConnFailed, err)
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

// configureSession applies the REST client policy, intents and event handlers
// Start gives every gateway session.
func (d *Discord) configureSession(sess *discordgo.Session) {
	// discordgo's default client follows 3xx while keeping the Authorization
	// header — SSRF / token leakage via a hostile redirect. Stop at hop one.
	sess.Client = &http.Client{
		Timeout:   20 * time.Second,
		Transport: d.restTransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	sess.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	// Inline handlers see a drop's Disconnect before its reconnect's Connect;
	// with a goroutine per event a late Disconnect could overwrite
	// "connected". Connect and the first Ready fire inside Open under the
	// session lock, so no handler may call back into sess. Messages keep
	// their own goroutine.
	sess.SyncEvents = true
	sess.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		go d.onMessageCreate(s, m)
	})
	sess.AddHandler(d.onReady)
	sess.AddHandler(d.onConnect)
	sess.AddHandler(d.onResumed)
	sess.AddHandler(d.onDisconnect)
}

// onReady marks the gateway connected and backfills botID: Open() can return
// before READY (Op 9 / Op 1 first packets), leaving State.User nil (#2009).
func (d *Discord) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	d.connState.Set(platform.ConnConnected)
	if r != nil && r.User != nil {
		d.setBotID(r.User.ID)
		slog.Info("discord ready: bot identity set",
			"bot_id", r.User.ID,
			"bot_name", osutil.SanitizeForLog(r.User.Username, 128))
	}
}

// onConnect: discordgo emits Connect once READY or RESUMED has arrived, at
// the end of a successful Open (the first one and every reconnect).
func (d *Discord) onConnect(_ *discordgo.Session, _ *discordgo.Connect) {
	d.connState.Set(platform.ConnConnected)
}

func (d *Discord) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	d.connState.Set(platform.ConnConnected)
}

// onDisconnect: discordgo emits Disconnect when it closes the websocket and,
// unless Stop closed it, retries Open with backoff until one succeeds. Failed
// attempts emit nothing, so Since stays at the drop and the reason comes from
// the disconnect probe. Stop leaves the state as it was.
func (d *Discord) onDisconnect(s *discordgo.Session, _ *discordgo.Disconnect) {
	if d.stopped() {
		return
	}
	d.connState.Set(platform.ConnDisconnected)
	if s != nil && d.stopCtx != nil && d.probing.CompareAndSwap(false, true) {
		d.dispatch.Go("discord disconnect probe", func() { d.runDisconnectProbe(s) })
	}
}

// runDisconnectProbe asks REST who the bot is while the gateway stays down:
// discordgo reports drop and reconnect errors only to its package-global
// logger, which has no session identity and hides them at the default level.
// A rejected token is terminal; any other error becomes LastError. Close codes
// such as 4014 (disallowed intents) stay invisible and are left to doctor's
// grace period.
func (d *Discord) runDisconnectProbe(sess *discordgo.Session) {
	for {
		d.probeWhileDisconnected(sess)
		d.probing.Store(false)
		// A Disconnect between the last state check and the Store found
		// probing set and started nothing; pick it up here.
		if d.stopCtx.Err() != nil || !d.stillDisconnected() || !d.probing.CompareAndSwap(false, true) {
			return
		}
	}
}

func (d *Discord) stillDisconnected() bool {
	st, _ := d.connState.Snapshot()
	return st.State == platform.ConnDisconnected
}

func (d *Discord) probeWhileDisconnected(sess *discordgo.Session) {
	delay := durationOr(d.probeDelay, discordProbeDelay)
	maxInterval := durationOr(d.probeMaxInterval, discordProbeMaxInterval)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-d.stopCtx.Done():
			return
		case <-timer.C:
		}
		if !d.stillDisconnected() {
			return
		}
		if d.probeOnce(sess) {
			return
		}
		delay = nextProbeDelay(delay, maxInterval)
		timer.Reset(delay)
	}
}

// probeOnce runs one REST check and reports whether the state is now terminal.
// Both verdicts apply only while the state is still disconnected, so a check
// that outlives a reconnect leaves the recovered state and LastError alone.
func (d *Discord) probeOnce(sess *discordgo.Session) bool {
	_, err := d.fetchSelf(sess)
	if err == nil {
		slog.Debug("discord gateway down but REST accepts the bot token")
		return false
	}
	if d.stopCtx.Err() != nil {
		return true
	}
	if code := restStatus(err); code == http.StatusUnauthorized || code == http.StatusForbidden {
		if d.connState.FailIf(platform.ConnDisconnected, platform.ConnFailed,
			fmt.Errorf("discord rejected the bot token (HTTP %d): update platforms.discord.bot_token and restart", code)) {
			slog.Error("discord rejected the bot token while the gateway is down; update platforms.discord.bot_token and restart",
				"status", code)
		}
		return true
	}
	d.connState.FailIf(platform.ConnDisconnected, platform.ConnDisconnected,
		fmt.Errorf("gateway down; REST probe: %w", probeReason(err)))
	return false
}

// discordRetriesExhausted prefixes the plain error discordgo v0.29.0 returns
// for a 502 that outlived its retries; that error carries the response body.
const discordRetriesExhausted = "Exceeded Max retries HTTP "

// probeReason reduces a failed response to its status, because a body can
// echo request details or be a whole HTML error page. Transport errors pass
// through as they are.
func probeReason(err error) error {
	if code := restStatus(err); code != 0 {
		return fmt.Errorf("HTTP %d", code)
	}
	var rateLimited *discordgo.RateLimitError
	if errors.As(err, &rateLimited) {
		return fmt.Errorf("HTTP %d", http.StatusTooManyRequests)
	}
	if strings.HasPrefix(err.Error(), discordRetriesExhausted) {
		return fmt.Errorf("HTTP %d", http.StatusBadGateway)
	}
	return err
}

// restStatus is the HTTP status of a discordgo RESTError, or 0.
func restStatus(err error) int {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil {
		return restErr.Response.StatusCode
	}
	return 0
}

// nextProbeDelay doubles the wait between checks up to maxInterval, so a long
// outage costs a handful of REST calls rather than one every few seconds.
func nextProbeDelay(delay, maxInterval time.Duration) time.Duration {
	return min(2*delay, maxInterval)
}

func durationOr(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Stop implements RunnablePlatform. Closes Discord WebSocket gateway.
func (d *Discord) Stop() error {
	if d.stopCancel != nil {
		d.stopCancel()
	}
	if d.session != nil {
		if err := d.closeSession(); err != nil {
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

// closeSession closes the gateway session, giving up after closeTimeout so a
// handshake stuck inside discordgo cannot hold up shutdown. The abandoned
// Close finishes on its own if the handshake ever ends.
func (d *Discord) closeSession() error {
	timeout := d.closeTimeout
	if timeout <= 0 {
		timeout = discordCloseTimeout
	}
	errc := make(chan error, 1)
	go func() { errc <- d.session.Close() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-errc:
		return err
	case <-timer.C:
		slog.Warn("discord: gateway close timed out; continuing shutdown", "timeout", timeout)
		return nil
	}
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
		// Only attachments cost a download; text-only messages go straight to
		// the handler, which judges them anyway.
		if len(pending) > 0 && d.admit != nil && !d.admit(d.stopCtx, msg) {
			return
		}
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
