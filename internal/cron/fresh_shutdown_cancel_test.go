package cron

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// stopDuringSendSession cancels the scheduler's stopCtx while its Send is in
// flight, as Scheduler.Stop does on a restart, and returns the ctx error the
// way Process.Send does: the CLI is left running behind its shim.
type stopDuringSendSession struct{ s *Scheduler }

func (x stopDuringSendSession) Send(ctx context.Context, _ string) (SendResult, error) {
	x.s.stopCancel()
	<-ctx.Done()
	return SendResult{}, ctx.Err()
}
func (stopDuringSendSession) SessionID() string                     { return "" }
func (stopDuringSendSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

// TestFreshSendCancelledByShutdownKeepsSession: a fresh-context run whose
// Send is cut by shutdown keeps its in-flight marker for the next process to
// adopt (#2712), so it must not Reset the session; Reset would Close the shim
// and kill the very turn the marker points at. Only the preflight Reset at
// run start may reach the router.
func TestFreshSendCancelledByShutdownKeepsSession(t *testing.T) {
	t.Parallel()
	router := &reapRouter{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router})
	router.sess = stopDuringSendSession{s: s}
	j := &Job{ID: "job-shutdown", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	s.putJobForTest(j)

	s.executeOpt(j.ID, true /* viaTriggerNow: skip jitter */)

	resets, _ := router.snapshot()
	n := 0
	for _, k := range resets {
		if k == sessionkey.CronKey(j.ID) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Reset(%s) called %d times, want 1 (the preflight only); a shutdown cancel must leave the shim-held CLI for adoption", sessionkey.CronKey(j.ID), n)
	}
}
