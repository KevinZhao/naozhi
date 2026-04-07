package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/routing"
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

	loaded := loadJobs(path)
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
	result := loadJobs("/nonexistent/path.json")
	if result != nil {
		t.Error("expected nil for missing file")
	}
}

func TestLoadJobsEmpty(t *testing.T) {
	result := loadJobs("")
	if result != nil {
		t.Error("expected nil for empty path")
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
		agent, text := routing.ResolveAgent(tt.text, cmds)
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
