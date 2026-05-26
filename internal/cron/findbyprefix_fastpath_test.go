package cron

import (
	"errors"
	"testing"
)

// TestFindByPrefixLocked_FullIDFastPath pins the R246-GO-16 (#705) fast path:
// when the supplied idPrefix is a canonical 16-char hex ID, findByPrefixLocked
// must return the matching job via direct map lookup without falling back to
// the O(N) scan. The (Platform, ChatID) scoping check still applies — a full
// ID from a different chat must not leak through the fast path.
func TestFindByPrefixLocked_FullIDFastPath(t *testing.T) {
	s := &Scheduler{
		jobs: make(map[string]*Job),
	}
	full := "0123456789abcdef" // 16-char lowercase hex
	want := &Job{ID: full, Platform: "feishu", ChatID: "chat-A"}
	other := &Job{ID: "fedcba9876543210", Platform: "feishu", ChatID: "chat-B"}
	s.jobs[want.ID] = want
	s.jobs[other.ID] = other

	t.Run("hit returns job", func(t *testing.T) {
		got, err := s.findByPrefixLocked(full, "feishu", "chat-A")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != want {
			t.Fatalf("findByPrefixLocked returned %v, want %v", got, want)
		}
	})

	t.Run("wrong chat falls through to scan and misses", func(t *testing.T) {
		// chat-A's full ID with chat-B's identity must NOT match. The fast
		// path's Platform/ChatID guard rejects it; the slow path's
		// HasPrefix scan also requires Platform+ChatID match so no other
		// job satisfies the predicate.
		_, err := s.findByPrefixLocked(full, "feishu", "chat-B")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})

	t.Run("non-canonical 16-char string falls through to scan", func(t *testing.T) {
		// Uppercase hex fails IsValidID, so the fast path is bypassed; the
		// scan loop runs HasPrefix and (since job IDs are lowercase) finds
		// no match.
		_, err := s.findByPrefixLocked("0123456789ABCDEF", "feishu", "chat-A")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})

	t.Run("short prefix uses scan", func(t *testing.T) {
		got, err := s.findByPrefixLocked("01234567", "feishu", "chat-A")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != want {
			t.Fatalf("findByPrefixLocked short-prefix returned %v, want %v", got, want)
		}
	})
}
