package cli

import (
	"regexp"
	"sync"
	"testing"
)

// TestNewEventUUID_Shape locks the wire width + charset. Downstream
// (MergedSource dedup) hashes on this value so a drift would
// silently break dedup for the transitional period.
func TestNewEventUUID_Shape(t *testing.T) {
	uuid := newEventUUID()
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	if !re.MatchString(uuid) {
		t.Errorf("UUID shape mismatch: %q", uuid)
	}
}

// TestNewEventUUID_Unique gives a probabilistic sanity check —
// running 10 000 calls without a collision is extremely unlikely if
// the RNG is broken, and cheap enough to run on every CI build.
func TestNewEventUUID_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 10_000)
	for i := 0; i < 10_000; i++ {
		u := newEventUUID()
		if _, dup := seen[u]; dup {
			t.Fatalf("collision after %d generations: %q", i, u)
		}
		seen[u] = struct{}{}
	}
}

// TestNewEventUUID_Concurrent exposes the function to -race and
// reasonable contention. The atomic counter fallback path is the
// only shared state; uncovered by the other tests because crypto/rand
// normally succeeds.
func TestNewEventUUID_Concurrent(t *testing.T) {
	const workers = 8
	const per = 500

	var wg sync.WaitGroup
	ch := make(chan string, workers*per)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				ch <- newEventUUID()
			}
		}()
	}
	wg.Wait()
	close(ch)

	seen := make(map[string]struct{}, workers*per)
	for u := range ch {
		if _, dup := seen[u]; dup {
			t.Fatalf("concurrent collision: %q", u)
		}
		seen[u] = struct{}{}
	}
}

// TestDeriveLegacyUUID_Deterministic is the foundation of
// MergedSource's dedup: the same (time, type, summary, detail) tuple
// MUST always derive the same UUID. Breakage here would cause old
// Claude JSONL replays to re-duplicate existing naozhi-local entries.
func TestDeriveLegacyUUID_Deterministic(t *testing.T) {
	got1 := DeriveLegacyUUID(1700000000000, "user", "hi", "")
	got2 := DeriveLegacyUUID(1700000000000, "user", "hi", "")
	if got1 != got2 {
		t.Errorf("non-deterministic: %q vs %q", got1, got2)
	}
}

// TestDeriveLegacyUUID_InputsAffectOutput: changing any input must
// change the output. Guards against accidental hash-input reduction.
func TestDeriveLegacyUUID_InputsAffectOutput(t *testing.T) {
	base := DeriveLegacyUUID(1, "user", "hi", "")
	cases := []struct {
		name string
		got  string
	}{
		{"time", DeriveLegacyUUID(2, "user", "hi", "")},
		{"type", DeriveLegacyUUID(1, "text", "hi", "")},
		{"summary", DeriveLegacyUUID(1, "user", "hello", "")},
		{"detail", DeriveLegacyUUID(1, "user", "hi", "more")},
	}
	for _, tc := range cases {
		if tc.got == base {
			t.Errorf("%s change did not alter UUID: still %q", tc.name, base)
		}
	}
}

// TestDeriveLegacyUUID_Shape locks the width so it matches
// newEventUUID's shape — MergedSource dedups UUID strings directly
// and must not have to special-case one shape vs the other.
func TestDeriveLegacyUUID_Shape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	if got := DeriveLegacyUUID(1, "user", "hi", ""); !re.MatchString(got) {
		t.Errorf("DeriveLegacyUUID shape mismatch: %q", got)
	}
}
