package upstream

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// TestConnectorBackoffSchedule_Pinned freezes the #870 RetryPolicy contract:
// the connector reconnect loop doubles its backoff 1s -> 2s -> 4s -> 8s ->
// 16s -> 30s (capped at reconnectBackoffCeiling), and once the circuit breaker
// trips at circuitBreakerThreshold consecutive failures the backoff floor
// jumps to circuitBreakerBackoff (5m). A future #870 interface-extraction PR
// that pulls the inline Run() loop behind a RetryPolicy abstraction must keep
// this exact schedule.
//
// No wall-clock waits: the doubling is a pure arithmetic recurrence
// (min(backoff*2, ceiling)), so we replay the SAME expression Run() uses at
// connector.go:302 and assert the produced sequence. The constants are read
// directly from the package-level vars (the injectable surface the connector
// already exposes — see TestCircuitBreakerVars_PackageLevelVars).
func TestConnectorBackoffSchedule_Pinned(t *testing.T) {
	t.Parallel()

	// Constants frozen by the #870 RetryPolicy contract. Read live so a drift
	// in the package var trips here, not just in a source-text check.
	if got := reconnectBackoffCeiling; got != 30*time.Second {
		t.Errorf("reconnectBackoffCeiling = %v, want 30s (RetryPolicy ceiling)", got)
	}
	if got := circuitBreakerThreshold; got != 6 {
		t.Errorf("circuitBreakerThreshold = %d, want 6 (RetryPolicy trip count)", got)
	}
	if got := circuitBreakerBackoff; got != 5*time.Minute {
		t.Errorf("circuitBreakerBackoff = %v, want 5m (RetryPolicy breaker floor)", got)
	}

	// Replay the doubling recurrence with the production expression. Starting
	// at 1s (Run's `backoff := time.Second`), each post-sleep step applies
	// `min(backoff*2, reconnectBackoffCeiling)` exactly as connector.go:302.
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // ceiling reached (would be 32s uncapped)
		30 * time.Second, // stays pinned at the ceiling thereafter
	}
	backoff := time.Second
	got := []time.Duration{backoff}
	for i := 1; i < len(want); i++ {
		// Only doubles while below the breaker floor — mirrors the
		// `if backoff < circuitBreakerBackoff` guard wrapping the double.
		if backoff < circuitBreakerBackoff {
			backoff = min(backoff*2, reconnectBackoffCeiling)
		}
		got = append(got, backoff)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff step %d = %v, want %v (full sequence %v)", i, got[i], want[i], got)
		}
	}

	// Breaker floor jump: once tripped, the floor is raised to
	// circuitBreakerBackoff (5m), which dwarfs the 30s ceiling. Pin that the
	// floor genuinely exceeds the doubling ceiling so the breaker changes
	// behaviour (the same invariant TestCircuitBreakerVars asserts, restated
	// as part of the frozen schedule).
	if circuitBreakerBackoff <= reconnectBackoffCeiling {
		t.Errorf("breaker floor %v must exceed doubling ceiling %v", circuitBreakerBackoff, reconnectBackoffCeiling)
	}
}

// TestConnectorBackoffSource_DoublingExpressionPinned anchors the structural
// shape of the doubling site so a refactor that replaces the
// `min(backoff*2, reconnectBackoffCeiling)` recurrence with a different
// expression (e.g. a fixed step, or an unbounded double) is caught even if a
// behavioural test happens to still pass. Complements the arithmetic replay
// above the way TestRun_BreakerSourceContract complements the breaker
// behaviour tests.
func TestConnectorBackoffSource_DoublingExpressionPinned(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("connector.go")
	if err != nil {
		t.Fatalf("read connector.go: %v", err)
	}

	// `backoff := time.Second` — the 1s start of the schedule.
	if !regexp.MustCompile(`backoff\s*:=\s*time\.Second`).Match(src) {
		t.Error("Run no longer initialises backoff to time.Second — RetryPolicy 1s start changed")
	}
	// `min(backoff*2, reconnectBackoffCeiling)` — the doubling+ceiling step.
	doubling := regexp.MustCompile(`min\(\s*backoff\s*\*\s*2\s*,\s*reconnectBackoffCeiling\s*\)`)
	if !doubling.Match(src) {
		t.Error("doubling site `min(backoff*2, reconnectBackoffCeiling)` not found — " +
			"the 1s->30s doubling schedule may have been regressed out")
	}
	// `backoff = circuitBreakerBackoff` — the breaker floor jump.
	if !regexp.MustCompile(`backoff\s*=\s*circuitBreakerBackoff`).Match(src) {
		t.Error("breaker floor jump `backoff = circuitBreakerBackoff` not found — " +
			"the threshold-6 -> 5m floor may have been regressed out")
	}
}
