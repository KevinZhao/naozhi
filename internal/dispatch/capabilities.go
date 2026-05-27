package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// Capabilities groups the host-supplied hooks the Dispatcher reaches into the
// surrounding Server through (Send / Takeover / ReplyFooter). Implementations
// live in the host package (server.serverCaps) so dispatch stays free of
// server / Hub references.
//
// NewDispatcher always installs a non-nil Capabilities so the hot path can
// dereference unconditionally; legacy DispatcherConfig.{SendFn,TakeoverFn,
// ReplyFooterFn} closures are wrapped in an internal adapter for backward
// compatibility. R243-ARCH-10.
//
// R245-ARCH-45 (#904): this interface IS the requested SessionFlow bundle.
// Pre-bundle code injected three independent closures (SendFn / TakeoverFn /
// ReplyFooterFn); tests had to mock each one and any reviewer reading
// NewDispatcher had to track three separate nil checks. The bundle gives
// callers and tests a single mock surface (server.serverCaps in production,
// a stub Capabilities impl in tests). The legacy *Fn fields are retained as
// Deprecated entries on DispatcherConfig only as a backward-compatibility
// shim for the existing test fixtures; new wiring should populate
// DispatcherConfig.Capabilities directly. GetOrCreate is not part of this
// bundle on purpose — it lives on SessionRouter (the cfg.Router slot), which
// already has its own one-interface mock surface.
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

// NoopCapabilities is the default Capabilities installed when callers leave
// DispatcherConfig.Capabilities unset AND don't provide a legacy *Fn closure.
// Take/ReplyFooter return their documented defaults (false / ""); Send panics.
//
// In production, NewDispatcher's R248-ARCH-2 boot-panic gate fires before any
// message arrives so misconfigured wireup fails loud at startup; this method
// is the runtime backstop for tests/headless contexts that opt out via
// AllowMissingSender and then still try to call Send.
type NoopCapabilities struct{}

// Send panics with a "wireup missing" message. NoopCapabilities is the
// constructor-default; the boot-panic gate (NewDispatcher, R248-ARCH-2)
// catches missing Send wireup before any traffic arrives, so reaching this
// method at runtime means a test opted out via DispatcherConfig.
// AllowMissingSender and then still tried to call Send.
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
