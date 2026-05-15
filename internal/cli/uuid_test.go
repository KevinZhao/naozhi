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
