package cron

// R20260603140013-CR-1 regression tests.
//
// UpdateJob applies non-Schedule fields via JobUpdate.applyTo (Prompt /
// WorkDir / Notify / NotifyPlatform / NotifyChatID / FreshContext / Title /
// Backend, plus LastSessionID clearing on WorkDir change) directly into the
// live *Job before persistJobsLocked. If the persist failed, those writes —
// and any Schedule field mutation — used to stay in memory while disk kept
// the old values, so a restart replayed the stale persisted job and silently
// reverted the edit (memory/disk divergence). The fix snapshots *j by value
// before applyTo and restores it on the persist-failure path.
//
// These tests force a marshal failure (via withFailingMarshal) and assert the
// in-memory Job reverts to its pre-update state across every applyTo-written
// field plus Schedule.

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestUpdateJob_PersistFailureRollsBackAllFields(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	origNotify := true
	seed := &Job{
		Schedule:       "@every 1h",
		Prompt:         "orig-prompt",
		WorkDir:        "/tmp/orig",
		Platform:       "feishu",
		ChatID:         "chat1",
		ChatType:       "direct",
		Notify:         &origNotify,
		NotifyPlatform: "feishu",
		NotifyChatID:   "notify-orig",
		FreshContext:   false,
		Title:          "orig-title",
		Backend:        "orig-backend",
		LastSessionID:  "sess-orig",
		Paused:         true, // avoid live cron entry registration
	}
	if err := s.AddJob(seed); err != nil {
		t.Fatalf("AddJob seed: %v", err)
	}
	id := seed.ID

	// Capture the pre-update snapshot the way a restart-replay would observe
	// it (a value copy of the persisted job).
	s.mu.RLock()
	before := *s.jobs[id]
	s.mu.RUnlock()

	withFailingMarshal(t, s)

	newPrompt := "new-prompt"
	newWorkDir := "/tmp/new"
	newNotify := false
	newNotifyPlatform := "dashboard"
	newNotifyChatID := "notify-new"
	newFresh := true
	newTitle := "new-title"
	newBackend := "new-backend"
	newSchedule := "@every 2h"

	_, err := s.UpdateJob(id, JobUpdate{
		Prompt:         &newPrompt,
		WorkDir:        &newWorkDir,
		Notify:         &newNotify,
		NotifyPlatform: &newNotifyPlatform,
		NotifyChatID:   &newNotifyChatID,
		FreshContext:   &newFresh,
		Title:          &newTitle,
		Backend:        &newBackend,
		Schedule:       &newSchedule,
	})
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("UpdateJob err = %v, want ErrPersistFailed", err)
	}

	s.mu.RLock()
	got := *s.jobs[id]
	s.mu.RUnlock()

	if got.Prompt != before.Prompt {
		t.Errorf("Prompt not rolled back: got %q, want %q", got.Prompt, before.Prompt)
	}
	if got.WorkDir != before.WorkDir {
		t.Errorf("WorkDir not rolled back: got %q, want %q", got.WorkDir, before.WorkDir)
	}
	// applyTo clears LastSessionID when WorkDir changes; rollback must restore it.
	if got.LastSessionID != before.LastSessionID {
		t.Errorf("LastSessionID not rolled back: got %q, want %q", got.LastSessionID, before.LastSessionID)
	}
	if got.Notify == nil || *got.Notify != *before.Notify {
		t.Errorf("Notify not rolled back: got %v, want %v", got.Notify, before.Notify)
	}
	if got.NotifyPlatform != before.NotifyPlatform {
		t.Errorf("NotifyPlatform not rolled back: got %q, want %q", got.NotifyPlatform, before.NotifyPlatform)
	}
	if got.NotifyChatID != before.NotifyChatID {
		t.Errorf("NotifyChatID not rolled back: got %q, want %q", got.NotifyChatID, before.NotifyChatID)
	}
	if got.FreshContext != before.FreshContext {
		t.Errorf("FreshContext not rolled back: got %v, want %v", got.FreshContext, before.FreshContext)
	}
	if got.Title != before.Title {
		t.Errorf("Title not rolled back: got %q, want %q", got.Title, before.Title)
	}
	if got.Backend != before.Backend {
		t.Errorf("Backend not rolled back: got %q, want %q", got.Backend, before.Backend)
	}
	if got.Schedule != before.Schedule {
		t.Errorf("Schedule not rolled back: got %q, want %q", got.Schedule, before.Schedule)
	}
}

// TestUpdateJob_PersistSuccessAppliesFields is the positive counterpart: with a
// working marshaler the fields must actually change, so a regression that
// always rolled back (e.g. inverted error check) would be caught.
func TestUpdateJob_PersistSuccessAppliesFields(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	seed := &Job{
		Schedule: "@every 1h",
		Prompt:   "orig-prompt",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Title:    "orig-title",
		Paused:   true,
	}
	if err := s.AddJob(seed); err != nil {
		t.Fatalf("AddJob seed: %v", err)
	}
	id := seed.ID

	newPrompt := "new-prompt"
	newTitle := "new-title"
	if _, err := s.UpdateJob(id, JobUpdate{Prompt: &newPrompt, Title: &newTitle}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	s.mu.RLock()
	got := *s.jobs[id]
	s.mu.RUnlock()

	if got.Prompt != newPrompt {
		t.Errorf("Prompt not applied: got %q, want %q", got.Prompt, newPrompt)
	}
	if got.Title != newTitle {
		t.Errorf("Title not applied: got %q, want %q", got.Title, newTitle)
	}
}
