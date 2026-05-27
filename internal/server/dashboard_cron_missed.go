package server

import (
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
)

// This file holds the missed-schedule cache shared by the cron list handler
// (handleList) so a 1-Hz dashboard poll does not re-Parse the cron
// expression for every job on every tick. Extracted from dashboard_cron.go
// (#1281) — the cache state is a self-contained subsystem and pulling it
// out keeps the handler bodies focused on routing/persistence. The
// CronHandlers struct still owns the mu/map fields; only the helper, the
// value type, and the tuning constants moved.

// missedVerdict caches one HasMissedSchedule return tuple plus the inputs
// that decide whether the entry is still valid. Stored under
// CronHandlers.missedCache keyed by `jobID|schedule|startedNs` so a
// schedule edit (UpdateJob) or scheduler restart invalidates by key
// turnover (old keys become unreachable; the entry GCs away once the cap
// rotates). LastRunAt is intentionally NOT in the key — instead, it lives
// in the value as `lastRunNanos` so a tick that follows a fresh run
// triggers a recompute without growing the keyspace. R245-PERF-4 (#857).
type missedVerdict struct {
	missed       bool
	prevAt       time.Time
	lastRunNanos int64
	computedAt   time.Time
}

// missedCacheTTL is the freshness window for cached HasMissedSchedule
// verdicts. 1 s matches the dashboard poll cadence — verdicts that are
// up to one tick stale are equivalent to verdicts a parallel poller
// would have just computed, which is the same staleness the human eye
// sees on the rendered card anyway. R245-PERF-4 (#857).
const missedCacheTTL = time.Second

// missedCacheCap caps the cache size so a runtime UpdateJob storm (which
// turns over the (jobID, schedule, startedAt) key on every edit) cannot
// grow the map without bound. 2500 entries × ~120 bytes ≈ 300 KiB worst
// case — comfortably within budget for a heartbeat-path data structure
// and well above the practical N for a single naozhi instance. When the
// cap is hit we drop the entire map and let it rebuild; a sweep would
// pay map-iteration cost on a hot path for marginal benefit. R245-PERF-4
// (#857).
const missedCacheCap = 2500

// missedScheduleVerdict returns HasMissedSchedule(j, now, startedAt) but
// memoises the result for missedCacheTTL so the 1 Hz dashboard poll does
// not re-Parse the cron expression on every job per tick. Cache hits skip
// the regexp NFA build entirely; misses fall through to the cron package
// and store the freshly computed tuple for the next poller. Safe to call
// from concurrent goroutines (mu-protected map; map access is short and
// uncontended in practice because handleList serialises per request, but
// multiple parallel dashboard tabs each have their own request goroutine
// and may overlap). R245-PERF-4 (#857).
func (h *CronHandlers) missedScheduleVerdict(j *cron.Job, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	startedNs := startedAt.UnixNano()
	key := j.ID + "|" + j.Schedule + "|" + strconv.FormatInt(startedNs, 10)
	lastRunNanos := j.LastRunAt.UnixNano()

	h.missedCacheMu.Lock()
	if h.missedCache != nil {
		if v, ok := h.missedCache[key]; ok {
			if v.lastRunNanos == lastRunNanos && now.Sub(v.computedAt) < missedCacheTTL {
				h.missedCacheMu.Unlock()
				return v.missed, v.prevAt
			}
		}
	}
	h.missedCacheMu.Unlock()

	missed, prevAt := cron.HasMissedSchedule(j, now, startedAt)

	h.missedCacheMu.Lock()
	if h.missedCache == nil || len(h.missedCache) >= missedCacheCap {
		// Lazy-init AND cap-reset use the same allocation: dropping the
		// whole map at the cap is cheaper than walking it to evict the
		// oldest entry, and the cap is large enough that a real workload
		// (jobs-on-disk × handful-of-tabs) will not approach it.
		h.missedCache = make(map[string]missedVerdict, 64)
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
