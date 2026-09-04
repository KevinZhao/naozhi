// File eventlog_query.go: the read path — Entries / LastN / EntriesSince /
// EntriesBefore (and their *Append buffer-reuse variants), Count, and the
// lock-free summary accessors.

package cli

import (
	"slices"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Entries returns a copy of all entries in chronological order.
//
// It allocates up to maxSize (~140KB) per call; hot loops (dashboard poll,
// agent_tailer fan-in) should use LastN with a bounded n or EntriesAppend
// with a pooled buffer. Entries is the one-shot convenience for tests and
// history dumps.
func (l *EventLog) Entries() []clievent.EventEntry {
	return l.LastNAppend(nil, 0)
}

// LastN returns the most recent n entries in chronological order.
// If n <= 0 or n >= count, all entries are returned.
func (l *EventLog) LastN(n int) []clievent.EventEntry {
	return l.LastNAppend(nil, n)
}

// EntriesAppend copies all entries in chronological order into `dst`,
// reslicing it (growing only if cap is short) so a sync.Pool buffer makes the
// hot path alloc-free. Pass dst[:0] when reusing; nil behaves like Entries().
// The returned slice is owned by the caller and never retained by the
// EventLog; callers that hand it to a channel must not recycle it until the
// consumer is done.
func (l *EventLog) EntriesAppend(dst []clievent.EventEntry) []clievent.EventEntry {
	return l.LastNAppend(dst, 0)
}

// LastNAppend is the buffer-reusing variant of LastN. See EntriesAppend
// for the lifetime contract; pass `n<=0` for "all entries" semantics.
func (l *EventLog) LastNAppend(dst []clievent.EventEntry, n int) []clievent.EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := l.count
	if n > 0 && n < count {
		count = n
	}
	if cap(dst) >= count {
		dst = dst[:count]
	} else {
		dst = make([]clievent.EventEntry, count)
	}
	start := (l.head - count + l.maxSize) % l.maxSize
	// Branch-on-wrap avoids a per-step modulo on the hot polling path.
	if start+count <= l.maxSize {
		copy(dst, l.entries[start:start+count])
	} else {
		n1 := l.maxSize - start
		copy(dst, l.entries[start:l.maxSize])
		copy(dst[n1:], l.entries[:count-n1])
	}
	return dst
}

// LastNVisible returns the tail of the ring as a CONTIGUOUS chronological
// slice (internal events included) containing at least `visibleTarget`
// entries for which IsVisibleEntry is true, stopping early when the slice
// reaches maxTotal (cost ceiling under tool_use / task_progress floods) or the
// ring is exhausted. Internal events ride along because the dashboard's first
// paint rebuilds turn state and the running banner from them and filters them
// out of the transcript itself. visibleTarget <= 0 means LastN(maxTotal);
// maxTotal <= 0 means the whole ring. The earliest Time returned is the cursor
// for continuing into the disk tier.
func (l *EventLog) LastNVisible(visibleTarget, maxTotal int) []clievent.EventEntry {
	return l.LastNVisibleAppend(nil, visibleTarget, maxTotal)
}

// LastNVisibleAppend is the buffer-reusing variant of LastNVisible: matches
// are appended into dst[:0] so a pooled caller avoids the per-subscribe
// allocation of a maxTotal-cap slice (#1631). slices.Reverse runs OUTSIDE
// l.mu so a long walk never blocks a concurrent Append beyond the scan.
// Lifetime as EntriesAppend; nil dst returns nil on an empty ring.
func (l *EventLog) LastNVisibleAppend(dst []clievent.EventEntry, visibleTarget, maxTotal int) []clievent.EventEntry {
	l.mu.RLock()
	count := l.count
	if count == 0 {
		l.mu.RUnlock()
		if dst == nil {
			return nil
		}
		return dst[:0]
	}
	limit := maxTotal
	if limit <= 0 || limit > count {
		limit = count
	}
	// Walk backward from the newest slot (branch-on-wrap) into a reverse
	// buffer until a stop condition trips.
	rev := dst[:0]
	if cap(rev) < limit {
		rev = make([]clievent.EventEntry, 0, limit)
	}
	visible := 0
	idx := l.head - 1
	if idx < 0 {
		idx += l.maxSize
	}
	for i := 0; i < count && len(rev) < limit; i++ {
		e := l.entries[idx]
		rev = append(rev, e)
		if IsVisibleEntry(e) {
			visible++
			if visibleTarget > 0 && visible >= visibleTarget {
				break
			}
		}
		idx--
		if idx < 0 {
			idx += l.maxSize
		}
	}
	l.mu.RUnlock()
	slices.Reverse(rev)
	return rev
}

// Count returns the current number of valid entries (0..maxSize). Lock-free;
// lets pooled callers right-size a scratch buffer before LastNAppend.
func (l *EventLog) Count() int {
	return int(l.countAtomic.Load())
}

// EntriesSince returns entries after the given unix ms timestamp, in chronological order.
//
// Single backward scan into a reverse buffer; slices.Reverse runs OUTSIDE
// l.mu so a large initial subscribe cannot block a concurrent Append (#685).
// Returns []EventEntry, never pre-marshaled JSON: EventLog is wire-format
// agnostic, and the per-notify marshal cache lives in the WS hub
// (internal/server/wshub_eventpush_cache.go). Do NOT add a JSON cache here.
func (l *EventLog) EntriesSince(afterMS int64) []clievent.EventEntry {
	return l.EntriesSinceAppend(nil, afterMS)
}

// EntriesSinceAppend is the buffer-reusing variant of EntriesSince: matches
// are appended into dst[:0] so streaming-tail callers (1Hz × tabs × sessions,
// 1-5 matches each) avoid the per-poll allocation (#937). nil dst keeps
// EntriesSince's lazy-allocate behaviour and returns nil on no match.
// Lifetime as EntriesAppend.
func (l *EventLog) EntriesSinceAppend(dst []clievent.EventEntry, afterMS int64) []clievent.EventEntry {
	l.mu.RLock()
	if l.count == 0 {
		l.mu.RUnlock()
		if dst == nil {
			return nil
		}
		return dst[:0]
	}
	// Backward walk from the newest slot with branch-on-wrap (no per-step
	// modulo); allocate lazily on the first match.
	rev := dst[:0]
	idx := l.head - 1
	if idx < 0 {
		idx += l.maxSize
	}
	for i := l.count - 1; i >= 0; i-- {
		if l.entries[idx].Time <= afterMS {
			break
		}
		if cap(rev) == 0 {
			// Cap the hint so a full ring does not allocate a giant backing
			// array per notify; append grows organically past it.
			initialCap := l.count - i
			if initialCap > entriesSinceInitialCap {
				initialCap = entriesSinceInitialCap
			}
			rev = make([]clievent.EventEntry, 0, initialCap)
		}
		rev = append(rev, l.entries[idx])
		idx--
		if idx < 0 {
			idx += l.maxSize
		}
	}
	l.mu.RUnlock()
	if len(rev) == 0 {
		// nil for no-buffer callers; pooled callers keep their buffer.
		if dst == nil {
			return nil
		}
		return rev
	}
	slices.Reverse(rev)
	return rev
}

// EntriesBefore returns up to `limit` entries whose Time < beforeMS, in
// chronological order — the dashboard "load earlier" page. beforeMS of 0
// means no upper bound (equivalent to LastN); a non-positive limit returns nil.
func (l *EventLog) EntriesBefore(beforeMS int64, limit int) []clievent.EventEntry {
	return l.EntriesBeforeAppend(nil, beforeMS, limit)
}

// EntriesBeforeAppend is the buffer-reusing variant of EntriesBefore. See
// EntriesSinceAppend for the lifetime contract; `beforeMS<=0` and `limit<=0`
// behave exactly as in EntriesBefore (#937).
func (l *EventLog) EntriesBeforeAppend(dst []clievent.EventEntry, beforeMS int64, limit int) []clievent.EventEntry {
	if limit <= 0 {
		if dst == nil {
			return nil
		}
		return dst[:0]
	}
	l.mu.RLock()
	if l.count == 0 {
		l.mu.RUnlock()
		if dst == nil {
			return nil
		}
		return dst[:0]
	}

	// Per-entry filter on every slot, no "collect greedily once crossed"
	// shortcut: Time values are caller-supplied and live + replay batches can
	// interleave, so the ring is not guaranteed monotonic and the shortcut
	// could hand the dashboard a stale page (#1383). This is a user-scroll
	// path; O(count) is fine.
	rev := dst[:0]
	allocated := false
	idx := l.head - 1
	if idx < 0 {
		idx += l.maxSize
	}
	for i := l.count - 1; i >= 0 && len(rev) < limit; i-- {
		if beforeMS > 0 && l.entries[idx].Time >= beforeMS {
			idx--
			if idx < 0 {
				idx += l.maxSize
			}
			continue
		}
		if !allocated && cap(rev) == 0 {
			initialCap := limit
			if remaining := i + 1; remaining < initialCap {
				initialCap = remaining
			}
			rev = make([]clievent.EventEntry, 0, initialCap)
			allocated = true
		}
		rev = append(rev, l.entries[idx])
		idx--
		if idx < 0 {
			idx += l.maxSize
		}
	}
	l.mu.RUnlock()
	if len(rev) == 0 {
		if dst == nil {
			return nil
		}
		return rev
	}
	slices.Reverse(rev)
	return rev
}

// LastPromptSummary returns the summary of the most recent "user" entry.
func (l *EventLog) LastPromptSummary() string {
	return loadAtomicString(&l.lastPromptSummary)
}

// LastActivitySummary returns the summary of the most recent "tool_use" or "thinking" entry.
func (l *EventLog) LastActivitySummary() string {
	return loadAtomicString(&l.lastActivitySummary)
}

// LastResponseSummary returns the summary of the most recent assistant "text"
// entry (sidebar preview line); empty until assistant text has streamed.
func (l *EventLog) LastResponseSummary() string {
	return loadAtomicString(&l.lastResponseSummary)
}

// LastEventAt returns the wall-clock time of the most recent live Append, or
// the zero Time when only replays (or nothing) have landed. Router.Cleanup
// uses it to avoid killing a long but actively streaming turn. Lock-free.
func (l *EventLog) LastEventAt() time.Time {
	ns := l.lastEventAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// UserTurnCount returns the cumulative count of "user" entries appended since
// the Process was spawned (SessionSnapshot.MessageCount). Ring eviction does
// not decrement it.
func (l *EventLog) UserTurnCount() int64 {
	return l.userTurnCount.Load()
}

// loadAtomicString and storeAtomicString are package-private aliases for
// textutil.LoadAtomicString / StoreAtomicString so the dense Append hot path
// stays readable. Contract (equal-value short-circuit, last-writer-wins under
// l.mu) is documented on the textutil helpers.
func loadAtomicString(v *atomic.Pointer[string]) string {
	return textutil.LoadAtomicString(v)
}

func storeAtomicString(v *atomic.Pointer[string], s string) {
	textutil.StoreAtomicString(v, s)
}
