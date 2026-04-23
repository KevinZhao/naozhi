package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/transcribe"
	"golang.org/x/sync/singleflight"
)

var feishuHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		// open.feishu.cn supports TLS 1.2+; pin the floor so a future Go
		// toolchain regression can't silently accept legacy protocols.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	},
	// Block all redirects. The Feishu Open API does not rely on redirects
	// for any documented flow (token fetch, send, upload, resource
	// download). Following a redirect would let a compromised upstream
	// (or DNS attacker mid-handshake before the cached cert is served)
	// direct the bearer-token-carrying request at an internal address
	// (IMDS, loopback admin port, etc.) — a classic SSRF-via-redirect.
	// Returning ErrUseLastResponse makes the client surface the 3xx
	// response as-is so the caller fails cleanly instead of following.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// APIError is the typed error returned by Feishu Open API calls (token
// fetch, send message, upload image). Callers can use errors.As to inspect
// Code and decide retry policy via IsPermanent — rate-limit / 5xx codes
// should be retried, invalid-credential codes should not.
type APIError struct {
	Code int
	Msg  string
	Op   string // "send", "token", "upload", etc. — for diagnostic context
}

func (e *APIError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("feishu %s: code=%d msg=%s", e.Op, e.Code, e.Msg)
	}
	return fmt.Sprintf("feishu %s: code=%d", e.Op, e.Code)
}

// IsPermanent reports whether the error indicates a non-transient condition
// (app credentials invalid, app disabled by vendor) where retrying with the
// same request will never succeed. Used by reconnect loops to break out
// instead of hammering the API forever.
//
// Code references: open.feishu.cn/document/server-docs/getting-started/server-error-codes
//   - 99991663: invalid app_secret
//   - 99991664: app disabled
//   - 99991668: app not authorized
//   - 1061045: bot not in chat (permanent for that chat)
//   - 230001: invalid receive_id
func (e *APIError) IsPermanent() bool {
	switch e.Code {
	case 99991663, 99991664, 99991668, 1061045, 230001:
		return true
	}
	return false
}

// Config holds Feishu app credentials.
type Config struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	ConnectionMode    string `yaml:"connection_mode"` // "websocket" (default) | "webhook"
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLen       int    `yaml:"max_reply_length"`
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

	// Token refresh circuit breaker: if the upstream token endpoint returns
	// an error (e.g. app_secret revoked), subsequent refresh attempts within
	// tokenFailCooldown are short-circuited to the cached error. Prevents
	// hammering open.feishu.cn at the per-request rate when every reply path
	// needs a token. singleflight alone does not cache errors.
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
	wg      sync.WaitGroup // tracks in-flight message handler goroutines
	hookSem chan struct{}  // limits concurrent webhook handler goroutines
	startMu sync.Mutex
	started bool

	// cleanupWg tracks the cleanupNonces goroutine so Stop() can wait it out.
	cleanupWg sync.WaitGroup

	// Replay protection: stores "ts:nonce" -> expiry unix timestamp.
	seenNonces sync.Map
	// seenNoncesCount tracks the approximate size of seenNonces so we can
	// refuse new inserts past maxSeenNonces without the O(n) scan Range
	// would require. Incremented on successful LoadOrStore-miss,
	// decremented by cleanupNonces on expiry. Concurrent Add → eventual
	// consistency: at worst we accept a few extra entries between the
	// check and increment, which is bounded and harmless.
	seenNoncesCount atomic.Int64

	// reactionIDs caches (messageID + emoji_type) -> reaction_id returned by
	// the create-reaction API, so RemoveReaction can later target the correct
	// reaction. Feishu's delete endpoint requires the reaction_id (there's no
	// "delete by emoji type" form). Entries are deleted on successful removal.
	reactionIDs sync.Map
}

// New creates a Feishu platform adapter. transcriber may be nil to disable voice.
func New(cfg Config, transcriber transcribe.Service) *Feishu {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = 4000
	}
	mode := cfg.ConnectionMode
	if mode == "" {
		mode = "websocket"
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Feishu{cfg: cfg, mode: mode, baseURL: "https://open.feishu.cn", transcriber: transcriber, hookSem: make(chan struct{}, 20), stopCtx: ctx, stopCancel: cancel}
	f.cleanupWg.Add(1)
	go func() {
		defer f.cleanupWg.Done()
		f.cleanupNonces(ctx)
	}()
	return f
}

// cleanupNonces periodically removes expired entries from seenNonces.
// Runs until ctx is cancelled (i.e. until Stop() is called).
//
// Aligned with verifyTimestamp's 5-minute freshness window: a request older
// than 5 min is rejected by timestamp verification, so holding nonces beyond
// that window just bloats the map without any replay-defense value.
const nonceTTL = 5 * time.Minute

// maxSeenNonces caps the replay-protection map so a flood of authenticated
// requests with unique nonces cannot bloat memory past a predictable ceiling.
// 50k entries × (~48B key + 24B value) ≈ 3.6 MB, well below a typical
// heap budget. Legitimate traffic with 5-minute TTL is far below this cap.
const maxSeenNonces = 50000

// tokenFailCooldown bounds how long a failed tenant-access-token refresh is
// cached so concurrent callers do not re-hit open.feishu.cn on every reply
// when credentials are revoked. 5s balances operator-visible recovery time
// with upstream rate protection.
const tokenFailCooldown = 5 * time.Second

func (f *Feishu) cleanupNonces(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now().Unix()
			deleted := int64(0)
			f.seenNonces.Range(func(k, v any) bool {
				// Defensive type assertion: sync.Map has no compile-time type
				// safety, so guard against accidental cross-type Store from a
				// future refactor. Drop malformed entries so the map recovers.
				ts, ok := v.(int64)
				if !ok || ts < now {
					f.seenNonces.Delete(k)
					deleted++
				}
				return true
			})
			if deleted > 0 {
				// Clamp at zero: every webhook insert MUST pair with Add(1),
				// but defensive type assertion above can delete entries that
				// bypassed the counted insert path (future refactor risk). A
				// negative counter would eventually bump legitimate traffic
				// against the maxSeenNonces ceiling until restart.
				if n := f.seenNoncesCount.Add(-deleted); n < 0 {
					f.seenNoncesCount.Store(0)
				}
			}
		case <-ctx.Done():
			return
		}
	}
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
	if f.mode == "websocket" {
		slog.Info("feishu using websocket mode (no public IP needed)")
		return f.startWebSocket()
	}
	// Webhook mode exposes a public HTTP endpoint. Refuse to start if neither
	// VerificationToken nor EncryptKey is configured — without either, any
	// caller on the open internet can inject forged events.
	if f.cfg.VerificationToken == "" && f.cfg.EncryptKey == "" {
		return fmt.Errorf("feishu webhook mode requires verification_token or encrypt_key to be configured")
	}
	// VerificationToken-only mode relies on a plaintext shared secret in the
	// request body; if that token ever leaks, events can be forged without
	// access to the EncryptKey HMAC. Surface a startup warning so operators
	// know to configure EncryptKey as well. Not fatal — existing v1-only
	// deployments remain functional.
	if f.cfg.EncryptKey == "" {
		slog.Warn("feishu webhook: verification_token-only mode is less secure than encrypt_key HMAC — configure encrypt_key for defence-in-depth")
	}
	slog.Info("feishu using webhook mode")
	return nil
}

// Stop implements RunnablePlatform. Stops WebSocket connection.
func (f *Feishu) Stop() error {
	f.startMu.Lock()
	cancel := f.cancel
	done := f.done
	f.startMu.Unlock()

	// Cancel lifecycle context so webhook goroutines respond to shutdown.
	f.stopCancel()

	if cancel != nil {
		cancel()
		// SDK's Start() may block indefinitely (select{}); don't wait forever.
		// Use NewTimer + defer Stop so the fast path (SDK exits cleanly)
		// doesn't leave a 5s timer goroutine parked until the timeout elapses.
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			slog.Warn("feishu websocket stop timed out")
		}
	}
	f.wg.Wait()        // always wait for in-flight message handlers to finish
	f.cleanupWg.Wait() // wait for cleanupNonces goroutine to exit
	return nil
}

// Reply sends a message to a Feishu chat. Handles text and/or images.
func (f *Feishu) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	var lastMsgID string

	// Send text message
	if msg.Text != "" {
		id, err := f.sendText(ctx, msg.ChatID, msg.Text)
		if err != nil {
			return "", err
		}
		lastMsgID = id
	}

	// Send image messages
	for _, img := range msg.Images {
		id, err := f.sendImage(ctx, msg.ChatID, img)
		if err != nil {
			slog.Warn("feishu send image failed", "err", err)
			continue
		}
		lastMsgID = id
	}

	return lastMsgID, nil
}

func (f *Feishu) sendText(ctx context.Context, chatID, text string) (string, error) {
	// Always send as card so EditMessage (PATCH) can later update with markdown.
	// Previously, plain text messages couldn't be edited to card format, causing
	// the thinking status message + final reply to appear as two separate messages.
	return f.sendCard(ctx, chatID, text)
}

// buildMarkdownCardJSON marshals a Feishu interactive card (schema 2.0) with
// a single markdown element.
//
// Schema 2.0 is required for full GitHub-flavored markdown rendering —
// headings (#/##/###), fenced code blocks, tables, and blockquotes. The
// legacy 1.0 shape (bare "elements" array) only supports a restricted subset
// (bold/italic/links/lists) so Claude-style output rendered as plain text.
// See: open.feishu.cn/document/feishu-cards/quick-start
func buildMarkdownCardJSON(text string) ([]byte, error) {
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag":     "markdown",
					"content": text,
				},
			},
		},
	}
	// Disable HTML escaping so Claude output containing `<`, `>`, `&`
	// (common in code blocks, shell redirection, arrow operators) is
	// preserved verbatim in the Feishu card. Default json.Marshal would
	// emit `\u003c` / `\u003e` / `\u0026` which renders as literal
	// escape sequences inside the markdown element.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(card); err != nil {
		return nil, err
	}
	// json.Encoder appends a trailing '\n'; strip it so the result is a
	// clean JSON object (the downstream outer Marshal expects a pure
	// JSON value, not NDJSON).
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
	reqBody, err := json.Marshal(map[string]any{
		"receive_id": chatID,
		"msg_type":   "interactive",
		"content":    string(cardJSON),
	})
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
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

	content, err := json.Marshal(map[string]string{"image_key": imageKey})
	if err != nil {
		return "", fmt.Errorf("marshal content: %w", err)
	}
	reqBody, err := json.Marshal(map[string]any{
		"receive_id": chatID,
		"msg_type":   "image",
		"content":    string(content),
	})
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

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
		return "", fmt.Errorf("send image message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		// Return typed error so callers can use IsPermanent to short-circuit
		// retries on e.g. invalid token / permission errors.
		return "", &APIError{Code: result.Code, Msg: result.Msg, Op: "send_image"}
	}
	return result.Data.MessageID, nil
}

// DownloadImage downloads an image from a message via Feishu API.
func (f *Feishu) DownloadImage(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
	return f.downloadResource(ctx, messageID, fileKey, "image", 10*1024*1024, "image/png")
}

// DownloadAudio downloads an audio file from a message via Feishu API.
func (f *Feishu) DownloadAudio(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
	return f.downloadResource(ctx, messageID, fileKey, "audio", 20*1024*1024, "audio/ogg")
}

// downloadResource downloads a message resource (image/audio) from the Feishu API.
func (f *Feishu) downloadResource(ctx context.Context, messageID, fileKey, resType string, maxBytes int64, defaultMIME string) ([]byte, string, error) {
	// Guard against a caller passing math.MaxInt64 (which would overflow
	// maxBytes+1 below and degrade LimitReader to 0-byte reads). No current
	// caller does this, but the contract should be self-protecting.
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
		return nil, "", fmt.Errorf("download %s: status %d, body: %s", resType, resp.StatusCode, body)
	}

	// Read up to maxBytes+1 so we can distinguish "exactly maxBytes" (legal)
	// from "exceeds maxBytes" (silently truncated by LimitReader). If we read
	// exactly maxBytes+1 bytes, the payload was larger than the limit and we
	// reject it rather than delivering a truncated file to the CLI.
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

	// Content-based verification: the Content-Type header is upstream-
	// provided and not authoritative (SSRF or compromised proxy could
	// deliver arbitrary bytes labeled as image/png). http.DetectContentType
	// sniffs the first 512 bytes; reject anything whose detected family does
	// not match the expected resource type.
	//
	// Feishu voice messages are OGG/Opus. Go's sniffer implements the WHATWG
	// MIME-Sniffing standard which emits `application/ogg` (not `audio/ogg`)
	// for OGG containers, and returns `application/octet-stream` for formats
	// it does not know (e.g. Opus-in-WebM). The accept-list below covers the
	// OGG case explicitly while still rejecting clearly-wrong families
	// (image/*, text/*, etc.).
	if len(data) > 0 {
		sniffed := http.DetectContentType(data)
		ok := true
		switch resType {
		case "image":
			ok = strings.HasPrefix(sniffed, "image/")
		case "audio":
			ok = strings.HasPrefix(sniffed, "audio/") || sniffed == "application/ogg"
		}
		if !ok {
			return nil, "", fmt.Errorf("download %s: mime mismatch (header=%s sniffed=%s)", resType, contentType, sniffed)
		}
	}
	return data, contentType, nil
}

// replyError sends an error message directly to the user, bypassing Claude.
// Uses a short-lived context derived from stopCtx rather than the caller's
// ctx: callers often pass a ctx that may already be cancelled (e.g. the
// webhook handler's ctx tied to the HTTP request, or a ctx timed out while
// downloading an image), and we still want the user-facing error notice to
// land. stopCtx is cancelled only at Feishu.Stop().
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

	// Derive filename extension from MIME type
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}
	if result.Code != 0 {
		return "", &APIError{Code: result.Code, Msg: result.Msg, Op: "upload_image"}
	}
	return result.Data.ImageKey, nil
}

// EditMessage updates an existing Feishu card message via PATCH.
// All messages are sent as cards (interactive), so we always use the card PATCH API.
func (f *Feishu) EditMessage(ctx context.Context, msgID string, text string) error {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	cardJSON, err := buildMarkdownCardJSON(text)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	reqBody, err := json.Marshal(map[string]string{
		"content": string(cardJSON),
	})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	// PathEscape the msgID: like AddReaction/RemoveReaction/downloadResource,
	// protect against a crafted ID containing "/" or "?" that could redirect
	// the PATCH to a different Open-API endpoint.
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("decode edit response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu edit error: code=%d msg=%s", result.Code, result.Msg)
	}
	return nil
}

// getAccessToken returns a valid tenant access token, refreshing if needed.
// Uses singleflight to merge concurrent refresh requests.
//
// The caller's ctx is intentionally ignored for the refresh request: when
// many request goroutines collide on an expired token, singleflight merges
// them into one HTTP call; honouring any single caller's cancellation would
// abort the shared refresh and fail every merged caller. Instead we bound
// the refresh with f.stopCtx + 10s so Stop() still aborts it promptly.
func (f *Feishu) getAccessToken(_ context.Context) (string, error) {
	// Fast path: single RLock checks both cached-token freshness and the
	// circuit-breaker. Splitting these into two separate RLock blocks used
	// to create a window where a concurrent refresh could mutate
	// tokenLastFailed between the two reads, letting a stale token be
	// returned or the circuit breaker be bypassed.
	f.tokenMu.RLock()
	if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
		token := f.accessToken
		f.tokenMu.RUnlock()
		return token, nil
	}
	if f.tokenLastFailed != nil && time.Since(f.tokenLastFailAt) < tokenFailCooldown {
		err := f.tokenLastFailed
		f.tokenMu.RUnlock()
		return "", err
	}
	f.tokenMu.RUnlock()

	// Slow path: singleflight merges concurrent refresh calls
	v, err, _ := f.tokenGroup.Do("token", func() (any, error) {
		// Double-check under read lock
		f.tokenMu.RLock()
		if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
			token := f.accessToken
			f.tokenMu.RUnlock()
			return token, nil
		}
		f.tokenMu.RUnlock()

		reqBody, err := json.Marshal(map[string]string{
			"app_id":     f.cfg.AppID,
			"app_secret": f.cfg.AppSecret,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal token request: %w", err)
		}

		// Derive the refresh context from the long-lived stopCtx rather than
		// the caller's ctx so one caller's cancellation does not torpedo the
		// singleflight-merged refresh for all concurrent callers. Stop() still
		// aborts by cancelling stopCtx. singleflight shares the returned
		// (v, err) value with late callers — they never see refreshCtx — so
		// the `defer cancel()` here only bounds the in-flight HTTP request.
		refreshCtx, cancel := context.WithTimeout(f.stopCtx, 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(refreshCtx, "POST",
			f.baseURL+"/open-apis/auth/v3/tenant_access_token/internal",
			bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("create token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := feishuHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request token: %w", err)
		}
		defer resp.Body.Close()

		var result struct {
			Code              int    `json:"code"`
			TenantAccessToken string `json:"tenant_access_token"`
			Expire            int    `json:"expire"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			return nil, fmt.Errorf("decode token response: %w", err)
		}
		if result.Code != 0 {
			return nil, &APIError{Code: result.Code, Op: "token"}
		}
		// Feishu normally returns Expire≈7200, but edge cases (clock skew,
		// API misbehaviour) can yield 0 or very small values. Without the
		// clamp `result.Expire-60` underflows to a negative number, so
		// `time.Now().Add(...)` produces an already-expired deadline and every
		// subsequent call would fire a fresh refresh. Treat anything below 60s
		// as "honour the 30s minimum caching window" to keep singleflight effective.
		if result.TenantAccessToken == "" {
			return nil, &APIError{Code: result.Code, Msg: "empty token", Op: "token"}
		}
		ttl := time.Duration(result.Expire-60) * time.Second
		if ttl < 30*time.Second {
			ttl = 30 * time.Second
		}

		f.tokenMu.Lock()
		f.accessToken = result.TenantAccessToken
		f.tokenExpiry = time.Now().Add(ttl)
		// Clear circuit breaker on success so a transient failure does not
		// keep blocking future refreshes after recovery.
		f.tokenLastFailed = nil
		f.tokenLastFailAt = time.Time{}
		f.tokenMu.Unlock()

		return result.TenantAccessToken, nil
	})
	if err != nil {
		// Record the failure so subsequent callers within the cooldown
		// short-circuit without another HTTP round-trip.
		f.tokenMu.Lock()
		f.tokenLastFailed = err
		f.tokenLastFailAt = time.Now()
		f.tokenMu.Unlock()
		return "", err
	}
	// Defensive type assertion: singleflight.Do returns `any` and the callback
	// path already returns a string, but guard against accidental refactor
	// regression (e.g., returning a struct wrapper) rather than panicking
	// on hot auth path.
	token, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("unexpected token type %T", v)
	}
	return token, nil
}

// verifySignature verifies the request signature (for encrypt_key mode).
// Uses the incremental hash.Hash interface to avoid copying the body into a
// concatenated string — webhook bodies can be up to 64 KB, and the old
// `timestamp + nonce + encryptKey + string(body)` path allocated ~64 KB per
// request and did it twice (once for the string, once for the []byte cast).
// Also hex-encodes via encoding/hex to avoid the fmt.Sprintf "%x" parse
// overhead, and compares as bytes under ConstantTimeCompare without stringy
// intermediate allocation.
func verifySignature(timestamp, nonce, encryptKey string, body []byte, signature string) bool {
	if encryptKey == "" {
		return true
	}
	h := sha256.New()
	h.Write([]byte(timestamp))
	h.Write([]byte(nonce))
	h.Write([]byte(encryptKey))
	h.Write(body)
	var sumBuf [sha256.Size]byte
	sum := h.Sum(sumBuf[:0])
	var hexBuf [sha256.Size * 2]byte
	hex.Encode(hexBuf[:], sum)
	return subtle.ConstantTimeCompare(hexBuf[:], []byte(signature)) == 1
}

// verifyTimestamp checks that the request timestamp is within 5 minutes of now.
func verifyTimestamp(timestamp string) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	diff := time.Now().Unix() - ts
	if diff < 0 {
		diff = -diff
	}
	return diff <= 300 // 5 minutes
}

// reactionEmojiType maps platform-agnostic ReactionType to Feishu emoji_type.
// Feishu's reaction API uses string emoji_types (see OpenAPI docs). Unknown
// types return "" so callers can skip.
func reactionEmojiType(r platform.ReactionType) string {
	switch r {
	case platform.ReactionQueued:
		// HOURGLASS hints "waiting" without implying success or failure.
		return "HOURGLASS"
	}
	return ""
}

// reactionCacheKey builds the (msgID, emojiType) composite key for reactionIDs.
func reactionCacheKey(messageID, emojiType string) string {
	return messageID + "|" + emojiType
}

// AddReaction implements platform.Reactor. Creates a reaction on messageID
// via POST /open-apis/im/v1/messages/:msg_id/reactions and caches the
// returned reaction_id so RemoveReaction can later delete by id.
//
// Returns nil on HTTP success. Server-side "already reacted" errors are
// treated as success (the reaction_id is still returned by Feishu). All
// other API errors are wrapped.
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
	reqBody, err := json.Marshal(map[string]any{
		"reaction_type": map[string]string{"emoji_type": emojiType},
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("decode reaction response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu reaction api: code=%d msg=%s", result.Code, result.Msg)
	}
	if result.Data.ReactionID != "" {
		f.reactionIDs.Store(reactionCacheKey(messageID, emojiType), result.Data.ReactionID)
	}
	return nil
}

// RemoveReaction implements platform.Reactor. Deletes a previously added
// reaction by consulting the cached reaction_id. If no id is cached (e.g.,
// process restart between Add and Remove), returns nil silently — the
// reaction will linger but that is acceptable for best-effort UX feedback.
func (f *Feishu) RemoveReaction(ctx context.Context, messageID string, r platform.ReactionType) error {
	if messageID == "" {
		return nil
	}
	emojiType := reactionEmojiType(r)
	if emojiType == "" {
		return nil
	}
	cacheKey := reactionCacheKey(messageID, emojiType)
	v, ok := f.reactionIDs.LoadAndDelete(cacheKey)
	if !ok {
		return nil
	}
	reactionID, _ := v.(string)
	if reactionID == "" {
		return nil
	}
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("decode delete reaction response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu delete reaction api: code=%d msg=%s", result.Code, result.Msg)
	}
	return nil
}
