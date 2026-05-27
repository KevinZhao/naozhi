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

// TestCLIIdentityPacking_GettersStillRoundTrip pins the public API:
// SetBackend / SetCLIName / SetCLIVersion in any order, the corresponding
// getters AND Snapshot return the values that were set. This catches
// regressions where the new packed layout drops a field on partial-update.
func TestCLIIdentityPacking_GettersStillRoundTrip(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "t:direct:u:a"}
	s.SetCLIName("claude-code")
	s.SetBackend("claude")
	s.SetCLIVersion("2.0.0")

	if got := s.Backend(); got != "claude" {
		t.Errorf("Backend: got %q, want %q", got, "claude")
	}
	if got := s.CLIName(); got != "claude-code" {
		t.Errorf("CLIName: got %q, want %q", got, "claude-code")
	}
	if got := s.CLIVersion(); got != "2.0.0" {
		t.Errorf("CLIVersion: got %q, want %q", got, "2.0.0")
	}

	snap := s.Snapshot()
	if snap.Backend != "claude" || snap.CLIName != "claude-code" || snap.CLIVersion != "2.0.0" {
		t.Errorf("Snapshot: got Backend=%q CLIName=%q CLIVersion=%q",
			snap.Backend, snap.CLIName, snap.CLIVersion)
	}
}

// TestCLIIdentityPacking_PartialUpdatePreservesOthers covers the
// router_discovery.go path that sets CLIName + CLIVersion but leaves
// Backend at "" (registerStub / discovery branches). A naive replacement
// that swapped the whole pointer to {backend:"", cliName, cliVersion}
// would clobber a Backend that an earlier shim-reconnect path had set.
func TestCLIIdentityPacking_PartialUpdatePreservesOthers(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "t:direct:u:a"}
	s.SetBackend("kiro")
	s.SetCLIName("kiro")
	s.SetCLIVersion("1.0.0")

	// A subsequent partial update (e.g. router refresh) overwrites only
	// CLIName/CLIVersion. Backend MUST survive.
	s.SetCLIName("kiro")
	s.SetCLIVersion("1.1.0")

	if got := s.Backend(); got != "kiro" {
		t.Errorf("partial update clobbered Backend: got %q, want %q", got, "kiro")
	}
	if got := s.CLIVersion(); got != "1.1.0" {
		t.Errorf("CLIVersion not updated: got %q, want %q", got, "1.1.0")
	}
}

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

// TestCLIIdentityPacking_ZeroValueReadIsSafe pins the nil-pointer load
// path: a freshly-constructed session whose cliIdentity has never been
// stored must return all-empty strings (matches legacy
// atomic.Pointer[string].Load==nil → "" behaviour the getters used to
// rely on via loadAtomicString).
func TestCLIIdentityPacking_ZeroValueReadIsSafe(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "t:direct:u:a"}
	if got := s.Backend(); got != "" {
		t.Errorf("zero-state Backend: got %q, want empty", got)
	}
	if got := s.CLIName(); got != "" {
		t.Errorf("zero-state CLIName: got %q, want empty", got)
	}
	if got := s.CLIVersion(); got != "" {
		t.Errorf("zero-state CLIVersion: got %q, want empty", got)
	}
	snap := s.Snapshot()
	if snap.Backend != "" || snap.CLIName != "" || snap.CLIVersion != "" {
		t.Errorf("zero-state Snapshot: got Backend=%q CLIName=%q CLIVersion=%q",
			snap.Backend, snap.CLIName, snap.CLIVersion)
	}
}
