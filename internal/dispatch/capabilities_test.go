package dispatch

import (
	"context"

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
