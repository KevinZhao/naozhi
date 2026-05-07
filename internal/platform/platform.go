package platform

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
)

// MessageHandler is the callback invoked when a platform receives a message.
type MessageHandler func(ctx context.Context, msg IncomingMessage)

// Image represents an image attachment downloaded by a platform or to be sent.
type Image struct {
	Data     []byte
	MimeType string // e.g., "image/png", "image/jpeg"
}

// IncomingMessage is the platform-agnostic inbound message.
type IncomingMessage struct {
	Platform string
	EventID  string
	// MessageID is the platform-native message identifier (e.g., Feishu
	// message_id, Slack ts, Discord message ID). Optional: platforms that
	// can't report it leave it empty. Used by Reactor-capable platforms so
	// dispatch can react back on the user's original message.
	MessageID string
	UserID    string
	ChatID    string
	ChatType  string // "direct" | "group"
	Text      string
	MentionMe bool
	Images    []Image
}

// OutgoingMessage is the platform-agnostic outbound message.
type OutgoingMessage struct {
	ChatID   string
	Text     string
	ThreadID string
	Images   []Image
}

// Platform is the interface every IM platform must implement.
type Platform interface {
	Name() string
	RegisterRoutes(mux *http.ServeMux, handler MessageHandler)
	Reply(ctx context.Context, msg OutgoingMessage) (msgID string, err error)
	EditMessage(ctx context.Context, msgID string, text string) error
	MaxReplyLength() int
}

// SupportsInterimMessages reports whether a platform can handle interim
// notifications (e.g. "thinking...", "new session") before the final reply.
// Platforms like WeChat iLink use single-use reply tokens and should return false.
func SupportsInterimMessages(p Platform) bool {
	type interim interface {
		SupportsInterimMessages() bool
	}
	if i, ok := p.(interim); ok {
		return i.SupportsInterimMessages()
	}
	return false // default: not supported (opt-in)
}

// ReactionType is a platform-agnostic reaction key. Adapters map each type
// to a platform-specific emoji / reaction string.
type ReactionType string

const (
	// ReactionQueued signals "message received, waiting in queue". Placed on
	// the user's incoming message when it gets enqueued, removed after the
	// turn that consumes it completes.
	ReactionQueued ReactionType = "queued"
)

// Reactor is an optional capability: platforms that can add/remove reactions
// on inbound messages implement it. Enables non-intrusive queue feedback —
// a reaction on the user's own message instead of a separate bot reply.
//
// Implementations should be idempotent-tolerant: AddReaction on an existing
// reaction or RemoveReaction on an absent one should return nil, since
// dispatch treats reaction ops as best-effort.
type Reactor interface {
	AddReaction(ctx context.Context, messageID string, reaction ReactionType) error
	RemoveReaction(ctx context.Context, messageID string, reaction ReactionType) error
}

// AsReactor returns p as a Reactor if it implements the interface.
func AsReactor(p Platform) (Reactor, bool) {
	r, ok := p.(Reactor)
	return r, ok
}

// RunnablePlatform extends Platform for platforms needing background goroutines.
type RunnablePlatform interface {
	Platform
	Start(handler MessageHandler) error
	Stop() error
}

// RecoverHandler catches panics in platform message handler goroutines,
// preventing a single malformed message from crashing the entire platform listener.
func RecoverHandler(label string) {
	if r := recover(); r != nil {
		slog.Error("panic in platform handler (recovered)",
			"handler", label, "panic", r, "stack", string(debug.Stack()))
	}
}

// SplitText splits text into chunks of at most maxRunes runes, preferring
// newline boundaries in the second half of each chunk when possible.
func SplitText(text string, maxRunes int) []string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	for text != "" {
		// Advance up to maxRunes runes to find the byte boundary.
		end, count := 0, 0
		for count < maxRunes && end < len(text) {
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
			count++
		}
		if end == len(text) {
			chunks = append(chunks, text)
			break
		}
		// Prefer splitting at a newline in the second half.
		if idx := strings.LastIndex(text[:end], "\n"); idx > end/2 {
			end = idx + 1
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

// ImageExt returns a file extension (with leading dot) for the given MIME type.
// Falls back to ".png" for unrecognized types.
func ImageExt(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// PermanentError is implemented by platform-specific errors that should
// bypass retry loops (invalid credentials, chat removed, etc.). Callers
// that want retry behaviour still wrap/compose; loops that respect this
// interface break out early instead of exhausting backoff budget.
type PermanentError interface {
	error
	IsPermanent() bool
}

// IsPermanent walks the error chain and reports whether any wrapped error
// signals a permanent condition. Returns false for nil.
//
// errors.As already walks the full chain (including branches joined via
// errors.Join) so a single call subsumes the earlier manual Unwrap loop.
// The manual loop also exited early on errors.Join boundaries where
// errors.Unwrap returns nil, which could miss a PermanentError buried in
// a join branch.
func IsPermanent(err error) bool {
	var pe PermanentError
	return errors.As(err, &pe) && pe.IsPermanent()
}

// ReplyWithRetry calls p.Reply up to maxAttempts times with exponential backoff
// starting at 500 ms, doubling each retry up to 4 s. It returns on the first
// success. If all attempts fail the last error is returned.
//
// Each backoff is scaled by a ±25% jitter so that many chats failing in the
// same tick do not retry on synchronised wall-clock boundaries — the common
// thundering-herd scenario when a shared upstream (e.g. Feishu open API)
// briefly 5xxs.
//
// A PermanentError short-circuits the loop — retrying an "app disabled" or
// "bot not in chat" error just burns time and amplifies load during an
// outage without changing the outcome.
func ReplyWithRetry(ctx context.Context, p Platform, msg OutgoingMessage, maxAttempts int) (string, error) {
	backoff := 500 * time.Millisecond
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			wait := jitterBackoff(backoff)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
			if backoff < 4*time.Second {
				backoff *= 2
			}
		}
		id, err := p.Reply(ctx, msg)
		if err == nil {
			return id, nil
		}
		lastErr = err
		slog.Warn("platform reply attempt failed", "platform", p.Name(), "chat", msg.ChatID, "attempt", i+1, "err", err)
		if IsPermanent(err) {
			slog.Error("platform reply permanent failure; aborting retries",
				"platform", p.Name(), "chat", msg.ChatID, "attempt", i+1, "err", err)
			return "", err
		}
	}
	slog.Error("platform reply failed after all attempts", "platform", p.Name(), "chat", msg.ChatID, "attempts", maxAttempts, "err", lastErr)
	return "", lastErr
}

// jitterBackoff returns d scaled by a random factor in [0.75, 1.25). Exposed
// at package scope so other reconnect loops (relay, upstream connector) can
// share the same shape. math/rand/v2 uses a per-goroutine source so
// concurrent callers do not contend on a global lock.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// Float64 returns [0,1); remap to [0.75, 1.25).
	factor := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(d) * factor)
}
