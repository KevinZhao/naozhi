package platform

import (
	"context"
	"log/slog"
	"net/http"
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
	Platform  string
	EventID   string
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

// RunnablePlatform extends Platform for platforms needing background goroutines.
type RunnablePlatform interface {
	Platform
	Start(handler MessageHandler) error
	Stop() error
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

// ReplyWithRetry calls p.Reply up to maxAttempts times with exponential backoff
// starting at 500 ms, doubling each retry up to 4 s. It returns on the first
// success. If all attempts fail the last error is returned.
func ReplyWithRetry(ctx context.Context, p Platform, msg OutgoingMessage, maxAttempts int) (string, error) {
	backoff := 500 * time.Millisecond
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			timer := time.NewTimer(backoff)
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
	}
	slog.Error("platform reply failed after all attempts", "platform", p.Name(), "chat", msg.ChatID, "attempts", maxAttempts, "err", lastErr)
	return "", lastErr
}
