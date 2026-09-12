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

	// tokenTTLBuffer (seconds) is subtracted from Feishu's reported token
	// expiry so a cached token is never used at its boundary (skew, latency).
	tokenTTLBuffer = 60

	// minTokenCacheDuration floors the TTL when Feishu reports an unusually
	// short expiry, keeping singleflight effective against refresh storms.
	minTokenCacheDuration = 30 * time.Second

	// maxWebhookBodyBytes caps the webhook request body; 64 KiB is well above
	// any legitimate Feishu payload.
	maxWebhookBodyBytes = 64 * 1024

	// maxWebhookNonceLen bounds X-Lark-Request-Nonce (16 chars in practice)
	// so a header flood cannot bloat the seenNonces map.
	maxWebhookNonceLen = 128

	// maxEventIDLen caps inbound event_id before it lands in the dedup map;
	// shared by transport_ws.go and transport_hook.go.
	maxEventIDLen = 256

	// maxIncomingTextBytes caps decoded inbound text; aliases
	// platform.DefaultMaxIncomingBytes so all adapters share one source of truth.
	maxIncomingTextBytes = platform.DefaultMaxIncomingBytes

	// maxWebhookTokenLen bounds the body token before constantTimeEqualString
	// hashes it (~32 bytes real) so a 64 KiB body is not a per-request SHA-256 DoS lever.
	maxWebhookTokenLen = 512

	// maxWebhookSigLen bounds X-Lark-Signature (64-byte hex real) before
	// verifySignature concatenates + hashes.
	maxWebhookSigLen = 256

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

// IsTokenInvalidated implements platform.TokenInvalidatedError: the cache was
// just cleared, so ReplyWithRetry may grant one extra retry with a fresh token (#1339).
func (e *APIError) IsTokenInvalidated() bool {
	return e.IsTokenExpired()
}

// Config holds Feishu app credentials.
type Config struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	ConnectionMode    string `yaml:"connection_mode"` // "websocket" (default) | "webhook"
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLen       int    `yaml:"max_reply_length"`

	// AllowInsecureWebhook opts in to verification_token-only webhook mode (no
	// encrypt_key HMAC): a passive observer who sees the plaintext token can
	// forge/replay events within the 5min window, so the webhook refuses to
	// start without this explicit, audited choice (#1507).
	AllowInsecureWebhook bool `yaml:"allow_insecure_webhook"`
}

// Feishu implements the Platform and RunnablePlatform interfaces.
type Feishu struct {
	cfg         Config
	mode        string // resolved connection mode
	baseURL     string // API base URL (overridable for testing)
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

	// Lifecycle context: cancelled on Stop(), used by webhook goroutines.
	stopCtx    context.Context
	stopCancel context.CancelFunc

	// WebSocket lifecycle
	handler platform.MessageHandler
	cancel  context.CancelFunc
	done    chan struct{}
	// dispatch bounds concurrent inbound handler goroutines (both transports)
	// and tracks them plus the bot-info self-heal so Stop() can drain (#2254).
	dispatch platform.BoundedDispatch
	startMu  sync.Mutex
	started  bool

	// cleanupWg tracks the cleanupNonces goroutine so Stop() can wait it out.
	cleanupWg sync.WaitGroup

	// Replay protection: stores "ts:nonce" -> expiry unix timestamp.
	seenNonces sync.Map
	// seenNoncesCount approximates len(seenNonces) so the cap check avoids an
	// O(n) Range. Eventually consistent: a few extra entries between check and
	// increment are bounded and harmless.
	seenNoncesCount atomic.Int64
	// nonceEvictMu serializes evictOldestNonces: overlapping evictions would
	// split the counter adjustment and let it dip below the map's real size,
	// bypassing the cap gate. Eviction-and-recount is one critical section (#1534).
	nonceEvictMu sync.Mutex

	// evictNoncesFn lets tests inject the eviction step (incl. the evicted==0
	// fallback) without seeding a 50k-entry map; nil = evictOldestNonces.
	evictNoncesFn func() int

	// reactionIDs caches (messageID + emoji_type) -> reactionCacheEntry because
	// Feishu's delete endpoint needs the reaction_id. Entries go on successful
	// removal or when cleanupNoncesTick sees the expiry, so unpaired Adds
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

	// insecureWebhookWarnOnce emits one runtime SECURITY error on the first
	// live delivery in verification_token-only mode — traffic-correlated and
	// harder to miss than the boot Warn; Once prevents log amplification (#1724).
	insecureWebhookWarnOnce sync.Once
}

// New creates a Feishu platform adapter. transcriber may be nil to disable voice.
func New(cfg Config, transcriber transcribe.Service) *Feishu {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = platform.DefaultMaxReplyLen
	}
	mode := cfg.ConnectionMode
	if mode == "" {
		mode = "websocket"
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Feishu{cfg: cfg, mode: mode, baseURL: "https://open.feishu.cn", transcriber: transcriber, dispatch: platform.BoundedDispatch{Name: "feishu"}, stopCtx: ctx, stopCancel: cancel}
	f.cleanupWg.Add(1)
	go func() {
		defer f.cleanupWg.Done()
		// Field lookup (not the local ctx) so a future stopCtx swap cannot
		// leave this goroutine on a never-cancelled context.
		f.cleanupNonces(f.stopCtx)
	}()
	return f
}

// nonceTTL matches verifyTimestamp's 5-minute freshness window; older
// requests are rejected by timestamp anyway, so longer retention only bloats the map.
func (f *Feishu) Name() string { return "feishu" }

func (f *Feishu) MaxReplyLength() int { return f.cfg.MaxReplyLen }

func (f *Feishu) SupportsInterimMessages() bool { return true }

// RegisterRoutes registers webhook routes (only in webhook mode).
func (f *Feishu) RegisterRoutes(mux *http.ServeMux, handler platform.MessageHandler) {
	if f.mode == "webhook" {
		f.registerWebhook(mux, handler)
	}
}

// Start implements RunnablePlatform. Launches WebSocket connection in WS mode.
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

	if f.mode == "websocket" {
		slog.Info("feishu using websocket mode (no public IP needed)")
		return f.startWebSocket()
	}
	// Webhook mode is a public endpoint: without either credential anyone on
	// the internet could inject forged events.
	if f.cfg.VerificationToken == "" && f.cfg.EncryptKey == "" {
		return fmt.Errorf("feishu webhook mode requires verification_token or encrypt_key to be configured")
	}
	// Token-only mode is forgeable if the plaintext token leaks; require the
	// explicit allow_insecure_webhook opt-in (#1507).
	if f.cfg.EncryptKey == "" {
		if !f.cfg.AllowInsecureWebhook {
			return fmt.Errorf("feishu webhook: verification_token-only mode has no HMAC and is replay/forgery-prone if the token leaks; " +
				"configure encrypt_key (recommended) or set allow_insecure_webhook: true to accept this risk")
		}
		slog.Warn("feishu webhook: running in verification_token-only mode (allow_insecure_webhook=true) — no encrypt_key HMAC; events are replay/forgery-prone if the token leaks")
	}
	slog.Info("feishu using webhook mode")
	return nil
}

// fetchBotInfo populates botOpenID via GET /open-apis/bot/v3/info. Note the
// `bot` field is at top level, NOT under `data` (older API predating the
// standard envelope).
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
	f.cleanupWg.Wait() // cleanupNonces goroutine
	return nil
}

// Reply sends a message to a Feishu chat. Handles text and/or images.
