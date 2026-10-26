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
