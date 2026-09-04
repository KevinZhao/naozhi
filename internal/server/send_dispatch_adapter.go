// serverCaps 是 *Server 与 internal/dispatch 之间的薄壳——把 Server 的
// sendWithBroadcast / tryAutoTakeover / replyTagForBackend 绑在
// dispatch.Capabilities interface 上。
package server

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// serverCaps adapts *Server's hooks (sendWithBroadcast, tryAutoTakeover,
// replyTagForBackend) into the dispatch.Capabilities interface that
// NewDispatcher consumes. Methods on a struct rather than method-value
// closures: no per-hook funcval allocation, one *Server reference, and
// tests can still swap in a fake Capabilities.
type serverCaps struct{ s *Server }

// Send forwards to Server.sendWithBroadcast (Hub when registered; sess.Send
// only for Headless Servers — a non-headless Server with no hub panics; see
// send.go).
func (c serverCaps) Send(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error) {
	return c.s.sendWithBroadcast(ctx, key, sess, text, images, onEvent)
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
