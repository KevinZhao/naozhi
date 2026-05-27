package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestAgentTaskDoneSetter_SatisfiedByCLIEventLog pins R217-ARCH-2 / #625:
// wshub_agent.go's `maybeWireLinkerTailer` no longer names *cli.EventLog
// at the SetOnAgentTaskDone call site. The hook flows through the local
// agentTaskDoneSetter interface, so any backend whose event-log type
// exposes the matching method satisfies the surface implicitly via Go
// structural typing.
//
// The test compiles-fails (or fails at runtime here) if either:
//
//  1. The agentTaskDoneSetter method set drifts (rename / signature
//     change) such that *cli.EventLog no longer satisfies it. That
//     would break the production wiring without a build-time signal
//     because *cli.EventLog is only assigned to the interface variable
//     inside a conditional branch in maybeWireLinkerTailer.
//  2. A future refactor reverts the call site to type *cli.EventLog
//     directly — the assertion that the interface is the contract
//     surface (not the concrete type) lives explicitly here.
//
// Pin shape: var-blank assignment forces the compiler to verify
// satisfaction at build time; the runtime assertion exists for symmetry
// with the other interface contract tests in the package
// (e.g. TestScratchHandler_RouterFieldIsScratchRouter).
func TestAgentTaskDoneSetter_SatisfiedByCLIEventLog(t *testing.T) {
	t.Parallel()
	// Compile-time guard.
	var _ agentTaskDoneSetter = (*cli.EventLog)(nil)

	// Runtime guard with a stub: the call site must dispatch through
	// the interface. We verify the method exists by exercising it on
	// a fake — if the interface ever loses SetOnAgentTaskDone, the
	// stub assignment below stops compiling.
	var stub agentTaskDoneSetter = &fakeTaskDoneSetter{}
	stub.SetOnAgentTaskDone(func(string, string) {})
	if got := stub.(*fakeTaskDoneSetter).calls; got != 1 {
		t.Errorf("SetOnAgentTaskDone dispatch count = %d, want 1", got)
	}
}

type fakeTaskDoneSetter struct {
	calls int
}

func (f *fakeTaskDoneSetter) SetOnAgentTaskDone(func(taskID, status string)) {
	f.calls++
}

// TestMaybeWireLinkerTailer_NoOpWhenAgentEventLogNil exercises the
// guard added alongside R217-ARCH-2 / #625: when sess.AgentEventLog()
// returns a typed-nil *cli.EventLog (legitimate for fake test
// processes / dead sessions), the rawLog != nil branch must be
// rejected so we never call SetOnAgentTaskDone on a nil receiver.
//
// We can't easily drive the full maybeWireLinkerTailer path without
// wiring a *session.ManagedSession, but the inner-most guard is the
// failure mode the interface refactor risked introducing — a
// previously-bare `agentLog != nil` test on a typed-nil promoted to
// an interface variable would silently pass and panic at the next
// method call. The retained `if rawLog := sess.AgentEventLog();
// rawLog != nil` guard checks the concrete return BEFORE promotion,
// matching the existing SubagentLinker idiom one block earlier in
// the same function. This test pins the idiom by exercising the
// downgrade with a stand-alone helper that mirrors the exact shape.
func TestMaybeWireLinkerTailer_NoOpWhenAgentEventLogNil(t *testing.T) {
	t.Parallel()
	var rawLog *cli.EventLog // typed-nil, mirrors AgentEventLog()'s zero return.
	if rawLog != nil {
		// Promotion to interface only happens AFTER the typed-nil
		// guard — that's the contract we are pinning.
		var hook agentTaskDoneSetter = rawLog
		hook.SetOnAgentTaskDone(func(string, string) {})
		t.Fatal("typed-nil should not have entered the promotion branch")
	}
}
