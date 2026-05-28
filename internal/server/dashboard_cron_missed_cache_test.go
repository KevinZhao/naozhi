package server

import (
	"strconv"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
)

// TestMissedScheduleVerdict_CachesAcrossPolls pins R245-PERF-4 (#857):
// the cache must serve a second call within missedCacheTTL without
// re-running cron.HasMissedSchedule, and the cached verdict must agree
// with the canonical function. We assert both halves: agreement on the
// first call (proves we are not silently flipping verdicts) and
// agreement on the second call (proves cache hits round-trip the same
// tuple as the real computation).
//
// The schedule "@every 1m" with LastRunAt zero and CreatedAt one hour
// ago crosses the "never-run AND created > period ago" branch in
// HasMissedSchedule, producing missed=true with a non-zero prevAt —
// gives us a non-trivial verdict whose cache hit/miss we can observe.
func TestMissedScheduleVerdict_CachesAcrossPolls(t *testing.T) {
	t.Parallel()

	h := &CronHandlers{}
	now := time.Now()
	startedAt := now.Add(-2 * time.Hour) // long past suppression window
	j := &cron.Job{
		ID:        "job-cached-once",
		Schedule:  "@every 1m",
		CreatedAt: now.Add(-1 * time.Hour),
	}

	missed1, prev1 := h.missedScheduleVerdict(j, now, startedAt)
	wantMissed, wantPrev := cron.HasMissedSchedule(j, now, startedAt)
	if missed1 != wantMissed {
		t.Errorf("first call missed = %v, want %v (verdict diverges from canonical)", missed1, wantMissed)
	}
	if !prev1.Equal(wantPrev) {
		t.Errorf("first call prevAt = %v, want %v", prev1, wantPrev)
	}

	// Second call within TTL should round-trip the same tuple via the
	// cache. We don't have a Parse counter to assert the path was hit,
	// but verdict equality + the cache map being populated below covers
	// the contract from both sides.
	missed2, prev2 := h.missedScheduleVerdict(j, now, startedAt)
	if missed2 != missed1 || !prev2.Equal(prev1) {
		t.Errorf("second call diverged from first: got (%v, %v), want (%v, %v)", missed2, prev2, missed1, prev1)
	}

	// Cache must contain exactly one entry under the composite key.
	h.missedCacheMu.Lock()
	got := len(h.missedCache)
	h.missedCacheMu.Unlock()
	if got != 1 {
		t.Errorf("cache size = %d, want 1 after two calls for the same job", got)
	}
}

// TestMissedScheduleVerdict_LastRunAtChangeInvalidates pins the cache
// invalidation contract: a fresh run advances Job.LastRunAt, which must
// force a recompute even within missedCacheTTL — otherwise a job that
// just succeeded would still report missed=true for up to a second.
func TestMissedScheduleVerdict_LastRunAtChangeInvalidates(t *testing.T) {
	t.Parallel()

	h := &CronHandlers{}
	now := time.Now()
	startedAt := now.Add(-2 * time.Hour)
	j := &cron.Job{
		ID:        "job-lastrun-flips",
		Schedule:  "@every 1m",
		CreatedAt: now.Add(-1 * time.Hour),
	}

	// Prime the cache: never-run job, missed=true.
	missed1, _ := h.missedScheduleVerdict(j, now, startedAt)
	if !missed1 {
		t.Fatalf("setup: expected missed=true for never-run @every-1m job created an hour ago, got false")
	}

	// Job runs successfully — LastRunAt advances to ~now.
	j.LastRunAt = now

	// Same TTL window. Cache hit by key would still report missed=true;
	// the lastRunNanos guard MUST recompute and report missed=false
	// because the just-run window is well inside the period × 1.5
	// slack.
	missed2, _ := h.missedScheduleVerdict(j, now, startedAt)
	if missed2 {
		t.Errorf("after LastRunAt advance, missed = true; cache invalidation guard failed (#857)")
	}
}

// TestMissedScheduleVerdict_NilJob locks the nil-guard so a future
// caller cannot panic the dashboard list handler by handing in a stale
// pointer.
func TestMissedScheduleVerdict_NilJob(t *testing.T) {
	t.Parallel()
	h := &CronHandlers{}
	missed, prev := h.missedScheduleVerdict(nil, time.Now(), time.Now())
	if missed || !prev.IsZero() {
		t.Errorf("nil job: got (%v, %v), want (false, zero)", missed, prev)
	}
}

// TestMissedScheduleVerdict_EvictsOldestOnOverflow verifies the LRU-by-
// computedAt eviction path: a burst that hits missedCacheCap must shed
// roughly the oldest decile of entries (R260528-PERF-5 / #1352) — not
// drop the entire map — so the warm long-lived heartbeat fleet survives
// a UpdateJob churn burst and the next poll does not pay N×Parse.
func TestMissedScheduleVerdict_EvictsOldestOnOverflow(t *testing.T) {
	t.Parallel()

	h := &CronHandlers{}
	now := time.Now()

	// Pre-seed the cache up to the cap with computedAt timestamps spread
	// across a deterministic ordering: index 0 is the oldest, index
	// missedCacheCap-1 is the newest. The eviction sweep should target
	// the low-index entries.
	h.missedCacheMu.Lock()
	h.missedCache = make(map[string]missedVerdict, missedCacheCap)
	for i := 0; i < missedCacheCap; i++ {
		key := "k|x|" + strconv.Itoa(i)
		h.missedCache[key] = missedVerdict{
			computedAt: now.Add(time.Duration(i) * time.Microsecond),
		}
	}
	preLen := len(h.missedCache)
	h.missedCacheMu.Unlock()

	if preLen != missedCacheCap {
		t.Fatalf("setup: cache size = %d, want %d", preLen, missedCacheCap)
	}

	// One more verdict push — the cap branch should evict the oldest
	// decile and insert the fresh entry.
	startedAt := now.Add(-2 * time.Hour)
	j := &cron.Job{ID: "overflow-trigger", Schedule: "@every 1m", CreatedAt: now.Add(-1 * time.Hour)}
	h.missedScheduleVerdict(j, now, startedAt)

	h.missedCacheMu.Lock()
	defer h.missedCacheMu.Unlock()
	postLen := len(h.missedCache)

	// After eviction: cap - drop + 1 (the new entry). drop ~= cap/ratio.
	wantDrop := missedCacheCap / missedCacheEvictRatio
	wantLen := missedCacheCap - wantDrop + 1
	if postLen != wantLen {
		t.Errorf("post-eviction cache size = %d, want %d (cap=%d, drop=%d, +1 fresh)",
			postLen, wantLen, missedCacheCap, wantDrop)
	}
	if postLen >= missedCacheCap {
		t.Errorf("cap eviction failed: cache size = %d, want < %d", postLen, missedCacheCap)
	}

	// The oldest decile must be gone; the newest decile must have
	// survived. Sample both ends to prove LRU-by-computedAt was honoured.
	oldestKey := "k|x|0"
	if _, stillThere := h.missedCache[oldestKey]; stillThere {
		t.Errorf("oldest entry %q survived eviction; LRU-by-computedAt order broken", oldestKey)
	}
	newestKey := "k|x|" + strconv.Itoa(missedCacheCap-1)
	if _, stillThere := h.missedCache[newestKey]; !stillThere {
		t.Errorf("newest entry %q evicted; LRU-by-computedAt order broken", newestKey)
	}
}
