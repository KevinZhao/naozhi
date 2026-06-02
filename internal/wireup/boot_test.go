package wireup

import (
	"testing"
)

// TestValidate_RequiredStepsRecorded confirms Validate passes once the
// boot steps have run. EnsureCLIBackends records "cli-backends"; the
// package init() records "history-backends" on import — so by the time
// this test runs, both required steps are present and Validate must
// succeed. This pins the #1165 extension-point contract: the health hook
// reports green only when the wire path is actually wired.
func TestValidate_RequiredStepsRecorded(t *testing.T) {
	EnsureCLIBackends()
	if err := Validate(); err != nil {
		t.Fatalf("Validate() after EnsureCLIBackends = %v, want nil", err)
	}
}

// TestBootSteps_IncludesRequired verifies the boot registry — the first
// production consumer of Registry[T] (#1579) — actually carries the
// required steps and exposes them via the Registry audit surface
// (Names()). This is the "migrate a real subsystem to prove its value"
// resolution: a real, non-test instantiation drives Registry[BootStep].
func TestBootSteps_IncludesRequired(t *testing.T) {
	EnsureCLIBackends()
	steps := BootSteps()
	want := map[string]bool{"cli-backends": false, "history-backends": false}
	for _, s := range steps {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("BootSteps() missing required step %q; got %v", name, steps)
		}
	}
}

// TestRecordBootStep_Idempotent ensures a repeated record under the same
// name is a no-op (not a duplicate panic). The boot helpers' once-guards
// already prevent re-entry in production, but recordBootStep's own guard
// keeps test ordering — multiple tests calling EnsureCLIBackends — safe.
func TestRecordBootStep_Idempotent(t *testing.T) {
	recordBootStep("cli-backends", BootStep{Kind: "cli-backends", Detail: "dup"})
	recordBootStep("cli-backends", BootStep{Kind: "cli-backends", Detail: "dup2"})
	if err := Validate(); err != nil {
		t.Fatalf("Validate() after duplicate record = %v, want nil", err)
	}
}

// TestValidate_DetectsMissingStep exercises the failure path: when
// history-backends is absent Validate must return a non-nil error that
// names the missing step.
func TestValidate_DetectsMissingStep(t *testing.T) {
	// Save and restore the real bootRegistry so this test does not mutate
	// global state. We swap it with a registry that only has cli-backends.
	orig := bootRegistry
	t.Cleanup(func() { bootRegistry = orig })

	bootRegistry = NewRegistry[BootStep]("boot-step-test")
	bootRegistry.Register("cli-backends", BootStep{Kind: "cli-backends"})
	// history-backends intentionally absent.

	err := Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error for missing history-backends")
	}
	if !containsStr(err.Error(), "history-backends") {
		t.Errorf("Validate() error = %q; expected it to mention history-backends", err.Error())
	}
}

// TestValidate_KindDoesNotSatisfyName guards the fix for R164029-CR-1:
// registering a step whose Kind matches a required name but whose
// registered name does not must still cause Validate to report the
// required name as missing.
func TestValidate_KindDoesNotSatisfyName(t *testing.T) {
	orig := bootRegistry
	t.Cleanup(func() { bootRegistry = orig })

	bootRegistry = NewRegistry[BootStep]("boot-step-test2")
	// Register with a different name but Kind == "cli-backends".
	// Before the fix, the have-map keyed on Kind would mark cli-backends
	// as present; after the fix it must still be missing.
	bootRegistry.Register("history-backends", BootStep{Kind: "history-backends"})
	bootRegistry.Register("alt-cli", BootStep{Kind: "cli-backends"})

	err := Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error: 'cli-backends' step not registered by that exact name")
	}
	if !containsStr(err.Error(), "cli-backends") {
		t.Errorf("Validate() error = %q; expected it to mention cli-backends", err.Error())
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
