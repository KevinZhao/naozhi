// Phase 3f-prep / R-dispatch-adapter-extract (2026-05-28):
// serverCaps adapter（实现 dispatch.Capabilities 接口）抽到独立文件。
// 纯物理切分、零行为变化。
//
// serverCaps 是 *Server 与 internal/dispatch 之间的薄壳——它把 Server
// 的 sendWithBroadcast / tryAutoTakeover / replyTagForBackend 绑在
// dispatch.Capabilities interface 上。把它独立成文件让 send.go 聚焦
// sendWithBroadcast 主流程；adapter 部分单独检查。
package server

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// serverCaps adapts *Server's hooks (sendWithBroadcast,
// tryAutoTakeover, replyTagForBackend) into the dispatch.Capabilities
// interface that NewDispatcher consumes. Replaces the legacy
// SendFn / TakeoverFn / ReplyFooterFn closure-as-DI wireup so adding a
// future hook (e.g. a stream-cancel callback) costs one method here
// instead of a new DispatcherConfig closure field plus its nil-fallback
// line. R243-ARCH-10.
//
// Renamed from dispatchCapabilities (R248-CR-3) to disambiguate from the
// dispatch.Capabilities interface this struct implements.
//
// WHY METHODS, NOT METHOD-VALUE CLOSURES (R248-CR-8): the alternative
// wireup `Capabilities: dispatch.Capabilities{Send: s.sendWithBroadcast,
// Takeover: s.tryAutoTakeover, ReplyFooter: replyTagForBackendOnRouter}`
// would allocate one funcval per method per Server lifetime — small in
// absolute terms but each funcval boxes the receiver pointer, and any
// future hook added to the interface would force a corresponding alloc
// at construction. Method-on-struct dispatch is a static call shape (no
// funcval, no receiver box) that compiles to the same machine code as
// the inlined Server method calls would have, while keeping the seam
// for tests to swap in a fake Capabilities. The struct-receiver style
// also lets `c.s` carry the *Server reference once instead of three
// times (one per closed-over field).
type serverCaps struct{ s *Server }

// Send forwards to Server.sendWithBroadcast (delegates to Hub when
// registered, falls back to sess.Send only for Headless Servers — a
// non-headless Server with no hub panics; see send.go). Tracks
// dashboard "running"/"ready" transitions; see send.go top docstring.
//
// The 1-line forward is intentional — see serverCaps godoc for why we
// did not use a method-value (`Send: s.sendWithBroadcast`) closure on
// DispatcherConfig instead. R248-CR-8.
func (c serverCaps) Send(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
	return c.s.sendWithBroadcast(ctx, key, sess, text, images, onEvent)
}

// Takeover forwards to Server.tryAutoTakeover. Returns true when an
// external Claude session was adopted; the dispatcher discards the
// result either way (GetOrCreate runs unconditionally afterwards).
//
// 1-line forward — see serverCaps godoc for the method-value rationale.
// R248-CR-8.
func (c serverCaps) Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool {
	return c.s.tryAutoTakeover(ctx, chatKey, key, opts)
}

// ReplyFooter resolves the per-session reply tag from a backendID,
// falling back to the router's default backend when the session has not
// pinned one (legacy / pre-multi-backend sessions). replyTagForBackend
// returns "" for unknown ids so dispatch will skip the footer rather
// than emit a garbled tag.
//
// Three-line body (default-backend lookup + tag map) is the only forward
// in this struct that does more than direct delegation — kept as a
// method (not a closure) for consistency with Send/Takeover and so a
// future caller cannot add a similar lookup elsewhere without the
// canonical entry point. R248-CR-8.
func (c serverCaps) ReplyFooter(backendID string) string {
	if backendID == "" {
		backendID = c.s.router.DefaultBackend()
	}
	return replyTagForBackend(backendID)
}
