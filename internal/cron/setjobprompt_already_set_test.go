package cron

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSetJobPrompt_FirstSetSucceedsThenAlreadySet pins R250531-CR-8 (#1503):
// SetJobPrompt auto-fills the FIRST prompt of a dashboard-created (paused,
// empty-prompt) job, but the SECOND call against a now-non-empty prompt must
// return ErrPromptAlreadySet rather than the old silent `return nil`. The
// sentinel makes the no-op observable so callers do not mistake a 200/nil for
// a successful edit — real edits go through UpdateJob.
func TestSetJobPrompt_FirstSetSucceedsThenAlreadySet(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Dashboard placeholder: paused job with empty prompt injected directly
	// (AddJob rejects empty prompts), mirroring the wshub auto-save flow.
	j := &Job{
		ID:       "abcd1234abcd1234",
		Schedule: "@every 1h",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true,
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// First set: fills the prompt and unpauses.
	if err := s.SetJobPrompt(j.ID, "first prompt"); err != nil {
		t.Fatalf("first SetJobPrompt: %v", err)
	}

	// Second set against an already-filled prompt must surface the sentinel
	// and must NOT overwrite the stored prompt.
	err := s.SetJobPrompt(j.ID, "edited prompt")
	if !errors.Is(err, ErrPromptAlreadySet) {
		t.Fatalf("second SetJobPrompt err = %v, want ErrPromptAlreadySet", err)
	}

	s.mu.Lock()
	got := s.jobs[j.ID].Prompt
	s.mu.Unlock()
	if got != "first prompt" {
		t.Fatalf("prompt mutated by no-op call: got %q, want %q", got, "first prompt")
	}
}

// TestSetJobPrompt_MissingJobStillNotFound guards that the new
// ErrPromptAlreadySet branch did not displace the ErrJobNotFound path: a
// lookup miss must still wrap ErrJobNotFound, not the already-set sentinel.
func TestSetJobPrompt_MissingJobStillNotFound(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	err := s.SetJobPrompt("0000000000000000", "x")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("SetJobPrompt missing id err = %v, want ErrJobNotFound", err)
	}
	if errors.Is(err, ErrPromptAlreadySet) {
		t.Fatalf("missing-id path must not return ErrPromptAlreadySet")
	}
}
