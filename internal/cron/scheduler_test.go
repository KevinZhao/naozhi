package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

func TestGenerateID(t *testing.T) {
	id := generateID()
	if len(id) != 16 {
		t.Errorf("expected 16 char ID, got %d: %q", len(id), id)
	}
	// Should be unique
	id2 := generateID()
	if id == id2 {
		t.Error("expected unique IDs")
	}
}

func TestValidateSchedule(t *testing.T) {
	tests := []struct {
		schedule string
		wantErr  bool
	}{
		{"@every 30m", false},
		{"@daily", false},
		{"@hourly", false},
		{"0 9 * * 1-5", false},
		{"*/5 * * * *", false},
		{"invalid", true},
		{"", true},
	}
	for _, tt := range tests {
		err := validateSchedule(tt.schedule)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateSchedule(%q): err=%v, wantErr=%v", tt.schedule, err, tt.wantErr)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")

	jobs := map[string]*Job{
		"abc12345": {
			ID:        "abc12345",
			Schedule:  "@every 1h",
			Prompt:    "check status",
			Platform:  "feishu",
			ChatID:    "chat1",
			ChatType:  "direct",
			CreatedBy: "user1",
			CreatedAt: time.Now().Truncate(time.Second),
		},
		"def67890": {
			ID:        "def67890",
			Schedule:  "0 9 * * *",
			Prompt:    "/review scan PRs",
			Platform:  "slack",
			ChatID:    "C123",
			ChatType:  "group",
			CreatedBy: "user2",
			CreatedAt: time.Now().Truncate(time.Second),
			Paused:    true,
		},
	}

	if err := saveJobs(path, jobs); err != nil {
		t.Fatalf("saveJobs: %v", err)
	}

	loaded, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(loaded))
	}

	j := loaded["abc12345"]
	if j == nil || j.Schedule != "@every 1h" || j.Prompt != "check status" {
		t.Errorf("unexpected job: %+v", j)
	}

	j2 := loaded["def67890"]
	if j2 == nil || !j2.Paused {
		t.Errorf("expected paused job: %+v", j2)
	}
}

func TestLoadJobsMissing(t *testing.T) {
	result, err := loadJobs("/nonexistent/path.json")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if result != nil {
		t.Error("expected nil for missing file")
	}
}

func TestLoadJobsEmpty(t *testing.T) {
	result, err := loadJobs("")
	if err != nil {
		t.Fatalf("empty path should not error: %v", err)
	}
	if result != nil {
		t.Error("expected nil for empty path")
	}
}

// TestLoadJobsOversizeRefuses guards the critical data-loss bug: a store file
// larger than maxCronStoreBytes must fail loadly (returned error) AND leave
// the original file untouched, so Scheduler.Start can abort and the operator
// can inspect/recover the real data.
func TestLoadJobsOversizeRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	// maxCronStoreBytes+1 bytes of valid-looking JSON prefix; contents don't
	// matter because the size check fires before Unmarshal.
	payload := make([]byte, maxCronStoreBytes+1)
	for i := range payload {
		payload[i] = 'x'
	}
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m, err := loadJobs(path)
	if err == nil {
		t.Fatal("expected oversize error, got nil")
	}
	if m != nil {
		t.Errorf("expected nil map on oversize, got %d entries", len(m))
	}

	// File must still be on disk — critically, NOT renamed and NOT truncated.
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("original file missing after oversize refusal: %v", statErr)
	}
	if info.Size() != int64(len(payload)) {
		t.Errorf("file mutated after oversize refusal: size=%d want=%d", info.Size(), len(payload))
	}
}

// TestLoadJobsCorruptPreserves verifies the parse-failure path: the corrupt
// file is renamed (not deleted), loadJobs returns (nil, nil) so the scheduler
// can start fresh without destroying the evidence copy.
func TestLoadJobsCorruptPreserves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	if err := os.WriteFile(path, []byte("{ this is not valid json"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m, err := loadJobs(path)
	if err != nil {
		t.Fatalf("parse failure should rescue and return (nil, nil), got err=%v", err)
	}
	if m != nil {
		t.Errorf("expected nil map on corrupt file, got %d entries", len(m))
	}

	// Original file should be renamed away.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected original file to be renamed, got stat err=%v", statErr)
	}

	// A .corrupt.* sibling must exist.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	foundCorrupt := false
	for _, e := range entries {
		if len(e.Name()) > len("cron_jobs.json.corrupt.") &&
			e.Name()[:len("cron_jobs.json.corrupt.")] == "cron_jobs.json.corrupt." {
			foundCorrupt = true
			break
		}
	}
	if !foundCorrupt {
		t.Error("expected cron_jobs.json.corrupt.* sibling, not found")
	}
}

// TestSchedulerStartFailsOnOversize is the end-to-end guarantee: when the
// store is oversize, Start returns an error so main.go can os.Exit(1) before
// any code path triggers persistJobsLocked and clobbers the file with `[]`.
func TestSchedulerStartFailsOnOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	payload := make([]byte, maxCronStoreBytes+1)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	originalSize := int64(len(payload))

	s := NewScheduler(SchedulerConfig{
		StorePath: path,
		MaxJobs:   5,
	})
	if err := s.Start(); err == nil {
		// If Start unexpectedly succeeded, stop the cron goroutine before
		// failing so the test doesn't leak a goroutine into sibling runs.
		s.Stop()
		t.Fatal("expected Start to fail on oversize store, got nil")
	}

	// The file must be untouched after the failed Start: no save path should
	// have run. Re-check size as a lightweight tamper probe.
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("store file missing after failed Start: %v", statErr)
	}
	if info.Size() != originalSize {
		t.Errorf("store file clobbered after failed Start: size=%d want=%d", info.Size(), originalSize)
	}
}

func TestSaveJobsCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "cron_jobs.json")

	err := saveJobs(path, map[string]*Job{})
	if err != nil {
		t.Fatalf("saveJobs with nested dir: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestResolveAgent(t *testing.T) {
	cmds := map[string]string{
		"review":   "code-reviewer",
		"research": "researcher",
	}
	tests := []struct {
		text      string
		wantAgent string
		wantText  string
	}{
		{"hello", "general", "hello"},
		{"/review check PRs", "code-reviewer", "check PRs"},
		{"/research blockchain", "researcher", "blockchain"},
		{"/unknown stuff", "general", "/unknown stuff"},
	}
	for _, tt := range tests {
		agent, text := session.ResolveAgent(tt.text, cmds)
		if agent != tt.wantAgent || text != tt.wantText {
			t.Errorf("ResolveAgent(%q): got (%q, %q), want (%q, %q)", tt.text, agent, text, tt.wantAgent, tt.wantText)
		}
	}
}

func TestSchedulerAddAndList(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{
		Schedule: "@every 1h",
		Prompt:   "test prompt",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.ID == "" {
		t.Error("expected non-empty ID")
	}

	jobs := s.ListJobs("feishu", "chat1")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("unexpected job ID: %s", jobs[0].ID)
	}

	// Different chat should be empty
	jobs2 := s.ListJobs("feishu", "other-chat")
	if len(jobs2) != 0 {
		t.Errorf("expected 0 jobs for other chat, got %d", len(jobs2))
	}
}

func TestSchedulerMaxJobs(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 2})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	for i := 0; i < 2; i++ {
		err := s.AddJob(&Job{Schedule: "@hourly", Prompt: "test", Platform: "p", ChatID: "c"})
		if err != nil {
			t.Fatalf("AddJob %d: %v", i, err)
		}
	}

	err := s.AddJob(&Job{Schedule: "@hourly", Prompt: "test", Platform: "p", ChatID: "c"})
	if err == nil {
		t.Error("expected max jobs error")
	}
}

func TestSchedulerPauseResume(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "test", Platform: "p", ChatID: "c"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	_, err := s.PauseJob(job.ID[:4], "p", "c")
	if err != nil {
		t.Fatalf("PauseJob: %v", err)
	}

	jobs := s.ListJobs("p", "c")
	if !jobs[0].Paused {
		t.Error("expected paused")
	}

	// Pause again should fail
	_, err = s.PauseJob(job.ID[:4], "p", "c")
	if err == nil {
		t.Error("expected error on double pause")
	}

	_, err = s.ResumeJob(job.ID[:4], "p", "c")
	if err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}

	jobs = s.ListJobs("p", "c")
	if jobs[0].Paused {
		t.Error("expected not paused")
	}
}

func TestSchedulerDelete(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "test", Platform: "p", ChatID: "c"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	_, err := s.DeleteJob(job.ID[:4], "p", "c")
	if err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	jobs := s.ListJobs("p", "c")
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs after delete, got %d", len(jobs))
	}

	// Delete nonexistent
	_, err = s.DeleteJob("xxxxxxxx", "p", "c")
	if err == nil {
		t.Error("expected error deleting nonexistent job")
	}
}

// TestJobRunningGuardReentry locks in the R31-REL2 invariant: a second execute
// for the same job ID while the first is still holding the guard must be
// short-circuited by the CAS gate, not run concurrently.
func TestJobRunningGuardReentry(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 10})

	g := s.jobRunningGuard("job-x")
	if !g.CompareAndSwap(false, true) {
		t.Fatal("initial CAS should succeed")
	}
	if g2 := s.jobRunningGuard("job-x"); g2 != g {
		t.Fatal("jobRunningGuard should return the same *atomic.Bool for the same id")
	}
	if g.CompareAndSwap(false, true) {
		t.Fatal("re-entrant CAS should fail while guard is held")
	}
	g.Store(false)
	if !g.CompareAndSwap(false, true) {
		t.Fatal("CAS should succeed after guard released")
	}
	g.Store(false)

	s.runningJobs.Delete("job-x")
	if g3 := s.jobRunningGuard("job-x"); g3 == g {
		t.Fatal("guard should be freshly allocated after delete")
	}
}

func TestSchedulerInvalidSchedule(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	err := s.AddJob(&Job{Schedule: "invalid", Prompt: "test", Platform: "p", ChatID: "c"})
	if err == nil {
		t.Error("expected error for invalid schedule")
	}
}

func TestPreviewScheduleN(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	runs, err := s.PreviewScheduleN("@every 1h", 5)
	if err != nil {
		t.Fatalf("PreviewScheduleN: %v", err)
	}
	if len(runs) != 5 {
		t.Fatalf("expected 5 runs, got %d", len(runs))
	}
	// Strictly ascending; consecutive gap should match the schedule interval.
	for i := 1; i < len(runs); i++ {
		if !runs[i].After(runs[i-1]) {
			t.Errorf("run %d (%v) not after run %d (%v)", i, runs[i], i-1, runs[i-1])
		}
		if gap := runs[i].Sub(runs[i-1]); gap != time.Hour {
			t.Errorf("run %d gap = %v, want 1h", i, gap)
		}
	}

	// n <= 0 should fall back to 1 run without erroring (callers clamp).
	runs, err = s.PreviewScheduleN("@every 30m", 0)
	if err != nil {
		t.Fatalf("PreviewScheduleN zero: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("expected 1 run for n=0, got %d", len(runs))
	}

	// Invalid expressions surface the parse error.
	if _, err := s.PreviewScheduleN("not a cron", 3); err == nil {
		t.Error("expected parse error for invalid schedule")
	}
}

// TestEnsureStub exercises the recovery hook used by handleSubscribe when a
// cron stub was torn down by sidebar "×". The stub must be re-registerable
// idempotently while the job still exists, and refuse to create a stub once
// the job is gone.
func TestEnsureStub(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	s := NewScheduler(SchedulerConfig{
		Router:  router,
		MaxJobs: 10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@hourly", Prompt: "hello", Platform: "p", ChatID: "c", WorkDir: "/tmp"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	key := "cron:" + job.ID

	// AddJob already registers the stub; simulate sidebar "×" that removed it.
	if !router.Remove(key) {
		t.Fatalf("expected router to drop the stub registered by AddJob")
	}
	if router.GetSession(key) != nil {
		t.Fatalf("stub should be gone after Remove")
	}

	// EnsureStub recovers the stub for a still-registered job.
	if !s.EnsureStub(key) {
		t.Fatalf("EnsureStub should return true for an existing job")
	}
	sess := router.GetSession(key)
	if sess == nil {
		t.Fatalf("EnsureStub should have re-registered the stub")
	}

	// Second call is idempotent.
	if !s.EnsureStub(key) {
		t.Fatalf("EnsureStub should stay true when stub already present")
	}
	if router.GetSession(key) != sess {
		t.Fatalf("EnsureStub must not create a duplicate stub")
	}

	// Non-cron and malformed keys reject cleanly.
	if s.EnsureStub("planner:foo:bar") {
		t.Error("EnsureStub should reject non-cron keys")
	}
	if s.EnsureStub("cron:") {
		t.Error("EnsureStub should reject the empty id")
	}
	if s.EnsureStub("cron:nosuchjob") {
		t.Error("EnsureStub should reject an unknown job id")
	}

	// Paused jobs still get a stub — the panel must be openable so the user
	// can resume them.
	if _, err := s.PauseJobByID(job.ID); err != nil {
		t.Fatalf("PauseJobByID: %v", err)
	}
	router.Remove(key)
	if !s.EnsureStub(key) {
		t.Error("EnsureStub should succeed for paused jobs")
	}

	// After DeleteJobByID, the recovery path must no-op.
	if _, err := s.DeleteJobByID(job.ID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	if s.EnsureStub(key) {
		t.Error("EnsureStub should return false after DeleteJobByID")
	}
}

func TestSchedulerPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	// Create and add job
	s1 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10})
	s1.Start()
	s1.AddJob(&Job{Schedule: "@hourly", Prompt: "persist me", Platform: "p", ChatID: "c"})
	s1.Stop()

	// Reload
	s2 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10})
	s2.Start()
	defer s2.Stop()

	jobs := s2.ListJobs("p", "c")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 persisted job, got %d", len(jobs))
	}
	if jobs[0].Prompt != "persist me" {
		t.Errorf("unexpected prompt: %s", jobs[0].Prompt)
	}
}
