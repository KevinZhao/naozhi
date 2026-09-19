package cron

// run_gate.go — the per-job execution gate: at most one run of a job at a time
// (docs/rfc/cron-jobtable-extraction.md §3.3).
//
// This is deliberately NOT part of jobTable, and not entryMu either. The three
// locks answer different questions — jobTable.mu: "what jobs exist and how are
// they indexed"; entryMu: "who may swap a job's robfig entry"; runGate: "is a
// run of this job in flight right now" — and the reviews that shaped the RFC
// found the gate and the table are never held together today, with nothing but
// convention keeping that true. Making the gate its own type with its own
// state is half of the guarantee; TestGateNeverUnderTableLock is the other
// half, the machine check that a future caller does not nest them.
//
// Two pieces of state move in lockstep:
//
//   - runningJobs maps jobID → *runInflight, whose atomic running Bool is the
//     CAS that rejects overlapping runs (TriggerNow bypasses robfig's
//     SkipIfStillRunning, so this is the uniform guard for every path).
//   - jobGates shards mutexes over jobIDs to make the two multi-step sequences
//     atomic against each other: acquire's load → CAS pair, and reclaimIfIdle's
//     load → running-check → delete pair. Without that a DeleteJob racing a
//     TriggerNow drops the map entry between load and CAS, a second acquire
//     LoadOrStores a fresh gate, and both CAS-win → double execution (#1706).
//
// Lock order: the gate is a leaf — nothing inside a gate operation calls out,
// so nothing inside one can want jobTable.mu or entryMu. jobTable.mu is never
// held on entry (machine-checked by TestGateNeverUnderTableLock). entryMu MAY
// be held on entry, in exactly one direction: DeleteJob's transaction reclaims
// the gate entry while holding entryMu, and must — otherwise every deleted job
// leaks its *runInflight until the next delete sweeps the shard. entryMu →
// gate is therefore the documented order; the reverse is structurally
// impossible while the gate stays a leaf.
//
// Scheduler holds this as the named field gate. Tests that inspect the slot
// directly go through the gateForTest port in export_test.go.

import (
	"fmt"
	"log/slog"
	"sync"
)

// jobGateShards is the fixed number of mutexes sharding the per-jobID gate.
// 64 keeps contention negligible for maxJobsHardCap=500 (~8 jobs per shard)
// at a flat 64×sizeof(sync.Mutex) regardless of job-ID churn.
const jobGateShards = 64

type runGate struct {
	// runningJobs maps jobID → *runInflight. Entries are created lazily by
	// jobInflight and reclaimed on DeleteJob via cleanupRunningJobIfIdle when
	// the gate is idle.
	runningJobs sync.Map
	// jobGates shards a fixed pool of mutexes (hashed jobID); see the file
	// comment for the two sequences they serialise.
	jobGates [jobGateShards]sync.Mutex
}

// gateHook, when set, runs at the top of every gate operation that takes a
// shard mutex. Test-only seam: the lock-discipline test installs a hook that
// proves the registry lock is not held by the goroutine entering the gate.
var gateHook func()

// jobGateLock returns the sharded mutex for jobID.
func (g *runGate) jobGateLock(jobID string) *sync.Mutex {
	return &g.jobGates[jobGateShardIndex(jobID)]
}

// jobGateShardIndex hashes jobID to a shard via FNV-1a (32-bit). Inlined
// rather than pulling in hash/fnv so the hot acquire path pays no
// interface/alloc overhead — jobIDs are short hex strings.
func jobGateShardIndex(jobID string) uint32 {
	var h uint32 = 2166136261 // FNV offset basis
	for i := 0; i < len(jobID); i++ {
		h ^= uint32(jobID[i])
		h *= 16777619 // FNV prime
	}
	return h % jobGateShards
}

// acquire wins or loses the run slot for jobID in one atomic step: shard lock,
// lazy inflight lookup, CAS. On true the caller owns the run and must
// eventually Store(false) through its finalizer; on false another run is in
// flight and the caller decides what an overlap means for its entry point
// (execAcquireSlot broadcasts a skip; dispatchReplay returns a 409).
func (g *runGate) acquire(jobID string) (*runInflight, bool) {
	if gateHook != nil {
		gateHook()
	}
	gate := g.jobGateLock(jobID)
	gate.Lock()
	inflight := g.jobInflight(jobID)
	won := inflight.running.CompareAndSwap(false, true)
	gate.Unlock()
	return inflight, won
}

// jobInflight returns a lazily created *runInflight per job ID. The embedded
// atomic Bool is the CAS gate acquire uses to reject concurrent runs; the
// surrounding metadata (RunID/StartedAt/Phase) feeds the list API.
func (g *runGate) jobInflight(id string) *runInflight {
	if v, ok := g.runningJobs.Load(id); ok {
		if inf, ok := v.(*runInflight); ok && inf != nil {
			return inf
		}
	}
	guard := &runInflight{}
	actual, _ := g.runningJobs.LoadOrStore(id, guard)
	if inf, ok := actual.(*runInflight); ok && inf != nil {
		return inf
	}
	// Should be unreachable given LoadOrStore's contract, but never return
	// nil to callers — they immediately call methods on the result.
	return guard
}

// cleanupRunningJobIfIdle drops the runningJobs entry for jobID iff the CAS
// gate is currently false (no in-flight execute holds it), so a deployment
// that adds and deletes thousands of jobs does not accumulate dead
// *runInflight structs (#758). If the gate is held the entry is left alone —
// the executing goroutine still holds the pointer and is about to release;
// the leak is bounded by jobs deleted mid-run.
//
// Returns true if the entry was deleted. Safe to call after the registry lock
// is released — sync.Map needs no scheduler lock; callers run it from
// lock-free postCleanup branches.
func (g *runGate) cleanupRunningJobIfIdle(jobID string) bool {
	if gateHook != nil {
		gateHook()
	}
	// Take the per-jobID gate around the whole Load → running-check → delete
	// sequence so it is atomic relative to acquire's load → CAS pair, which
	// holds the same gate (#1706).
	gate := g.jobGateLock(jobID)
	gate.Lock()
	defer gate.Unlock()

	v, ok := g.runningJobs.Load(jobID)
	if !ok {
		return false
	}
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		// Package invariant violated upstream; log loud (#1392). CompareAndDelete
		// on the observed v (not LoadAndDelete on the key) so a concurrent
		// jobInflight that already replaced this stale entry is not collateral.
		slog.Error("cron: runningJobs holds unexpected value type; sweeping",
			"job_id", jobID, "type", fmt.Sprintf("%T", v))
		g.runningJobs.CompareAndDelete(jobID, v)
		return true
	}
	if inf.running.Load() {
		// In-flight execute() still holds the pointer and is about to release;
		// leaking this one entry until the next DeleteJob sweep is cheaper than
		// risking a CAS-gate split.
		return false
	}
	// CompareAndDelete on OUR observed inf pointer (not LoadAndDelete on the
	// key) so a fresh *runInflight stored by a racing AddJob+jobInflight is left
	// alone (#1416). The residual race — acquire loaded the inflight then
	// CAS-won on the orphaned old gate after we deleted it — is closed by the
	// per-jobID gate held above (#1706).
	g.runningJobs.CompareAndDelete(jobID, inf)
	return true
}

// peek returns the inflight record for jobID without creating one. The
// defensive type assertion mirrors cleanupRunningJobIfIdle's: a refactor that
// stores a different type degrades to "no inflight" instead of panicking.
func (g *runGate) peek(jobID string) (*runInflight, bool) {
	v, ok := g.runningJobs.Load(jobID)
	if !ok {
		return nil, false
	}
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		return nil, false
	}
	return inf, true
}

// rangeInflight invokes fn for every tracked *runInflight; fn returning false
// stops the iteration early, like sync.Map.Range. Entries holding an
// unexpected type are skipped.
func (g *runGate) rangeInflight(fn func(*runInflight) bool) {
	g.runningJobs.Range(func(_, v any) bool {
		inf, ok := v.(*runInflight)
		if !ok || inf == nil {
			return true
		}
		return fn(inf)
	})
}
