package sysession

import "sync"

// runRingCap caps DaemonRun records kept per daemon: enough for the dashboard
// "recent activity" panel, small enough to bound RAM (RFC §3.4).
const runRingCap = 50

// runRing is a fixed-size per-daemon ring buffer of DaemonRun records. Append
// is O(1); Snapshot returns a chronological copy. All access is mu-protected.
//
// Invariants:
//   - len(buf) == runRingCap for the ring's lifetime (set once in newRunRing).
//   - 0 <= head < runRingCap at all times; Append wraps to 0 at runRingCap.
//   - filled becomes true on the first wrap and never returns to false.
//
// Snapshot 的两段 copy 依赖 0 <= head < runRingCap；任何重构要保持此约束，
// 否则 out[runRingCap-r.head:] 切片表达式将越界 panic。
type runRing struct {
	mu     sync.Mutex
	buf    []DaemonRun
	head   int  // next write position; invariant: 0 <= head < runRingCap
	filled bool // true once the ring has wrapped
}

func newRunRing() *runRing {
	return &runRing{buf: make([]DaemonRun, runRingCap)}
}

// Append records a finished run; old entries are overwritten once the ring is full.
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

// Snapshot returns a caller-owned, chronologically ordered (oldest → newest)
// copy of every recorded run; an empty slice (not nil) when none.
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

// Latest returns the most recently appended run, or zero-value + false when empty.
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
