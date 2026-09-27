// serverCaps 是 *Server 与 internal/dispatch 之间的薄壳——把 Server 的
// sendWithBroadcast / tryAutoTakeover / replyTagForBackend 绑在
// dispatch.Capabilities interface 上。
package server

import (
	"context"
	"fmt"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/session"
)

// serverCaps adapts *Server's hooks (sendWithBroadcast, tryAutoTakeover,
// replyTagForBackend) into the dispatch.Capabilities interface that
// NewDispatcher consumes. Methods on a struct rather than method-value
// closures: no per-hook funcval allocation, one *Server reference, and
// tests can still swap in a fake Capabilities.
type serverCaps struct{ s *Server }

// Send forwards to Server.sendWithBroadcast, which delegates to the Hub's
// send engine (see send.go). The session is the one dispatchRouter handed the
// dispatcher, coming back unchanged; anything else is a wiring fault.
func (c serverCaps) Send(ctx context.Context, key string, sess dispatch.Session, text string, images []clievent.Attachment, onEvent clievent.EventCallback) (*clievent.SendResult, error) {
	ms, ok := sess.(*session.ManagedSession)
	if !ok || ms == nil {
		return nil, fmt.Errorf("server: dispatch sent on a session dispatchRouter did not produce (%T)", sess)
	}
	return c.s.sendWithBroadcast(ctx, key, ms, text, images, onEvent)
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
		backendID = c.s.router.DefaultBackend()
	}
	return replyTagForBackend(backendID)
}
