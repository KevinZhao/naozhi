package cron

import (
	"errors"
	"expvar"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// runCounterNames enumerates every per-run lifecycle counter in the order the
// delta vectors below are laid out. Kept as a slice (not a map) so a failure
// message lists counters deterministically.
var runCounterNames = []string{
	"Started", "Ended",
	"Succeeded", "Failed", "TimedOut", "Canceled", "Skipped",
	"SandboxFailed", "SandboxTimedOut",
}

func runCounters() []*expvar.Int {
	return []*expvar.Int{
		metrics.CronRunStartedTotal, metrics.CronRunEndedTotal,
		metrics.CronRunSucceededTotal, metrics.CronRunFailedTotal, metrics.CronRunTimedOutTotal,
		metrics.CronRunCanceledTotal, metrics.CronRunSkippedTotal,
		metrics.CronSandboxRunFailedTotal, metrics.CronSandboxRunTimedOutTotal,
	}
}

// counterDeltas describes the expected +N per counter for one terminal path.
// Zero-valued fields mean "must not move".
type counterDeltas struct {
	Started, Ended                                 int64
	Succeeded, Failed, TimedOut, Canceled, Skipped int64
	SandboxFailed, SandboxTimedOut                 int64
}

func (d counterDeltas) vector() []int64 {
	return []int64{d.Started, d.Ended, d.Succeeded, d.Failed, d.TimedOut, d.Canceled, d.Skipped, d.SandboxFailed, d.SandboxTimedOut}
}

func snapshotCounters() []int64 {
	cs := runCounters()
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.Value()
	}
	return out
}

// waitCounterDeltas polls until CronRunEndedTotal has moved by want.Ended
// (it is the LAST bump in finishRun, after emitRunEnded, so a broadcast-based
// wait alone would race it) and then returns the full delta vector.
func waitCounterDeltas(t *testing.T, before []int64, wantEnded int64) []int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if metrics.CronRunEndedTotal.Value()-before[1] >= wantEnded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CronRunEndedTotal never advanced by %d", wantEnded)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Small settle so any bump ordered AFTER Ended on a path (there are none
	// today; the assertion below would catch one) is observed, not missed.
	time.Sleep(5 * time.Millisecond)
	after := snapshotCounters()
	deltas := make([]int64, len(after))
	for i := range after {
		deltas[i] = after[i] - before[i]
	}
	return deltas
}

func assertCounterDeltas(t *testing.T, got []int64, want counterDeltas) {
	t.Helper()
	wv := want.vector()
	for i, name := range runCounterNames {
		if got[i] != wv[i] {
			t.Errorf("%s delta = %d, want %d (full: got %v want %v)", name, got[i], wv[i], got, wv)
		}
	}
}

// sandboxDirectArgs builds the sandboxExecArgs used by the direct
// finishSandboxRun rows (the paths executeSandbox reaches only via ctx
// deadline / shutdown cancel, which are not deterministic to provoke through
// a fake runner).
func sandboxDirectArgs(j *Job, runID string) sandboxExecArgs {
	return sandboxExecArgs{
		job:       j,
		snap:      jobSnapshot{jobID: j.ID, prompt: j.Prompt, label: jobTitleOrFallback(j)},
		runID:     runID,
		startedAt: time.Now().Add(-time.Minute),
		trigger:   TriggerScheduled,
		finalizer: &runFinalizer{},
		lg:        slog.Default(),
	}
}

// TestSandboxTerminalPaths_CounterDeltas is the #2173 acceptance matrix:
// every terminal path × every lifecycle counter, each advancing exactly once
// where expected and not at all elsewhere. Rows:
//
//   - executeSandbox end-to-end (success / failed-clean / transport-unconfirmed
//     / preflight error) — Started via emitRunStarted, everything else via
//     finishRun → bumpRunStateMetrics(sandbox=true).
//   - finishSandboxRun direct (timed-out / shutdown-cancel) — the two states
//     executeSandbox only reaches through ctx.Err().
//   - finishRun direct with sandbox=false — a LOCAL failure / timeout must not
//     touch the sandbox buckets.
//   - reconcileOneSandboxOrphan (live job / job gone / job deleted in gap) —
//     the three orphan branches must move the same counters as a live sandbox
//     transport failure, with broadcasts only when the job exists.
func TestSandboxTerminalPaths_CounterDeltas(t *testing.T) {
	sandboxFailed := counterDeltas{Started: 1, Ended: 1, Failed: 1, SandboxFailed: 1}
	orphanClosed := sandboxFailed // orphans are Failed/sandbox_transport by construction

	cases := []struct {
		name         string
		runner       *fakeSandboxRunner
		withJob      bool
		run          func(t *testing.T, s *Scheduler, j *Job, storePath string)
		want         counterDeltas
		wantStarted  int // broadcast frames
		wantEnded    int
		wantStopCall int
	}{
		{
			name:        "exec success",
			runner:      &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"}},
			withJob:     true,
			run:         func(_ *testing.T, s *Scheduler, j *Job, _ string) { s.executeOpt(j, true) },
			want:        counterDeltas{Started: 1, Ended: 1, Succeeded: 1},
			wantStarted: 1, wantEnded: 1,
		},
		{
			name:        "exec failed-clean",
			runner:      &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateFailedClean, ErrMsg: "exit 1"}},
			withJob:     true,
			run:         func(_ *testing.T, s *Scheduler, j *Job, _ string) { s.executeOpt(j, true) },
			want:        sandboxFailed,
			wantStarted: 1, wantEnded: 1,
		},
		{
			name:        "exec failed-transport stop unconfirmed",
			runner:      &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "stream lost"}},
			withJob:     true,
			run:         func(_ *testing.T, s *Scheduler, j *Job, _ string) { s.executeOpt(j, true) },
			want:        sandboxFailed,
			wantStarted: 1, wantEnded: 1,
		},
		{
			name:        "exec preflight error",
			runner:      &fakeSandboxRunner{err: errors.New("empty prompt")},
			withJob:     true,
			run:         func(_ *testing.T, s *Scheduler, j *Job, _ string) { s.executeOpt(j, true) },
			want:        sandboxFailed,
			wantStarted: 1, wantEnded: 1,
		},
		{
			name:    "finish timed-out (sandbox)",
			runner:  &fakeSandboxRunner{},
			withJob: true,
			run: func(_ *testing.T, s *Scheduler, j *Job, _ string) {
				s.finishSandboxRun(sandboxDirectArgs(j, "deadbeef00000101"), RunStateTimedOut, ErrClassSandboxTransport, "", "deadline", nil)
			},
			want:      counterDeltas{Ended: 1, TimedOut: 1, SandboxTimedOut: 1},
			wantEnded: 1,
		},
		{
			name:    "finish canceled skipPersist (sandbox)",
			runner:  &fakeSandboxRunner{},
			withJob: true,
			run: func(_ *testing.T, s *Scheduler, j *Job, _ string) {
				s.finishSandboxRunSkipPersist(sandboxDirectArgs(j, "deadbeef00000102"), RunStateCanceled, ErrClassCanceled, "", "shutdown", nil)
			},
			want:      counterDeltas{Ended: 1, Canceled: 1},
			wantEnded: 1,
		},
		{
			name:    "local finishRun failed does not touch sandbox buckets",
			runner:  &fakeSandboxRunner{},
			withJob: true,
			run: func(_ *testing.T, s *Scheduler, j *Job, _ string) {
				s.finishRun(finishArgs{job: j, runID: "deadbeef00000103", startedAt: time.Now().Add(-time.Minute),
					trigger: TriggerScheduled, state: RunStateFailed, errClass: ErrClassSendError, errMsg: "boom",
					finalizer: &runFinalizer{}})
			},
			want:      counterDeltas{Ended: 1, Failed: 1},
			wantEnded: 1,
		},
		{
			name:    "local finishRun timed-out does not touch sandbox buckets",
			runner:  &fakeSandboxRunner{},
			withJob: true,
			run: func(_ *testing.T, s *Scheduler, j *Job, _ string) {
				s.finishRun(finishArgs{job: j, runID: "deadbeef00000104", startedAt: time.Now().Add(-time.Minute),
					trigger: TriggerScheduled, state: RunStateTimedOut, errClass: ErrClassDeadlineExceeded, errMsg: "slow",
					finalizer: &runFinalizer{}})
			},
			want:      counterDeltas{Ended: 1, TimedOut: 1},
			wantEnded: 1,
		},
		{
			name:    "orphan live job",
			runner:  &fakeSandboxRunner{},
			withJob: true,
			run: func(t *testing.T, s *Scheduler, j *Job, storePath string) {
				writePendingFixture(t, storePath, sandboxPending{
					JobID: j.ID, RunID: "abcabcabc0000101",
					RuntimeSessionID: "run-abcabcabc0000101-1234567890123456789",
					StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
				})
				s.reconcileSandboxPending()
			},
			want:        orphanClosed,
			wantStarted: 1, wantEnded: 1, wantStopCall: 1,
		},
		{
			name:    "orphan job gone (deleted while down)",
			runner:  &fakeSandboxRunner{},
			withJob: false,
			run: func(t *testing.T, s *Scheduler, _ *Job, storePath string) {
				writePendingFixture(t, storePath, sandboxPending{
					JobID: "0123456789abcdef", RunID: "abcabcabc0000102",
					RuntimeSessionID: "run-abcabcabc0000102-1234567890123456789",
					StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
				})
				s.reconcileSandboxPending()
			},
			want:         orphanClosed,
			wantStopCall: 1,
		},
		{
			name:    "orphan job deleted in snapshot gap (#2156)",
			runner:  &fakeSandboxRunner{},
			withJob: true,
			run: func(t *testing.T, s *Scheduler, j *Job, storePath string) {
				p := sandboxPending{
					JobID: j.ID, RunID: "abcabcabc0000103",
					RuntimeSessionID: "run-abcabcabc0000103-1234567890123456789",
					StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
				}
				path := writePendingFixture(t, storePath, p)
				s.mu.Lock()
				delete(s.jobs, j.ID)
				s.mu.Unlock()
				s.reconcileOneSandboxOrphan(p, path)
			},
			want:         orphanClosed,
			wantStopCall: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
			s, rec := sandboxTestScheduler(t, tc.runner, storePath)
			var j *Job
			if tc.withJob {
				j = sandboxJob(t, s)
			}
			before := snapshotCounters()
			tc.run(t, s, j, storePath)
			got := waitCounterDeltas(t, before, tc.want.Ended)
			assertCounterDeltas(t, got, tc.want)
			if rec.startedCount() != tc.wantStarted || rec.endedCount() != tc.wantEnded {
				t.Errorf("broadcast frames started=%d ended=%d, want %d/%d",
					rec.startedCount(), rec.endedCount(), tc.wantStarted, tc.wantEnded)
			}
			tc.runner.mu.Lock()
			stops := len(tc.runner.stopped)
			tc.runner.mu.Unlock()
			if stops != tc.wantStopCall {
				t.Errorf("StopSession calls = %d, want %d", stops, tc.wantStopCall)
			}
		})
	}
}

// TestReconcileOrphan_TerminalCounterParity pins the #2172 convergence: the
// live-job branch (emitRunStarted + finishRun) and the job-gone branch
// (metrics-only mirror inside finishOrphanRun) must advance an IDENTICAL
// counter vector. Any future divergence between the two — the class of bug
// behind R20260613-CR-2 and R20260614-GO-001 — fails here even if each branch
// is individually "right" for a different definition of right.
func TestReconcileOrphan_TerminalCounterParity(t *testing.T) {
	deltasFor := func(t *testing.T, withJob bool) []int64 {
		t.Helper()
		storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
		runner := &fakeSandboxRunner{}
		s, _ := sandboxTestScheduler(t, runner, storePath)
		jobID := "0123456789abcdef"
		if withJob {
			jobID = sandboxJob(t, s).ID
		}
		writePendingFixture(t, storePath, sandboxPending{
			JobID: jobID, RunID: "abcabcabc0000110",
			RuntimeSessionID: "run-abcabcabc0000110-1234567890123456789",
			StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
		})
		before := snapshotCounters()
		s.reconcileSandboxPending()
		return waitCounterDeltas(t, before, 1)
	}
	live := deltasFor(t, true)
	gone := deltasFor(t, false)
	for i, name := range runCounterNames {
		if live[i] != gone[i] {
			t.Errorf("%s: live-job delta %d != job-gone delta %d (branches drifted)", name, live[i], gone[i])
		}
	}
}
