package cron

import "sync"

// jobGateShards is the fixed number of mutexes sharding the per-jobID gate.
// 64 keeps contention negligible for the maxJobsHardCap=500 working set
// (expected ~8 jobs per shard) while costing a flat 64×sizeof(sync.Mutex)
// regardless of how many job IDs churn through the process lifetime.
const jobGateShards = 64

// jobGateLock returns the sharded mutex that serialises the
// jobInflight-load → running.CompareAndSwap pair in executeOpt against the
// Load → running-check → CompareAndDelete pair in cleanupRunningJobIfIdle
// for the same jobID.
//
// R20260603140013-GO-2 (#1706): without this serialisation the two pairs
// race. executeOpt does `inflight := s.jobInflight(j.ID)` (gets the old
// *runInflight) and only THEN CASes its gate; a DeleteJob racing TriggerNow
// can run cleanupRunningJobIfIdle's CompareAndDelete in that gap, dropping
// the map entry while executeOpt still holds — and successfully CASes — the
// now-orphaned old gate. A second executeOpt for the same jobID then
// LoadOrStores a FRESH *runInflight and CAS-wins on it too, so two
// goroutines hold distinct gates for one jobID → double execution. The old
// code (scheduler_run.go comment, pre-fix) accepted this on the grounds the
// second precondition (crypto/rand ID reuse) is ~2^-32, but the first
// precondition (DeleteJob racing TriggerNow) is deterministic, so the window
// was real. Holding this lock across BOTH pairs closes it: cleanup can only
// observe the gate as either idle-and-deletable (executeOpt not in its
// load→CAS window) or running (CAS already won → cleanup skips), never the
// orphan-in-between state.
//
// Sharding rather than a growing map[jobID]*sync.Mutex is deliberate: a
// per-key lock map would reintroduce exactly the unbounded growth that
// cleanupRunningJobIfIdle exists to bound, plus a lock-map-cleanup TOCTOU of
// its own. The gate is a pure leaf lock — it is taken only outside s.mu (both
// executeOpt's gate and deleteJobPostCleanup run lock-free), and nothing
// inside the critical sections re-enters it, so it cannot participate in a
// lock-ordering cycle.
func (s *Scheduler) jobGateLock(jobID string) *sync.Mutex {
	return &s.jobGates[jobGateShardIndex(jobID)]
}

// jobGateShardIndex hashes jobID to a shard via FNV-1a (32-bit). Inlined
// rather than pulling in hash/fnv so the hot executeOpt path pays no
// interface/alloc overhead — jobIDs are short hex strings.
func jobGateShardIndex(jobID string) uint32 {
	var h uint32 = 2166136261 // FNV offset basis
	for i := 0; i < len(jobID); i++ {
		h ^= uint32(jobID[i])
		h *= 16777619 // FNV prime
	}
	return h % jobGateShards
}
