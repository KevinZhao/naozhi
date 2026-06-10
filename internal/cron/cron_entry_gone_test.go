package cron

import (
	"path/filepath"
	"testing"

	robfigcron "github.com/robfig/cron/v3"
)

// TestCronEntryGone_ZeroIDIsGone pins the cheap-precondition behaviour
// of cronEntryGoneLocked: an entryID of 0 is the zero EntryID — robfig/cron
// reserves it for "never registered" — so the helper must report gone
// without consulting s.cron at all. This matches the historical
// `entryID != 0` guard at TriggerNow's call site (R242-ARCH-29 / #774).
func TestCronEntryGone_ZeroIDIsGone(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	if !s.cronEntryGoneLocked(robfigcron.EntryID(0)) {
		t.Errorf("cronEntryGoneLocked(0) = false; want true (zero EntryID is reserved as 'never registered')")
	}
}

// TestCronEntryGone_LiveEntryIsPresent registers a job, captures its
// real entryID, and confirms cronEntryGoneLocked reports false. Without this
// test a future cron lib bump that breaks the WrappedJob == nil
// sentinel could regress unnoticed (the prior open-coded check at the
// TriggerNow call site had the same blind spot).
func TestCronEntryGone_LiveEntryIsPresent(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{
		Router: &fakeRouter{},
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	j := &Job{
		ID:       "live-job-aaaa1111",
		Schedule: "@every 1h",
		Prompt:   "live job",
		Platform: "test",
		ChatID:   "live-chat",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s.mu.RLock()
	entryID := j.entryID
	s.mu.RUnlock()
	if entryID == 0 {
		t.Fatalf("AddJob did not assign a non-zero entryID")
	}
	if s.cronEntryGoneLocked(entryID) {
		t.Errorf("cronEntryGoneLocked(%d) = true on a live entry; want false", entryID)
	}
}

// TestCronEntryGone_OrphanIDReportsGone mirrors the
// TestTriggerNow_EntryGoneReleasesWG fixture: an entryID that
// s.cron has never registered must be reported gone so callers route
// to the entry-gone branch instead of attempting to execute against
// a stale ID. This is the contract the helper exists to encapsulate.
func TestCronEntryGone_OrphanIDReportsGone(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	// 99999 is an EntryID that s.cron has never seen — robfig/cron's
	// Entry() returns a zero Entry struct (WrappedJob == nil) for any
	// unknown ID, which is the exact sentinel cronEntryGoneLocked tests for.
	if !s.cronEntryGoneLocked(robfigcron.EntryID(99999)) {
		t.Errorf("cronEntryGoneLocked(99999) = false on never-registered ID; want true")
	}
}
