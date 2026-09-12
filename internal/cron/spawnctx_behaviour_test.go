package cron

import (
	"context"
	"errors"
	"testing"
	"time"
)

// spawnCtxObserverRouter captures the ctx executeOpt hands to GetOrCreate (the
// spawn ctx) and, when the resulting session's Send runs, records whether that
// spawn ctx had already been cancelled. Both facts are the seam the source
// anchors said did not exist ("no ergonomic runtime seam to observe the spawn ctx
// — it's a local and *ManagedSession is concrete, not mockable"), which stopped
// being true once cron took a Session interface: GetOrCreate receives the spawn
// ctx and Send is called afterwards on the returned Session.
type spawnCtxObserverRouter struct {
	reapRouter
	blockUntilCancel bool

	gotSpawnCtx     context.Context
	entered         chan struct{}
	spawnDoneAtSend chan bool
}

func (r *spawnCtxObserverRouter) GetOrCreate(ctx context.Context, _ string, _ AgentOpts) (Session, SessionStatus, error) {
	r.gotSpawnCtx = ctx
	if r.blockUntilCancel {
		close(r.entered)
		<-ctx.Done()
		return nil, SessionExisting, ctx.Err()
	}
	return &spawnObservingSession{r: r}, SessionNew, nil
}

type spawnObservingSession struct{ r *spawnCtxObserverRouter }

func (s *spawnObservingSession) Send(_ context.Context, _ string) (SendResult, error) {
	done := false
	select {
	case <-s.r.gotSpawnCtx.Done():
		done = true
	default:
	}
	s.r.spawnDoneAtSend <- done
	return SendResult{Text: "ok", SessionID: "sess-spawn-1"}, nil
}
func (s *spawnObservingSession) SessionID() string                     { return "sess-spawn-1" }
func (s *spawnObservingSession) InterruptViaControl() InterruptOutcome { return InterruptSent }

// TestSpawnCtxCancelsWhenStopCtxCancels replaces a textual assertion that
// scheduler_run.go contains `ctx, spawnCancel := context.WithTimeout(s.stopCtx,
// jobTimeout)`. The invariant (R242-PERF-14 / #680): a Background parent leaks
// the spawn timer past Stop() and reintroduces the use-after-free class race
// fixed in #1078. Observed rather than grepped: GetOrCreate blocks on the ctx it
// was given, stopCtx is cancelled, and the ctx must fire.
func TestSpawnCtxCancelsWhenStopCtxCancels(t *testing.T) {
	t.Parallel()

	router := &spawnCtxObserverRouter{blockUntilCancel: true, entered: make(chan struct{})}
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})
	stopCtx, stopCancel := context.WithCancel(context.Background())
	s.stopCtx = stopCtx

	j := &Job{ID: "job-spawnctx-stop", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.executeOpt(j, true); close(done) }()

	select {
	case <-router.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("GetOrCreate was never entered")
	}
	stopCancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeOpt did not return after stopCtx cancel; the spawn ctx is parented on context.Background (R242-PERF-14 / #680)")
	}
	if err := router.gotSpawnCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("spawn ctx ended with %v, want context.Canceled", err)
	}
}

// TestSpawnCtxCancelledBeforeSend replaces the textual `defer spawnCancel()` /
// `spawnCancel: spawnCancel,` anchors. The invariant (#1078 / #423): the spawn
// ctx is cancelled EAGERLY when GetOrCreate returns, not left to the deferred
// safety net at function exit — otherwise its timer lives for the whole send.
// Observable directly: by the time Send runs, the ctx GetOrCreate was given must
// already be done.
func TestSpawnCtxCancelledBeforeSend(t *testing.T) {
	t.Parallel()

	router := &spawnCtxObserverRouter{spawnDoneAtSend: make(chan bool, 1)}
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	j := &Job{ID: "job-spawncancel-eager", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true)

	select {
	case doneAtSend := <-router.spawnDoneAtSend:
		if !doneAtSend {
			t.Error("the spawn ctx was still live when Send ran; spawnCancel must fire eagerly at GetOrCreate exit, not only via the deferred safety net (#1078 / #423)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send was never called")
	}
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	if got := rec.endedAtCron(0); got.State != RunStateSucceeded {
		t.Fatalf("state = %q, want succeeded", got.State)
	}
}
