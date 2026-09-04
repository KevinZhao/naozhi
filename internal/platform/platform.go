package platform

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
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
	// MessageID is the platform-native message id (optional); Reactor-capable
	// platforms use it so dispatch can react on the user's original message.
	MessageID string
	UserID    string
	ChatID    string
	ChatType  string // "direct" | "group"
	Text      string
	MentionMe bool
	Images    []Image
	// AgentID, when non-empty, pins the target agent (bypassing slash-command
	// resolution) for synthetic messages such as an AskUserQuestion card click
	// (#2148). The dispatcher whitelist-validates it before honouring it.
	AgentID string
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

// InterimMessageCapable is an optional capability for platforms that can
// deliver interim notifications ("thinking...") before the final reply.
type InterimMessageCapable interface {
	SupportsInterimMessages() bool
}

// SupportsInterimMessages reports whether a platform can handle interim
// notifications; false (opt-in) when the capability is absent.
func SupportsInterimMessages(p Platform) bool {
	if i, ok := AsCapability[InterimMessageCapable](p); ok {
		return i.SupportsInterimMessages()
	}
	return false
}

// SingleUseReplyTokenCapable is an optional capability for platforms whose
// reply token is consumed by the first send (WeChat iLink): the dispatcher
// must collapse a long reply into one truncated message instead of N chunks,
// of which only the first would arrive (#2136).
type SingleUseReplyTokenCapable interface {
	UsesSingleUseReplyToken() bool
}

// UsesSingleUseReplyToken reports whether a platform can deliver only one
// reply per inbound message; false (opt-in) when the capability is absent.
func UsesSingleUseReplyToken(p Platform) bool {
	if s, ok := AsCapability[SingleUseReplyTokenCapable](p); ok {
		return s.UsesSingleUseReplyToken()
	}
	return false
}

// ReactionType is a platform-agnostic reaction key mapped per adapter.
type ReactionType string

const (
	// ReactionQueued marks "received, waiting in queue" on the user's message;
	// removed after the consuming turn completes.
	ReactionQueued ReactionType = "queued"
)

// DefaultMaxReplyLen is the fallback per-message split length (Feishu/Slack
// ~4000-byte ceiling; safe for Weixin's 5000) when an adapter's MaxReplyLen is unset.
const DefaultMaxReplyLen = 4000

// DiscordMaxReplyLen is Discord's hard 2000-character content ceiling
// (BASE_TYPE_MAX_LENGTH), declared here so every reply ceiling has one home.
const DiscordMaxReplyLen = 2000

// DefaultMaxIncomingBytes caps inbound text forwarded into dispatch. The
// shim's 12 MB line ceiling and the queue's 4 MB coalesce cap are backstops,
// not the security boundary — this is the policy entry point for all adapters.
const DefaultMaxIncomingBytes = 8 * 1024

// EncodeMessageRef joins (chatID, msgID) into the composite "chatID:msgID"
// reference Slack and Discord use. Halves are joined verbatim — adapters'
// IDs contain no ':' — so callers round-tripping arbitrary strings must escape.
func EncodeMessageRef(chatID, msgID string) string {
	return chatID + ":" + msgID
}

// DecodeMessageRef splits a composite "chatID:msgID" reference; ok=false when
// ':' is absent, which callers must treat as a wire-format error.
func DecodeMessageRef(ref string) (chatID, msgID string, ok bool) {
	idx := strings.Index(ref, ":")
	if idx < 0 {
		return "", "", false
	}
	return ref[:idx], ref[idx+1:], true
}

// AsCapability is the generic discriminator for optional Platform
// capabilities: a new capability is one interface declaration plus a typed
// call at the use-site, no per-capability helper.
func AsCapability[T any](p Platform) (T, bool) {
	c, ok := p.(T)
	return c, ok
}

// Reactor is an optional capability for platforms that can add/remove
// reactions on inbound messages. Implementations should tolerate repeats
// (AddReaction on an existing reaction, RemoveReaction on an absent one → nil).
type Reactor interface {
	AddReaction(ctx context.Context, messageID string, reaction ReactionType) error
	RemoveReaction(ctx context.Context, messageID string, reaction ReactionType) error
}

// QuestionCard is the platform-agnostic AskUserQuestion payload
// (docs/rfc/askuser-question.md). SessionKey is intentionally absent: click
// routing is re-derived from the chat context, never carried in the card.
type QuestionCard struct {
	// ToolUseID correlates the card action back to the assistant tool_use block.
	ToolUseID string
	// ChatType ("direct"/"group") is embedded by adapters whose card callback
	// cannot recover it from the envelope (Feishu WebSocket); empty = unknown.
	ChatType string
	// AgentID is the asking session's agent id, embedded for the same reason so
	// the answer routes to the SAME agent session (#2148); empty = unknown.
	AgentID string
	// Items is one or more questions, each rendered as its own block.
	Items []QuestionItem
}

// QuestionItem mirrors clievent.AskQuestionItem without a reverse dependency
// on internal/cli.
type QuestionItem struct {
	Question    string
	Header      string
	MultiSelect bool
	Options     []QuestionOption
}

// QuestionOption is one selectable choice in a QuestionItem.
type QuestionOption struct {
	Label       string
	Description string
}

// QuestionCardSender is an optional capability for native AskUserQuestion
// cards; without it dispatch falls back to a plain-text option list.
// SendQuestionCard returns the card's message id so dispatch can edit it later.
type QuestionCardSender interface {
	SendQuestionCard(ctx context.Context, chatID string, card QuestionCard) (msgID string, err error)
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
	return SplitTextWithCount(text, maxRunes, utf8.RuneCountInString(text))
}

// SplitTextWithCount is SplitText for callers that already computed
// utf8.RuneCountInString(text), avoiding a second O(n) scan. A wrong
// runeCount only affects the single-chunk fast path, never chunk boundaries.
func SplitTextWithCount(text string, maxRunes, runeCount int) []string {
	if runeCount <= maxRunes {
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

// PermanentError is implemented by platform errors that should bypass retry
// loops (invalid credentials, chat removed, etc.).
type PermanentError interface {
	error
	IsPermanent() bool
}

// IsPermanent reports whether any error in the chain (incl. errors.Join
// branches) signals a permanent condition. False for nil.
func IsPermanent(err error) bool {
	var pe PermanentError
	return errors.As(err, &pe) && pe.IsPermanent()
}

// TokenInvalidatedError is implemented by platform errors meaning the cached
// auth token was rejected and just invalidated; ReplyWithRetry then pauses
// briefly and grants one extra retry so the fresh-token attempt does not
// share its budget with the stale-token attempts (#1339).
type TokenInvalidatedError interface {
	error
	IsTokenInvalidated() bool
}

// IsTokenInvalidated reports whether any error in the chain signals a
// just-invalidated auth token. False for nil.
func IsTokenInvalidated(err error) bool {
	var te TokenInvalidatedError
	return errors.As(err, &te) && te.IsTokenInvalidated()
}

// tokenRotationDelay is the pause before the retry following a token
// invalidation: invisible on the happy path, enough for typical Feishu
// open-API replication (#1339).
const tokenRotationDelay = 50 * time.Millisecond

// ReplyWithRetry calls p.Reply up to maxAttempts times with ±25%-jittered
// exponential backoff (500 ms doubling to 4 s) so a shared upstream 5xx does
// not produce a synchronised thundering herd. A PermanentError short-circuits.
// A TokenInvalidatedError extends the budget by one (at most once) and adds
// tokenRotationDelay before the next attempt (#1339).
func ReplyWithRetry(ctx context.Context, p Platform, msg OutgoingMessage, maxAttempts int) (string, error) {
	backoff := 500 * time.Millisecond
	var lastErr error
	tokenRotationGranted := false
	rotationPendingFromAttempt := -1
	limit := maxAttempts
	for i := 0; i < limit; i++ {
		if i > 0 {
			wait := osutil.JitterBackoff(backoff)
			// Only the retry immediately following a rotation gets the extra pause.
			if rotationPendingFromAttempt == i-1 {
				wait += tokenRotationDelay
				rotationPendingFromAttempt = -1
			}
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
		if IsTokenInvalidated(err) {
			rotationPendingFromAttempt = i
			if !tokenRotationGranted {
				tokenRotationGranted = true
				limit++
				slog.Info("platform reply token-invalidated; granting one extra retry",
					"platform", p.Name(), "chat", msg.ChatID, "attempt", i+1, "new_limit", limit)
			}
		}
	}
	slog.Error("platform reply failed after all attempts", "platform", p.Name(), "chat", msg.ChatID, "attempts", limit, "err", lastErr)
	return "", lastErr
}
