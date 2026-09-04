// since_cursor.go — SinceCursor, the shared streaming watermark used by
// every consumer that tails an EventLog via the (EntriesSince, notify)
// pair.
//
// It exists to fix R20260530-GO-1 (#1481) and its dashboard twin (#2402):
// EntriesSince(t) returns only entries with Time strictly > t, so a live
// Append that lands in the SAME wall-clock millisecond as the tail of an
// already-delivered batch — but arrives in a LATER notify wave — was
// silently dropped (its Time == watermark). The ACP (kiro) backend makes
// this a per-turn certainty rather than a rare race: ReadEvent synthesises
// the turn-end (thinking, text, result) events from a single stdout frame
// and each is appended via its own AppendBatch call within the same
// millisecond, with a subscriber notify in between. A pusher that drains
// between those appends advances its cursor to T and the remaining
// same-millisecond entries (including the visible reply text) never match
// `Time > T` again.
//
// The cursor queries inclusively of the watermark millisecond
// (QueryAfter == watermark-1) and dedups by UUID: every EventLog entry is
// stamped with a unique 32-hex UUID (stampUUID), so an entry re-returned
// at the watermark millisecond is delivered only if its UUID was not
// already sent. sentAtWM holds exactly the UUIDs delivered at
// Time == watermark.
//
// History: born as `sinceCursor` inside internal/upstream (reverse-node
// streaming, R20260530-GO-1); promoted here so the local dashboard pusher
// (internal/server/wshub_eventpush.go) shares the same implementation
// instead of re-fixing the bug independently (#2402).
//
// R164029-PERF-4 (#1599): sentAtWM was a map[string]struct{} rebuilt on
// every watermark advance. At any instant it only holds the UUIDs delivered
// at the single trailing millisecond (typically 1-3 entries), so a map was
// pure overhead — each notify wave inserted N short-lived UUID keys and the
// bucket array was never released by clear(). A reused string slice
// eliminates the per-wave map allocation: it is truncated (cap retained) on
// advance and a linear scan over a handful of entries is cheaper than map
// hashing at this size. A slice (not a fixed array) keeps dedup correctness
// even in the rare case that more events than expected land in the same
// millisecond.
//
// Not goroutine-safe: each streaming loop owns exactly one cursor.
//
// One-shot request/response readers (dashboard HTTP poll, relay
// fetch_events, agent_events) cannot hold a cursor; they use
// SinceInclusive (since_inclusive.go) for the same watermark-1 query and
// leave the uuid dedup to the client.
package cli

import "github.com/naozhi/naozhi/internal/cli/clievent"

// SinceCursor tracks a streaming watermark over EventLog entries with
// same-millisecond UUID dedup. See the file header for the rationale.
type SinceCursor struct {
	watermark int64
	sentAtWM  []string
}

// NewSinceCursor returns a cursor in the pre-subscribe state:
// QueryAfter() == -1, i.e. "deliver everything".
func NewSinceCursor() *SinceCursor {
	return &SinceCursor{}
}

// Reset rewinds the cursor to the pre-subscribe state. Used on session
// pointer swap (e.g. /new): a replaced session has a fresh event log whose
// wall-clock timestamps can predate the old watermark (NTP jumps or fast
// /new), so the first notify after a swap must deliver the full new history.
func (s *SinceCursor) Reset() {
	s.watermark = 0
	s.sentAtWM = s.sentAtWM[:0]
}

// Watermark returns the timestamp (unix ms) of the newest delivered entry,
// or 0 before anything was delivered. Callers that need a stable per-wave
// fingerprint (e.g. the WS hub's coalesced marshal cache) read this instead
// of tracking a parallel lastTime.
func (s *SinceCursor) Watermark() int64 {
	return s.watermark
}

// QueryAfter returns the afterMS to pass to EntriesSince. Subtracting one
// re-admits the watermark millisecond; Filter then drops the dupes. When the
// watermark is 0 (initial / post-Reset) this is -1, i.e. "everything".
func (s *SinceCursor) QueryAfter() int64 {
	return s.watermark - 1
}

// Filter drops entries at the watermark millisecond that were already
// delivered. Entries with Time > watermark are always new. The input
// slice's backing array is reused in place (the write index never overtakes
// the read index), so no extra allocation occurs on the hot streaming path.
func (s *SinceCursor) Filter(cand []clievent.EventEntry) []clievent.EventEntry {
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
// watermark millisecond. Linear over sentAtWM, which holds only the handful
// of UUIDs at that millisecond.
func (s *SinceCursor) containsWM(uuid string) bool {
	for _, u := range s.sentAtWM {
		if u == uuid {
			return true
		}
	}
	return false
}

// Advance records that the given entries were delivered. Entries are
// chronological, so the last one carries the new high-water timestamp. When
// the watermark moves forward the dedup set is rebuilt for the new trailing
// millisecond; same-millisecond redeliveries accumulate into it.
func (s *SinceCursor) Advance(delivered []clievent.EventEntry) {
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
