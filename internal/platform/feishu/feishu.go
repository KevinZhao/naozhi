package feishu

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/transcribe"
	"golang.org/x/sync/singleflight"
)

const (
	// maxAPIRespBodyBytes caps every Feishu Open API JSON response read so a
	// misbehaving upstream cannot force unbounded memory growth.
	maxAPIRespBodyBytes = 1 << 20

	// Attachment download caps match Feishu's documented limits exactly.
	maxImageDownloadBytes = 10 * 1024 * 1024
	maxAudioDownloadBytes = 20 * 1024 * 1024

	// fileDownloadTimeout bounds a whole file download, body included: a
	// limits.MaxFileAttachmentBytes file needs only ~190 KB/s to finish.
	fileDownloadTimeout = 3 * time.Minute

	// tokenTTLBuffer (seconds) is subtracted from Feishu's reported token
	// expiry so a cached token is never used at its boundary (skew, latency).
	tokenTTLBuffer = 60

	// minTokenCacheDuration floors the TTL when Feishu reports an unusually
	// short expiry, keeping singleflight effective against refresh storms.
	minTokenCacheDuration = 30 * time.Second

	// tokenFailCooldown bounds how long a failed token refresh is cached: 5s
	// balances operator-visible recovery with upstream rate protection.
	tokenFailCooldown = 5 * time.Second

	// maxEventIDLen caps inbound event_id before it lands in the dedup map.
	maxEventIDLen = 256

	// maxIncomingTextBytes caps decoded inbound text; aliases
	// platform.DefaultMaxIncomingBytes so all adapters share one source of truth.
	maxIncomingTextBytes = platform.DefaultMaxIncomingBytes

	// wsStopTimeout caps how long Stop() waits for the lark-ws SDK, whose
	// Start() may block in select{} — keep systemd's stop deadline meaningful.
	wsStopTimeout = 5 * time.Second
)

var feishuHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		// Pin the TLS floor so a toolchain regression cannot accept legacy protocols.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	},
	// The Open API uses no redirects; following one would let a compromised
	// upstream aim the bearer-token request at an internal address (IMDS,
	// loopback admin) — SSRF-via-redirect. Surface the 3xx as-is instead.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// feishuFileDownloadClient shares feishuHTTPClient's connection pool and
// redirect policy; only the time budget differs, because the shared 10s
// Client.Timeout also covers the body read and would fail any file slower
// than ~3.4 MB/s long before the byte cap.
var feishuFileDownloadClient = &http.Client{
	Timeout:       fileDownloadTimeout,
	Transport:     feishuHTTPClient.Transport,
	CheckRedirect: feishuHTTPClient.CheckRedirect,
}

// APIError is the typed error returned by Feishu Open API calls; callers use
// errors.As on Code and IsPermanent to decide retry policy.
type APIError struct {
	Code int
	Msg  string
	Op   string // "send", "token", "upload", etc. — for diagnostic context
}

func (e *APIError) Error() string {
	// %q: a MITM'd upstream msg could carry bidi/C1/newline bytes into slog attrs.
	if e.Msg != "" {
		return fmt.Sprintf("feishu %s: code=%d msg=%q", e.Op, e.Code, e.Msg)
	}
	return fmt.Sprintf("feishu %s: code=%d", e.Op, e.Code)
}

// IsPermanent reports whether retrying the same request can never succeed
// (open.feishu.cn server-error-codes): 99991663 invalid app_secret, 99991664
// app disabled, 99991668 not authorized, 1061045 bot not in chat, 230001
// invalid receive_id. Token-expired codes are NOT permanent (IsTokenExpired).
func (e *APIError) IsPermanent() bool {
	switch e.Code {
	case 99991663, 99991664, 99991668, 1061045, 230001:
		return true
	}
	return false
}

// IsTokenExpired reports whether Feishu rejected the presented access token
// (99991671 tenant / 99991672 app / 99991673 user). The cached token MUST then
// be invalidated, or ReplyWithRetry's attempts all resend the same stale token.
func (e *APIError) IsTokenExpired() bool {
	switch e.Code {
	case 99991671, 99991672, 99991673:
		return true
	}
	return false
}

// isRateLimited reports a frequency limit: 99991400 per app, 11232-11234
// and 230020 on sending messages. The same request may succeed later.
func (e *APIError) isRateLimited() bool {
	switch e.Code {
	case 99991400, 11232, 11233, 11234, 230020:
		return true
	}
	return false
}

// IsTokenInvalidated implements platform.TokenInvalidatedError: the cache was
// just cleared, so ReplyWithRetry may grant one extra retry with a fresh token (#1339).
func (e *APIError) IsTokenInvalidated() bool {
	return e.IsTokenExpired()
}

// Config holds Feishu app credentials.
type Config struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLen       int    `yaml:"max_reply_length"`
}

// Feishu implements the Platform and RunnablePlatform interfaces.
type Feishu struct {
	cfg         Config
	baseURL     string       // API base URL (overridable for testing)
	fileHTTP    *http.Client // file downloads (overridable for testing)
	accessToken string
	tokenExpiry time.Time
	tokenMu     sync.RWMutex
	tokenGroup  singleflight.Group

	// botInfoSF collapses concurrent lazy bot-info re-fetches into one call.
	botInfoSF singleflight.Group

	// Token refresh circuit breaker: a failed refresh is cached for
	// tokenFailCooldown so every reply path does not re-hit open.feishu.cn
	// (singleflight alone does not cache errors).
	tokenLastFailAt time.Time
	tokenLastFailed error

	transcriber transcribe.Service // nil when STT not configured

	// admit gates media downloads and transcription; nil admits everyone.
	// Set by SetAdmission before RegisterRoutes/Start, read-only after.
	admit platform.AdmitFunc

	// Lifecycle context: cancelled on Stop(), used by message goroutines.
	stopCtx    context.Context
	stopCancel context.CancelFunc

	// WebSocket lifecycle
	handler platform.MessageHandler
	cancel  context.CancelFunc
	done    chan struct{}
	// dispatch bounds concurrent inbound handler goroutines
	// and tracks them plus the bot-info self-heal so Stop() can drain (#2254).
	dispatch platform.BoundedDispatch
	startMu  sync.Mutex
	started  bool
	// connState is fed by the websocket transport's lifecycle hooks.
	connState platform.ConnTracker

	// cleanupWg tracks the cleanupReactions goroutine so Stop() can wait it out.
	cleanupWg sync.WaitGroup

	// reactionIDs caches (messageID + emoji_type) -> reactionCacheEntry because
	// Feishu's delete endpoint needs the reaction_id. Entries go on successful
	// removal or when cleanupReactionsTick sees the expiry, so unpaired Adds
	// (restart, message deleted) cannot accumulate forever.
	reactionIDs sync.Map

	// botOpenID is the bot's own open_id for isBotMentioned. Empty when
	// bot/v3/info failed, in which case the check degrades to "any @ counts"
	// and the degraded branch kicks a rate-limited self-heal re-fetch so an
	// ambient @other-bot mention cannot wake this bot for the whole process
	// lifetime. Guarded by botInfoMu.
	botInfoMu          sync.RWMutex
	botOpenID          string
	lastBotInfoFetchNs int64 // unix nanos of the last fetch attempt; rate-limits self-heal

	// unsupportedNotices rate-limits the unsupported-message-type notice per chat.
	unsupportedNotices noticeThrottle
}

// New creates a Feishu platform adapter. transcriber may be nil to disable voice.
func New(cfg Config, transcriber transcribe.Service) *Feishu {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = platform.DefaultMaxReplyLen
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Feishu{cfg: cfg, baseURL: "https://open.feishu.cn", fileHTTP: feishuFileDownloadClient, transcriber: transcriber, dispatch: platform.BoundedDispatch{Name: "feishu"}, stopCtx: ctx, stopCancel: cancel}
	f.cleanupWg.Add(1)
	go func() {
		defer f.cleanupWg.Done()
		// Field lookup (not the local ctx) so a future stopCtx swap cannot
		// leave this goroutine on a never-cancelled context.
		f.cleanupReactions(f.stopCtx)
	}()
	return f
}

func (f *Feishu) Name() string { return "feishu" }

func (f *Feishu) MaxReplyLength() int { return f.cfg.MaxReplyLen }

func (f *Feishu) SupportsInterimMessages() bool { return true }

// ConnState implements platform.ConnStateReporter from the websocket
// transport's lifecycle hooks.
func (f *Feishu) ConnState() (platform.ConnState, bool) {
	return f.connState.Snapshot()
}

// SetAdmission implements platform.Admitter.
func (f *Feishu) SetAdmission(fn platform.AdmitFunc) { f.admit = fn }

// admitted reports whether msg's sender may cost a media download or a
// transcription.
func (f *Feishu) admitted(ctx context.Context, msg platform.IncomingMessage) bool {
	return f.admit == nil || f.admit(ctx, msg)
}

// RegisterRoutes is a no-op: Feishu events arrive over the websocket long
// connection, so there is no HTTP route.
func (f *Feishu) RegisterRoutes(*http.ServeMux, platform.MessageHandler) {}

// Start implements RunnablePlatform. Launches the WebSocket connection.
func (f *Feishu) Start(handler platform.MessageHandler) error {
	f.startMu.Lock()
	if f.started {
		f.startMu.Unlock()
		return fmt.Errorf("feishu platform already started")
	}
	f.started = true
	f.startMu.Unlock()

	f.handler = handler

	// Best-effort, 5s-boxed fetch of the bot's open_id; failure degrades
	// isBotMentioned to "any @ is a hit" (same contract as slack's AuthTest).
	// IIFE + defer so the timer is released even if fetchBotInfo panics.
	func() {
		fetchCtx, cancelFetch := context.WithTimeout(f.stopCtx, 5*time.Second)
		defer cancelFetch()
		// Stamp so the self-heal cooldown counts from this fetch, not the
		// first group mention.
		atomic.StoreInt64(&f.lastBotInfoFetchNs, time.Now().UnixNano())
		if err := f.fetchBotInfo(fetchCtx); err != nil {
			slog.Warn("feishu fetch bot info failed — group mention filtering will fall back to 'any mention' (less precise)",
				"err", err)
		}
	}()

	slog.Info("feishu using websocket mode (no public IP needed)")
	return f.startWebSocket()
}

// Stop implements RunnablePlatform. Stops WebSocket connection.
func (f *Feishu) Stop() error {
	f.startMu.Lock()
	cancel := f.cancel
	done := f.done
	f.startMu.Unlock()

	f.stopCancel()

	if cancel != nil {
		cancel()
		// SDK's Start() may block indefinitely (select{}); don't wait forever.
		timer := time.NewTimer(wsStopTimeout)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			slog.Warn("feishu websocket stop timed out")
		}
	}
	f.dispatch.Wait()  // in-flight message handlers
	f.cleanupWg.Wait() // cleanupReactions goroutine
	return nil
}
