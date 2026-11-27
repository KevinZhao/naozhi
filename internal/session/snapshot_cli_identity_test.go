package session

// R215-ARCH-P2-7 regression: backend / cliName / cliVersion are packed into
// a single atomic.Pointer (cliIdentity) so Snapshot() reads all three with
// one atomic Load instead of three. These tests pin the externally observable
// contract — public Backend / CLIName / CLIVersion / Snapshot getters keep
// returning the same values for the same writes — and assert that concurrent
// SetBackend / SetCLIName / SetCLIVersion writers compose without dropping
// fields (which would have been the symptom of a naive non-CAS replacement
// of the three independent atomic.Pointer[string] fields).

import (
	"sync"
	"testing"
)

// TestCLIIdentityPacking_ConcurrentWritersDoNotDropFields exercises the
// CAS loop in updateCLIIdentity. A non-CAS implementation that did
// `next := *cur; next.X = v; store(&next)` would race: thread A reads
// cur, thread B reads cur (same pointer), both compute disjoint nexts,
// last writer wins, the other's field is dropped. With CAS the loser
// retries and re-applies its mutation on top of the winner.
func TestCLIIdentityPacking_ConcurrentWritersDoNotDropFields(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "t:direct:u:a"}

	var wg sync.WaitGroup
	const iterations = 200
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			s.SetBackend("claude")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			s.SetCLIName("claude-code")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			s.SetCLIVersion("2.0.0")
		}
	}()
	wg.Wait()

	// All three fields must be at their final values; if any drop
	// occurred between iterations we'd see the zero string for that
	// field on the last observed snapshot.
	if got := s.Backend(); got != "claude" {
		t.Errorf("Backend dropped under concurrent writes: got %q", got)
	}
	if got := s.CLIName(); got != "claude-code" {
		t.Errorf("CLIName dropped under concurrent writes: got %q", got)
	}
	if got := s.CLIVersion(); got != "2.0.0" {
		t.Errorf("CLIVersion dropped under concurrent writes: got %q", got)
	}
}
