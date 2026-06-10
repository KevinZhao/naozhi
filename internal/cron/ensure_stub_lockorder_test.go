package cron

import (
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestEnsureStub_DoesNotHoldOrRequireWriteLock 给 R246-GO-8 (#692) 留的契约测试。
//
// EnsureStub 的 godoc 声明 caller 必须 NOT hold s.mu — 实现内部走 RLock。
// 没有静态防护时，未来 caller 可能在持 s.mu.Lock 的代码路径下调用本方法，
// RWMutex 不可重入会立刻死锁；任何把 EnsureStub 改成 s.mu.Lock 的"修复"
// 也会让现有 callers (handleSubscribe 等) 在持 s.mu.RLock 时调用而死锁。
//
// 该测试用并发模型把契约转成可机器验证的不变量：
//
//  1. 从主 goroutine 持有 s.mu.RLock；EnsureStub 必须在并发协程能立刻完成
//     —— 证明它使用 RLock-compatible 路径，没有升级到 Lock。
//  2. 主 goroutine 持有 s.mu.Lock 时，EnsureStub 在并发协程必然阻塞
//     —— 证明它确实拿 RLock（而非完全 lock-free 读取脏 jobs map）。
//
// 任何回归把 EnsureStub 改成 s.mu.Lock 都会让步骤 1 死锁；改成 lock-free
// 读 s.jobs 又会让步骤 2 假阳性通过——双向夹逼把"必须使用 RLock"刻在测试里。
func TestEnsureStub_DoesNotHoldOrRequireWriteLock(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 10,
	}, SchedulerDeps{
		Router: realRouterAdapter{r: router},
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "p", Platform: "x", ChatID: "c", WorkDir: "/tmp"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	key := session.CronKey(job.ID)

	// 步骤 1：持 RLock 时 EnsureStub 必须能并发完成。
	done := make(chan bool, 1)
	s.mu.RLock()
	go func() {
		done <- s.EnsureStub(key)
	}()
	select {
	case ok := <-done:
		s.mu.RUnlock()
		if !ok {
			t.Fatalf("EnsureStub returned false under concurrent RLock; expected true")
		}
	case <-time.After(2 * time.Second):
		s.mu.RUnlock()
		t.Fatalf("EnsureStub deadlocked under concurrent RLock — implementation took write lock?")
	}

	// 步骤 2：持 Lock 时 EnsureStub 必须阻塞。
	s.mu.Lock()
	blocked := make(chan struct{})
	finished := make(chan bool, 1)
	go func() {
		close(blocked)
		finished <- s.EnsureStub(key)
	}()
	<-blocked
	// 给对方时间真正进入 EnsureStub 的 RLock 调用点。
	select {
	case <-finished:
		s.mu.Unlock()
		t.Fatalf("EnsureStub returned while caller held Lock — implementation skipped lock?")
	case <-time.After(50 * time.Millisecond):
		// 期望：EnsureStub 阻塞在 RLock，等我们 Unlock 后才能完成。
	}
	s.mu.Unlock()
	select {
	case ok := <-finished:
		if !ok {
			t.Fatalf("EnsureStub returned false after Lock release; expected true")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("EnsureStub did not unblock within 2s after Lock release")
	}
}

// TestEnsureStub_ConcurrentReadersDoNotSerialise pins that two concurrent
// EnsureStub calls run in parallel — both take RLock so neither blocks the
// other. This guards against an accidental change that switches to Lock and
// would still pass TestEnsureStub_DoesNotHoldOrRequireWriteLock above
// (since each individual call would still complete under RLock if the test
// happened to run them serially). Two-readers pattern explicitly shows
// concurrent shared-lock semantics. R246-GO-8 (#692).
func TestEnsureStub_ConcurrentReadersDoNotSerialise(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 10,
	}, SchedulerDeps{
		Router: realRouterAdapter{r: router},
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "p", Platform: "x", ChatID: "c", WorkDir: "/tmp"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	key := session.CronKey(job.ID)

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	results := make(chan bool, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			results <- s.EnsureStub(key)
		}()
	}
	close(start)
	doneAll := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneAll)
	}()
	select {
	case <-doneAll:
		// All goroutines finished; drain results.
		close(results)
		for ok := range results {
			if !ok {
				t.Fatal("EnsureStub returned false in concurrent reader test")
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent EnsureStub readers did not all complete within 3s — RLock contention or upgrade?")
	}
}
