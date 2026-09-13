package cron

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newSchedulerWithStore(t *testing.T) (*Scheduler, string) {
	t.Helper()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	if s.runStore == nil || s.runStore.disabled {
		t.Fatal("runStore should be enabled when StorePath is set")
	}
	return s, storePath
}

// TestInterruptedLocalRunAppearsInHistory is the defect Epic H #2546 names: a
// local run still executing when the process went away left NO history record.
// finishRun's shutdown-cancel path sets skipPersist, which gates both the
// Job-field update (correct to skip — a cancel must not move LastRunAt) and the
// runs/ append (wrong to skip — the run did execute). A hard kill never reaches
// finishRun at all, so the marker is what covers both.
//
// The restart is simulated the way it actually happens: process A writes the
// marker and dies without finishing; process B starts on the same store dir and
// reconciles.
func TestInterruptedLocalRunAppearsInHistory(t *testing.T) {
	t.Parallel()
	s1, storePath := newSchedulerWithStore(t)

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m", Prompt: "do thing", WorkDir: "/tmp/wd"}
	s1.mu.Lock()
	s1.jobs[jobID] = j
	s1.mu.Unlock()

	runID := mustGenerateRunID()
	startedAt := time.Now().Add(-90 * time.Second)
	if path := s1.writeRunInflightMarker(runInflightMarker{
		JobID: jobID, RunID: runID, Trigger: TriggerScheduled,
		StartedAtMS: startedAt.UnixMilli(), Prompt: "do thing", WorkDir: "/tmp/wd", Fresh: true,
	}, slog.Default()); path == "" {
		t.Fatal("marker write failed")
	}
	// Process A dies here: no finishRun, so the marker survives.

	// Process B: same store, so it inherits the marker.
	s2 := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: &fakeRouter{}})
	s2.mu.Lock()
	s2.jobs[jobID] = j
	s2.mu.Unlock()
	s2.reconcileRunInflight()

	got, err := s2.Run(jobID, runID)
	if err != nil {
		t.Fatalf("the interrupted run is not in history: %v", err)
	}
	if got.State != RunStateCanceled {
		t.Errorf("state = %q, want %q", got.State, RunStateCanceled)
	}
	if got.ErrorClass != ErrClassInterrupted {
		t.Errorf("errorClass = %q, want %q — an operator must be able to tell this from a manual cancel",
			got.ErrorClass, ErrClassInterrupted)
	}
	if got.Prompt != "do thing" || got.WorkDir != "/tmp/wd" || !got.Fresh {
		t.Errorf("record lost marker fields: prompt=%q workDir=%q fresh=%v", got.Prompt, got.WorkDir, got.Fresh)
	}
	if !got.StartedAt.Equal(startedAt.Truncate(time.Millisecond)) && got.StartedAt.UnixMilli() != startedAt.UnixMilli() {
		t.Errorf("startedAt = %v, want %v (the row must show when the run actually began)", got.StartedAt, startedAt)
	}
	if got.EndedAt.Before(got.StartedAt) {
		t.Errorf("endedAt %v precedes startedAt %v", got.EndedAt, got.StartedAt)
	}

	// The marker must be gone, or every subsequent boot re-records the same run.
	if left := markerFiles(t, s2); len(left) != 0 {
		t.Errorf("markers left after reconcile: %v", left)
	}
	// And a second pass must add nothing.
	before := len(s2.ListRuns(jobID, 100, time.Time{}))
	s2.reconcileRunInflight()
	if after := len(s2.ListRuns(jobID, 100, time.Time{})); after != before {
		t.Errorf("second reconcile added records: %d -> %d", before, after)
	}
}

// TestFinishRunClearsTheInflightMarker: any terminal state invalidates the
// marker's claim, so a completed run must not resurface as interrupted.
func TestFinishRunClearsTheInflightMarker(t *testing.T) {
	t.Parallel()
	s, _ := newSchedulerWithStore(t)

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	runID := mustGenerateRunID()
	if path := s.writeRunInflightMarker(runInflightMarker{
		JobID: jobID, RunID: runID, StartedAtMS: time.Now().UnixMilli(),
	}, slog.Default()); path == "" {
		t.Fatal("marker write failed")
	}
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: time.Now().Add(-time.Second), trigger: TriggerScheduled,
		state: RunStateSucceeded, result: "ok",
	})
	if left := markerFiles(t, s); len(left) != 0 {
		t.Fatalf("marker survived a successful finish: %v", left)
	}
	// Reconcile must find nothing to record — the run already has its real row.
	s.reconcileRunInflight()
	runs := s.ListRuns(jobID, 100, time.Time{})
	if len(runs) != 1 {
		t.Fatalf("want exactly the succeeded record, got %d: %+v", len(runs), runs)
	}
	if runs[0].State != RunStateSucceeded {
		t.Errorf("state = %q, want succeeded — a finished run must not be re-recorded as interrupted", runs[0].State)
	}
}

// TestFinishRunClearsMarkerOnSkipPersistPaths: the cancel path is exactly the one
// that used to leave no history, so it must still clear its marker — otherwise
// the run would appear TWICE next boot (once canceled, once interrupted).
func TestFinishRunClearsMarkerOnSkipPersistPaths(t *testing.T) {
	t.Parallel()
	s, _ := newSchedulerWithStore(t)

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	runID := mustGenerateRunID()
	s.writeRunInflightMarker(runInflightMarker{
		JobID: jobID, RunID: runID, StartedAtMS: time.Now().UnixMilli(),
	}, slog.Default())
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: time.Now().Add(-time.Second), trigger: TriggerScheduled,
		state: RunStateCanceled, errClass: ErrClassCanceled, skipPersist: true,
	})
	if left := markerFiles(t, s); len(left) != 0 {
		t.Errorf("marker survived a skipPersist finish: %v — the run would be recorded twice next boot", left)
	}
}

// TestReconcileDropsUnusableMarkers: a corrupt or incomplete marker carries
// nothing to record, and must not be re-read on every boot.
func TestReconcileDropsUnusableMarkers(t *testing.T) {
	t.Parallel()
	s, _ := newSchedulerWithStore(t)
	jobID := mustGenerateID()
	s.mu.Lock()
	s.jobs[jobID] = &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Unlock()

	dir := s.runInflightDir()
	if err := s.mkdirStateSubtree(dir); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"corrupt.json":   `{not json`,
		"nojobid.json":   `{"run_id":"aaaa","started_at_ms":1}`,
		"norunid.json":   `{"job_id":"bbbb","started_at_ms":1}`,
		"nostarted.json": `{"job_id":"bbbb","run_id":"aaaa"}`,
		"notjson.txt":    `ignored: wrong extension`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.reconcileRunInflight()

	if got := len(s.ListRuns(jobID, 100, time.Time{})); got != 0 {
		t.Errorf("unusable markers produced %d records, want 0", got)
	}
	left := markerFiles(t, s)
	if len(left) != 1 || left[0] != "notjson.txt" {
		t.Errorf("markers left = %v, want only the non-.json file (the rest must not be re-read every boot)", left)
	}
}

// TestReconcileSkipsDeletedJobs: a job removed while naozhi was down has no
// history to attach to.
func TestReconcileSkipsDeletedJobs(t *testing.T) {
	t.Parallel()
	s, _ := newSchedulerWithStore(t)
	goneJob := mustGenerateID()
	runID := mustGenerateRunID()
	s.writeRunInflightMarker(runInflightMarker{
		JobID: goneJob, RunID: runID, StartedAtMS: time.Now().UnixMilli(),
	}, slog.Default())

	s.reconcileRunInflight()
	if got := len(s.ListRuns(goneJob, 100, time.Time{})); got != 0 {
		t.Errorf("recorded %d runs for a job that no longer exists, want 0", got)
	}
	if left := markerFiles(t, s); len(left) != 0 {
		t.Errorf("marker for a deleted job left behind: %v", left)
	}
}

// markerFiles lists the marker directory's entries by name.
func markerFiles(t *testing.T, s *Scheduler) []string {
	t.Helper()
	dir := s.runInflightDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestExecuteWritesTheInflightMarker closes the gap the tests above leave: they
// call writeRunInflightMarker directly, which proves the marker mechanism works
// but not that a real run ever writes one. The wiring is the easiest half to get
// wrong — exactly how the shim-log sweep shipped inert in v0.1.0.
//
// A router whose Send blocks until told lets the marker be observed WHILE the run
// is in flight, which is the only moment it exists.
func TestExecuteWritesTheInflightMarker(t *testing.T) {
	t.Parallel()
	s, _ := newSchedulerWithStore(t)

	release := make(chan struct{})
	sess := &gatedSendSession{entered: make(chan struct{}), release: release}
	s.router = &gatedSendRouter{sess: sess}

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m", Prompt: "do thing"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.executeOpt(j, true /* viaTriggerNow: skip jitter */); close(done) }()

	select {
	case <-sess.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Send was never entered")
	}
	// Mid-flight: exactly one marker, and it names this run.
	inflightMarkers := markerFiles(t, s)
	if len(inflightMarkers) != 1 {
		t.Fatalf("markers while in flight = %v, want exactly 1 — executeAcquired does not write one", inflightMarkers)
	}
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}
	// And finishRun cleared it.
	if left := markerFiles(t, s); len(left) != 0 {
		t.Errorf("markers after the run finished = %v, want none", left)
	}
}

type gatedSendSession struct {
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (g *gatedSendSession) Send(ctx context.Context, _ string) (SendResult, error) {
	if !g.once {
		g.once = true
		close(g.entered)
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return SendResult{}, ctx.Err()
	}
	return SendResult{Text: "ok", SessionID: "sess-gated"}, nil
}
func (g *gatedSendSession) SessionID() string                     { return "sess-gated" }
func (g *gatedSendSession) InterruptViaControl() InterruptOutcome { return InterruptSent }

type gatedSendRouter struct {
	reapRouter
	sess *gatedSendSession
}

func (r *gatedSendRouter) GetOrCreate(_ context.Context, _ string, _ AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionNew, nil
}
