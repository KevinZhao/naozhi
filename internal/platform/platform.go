package platform

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
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

// InterimMessageCapable is an optional capability: platforms that can deliver
// interim notifications (e.g. "thinking...", "new session") before the final
// reply implement it. Platforms like WeChat iLink use single-use reply tokens
// and deliberately omit it.
//
// R214-ARCH-2 (#402): promoted from an inline anonymous interface to a named
// capability so it discovers through the same AsCapability[T] discriminator
// (R239-ARCH-H) as Reactor / QuestionCardSender — one capability-extension
// pattern instead of a bespoke per-capability type-assert.
type InterimMessageCapable interface {
	SupportsInterimMessages() bool
}

// SupportsInterimMessages reports whether a platform can handle interim
// notifications before the final reply. Defaults to false (opt-in) for
// platforms that do not implement InterimMessageCapable.
func SupportsInterimMessages(p Platform) bool {
	if i, ok := AsCapability[InterimMessageCapable](p); ok {
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

// DefaultMaxReplyLen is the fallback per-message split-length applied when
// a platform adapter's MaxReplyLen config is not set. Matches Feishu's and
// Slack's documented per-message text ceiling (~4000 bytes / ~1333 CJK
// chars), which is also a safe default for Weixin's 5000-byte ceiling.
// Promoted here so all three adapters share one source of truth.
const DefaultMaxReplyLen = 4000

// DefaultMaxIncomingBytes caps the per-message text byte length that any
// platform adapter forwards into dispatch. ~8 KiB is well above any
// reasonable single-message payload from a human user and bounds the
// worst-case path that flows into dispatch. The shim's 12 MB line ceiling
// and the dispatch queue's 4 MB coalesce cap are final backstops, not the
// intended security boundary — this constant is the policy entry point so
// all adapters agree on a single value (R230C-ARCH-6).
const DefaultMaxIncomingBytes = 8 * 1024

// EncodeMessageRef joins (chatID, msgID) into the composite "chatID:msgID"
// form that Slack and Discord adapters use for outbound EditMessage and
// reaction handles. Centralised so a future change to the separator (e.g.
// to support chat IDs that may legitimately contain ':') only touches one
// pair of helpers (R230C-ARCH-7).
//
// Both halves are joined verbatim — adapters validate inputs at construction
// time (Slack channel IDs are uppercase alphanumeric; Discord channel IDs
// are decimal snowflake digits), so a literal ':' inside either half is not
// expected. Callers that want to round-trip arbitrary strings must escape
// before passing in.
func EncodeMessageRef(chatID, msgID string) string {
	return chatID + ":" + msgID
}

// DecodeMessageRef splits a composite "chatID:msgID" message reference into
// its two halves. Returns ok=false when the input does not contain ':' — the
// caller should treat that as a wire-format error rather than silently using
// empty strings, since the split halves drive REST API calls keyed by both
// IDs (e.g. Slack ChannelID + Timestamp, Discord ChannelID + MessageID).
func DecodeMessageRef(ref string) (chatID, msgID string, ok bool) {
	idx := strings.Index(ref, ":")
	if idx < 0 {
		return "", "", false
	}
	return ref[:idx], ref[idx+1:], true
}

// AsCapability is the generic discriminator for optional Platform
// capabilities. Each capability is declared as its own interface;
// callers query support via AsCapability[T](p). Adding a new
// capability is now a single interface declaration in this package
// and a typed call at the use-site — no per-capability AsX helper
// required (R214-ARCH-2 #402 / R239-ARCH-H).
//
// All call sites query capabilities via AsCapability[T](p) directly;
// the former per-capability AsReactor / AsQuestionCardSender wrappers
// were removed once their last callers migrated, so a new capability
// adds zero new helper functions.
func AsCapability[T any](p Platform) (T, bool) {
	c, ok := p.(T)
	return c, ok
}

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

// QuestionCard is the platform-agnostic payload for an AskUserQuestion prompt.
// Adapters turn this into a native interactive card (Feishu interactive
// card, Slack block actions, etc.). See docs/rfc/askuser-question.md.
//
// SessionKey is intentionally absent — routing of card-click replies is
// re-derived from the operator's own chat context, not carried in the
// card payload. Embedding it would widen the attack surface without any
// benefit on the inbound path.
type QuestionCard struct {
	// ToolUseID is the correlation id from the assistant tool_use block —
	// carried into card action callbacks so the handler knows which
	// question the user answered.
	ToolUseID string
	// Items is one or more questions. Adapters render each as its own
	// labelled block.
	Items []QuestionItem
}

// QuestionItem mirrors cli.AskQuestionItem but lives in the platform package
// so adapters don't need a reverse dependency on internal/cli. Kept as a
// plain struct so tests can build fixtures without importing cli.
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

// QuestionCardSender is an optional capability: platforms that support native
// interactive cards for AskUserQuestion implement it. Missing implementations
// degrade to a plain-text reply listing the options (handled in dispatch).
//
// SendQuestionCard returns the platform-native message id of the posted card
// so dispatch can later edit it to "✅ 已回答 …" once the user selects.
type QuestionCardSender interface {
	SendQuestionCard(ctx context.Context, chatID string, card QuestionCard) (msgID string, err error)
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

// TokenInvalidatedError is implemented by platform-specific errors that
// indicate the cached auth token was rejected and has just been
// invalidated. ReplyWithRetry uses this signal to (a) sleep briefly
// before the next attempt so the freshly-issued token has time to
// propagate at the upstream side, and (b) grant a one-shot extra retry
// so the post-rotation attempt does not have to share the original
// budget with attempts that all carried the same stale token. Modeled
// after PermanentError; see RFC R83 / RETRY1 and issue #1339 for the
// race that motivated this signal.
type TokenInvalidatedError interface {
	error
	IsTokenInvalidated() bool
}

// IsTokenInvalidated walks the error chain and reports whether any
// wrapped error signals that an auth token was just invalidated.
// Returns false for nil.
func IsTokenInvalidated(err error) bool {
	var te TokenInvalidatedError
	return errors.As(err, &te) && te.IsTokenInvalidated()
}

// tokenRotationDelay is the short pause inserted before a retry that
// follows a token-invalidation error. It gives the upstream platform a
// chance to register the freshly-issued token before the next request
// presents it. 50 ms is small enough to be invisible to users on the
// happy path (one-shot, only on token rotation) and large enough to
// cover typical Feishu open-API replication windows. Issue #1339.
const tokenRotationDelay = 50 * time.Millisecond

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
//
// A TokenInvalidatedError on attempt N is special-cased: the loop budget
// is extended by one (capped, applied at most once per call) so the
// post-rotation retry does not have to share its slot with attempts that
// all carried the same stale token, and a short tokenRotationDelay is
// inserted before that retry so the freshly-issued token has time to
// propagate at the upstream side. Without this, a token rotation on
// attempt 1 would consume two of three attempts to recover, leaving
// only one shot for the actually-fresh-token request — which the
// upstream might still reject in the millisecond window before
// activation lands. Issue #1339.
func ReplyWithRetry(ctx context.Context, p Platform, msg OutgoingMessage, maxAttempts int) (string, error) {
	backoff := 500 * time.Millisecond
	var lastErr error
	tokenRotationGranted := false
	rotationPendingFromAttempt := -1
	limit := maxAttempts
	for i := 0; i < limit; i++ {
		if i > 0 {
			wait := osutil.JitterBackoff(backoff)
			// If the previous attempt invalidated a token, lengthen the
			// pre-retry pause by tokenRotationDelay so the freshly-issued
			// token has time to register upstream. Applied only to the
			// retry that immediately follows the rotation.
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
