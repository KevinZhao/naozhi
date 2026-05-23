package weixin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
	handlerWg sync.WaitGroup // tracks in-flight message handler goroutines
	cleanupWg sync.WaitGroup // tracks the token cleanup goroutine

	// hookSem caps concurrent inbound-message handler goroutines spawned
	// per poll cycle. The iLink relay can return a batch of messages in a
	// single getUpdates response; a misbehaving or compromised relay
	// returning thousands per poll would otherwise spawn unbounded
	// goroutines simultaneously. Mirrors feishu/slack hookSem (R235-SEC-9).
	hookSem chan struct{}

	// contextTokens caches the latest context_token per user for reply.
	// Value is *tokenEntry (token + last-updated unix seconds) so we can
	// drop entries whose tokens have gone stale — otherwise users who
	// message once and never return accumulate forever.
	contextTokens sync.Map // map[userID]*tokenEntry
}

// weixinHookSemSize bounds concurrent in-flight handler goroutines per
// poll cycle. 20 matches the feishu webhook hookSem cap; legitimate iLink
// relays rarely return more than a handful of messages per poll.
const weixinHookSemSize = 20

type tokenEntry struct {
	token     string
	updatedNs int64 // time.Now().UnixNano() at Store
}

// tokenTTL is the idle time after which a cached context_token is evicted.
// iLink tokens are short-lived anyway; aggressive eviction is safe because
// the next inbound message refreshes it.
const tokenTTL = 24 * time.Hour

// cleanupInterval controls how often the background goroutine scans.
const tokenCleanupInterval = 1 * time.Hour

// maxIncomingTextBytes bounds the per-message text handed to the dispatcher.
// Aliases platform.DefaultMaxIncomingBytes (R230C-ARCH-6). iLink's 2 MiB
// response budget covers batch polling, not individual messages; without a
// per-message cap a single user (or a compromised iLink relay) can push
// megabyte text through every cron/send path, amplifying stdin bytes on
// each replay.
const maxIncomingTextBytes = platform.DefaultMaxIncomingBytes

// New creates a WeChat platform adapter.
func New(cfg Config) *Weixin {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = platform.DefaultMaxReplyLen
	}
	return &Weixin{
		cfg:     cfg,
		api:     newAPIClient(cfg.BaseURL, cfg.Token),
		hookSem: make(chan struct{}, weixinHookSemSize),
	}
}

func (w *Weixin) Name() string { return "weixin" }

func (w *Weixin) MaxReplyLength() int { return w.cfg.MaxReplyLen }

// SupportsInterimMessages returns false — iLink Bot context_token is single-use.
func (w *Weixin) SupportsInterimMessages() bool { return false }

// RegisterRoutes is a no-op (long-poll, no inbound HTTP).
func (w *Weixin) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

// Start implements RunnablePlatform. Launches getUpdates long-poll loop.
func (w *Weixin) Start(handler platform.MessageHandler) error {
	w.startMu.Lock()
	if w.started {
		w.startMu.Unlock()
		return fmt.Errorf("weixin platform already started")
	}
	w.started = true
	w.startMu.Unlock()

	w.handler = handler

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	go w.pollLoop(ctx)

	w.cleanupWg.Add(1)
	go func() {
		defer w.cleanupWg.Done()
		w.cleanupTokensLoop(ctx)
	}()

	slog.Info("weixin platform started", "base_url", w.cfg.BaseURL)
	return nil
}

// Stop implements RunnablePlatform.
func (w *Weixin) Stop() error {
	if w.cancel != nil {
		w.cancel()
	}
	w.handlerWg.Wait()
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
	// Guard parity with feishu/slack/discord: an empty text body produces a
	// no-op reply instead of pushing a blank bubble to the user. Images are
	// logged but dropped — the iLink Bot API does not accept attachments.
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
		// %q + sanitize: ChatID arrives from the iLink relay; if it ever
		// carried a control byte, embedding it raw in an error string would
		// leak through any caller that surfaces err.Error() to logs or IM.
		return "", fmt.Errorf("weixin: no context_token for user %q (no inbound message yet)",
			osutil.SanitizeForLog(msg.ChatID, 128))
	}

	if err := w.api.sendMessage(ctx, msg.ChatID, msg.Text, contextToken); err != nil {
		return "", fmt.Errorf("weixin send: %w", err)
	}
	// R232-SEC-11: ChatID arrives from the iLink relay; sanitize before
	// embedding in the returned MessageID so any control byte / log-injection
	// sequence cannot survive into downstream slog/IM surfaces that print the
	// id verbatim.
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

		// Check API-level errors
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

		// Update cursor
		if resp.GetUpdatesBuf != "" {
			cursor = resp.GetUpdatesBuf
		}

		// Process messages
		for _, msg := range resp.Msgs {
			// Only process user messages, skip bot messages
			if msg.MessageType != msgTypeUser {
				continue
			}

			text := extractText(msg)
			if text == "" {
				// No text item: iLink sent an image/audio/other attachment we
				// don't currently forward. Debug-level so bursts of media from
				// one user don't flood operator logs; still queryable when
				// troubleshooting "why didn't my message go through".
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

			// Cache context_token for reply. Bound the stored length so a
			// misbehaving (or compromised) iLink relay can't pin arbitrary
			// memory per user for the 24h TTL window. Real tokens are UUID-
			// scale strings; legitimate values stay well under 512 bytes.
			// (R227-SEC-8)
			const maxContextTokenLen = 512
			if msg.ContextToken != "" && len(msg.ContextToken) <= maxContextTokenLen {
				w.contextTokens.Store(from, &tokenEntry{
					token:     msg.ContextToken,
					updatedNs: time.Now().UnixNano(),
				})
			}

			// When the upstream response omits message_id the zero value "0"
			// is not a real ID — every zero-id message would collide on the
			// first dedup call and then pass through, so leave EventID empty
			// and let Dedup.Seen's empty-string guard treat it as unknown.
			eventID := ""
			if msg.MessageID != 0 {
				eventID = fmt.Sprintf("%d", msg.MessageID)
			}
			incoming := platform.IncomingMessage{
				Platform:  "weixin",
				EventID:   eventID,
				UserID:    from,
				ChatID:    from, // direct chat, reply to the sender
				ChatType:  "direct",
				Text:      text,
				MentionMe: true, // direct messages always mention the bot
			}

			// R235-SEC-9: gate handler spawn through hookSem so a relay that
			// returns a giant batch (or repeats messages quickly) cannot
			// outpace dispatch and explode goroutine count. If the cap is
			// exhausted we drop the message and emit a warning rather than
			// blocking the poll loop indefinitely — pollLoop must keep
			// draining cursors so other users' messages aren't starved.
			select {
			case w.hookSem <- struct{}{}:
			default:
				slog.Warn("weixin handler queue saturated, dropping message",
					"from", osutil.SanitizeForLog(from, 128),
					"msg_id", msg.MessageID)
				continue
			}
			w.handlerWg.Add(1)
			go func() {
				defer func() { <-w.hookSem }()
				defer w.handlerWg.Done()
				defer platform.RecoverHandler("weixin")
				w.handler(ctx, incoming)
			}()
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
