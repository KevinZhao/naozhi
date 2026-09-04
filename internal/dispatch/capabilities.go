package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// Capabilities groups the host-supplied hooks (Send / Takeover / ReplyFooter)
// the Dispatcher reaches into the surrounding Server through, so dispatch
// stays free of server / Hub references (production: server.serverCaps).
// NewDispatcher always installs a non-nil Capabilities; the Deprecated
// DispatcherConfig.{SendFn,TakeoverFn,ReplyFooterFn} closures are wrapped in
// an internal adapter. GetOrCreate is deliberately not part of this bundle —
// it lives on SessionRouter (#904).
type Capabilities interface {
	// Send forwards a turn payload to the session router after guard /
	// queue gating has succeeded. Implementations must NOT silently drop —
	// a missing send path is a constructor bug (see NoopCapabilities.Send).
	Send(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error)

	// Takeover is invoked on the first message of every chat to let the host
	// adopt an external Claude session. Returns true on adoption; the
	// dispatcher runs GetOrCreate unconditionally afterwards either way.
	Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool

	// ReplyFooter returns the per-session reply tag (e.g. "cc" / "kiro") for
	// the session's backend ID; the IM reply path appends "\n\n— <tag>" when
	// non-empty. Empty backendID means "no backend pinned" and typically
	// resolves to the router's default backend tag.
	ReplyFooter(backendID string) string
}

// NoopCapabilities is the default Capabilities when callers leave
// DispatcherConfig.Capabilities unset and provide no legacy *Fn closure.
// Takeover/ReplyFooter return false / ""; Send panics.
type NoopCapabilities struct{}

// Send panics: NewDispatcher's boot-panic gate catches missing Send wireup at
// startup, so reaching this at runtime means a test opted out via
// DispatcherConfig.AllowMissingSender and still called Send.
func (NoopCapabilities) Send(context.Context, string, *session.ManagedSession, string, []cli.Attachment, cli.EventCallback) (*cli.SendResult, error) {
	panic("dispatch: Capabilities.Send not wired (set DispatcherConfig.Capabilities or DispatcherConfig.SendFn)")
}

// Takeover returns false (no external session adopted).
func (NoopCapabilities) Takeover(context.Context, string, string, session.AgentOpts) bool {
	return false
}

// ReplyFooter returns "" (no footer appended).
func (NoopCapabilities) ReplyFooter(string) string { return "" }

// MessageSender narrows Capabilities to the single required Send hook so
// consumers / tests can depend on the smallest seam they need. Every
// Capabilities also satisfies MessageSender (#373).
type MessageSender interface {
	Send(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error)
}

// TakeoverHook isolates the optional first-message takeover probe.
type TakeoverHook interface {
	Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool
}

// ReplyFooterHook isolates the optional reply tag suffix used by the IM
// reply path.
type ReplyFooterHook interface {
	ReplyFooter(backendID string) string
}

// Compile-time pin: Capabilities satisfies all three facets.
var (
	_ MessageSender   = (Capabilities)(nil)
	_ TakeoverHook    = (Capabilities)(nil)
	_ ReplyFooterHook = (Capabilities)(nil)
)

// SessionView is the narrow read-only seam over *session.ManagedSession that
// the dispatch send path consumes. MessageSender.Send still takes the
// concrete pointer for back-compat; new dispatch-internal helpers should
// accept SessionView so test fakes need not implement the full
// ManagedSession surface (#1366).
type SessionView interface {
	// SessionID returns the active CLI session identifier.
	SessionID() string
	// Backend returns the backend identifier (e.g. "claude" / "kiro");
	// empty for legacy stores predating the Backend field.
	Backend() string
	// InterruptViaControl aborts the in-flight turn via an in-band
	// stream-json control_request; see ManagedSession.InterruptViaControl.
	InterruptViaControl() session.InterruptOutcome
}

// Compile-time pin: *session.ManagedSession satisfies SessionView.
var _ SessionView = (*session.ManagedSession)(nil)

// closureCapabilities adapts the Deprecated SendFn / TakeoverFn /
// ReplyFooterFn closures into a Capabilities; nil closures fall back to
// NoopCapabilities behaviour.
type closureCapabilities struct {
	send        func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error)
	takeover    func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool
	replyFooter func(backendID string) string
}

func (c closureCapabilities) Send(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error) {
	if c.send == nil {
		return NoopCapabilities{}.Send(ctx, key, sess, text, images, onEvent)
	}
	return c.send(ctx, key, sess, text, images, onEvent)
}

func (c closureCapabilities) Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool {
	if c.takeover == nil {
		return false
	}
	return c.takeover(ctx, chatKey, key, opts)
}

func (c closureCapabilities) ReplyFooter(backendID string) string {
	if c.replyFooter == nil {
		return ""
	}
	return c.replyFooter(backendID)
}
