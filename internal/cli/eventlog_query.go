// File eventlog_query.go: the read path — Entries / LastN / EntriesSince / EntriesBefore (and
// their *Append buffer-reuse variants), Count, and the lock-free summary
// accessors.
// Split from eventlog.go per docs/rfc/eventlog-split.md (ARCH-EVENTLOG-SPLIT);
// the EventLog struct and constructor live in eventlog.go.

package cli

import (
	"slices"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// Entries returns a copy of all entries in chronological order.
//
// Uses defer RUnlock: a panic during make([]EventEntry, l.count) (e.g. OOM
// for very large rings) would otherwise leave the lock permanently held and
// deadlock subsequent writers. The defer cost is a handful of ns and not
// material on the broadcast fan-out path.
//
// R247-PERF-13 / R246-PERF-16 [REPEAT-3]: Entries() allocates a fresh
// `[]EventEntry` of up to `maxSize=500` slots (~140KB) on every call, and the
// dashboard subscribe path on a 500-session deployment hits this in steady
// state. Callers that re-fetch the whole log on a hot loop (dashboard 1Hz
// poll, agent_tailer fan-in) should prefer `LastN(n)` with a bounded `n` to
// keep the working set small, or use `EntriesAppend(dst)` to recycle a
// caller-owned backing array via sync.Pool. Entries() is retained as the
// "give me everything" convenience used by tests and one-shot history dumps;
// the documented expectation is that production hot paths bound their reads.
func (l *EventLog) Entries() []EventEntry {
	return l.LastNAppend(nil, 0)
}

// LastN returns the most recent n entries in chronological order.
// If n <= 0 or n >= count, all entries are returned.
//
// Uses defer RUnlock; see Entries for rationale. Backing array pooled —
// see Entries godoc for the lifetime contract.
func (l *EventLog) LastN(n int) []EventEntry {
	return l.LastNAppend(nil, n)
}

// EntriesAppend copies all entries in chronological order into `dst`,
// reslicing it (and growing the backing array if cap is short). When
// `dst` already has enough capacity (e.g. retrieved from a sync.Pool
// of pre-grown buffers), no allocation occurs on the hot path.
//
// Pass dst[:0] when reusing a pooled buffer; passing nil is equivalent
// to Entries() (allocates a fresh slice sized exactly to l.count).
//
// R247-PERF-13: callers on the dashboard fan-out path (poll-style
// refresh on every WS notify) can amortise the per-call ~140KB
// allocation by holding a sync.Pool of `[]EventEntry` and rotating
// the slice through this method. Lifetime contract: the returned
// slice is fully owned by the caller after the call returns; the
// EventLog never retains a reference. Callers that route the slice
// onto a channel must NOT recycle it until the consumer signals
// completion — standard pool-of-slice discipline.
func (l *EventLog) EntriesAppend(dst []EventEntry) []EventEntry {
	return l.LastNAppend(dst, 0)
}

// LastNAppend is the buffer-reusing variant of LastN. See EntriesAppend
// for the lifetime contract; pass `n<=0` for "all entries" semantics.
func (l *EventLog) LastNAppend(dst []EventEntry, n int) []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := l.count
	if n > 0 && n < count {
		count = n
	}
	if cap(dst) >= count {
		dst = dst[:count]
	} else {
		dst = make([]EventEntry, count)
	}
	start := (l.head - count + l.maxSize) % l.maxSize
	// Branch-on-wrap: avoid per-step modulo on the hot WS polling path.
	// Mirrors the EntriesSince / LastNVisible branch-on-wrap pattern.
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
// slice (internal events included) chosen so that the slice contains at least
// `visibleTarget` visible entries — i.e. entries IsVisibleEntry reports true
// for. The walk stops at the first of:
//
//   - the slice holds >= visibleTarget visible entries, OR
//   - the slice length reaches maxTotal (a cost ceiling — when the tail is
//     dominated by a parallel agent team's tool_use / task_progress flood the
//     visible count may end up below the target), OR
//   - the whole ring has been scanned.
//
// Why a contiguous run rather than only the visible entries: the dashboard's
// initial render rebuilds turnState by scanning the history backward for the
// last turn boundary, and the running banner ("正在使用 X") is reconstructed
// from the interleaved tool_use / task_* events. Dropping the internal events
// here would leave the banner blank on first paint. The dashboard still
// filters them out of the transcript via processEventsForDisplay; they ride
// along purely to seed turn state.
//
// visibleTarget <= 0 falls back to LastN(maxTotal) semantics (no visible
// accounting). maxTotal <= 0 is treated as "the whole ring".
//
// The earliest Time in the returned slice is the cursor a caller uses to
// continue paginating into the disk tier (EventEntriesBeforeCtx) when the ring
// alone could not satisfy visibleTarget.
func (l *EventLog) LastNVisible(visibleTarget, maxTotal int) []EventEntry {
	return l.LastNVisibleAppend(nil, visibleTarget, maxTotal)
}

// LastNVisibleAppend is the buffer-reusing variant of LastNVisible.
// Matched entries are appended into `dst` (re-sliced from `dst[:0]`); when
// `dst` already has enough capacity — e.g. retrieved from a
// sync.Pool[*[]EventEntry] rotated across polls (the listRefsPool pattern
// in router_core) — the dashboard first-render walk of up to maxTotal ring
// slots no longer allocates a fresh rev slice on every call.
//
// R20260602-PERF-8 (#1631): dashboard first render uses
// visibleTarget=50 / maxTotal=200, so the unpooled LastNVisible allocated
// a 200-cap []EventEntry under l.mu.RLock on each subscribe. Mirrors the
// EntriesSinceAppend / LastNAppend convention so a pooled caller can drop
// that per-call allocation. The slices.Reverse stays OUTSIDE l.mu (same as
// EntriesSince, R220-PERF-3): the RLock is released the instant the
// backward scan finishes and the reverse touches only the locally-owned
// buffer, so a long maxTotal walk never blocks a concurrent Append longer
// than the scan itself.
//
// Lifetime: the returned slice is fully owned by the caller after the call
// returns; the EventLog never retains a reference. Passing nil falls back
// to LastNVisible's allocate-and-return behaviour (and returns nil on an
// empty ring, preserving the original API contract).
func (l *EventLog) LastNVisibleAppend(dst []EventEntry, visibleTarget, maxTotal int) []EventEntry {
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
	// Walk backward from the newest slot, branch-on-wrap (no per-step modulo),
	// collecting into a reverse buffer until a stop condition trips. Reuse the
	// caller's pooled backing array when it is large enough; append grows it
	// organically otherwise.
	rev := dst[:0]
	if cap(rev) < limit {
		rev = make([]EventEntry, 0, limit)
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

// Count returns the current number of valid entries (0..maxSize).
// Useful for sync.Pool-backed callers that want to right-size their
// scratch buffer before a LastNAppend call.
func (l *EventLog) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.count
}

// EntriesSince returns entries after the given unix ms timestamp, in chronological order.
// Single-pass backward scan collects matches into a reverse buffer; the caller
// receives them in chronological order. Previous implementation did two passes
// (count, then copy forward), touching each matched ring slot twice. For the
// hot streaming path (k = 1-5 new events per notify) the constant savings are
// small but the code path is simpler and avoids the arithmetic error surface
// of two separate modular indexing expressions.
//
// R220-PERF-3 (#685): the in-place slices.Reverse runs OUTSIDE l.mu so an
// initial subscribe with hundreds of matched entries cannot block a
// concurrent Append. The reverse-buffer (`rev`) holds value-copied
// EventEntry slots; once the scan returns we no longer touch l.entries
// and the lock can be released. RUnlock is explicit (not deferred) so
// the reverse touches the locally-owned slice only, shrinking the
// reader-blocks-writer footprint from "RLock for the whole function"
// to "RLock for the scan only".
//
// R222-PERF-11 (#708) cross-reference: this method intentionally returns
// []EventEntry, not pre-marshaled JSON bytes. The original ticket proposed
// caching marshaled output here so multi-tab dashboards on the same session
// would not each pay a fresh json.Marshal per notify wave. That cache lives
// at the WS hub layer instead — see internal/server/wshub_eventpush_cache.go
// (R214-PERF-4) — because EventLog is wire-format-agnostic (other consumers
// are platform IM senders, history persisters, the cron scheduler, etc.,
// each with their own encoding). Future contributors: do NOT introduce a
// JSON cache at this layer; extend the hub-side coalescer if a new fan-out
// site needs the same coalescing.
func (l *EventLog) EntriesSince(afterMS int64) []EventEntry {
	return l.EntriesSinceAppend(nil, afterMS)
}

// EntriesSinceAppend is the buffer-reusing variant of EntriesSince. The
// matched entries are appended into `dst` (re-sliced from `dst[:0]`); when
// `dst` already has enough capacity (e.g. retrieved from a sync.Pool of
// pre-grown buffers) no allocation occurs on the matched path.
//
// R249-PERF-18 (#937): dashboard streaming-tail callers fan out at
// ~1Hz × N tabs × N sessions and the typical match count is 1-5 entries,
// so the per-call allocation of a fresh `[]EventEntry` was the dominant
// per-poll heap churn even though each slice itself was small. Mirrors
// the LastNAppend / EntriesAppend pattern already used for full-ring reads.
//
// Pass dst[:0] when reusing a pooled buffer; passing nil falls back to
// EntriesSince's allocate-lazily behaviour and returns nil when no entries
// match (preserving the pre-existing API contract). Lifetime: the returned
// slice is fully owned by the caller after the call returns; the EventLog
// never retains a reference.
func (l *EventLog) EntriesSinceAppend(dst []EventEntry, afterMS int64) []EventEntry {
	l.mu.RLock()
	if l.count == 0 {
		l.mu.RUnlock()
		// Preserve the original "nil when no matches" return contract for
		// dst==nil callers. Append-mode callers passing a pooled dst[:0]
		// receive their own buffer back length-zero.
		if dst == nil {
			return nil
		}
		return dst[:0]
	}
	// First pass: collect matches in reverse order. Most calls match 0-5
	// entries so we allocate lazily only when the first match is found.
	//
	// R249-PERF-17: hoist the modulo arithmetic out of the loop.
	// Previously each iter recomputed `(l.head - l.count + i + l.maxSize) % l.maxSize`
	// — a DIV per step. Walk backward from the newest slot with a cheap
	// branch-on-wrap instead. ~5-10ns × notify wave on hot streaming path.
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
			// Typical streaming match count is 1-5; cap at entriesSinceInitialCap
			// so sessions with hundreds of buffered entries don't allocate a
			// giant backing array on every notify. `append` will grow organically
			// if the match count exceeds this hint. R249-PERF-18 (#937).
			initialCap := l.count - i
			if initialCap > entriesSinceInitialCap {
				initialCap = entriesSinceInitialCap
			}
			rev = make([]EventEntry, 0, initialCap)
		}
		rev = append(rev, l.entries[idx])
		idx--
		if idx < 0 {
			idx += l.maxSize
		}
	}
	l.mu.RUnlock()
	if len(rev) == 0 {
		// Original EntriesSince returned nil when nothing matched; preserve
		// that for the no-buffer caller. Pool callers with cap(dst)>0 keep
		// their buffer length-zero so they can retain it for the next poll.
		if dst == nil {
			return nil
		}
		return rev
	}
	slices.Reverse(rev)
	return rev
}

// EntriesBefore returns up to `limit` entries whose Time < beforeMS, in
// chronological order. Drives the dashboard "load earlier" pagination:
// caller passes the timestamp of the oldest currently-rendered event and
// gets the preceding page.
//
// A beforeMS of 0 is treated as "no upper bound" (equivalent to LastN).
// A non-positive limit returns nil.
func (l *EventLog) EntriesBefore(beforeMS int64, limit int) []EventEntry {
	return l.EntriesBeforeAppend(nil, beforeMS, limit)
}

// EntriesBeforeAppend is the buffer-reusing variant of EntriesBefore.
// See EntriesSinceAppend for the lifetime contract; semantics for
// `beforeMS<=0` and `limit<=0` match EntriesBefore exactly. R249-PERF-18
// (#937): wired alongside EntriesSinceAppend so dashboard pagination
// callers can rotate a single sync.Pool[*[]EventEntry] across both
// streaming-tail and load-earlier paths.
func (l *EventLog) EntriesBeforeAppend(dst []EventEntry, beforeMS int64, limit int) []EventEntry {
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

	// Walk backward from newest, skip entries whose Time >= beforeMS, collect
	// up to `limit` matches into a reverse buffer. Single pass keeps the code
	// symmetric with EntriesSince.
	//
	// R040034-CHANGES (#1383 review): the previous "crossed" fast-path
	// assumed entries are stored in monotonically-non-decreasing Time
	// order, switching from per-entry filter to "collect greedily" once
	// the first sub-beforeMS entry was seen. That assumption is too strong:
	// Append/AppendBatch accept caller-supplied Time values (only stamping
	// defaultTime for Time==0), and live + AppendBatchReplay can interleave
	// during InjectHistory replay. A late high-Time entry behind an
	// earlier low-Time entry would let the fast-path emit it as if it
	// satisfied Time < beforeMS — a false positive that handed a stale
	// page to the dashboard.
	//
	// Per-entry filter is now the only mode. Walk cost is O(count) in
	// the worst case (500-entry ring) but EntriesBefore is the dashboard
	// "load earlier" path, only invoked on user scroll / paginate, far
	// from any hot path. The earlier optimisation traded correctness for
	// a bound that almost no caller observed.
	//
	// R249-PERF-17: walk backward with hoisted index instead of recomputing
	// (l.head - l.count + i + l.maxSize) % l.maxSize per iter. Same shape
	// as EntriesSince — branch-on-wrap is one CMOV/cmp vs an IDIV.
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
			rev = make([]EventEntry, 0, initialCap)
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
// entry. Used by the sidebar to render a 30-rune dim preview line under the
// prompt (R110-P1). Empty when no assistant text has streamed yet.
func (l *EventLog) LastResponseSummary() string {
	return loadAtomicString(&l.lastResponseSummary)
}

// LastEventAt returns the wall-clock time of the most recent live Append,
// or the zero Time when no live event has been appended yet (only
// InjectHistory / AppendBatch replays, or a freshly spawned log).
// Consumed by Router.Cleanup to avoid misclassifying a long-running but
// actively streaming turn as a stuck session. Lock-free.
func (l *EventLog) LastEventAt() time.Time {
	ns := l.lastEventAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// UserTurnCount returns the cumulative count of "user" entries appended to
// this log since the Process was spawned. Consumed by SessionSnapshot.MessageCount
// for sidebar / main-header display. Increments once per Append of a user entry
// and by the batch's user-entry count inside AppendBatch. Ring-buffer eviction
// does not decrement.
func (l *EventLog) UserTurnCount() int64 {
	return l.userTurnCount.Load()
}

// loadAtomicString and storeAtomicString are thin wrappers around the
// shared textutil.LoadAtomicString / textutil.StoreAtomicString helpers
// (R219-CR-1: was a word-for-word copy of session.loadAtomicString /
// storeAtomicString — both word orders inverted before the rename). Kept
// as package-private aliases so the dense Append hot path stays readable
// and call sites do not have to spell out the textutil import path.
// Behavioural contract — fast-path short-circuit on equal value,
// last-writer-wins under l.mu — is documented on the textutil helpers;
// do not re-document the rationale here to keep the two in sync.
//
// R215-PERF-P2-4 archive anchor: the `new(string)` heap alloc on actual
// change is structurally required by atomic.Pointer[string] — Pointer.Store
// needs an addressable string slot. The textutil.StoreAtomicString
// fast-path skips the alloc when the value is unchanged, which covers the
// common steady-state case (same prompt summary repeated). On real
// change there is no zero-alloc atomic-string solution short of moving
// to atomic.Value (which has comparable cost) or a uintptr+intern-table
// scheme (much larger refactor for marginal gain on a low-frequency path
// — turn boundaries, not per stdout line).
func loadAtomicString(v *atomic.Pointer[string]) string {
	return textutil.LoadAtomicString(v)
}

func storeAtomicString(v *atomic.Pointer[string], s string) {
	textutil.StoreAtomicString(v, s)
}
