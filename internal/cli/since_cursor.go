// since_cursor.go — SinceCursor, the shared streaming watermark used by every
// consumer that tails an EventLog via the (EntriesSince, notify) pair.
//
// EntriesSince(t) returns entries with Time strictly > t, so a live Append in
// the SAME millisecond as an already-delivered batch that arrives in a LATER
// notify wave would be dropped (#1481, #2402); ACP's turn-end events make this
// a per-turn certainty. The cursor therefore queries inclusively (QueryAfter ==
// watermark-1) and dedups by the unique UUID stampUUID puts on every entry;
// sentAtWM holds the UUIDs delivered at Time == watermark. Not goroutine-safe:
// each streaming loop owns one cursor. One-shot readers use SinceInclusive.
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
// pointer swap (e.g. /new): the new event log's timestamps can predate the
// old watermark, so the first notify after a swap must deliver everything.
func (s *SinceCursor) Reset() {
	s.watermark = 0
	s.sentAtWM = s.sentAtWM[:0]
}

// Watermark returns the timestamp (unix ms) of the newest delivered entry,
// or 0 before anything was delivered.
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
// delivered. The input's backing array is reused in place (the write index
// never overtakes the read index), so the hot path does not allocate.
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

// containsWM reports whether uuid was already delivered at the watermark
// millisecond.
func (s *SinceCursor) containsWM(uuid string) bool {
	for _, u := range s.sentAtWM {
		if u == uuid {
			return true
		}
	}
	return false
}

// Advance records that the given (chronological) entries were delivered.
// When the watermark moves forward the dedup set is rebuilt for the new
// trailing millisecond; same-millisecond redeliveries accumulate into it.
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
