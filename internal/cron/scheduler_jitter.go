package cron

import (
	"context"
	mrand "math/rand/v2"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// applyJitter 在执行 cron job 前引入一段随机延迟，用来把"整点共振起跑"的
// CPU / API 峰值打散。窗口上界 = min(jitterMax, period/4)：
//   - 5m 周期 → 最多抖 75s（不蚕食 1m 节奏）
//   - 30m 周期 → 最多抖 7m30s
//   - 1h+ 周期 → 抖满 jitterMax（默认 2m）
//
// 无法解析 schedule 或 period<=0 时用 jitterMax 兜底。抖动尊重 ctx：
// Stop() / 进程关机期间 stopCtx 取消 → 立即返回（不再执行 job）。
//
// 用 math/rand/v2（per-goroutine 安全且无全局锁），安全性不敏感：
// 这里的随机只影响启动时刻分布，不是密码学用途。
//
// R246-GO-22: NewTimer/defer Stop 在每次 tick 都分配 *time.Timer，
// 当前规模（~100 timer/min @ 100 jobs * 1Hz）成本可忽略，无需优化。
// 未来若 job 数突破 ~5000/min（≈ 80 alloc/s）再考虑 sync.Pool[*time.Timer]
// 或退化到 runtime.timeSleep 直接路径；提前优化只会让控制流更晦涩。
// time.After(d) 同样会 alloc *Timer 但不能被 Stop()，ctx 取消时会泄漏到
// 触发点为止，不适合此处。
func applyJitter(ctx context.Context, schedule string, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	// R250-CR-14 (#1147): the string-keyed entry point re-parses on every
	// call. Production now prefers applyJitterSched with the pre-parsed
	// robfigcron.Schedule pulled from s.cron.Entry, but this signature is
	// retained for tests and for the fallback path when entryID is 0 /
	// concurrently removed. Keep the parse → period → sleep pipeline
	// behaviourally identical to applyJitterSched so the two paths cannot
	// diverge.
	period := schedulePeriod(schedule, time.Now())
	jitterSleep(ctx, period, jitterMax)
}

// applyJitterSched is the entry point for the cron tick hot path. It reuses
// the already-parsed robfigcron.Schedule that the cron engine holds inside
// each Entry, avoiding a redundant cronParser.Parse on every tick. Behaviour
// is otherwise identical to applyJitter — same window cap (period/4), same
// jitterMax fallback, same ctx.Done() short-circuit. R250-CR-14 / #1147.
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
// jitterMax by period/4 (with period<=0 meaning "use jitterMax as-is"),
// roll a random duration in [0, window), and sleep on a Timer that respects
// ctx cancellation. Extracted so the parse-once vs reuse-parsed split lives
// only in the two thin entry points above. R250-CR-14 / #1147.
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
	// R20260527122801-GO-018 defensive: int64(window) underflow guard
	// so future Schedule providers returning non-monotonic Next don't
	// panic Int64N. window is already a time.Duration (int64) and the
	// `window <= 0` check above covers the normal range, but a hostile
	// or buggy custom Schedule could conceivably produce a period that
	// arithmetic clamps to a non-positive int64; mrand.Int64N panics
	// on n <= 0, so a single extra branch keeps the tick goroutine
	// from going down to robfig/cron's recover path.
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
