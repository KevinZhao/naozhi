// connector_subscribe.go owns the live event streaming loop — pumps
// EventLog deltas + session_state transitions to the primary while the
// dashboard / IM client has an active subscription on this key.
// Subscription lifecycle (cancel handles, subExited bookkeeping) is in
// connector_conn.go; this file is purely the per-key streaming worker.
package upstream

import (
	"context"
	"log/slog"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// sinceCursor moved to internal/cli as cli.SinceCursor (#2402) so the local
// dashboard pusher (internal/server/wshub_eventpush.go) and this reverse-node
// streamer share one implementation of the R20260530-GO-1 (#1481)
// same-millisecond dedup fix. See internal/cli/since_cursor.go for the full
// rationale (inclusive watermark query + UUID dedup at the trailing
// millisecond).

func (c *Connector) streamEvents(ctx context.Context, writeJSON func(any) error, key string, notify <-chan struct{}) {
	sess := c.router.SessionFor(key)
	if sess == nil {
		return
	}
	var lastState string
	// R20260530-GO-1 (#1481): see sinceCursor — guards against same-millisecond
	// events being dropped across notify waves.
	csr := cli.NewSinceCursor()
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				// Session was reset/replaced; the notify channel is closed.
				// Send final state so the hub knows the process died and can
				// trigger a re-subscribe when the next send arrives.
				//
				// RNEW-005: if Reset removed the session from the router
				// between the notify close and our SessionFor below, the
				// previous code returned silently — leaving the primary
				// unaware that the key no longer has a live stream. Always
				// emit a terminal session_state so reverseconn.go's
				// session_state handler can propagate it downstream and the
				// primary can re-subscribe on the next send.
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
			// Re-fetch session in case it was replaced (e.g. via /new). A
			// replaced session has a fresh event log whose wall-clock
			// timestamps can be earlier than the old watermark (NTP jumps or
			// fast /new), causing EntriesSince to drop the new session's
			// first events. Reset the cursor on pointer change so the first
			// notify after a swap delivers the full new history.
			if cur := c.router.SessionFor(key); cur != nil && cur != sess {
				sess = cur
				lastState = ""
				csr.Reset()
			}
			// Query inclusively of the watermark millisecond, then dedup by
			// UUID so same-millisecond events arriving in a later notify wave
			// are still delivered exactly once. See sinceCursor.
			cand := sess.EventEntriesSince(csr.QueryAfter())
			entries := csr.Filter(cand)
			if len(entries) > 0 {
				if err := writeJSON(node.ReverseMsg{Type: "events", Key: key, Events: entries}); err != nil {
					return
				}
				csr.Advance(entries)
			}
			// Only push session_state when it actually changes.
			// RNEW-005 invariant: sess is non-nil here. It was nil-checked
			// at loop entry (line 1031) and the only code path that
			// reassigns it inside the loop (line 1057) also gates on
			// non-nil. Do not introduce any assignment to sess without
			// re-verifying this precondition.
			//
			// R230C-PERF-1: use the lightweight State()/DeathReason()
			// pair instead of Snapshot() — the latter performs ~10
			// atomic loads, parseKeyParts, and a SetModel mirror write
			// just to surface State, and this branch fires on every
			// agent_message_chunk (10-50/s × N subscribed sessions).
			// Snapshot is still used on the close path above where the
			// extra cost is amortised over a once-per-session terminal.
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
