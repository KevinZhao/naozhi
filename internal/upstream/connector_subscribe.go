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

// sinceCursor tracks the streaming watermark for streamEvents. It exists to
// fix R20260530-GO-1 (#1481): EventEntriesSince(t) returns only entries with
// Time strictly > t, so a live Append that lands in the SAME wall-clock
// millisecond as the tail of an already-delivered batch — but arrives in a
// LATER notify wave — was silently dropped (its Time == watermark). The cursor
// queries inclusively of the watermark millisecond (queryAfter == watermark-1)
// and dedups by UUID: every EventLog entry is stamped with a unique 32-hex
// UUID, so an entry re-returned at the watermark millisecond is delivered only
// if its UUID was not already sent. sentAtWM holds exactly the UUIDs delivered
// at Time == watermark.
//
// R164029-PERF-4 (#1599): sentAtWM was a map[string]struct{} rebuilt on every
// watermark advance. At any instant it only holds the UUIDs delivered at the
// single trailing millisecond (typically 1-3 entries), so a map was pure
// overhead — each notify wave inserted N short-lived UUID keys and the bucket
// array was never released by clear(). A reused string slice eliminates the
// per-wave map allocation: it is truncated (cap retained) on advance and a
// linear scan over a handful of entries is cheaper than map hashing at this
// size. A slice (not a fixed array) keeps dedup correctness even in the rare
// case that more events than expected land in the same millisecond.
type sinceCursor struct {
	watermark int64
	sentAtWM  []string
}

func newSinceCursor() *sinceCursor {
	return &sinceCursor{}
}

// reset rewinds the cursor to the pre-subscribe state. Used on session pointer
// swap (e.g. /new): a replaced session has a fresh event log whose wall-clock
// timestamps can predate the old watermark (NTP jumps or fast /new), so the
// first notify after a swap must deliver the full new history.
func (s *sinceCursor) reset() {
	s.watermark = 0
	s.sentAtWM = s.sentAtWM[:0]
}

// queryAfter returns the afterMS to pass to EventEntriesSince. Subtracting one
// re-admits the watermark millisecond; filter then drops the dupes. When the
// watermark is 0 (initial / post-reset) this is -1, i.e. "everything".
func (s *sinceCursor) queryAfter() int64 {
	return s.watermark - 1
}

// filter drops entries at the watermark millisecond that were already
// delivered. Entries with Time > watermark are always new. The input slice's
// backing array is reused in place (the write index never overtakes the read
// index), so no extra allocation occurs on the hot streaming path.
func (s *sinceCursor) filter(cand []cli.EventEntry) []cli.EventEntry {
	out := cand[:0]
	for _, e := range cand {
		if e.Time == s.watermark && s.containsWM(e.UUID) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// containsWM reports whether uuid was already delivered at the trailing
// watermark millisecond. Linear over sentAtWM, which holds only the handful of
// UUIDs at that millisecond.
func (s *sinceCursor) containsWM(uuid string) bool {
	for _, u := range s.sentAtWM {
		if u == uuid {
			return true
		}
	}
	return false
}

// advance records that the given entries were delivered. Entries are
// chronological, so the last one carries the new high-water timestamp. When
// the watermark moves forward the dedup set is rebuilt for the new trailing
// millisecond; same-millisecond redeliveries accumulate into it.
func (s *sinceCursor) advance(delivered []cli.EventEntry) {
	if len(delivered) == 0 {
		return
	}
	newWM := delivered[len(delivered)-1].Time
	if newWM != s.watermark {
		s.watermark = newWM
		s.sentAtWM = s.sentAtWM[:0]
	}
	for _, e := range delivered {
		if e.Time == s.watermark && !s.containsWM(e.UUID) {
			s.sentAtWM = append(s.sentAtWM, e.UUID)
		}
	}
}

func (c *Connector) streamEvents(ctx context.Context, writeJSON func(any) error, key string, notify <-chan struct{}) {
	sess := c.router.GetSession(key)
	if sess == nil {
		return
	}
	var lastState string
	// R20260530-GO-1 (#1481): see sinceCursor — guards against same-millisecond
	// events being dropped across notify waves.
	csr := newSinceCursor()
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
			// timestamps can be earlier than the old watermark (NTP jumps or
			// fast /new), causing EntriesSince to drop the new session's
			// first events. Reset the cursor on pointer change so the first
			// notify after a swap delivers the full new history.
			if cur := c.router.GetSession(key); cur != nil && cur != sess {
				sess = cur
				lastState = ""
				csr.reset()
			}
			// Query inclusively of the watermark millisecond, then dedup by
			// UUID so same-millisecond events arriving in a later notify wave
			// are still delivered exactly once. See sinceCursor.
			cand := sess.EventEntriesSince(csr.queryAfter())
			entries := csr.filter(cand)
			if len(entries) > 0 {
				if err := writeJSON(node.ReverseMsg{Type: "events", Key: key, Events: entries}); err != nil {
					return
				}
				csr.advance(entries)
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
