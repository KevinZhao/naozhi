package cron

import (
	"reflect"
	"testing"
)

// TestRunInflightView_ExportedReturnType_R249_ARCH_16 pins the #982 fix:
// the canonical inflight-view type is the EXPORTED RunInflightView, and the
// internal runInflightView is a transparent alias to it. CurrentRun's first
// return value must therefore be the exported, non-empty-package-path type so
// the public API does not leak an unexported struct (golint unexported-return).
func TestRunInflightView_ExportedReturnType_R249_ARCH_16(t *testing.T) {
	t.Parallel()

	// The alias and the exported type must be identical at the type level —
	// a value of one is assignable to the other with no conversion.
	var internal runInflightView
	var exported RunInflightView = internal // compile-time identity check
	_ = exported

	// CurrentRun's first result is the exported type and resolves to a name
	// that is exported (first rune upper-case) — guarding against a future
	// regression that reintroduces the unexported return.
	m, ok := reflect.TypeOf((*Scheduler)(nil)).MethodByName("CurrentRun")
	if !ok {
		t.Fatal("Scheduler.CurrentRun method not found")
	}
	// method signature: func(*Scheduler, string) (RunInflightView, bool)
	ret0 := m.Type.Out(0)
	if ret0.Name() != "RunInflightView" {
		t.Fatalf("CurrentRun return type = %q, want RunInflightView", ret0.Name())
	}
	if r := []rune(ret0.Name())[0]; r < 'A' || r > 'Z' {
		t.Fatalf("CurrentRun returns unexported type %q (#982 regression)", ret0.Name())
	}
}
