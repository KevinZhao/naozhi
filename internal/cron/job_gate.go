package cron

import "sync"

// jobGateShards is the fixed number of mutexes sharding the per-jobID gate.
// 64 keeps contention negligible for maxJobsHardCap=500 (~8 jobs per shard)
// at a flat 64×sizeof(sync.Mutex) regardless of job-ID churn.
const jobGateShards = 64

// jobGateLock returns the sharded mutex that serialises executeOpt's
// jobInflight-load → running.CompareAndSwap pair against
// cleanupRunningJobIfIdle's Load → running-check → CompareAndDelete pair for
// the same jobID. Without it a DeleteJob racing TriggerNow can drop the map
// entry between executeOpt's load and CAS, so a second executeOpt LoadOrStores
// a fresh gate and both CAS-win → double execution (#1706). Holding the lock
// across BOTH pairs means cleanup sees the gate either idle-and-deletable or
// running, never the orphan in-between. Sharded rather than a growing
// map[jobID]*sync.Mutex (that would reintroduce the unbounded growth cleanup
// exists to bound). Pure leaf lock: taken only outside s.mu, never re-entered.
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
