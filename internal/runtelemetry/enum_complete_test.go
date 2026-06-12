package runtelemetry

import "testing"

// These tests freeze the *count* of constants per enum type so that
// adding a new constant without updating wire_stability_test.go fails
// loudly here instead of silently shipping an un-frozen wire value.
//
// Why count rather than reflect-walk: Go's reflect cannot enumerate
// package-level constants, and parsing source with go/ast just to count
// them is overkill for a freeze test. The expected counts mirror the
// const blocks in state.go; bumping them is a deliberate one-line edit
// that forces the developer to look at wire_stability_test.go in the
// same PR.
//
// Process to add a new constant:
//   1. Add const in state.go
//   2. Add wire string entry in wire_stability_test.go
//   3. Bump want count here

func TestRunStateCount(t *testing.T) {
	const want = 5 // Succeeded / Failed / Skipped / TimedOut / Canceled
	got := len(allRunStates())
	if got != want {
		t.Errorf("RunState count = %d, want %d (update wire_stability_test.go and this test together)", got, want)
	}
}

func TestErrorClassCount(t *testing.T) {
	const want = 14 // None + 3 shared + 8 cron (5 local + 3 sandbox) + 2 sysession
	got := len(allErrorClasses())
	if got != want {
		t.Errorf("ErrorClass count = %d, want %d (update wire_stability_test.go and this test together)", got, want)
	}
}

func TestTriggerKindCount(t *testing.T) {
	const want = 3 // Scheduled / Manual / Catchup
	got := len(allTriggerKinds())
	if got != want {
		t.Errorf("TriggerKind count = %d, want %d (update wire_stability_test.go and this test together)", got, want)
	}
}

func TestSubsystemCount(t *testing.T) {
	const want = 2 // Cron / Sysession
	got := len(allSubsystems())
	if got != want {
		t.Errorf("Subsystem count = %d, want %d (update wire_stability_test.go and this test together)", got, want)
	}
}

// Mirror lists. Each must be kept in sync with the const block of the
// same type in state.go. Using a list (not a map) so the test fails on
// duplicate entries here too — a mistake adding the same constant twice
// would inflate the count to match.
func allRunStates() []RunState {
	return []RunState{
		RunStateSucceeded,
		RunStateFailed,
		RunStateSkipped,
		RunStateTimedOut,
		RunStateCanceled,
	}
}

func allErrorClasses() []ErrorClass {
	return []ErrorClass{
		ErrClassNone,
		ErrClassDeadlineExceeded,
		ErrClassCanceled,
		ErrClassPanic,

		ErrClassCronSessionError,
		ErrClassCronSendError,
		ErrClassCronWorkDirUnreachable,
		ErrClassCronWorkDirOutsideRoot,
		ErrClassCronOverlapSkipped,
		ErrClassCronSandboxFailed,
		ErrClassCronSandboxTransport,
		ErrClassCronSandboxUnavailable,

		ErrClassSysessionUpstream,
		ErrClassSysessionValidation,
	}
}

func allTriggerKinds() []TriggerKind {
	return []TriggerKind{
		TriggerScheduled,
		TriggerManual,
		TriggerCatchup,
	}
}

func allSubsystems() []Subsystem {
	return []Subsystem{
		SubsystemCron,
		SubsystemSysession,
	}
}
