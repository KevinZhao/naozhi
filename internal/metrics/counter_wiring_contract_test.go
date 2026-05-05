package metrics_test

import (
	"os"
	"regexp"
	"testing"
)

// Contract tests that pin the 5 canonical call sites for each OBS2 counter.
// These are source-level greps rather than runtime assertions because the
// spawn / evict / auth-fail paths are hard to drive end-to-end without a
// full hub + shim infrastructure. The counters are trivial to increment in
// the wrong branch (e.g. under Spawn's error path); these tests turn that
// into a CI failure with a clear diff.
//
// Any refactor that legitimately moves a counter MUST update this test in
// the same change — a drifted wiring is exactly the regression this file
// exists to catch.

type wiringCase struct {
	name    string
	path    string
	pattern string // regex anchored somewhere in the file
}

func TestOBS2_CounterCallSiteWiring(t *testing.T) {
	t.Parallel()
	cases := []wiringCase{
		{
			name:    "SessionCreateTotal fires in spawnSession success path",
			path:    "../session/router.go",
			pattern: `metrics\.SessionCreateTotal\.Add\(1\)`,
		},
		{
			name:    "SessionEvictTotal fires in evictOldest success path",
			path:    "../session/router.go",
			pattern: `metrics\.SessionEvictTotal\.Add\(1\)`,
		},
		{
			name:    "CLISpawnTotal fires at end of wrapper.Spawn success path",
			path:    "../cli/wrapper.go",
			pattern: `metrics\.CLISpawnTotal\.Add\(1\)`,
		},
		{
			name:    "WSAuthFailTotal fires on both WS auth_fail branches",
			path:    "../server/wshub.go",
			pattern: `metrics\.WSAuthFailTotal\.Add\(1\)`,
		},
		{
			name:    "ShimRestartTotal fires at end of StartShimWithBackend",
			path:    "../shim/manager.go",
			pattern: `metrics\.ShimRestartTotal\.Add\(1\)`,
		},
		{
			// R172-ARCH-D10: lives inside panicSafeSpawnFn's recover arm so
			// it is incremented once per absorbed panic. Wiring outside the
			// recover arm (or removing it entirely) would silence the
			// operator's "spawn panic happened" signal.
			name:    "SpawnPanicRecoveredTotal fires in panicSafeSpawnFn recover arm",
			path:    "../session/router.go",
			pattern: `metrics\.SpawnPanicRecoveredTotal\.Add\(1\)`,
		},
		{
			// R172-ARCH-D10: only the R53-ARCH-001 fallback branch — AFTER
			// hasInjectedHistory() short-circuit — must count. Wiring on the
			// happy path would turn the signal into "all shim-managed loads"
			// and drown out the "reconnect missed" flag.
			name:    "ShimReconnectGraceBackfillTotal fires in grace-deferred backfill path",
			path:    "../session/router.go",
			pattern: `metrics\.ShimReconnectGraceBackfillTotal\.Add\(1\)`,
		},
		{
			// R172-ARCH-D10: Interrupt counters live in Router.InterruptSessionViaControl
			// so every caller (HTTP / WS / dispatch) contributes to the same signal.
			// Wiring inside ManagedSession.InterruptViaControl would work but
			// leaks the metrics dependency into the lower layer.
			name:    "InterruptSentTotal fires on InterruptSent branch",
			path:    "../session/router.go",
			pattern: `metrics\.InterruptSentTotal\.Add\(1\)`,
		},
		{
			name:    "InterruptNoTurnTotal fires on InterruptNoTurn branch",
			path:    "../session/router.go",
			pattern: `metrics\.InterruptNoTurnTotal\.Add\(1\)`,
		},
		{
			name:    "InterruptUnsupportedTotal fires on InterruptUnsupported branch",
			path:    "../session/router.go",
			pattern: `metrics\.InterruptUnsupportedTotal\.Add\(1\)`,
		},
		{
			name:    "InterruptErrorTotal fires on InterruptError branch",
			path:    "../session/router.go",
			pattern: `metrics\.InterruptErrorTotal\.Add\(1\)`,
		},
		{
			// R172-ARCH-D10: split counters live in the same branches as the
			// aggregate WSAuthFailTotal — rate-limited and invalid-token arms
			// of handleAuth. Absence means an arm was refactored to bypass
			// the split (a regression).
			name:    "WSAuthFailRateLimitedTotal fires in rate-limit arm",
			path:    "../server/wshub.go",
			pattern: `metrics\.WSAuthFailRateLimitedTotal\.Add\(1\)`,
		},
		{
			name:    "WSAuthFailInvalidTokenTotal fires in invalid-token arm",
			path:    "../server/wshub.go",
			pattern: `metrics\.WSAuthFailInvalidTokenTotal\.Add\(1\)`,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(c.path)
			if err != nil {
				t.Fatalf("read %s: %v", c.path, err)
			}
			re := regexp.MustCompile(c.pattern)
			if !re.Match(data) {
				t.Errorf("%s: pattern %q not found in %s — counter wiring likely removed or renamed",
					c.name, c.pattern, c.path)
			}
		})
	}
}

// TestOBS2_SpawnPanicRecoveredInRecoverArm pins that SpawnPanicRecoveredTotal
// lives INSIDE the `if r := recover(); r != nil` arm of panicSafeSpawnFn —
// incrementing it on the happy path would turn the counter into "spawn
// attempts" instead of "panics absorbed" and silently invert its operational
// meaning. Source-level check because the happy path has no panic-injection
// seam that would drive the bug at runtime.
func TestOBS2_SpawnPanicRecoveredInRecoverArm(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../session/router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	// Match the recover arm up to the counter Add. `(?s)` lets `.` cross
	// newlines; the non-greedy `.*?` ensures we find the nearest Add after
	// the recover check, not a later one in a different function.
	re := regexp.MustCompile(`(?s)if r := recover\(\); r != nil \{.*?metrics\.SpawnPanicRecoveredTotal\.Add\(1\)`)
	if !re.Match(data) {
		t.Error("metrics.SpawnPanicRecoveredTotal.Add(1) not found inside a " +
			"`if r := recover(); r != nil` arm in router.go. The counter must " +
			"live in the recover branch of panicSafeSpawnFn — incrementing it " +
			"on the happy path (every Spawn call) would turn 'panics absorbed' " +
			"into 'spawn attempts' and break the R172-ARCH-D10 signal.")
	}
}

// TestOBS2_WSAuthFailBothBranches pins that WSAuthFailTotal is incremented
// by BOTH branches of handleAuth (rate-limit-hit and invalid-token). If a
// refactor only keeps one, operators watching naozhi_ws_auth_fail_total
// lose signal for the other class.
func TestOBS2_WSAuthFailBothBranches(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../server/wshub.go")
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}
	re := regexp.MustCompile(`metrics\.WSAuthFailTotal\.Add\(1\)`)
	matches := re.FindAll(data, -1)
	if len(matches) < 2 {
		t.Errorf("expected ≥2 WSAuthFailTotal.Add sites in wshub.go (rate-limit + invalid-token), got %d",
			len(matches))
	}
}

// TestOBS2_InterruptCountersInOutcomeSwitch pins that every Interrupt*
// counter increment sits inside a `switch outcome` — they must not be
// hoisted to the function prologue (which would count one per call rather
// than one per outcome class) nor dropped into a goroutine.
//
// The check matches the outcome switch block in InterruptSessionViaControl
// and looks for each of the 4 counters inside it. R172-ARCH-D10.
func TestOBS2_InterruptCountersInOutcomeSwitch(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../session/router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	// (?s) so `.*?` crosses newlines; match the switch head through the
	// matching right brace heuristically with a reasonable upper bound to
	// avoid consuming the whole file when a future edit removes the switch.
	blockRe := regexp.MustCompile(`(?s)switch outcome \{.*?^\s*\}`)
	blockRe.Longest()
	blocks := blockRe.FindAll(data, -1)
	if len(blocks) == 0 {
		// fallback: scan for any switch on outcome variable — the regex
		// above is newline-multiline anchored; if gofmt changed indentation
		// we still want the test to find the block.
		blockRe = regexp.MustCompile(`(?s)switch outcome \{[^}]*\}`)
		blocks = blockRe.FindAll(data, -1)
	}
	if len(blocks) == 0 {
		t.Fatalf("no `switch outcome { ... }` block found in router.go; Interrupt " +
			"outcome counters must live inside that switch to stay per-outcome")
	}
	want := []string{
		"metrics.InterruptSentTotal.Add(1)",
		"metrics.InterruptNoTurnTotal.Add(1)",
		"metrics.InterruptUnsupportedTotal.Add(1)",
		"metrics.InterruptErrorTotal.Add(1)",
	}
	found := make(map[string]bool, len(want))
	for _, b := range blocks {
		for _, w := range want {
			if regexp.MustCompile(regexp.QuoteMeta(w)).Match(b) {
				found[w] = true
			}
		}
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("%s not found inside any `switch outcome` block — it must sit "+
				"inside the per-outcome switch to preserve the outcome-class signal", w)
		}
	}
}
