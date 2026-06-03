package cron

import (
	"testing"
	"time"
)

// TestHasMissedSchedule_NilJob 验证 nil 输入不 panic。
func TestHasMissedSchedule_NilJob(t *testing.T) {
	t.Parallel()
	missed, prev := HasMissedSchedule(nil, time.Now(), time.Time{})
	if missed || !prev.IsZero() {
		t.Fatalf("nil job should return (false, zero); got (%v, %v)", missed, prev)
	}
}

// TestHasMissedSchedule_UnparsableSchedule_NoMiss 验证无法解析的 schedule
// 不误报 missed（保守）。
func TestHasMissedSchedule_UnparsableSchedule_NoMiss(t *testing.T) {
	t.Parallel()
	j := &Job{Schedule: "not-a-cron", CreatedAt: time.Now().Add(-time.Hour)}
	missed, _ := HasMissedSchedule(j, time.Now(), time.Time{})
	if missed {
		t.Fatal("bogus schedule should not be flagged missed")
	}
}

// TestHasMissedSchedule_StartupSuppression 验证刚启动的 5×period 抑制
// 窗口内不判 missed，即使 LastRunAt 为零。
func TestHasMissedSchedule_StartupSuppression(t *testing.T) {
	t.Parallel()
	now := time.Now()
	startedAt := now.Add(-30 * time.Minute) // 刚启动半小时
	j := &Job{
		Schedule:  "@every 30m", // period=30m, 5×period=2h30m
		CreatedAt: now.Add(-24 * time.Hour),
	}
	missed, _ := HasMissedSchedule(j, now, startedAt)
	if missed {
		t.Fatal("within startup suppression window should not flag missed")
	}
}

// TestHasMissedSchedule_RecentRun_NoMiss 验证刚跑过的 job 不判 missed。
func TestHasMissedSchedule_RecentRun_NoMiss(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// startedAt 足够早以绕过抑制窗口
	startedAt := now.Add(-10 * time.Hour)
	j := &Job{
		Schedule:  "@every 30m",
		CreatedAt: now.Add(-24 * time.Hour),
		LastRunAt: now.Add(-20 * time.Minute), // 20m 内跑过，比 period*1.5 新
	}
	missed, _ := HasMissedSchedule(j, now, startedAt)
	if missed {
		t.Fatal("recent run within period*1.5 should not be missed")
	}
}

// TestHasMissedSchedule_StaleRun_Missed 验证 LastRunAt 比 prev expected
// 老于 period*1.5 时判 missed。
func TestHasMissedSchedule_StaleRun_Missed(t *testing.T) {
	t.Parallel()
	now := time.Now()
	startedAt := now.Add(-10 * time.Hour)
	j := &Job{
		Schedule:  "@every 30m",
		CreatedAt: now.Add(-24 * time.Hour),
		LastRunAt: now.Add(-3 * time.Hour), // 3h 未跑，远超 period*1.5 (45m)
	}
	missed, prev := HasMissedSchedule(j, now, startedAt)
	if !missed {
		t.Fatal("3h stale LastRunAt should be flagged missed")
	}
	if prev.IsZero() {
		t.Error("prev expected time should be non-zero when missed=true")
	}
	// prev 应该在 last run 和 now 之间
	if !prev.After(j.LastRunAt) || !prev.Before(now) {
		t.Errorf("prev=%v should be between LastRunAt=%v and now=%v", prev, j.LastRunAt, now)
	}
}

// TestHasMissedSchedule_NeverRun_CreatedRecent 验证刚创建不到一个周期
// 的 job 即使没跑过也不算 missed（还没到它的第一次执行时刻）。
func TestHasMissedSchedule_NeverRun_CreatedRecent(t *testing.T) {
	t.Parallel()
	now := time.Now()
	startedAt := now.Add(-10 * time.Hour)
	j := &Job{
		Schedule:  "@every 30m",
		CreatedAt: now.Add(-10 * time.Minute), // 创建才 10m
	}
	missed, _ := HasMissedSchedule(j, now, startedAt)
	if missed {
		t.Fatal("never-run job within one period of creation should not be missed")
	}
}

// TestHasMissedSchedule_NeverRun_CreatedLongAgo 验证创建超过一个周期
// 但从未跑过的 job 判 missed。
func TestHasMissedSchedule_NeverRun_CreatedLongAgo(t *testing.T) {
	t.Parallel()
	now := time.Now()
	startedAt := now.Add(-10 * time.Hour)
	j := &Job{
		Schedule:  "@every 30m",
		CreatedAt: now.Add(-5 * time.Hour), // 5h 前创建，远超一个 period
	}
	missed, _ := HasMissedSchedule(j, now, startedAt)
	if !missed {
		t.Fatal("never-run job created 5h ago with 30m schedule should be missed")
	}
}

// TestPreviousTickBefore_IntervalSchedule 验证 previousTickBefore 在
// 简单 @every N 形态下能正确回推到上一次 tick。
func TestPreviousTickBefore_IntervalSchedule(t *testing.T) {
	t.Parallel()
	now := time.Now()
	prev := previousTickBefore("@every 30m", now)
	if prev.IsZero() {
		t.Fatal("should return non-zero prev for @every 30m")
	}
	// prev 应该严格 before now
	if !prev.Before(now) {
		t.Errorf("prev=%v should be strictly before now=%v", prev, now)
	}
	// 距 now 不超过一个 period
	if now.Sub(prev) > 31*time.Minute {
		t.Errorf("prev=%v is more than one period before now=%v", prev, now)
	}
}

// TestPreviousTickBefore_Unparsable 验证错误 schedule 返回零值。
func TestPreviousTickBefore_Unparsable(t *testing.T) {
	t.Parallel()
	prev := previousTickBefore("not-a-cron", time.Now())
	if !prev.IsZero() {
		t.Fatalf("unparsable schedule should return zero time, got %v", prev)
	}
}

// TestHasMissedSchedule_DailySchedule_RecentRun_NotMissed 锁定 daily cron
// 形态（`0 9 * * *`，period=24h）下 "LastRunAt 落在最近一次应触发时刻附近"
// 不判 missed。现有 TestHasMissedSchedule_RecentRun_NoMiss 只覆盖 `@every 30m`
// 等短周期；daily 形态是生产里最常见但测试盲区，`previousTickBefore` 回推
// 3×period（= 72h）的分支没有独立覆盖。RNEW-TEST-431。
//
// TZ 说明：now/LastRunAt 用固定 UTC 时刻。schedulePeriod 现在从调用方
// 显式接受 now（见 job.go）——HasMissedSchedule 把它传递给
// previousTickBefore 和 schedulePeriod，三者锚到同一瞬间，不再受 host TZ
// 或 DST 切换影响。
func TestHasMissedSchedule_DailySchedule_RecentRun_NotMissed(t *testing.T) {
	t.Parallel()
	// 用固定 UTC 时刻避免 host TZ 影响：9:15 UTC 对应 `0 9 * * *` 的最近一次
	// tick 是 "今天 9:00 UTC"，距 now 只差 15 分钟，远小于 period*1.5=36h。
	now := time.Date(2026, 5, 9, 9, 15, 0, 0, time.UTC)
	// startedAt 放 8 天前，足够越过 5×period=5 天的启动抑制窗口。
	startedAt := now.Add(-8 * 24 * time.Hour)
	j := &Job{
		Schedule:  "0 9 * * *",
		CreatedAt: now.Add(-30 * 24 * time.Hour),
		LastRunAt: time.Date(2026, 5, 9, 9, 0, 0, 0, time.UTC),
	}
	missed, _ := HasMissedSchedule(j, now, startedAt)
	if missed {
		t.Fatalf("daily 9am cron run at the expected tick must not be flagged missed (now=%v lastRun=%v)", now, j.LastRunAt)
	}
}

// TestHasMissedSchedule_DailySchedule_StaleByThreeDays_Missed 锁定 daily cron
// 形态下 "LastRunAt 已过 3 天没跑" 必须判 missed。对照上一个 not-missed
// 测试，这对断言同时覆盖 HasMissedSchedule 对 daily 形态的"跑过但过期"
// 判定；period*1.5=36h，3 天远超阈值。RNEW-TEST-431。
func TestHasMissedSchedule_DailySchedule_StaleByThreeDays_Missed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 9, 9, 15, 0, 0, time.UTC)
	startedAt := now.Add(-8 * 24 * time.Hour)
	j := &Job{
		Schedule:  "0 9 * * *",
		CreatedAt: now.Add(-30 * 24 * time.Hour),
		LastRunAt: now.Add(-3 * 24 * time.Hour), // 3 天没跑
	}
	missed, prev := HasMissedSchedule(j, now, startedAt)
	if !missed {
		t.Fatalf("daily 9am cron with LastRunAt=3 days ago must be flagged missed (now=%v lastRun=%v)", now, j.LastRunAt)
	}
	if prev.IsZero() {
		t.Error("prev expected tick must be non-zero when missed=true")
	}
	if !prev.Before(now) || !prev.After(j.LastRunAt) {
		t.Errorf("prev=%v should sit strictly between LastRunAt=%v and now=%v", prev, j.LastRunAt, now)
	}
}

// TestHasMissedSchedule_DailySchedule_StartupSuppression 锁定 daily cron 形态
// 下的启动抑制：5×period=5 天；重启 15 分钟后即使 LastRunAt 很老也不应
// 报 missed（避免运行几分钟就拉起一堆 "错过" 告警）。RNEW-TEST-431。
func TestHasMissedSchedule_DailySchedule_StartupSuppression(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 9, 9, 15, 0, 0, time.UTC)
	startedAt := now.Add(-15 * time.Minute) // 刚启动 15 分钟
	j := &Job{
		Schedule:  "0 9 * * *",
		CreatedAt: now.Add(-30 * 24 * time.Hour),
		LastRunAt: now.Add(-10 * 24 * time.Hour), // 10 天没跑
	}
	missed, _ := HasMissedSchedule(j, now, startedAt)
	if missed {
		t.Fatalf("startup suppression (5×24h window) must swallow missed flag even for 10-day-stale LastRunAt (startedAt=%v)", startedAt)
	}
}

// TestHasMissedScheduleCached_MatchesUncached 验证 cached 变体在缓存命中
// (cachedSched 非 nil) 时与 HasMissedSchedule 返回完全一致的结果，且在
// 缓存未命中 (cachedSched nil) 时回落到 cronParser.Parse 路径。R241-PERF-3
// (#477)：保证替换 dashboard 1Hz handleList 的 HasMissedSchedule 调用为
// HasMissedScheduleCached 后行为不变。
func TestHasMissedScheduleCached_MatchesUncached(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-10 * time.Hour) // 绕过抑制窗口
	cases := []struct {
		name string
		job  *Job
	}{
		{"recent_run_no_miss", &Job{Schedule: "@every 30m", CreatedAt: now.Add(-24 * time.Hour), LastRunAt: now.Add(-20 * time.Minute)}},
		{"stale_run_miss", &Job{Schedule: "@every 30m", CreatedAt: now.Add(-24 * time.Hour), LastRunAt: now.Add(-3 * time.Hour)}},
		{"never_run_recent_create_no_miss", &Job{Schedule: "@every 30m", CreatedAt: now.Add(-5 * time.Minute)}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+"_cold_cache", func(t *testing.T) {
			t.Parallel()
			// cachedSched nil → falls back to parse, must equal HasMissedSchedule
			gotMissed, gotPrev := HasMissedScheduleCached(tc.job, now, startedAt)
			wantMissed, wantPrev := HasMissedSchedule(tc.job, now, startedAt)
			if gotMissed != wantMissed || !gotPrev.Equal(wantPrev) {
				t.Fatalf("cold cache divergence: got=(%v,%v) want=(%v,%v)", gotMissed, gotPrev, wantMissed, wantPrev)
			}
		})
		t.Run(tc.name+"_warm_cache", func(t *testing.T) {
			t.Parallel()
			// Pre-populate cachedSched manually (registerJob normally does this).
			sched, err := cronParser.Parse(tc.job.Schedule)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			warmJob := *tc.job
			warmJob.cachedSched = sched
			gotMissed, gotPrev := HasMissedScheduleCached(&warmJob, now, startedAt)
			wantMissed, wantPrev := HasMissedSchedule(tc.job, now, startedAt)
			if gotMissed != wantMissed || !gotPrev.Equal(wantPrev) {
				t.Fatalf("warm cache divergence: got=(%v,%v) want=(%v,%v)", gotMissed, gotPrev, wantMissed, wantPrev)
			}
		})
	}
}

// TestHasMissedScheduleCached_NilJob 验证 nil 输入不 panic（与
// HasMissedSchedule 同样的保守降级）。
func TestHasMissedScheduleCached_NilJob(t *testing.T) {
	t.Parallel()
	missed, prev := HasMissedScheduleCached(nil, time.Now(), time.Time{})
	if missed || !prev.IsZero() {
		t.Fatalf("nil job should return (false, zero); got (%v, %v)", missed, prev)
	}
}

// TestHasMissedScheduleCached_CachedPeriodMatchesRecomputed verifies that when
// cachedPeriod is set (>0), HasMissedScheduleCached produces the same result as
// the uncached path (cachedPeriod=0 → recomputes via schedulePeriodFromSched).
// This guards R20260603040203-PERF-9: the fast path must be behaviourally
// equivalent to the recompute path across all interesting inputs.
func TestHasMissedScheduleCached_CachedPeriodMatchesRecomputed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-10 * time.Hour)

	cases := []struct {
		name string
		job  *Job
	}{
		{
			"recent_run_no_miss",
			&Job{Schedule: "@every 30m", CreatedAt: now.Add(-24 * time.Hour), LastRunAt: now.Add(-20 * time.Minute)},
		},
		{
			"stale_run_miss",
			&Job{Schedule: "@every 30m", CreatedAt: now.Add(-24 * time.Hour), LastRunAt: now.Add(-3 * time.Hour)},
		},
		{
			"never_run_old_creation_miss",
			&Job{Schedule: "@every 30m", CreatedAt: now.Add(-5 * time.Hour)},
		},
		{
			"never_run_recent_creation_no_miss",
			&Job{Schedule: "@every 30m", CreatedAt: now.Add(-5 * time.Minute)},
		},
		{
			"daily_recent_run_no_miss",
			&Job{Schedule: "0 9 * * *", CreatedAt: now.Add(-30 * 24 * time.Hour), LastRunAt: time.Date(2026, 5, 9, 9, 0, 0, 0, time.UTC)},
		},
		{
			"daily_stale_run_miss",
			&Job{Schedule: "0 9 * * *", CreatedAt: now.Add(-30 * 24 * time.Hour), LastRunAt: now.Add(-3 * 24 * time.Hour)},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build a warm-cache job: set both cachedSched and cachedPeriod
			// as registerJob would, but without touching the scheduler.
			sched, err := cronParser.Parse(tc.job.Schedule)
			if err != nil {
				t.Fatalf("parse schedule: %v", err)
			}
			period := schedulePeriodFromSched(sched, now)
			if period <= 0 {
				t.Skip("schedule produces non-positive period, skipping")
			}

			warmJob := *tc.job
			warmJob.cachedSched = sched
			warmJob.cachedPeriod = period // fast path: skip schedulePeriodFromSched

			// The uncached path recomputes the period from scratch.
			wantMissed, wantPrev := HasMissedSchedule(tc.job, now, startedAt)

			// The cached path must return an identical result.
			gotMissed, gotPrev := HasMissedScheduleCached(&warmJob, now, startedAt)

			if gotMissed != wantMissed || !gotPrev.Equal(wantPrev) {
				t.Fatalf("cachedPeriod fast path diverges from recompute: got=(%v,%v) want=(%v,%v)",
					gotMissed, gotPrev, wantMissed, wantPrev)
			}
		})
	}
}
