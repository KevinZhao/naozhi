package weixin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

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

	// contextTokens caches the latest context_token per user for reply.
	// Value is *tokenEntry (token + last-updated unix seconds) so we can
	// drop entries whose tokens have gone stale — otherwise users who
	// message once and never return accumulate forever.
	contextTokens sync.Map // map[userID]*tokenEntry
}

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

// New creates a WeChat platform adapter.
func New(cfg Config) *Weixin {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = 4000
	}
	return &Weixin{
		cfg: cfg,
		api: newAPIClient(cfg.BaseURL, cfg.Token),
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
			"chat", msg.ChatID, "image_count", len(msg.Images))
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
		return "", fmt.Errorf("weixin: no context_token for user %s (no inbound message yet)", msg.ChatID)
	}

	if err := w.api.sendMessage(ctx, msg.ChatID, msg.Text, contextToken); err != nil {
		return "", fmt.Errorf("weixin send: %w", err)
	}
	return fmt.Sprintf("weixin:%s:%d", msg.ChatID, time.Now().UnixMilli()), nil
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
				"errmsg", resp.ErrMsg,
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
				slog.Debug("weixin non-text message ignored", "from", msg.FromUserID, "msg_id", msg.MessageID, "items", len(msg.ItemList))
				continue
			}

			from := msg.FromUserID
			if from == "" {
				continue
			}

			// Cache context_token for reply
			if msg.ContextToken != "" {
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

			w.handlerWg.Add(1)
			go func() {
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
