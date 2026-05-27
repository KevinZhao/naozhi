package cron

import (
	"testing"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// TestRegisterJob_PopulatesCachedPeriod 验证 registerJob 把 schedule 周期
// 缓存到 j.cachedPeriod，避免 hot jitter 路径每 tick 重做 sched.Next。
// R242-PERF-2 / #664: 修复前 applyJitterSched 每次 tick 调
// schedulePeriodFromSched(sched, time.Now()) 跑 2× sched.Next；现在
// registerJob 在注册 entry 时一次性算好周期，cachedPeriod>0 时 hot path
// 直接 jitterSleep 跳过解析。
func TestRegisterJob_PopulatesCachedPeriod(t *testing.T) {
	t.Parallel()

	s := &Scheduler{
		jobs: make(map[string]*Job),
		cron: robfigcron.New(robfigcron.WithParser(cronParser)),
	}

	j := &Job{
		ID:       "0123456789abcdef",
		Schedule: "@every 30m",
	}

	if err := s.registerJob(j); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	t.Cleanup(func() { s.cron.Remove(j.entryID) })

	if j.entryID == 0 {
		t.Fatalf("expected non-zero entryID after registerJob")
	}
	if j.cachedPeriod <= 0 {
		t.Fatalf("expected cachedPeriod > 0 after registerJob, got %v", j.cachedPeriod)
	}
	// @every 30m 应该缓存为 30 分钟（允许小误差因为 schedulePeriodFromSched
	// 用 sched.Next 推断；连续两次 Next 之间稳定为 30m）。
	if want := 30 * time.Minute; j.cachedPeriod != want {
		t.Fatalf("cachedPeriod = %v, want %v", j.cachedPeriod, want)
	}
}

// TestRegisterJob_CachedPeriodConsistentWithSched 校验 cachedPeriod 与
// 实时算的 schedulePeriodFromSched 一致——避免缓存值与 hot path 兜底
// 分支（cachedPeriod==0 走 applyJitterSched）行为分裂。
func TestRegisterJob_CachedPeriodConsistentWithSched(t *testing.T) {
	t.Parallel()

	s := &Scheduler{
		jobs: make(map[string]*Job),
		cron: robfigcron.New(robfigcron.WithParser(cronParser)),
	}

	cases := []string{
		"@every 5m",
		"*/10 * * * *",
		"0 9 * * *",
	}
	for _, expr := range cases {
		j := &Job{ID: "fedcba9876543210", Schedule: expr}
		if err := s.registerJob(j); err != nil {
			t.Fatalf("%s: registerJob: %v", expr, err)
		}
		sched := s.cron.Entry(j.entryID).Schedule
		want := schedulePeriodFromSched(sched, time.Now())
		if j.cachedPeriod != want {
			t.Errorf("%s: cachedPeriod=%v want=%v", expr, j.cachedPeriod, want)
		}
		s.cron.Remove(j.entryID)
		j.entryID = 0
	}
}
