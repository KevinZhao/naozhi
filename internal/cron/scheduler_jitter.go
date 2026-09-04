package cron

import (
	"context"
	mrand "math/rand/v2"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// applyJitter 在执行 cron job 前引入一段随机延迟，把"整点共振起跑"的 CPU / API
// 峰值打散。窗口上界 = min(jitterMax, period/4)：5m 周期最多抖 75s，1h+ 抖满
// jitterMax。无法解析 schedule 或 period<=0 时用 jitterMax 兜底。抖动尊重
// ctx：Stop() / 关机时 stopCtx 取消 → 立即返回（不再执行 job）。
//
// 用 math/rand/v2（per-goroutine 安全且无全局锁）；随机只影响启动时刻分布，
// 非密码学用途。NewTimer/defer Stop 每 tick 分配一个 *time.Timer，当前规模
// 成本可忽略；time.After 不能 Stop，ctx 取消时会泄漏到触发点，不适合此处。
func applyJitter(ctx context.Context, schedule string, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	// String-keyed entry point re-parses on every call; production prefers
	// applyJitterSched, this remains for tests and the entryID==0 fallback.
	// Keep the pipeline identical to applyJitterSched (#1147).
	period := schedulePeriod(schedule, time.Now())
	jitterSleep(ctx, period, jitterMax)
}

// applyJitterSched is the cron tick hot-path entry point. It reuses the
// robfigcron.Schedule the cron engine already parsed, avoiding a redundant
// cronParser.Parse per tick; otherwise identical to applyJitter (#1147).
func applyJitterSched(ctx context.Context, sched robfigcron.Schedule, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	var period time.Duration
	if sched != nil {
		period = schedulePeriodFromSched(sched, time.Now())
	}
	jitterSleep(ctx, period, jitterMax)
}

// jitterSleep is the shared tail of applyJitter / applyJitterSched: clamp
// jitterMax by period/4 (period<=0 = use jitterMax as-is), roll a random
// duration in [0, window), and sleep on a Timer that respects ctx.
func jitterSleep(ctx context.Context, period, jitterMax time.Duration) {
	window := jitterMax
	if period > 0 {
		if quarter := period / 4; quarter < window {
			window = quarter
		}
	}
	if window <= 0 {
		return
	}
	// mrand.Int64N panics on n <= 0; a buggy custom Schedule with
	// non-monotonic Next could clamp period to a non-positive int64, so guard
	// rather than fall into robfig/cron's recover path.
	n := int64(window)
	if n <= 0 {
		return
	}
	d := time.Duration(mrand.Int64N(n))
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
