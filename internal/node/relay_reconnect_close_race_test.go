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
	pat1 := regexp.MustCompile(`(?s)for _, e := range resubscribes \{.*?\}\s*//[^\n]*R185-REL-M1.*?r\.mu\.Lock\(\)\s*\n\s*stillOpen := !r\.closed\s*\n\s*r\.mu\.Unlock\(\)`)
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
	pat3 := regexp.MustCompile(`(?s)if !stillOpen \{[^}]*\}\s*slog\.Info\("relay reconnected"`)
	if !pat3.MatchString(src) {
		t.Error("success INFO must follow the WARN branch's return so both logs are mutually exclusive (R185-REL-M1)")
	}
}
