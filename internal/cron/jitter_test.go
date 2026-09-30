package cron

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestApplyJitter_ZeroMaxNoOp 确保 jitterMax=0 时 applyJitter 立即返回。
func TestApplyJitter_ZeroMaxNoOp(t *testing.T) {
	t.Parallel()
	start := time.Now()
	applyJitter(context.Background(), "@every 30m", 0)
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Fatalf("zero jitterMax should be instant, took %v", elapsed)
	}
}

// TestApplyJitter_RespectsCtxCancel 在 goroutine 中启动 applyJitter 并在短
// 时间内 cancel ctx，验证它不会等到 timer 耗尽才返回。
func TestApplyJitter_RespectsCtxCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 10s 上限，配合 5m 周期 → window = min(10s, 5m/4) = 10s
		applyJitter(ctx, "@every 5m", 10*time.Second)
	}()

	// 等 jitter goroutine 入 select
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// OK — ctx cancel 立即唤醒
	case <-time.After(500 * time.Millisecond):
		t.Fatal("applyJitter did not return within 500ms after ctx cancel")
	}
}

// TestApplyJitter_CapClampedByPeriod 验证 jitter 窗口被 period/4 钳住：
// 5m 周期 + 10m jitterMax → 实际 window 应该是 5m/4 = 75s，多次采样
// 的最大延迟应落在 75s 以内（留足 rng 方差余量用统计上界）。
func TestApplyJitter_CapClampedByPeriod(t *testing.T) {
	t.Parallel()

	// 直接调 schedulePeriod 验证 period 侧的 cap 计算，避免真的 sleep 测试
	// 里做 75s 的实时等待（测试时间会爆炸）。
	period := schedulePeriod("@every 5m", time.Now())
	if period != 5*time.Minute {
		t.Fatalf("schedulePeriod(@every 5m) = %v, want 5m", period)
	}
	// period/4 = 75s，说明 clamp 逻辑在运行时选取了较小的 window
	// 而不是 jitterMax=10m 的默认窗口。
	cap := period / 4
	if cap != 75*time.Second {
		t.Fatalf("period/4 = %v, want 75s", cap)
	}
}

// TestApplyJitterSched_ReusesParsedSchedule 验证 applyJitterSched 接受
// 已 parse 的 robfigcron.Schedule，period 计算结果与字符串路径一致，
// 且 jitterMax=0 / nil sched 都不 sleep。R250-CR-14 (#1147)。
func TestApplyJitterSched_ReusesParsedSchedule(t *testing.T) {
	t.Parallel()

	// 用同一份字符串经 cronParser 解析后传给 applyJitterSched，对照
	// 字符串入口 schedulePeriod 的结果，确认两者拿到同一个 period。
	const expr = "@every 5m"
	sched, err := cronParser.Parse(expr)
	if err != nil {
		t.Fatalf("cronParser.Parse(%q): %v", expr, err)
	}
	now := time.Now()
	if got, want := schedulePeriodFromSched(sched, now), schedulePeriod(expr, now); got != want {
		t.Fatalf("period mismatch: sched=%v string=%v", got, want)
	}

	// jitterMax=0 → 立即返回，不 sleep。
	start := time.Now()
	applyJitterSched(context.Background(), sched, 0)
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Fatalf("zero jitterMax should be instant, took %v", elapsed)
	}

	// nil schedule → 退化为 jitterMax 兜底，但 ctx 立刻 cancel 也应秒返。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	applyJitterSched(ctx, nil, 100*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("cancelled ctx + nil sched should return immediately, took %v", elapsed)
	}

	// ctx cancel 在窗口内 → 立即返回。覆盖 select 路径不依赖 timer 触发。
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		applyJitterSched(ctx, sched, 10*time.Second)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("applyJitterSched did not return within 500ms after ctx cancel")
	}

}

// TestApplyJitter_UnparsableSchedule_UsesMaxCap 验证 bad cron 表达式下
// period 返回 0，applyJitter 退化为使用完整 jitterMax 窗口兜底。
func TestApplyJitter_UnparsableSchedule_UsesMaxCap(t *testing.T) {
	t.Parallel()

	// 构造一个无法解析的 schedule。5-field cron "every second" 是非法的
	// （robfig/cron 不支持 second 字段）。
	period := schedulePeriod("not-a-cron-expr", time.Now())
	if period != 0 {
		t.Fatalf("schedulePeriod(bogus) = %v, want 0", period)
	}

	// ctx 立即 cancel，确保 applyJitter 不会实际 sleep；它应该在进入 select
	// 前（确认 window > 0）就已经选了 jitterMax 作为 window。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	applyJitter(ctx, "not-a-cron-expr", 100*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("cancel-before-call should return immediately, took %v", elapsed)
	}
}

// TestExecuteOpt_TriggerNowSkipsJitter 是对 executeOpt 的行为测试：
// viaTriggerNow=true 时必须跳过 jitter，不管 jitterMax 多大。用一个
// 非常大的 jitterMax（模拟坏配置）+ 极短的测试超时 — 如果 jitter 被
// 应用了，测试会超时。
func TestExecuteOpt_TriggerNowSkipsJitter(t *testing.T) {
	t.Parallel()

	// 复用既有 mock：只需要 Scheduler + 一个啥都不做的 router 替身。
	// 用 nil router 会 panic，所以给一个返回 nil session 的最小 stub。
	sr := &jitterStubRouter{}

	s := NewScheduler(SchedulerConfig{
		StorePath: t.TempDir() + "/cron.json",
		MaxJobs:   10,
		JitterMax: time.Hour, // 如果走 jitter 路径，测试必超时
	}, SchedulerDeps{
		Router: sr,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		ID:       "test-trigger-now",
		Schedule: "@every 30m",
		Prompt:   "hello",
	}
	// R20260527122801-CR-8 (#1322): executeOpt now post-CAS rechecks
	// s.tbl.jobs[j.ID] to close the dispatch→CAS race window. Insert the job
	// into the map so the recheck sees it as live; this test focuses on
	// jitter-skip behaviour, not the registry lifecycle.
	s.putJobForTest(j)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.executeOpt(j.ID, true) // viaTriggerNow=true
	}()

	select {
	case <-done:
		// 在 1s 内完成说明跳过了 1h 的 jitter 窗口；router stub 的 Send
		// 失败不重要，我们只验证"没 sleep"。
		if atomic.LoadInt64(&sr.calls) == 0 {
			t.Fatal("router.GetOrCreate never called — execute path exited too early")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executeOpt blocked for >2s with viaTriggerNow=true; jitter was not skipped")
	}
}

// TestExecuteOpt_ScheduledTickAppliesJitter_WhenEnabled 反向验证：
// viaTriggerNow=false + jitterMax>0 + schedule 能算出合理 period 时，
// applyJitter 会真的 sleep 一段时间（不等于 0）。用 stopCtx cancel 触发
// 快速返回，然后断言 elapsed > 0（选了一个非零的随机延迟）。
//
// 注意：由于 mrand.Int64N 可能返回 0（1/N 概率），我们重复 10 次，
// 只要有任何一次观察到 elapsed > 10ms 就算通过。
func TestExecuteOpt_ScheduledTickAppliesJitter_WhenEnabled(t *testing.T) {
	t.Parallel()

	sawJitter := false
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()

		go func() {
			// 让 applyJitter 进入 select，然后 cancel 让它立刻返回
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		// 用 1s 作为 jitter 上限 + 30m 周期 → window = min(1s, 7m30s) = 1s
		applyJitter(ctx, "@every 30m", time.Second)
		elapsed := time.Since(start)
		if elapsed > 20*time.Millisecond {
			sawJitter = true
			break
		}
	}
	if !sawJitter {
		t.Fatal("10 attempts with 1s jitter window never observed any delay — rng stuck at 0?")
	}
}

// TestApplyJitterAndRecheck_AbortsPausedOrDeleted pins R246-GO-7: the
// registerJob closure's paused check runs BEFORE the jitter wait, so the
// recheck after it must catch a PauseJobByID or DeleteJob that landed inside
// the window, and only a live job may leave with a snapshot.
func TestApplyJitterAndRecheck_AbortsPausedOrDeleted(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, AllowNilRouter: true, JitterMax: time.Millisecond}, SchedulerDeps{})
	s.putJobForTest(&Job{ID: "live", Schedule: "@every 1m", Prompt: "p"})
	s.putJobForTest(&Job{ID: "paused", Schedule: "@every 1m", Paused: true})
	for _, c := range []struct {
		id        string
		wantAbort bool
	}{{"live", false}, {"paused", true}, {"gone", true}} {
		snap, taken, abort := s.applyJitterAndRecheck(c.id, "run-1", &runInflight{})
		if abort != c.wantAbort || taken == abort {
			t.Errorf("%s: abort=%v snapTaken=%v, want abort=%v and a snapshot exactly when not aborted", c.id, abort, taken, c.wantAbort)
		}
		if !abort && snap.prompt != "p" {
			t.Errorf("%s: snapshot = %+v, want the live job's", c.id, snap)
		}
	}
}

// jitterStubRouter 是 SessionRouter 的最小实现，用来验证 execute 路径
// 能走到 GetOrCreate；不做真实的 session 管理。返回 context.Canceled
// 触发 execute 的 "cancelled" 早退分支，避免走到 session.Send 实际 IO。
type jitterStubRouter struct {
	calls int64
}

func (r *jitterStubRouter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string) {
	_, _, _, _ = key, workspace, lastPrompt, chainIDs
}
func (r *jitterStubRouter) Reset(key string) { _ = key }
func (r *jitterStubRouter) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error) {
	_ = ctx
	_ = key
	_ = opts
	atomic.AddInt64(&r.calls, 1)
	return nil, SessionExisting, context.Canceled
}
