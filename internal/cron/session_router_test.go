package cron

import (
	"context"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// fakeSessionRouter is a minimal SessionRouter that records calls so tests
// can assert which router APIs the scheduler reaches for without spinning
// up a real *session.Router. Any method the scheduler adds to SessionRouter
// must also be added here — which is the point: the compiler forces the
// contract to stay narrow.
//
// Post Phase B (docs/rfc/cron-sysession-merge.md §3.3) the SessionRouter
// interface speaks in cron-local types (cron.AgentOpts, cron.Session,
// cron.SessionStatus); the internal/wireup/cron_router_adapter.go bridges
// against *session.Router for the production wiring.
type fakeSessionRouter struct {
	mu            sync.Mutex
	registerCalls []stubCall
	resetCalls    []string
	getCreateKeys []string
}

type stubCall struct {
	key       string
	workspace string
	prompt    string
	chainIDs  []string
}

func (f *fakeSessionRouter) RegisterCronStubWithChain(key, workspace, prompt string, chainIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerCalls = append(f.registerCalls, stubCall{key, workspace, prompt, chainIDs})
}

func (f *fakeSessionRouter) Reset(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls = append(f.resetCalls, key)
}

func (f *fakeSessionRouter) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCreateKeys = append(f.getCreateKeys, key)
	// Returning (nil, zero, nil) lets execute() run far enough to exercise
	// the router integration point without invoking a real CLI. The
	// scheduler handles a nil session gracefully further down when
	// dereferencing; we only assert on the recorded call here.
	return nil, SessionExisting, nil
}

// TestSchedulerConfig_AcceptsSessionRouterInterface is the compile-time
// contract test: if a future refactor accidentally adds a new method to
// Scheduler's router calls without also widening SessionRouter, this test
// fails to build — fakeSessionRouter would no longer satisfy the interface.
// The test body itself is light because the value comes from the type
// signatures; we just make sure NewScheduler + AddJob accept our fake.
func TestSchedulerConfig_AcceptsSessionRouterInterface(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:  fake,
		MaxJobs: 10,
	})
	if s == nil {
		t.Fatal("NewScheduler returned nil with fake router")
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{
		Schedule: "@hourly",
		Prompt:   "hello from fake",
		Platform: "p",
		ChatID:   "c",
		WorkDir:  "/tmp",
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.registerCalls) != 1 {
		t.Fatalf("expected exactly 1 stub register call, got %d", len(fake.registerCalls))
	}
	got := fake.registerCalls[0]
	wantKey := sessionkey.CronKey(job.ID)
	if got.key != wantKey {
		t.Errorf("register key = %q, want %q", got.key, wantKey)
	}
	if got.workspace != "/tmp" {
		t.Errorf("register workspace = %q, want %q", got.workspace, "/tmp")
	}
	if got.prompt != "hello from fake" {
		t.Errorf("register prompt = %q, want %q", got.prompt, "hello from fake")
	}
	// 新加的 job 还没执行过，不应该带 chainIDs；将来 chainFromLastSessionID
	// 改了逻辑这里会立刻报错，把契约固化下来。
	if got.chainIDs != nil {
		t.Errorf("register chainIDs = %v, want nil for job without LastSessionID", got.chainIDs)
	}
}

// TestSchedulerConfig_ResetCalledOnDelete pins that DeleteJobByID routes
// through SessionRouter.Reset — the cron package must not grow a direct
// *session.Router dependency for this code path.
func TestSchedulerConfig_ResetCalledOnDelete(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:  fake,
		MaxJobs: 10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "x", Platform: "p", ChatID: "c"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := s.DeleteJobByID(job.ID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	wantKey := sessionkey.CronKey(job.ID)
	found := false
	for _, k := range fake.resetCalls {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected Reset(%q) in %v", wantKey, fake.resetCalls)
	}
}

// The legacy "*session.Router satisfies cron.SessionRouter" compile-time
// assertion was removed in Phase B: the interface now speaks in
// cron-local types and the production glue lives in
// internal/wireup/cron_router_adapter.go (with its own round-trip test).
