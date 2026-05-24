package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// Capabilities groups the host-supplied hooks that Dispatcher needs to
// reach back into the surrounding Server (send-with-broadcast, takeover,
// per-session reply tag). Bundling these into a single narrow interface
// supersedes the legacy closure-as-DI pattern (DispatcherConfig.SendFn /
// TakeoverFn / ReplyFooterFn) so:
//
//   - Wireup is one assignment (Capabilities: serverCaps{s}) instead of
//     three closure-fields that are easy to omit silently.
//   - The Dispatcher hot path can call the methods unconditionally; the
//     constructor always installs a non-nil implementation (NoopCapabilities
//     when callers don't supply one).
//   - Future extensions (e.g. a 4th hook for stream cancel) require one
//     interface method instead of a new closure field + a nil-fallback line.
//
// Implementations live in the host package (server.serverCapabilities) so
// dispatch stays free of cli.Server / Hub references, preserving the
// reverse-dependency boundary documented in docs/rfc/server-split.md.
//
// The legacy DispatcherConfig.SendFn / TakeoverFn / ReplyFooterFn fields
// remain supported via an internal closure→Capabilities adapter so existing
// test seams (dispatch_test.go, server_test.go) keep building during the
// transition. New code SHOULD set DispatcherConfig.Capabilities directly.
//
// Tracked under TODO R243-ARCH-10 (see docs/TODO.md).
type Capabilities interface {
	// Send forwards a turn payload to the session router after guard /
	// queue gating has succeeded. Production wires
	// Server.sendWithBroadcast (server.go ~930). Required: implementations
	// must NOT silently drop — a missing send path is a constructor bug
	// and the historical NewDispatcher contract panics rather than
	// suppressing the message (see NoopCapabilities.Send).
	Send(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)

	// Takeover is invoked on the first message of every chat to give the
	// host a chance to adopt an external Claude session. Returns true
	// when adoption succeeded; the dispatcher discards the result either
	// way (GetOrCreate runs unconditionally afterwards).
	//
	// Default: NoopCapabilities returns false (no external session).
	Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool

	// ReplyFooter returns the per-session reply tag (e.g. "cc" / "kiro")
	// given the session's backend ID. The IM reply path appends
	// "\n\n— <tag>" when the result is non-empty. Empty backendID means
	// "session has no backend pinned"; implementations typically resolve
	// it to the router's default backend tag.
	//
	// Default: NoopCapabilities returns "" (no footer).
	ReplyFooter(backendID string) string
}

// NoopCapabilities is the default Capabilities used when callers leave
// DispatcherConfig.Capabilities unset AND don't provide the legacy
// SendFn/TakeoverFn/ReplyFooterFn fields.
//
// Send deliberately panics: the historical contract for the SendFn closure
// was "no fallback — missing wireup must surface as a constructor-time
// panic, not a silent drop in production" (dispatch.go docstring on the
// old sendFn field). We keep that semantic so a misconfigured deployment
// fails loud at boot rather than accepting messages and dropping the
// reply silently. Tests exercising non-send paths (e.g. fakeGuard
// scenarios) MUST still install a stub Send.
//
// Takeover and ReplyFooter return their documented defaults (false / "")
// so headless constructions can rely on the dispatcher hot path
// dereferencing caps unconditionally without nil guards.
type NoopCapabilities struct{}

// Send panics: see type docstring. Mirrors the pre-refactor contract that
// DispatcherConfig.SendFn was required and NewDispatcher installed no
// fallback.
func (NoopCapabilities) Send(context.Context, string, *session.ManagedSession, string, []cli.ImageData, cli.EventCallback) (*cli.SendResult, error) {
	panic("dispatch: Capabilities.Send not wired (set DispatcherConfig.Capabilities or DispatcherConfig.SendFn)")
}

// Takeover returns false (no external session adopted).
func (NoopCapabilities) Takeover(context.Context, string, string, session.AgentOpts) bool {
	return false
}

// ReplyFooter returns "" (no footer appended).
func (NoopCapabilities) ReplyFooter(string) string { return "" }

// closureCapabilities adapts the legacy SendFn / TakeoverFn / ReplyFooterFn
// closures into a Capabilities implementation. Used internally by
// NewDispatcher when callers populate the Deprecated *Fn fields instead of
// Capabilities directly. nil closures fall back to NoopCapabilities
// behaviour (panic for Send, false for Takeover, "" for ReplyFooter).
type closureCapabilities struct {
	send        func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	takeover    func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool
	replyFooter func(backendID string) string
}

func (c closureCapabilities) Send(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
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
