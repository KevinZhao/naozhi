// connector_subscribe.go owns the live event streaming loop — pumps
// EventLog deltas + session_state transitions to the primary while the
// dashboard / IM client has an active subscription on this key.
// Subscription lifecycle (cancel handles, subExited bookkeeping) is in
// connector_conn.go; this file is purely the per-key streaming worker.
package upstream

import (
	"context"
	"log/slog"

	"github.com/naozhi/naozhi/internal/node"
)

func (c *Connector) streamEvents(ctx context.Context, writeJSON func(any) error, key string, notify <-chan struct{}) {
	sess := c.router.GetSession(key)
	if sess == nil {
		return
	}
	var lastTime int64
	var lastState string
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				// Session was reset/replaced; the notify channel is closed.
				// Send final state so the hub knows the process died and can
				// trigger a re-subscribe when the next send arrives.
				//
				// RNEW-005: if Reset removed the session from the router
				// between the notify close and our GetSession below, the
				// previous code returned silently — leaving the primary
				// unaware that the key no longer has a live stream. Always
				// emit a terminal session_state so reverseconn.go's
				// session_state handler can propagate it downstream and the
				// primary can re-subscribe on the next send.
				s := c.router.GetSession(key)
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
			// Re-fetch session in case it was replaced (e.g. via /new). A
			// replaced session has a fresh event log whose wall-clock
			// timestamps can be earlier than the old lastTime (NTP jumps or
			// fast /new), causing EntriesSince to drop the new session's
			// first events. Reset lastTime on pointer change so the first
			// notify after a swap delivers the full new history.
			if cur := c.router.GetSession(key); cur != nil && cur != sess {
				sess = cur
				lastTime = 0
				lastState = ""
			}
			entries := sess.EventEntriesSince(lastTime)
			if len(entries) > 0 {
				if err := writeJSON(node.ReverseMsg{Type: "events", Key: key, Events: entries}); err != nil {
					return
				}
				// entries are chronological; last entry has the highest timestamp
				lastTime = entries[len(entries)-1].Time
			}
			// Only push session_state when it actually changes.
			// RNEW-005 invariant: sess is non-nil here. It was nil-checked
			// at loop entry (line 1031) and the only code path that
			// reassigns it inside the loop (line 1057) also gates on
			// non-nil. Do not introduce any assignment to sess without
			// re-verifying this precondition — State() would panic on nil.
			//
			// R230C-PERF-1: read State() / DeathReason() directly instead
			// of building a full Snapshot. Snapshot mirrors live model
			// back into s.model and copies 10+ fields the connector
			// doesn't need; State+DeathReason are 2 atomic loads each.
			state := sess.State()
			if state != lastState {
				lastState = state
				if err := writeJSON(node.ReverseMsg{Type: "session_state", Key: key, State: state, Reason: sess.DeathReason()}); err != nil {
					slog.Debug("connector write session_state", "key", key, "err", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}
