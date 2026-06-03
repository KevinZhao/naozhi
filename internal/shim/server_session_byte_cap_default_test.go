package shim

import (
	"testing"
)

// TestMaxClientSessionBytes_DefaultIsGenerous pins the value contract for
// R216-SEC-3 (#541): the cumulative byte cap MUST remain large enough
// that no legitimate dashboard / IM / cron session ever brushes against
// it. The cap exists to neutralise a token-holding attacker driving
// sustained 16 MB payload churn — not to throttle real users.
//
// The chosen budget is maxClientLineBytes * 1000 (i.e. 1000 worst-case
// post-auth frames). At 16 MB/frame that's 16 GB cumulative gross —
// orders of magnitude above any realistic prompt+image traffic. If a
// future refactor accidentally drops the multiplier (e.g. sets the
// default to maxClientLineBytes alone) this test fails loudly so the
// regression is caught at PR time rather than after a real session
// gets disconnected mid-conversation.
func TestMaxClientSessionBytes_DefaultIsGenerous(t *testing.T) {
	// Reset to default by setting 0 (Swap returns the prior value, which
	// we restore via defer for test hygiene).
	prev := setMaxClientSessionBytes(0)
	defer setMaxClientSessionBytes(prev)

	got := maxClientSessionBytesValue()
	wantMin := int64(maxClientLineBytes()) * 1000

	if got < wantMin {
		t.Fatalf("default cap = %d, want at least %d (%dx maxClientLineBytes)",
			got, wantMin, 1000)
	}
}

// TestMaxClientSessionBytes_OverrideRoundTrips pins the test-knob
// behaviour: setMaxClientSessionBytes(v) sets the cap to v and a
// subsequent setMaxClientSessionBytes(0) reverts to the compiled-in
// default. This is the contract every test using the override depends
// on (defer setMaxClientSessionBytes(prev)) so a regression here would
// silently leak a small cap into unrelated tests.
func TestMaxClientSessionBytes_OverrideRoundTrips(t *testing.T) {
	prev := setMaxClientSessionBytes(0) // start from default
	defer setMaxClientSessionBytes(prev)

	defaultV := maxClientSessionBytesValue()

	// Override
	const small = int64(4 * 1024)
	setMaxClientSessionBytes(small)
	if got := maxClientSessionBytesValue(); got != small {
		t.Fatalf("override cap = %d, want %d", got, small)
	}

	// Restore by setting 0 — must come back to the default, not stay at
	// the override and not fall to 0.
	setMaxClientSessionBytes(0)
	if got := maxClientSessionBytesValue(); got != defaultV {
		t.Fatalf("after reset cap = %d, want default %d", got, defaultV)
	}
}
