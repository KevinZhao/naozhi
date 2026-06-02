package main

import (
	"reflect"
	"testing"
)

// TestRunShutdownSteps_PreservesContractOrder pins R20260530-ARCH-3 (#1487)
// and R260528-ARCH-15 (#1376): the teardown sequence sysmgr → scheduler →
// http-drain → router is a hard correctness contract. Unlike the existing
// source-string pin (runshutdown_phase_timing_test.go), this is a behavioral
// assertion on the ACTUAL call order — it executes the steps and records the
// observed sequence, so a future subsystem inserted at the wrong index (or a
// reorder that runs router before sysmgr) breaks loudly even if the slog
// fragments happen to stay textually present.
func TestRunShutdownSteps_PreservesContractOrder(t *testing.T) {
	t.Parallel()

	var calls []string
	rec := func(name string) func() {
		return func() { calls = append(calls, name) }
	}

	// Mirror main()'s build order. The names MUST match the production
	// step names so this test fails if main() renames a phase out from
	// under the source-string pin.
	steps := []shutdownStep{
		{name: "sysmgr", run: rec("sysmgr")},
		{name: "scheduler", run: rec("scheduler")},
		{name: "http-drain", run: rec("http-drain")},
		{name: "router", run: rec("router")},
	}

	ran := runShutdownSteps(steps)

	want := []string{"sysmgr", "scheduler", "http-drain", "router"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("teardown call order = %v, want %v (sysmgr must stop before scheduler/router; router last)", calls, want)
	}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("runShutdownSteps returned ran=%v, want %v", ran, want)
	}
}

// TestRunShutdownSteps_NilStepSkippedButOrderHeld pins the degraded-mode
// contract: when sysession is disabled main() passes a nil-run sysmgr step.
// The step is skipped (no call, no panic) but its slot must NOT shift the
// relative order of the remaining steps — scheduler still runs before router.
func TestRunShutdownSteps_NilStepSkippedButOrderHeld(t *testing.T) {
	t.Parallel()

	var calls []string
	rec := func(name string) func() {
		return func() { calls = append(calls, name) }
	}

	steps := []shutdownStep{
		{name: "sysmgr", run: nil}, // sysession disabled / build failed
		{name: "scheduler", run: rec("scheduler")},
		{name: "http-drain", run: rec("http-drain")},
		{name: "router", run: rec("router")},
	}

	ran := runShutdownSteps(steps)

	want := []string{"scheduler", "http-drain", "router"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("nil-sysmgr teardown order = %v, want %v", calls, want)
	}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("runShutdownSteps returned ran=%v, want %v (nil step must be skipped, not reordered)", ran, want)
	}
}
