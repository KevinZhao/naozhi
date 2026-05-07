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
