package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Capabilities groups the host-supplied hooks (Takeover / ReplyFooter) the
// IM reply path reaches into the surrounding Server through, so dispatch
// stays free of server / Hub references (production: server.serverCaps).
// NewDispatcher always installs a non-nil Capabilities; the Deprecated
// DispatcherConfig.{TakeoverFn,ReplyFooterFn} closures are wrapped in an
// internal adapter. Sessions and sends belong to the Turns side.
type Capabilities interface {
	// Takeover is invoked on the first message of every chat to let the host
	// adopt an external Claude session. Returns true on adoption; the turn
	// runs GetOrCreate unconditionally afterwards either way.
	Takeover(ctx context.Context, chatKey, key string, opts sessionview.AgentOpts) bool

	// ReplyFooter returns the per-session reply tag (e.g. "cc" / "kiro") for
	// the session's backend ID; the IM reply path appends "\n\n— <tag>" when
	// non-empty. Empty backendID means "no backend pinned" and typically
	// resolves to the router's default backend tag.
	ReplyFooter(backendID string) string

	// BackendIDs lists the backends a session may be pinned to, default
	// first; nil when the host has none. /backend validates against it.
	BackendIDs() []string
}

// NoopCapabilities is the default Capabilities when callers leave
// DispatcherConfig.Capabilities unset and provide no legacy *Fn closure:
// Takeover returns false and ReplyFooter "".
type NoopCapabilities struct{}

// Takeover returns false (no external session adopted).
func (NoopCapabilities) Takeover(context.Context, string, string, sessionview.AgentOpts) bool {
	return false
}

// ReplyFooter returns "" (no footer appended).
func (NoopCapabilities) ReplyFooter(string) string { return "" }

// BackendIDs returns nil (no backend catalogue).
func (NoopCapabilities) BackendIDs() []string { return nil }

// TakeoverHook isolates the optional first-message takeover probe.
type TakeoverHook interface {
	Takeover(ctx context.Context, chatKey, key string, opts sessionview.AgentOpts) bool
}

// ReplyFooterHook isolates the optional reply tag suffix used by the IM
// reply path.
type ReplyFooterHook interface {
	ReplyFooter(backendID string) string

	// BackendIDs lists the backends a session may be pinned to, default
	// first; nil when the host has none. /backend validates against it.
	BackendIDs() []string
}

// Compile-time pin: Capabilities satisfies both facets.
var (
	_ TakeoverHook    = (Capabilities)(nil)
	_ ReplyFooterHook = (Capabilities)(nil)
)

// SessionView is a wider read-only seam over the session than turn.Session,
// for dispatch-internal helpers that need the session ID or an in-band
// interrupt; test fakes implement it without the full ManagedSession surface
// (#1366).
type SessionView interface {
	// SessionID returns the active CLI session identifier.
	SessionID() string
	// Backend returns the backend identifier (e.g. "claude" / "kiro");
	// empty for legacy stores predating the Backend field.
	Backend() string
	// InterruptViaControl aborts the in-flight turn via an in-band
	// stream-json control_request; see ManagedSession.InterruptViaControl.
	InterruptViaControl() sessionview.InterruptOutcome
}

// closureCapabilities adapts the Deprecated TakeoverFn / ReplyFooterFn
// closures into a Capabilities; nil closures fall back to NoopCapabilities
// behaviour.
type closureCapabilities struct {
	takeover    func(ctx context.Context, chatKey, key string, opts sessionview.AgentOpts) bool
	replyFooter func(backendID string) string
}

func (c closureCapabilities) Takeover(ctx context.Context, chatKey, key string, opts sessionview.AgentOpts) bool {
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

// BackendIDs returns nil: the legacy closure form carries no catalogue.
func (c closureCapabilities) BackendIDs() []string { return nil }
