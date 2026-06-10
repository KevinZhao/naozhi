package cron

import (
	"errors"
	"testing"
)

// TestFindByPrefixLocked_FullIDFastPath pins the R246-GO-16 (#705) full-
// ID fast-path: when the caller supplies a complete 16-char hex job ID,
// findByPrefixLocked must hit s.jobs directly (O(1) map lookup) instead
// of scanning every entry. The test uses a Scheduler with N>1 jobs in
// the same chat and asserts the full-ID lookup returns exactly the
// matching job — equivalent to the scan path's behaviour but observable
// only by correctness (the speedup is a black-box property).
//
// Companion correctness probes:
//   - Full ID in a different chat scope must return ErrJobNotFound (not
//     leak the foreign job pointer back) — this is the cross-chat probe
//     guard the new fast path has to preserve.
//   - Partial prefix that uniquely identifies a job still resolves via
//     the scan tail (regression guard for "fast path swallowed the
//     scan branch").
//   - Partial prefix matching multiple jobs returns ErrAmbiguousPrefix
//     verbatim from the scan path.
func TestFindByPrefixLocked_FullIDFastPath(t *testing.T) {
	t.Parallel()

	// Build the scheduler with two jobs in the same chat plus one in a
	// different chat — covers the in-scope hit, cross-chat probe guard,
	// and ambiguous-prefix regression all at once.
	s := NewScheduler(SchedulerConfig{MaxJobs: 10}, SchedulerDeps{})
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
	jobC := &Job{Schedule: "@hourly", Prompt: "c", Platform: "p", ChatID: "c2"}
	if err := s.AddJob(jobC); err != nil {
		t.Fatalf("AddJob C: %v", err)
	}

	// Case 1: full ID hit in correct chat scope.
	s.mu.RLock()
	got, err := s.findByPrefixLocked(jobA.ID, "p", "c1")
	s.mu.RUnlock()
	if err != nil {
		t.Fatalf("full-ID hit: err=%v", err)
	}
	if got == nil || got.ID != jobA.ID {
		t.Fatalf("full-ID hit returned %v, want %s", got, jobA.ID)
	}

	// Case 2: full ID in WRONG chat scope must return ErrJobNotFound —
	// regression guard for "fast path leaks foreign job ptr".
	s.mu.RLock()
	_, err = s.findByPrefixLocked(jobC.ID, "p", "c1")
	s.mu.RUnlock()
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("cross-chat probe: err=%v, want wraps ErrJobNotFound", err)
	}

	// Case 3: partial prefix that uniquely identifies jobA still resolves
	// via the scan tail. Use the first 4 chars; with random 16-char hex
	// IDs the collision probability is ~2^-16 per pair, so for two jobs
	// we accept the residual flake risk and skip the case if the prefix
	// happens to match jobB. Safer: use the first char that differs
	// between jobA and jobB.
	short := uniquePrefix(jobA.ID, jobB.ID)
	if short == "" {
		t.Skip("16-char hex IDs collided in their first 8 chars; skip partial-prefix probe")
	}
	s.mu.RLock()
	got, err = s.findByPrefixLocked(short, "p", "c1")
	s.mu.RUnlock()
	if err != nil {
		t.Fatalf("partial-prefix hit: err=%v", err)
	}
	if got == nil || got.ID != jobA.ID {
		t.Fatalf("partial-prefix hit returned %v, want %s", got, jobA.ID)
	}

	// Case 4: empty / zero-length prefix is the historical "all jobs"
	// flag — with two jobs in the chat scope this must surface
	// ErrAmbiguousPrefix from the scan path. The fast path's
	// `len(idPrefix) == 16` gate keeps short prefixes on the scan
	// side, so this case pins that the tail is unchanged.
	s.mu.RLock()
	_, err = s.findByPrefixLocked("", "p", "c1")
	s.mu.RUnlock()
	if !errors.Is(err, ErrAmbiguousPrefix) {
		t.Fatalf("empty prefix in 2-job chat: err=%v, want wraps ErrAmbiguousPrefix", err)
	}

	// Case 5: full-length non-existent ID returns ErrJobNotFound (the
	// fast path's "map miss" branch falls through to the scan tail,
	// which then returns NotFound). Use a valid-shaped 16-hex string
	// that is not in the store.
	const ghostID = "ffffffffffffffff"
	s.mu.RLock()
	_, err = s.findByPrefixLocked(ghostID, "p", "c1")
	s.mu.RUnlock()
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("ghost full-ID: err=%v, want wraps ErrJobNotFound", err)
	}
}

// uniquePrefix returns the shortest prefix of a that is NOT a prefix of
// b. Returns "" if a and b are equal or share their entire length —
// neither case is reachable for two distinct random 16-hex IDs but the
// test guards the residual flake.
func uniquePrefix(a, b string) string {
	if a == b {
		return ""
	}
	for i := 1; i <= len(a) && i <= len(b); i++ {
		if a[:i] != b[:i] {
			return a[:i]
		}
	}
	return ""
}
