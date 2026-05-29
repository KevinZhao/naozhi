package server

import (
	"testing"
)

// TestHealthProbe_NilRouterIsNoOp pins the contract that the default
// probe factories (#647 R247-ARCH-12) return a closure that no-ops
// safely against a nil router and nil auth-section. Required because
// the test harness frequently builds a HealthHandler without a live
// router, and a nil deref here would block the unified-probe migration
// from landing.
func TestHealthProbe_NilRouterIsNoOp(t *testing.T) {
	auth := &healthAuthSection{}

	// Nil router + non-nil auth: must leave fields untouched.
	EventLogHealthProbe(nil)(auth)
	AttachmentTrackerHealthProbe(nil)(auth)
	if auth.EventLog != nil {
		t.Errorf("EventLog must be nil under nil router, got %+v", auth.EventLog)
	}
	if auth.AttachmentTracker != nil {
		t.Errorf("AttachmentTracker must be nil under nil router, got %+v", auth.AttachmentTracker)
	}

	// Nil auth-section: must NOT panic. The handleHealth call site
	// always passes a fresh auth, but defensive coding here makes the
	// probe safe to register against arbitrary harnesses.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("probe must not panic on nil auth, got %v", r)
		}
	}()
	EventLogHealthProbe(nil)(nil)
	AttachmentTrackerHealthProbe(nil)(nil)
}

// TestHealthProbe_TypeIsClosure pins the HealthProbe type as a func
// (not an interface). Closure-as-probe is load-bearing because the
// migration plan calls for tests to register ad-hoc probes via
// `RegisterProbe(func(...) {...})` — switching to an interface would
// silently force every test to define a named probe type.
func TestHealthProbe_TypeIsClosure(t *testing.T) {
	// If HealthProbe is ever changed to an interface, this assignment
	// from a literal func breaks at compile time. The presence of this
	// test in CI guarantees no silent reshape.
	var p HealthProbe = func(auth *healthAuthSection) {}
	if p == nil {
		t.Fatal("probe assignment from func literal must yield a non-nil HealthProbe")
	}
	auth := &healthAuthSection{}
	p(auth) // call must not panic
}

// TestSubsystemProbes_FanOutNilRouterIsNoOp pins the contract that
// handleHealth's fan-out over subsystemProbes() (#1052) leaves the
// auth-section untouched when the handler has a nil router — the exact
// shape the unauthenticated / test-harness path relies on. This guards
// the wiring step: a regression that made any registered probe write a
// non-nil pointer unconditionally would flip the omitempty section into
// the JSON output and break the byte-identical wire contract.
func TestSubsystemProbes_FanOutNilRouterIsNoOp(t *testing.T) {
	h := &HealthHandler{} // router nil
	probes := h.subsystemProbes()
	if len(probes) == 0 {
		t.Fatal("subsystemProbes must register at least one probe")
	}
	auth := &healthAuthSection{}
	for _, probe := range probes {
		probe(auth)
	}
	if auth.EventLog != nil {
		t.Errorf("EventLog must stay nil under nil router, got %+v", auth.EventLog)
	}
	if auth.AttachmentTracker != nil {
		t.Errorf("AttachmentTracker must stay nil under nil router, got %+v", auth.AttachmentTracker)
	}
}
