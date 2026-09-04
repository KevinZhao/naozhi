// Package weixin implements the WeChat iLink Bot platform adapter.
//
// Threat model: weixin uses iLink's outbound long-poll API — naozhi initiates
// every connection over TLS and presents the Token as a bearer credential.
// There is no inbound webhook to spoof or replay, so there is no HMAC /
// nonce / timestamp path here; the TLS handshake is the only inbound trust
// anchor and the Token (sent in request bodies, never logged) is the only
// credential. If iLink ever adds an inbound webhook, this package must grow a
// transport_hook.go mirroring feishu's (timestamp + nonce + signature) (#899).
package weixin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
)

// Config holds WeChat iLink Bot credentials.
type Config struct {
	Token       string
	BaseURL     string
	MaxReplyLen int
}

// Weixin implements Platform and RunnablePlatform via iLink Bot long-poll.
type Weixin struct {
	cfg     Config
	api     *apiClient
	handler platform.MessageHandler

	startMu   sync.Mutex
	started   bool
	cancel    context.CancelFunc
	cleanupWg sync.WaitGroup // tracks the token cleanup goroutine
	pollWg    sync.WaitGroup // tracks the pollLoop goroutine

	// dispatch bounds concurrent handler goroutines and lets Stop() drain them.
	dispatch platform.BoundedDispatch

	// contextTokens caches the latest context_token per user for reply, with
	// an update stamp so one-off users are evicted instead of accumulating.
	contextTokens sync.Map // map[userID]*tokenEntry
}

type tokenEntry struct {
	token     string
	updatedNs int64 // time.Now().UnixNano() at Store
}

// tokenTTL is the idle time after which a cached context_token is evicted;
// the next inbound message refreshes it.
const tokenTTL = 24 * time.Hour

// tokenCleanupInterval controls how often the eviction goroutine scans.
const tokenCleanupInterval = 1 * time.Hour

// maxIncomingTextBytes bounds per-message text handed to the dispatcher; the
// 2 MiB response budget covers batch polling, not one message.
const maxIncomingTextBytes = platform.DefaultMaxIncomingBytes

// maxWeixinMsgsPerPoll caps messages processed per poll: the 2 MB body cap
// would still let a hostile relay pack ~100k tiny records and spawn a
// goroutine each.
const maxWeixinMsgsPerPoll = 100

// validateBaseURLScheme requires HTTPS for the iLink base URL; loopback hosts
// are exempt so developers can wire local mock servers.
func validateBaseURLScheme(baseURL string) error {
	if baseURL == "" {
		// defaultBaseURL is https://.
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("weixin base_url parse: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("weixin base_url must use https:// (got %q); the iLink poll response carries no HMAC, so TLS is the only authenticity guarantee", baseURL)
}

// baseURLIsTLS reports whether the relay is reached over TLS; only an approved
// loopback http:// mock is not, and then authenticity has no backing at all.
func (w *Weixin) baseURLIsTLS() bool {
	if w.cfg.BaseURL == "" {
		return true // defaultBaseURL is https://
	}
	u, err := url.Parse(w.cfg.BaseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https")
}

// New creates a WeChat platform adapter.
func New(cfg Config) *Weixin {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = platform.DefaultMaxReplyLen
	}
	return &Weixin{
		cfg:      cfg,
		api:      newAPIClient(cfg.BaseURL, cfg.Token),
		dispatch: platform.BoundedDispatch{Name: "weixin"},
	}
}

func (w *Weixin) Name() string { return "weixin" }

func (w *Weixin) MaxReplyLength() int { return w.cfg.MaxReplyLen }

// SupportsInterimMessages returns false — iLink Bot context_token is single-use.
func (w *Weixin) SupportsInterimMessages() bool { return false }

// UsesSingleUseReplyToken returns true — the context_token is consumed by the
// first Reply, so the dispatcher must collapse long replies (#2136).
func (w *Weixin) UsesSingleUseReplyToken() bool { return true }

// RegisterRoutes is a no-op (long-poll, no inbound HTTP).
func (w *Weixin) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

// Start implements RunnablePlatform. Launches getUpdates long-poll loop.
func (w *Weixin) Start(handler platform.MessageHandler) error {
	// The poll response is fully trusted (no HMAC), so without TLS a MITM could
	// inject arbitrary from_user_id / prompt text.
	if err := validateBaseURLScheme(w.cfg.BaseURL); err != nil {
		return err
	}
	// Surface the trust posture in journalctl; a non-TLS loopback mock has no
	// authenticity anchor at all and MUST NOT face untrusted networks.
	if w.baseURLIsTLS() {
		slog.Info("weixin: inbound long-poll has no HMAC; authenticity relies on TLS to the iLink relay (see package threat model)")
	} else {
		slog.Warn("weixin: relay base_url is non-HTTPS loopback — NO TLS and NO HMAC; dev-only, do not expose to untrusted networks",
			"base_url", w.cfg.BaseURL)
	}
	w.startMu.Lock()
	if w.started {
		w.startMu.Unlock()
		return fmt.Errorf("weixin platform already started")
	}
	w.started = true
	// Lifecycle handles and wg.Add happen under startMu, before the spawns, so
	// a concurrent Stop() sees a fully-initialised cancel and cannot miss a Wait.
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.handler = handler
	w.pollWg.Add(1)
	w.cleanupWg.Add(1)
	w.startMu.Unlock()

	go func() {
		defer w.pollWg.Done()
		w.pollLoop(ctx)
	}()

	go func() {
		defer w.cleanupWg.Done()
		w.cleanupTokensLoop(ctx)
	}()

	slog.Info("weixin platform started", "base_url", w.cfg.BaseURL)
	return nil
}

// Stop implements RunnablePlatform.
func (w *Weixin) Stop() error {
	// Snapshot under startMu so a pre-Start or racing Stop() never sees a torn
	// cancel; release before the blocking Waits.
	w.startMu.Lock()
	cancel := w.cancel
	started := w.started
	w.startMu.Unlock()
	if !started || cancel == nil {
		return nil
	}
	cancel()
	w.pollWg.Wait()
	w.dispatch.Wait()
	w.cleanupWg.Wait()
	return nil
}

// cleanupTokensLoop evicts context_token entries idle for longer than tokenTTL.
// Prevents unbounded growth under high user churn.
func (w *Weixin) cleanupTokensLoop(ctx context.Context) {
	ticker := time.NewTicker(tokenCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-tokenTTL).UnixNano()
			w.contextTokens.Range(func(k, v any) bool {
				if e, ok := v.(*tokenEntry); ok && e.updatedNs < cutoff {
					w.contextTokens.Delete(k)
				}
				return true
			})
		}
	}
}

// Reply sends a text message to a WeChat user.
func (w *Weixin) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	// Images are dropped with a warning — the iLink Bot API takes no attachments.
	if len(msg.Images) > 0 {
		slog.Warn("weixin: image attachments are not supported; dropping images",
			"chat", osutil.SanitizeForLog(msg.ChatID, 128),
			"image_count", len(msg.Images))
	}
	if msg.Text == "" {
		return "", nil
	}

	ct, _ := w.contextTokens.Load(msg.ChatID)
	entry, _ := ct.(*tokenEntry)
	var contextToken string
	if entry != nil {
		contextToken = entry.token
	}
	if contextToken == "" {
		// ChatID comes from the relay; sanitize before it reaches err.Error().
		return "", fmt.Errorf("weixin: no context_token for user %q (no inbound message yet)",
			osutil.SanitizeForLog(msg.ChatID, 128))
	}

	if err := w.api.sendMessage(ctx, msg.ChatID, msg.Text, contextToken); err != nil {
		return "", fmt.Errorf("weixin send: %w", err)
	}
	// Sanitized ChatID: downstream slog/IM surfaces print the id verbatim.
	return fmt.Sprintf("weixin:%s:%d", osutil.SanitizeForLog(msg.ChatID, 128), time.Now().UnixMilli()), nil
}

// EditMessage is not supported by WeChat iLink Bot API.
func (w *Weixin) EditMessage(_ context.Context, _ string, _ string) error {
	return nil
}

// pollLoop runs the getUpdates long-poll loop until ctx is cancelled.
func (w *Weixin) pollLoop(ctx context.Context) {
	var cursor string
	consecutiveFailures := 0
	const maxFailures = 3
	const backoffDelay = 30 * time.Second
	const retryDelay = 2 * time.Second

	for {
		if ctx.Err() != nil {
			slog.Info("weixin poll loop stopped")
			return
		}

		pollCtx, pollCancel := context.WithTimeout(ctx, defaultLongPollTimeout+5*time.Second)
		resp, err := w.api.getUpdates(pollCtx, cursor)
		pollCancel()

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveFailures++
			slog.Error("weixin getUpdates error",
				"err", err,
				"failures", fmt.Sprintf("%d/%d", consecutiveFailures, maxFailures),
			)
			if consecutiveFailures >= maxFailures {
				consecutiveFailures = 0
				sleepCtx(ctx, backoffDelay)
			} else {
				sleepCtx(ctx, retryDelay)
			}
			continue
		}

		if resp.Ret != 0 || resp.ErrCode != 0 {
			consecutiveFailures++
			slog.Error("weixin getUpdates API error",
				"ret", resp.Ret,
				"errcode", resp.ErrCode,
				"errmsg", osutil.SanitizeForLog(resp.ErrMsg, 256),
				"failures", fmt.Sprintf("%d/%d", consecutiveFailures, maxFailures),
			)
			if consecutiveFailures >= maxFailures {
				consecutiveFailures = 0
				sleepCtx(ctx, backoffDelay)
			} else {
				sleepCtx(ctx, retryDelay)
			}
			continue
		}

		consecutiveFailures = 0

		if resp.GetUpdatesBuf != "" {
			cursor = resp.GetUpdatesBuf
		}

		// Truncate rather than drop the poll so the cursor still advances.
		if len(resp.Msgs) > maxWeixinMsgsPerPoll {
			slog.Warn("weixin poll: msg count exceeds cap, truncating",
				"count", len(resp.Msgs), "cap", maxWeixinMsgsPerPoll)
			resp.Msgs = resp.Msgs[:maxWeixinMsgsPerPoll]
		}

		for _, msg := range resp.Msgs {
			if msg.MessageType != msgTypeUser {
				continue
			}

			text := extractText(msg)
			if text == "" {
				// Image/audio/other attachment we do not forward; Debug so media
				// bursts do not flood operator logs.
				slog.Debug("weixin non-text message ignored",
					"from", osutil.SanitizeForLog(msg.FromUserID, 128),
					"msg_id", msg.MessageID,
					"items", len(msg.ItemList))
				continue
			}
			if len(text) > maxIncomingTextBytes {
				slog.Warn("weixin text exceeds cap, dropping",
					"from", osutil.SanitizeForLog(msg.FromUserID, 128),
					"msg_id", msg.MessageID,
					"size", len(text), "cap", maxIncomingTextBytes)
				continue
			}

			from := msg.FromUserID
			if from == "" {
				continue
			}

			// Bound the cached token so a misbehaving relay cannot pin arbitrary
			// memory per user for the TTL window; real tokens are UUID-scale.
			const maxContextTokenLen = 512
			if msg.ContextToken != "" && len(msg.ContextToken) <= maxContextTokenLen {
				w.contextTokens.Store(from, &tokenEntry{
					token:     msg.ContextToken,
					updatedNs: time.Now().UnixNano(),
				})
			} else if len(msg.ContextToken) > maxContextTokenLen {
				// Replies to this user will fail; log the length only, never the token.
				slog.Warn("weixin context_token exceeds cap, dropping (replies to this user will fail)",
					"from", osutil.SanitizeForLog(from, 128),
					"size", len(msg.ContextToken), "cap", maxContextTokenLen)
			}

			// EventID feeds the single cross-platform Dedup, so a bare integer
			// message_id must be namespaced by platform + sender (#2116). With
			// no message_id, leave EventID empty and give the dispatch fallback
			// key (fallback:<platform>:<chatID>:<MessageID>:<minute>) a
			// per-message distinguisher, or two distinct messages from one user
			// in the same minute collide (#2117).
			eventID := ""
			fallbackMsgID := ""
			if msg.MessageID != 0 {
				eventID = "weixin:" + from + ":" + strconv.Itoa(msg.MessageID)
			} else if msg.Seq != 0 {
				fallbackMsgID = "seq:" + strconv.Itoa(msg.Seq)
			} else if msg.CreateTimeMs != 0 {
				fallbackMsgID = "ts:" + strconv.FormatInt(msg.CreateTimeMs, 10)
			}
			incoming := platform.IncomingMessage{
				Platform:  "weixin",
				EventID:   eventID,
				MessageID: fallbackMsgID,
				UserID:    from,
				ChatID:    from, // direct chat, reply to the sender
				ChatType:  "direct",
				Text:      text,
				MentionMe: true, // direct messages always mention the bot
			}

			w.dispatch.TryGo("weixin", func() { w.handler(ctx, incoming) },
				"user", osutil.SanitizeForLog(from, 128))
		}
	}
}

// extractText returns the concatenated text from a message's item_list.
func extractText(msg weixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == msgItemTypeText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
