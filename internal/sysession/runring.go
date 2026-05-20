package sysession

import "sync"

// runRingCap caps how many DaemonRun records we keep per daemon.  Phase 1
// is in-memory only (RFC §3.4); 50 is enough for the dashboard "recent
// activity" panel and small enough that a daemon spinning at 1Hz for an
// hour can't eat measurable RAM.
const runRingCap = 50

// runRing is a fixed-size ring buffer of DaemonRun records, scoped per
// daemon.  Append is O(1); Snapshot returns a chronological copy
// (oldest → newest) so callers don't have to think about the head/tail
// pointer.
//
// All access is mu-protected.  We use a regular sync.Mutex (not RWMutex)
// because reads happen from a single dashboard goroutine on a low cadence
// (≤ 1Hz) so the lock-upgrade complexity isn't worth it.
type runRing struct {
	mu     sync.Mutex
	buf    []DaemonRun
	head   int  // next write position
	filled bool // true once the ring has wrapped
}

func newRunRing() *runRing {
	return &runRing{buf: make([]DaemonRun, runRingCap)}
}

// Append records a finished run.  Old entries are overwritten without
// notice once the ring is full — that's the contract.
func (r *runRing) Append(run DaemonRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = run
	r.head++
	if r.head >= runRingCap {
		r.head = 0
		r.filled = true
	}
}

// Snapshot returns a chronologically ordered copy (oldest → newest) of
// every recorded run.  Returns an empty slice (not nil) when no runs
// have been recorded.
//
// The caller owns the returned slice.
func (r *runRing) Snapshot() []DaemonRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.filled {
		// Linear order; head is the count.
		out := make([]DaemonRun, r.head)
		copy(out, r.buf[:r.head])
		return out
	}
	// Wrapped: oldest is at head, newest is at head-1 (mod cap).
	out := make([]DaemonRun, runRingCap)
	copy(out, r.buf[r.head:])
	copy(out[runRingCap-r.head:], r.buf[:r.head])
	return out
}

// Latest returns the most recently appended run, or zero-value + false
// when the ring is empty.  Cheaper than Snapshot when callers only need
// the last entry (dashboard "last_run_*" fields).
func (r *runRing) Latest() (DaemonRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.filled && r.head == 0 {
		return DaemonRun{}, false
	}
	idx := r.head - 1
	if idx < 0 {
		idx = runRingCap - 1
	}
	return r.buf[idx], true
}

// Len returns the number of runs currently in the ring (≤ runRingCap).
func (r *runRing) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.filled {
		return runRingCap
	}
	return r.head
}
