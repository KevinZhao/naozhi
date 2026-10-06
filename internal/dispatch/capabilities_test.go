package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// fakeCapabilities is newTestDispatcher's default Capabilities test double.
// Zero value behaves like NoopCapabilities (Takeover=false, ReplyFooter="")
// so tests only need to set the hooks they actually exercise.
type fakeCapabilities struct {
	takeover    func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool
	replyFooter func(backendID string) string
	backendIDs  []string
}

func (f fakeCapabilities) Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool {
	if f.takeover == nil {
		return false
	}
	return f.takeover(ctx, chatKey, key, opts)
}

func (f fakeCapabilities) BackendIDs() []string { return f.backendIDs }

func (f fakeCapabilities) ReplyFooter(backendID string) string {
	if f.replyFooter == nil {
		return ""
	}
	return f.replyFooter(backendID)
}

var _ Capabilities = fakeCapabilities{}

// R248-TEST-1: Takeover and ReplyFooter return their documented defaults
// (false / "") so the dispatcher hot path can dereference caps unconditionally
// without nil guards.
func TestNoopCapabilities_DefaultsForTakeoverAndReplyFooter(t *testing.T) {
	t.Parallel()
	caps := NoopCapabilities{}
	if got := caps.Takeover(context.Background(), "chat", "key", session.AgentOpts{}); got {
		t.Errorf("NoopCapabilities.Takeover = true, want false (no external session adopted)")
	}
	for _, backendID := range []string{"", "claude", "kiro", "made-up"} {
		if got := caps.ReplyFooter(backendID); got != "" {
			t.Errorf("ReplyFooter(%q) = %q, want \"\" (no footer)", backendID, got)
		}
	}
	var _ Capabilities = NoopCapabilities{}
}

// TestSessionView_ManagedSessionSatisfies is a compile-pinned guarantee
// that R260528-ARCH-5 (#1366) — the additive SessionView seam over
// *session.ManagedSession — keeps tracking the production type. A future
// rename of SessionID / Backend / InterruptViaControl on ManagedSession
// would surface here in CI rather than only at the satisfier var that
// has fewer eyeballs.
func TestSessionView_ManagedSessionSatisfies(t *testing.T) {
	t.Parallel()
	var view SessionView = (*session.ManagedSession)(nil)
	defer func() { _ = recover() }() // nil-receiver method calls panic; we only need compile-time satisfier
	_ = view.SessionID()
	_ = view.Backend()
}

// TestCapabilities_FacetSubsetting is a compile-pinned guarantee that the
// R248-ARCH-1 (#373) facet split stays back-compat: every Capabilities
// implementation still satisfies the narrower TakeoverHook / ReplyFooterHook
// interfaces.
func TestCapabilities_FacetSubsetting(t *testing.T) {
	t.Parallel()
	var caps Capabilities = NoopCapabilities{}
	var tk TakeoverHook = caps
	var ft ReplyFooterHook = caps
	if tk.Takeover(context.Background(), "", "", session.AgentOpts{}) {
		t.Error("TakeoverHook should return false for noop")
	}
	if ft.ReplyFooter("claude") != "" {
		t.Error("ReplyFooterHook should return empty for noop")
	}
}
