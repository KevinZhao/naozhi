package cron

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// panickingRouter is a SessionRouter whose GetOrCreate panics on first
// call. Used to drive R238-GO-9 (#801): TriggerNow's goroutine must
// recover from executeOpt panics so a single job's failure does not
// crash the calling goroutine (and, transitively, leak the triggerWG
// counter that triggerWG.Done in the caller would otherwise stall).
type panickingRouter struct {
	mu    sync.Mutex
	calls int
}

func (p *panickingRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (p *panickingRouter) Reset(string)                                               {}
func (p *panickingRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	panic(errors.New("simulated GetOrCreate panic"))
}

// TestR238GO9_TriggerNowRecoversExecuteOptPanic pins #801: a panic in
// executeOpt (here forced via a panicking router.GetOrCreate stub) must
// not propagate up to the TriggerNow goroutine. Pre-fix: the panic
// killed the goroutine and the deferred triggerWG.Done in the
// surrounding closure was the only thing keeping Stop() from wedging
// (which itself depends on every inflight defer firing — the recover
// makes the failure loud and bounded instead of relying on defer
// ordering luck). Post-fix: the recover swallows the panic, slog.Error
// records it, and the goroutine returns normally so the scheduler stays
// healthy for subsequent triggers.
func TestR238GO9_TriggerNowRecoversExecuteOptPanic(t *testing.T) {
	t.Parallel()
	r := &panickingRouter{}
	s := NewScheduler(SchedulerConfig{Router: r, MaxJobs: 5})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{
		Schedule: "@hourly",
		Prompt:   "panic-bait",
		Platform: "p",
		ChatID:   "c",
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Drive executeIfNotDeletedOrPaused directly (the TriggerNow goroutine
	// body in scheduler_jobs.go calls into this). Pre-fix this would
	// re-raise the panic into the test goroutine and the t.Fatalf would
	// be unreachable. Post-fix the recover swallows it and we return
	// cleanly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The goroutine running executeIfNotDeletedOrPaused must NOT
		// surface the panic. We add our own recover only as a belt-and-
		// suspenders for the regression case so the test reports "fail"
		// instead of "PANIC: ..." which is harder to triage.
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("TriggerNow goroutine panicked instead of recovering: %v", r)
			}
		}()
		s.executeIfNotDeletedOrPaused(job.ID)
	}()

	select {
	case <-done:
		// pass
	case <-time.After(5 * time.Second):
		t.Fatal("TriggerNow goroutine did not return within 5s; recover must let executeIfNotDeletedOrPaused complete")
	}

	// Confirm the panicking router actually panicked at least once —
	// guards against the test silently passing because the panic path
	// never executed (e.g. if a future refactor short-circuits before
	// GetOrCreate).
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == 0 {
		t.Error("panickingRouter.GetOrCreate was not called; test cannot assert recover semantics without driving the panic path")
	}
}

// TestR238GO9_TriggerNowPanicValueRecorded covers the Error-log path:
// recordTriggerNowPanic must accept arbitrary panic values (string,
// error, nil-with-panic-is-impossible) without re-panicking on the
// formatted slog.Error call. We invoke it directly to exercise the
// formatter without spinning up a full scheduler; the slog destination
// is the default writer.
func TestR238GO9_TriggerNowPanicValueRecorded(t *testing.T) {
	t.Parallel()
	// Three panic-value shapes: string (most common from `panic("…")`),
	// error (from `panic(errors.New(…))`), and a custom struct (from
	// runtime errors like nil-deref).
	cases := []struct {
		name string
		val  any
	}{
		{"string", "boom"},
		{"error", errors.New("boom err")},
		{"struct", struct{ X int }{X: 42}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("recordTriggerNowPanic re-panicked on %v: %v", tc.val, r)
				}
			}()
			recordTriggerNowPanic("test-job-id", tc.val)
		})
	}
}
