package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSandboxRunner records the job it received and returns a canned
// outcome, optionally feeding lines to the event sink first.
type fakeSandboxRunner struct {
	mu      sync.Mutex
	gotJobs []SandboxJob
	lines   []string
	outcome SandboxOutcome
	err     error
}

func (f *fakeSandboxRunner) RunJob(_ context.Context, job SandboxJob, sink func([]byte) error) (SandboxOutcome, error) {
	f.mu.Lock()
	f.gotJobs = append(f.gotJobs, job)
	f.mu.Unlock()
	if f.err != nil {
		return SandboxOutcome{}, f.err
	}
	for _, l := range f.lines {
		if sink != nil {
			if err := sink([]byte(l)); err != nil {
				return SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: err.Error()}, nil
			}
		}
	}
	return f.outcome, nil
}

// panicRouter fails the test if the sandbox RUN path ever touches the
// session router — the run-once model must not spawn or reset anything.
// RegisterCronStubWithChain is allowed: it fires on the AddJob CRUD path
// (sidebar stub at creation), which is placement-independent.
type panicRouter struct{ t *testing.T }

func (r panicRouter) RegisterCronStubWithChain(key, ws, p string, c []string) {}
func (r panicRouter) Reset(key string) {
	r.t.Errorf("sandbox run touched router: Reset(%q)", key)
}
func (r panicRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	r.t.Error("sandbox run touched router: GetOrCreate")
	return nil, SessionExisting, errors.New("must not be called")
}

func sandboxTestScheduler(t *testing.T, runner SandboxRunner, storePath string) (*Scheduler, *recordingBroadcaster) {
	t.Helper()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath},
		SchedulerDeps{Router: panicRouter{t: t}, Telemetry: rec, Sandbox: runner})
	t.Cleanup(func() { s.Stop() })
	return s, rec
}

func sandboxJob(t *testing.T, s *Scheduler) *Job {
	t.Helper()
	j := NewJobFull(JobInit{
		Schedule:  "@daily",
		Prompt:    "do the thing",
		IM:        JobIMContext{Platform: "dashboard", ChatID: "global"},
		Placement: PlacementSandbox,
	})
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return j
}

func waitEnded(t *testing.T, rec *recordingBroadcaster) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec.endedCount() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("run never reached terminal state")
}

func TestSandbox_SuccessRoutesThroughFinishRun(t *testing.T) {
	runner := &fakeSandboxRunner{
		lines:   []string{`{"kind":"cli","line":{"type":"result","is_error":false,"result":"答案"}}`},
		outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "答案"},
	}
	s, rec := sandboxTestScheduler(t, runner, "")
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	ev := rec.endedAtCron(0)
	if ev.State != RunStateSucceeded {
		t.Fatalf("state = %q, want succeeded", ev.State)
	}
	if ev.ErrorClass != ErrClassNone {
		t.Fatalf("error_class = %q, want empty", ev.ErrorClass)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.gotJobs) != 1 {
		t.Fatalf("runner saw %d jobs, want 1", len(runner.gotJobs))
	}
	if runner.gotJobs[0].Prompt != "do the thing" {
		t.Fatalf("prompt = %q", runner.gotJobs[0].Prompt)
	}
	if runner.gotJobs[0].RunID == "" {
		t.Fatal("RunID must be populated")
	}
}

func TestSandbox_FailedCleanMapsToSandboxFailed(t *testing.T) {
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedClean, ErrMsg: "exit 1"},
	}
	s, rec := sandboxTestScheduler(t, runner, "")
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	ev := rec.endedAtCron(0)
	if ev.State != RunStateFailed {
		t.Fatalf("state = %q, want failed", ev.State)
	}
	if ev.ErrorClass != ErrClassSandboxFailed {
		t.Fatalf("error_class = %q, want sandbox_failed", ev.ErrorClass)
	}
}

func TestSandbox_TransportMapsToSandboxTransport_UnconfirmedStopInMessage(t *testing.T) {
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "stream reset", StopConfirmed: false},
	}
	s, rec := sandboxTestScheduler(t, runner, "")
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	ev := rec.endedAtCron(0)
	if ev.ErrorClass != ErrClassSandboxTransport {
		t.Fatalf("error_class = %q, want sandbox_transport", ev.ErrorClass)
	}
	// §6.2: unconfirmed termination must be visible to operators.
	if !strings.Contains(ev.ErrorMsg, "UNKNOWN") {
		t.Fatalf("ErrorMsg %q must flag unconfirmed microVM fate", ev.ErrorMsg)
	}
}

func TestSandbox_NoRunnerConfigured(t *testing.T) {
	s, rec := sandboxTestScheduler(t, nil, "")
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	ev := rec.endedAtCron(0)
	if ev.ErrorClass != ErrClassSandboxUnavailable {
		t.Fatalf("error_class = %q, want sandbox_unavailable", ev.ErrorClass)
	}
}

func TestSandbox_EventsStreamToDisk(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	lines := []string{
		`{"kind":"boot","msg":"materialized"}`,
		`{"kind":"cli","line":{"type":"result","is_error":false}}`,
		`{"kind":"exit","code":0}`,
	}
	runner := &fakeSandboxRunner{
		lines:   lines,
		outcome: SandboxOutcome{State: SandboxStateSuccess},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	// §6.1 streaming-to-disk: every envelope line must be on disk.
	evDir := filepath.Join(dir, "sandboxevents", j.ID)
	entries, err := os.ReadDir(evDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("event log dir: entries=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(evDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(got) != len(lines) {
		t.Fatalf("event log has %d lines, want %d", len(got), len(lines))
	}
	for i := range lines {
		if got[i] != lines[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], lines[i])
		}
	}
}

func TestSandbox_NeverTouchesRouter(t *testing.T) {
	// panicRouter fails the test on any call; a full success pass proves
	// the sandbox path is router-free (structural cron-leak immunity).
	runner := &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateSuccess}}
	s, rec := sandboxTestScheduler(t, runner, "")
	j := sandboxJob(t, s)
	s.executeOpt(j, true)
	waitEnded(t, rec)
}

func TestPlacementValidation(t *testing.T) {
	for _, ok := range []string{"", "local", "sandbox"} {
		if err := validatePlacement(ok); err != nil {
			t.Fatalf("validatePlacement(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"cloud", "SANDBOX", "local "} {
		if err := validatePlacement(bad); err == nil {
			t.Fatalf("validatePlacement(%q) = nil, want error", bad)
		}
	}
}

func TestAddJob_SandboxWithWorkDirRejected(t *testing.T) {
	s, _ := sandboxTestScheduler(t, nil, "")
	j := NewJobFull(JobInit{
		Schedule: "@daily", Prompt: "x",
		IM:        JobIMContext{Platform: "dashboard", ChatID: "global"},
		Placement: PlacementSandbox, WorkDir: "/tmp/repo",
	})
	if err := s.AddJob(j); !errors.Is(err, ErrSandboxWorkDir) {
		t.Fatalf("AddJob = %v, want ErrSandboxWorkDir", err)
	}
}

func TestUpdateJob_PlacementFlipOntoWorkDirRejected(t *testing.T) {
	s, _ := sandboxTestScheduler(t, nil, "")
	j := NewJobFull(JobInit{
		Schedule: "@daily", Prompt: "x",
		IM:      JobIMContext{Platform: "dashboard", ChatID: "global"},
		WorkDir: "/tmp/repo",
	})
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sb := PlacementSandbox
	if _, err := s.UpdateJob(j.ID, JobUpdate{Placement: &sb}); !errors.Is(err, ErrSandboxWorkDir) {
		t.Fatalf("UpdateJob = %v, want ErrSandboxWorkDir", err)
	}
	// And the job must be unchanged (atomic abort).
	jobs := s.ListJobs("dashboard", "global")
	if len(jobs) != 1 || jobs[0].Placement != "" {
		t.Fatalf("job mutated after rejected update: %+v", jobs)
	}
}

func TestUpdateJob_PlacementSetAndClear(t *testing.T) {
	s, _ := sandboxTestScheduler(t, nil, "")
	j := NewJobFull(JobInit{
		Schedule: "@daily", Prompt: "x",
		IM: JobIMContext{Platform: "dashboard", ChatID: "global"},
	})
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sb := PlacementSandbox
	if _, err := s.UpdateJob(j.ID, JobUpdate{Placement: &sb}); err != nil {
		t.Fatalf("set sandbox: %v", err)
	}
	if got := s.ListJobs("dashboard", "global")[0].Placement; got != PlacementSandbox {
		t.Fatalf("placement = %q, want sandbox", got)
	}
	clear := ""
	if _, err := s.UpdateJob(j.ID, JobUpdate{Placement: &clear}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := s.ListJobs("dashboard", "global")[0].Placement; got != "" {
		t.Fatalf("placement = %q, want empty after clear", got)
	}
}
