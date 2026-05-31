package cron

import (
	"errors"
	"strings"
	"testing"
)

// TestFindByPrefixLocked_AmbiguousListsCollidingIDs pins the godoc contract
// documented under R249-CR-6 (#950): when a short prefix matches ≥2 jobs in
// the same (plat, chatID) scope, findByPrefixLocked returns ErrAmbiguousPrefix
// AND the wrapped message enumerates the colliding job IDs so the operator can
// disambiguate. Other tests assert the sentinel error class; this one pins the
// human-facing "matches <id>, <id>" body the godoc promises.
func TestFindByPrefixLocked_AmbiguousListsCollidingIDs(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	jobA := &Job{Schedule: "@hourly", Prompt: "a", Platform: "p", ChatID: "c1"}
	if err := s.AddJob(jobA); err != nil {
		t.Fatalf("AddJob A: %v", err)
	}
	jobB := &Job{Schedule: "@hourly", Prompt: "b", Platform: "p", ChatID: "c1"}
	if err := s.AddJob(jobB); err != nil {
		t.Fatalf("AddJob B: %v", err)
	}

	// Empty prefix matches every job in the chat scope (here: 2) → ambiguous.
	s.mu.RLock()
	_, err := s.findByPrefixLocked("", "p", "c1")
	s.mu.RUnlock()

	if !errors.Is(err, ErrAmbiguousPrefix) {
		t.Fatalf("err=%v, want wraps ErrAmbiguousPrefix", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, jobA.ID) || !strings.Contains(msg, jobB.ID) {
		t.Fatalf("ambiguous message %q must list both colliding IDs %s and %s", msg, jobA.ID, jobB.ID)
	}
}
