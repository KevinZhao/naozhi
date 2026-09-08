package cron

import (
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// missedVerdict caches one HasMissedSchedule tuple. Keyed by
// `jobID|schedule|startedNs` so schedule edits / restarts invalidate by key
// turnover; lastRunNanos in the value forces a recompute after a fresh run (#857).
type missedVerdict struct {
	missed       bool
	prevAt       time.Time
	lastRunNanos int64
	computedAt   time.Time
}

// missedCacheTTL matches the dashboard poll cadence: a verdict up to one tick
// stale is what a parallel poller would have just computed anyway (#857).
const missedCacheTTL = time.Second

// missedCacheCap bounds the cache (2500 × ~120 B ≈ 300 KiB) so an UpdateJob
// storm cannot grow it without bound. On overflow the oldest decile is shed
// rather than dropping the whole map, so long-lived jobs stay warm (#1352).
const missedCacheCap = 2500

// missedCacheEvictRatio is the fraction of oldest entries shed on overflow; 10%
// keeps the sort+delete sweep amortised without a second LRU index (#1352).
const missedCacheEvictRatio = 10

// missedScheduleVerdict returns HasMissedSchedule(j, now, startedAt), memoised
// for missedCacheTTL. Safe for concurrent callers (#857).
func (h *Handlers) missedScheduleVerdict(j *cronpkg.Job, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	startedNs := startedAt.UnixNano()
	key := j.ID + "|" + j.Schedule + "|" + strconv.FormatInt(startedNs, 10)
	lastRunNanos := j.LastRunAt.UnixNano()

	h.missedCacheMu.RLock()
	if h.missedCache != nil {
		if v, ok := h.missedCache[key]; ok {
			if v.lastRunNanos == lastRunNanos && now.Sub(v.computedAt) < missedCacheTTL {
				h.missedCacheMu.RUnlock()
				return v.missed, v.prevAt
			}
		}
	}
	h.missedCacheMu.RUnlock()

	missed, prevAt := cronpkg.HasMissedSchedule(j, now, startedAt)

	h.missedCacheMu.Lock()
	if h.missedCache == nil {
		h.missedCache = make(map[string]missedVerdict, 64)
	} else if len(h.missedCache) >= missedCacheCap {
		// Shed the oldest decile so long-lived entries survive an UpdateJob burst (#1352).
		evictOldestMissedCache(h.missedCache)
	}
	h.missedCache[key] = missedVerdict{
		missed:       missed,
		prevAt:       prevAt,
		lastRunNanos: lastRunNanos,
		computedAt:   now,
	}
	h.missedCacheMu.Unlock()
	return missed, prevAt
}

// evictOldestMissedCache drops the oldest 1/missedCacheEvictRatio of entries by
// computedAt (at least one). Caller holds h.missedCacheMu (#1352).
func evictOldestMissedCache(m map[string]missedVerdict) {
	n := len(m)
	if n == 0 {
		return
	}
	drop := n / missedCacheEvictRatio
	if drop < 1 {
		drop = 1
	}
	if drop > n {
		drop = n
	}
	type kt struct {
		k string
		t time.Time
	}
	entries := make([]kt, 0, n)
	for k, v := range m {
		entries = append(entries, kt{k: k, t: v.computedAt})
	}
	slices.SortFunc(entries, func(a, b kt) int {
		return a.t.Compare(b.t)
	})
	for i := 0; i < drop; i++ {
		delete(m, entries[i].k)
	}
}

// recentRunsPerJob is the per-job RecentRuns cap in HandleList's response
// (tooltip-bound); the runs drawer uses GET /api/cron/runs for pagination.
const recentRunsPerJob = 5

// batchRecentRunsWorkers caps concurrent RecentRuns goroutines in
// batchRecentRuns; above ~16 readers jobLock contention makes speedup negative (#525).
const batchRecentRunsWorkers = 8

// batchRecentRuns fans out scheduler.RecentRuns across at most
// batchRecentRunsWorkers goroutines and returns one result per job in input
// order (nil entries for jobs with no history). Nil-safe on empty input (#525).
func (h *Handlers) batchRecentRuns(jobs []cronpkg.JobWithNextRun, n int) [][]cronpkg.CronRunSummary {
	if len(jobs) == 0 || h.scheduler == nil {
		return nil
	}
	out := make([][]cronpkg.CronRunSummary, len(jobs))
	// Each worker atomically claims the next index so the distribution
	// self-balances without allocating a per-call channel (#1847).
	var next atomic.Int64
	workers := batchRecentRunsWorkers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				idx := int(next.Add(1)) - 1
				if idx >= len(jobs) {
					break
				}
				out[idx] = h.scheduler.RecentRuns(jobs[idx].Job.ID, n)
			}
		}()
	}
	wg.Wait()
	return out
}
