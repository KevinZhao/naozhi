package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestStart_OverCapJobsSurviveSaves: a job a lower cron.max_jobs left out at
// Start must still be on disk after that process saves (Stop always does),
// so raising the cap and restarting brings it back.
func TestStart_OverCapJobsSurviveSaves(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cron.json")

	s1 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10}, SchedulerDeps{})
	if err := s1.Start(); err != nil {
		t.Fatalf("phase 1 Start: %v", err)
	}
	for i, p := range []string{"alpha", "beta", "gamma"} {
		if err := s1.AddJob(&Job{Schedule: "@hourly", Prompt: p, Platform: "p", ChatID: "c"}); err != nil {
			t.Fatalf("AddJob %d: %v", i, err)
		}
	}
	s1.Stop()

	s2 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 2}, SchedulerDeps{})
	if err := s2.Start(); err != nil {
		t.Fatalf("phase 2 Start: %v", err)
	}
	if n := len(s2.ListJobs("p", "c")); n != 2 {
		t.Fatalf("at MaxJobs=2 loaded %d jobs, want 2", n)
	}
	// A mutation persists too, not only the shutdown save.
	if err := s2.AddJob(&Job{Schedule: "@hourly", Prompt: "delta", Platform: "p", ChatID: "c"}); err == nil {
		t.Fatal("AddJob over the cap succeeded")
	}
	s2.Stop()

	s3 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10}, SchedulerDeps{})
	if err := s3.Start(); err != nil {
		t.Fatalf("phase 3 Start: %v", err)
	}
	defer s3.Stop()
	if n := len(s3.ListJobs("p", "c")); n != 3 {
		t.Fatalf("after raising the cap again loaded %d jobs, want 3", n)
	}
}

// TestStart_UnadmittedJobsSurviveSaves covers the other two kinds Start turns
// away: a schedule that no longer plans and a work_dir outside allowed_root.
// Both are held as read and written back on the next save.
func TestStart_UnadmittedJobsSurviveSaves(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cron.json")
	stored := []map[string]any{
		{"id": "a1", "schedule": "@hourly", "prompt": "ok", "platform": "p", "chat_id": "c", "work_dir": root},
		{"id": "b2", "schedule": "CRON_TZ=No/Such_Zone 0 9 * * *", "prompt": "bad tz", "platform": "p", "chat_id": "c"},
		{"id": "c3", "schedule": "@hourly", "prompt": "outside", "platform": "p", "chat_id": "c", "work_dir": filepath.Join(dir, "elsewhere")},
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10, AllowedRoot: root}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := len(s.ListJobs("p", "c")); n != 1 {
		t.Fatalf("loaded %d jobs, want only a1", n)
	}
	s.Stop()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after []Job
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatalf("parse saved store: %v", err)
	}
	got := map[string]Job{}
	for _, j := range after {
		got[j.ID] = j
	}
	for _, want := range stored {
		id := want["id"].(string)
		j, ok := got[id]
		if !ok {
			t.Errorf("job %s erased from the store by the save", id)
			continue
		}
		if j.Prompt != want["prompt"] || j.Schedule != want["schedule"] {
			t.Errorf("job %s saved as prompt %q schedule %q, want it unchanged", id, j.Prompt, j.Schedule)
		}
	}
	if len(after) != len(stored) {
		t.Errorf("saved %d jobs, want %d", len(after), len(stored))
	}
}
