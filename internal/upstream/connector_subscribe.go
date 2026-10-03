// connector_subscribe.go owns the per-key live event streaming worker that
// pumps EventLog deltas + session_state transitions to the primary while a
// subscription is active. Subscription lifecycle (cancel handles, subExited
// bookkeeping) is in connector_conn.go.
package upstream

import (
	"context"
	"log/slog"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

// Same-millisecond dedup is shared with the local dashboard pusher via
// clievent.SinceCursor (#2402); see internal/cli/since_cursor.go.

// streamEvents pumps sess's events and state changes for key to the primary.
// sess is the session the subscribe handler already acked; re-resolving it
// here would let a Reset in between end the stream silently. Unless ctx ends
// or a write fails, it returns only after writing one final session_state with
// key's current router state (nil sess or closed notify; see
// writeTerminalState), so the primary re-subscribes.
func (c *Connector) streamEvents(ctx context.Context, writeJSON func(any) error, key string, sess Session, notify <-chan struct{}) {
	if sess == nil {
		c.writeTerminalState(writeJSON, key)
		return
	}
	var lastState string
	csr := clievent.NewSinceCursor()
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				// Session was reset/replaced (notify closed).
				c.writeTerminalState(writeJSON, key)
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
				if err := writeJSON(node.ReverseMsg{Type: "events", Key: key, Events: clievent.ForWire(entries)}); err != nil {
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

// writeTerminalState reports key's current router state, or dead/session_reset
// when Reset already removed it, so a replaced session reports its own state.
func (c *Connector) writeTerminalState(writeJSON func(any) error, key string) {
	msg := node.ReverseMsg{Type: "session_state", Key: key, State: "dead", Reason: reasonSessionReset}
	if s := c.router.SessionFor(key); s != nil {
		snap := s.Snapshot()
		msg.State = snap.State
		msg.Reason = snap.DeathReason
	}
	if err := writeJSON(msg); err != nil {
		slog.Debug("connector write final session_state", "key", key, "err", err)
	}
}
