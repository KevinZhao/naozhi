package shim

import (
	"strings"
	"testing"
	"time"
)

// TestStartShimContract_BuildArgsExtracted is a structural regression for
// R237-CR-11 (#717) / R246-CR-005 (#740): the 200+ line StartShimWithBackend
// function used to inline argv construction and ready-handshake parsing. The
// extraction landed as `(*Manager).buildShimArgs` plus `waitForShimReady`.
//
// If a future refactor regresses this — e.g. inlining argv back into
// StartShimWithBackend — `m.buildShimArgs` disappears and the package will
// fail to compile. This test pins the contract via a public-symbol smoke
// (the helper exists, accepts the documented inputs, returns a non-empty
// argv slice, and produces deterministic argument ordering).
//
// We intentionally do not import the test through reflect: a compile-time
// reference is the strongest possible signal.
func TestStartShimContract_BuildArgsExtracted(t *testing.T) {
	m := &Manager{
		bufferSize:      32,
		maxBufBytes:     1 << 16,
		idleTimeout:     time.Hour,
		watchdogTimeout: 10 * time.Second,
	}

	args := m.buildShimArgs("k", "/sock", "/state", "/cli", "", nil, "/cwd")
	if len(args) == 0 {
		t.Fatal("buildShimArgs returned empty argv")
	}
	if args[0] != "shim" || args[1] != "run" {
		t.Errorf("argv prefix = %v %v, want shim run", args[0], args[1])
	}

	// Cheap deterministic-order check: every spawn must put --key before
	// --socket before --state-file. The reconnect path on the shim side
	// reads them in this order, and reordering here would silently break
	// older state files written by an in-flight upgrade.
	joined := strings.Join(args, " ")
	keyIdx := strings.Index(joined, "--key")
	sockIdx := strings.Index(joined, "--socket")
	stateIdx := strings.Index(joined, "--state-file")
	if keyIdx < 0 || sockIdx < 0 || stateIdx < 0 {
		t.Fatalf("missing required flags in %q", joined)
	}
	if !(keyIdx < sockIdx && sockIdx < stateIdx) {
		t.Errorf("flag order broken: key=%d sock=%d state=%d", keyIdx, sockIdx, stateIdx)
	}
}

// TestStartShimContract_WaitForShimReadyExtracted pins the existence of the
// extracted ready-handshake helper. Same motivation as above (#717 / #740).
// A nil onFail panics the helper, so we use a no-op closure.
func TestStartShimContract_WaitForShimReadyExtracted(t *testing.T) {
	// Compile-time reference is sufficient — calling waitForShimReady with
	// real I/O is exercised by TestWaitForShimReady_* in
	// manager_start_helpers_test.go. This test fails to build if the helper
	// is renamed/removed without also retiring this contract.
	_ = waitForShimReady
}
