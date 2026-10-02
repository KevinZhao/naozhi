// serverCaps 是 *Server 与 internal/dispatch 之间的薄壳——把 Server 的
// tryAutoTakeover / replyTagForBackend 绑在 dispatch.Capabilities interface 上。
package server

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
)

// serverCaps adapts *Server's tryAutoTakeover / replyTagForBackend into the
// dispatch.Capabilities interface that NewDispatcher consumes. Methods on a
// struct rather than method-value closures: no per-hook funcval allocation,
// and tests can still swap in a fake Capabilities. IM sends go through
// turnSender, not through here.
type serverCaps struct {
	s *Server
}

// Takeover forwards to Server.tryAutoTakeover. Returns true when an
// external Claude session was adopted; the dispatcher ignores the result
// (GetOrCreate runs unconditionally afterwards).
func (c serverCaps) Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool {
	return c.s.tryAutoTakeover(ctx, chatKey, key, opts)
}

// ReplyFooter resolves the reply tag for backendID, defaulting to the
// router's default backend for sessions that have not pinned one.
// replyTagForBackend returns "" for unknown ids so dispatch skips the footer.
func (c serverCaps) ReplyFooter(backendID string) string {
	if backendID == "" {
		backendID = c.s.router.Backends().DefaultBackend()
	}
	return replyTagForBackend(backendID)
}
