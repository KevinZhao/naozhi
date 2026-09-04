// connector_subscribe.go owns the per-key live event streaming worker that
// pumps EventLog deltas + session_state transitions to the primary while a
// subscription is active. Subscription lifecycle (cancel handles, subExited
// bookkeeping) is in connector_conn.go.
package upstream

import (
	"context"
	"log/slog"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// Same-millisecond dedup is shared with the local dashboard pusher via
// cli.SinceCursor (#2402); see internal/cli/since_cursor.go.

func (c *Connector) streamEvents(ctx context.Context, writeJSON func(any) error, key string, notify <-chan struct{}) {
	sess := c.router.SessionFor(key)
	if sess == nil {
		return
	}
	var lastState string
	csr := cli.NewSinceCursor()
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				// Session was reset/replaced (notify closed). Always emit a
				// terminal session_state — even if Reset already removed the
				// session from the router — so the primary learns the key has
				// no live stream and re-subscribes on the next send.
				s := c.router.SessionFor(key)
				msg := node.ReverseMsg{Type: "session_state", Key: key, State: "dead", Reason: reasonSessionReset}
				if s != nil {
					snap := s.Snapshot()
					msg.State = snap.State
					msg.Reason = snap.DeathReason
				}
				if err := writeJSON(msg); err != nil {
					slog.Debug("connector write final session_state", "key", key, "err", err)
				}
				return
			}
			// Re-fetch in case the session was replaced (e.g. /new): the fresh
			// event log's timestamps can predate the old watermark, so reset
			// the cursor on pointer change to deliver the full new history.
			if cur := c.router.SessionFor(key); cur != nil && cur != sess {
				sess = cur
				lastState = ""
				csr.Reset()
			}
			// Inclusive watermark query + UUID dedup so same-millisecond events
			// arriving in a later notify wave are delivered exactly once.
			cand := sess.EventEntriesSince(csr.QueryAfter())
			entries := csr.Filter(cand)
			if len(entries) > 0 {
				if err := writeJSON(node.ReverseMsg{Type: "events", Key: key, Events: entries}); err != nil {
					return
				}
				csr.Advance(entries)
			}
			// Only push session_state when it changes. sess is non-nil here:
			// nil-checked at entry, and the only reassignment above gates on
			// non-nil. State()/DeathReason() instead of Snapshot() because this
			// branch fires on every agent_message_chunk.
			curState := sess.State()
			if curState != lastState {
				lastState = curState
				if err := writeJSON(node.ReverseMsg{Type: "session_state", Key: key, State: curState, Reason: sess.DeathReason()}); err != nil {
					slog.Debug("connector write session_state", "key", key, "err", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}
