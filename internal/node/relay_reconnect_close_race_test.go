package node

import (
	"os"
	"regexp"
	"testing"
)

// TestWSRelay_Source_ReconnectClosedRecheck locks R185-REL-M1: after the
// resubscribe loop in reconnect(), the code must re-check r.closed under
// r.mu and emit a distinct WARN log on the race, rather than unconditionally
// logging "relay reconnected" INFO. Without this, a Close() that lands after
// connect() but between resub loop iterations silently swallows subscribe
// frames (writeJSON's closed-guard returns), leaving the relay in a
// half-state that operators cannot distinguish from a real successful
// reconnect.
//
// This is a source-level anchor test because driving the race reliably
// would require injecting a hook into writeJSON — an API surface the
// package intentionally does not expose. The anchors below are the
// minimum structural invariants that collectively define the fix.
func TestWSRelay_Source_ReconnectClosedRecheck(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("relay.go")
	if err != nil {
		t.Fatalf("read relay.go: %v", err)
	}
	src := string(data)

	// Anchor 1: the resub fan-out loop must be followed by a re-check of
	// r.closed under r.mu. The pattern insists on Lock→read→Unlock to make
	// a future refactor that drops the lock or reads stale state fail this
	// test rather than silently regress.
	// (R20260605B-CORR-15 added a connAlive snapshot in the same critical
	// section; allow optional extra reads before the Unlock so this anchor
	// stays valid alongside that fix.)
	pat1 := regexp.MustCompile(`(?s)for _, e := range resubscribes \{.*?\}\s*//[^\n]*R185-REL-M1.*?r\.mu\.Lock\(\)\s*\n\s*stillOpen := !r\.closed\s*\n(?:[^\n]*\n)*?\s*r\.mu\.Unlock\(\)`)
	if !pat1.MatchString(src) {
		t.Error("reconnect must re-check r.closed under r.mu after the resub loop (R185-REL-M1)")
	}

	// Anchor 2: the not-open branch emits a distinct WARN with a phrase
	// that operators can grep for. This guards against replacing the WARN
	// with a silent return (which would regress the observability goal).
	pat2 := regexp.MustCompile(`(?s)if !stillOpen \{\s*slog\.Warn\("relay reconnect aborted by close".*?\n\s*return\s*\n\s*\}`)
	if !pat2.MatchString(src) {
		t.Error("race branch must emit slog.Warn with \"relay reconnect aborted by close\" and return (R185-REL-M1)")
	}

	// Anchor 3: the success INFO still ships the same attrs, but is now
	// reachable only on the stillOpen path — the WARN branch returns above.
	// This prevents a regression that logs both WARN and INFO on the race.
	// (Updated for R20260605B-CORR-15: a connAlive re-check branch now sits
	// between the stillOpen branch and the success INFO, so the INFO is
	// preceded by both the !stillOpen and the !connAlive guard branches.)
	pat3 := regexp.MustCompile(`(?s)if !connAlive \{[^}]*continue\s*\n\s*\}\s*slog\.Info\("relay reconnected"`)
	if !pat3.MatchString(src) {
		t.Error("success INFO must follow the connAlive guard's continue so a dead new conn does not log a false success (R20260605B-CORR-15)")
	}
}

// TestWSRelay_Source_ReconnectRetriesOnDeadNewConn locks R20260605B-CORR-15:
// reconnect()'s single-flight reconnecting flag is held for the whole
// duration of reconnect(). connect() spawns a FRESH readLoop before
// reconnect() writes its resubscribe frames; if that new socket dies during
// the resubscribe window the new readLoop's reconnect enqueue is silently
// dropped by the CompareAndSwap (flag still held). If reconnect() then
// returned, it would clear the flag with no live conn and NO scheduled
// retry — permanent liveness loss. The fix re-reads r.conn under r.mu after
// the resubscribe loop and, when it has been nilled by the new readLoop,
// loops to redial (continue) instead of returning a false success.
//
// Driving the millisecond resubscribe-window race deterministically would
// require a writeLoop/connect injection hook the package intentionally does
// not expose, so this is a source-level anchor test (same approach as the
// sibling R185-REL-M1 test above). The anchors are the minimum structural
// invariants that collectively define the fix.
func TestWSRelay_Source_ReconnectRetriesOnDeadNewConn(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("relay.go")
	if err != nil {
		t.Fatalf("read relay.go: %v", err)
	}
	src := string(data)

	// Anchor 1: the post-resub re-check must snapshot r.conn (not just
	// r.closed) under the same r.mu critical section. A refactor that drops
	// the connAlive read would re-introduce the dropped-enqueue liveness bug.
	pat1 := regexp.MustCompile(`(?s)r\.mu\.Lock\(\)\s*\n\s*stillOpen := !r\.closed\s*\n\s*connAlive := r\.conn != nil\s*\n\s*r\.mu\.Unlock\(\)`)
	if !pat1.MatchString(src) {
		t.Error("reconnect must snapshot r.conn (connAlive) alongside r.closed under r.mu after the resub loop (R20260605B-CORR-15)")
	}

	// Anchor 2: when the new conn was nilled (the dropped-enqueue case), the
	// reconnect loop must redial (continue) rather than return. The continue
	// re-enters the for loop's dial/backoff path, guaranteeing a retry is
	// scheduled within the same already-running reconnect goroutine.
	pat2 := regexp.MustCompile(`(?s)if !connAlive \{.*?backoff = min\(backoff\*2, maxBackoff\)\s*\n\s*continue\s*\n\s*\}`)
	if !pat2.MatchString(src) {
		t.Error("dead-new-conn branch must back off and continue the reconnect loop, not return (R20260605B-CORR-15)")
	}
}
