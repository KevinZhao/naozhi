// connector_subscribe.go owns the per-key live event streaming worker that
// pumps EventLog deltas + session_state transitions to the primary while a
// subscription is active. Subscription lifecycle (cancel handles, subExited
// bookkeeping) is in connector_conn.go.
package upstream

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

// Same-millisecond dedup is shared with the local dashboard pusher via
// clievent.SinceCursor (#2402); see internal/cli/since_cursor.go.

// subscribeHistoryTimeout bounds the disk-tier walk behind a want_history
// opening page, like the hub's initialHistoryDiskTimeout.
const subscribeHistoryTimeout = 2 * time.Second

// openStream returns the cursor sub's stream starts from. A want_history
// subscribe is first answered with its page in one `events` frame (the
// catch-up after sub.After, or else the visible-aware newest page) and the
// cursor starts past it. A plain subscribe starts at sub.After, so the first
// Append does not replay the whole ring the primary already fetched.
func (c *Connector) openStream(ctx context.Context, writeJSON func(any) error, sub node.ReverseMsg, sess Session) *clievent.SinceCursor {
	csr := clievent.NewSinceCursorAt(sub.After)
	if !sub.WantHistory || sess == nil {
		return csr
	}
	frame := node.ReverseMsg{Type: "events", Key: sub.Key}
	var entries []clievent.EventEntry
	if sub.After > 0 {
		entries = sess.EventEntriesSince(clievent.SinceInclusive(sub.After))
		// Like the hub's emptyInitialHistoryWanted: an empty catch-up only
		// for a running session.
		if len(entries) == 0 && sess.State() != "running" {
			return csr
		}
	} else {
		pageCtx, cancel := context.WithTimeout(ctx, subscribeHistoryTimeout)
		var hasMore bool
		entries, hasMore = sess.InitialHistoryPage(pageCtx, sub.Limit)
		cancel()
		// Sent even when empty: the dashboard leaves its blank state only on
		// an initial frame (#2432).
		frame.Initial, frame.HasMore = true, &hasMore
	}
	frame.Events = clievent.ForWire(entries)
	if err := writeJSON(frame); err != nil {
		slog.Debug("connector write subscribe history", "key", sub.Key, "err", err)
		return csr
	}
	csr.Advance(entries)
	return csr
}

// streamEvents pumps sess's events and state changes for key to the primary,
// starting after csr. sess is the session the subscribe handler already
// acked; re-resolving it here would let a Reset in between end the stream
// silently. Unless ctx ends or a write fails, it returns only after writing
// one final session_state with key's current router state (nil sess or
// closed notify; see writeTerminalState), so the primary re-subscribes.
func (c *Connector) streamEvents(ctx context.Context, writeJSON func(any) error, key string, sess Session, notify <-chan struct{}, csr *clievent.SinceCursor) {
	if sess == nil {
		c.writeTerminalState(writeJSON, key)
		return
	}
	var lastState string
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
