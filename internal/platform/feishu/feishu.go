package feishu

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
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
const nonceTTL = 5 * time.Minute

// maxSeenNonces caps the replay map (~3.6 MB at 50k entries) so a flood of
// authenticated unique-nonce requests cannot bloat memory.
const maxSeenNonces = 50000

// nonceEvictionBatch is how many oldest entries evictOldestNonces removes on
// cap-hit. Without it a leaked verification_token could pin the map at cap
// and 429 every legitimate webhook for nonceTTL; ~2% of cap keeps a sustained
// flood paying a steady cost instead of thrashing every insert (#1332).
const nonceEvictionBatch = 1024

// tokenFailCooldown bounds how long a failed token refresh is cached: 5s
// balances operator-visible recovery with upstream rate protection.
const tokenFailCooldown = 5 * time.Second

// nonceCleanupInterval is nonceTTL/2 so an expired entry never lingers past
// ~1.5 × TTL and seenNoncesCount does not creep toward the cap under
// sustained traffic.
const nonceCleanupInterval = nonceTTL / 2

func (f *Feishu) cleanupNonces(ctx context.Context) {
	ticker := time.NewTicker(nonceCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// The recover frame MUST live in the per-tick helper, not here: a
			// function-scope recover would unwind out of the loop and disable
			// replay protection until restart.
			f.cleanupNoncesTick()
		case <-ctx.Done():
			return
		}
	}
}

// cleanupNoncesTick performs one sweep of the expired-nonce map in its own
// recover frame so a panic is logged and the next tick retries.
func (f *Feishu) cleanupNoncesTick() {
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicRecoveredTotal.Add(1)
			slog.Error("feishu: cleanupNonces tick panic recovered; replay protection continues on next tick",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	// One wall-time basis for both sweeps so cutoff decisions are consistent.
	nowT := time.Now()
	now := nowT.Unix()
	deleted := int64(0)
	f.seenNonces.Range(func(k, v any) bool {
		// sync.Map has no type safety; drop malformed entries so the map recovers.
		ts, ok := v.(int64)
		if !ok || ts < now {
			f.seenNonces.Delete(k)
			deleted++
		}
		return true
	})
	if deleted > 0 {
		// Clamp at zero: malformed entries deleted above may have bypassed the
		// counted insert path, and a negative counter would eventually 429
		// legitimate traffic.
		if n := f.seenNoncesCount.Add(-deleted); n < 0 {
			f.seenNoncesCount.Store(0)
		}
	}

	// reactionIDs sweep rides the same tick (UnixNano: its TTL is finer).
	nowNano := nowT.UnixNano()
	f.reactionIDs.Range(func(k, v any) bool {
		entry, ok := v.(reactionCacheEntry)
		if !ok || entry.expiry <= nowNano {
			f.reactionIDs.Delete(k)
		}
		return true
	})
}

// evictNonces dispatches to the test hook when set, else evictOldestNonces.
func (f *Feishu) evictNonces() int {
	if f.evictNoncesFn != nil {
		return f.evictNoncesFn()
	}
	return f.evictOldestNonces()
}

// evictOldestNonces removes up to nonceEvictionBatch entries closest to
// natural expiry so fresh traffic keeps flowing when a leaked-token flood
// hits the cap (#1332). The replay regression is bounded: only nonces inside
// the current nonceTTL window are evicted, and timestamp verification still
// rejects older payloads. Returns the number actually removed.
func (f *Feishu) evictOldestNonces() int {
	// Serialized so overlapping evictions cannot split the counter decrement
	// and lapse the cap guard; the counter is resynced to the live size on exit (#1534).
	f.nonceEvictMu.Lock()
	defer f.nonceEvictMu.Unlock()

	type nonceEntry struct {
		key    any
		expiry int64
	}
	// Inserts missed by the Range are necessarily fresh, so excluding them only
	// makes eviction more aggressive on the genuinely-old set.
	entries := make([]nonceEntry, 0, nonceEvictionBatch*2)
	f.seenNonces.Range(func(k, v any) bool {
		ts, ok := v.(int64)
		if !ok {
			// Cross-type entry: sentinel expiry sorts it to the front.
			entries = append(entries, nonceEntry{key: k, expiry: 0})
			return true
		}
		entries = append(entries, nonceEntry{key: k, expiry: ts})
		return true
	})
	if len(entries) == 0 {
		return 0
	}
	slices.SortFunc(entries, func(a, b nonceEntry) int {
		return cmp.Compare(a.expiry, b.expiry)
	})
	limit := nonceEvictionBatch
	if limit > len(entries) {
		limit = len(entries)
	}
	deleted := 0
	for i := 0; i < limit; i++ {
		if _, loaded := f.seenNonces.LoadAndDelete(entries[i].key); loaded {
			deleted++
		}
	}
	if deleted > 0 {
		// Resync to the live size rather than Add(-deleted): concurrent inserts
		// and the cleanup ticker still adjust the counter, and a relative
		// decrement could drift below the real map size. Only runs on cap-hit.
		live := int64(0)
		f.seenNonces.Range(func(_, _ any) bool {
			live++
			return true
		})
		f.seenNoncesCount.Store(live)
	}
	return deleted
}

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
func (f *Feishu) fetchBotInfo(ctx context.Context) error {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", f.baseURL+"/open-apis/bot/v3/info", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		return &APIError{Code: result.Code, Msg: result.Msg, Op: "bot_info"}
	}
	if result.Bot.OpenID == "" {
		return fmt.Errorf("bot_info: empty open_id in response")
	}

	f.botInfoMu.Lock()
	f.botOpenID = result.Bot.OpenID
	f.botInfoMu.Unlock()
	// Upstream body could carry C1/bidi/newline bytes under a TLS MITM.
	slog.Info("feishu bot identity",
		"open_id", osutil.SanitizeForLog(result.Bot.OpenID, 64),
		"app_name", osutil.SanitizeForLog(result.Bot.AppName, 128))
	return nil
}

// isBotMentioned reports whether any mention targets this bot; when botOpenID
// is unknown any mention counts, so a degraded Start does not drop responses.
// The extractor closure lets the webhook and WebSocket schemas share the logic.
func (f *Feishu) isBotMentioned(count int, openIDAt func(i int) string) bool {
	f.botInfoMu.RLock()
	botID := f.botOpenID
	f.botInfoMu.RUnlock()
	if botID == "" {
		// Degraded: kick a rate-limited re-fetch so the open_id self-heals and
		// the next group mention matches strictly (#1009).
		if count > 0 {
			f.maybeRefreshBotInfo()
		}
		return count > 0
	}
	for i := 0; i < count; i++ {
		if openIDAt(i) == botID {
			return true
		}
	}
	return false
}

// botInfoRefreshCooldown rate-limits the self-heal re-fetch: short enough to
// recover from a transient Start failure, long enough that a revoked app does
// not cost a per-message API call.
const botInfoRefreshCooldown = time.Minute

// maybeRefreshBotInfo kicks a rate-limited, singleflight-merged background
// re-fetch of the bot's open_id while isBotMentioned is degraded. Non-blocking;
// tracked on f.dispatch so Stop() waits for it.
func (f *Feishu) maybeRefreshBotInfo() {
	// nil only for directly-constructed test fixtures; no lifecycle to anchor to.
	if f.stopCtx == nil {
		return
	}
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&f.lastBotInfoFetchNs)
	// delta < 0 = NTP step backwards: re-anchor rather than wedge the cooldown.
	if delta := now - last; delta >= 0 && delta < int64(botInfoRefreshCooldown) {
		return
	}
	if !atomic.CompareAndSwapInt64(&f.lastBotInfoFetchNs, last, now) {
		return
	}
	// Stop() cancels stopCtx then waits on dispatch; an Add after the counter
	// drained would panic, so re-check cancellation after the CAS and before Go.
	if f.stopCtx.Err() != nil {
		return
	}
	f.dispatch.Go("feishu bot info refresh", func() {
		// Constant key: one bot identity per adapter.
		_, _, _ = f.botInfoSF.Do("bot_info", func() (any, error) {
			ctx, cancel := context.WithTimeout(f.stopCtx, 5*time.Second)
			defer cancel()
			if err := f.fetchBotInfo(ctx); err != nil {
				slog.Warn("feishu lazy bot info re-fetch failed — group mention filtering stays in 'any @' fallback",
					"err", err)
				return nil, err
			}
			slog.Info("feishu bot open_id self-healed via lazy re-fetch — group mentions now matched strictly")
			return nil, nil
		})
	})
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
	f.cleanupWg.Wait() // cleanupNonces goroutine
	return nil
}

// Reply sends a message to a Feishu chat. Handles text and/or images.
func (f *Feishu) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	var lastMsgID string

	if msg.Text != "" {
		id, err := f.sendText(ctx, msg.ChatID, msg.Text)
		if err != nil {
			// A token-expired error invalidates the cache so the retry has a fresh token.
			return "", f.maybeInvalidateOnTokenError(err)
		}
		lastMsgID = id
	}

	// Non-token image failures are log-and-continue (earlier parts landed),
	// but a token-invalidation error must PROPAGATE so ReplyWithRetry can
	// grant its rotation retry — otherwise a single-image reply hitting
	// token-expiry is lost silently (#2305).
	for _, img := range msg.Images {
		id, err := f.sendImage(ctx, msg.ChatID, img)
		if err != nil {
			if invErr := f.maybeInvalidateOnTokenError(err); platform.IsTokenInvalidated(invErr) {
				return lastMsgID, invErr
			}
			slog.Warn("feishu send image failed", "err", err)
			continue
		}
		lastMsgID = id
	}

	return lastMsgID, nil
}

func (f *Feishu) sendText(ctx context.Context, chatID, text string) (string, error) {
	// Always a card so EditMessage (PATCH) can later replace it with markdown;
	// plain text cannot be edited into card format.
	return f.sendCard(ctx, chatID, text)
}

// Feishu interactive card, schema 2.0 (required for full GFM: headings,
// fenced code, tables, blockquotes). Typed so json.Marshal avoids
// map[string]any boxing on every reply.
type feishuMarkdownElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}
type feishuCardBody struct {
	Elements [1]feishuMarkdownElement `json:"elements"`
}
type feishuCard struct {
	Schema string         `json:"schema"`
	Body   feishuCardBody `json:"body"`
}

// buildMarkdownCardJSON marshals a single-markdown-element card.
func buildMarkdownCardJSON(text string) ([]byte, error) {
	card := feishuCard{
		Schema: "2.0",
		Body: feishuCardBody{
			Elements: [1]feishuMarkdownElement{{Tag: "markdown", Content: text}},
		},
	}
	// No HTML escaping: `<` etc. would render literally in the markdown element.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(card); err != nil {
		return nil, err
	}
	// Strip the Encoder's trailing '\n'; the outer Marshal expects a pure value.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sendCard sends a Feishu interactive card with markdown content.
func (f *Feishu) sendCard(ctx context.Context, chatID, text string) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	cardJSON, err := buildMarkdownCardJSON(text)
	if err != nil {
		return "", fmt.Errorf("marshal card: %w", err)
	}
	// `content` must be stringified JSON, not a nested object.
	reqBody, err := json.Marshal(struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}{ReceiveID: chatID, MsgType: "interactive", Content: string(cardJSON)})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	return f.postMessage(ctx, token, reqBody)
}

// postMessage sends a prepared message payload to the Feishu API.
func (f *Feishu) postMessage(ctx context.Context, token string, reqBody []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/messages?receive_id_type=chat_id",
		bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		return "", &APIError{Code: result.Code, Msg: result.Msg, Op: "send"}
	}

	return result.Data.MessageID, nil
}

func (f *Feishu) sendImage(ctx context.Context, chatID string, img platform.Image) (string, error) {
	imageKey, err := f.uploadImage(ctx, img.Data, img.MimeType)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}

	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	// `content` must be stringified JSON, not a nested object.
	content, err := json.Marshal(struct {
		ImageKey string `json:"image_key"`
	}{ImageKey: imageKey})
	if err != nil {
		return "", fmt.Errorf("marshal content: %w", err)
	}
	reqBody, err := json.Marshal(struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}{ReceiveID: chatID, MsgType: "image", Content: string(content)})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	return f.postMessage(ctx, token, reqBody)
}

// DownloadImage downloads an image from a message via Feishu API.
func (f *Feishu) DownloadImage(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
	return f.downloadResource(ctx, messageID, fileKey, "image", maxImageDownloadBytes, "image/png")
}

// DownloadAudio downloads an audio file from a message via Feishu API.
func (f *Feishu) DownloadAudio(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
	return f.downloadResource(ctx, messageID, fileKey, "audio", maxAudioDownloadBytes, "audio/ogg")
}

// downloadResource downloads a message resource (image/audio) from the Feishu API.
func (f *Feishu) downloadResource(ctx context.Context, messageID, fileKey, resType string, maxBytes int64, defaultMIME string) ([]byte, string, error) {
	// math.MaxInt64 would overflow maxBytes+1 and degrade LimitReader to 0 bytes.
	if maxBytes <= 0 || maxBytes >= (1<<62) {
		return nil, "", fmt.Errorf("download %s: invalid maxBytes %d", resType, maxBytes)
	}
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/resources/"+url.PathEscape(fileKey)+"?type="+url.QueryEscape(resType), nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download %s: %w", resType, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// %q: upstream body reaches slog attrs; escape bidi/C1/newline.
		return nil, "", fmt.Errorf("download %s: status %d, body: %q", resType, resp.StatusCode, body)
	}

	// maxBytes+1 distinguishes "exactly at limit" from "silently truncated";
	// reject rather than deliver a truncated file.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read %s body: %w", resType, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("download %s: payload exceeds %d-byte limit", resType, maxBytes)
	}

	contentType := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = defaultMIME
	}

	// The Content-Type header is not authoritative; sniff the bytes. Go's
	// WHATWG sniffer reports OGG as `application/ogg`, hence the explicit
	// accept. Audio additionally passes the audioMagicOK allowlist so a
	// crafted payload cannot widen the ffmpeg/Whisper attack surface just by
	// looking audio/* to the sniffer.
	if len(data) > 0 {
		sniffed := http.DetectContentType(data)
		ok := true
		switch resType {
		case "image":
			ok = strings.HasPrefix(sniffed, "image/")
		case "audio":
			ok = (strings.HasPrefix(sniffed, "audio/") || sniffed == "application/ogg") && audioMagicOK(data)
		}
		if !ok {
			return nil, "", fmt.Errorf("download %s: mime mismatch (header=%s sniffed=%s)", resType, contentType, sniffed)
		}
	}
	return data, contentType, nil
}

// audioMagicOK reports whether data starts with a magic number of a format the
// transcribe pipeline handles: OGG, MP3 (ID3v2 or frame sync), WAV, MP4/M4A
// (allowlisted ftyp brands) or FLAC. AMR and raw AAC-ADTS are intentionally
// absent — Feishu voice is OGG/Opus or M4A; add new formats here explicitly
// rather than widening the sniffer fallback. Pure so tests can table-drive it.
func audioMagicOK(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	// OGG.
	if bytes.HasPrefix(data, []byte("OggS")) {
		return true
	}
	// ID3v2: require a known major version so an ASCII "ID3…" string cannot pass.
	if len(data) >= 5 && bytes.HasPrefix(data, []byte("ID3")) && data[3] >= 2 && data[3] <= 4 {
		return true
	}
	// Raw MP3 frame sync; the bare 0xFFE* variants are not used by Feishu voice.
	if data[0] == 0xFF {
		switch data[1] {
		case 0xF2, 0xF3, 0xFA, 0xFB:
			return true
		}
	}
	// WAV: RIFF + 4-byte size + "WAVE".
	if len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")) {
		return true
	}
	// ftyp box: 4-byte size | "ftyp" | brand. QuickTime / Flash brands are
	// rejected — spottier Whisper/ffmpeg compatibility, not on the Feishu surface.
	if len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) {
		switch string(data[8:12]) {
		case "M4A ", "mp4a", "isom", "mp42", "dash":
			return true
		}
	}
	// FLAC.
	if bytes.HasPrefix(data, []byte("fLaC")) {
		return true
	}
	return false
}

// replyError sends an error notice directly to the user on a short-lived ctx
// derived from stopCtx, because the caller's ctx is often already cancelled.
func (f *Feishu) replyError(_ context.Context, chatID, text string) {
	rctx, cancel := context.WithTimeout(f.stopCtx, 5*time.Second)
	defer cancel()
	if _, err := f.Reply(rctx, platform.OutgoingMessage{ChatID: chatID, Text: text}); err != nil {
		slog.Warn("feishu reply error failed", "err", err)
	}
}

// uploadImage uploads image data to Feishu and returns the image_key.
func (f *Feishu) uploadImage(ctx context.Context, data []byte, mimeType string) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	filename := "image" + platform.ImageExt(mimeType)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("image_type", "message"); err != nil {
		return "", fmt.Errorf("write image_type field: %w", err)
	}
	part, err := w.CreateFormFile("image", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write image data: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/images", &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}
	if result.Code != 0 {
		return "", &APIError{Code: result.Code, Msg: result.Msg, Op: "upload_image"}
	}
	return result.Data.ImageKey, nil
}

// EditMessage updates an existing card message via PATCH (all messages are cards).
func (f *Feishu) EditMessage(ctx context.Context, msgID string, text string) error {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	cardJSON, err := buildMarkdownCardJSON(text)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	reqBody, err := json.Marshal(struct {
		Content string `json:"content"`
	}{Content: string(cardJSON)})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	// PathEscape: a crafted ID with "/" or "?" must not redirect the PATCH.
	req, err := http.NewRequestWithContext(ctx, "PATCH",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(msgID),
		bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("edit message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode edit response: %w", err)
	}
	if result.Code != 0 {
		// Structured so errors.As-based token refresh works on the edit path too.
		return &APIError{Code: result.Code, Msg: result.Msg, Op: "edit"}
	}
	return nil
}

// reactionRequestBody is the JSON body sent to POST /reactions (hot path:
// one call per dispatched IM message, typed to avoid map allocations).
type reactionRequestBody struct {
	ReactionType reactionTypeField `json:"reaction_type"`
}

type reactionTypeField struct {
	EmojiType string `json:"emoji_type"`
}

// AddReaction implements platform.Reactor: creates the reaction and caches the
// returned reaction_id so RemoveReaction can delete by id.
func (f *Feishu) AddReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return fmt.Errorf("feishu AddReaction: empty messageID")
	}
	emojiType := reactionEmojiType(r)
	if emojiType == "" {
		return fmt.Errorf("feishu AddReaction: unsupported reaction %q", r)
	}
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	reqBody, err := json.Marshal(reactionRequestBody{
		ReactionType: reactionTypeField{EmojiType: emojiType},
	})
	if err != nil {
		return fmt.Errorf("marshal reaction request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reactions",
		bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create reaction request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post reaction: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			ReactionID string `json:"reaction_id"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode reaction response: %w", err)
	}
	if result.Code != 0 {
		// %q escapes bidi/C1/newline in the upstream msg.
		return fmt.Errorf("feishu reaction api: code=%d msg=%q", result.Code, result.Msg)
	}
	if result.Data.ReactionID != "" {
		// Expiry lets cleanupNoncesTick GC entries that never see RemoveReaction.
		f.reactionIDs.Store(reactionCacheKey(messageID, emojiType), reactionCacheEntry{
			id:     result.Data.ReactionID,
			expiry: time.Now().Add(reactionCacheTTL).UnixNano(),
		})
	}
	return nil
}

// RemoveReaction implements platform.Reactor via the cached reaction_id; with
// no cached id (restart between Add and Remove) it returns nil and the
// reaction lingers — acceptable for best-effort UX feedback.
func (f *Feishu) RemoveReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return nil
	}
	emojiType := reactionEmojiType(r)
	if emojiType == "" {
		return nil
	}
	cacheKey := reactionCacheKey(messageID, emojiType)
	// Load, not LoadAndDelete: evict only AFTER the DELETE is confirmed so a
	// transient failure keeps the id for retry and ⏳ can still be cleared (#1984).
	v, ok := f.reactionIDs.Load(cacheKey)
	if !ok {
		return nil
	}
	entry, ok := v.(reactionCacheEntry)
	if !ok || entry.id == "" {
		// A malformed entry can never produce a valid DELETE; drop it.
		f.reactionIDs.Delete(cacheKey)
		return nil
	}
	reactionID := entry.id
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE",
		f.baseURL+"/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(reactionID),
		nil)
	if err != nil {
		return fmt.Errorf("create delete reaction request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete reaction: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIRespBodyBytes)).Decode(&result); err != nil {
		return fmt.Errorf("decode delete reaction response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu delete reaction api: code=%d msg=%q", result.Code, result.Msg)
	}
	f.reactionIDs.Delete(cacheKey)
	return nil
}
