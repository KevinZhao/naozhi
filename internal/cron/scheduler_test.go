package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

func TestGenerateID(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	// 直接 json.Marshal + os.WriteFile，绕过 saveJobs（生产路径用的是
	// persistJobsLocked + saveMarshaledSeq；saveJobs 已删除）。测试关心的
	// 是 loadJobs 能恢复出磁盘上写好的 JSON 数组。
	entries := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		entries = append(entries, j)
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal jobs: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write jobs: %v", err)
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
	t.Parallel()
	result, err := loadJobs("/nonexistent/path.json")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if result != nil {
		t.Error("expected nil for missing file")
	}
}

func TestLoadJobsEmpty(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestResolveAgent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// short-circuited by the CAS gate, not run concurrently. The CAS gate now
// lives inside *runInflight (cron-run-history.md P0); the test exercises
// the embedded atomic.Bool's semantics through jobInflight.
func TestJobRunningGuardReentry(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 10})

	g := s.jobInflight("job-x")
	if !g.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS should succeed")
	}
	if g2 := s.jobInflight("job-x"); g2 != g {
		t.Fatal("jobInflight should return the same *runInflight for the same id")
	}
	if g.running.CompareAndSwap(false, true) {
		t.Fatal("re-entrant CAS should fail while guard is held")
	}
	g.running.Store(false)
	if !g.running.CompareAndSwap(false, true) {
		t.Fatal("CAS should succeed after guard released")
	}
	g.running.Store(false)

	s.runningJobs.Delete("job-x")
	if g3 := s.jobInflight("job-x"); g3 == g {
		t.Fatal("guard should be freshly allocated after delete")
	}
}

func TestSchedulerInvalidSchedule(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	key := session.CronKey(job.ID)

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
	t.Parallel()
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

// TestRedactPathsInCronError covers R61-SEC-8: err.Error() from
// session.GetOrCreate / session.Send may enumerate workspace paths that
// then land in cron_jobs.json and every dashboard broadcast. Redaction
// must preserve the structural prefix so operators still see the class
// of failure.
func TestRedactPathsInCronError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"no path", "session error: context canceled", "session error: context canceled"},
		{"no path with colon", "err: context deadline exceeded", "err: context deadline exceeded"},
		{"posix absolute", "workspace /home/ec2-user/proj: permission denied",
			"workspace <path>: permission denied"},
		{"two posix paths", "copy /src/a to /dst/b failed",
			"copy <path> to <path> failed"},
		{"windows drive path", `open C:\Users\me\x: denied`,
			"open <path>: denied"},
		{"trailing newline preserved", "err: /a/b\nnext line", "err: <path>\nnext line"},
		{"quoted path stops at quote", `failed "/tmp/x"`, `failed "<path>"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactPathsInCronError(tc.input)
			if got != tc.want {
				t.Errorf("redactPathsInCronError(%q)\n  got  = %q\n  want = %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestSchedulerMaxJobsPerChat pins the per-chat cap wiring — default
// path, override path, and the zero-means-default fallback. R208-BL2.
func TestSchedulerMaxJobsPerChat(t *testing.T) {
	t.Parallel()

	addN := func(t *testing.T, s *Scheduler, n int) int {
		t.Helper()
		ok := 0
		for i := 0; i < n; i++ {
			err := s.AddJob(&Job{Schedule: "@hourly", Prompt: "p", Platform: "p", ChatID: "c"})
			if err != nil {
				return ok
			}
			ok++
		}
		return ok
	}

	t.Run("default_fallback", func(t *testing.T) {
		t.Parallel()
		s := NewScheduler(SchedulerConfig{MaxJobs: 500})
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer s.Stop()
		got := addN(t, s, DefaultMaxJobsPerChat+1)
		if got != DefaultMaxJobsPerChat {
			t.Errorf("default cap: accepted %d jobs, want %d", got, DefaultMaxJobsPerChat)
		}
	})

	t.Run("override_lower", func(t *testing.T) {
		t.Parallel()
		s := NewScheduler(SchedulerConfig{MaxJobs: 500, MaxJobsPerChat: 5})
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer s.Stop()
		got := addN(t, s, 6)
		if got != 5 {
			t.Errorf("override cap=5: accepted %d jobs, want 5", got)
		}
	})

	t.Run("zero_falls_back_to_default", func(t *testing.T) {
		t.Parallel()
		// Explicit zero must behave identically to unset — no way to
		// disable the cap without recompiling.
		s := NewScheduler(SchedulerConfig{MaxJobs: 500, MaxJobsPerChat: 0})
		if s.maxJobsPerChat != DefaultMaxJobsPerChat {
			t.Errorf("zero cap: resolved to %d, want %d (default)", s.maxJobsPerChat, DefaultMaxJobsPerChat)
		}
	})
}

// ---------------------------------------------------------------------------
// KnownSessionIDs (R245-ARCH cron+sys hide-from-history)
// ---------------------------------------------------------------------------

func TestKnownSessionIDs_NilScheduler(t *testing.T) {
	t.Parallel()
	var s *Scheduler
	got := s.KnownSessionIDs()
	if got == nil {
		t.Fatalf("KnownSessionIDs on nil Scheduler must return non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("nil-Scheduler map should be empty, got %d entries", len(got))
	}
}

func TestKnownSessionIDs_AggregatesFromJobs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job1 := &Job{Schedule: "@every 1h", Prompt: "p1", Platform: "feishu", ChatID: "c1", ChatType: "direct"}
	job2 := &Job{Schedule: "@every 1h", Prompt: "p2", Platform: "feishu", ChatID: "c2", ChatType: "direct"}
	if err := s.AddJob(job1); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := s.AddJob(job2); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Inject LastSessionID directly through the Scheduler's internal map —
	// the public surface to set this is via execute() side-effect, which
	// requires an actual running router.
	s.mu.Lock()
	s.jobs[job1.ID].LastSessionID = "11111111-aaaa-bbbb-cccc-000000000001"
	s.jobs[job2.ID].LastSessionID = "" // empty must NOT show up
	s.mu.Unlock()

	got := s.KnownSessionIDs()
	if !got["11111111-aaaa-bbbb-cccc-000000000001"] {
		t.Errorf("LastSessionID from job1 missing: %v", got)
	}
	if got[""] {
		t.Errorf("empty LastSessionID must not be added to set: %v", got)
	}
}

func TestKnownSessionIDs_NoJobsReturnsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	got := s.KnownSessionIDs()
	if got == nil {
		t.Fatalf("KnownSessionIDs must return non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d: %v", len(got), got)
	}
}

// TestSchedulerConfig_RunsKeepPlumbing verifies that SchedulerConfig.
// RunsKeepCount and RunsKeepWindow flow through to the runStore. Zero
// values must fall back to the documented defaults so existing callers
// that omit the fields keep prior behaviour. R250-GO-3.
func TestSchedulerConfig_RunsKeepPlumbing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Custom values are preserved.
	custom := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "custom.json"),
		MaxJobs:        5,
		RunsKeepCount:  17,
		RunsKeepWindow: 3 * time.Hour,
	})
	if got := custom.runStore.keepCount; got != 17 {
		t.Errorf("keepCount: got %d, want 17", got)
	}
	if got := custom.runStore.keepWindow; got != 3*time.Hour {
		t.Errorf("keepWindow: got %v, want 3h", got)
	}

	// Zero values fall back to defaults — additive contract.
	dflt := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "default.json"),
		MaxJobs:   5,
	})
	if got := dflt.runStore.keepCount; got != DefaultRunsKeepCount {
		t.Errorf("default keepCount: got %d, want %d", got, DefaultRunsKeepCount)
	}
	if got := dflt.runStore.keepWindow; got != DefaultRunsKeepWindow {
		t.Errorf("default keepWindow: got %v, want %v", got, DefaultRunsKeepWindow)
	}
}

// TestKnownSessionIDs_TTLCache verifies the TTL snapshot kicks in:
// the first call rebuilds; subsequent calls within the TTL serve the
// same content without re-reading job state. We cannot peek inside the
// build pipeline, so we proxy "did we hit the cache" by checking the
// recorded `generatedAt` does not advance on the second call.
// R250-PERF-7.
func TestKnownSessionIDs_TTLCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	job := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.mu.Lock()
	s.jobs[job.ID].LastSessionID = "11111111-aaaa-bbbb-cccc-000000000001"
	s.mu.Unlock()

	// First call populates cache.
	first := s.KnownSessionIDs()
	if !first["11111111-aaaa-bbbb-cccc-000000000001"] {
		t.Fatalf("first call missing seeded session id: %v", first)
	}

	s.knownSessionsCache.mu.Lock()
	gen1 := s.knownSessionsCache.generatedAt
	s.knownSessionsCache.mu.Unlock()
	if gen1.IsZero() {
		t.Fatalf("generatedAt must be populated after first call")
	}

	// Mutate LastSessionID *without* invalidating — the cache should
	// still serve the old snapshot until it expires or is invalidated.
	s.mu.Lock()
	s.jobs[job.ID].LastSessionID = "22222222-aaaa-bbbb-cccc-000000000002"
	s.mu.Unlock()

	cached := s.KnownSessionIDs()
	if !cached["11111111-aaaa-bbbb-cccc-000000000001"] {
		t.Errorf("TTL cache did not serve stale snapshot: %v", cached)
	}
	if cached["22222222-aaaa-bbbb-cccc-000000000002"] {
		t.Errorf("fresh value leaked into TTL window: %v", cached)
	}

	s.knownSessionsCache.mu.Lock()
	gen2 := s.knownSessionsCache.generatedAt
	s.knownSessionsCache.mu.Unlock()
	if !gen2.Equal(gen1) {
		t.Errorf("generatedAt advanced on cached read; cache miss when fresh: %v vs %v", gen1, gen2)
	}

	// Explicit invalidation drops the snapshot; the next call rebuilds.
	s.invalidateKnownSessionsCache()
	rebuilt := s.KnownSessionIDs()
	if !rebuilt["22222222-aaaa-bbbb-cccc-000000000002"] {
		t.Errorf("invalidate+rebuild did not pick up new session id: %v", rebuilt)
	}
}
