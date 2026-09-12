package cron

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// blockingSendSession blocks inside Send until its ctx is done, then reports what
// the ctx carried. That is the seam sendctx_parent_test.go's comment said did not
// exist ("no ergonomic seam to observe sendCtx itself... *ManagedSession is
// concrete, not interface-mockable") — true when it was written, no longer true
// since cron took a Session INTERFACE whose Send receives the ctx directly.
type blockingSendSession struct {
	entered chan struct{}
	ctxErr  chan error
}

func (s *blockingSendSession) Send(ctx context.Context, _ string) (SendResult, error) {
	close(s.entered)
	<-ctx.Done()
	s.ctxErr <- ctx.Err()
	return SendResult{}, ctx.Err()
}
func (s *blockingSendSession) SessionID() string                     { return "sess-block" }
func (s *blockingSendSession) InterruptViaControl() InterruptOutcome { return InterruptSent }

type blockingSendRouter struct {
	reapRouter
	sess *blockingSendSession
}

func (r *blockingSendRouter) GetOrCreate(_ context.Context, _ string, _ AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionNew, nil
}

// TestSendCtxCancelsWhenStopCtxCancels replaces a textual assertion that
// scheduler_run.go contains the literal
// `sendCtx, sendCancel := context.WithTimeout(s.stopCtx, a.sendBudget)`.
//
// The invariant (#790 / #500, R238-GO-4): Scheduler.Stop() must short-circuit an
// in-flight Send instead of letting it run for up to the job timeout after Stop
// returns. What matters is that the ctx handed to Send is a descendant of
// s.stopCtx — which is observable now: the fake Session records ctx.Err() when
// its ctx fires, so cancelling stopCtx mid-Send proves the parenting.
//
// The textual version passes on any refactor that keeps that exact line and
// fails on a rename that changes nothing; this one is the other way round.
//
// Its companion TestSendCtxCancelsOnStopCtxCancel went too: it asserted Go's own
// guarantee that a WithTimeout child of a cancelled context is cancelled — a
// stdlib property, not a naozhi one, and its own comment called the failure it
// guarded "extremely unlikely". This test exercises the same propagation through
// the real scheduler, so the stdlib restatement adds nothing.
func TestSendCtxCancelsWhenStopCtxCancels(t *testing.T) {
	t.Parallel()

	sess := &blockingSendSession{entered: make(chan struct{}), ctxErr: make(chan error, 1)}
	router := &blockingSendRouter{sess: sess}
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})

	stopCtx, stopCancel := context.WithCancel(context.Background())
	s.stopCtx = stopCtx

	j := &Job{ID: "job-sendctx-stop", Schedule: "@every 5m", Prompt: "ping"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.executeOpt(j, true /* viaTriggerNow: skip jitter */); close(done) }()

	// Wait for Send to actually be in flight before cancelling, so the assertion's
	// premise (a Send is blocked on its ctx) is guaranteed rather than timed.
	select {
	case <-sess.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Send was never entered")
	}
	stopCancel()

	select {
	case err := <-sess.ctxErr:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Send's ctx ended with %v, want context.Canceled — sendCtx is not parented on s.stopCtx (#790 / #500)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send's ctx did not fire after stopCtx was cancelled; sendCtx is parented on context.Background (#790 / #500 regression)")
	}

	testhelper.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, "executeOpt did not return after stopCtx cancel")
}
