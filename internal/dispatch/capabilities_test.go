package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// R248-TEST-1: NoopCapabilities.Send must panic with a "not wired" message so
// a misconfigured deployment fails loud at boot rather than accepting messages
// and silently dropping the reply. Mirrors the legacy "no fallback for SendFn"
// contract recorded on capabilities.go's NoopCapabilities docstring.
//
// Pinned defensively because the panic is the only thing that distinguishes
// NoopCapabilities from a real Capabilities implementation in production
// wiring; a refactor that replaced the panic with a silent return would
// regress to "messages accepted but never sent" without any other test
// catching it.
func TestNoopCapabilities_SendPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NoopCapabilities.Send did not panic; constructor-time wireup contract broken")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string; got %v", r, r)
		}
		if !strings.Contains(msg, "not wired") {
			t.Errorf("panic message %q does not contain 'not wired'; the operator-facing diagnostic is the contract", msg)
		}
	}()
	_, _ = NoopCapabilities{}.Send(context.Background(), "k", nil, "text", nil, nil)
}

// R248-TEST-1: Takeover and ReplyFooter return their documented defaults
// (false / "") so the dispatcher hot path can dereference caps unconditionally
// without nil guards. Table-driven so a future no-op extension (a 4th method)
// can drop in next to these without re-deriving the assertion shape.
func TestNoopCapabilities_DefaultsForTakeoverAndReplyFooter(t *testing.T) {
	t.Parallel()
	caps := NoopCapabilities{}

	if got := caps.Takeover(context.Background(), "chat", "key", session.AgentOpts{}); got {
		t.Errorf("NoopCapabilities.Takeover = true, want false (no external session adopted)")
	}

	footerCases := []struct {
		name      string
		backendID string
	}{
		{"empty backend", ""},
		{"claude backend", "claude"},
		{"kiro backend", "kiro"},
		{"unknown backend", "made-up"},
	}
	for _, tc := range footerCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := caps.ReplyFooter(tc.backendID); got != "" {
				t.Errorf("ReplyFooter(%q) = %q, want \"\" (no footer)", tc.backendID, got)
			}
		})
	}

	// Compile-time check (also documents intent): NoopCapabilities satisfies
	// the Capabilities interface.
	var _ Capabilities = NoopCapabilities{}
	// Reference cli.ImageData / session.ManagedSession through the interface
	// signature so this file's import of cli stays load-bearing if the
	// signature drifts.
	_ = func(c Capabilities) {
		_, _ = c.Send(context.Background(), "", (*session.ManagedSession)(nil), "", []cli.ImageData(nil), nil)
	}
}

// TestCapabilities_FacetSubsetting is a compile-pinned guarantee that the
// R248-ARCH-1 (#373) facet split stays back-compat: every Capabilities
// implementation still satisfies the narrower MessageSender / TakeoverHook /
// ReplyFooterHook interfaces. A facet-method rename or signature drift
// would silently break the documented "Capabilities IS the bundle" contract;
// pinning the assertions in a test (not just a top-level var) keeps the
// failure mode obvious in CI output.
func TestCapabilities_FacetSubsetting(t *testing.T) {
	t.Parallel()
	var caps Capabilities = NoopCapabilities{}

	var sender MessageSender = caps
	var tk TakeoverHook = caps
	var ft ReplyFooterHook = caps

	// Reach through each facet so the variables are load-bearing — a future
	// rename that broke the embedding contract would also fail to compile
	// here, not just at the var declaration.
	defer func() { _ = recover() }() // sender.Send panics by NoopCapabilities contract
	_, _ = sender.Send(context.Background(), "", nil, "", nil, nil)
	if tk.Takeover(context.Background(), "", "", session.AgentOpts{}) {
		t.Error("TakeoverHook should return false for noop")
	}
	if ft.ReplyFooter("claude") != "" {
		t.Error("ReplyFooterHook should return empty for noop")
	}
}
