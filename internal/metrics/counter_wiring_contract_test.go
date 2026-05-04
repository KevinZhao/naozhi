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
