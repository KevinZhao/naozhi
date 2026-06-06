package sysession

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// TestDaemonEnumsAliasRuntelemetry pins the R260528-ARCH-2/#1363 unification:
// DaemonRunState / DaemonTriggerKind are type aliases to the runtelemetry
// single-source vocabulary, and the sysession-local constants resolve to the
// same wire values. A regression that re-forks these into a private string
// type (or drifts the values) fails here at compile/assert time.
func TestDaemonEnumsAliasRuntelemetry(t *testing.T) {
	t.Parallel()
	// Compile-time alias proof: assigning across the alias boundary in both
	// directions only type-checks when the types are identical.
	var _ runtelemetry.RunState = DaemonRunSucceeded
	var _ DaemonRunState = runtelemetry.RunStateFailed
	var _ runtelemetry.TriggerKind = DaemonTriggerScheduled
	var _ DaemonTriggerKind = runtelemetry.TriggerManual

	stateWire := map[DaemonRunState]runtelemetry.RunState{
		DaemonRunSucceeded: runtelemetry.RunStateSucceeded,
		DaemonRunFailed:    runtelemetry.RunStateFailed,
		DaemonRunTimedOut:  runtelemetry.RunStateTimedOut,
		DaemonRunCanceled:  runtelemetry.RunStateCanceled,
	}
	for got, want := range stateWire {
		if got != want {
			t.Errorf("run state %q != runtelemetry %q", got, want)
		}
	}

	triggerWire := map[DaemonTriggerKind]runtelemetry.TriggerKind{
		DaemonTriggerScheduled: runtelemetry.TriggerScheduled,
		DaemonTriggerManual:    runtelemetry.TriggerManual,
	}
	for got, want := range triggerWire {
		if got != want {
			t.Errorf("trigger %q != runtelemetry %q", got, want)
		}
	}

	// ErrorClass is aliased too (R260528-ARCH-18 / #1379); 5/6 values map
	// 1:1 onto runtelemetry, the 6th (Timeout) keeps its divergent
	// pre-merge wire string by design — server.mapSysessionErrorClass
	// normalises it to deadline_exceeded before broadcast.
	var _ runtelemetry.ErrorClass = DaemonErrorClassNone
	var _ DaemonErrorClass = runtelemetry.ErrClassPanic
	classWire := map[DaemonErrorClass]runtelemetry.ErrorClass{
		DaemonErrorClassNone:       runtelemetry.ErrClassNone,
		DaemonErrorClassValidation: runtelemetry.ErrClassSysessionValidation,
		DaemonErrorClassUpstream:   runtelemetry.ErrClassSysessionUpstream,
		DaemonErrorClassPanic:      runtelemetry.ErrClassPanic,
		DaemonErrorClassCanceled:   runtelemetry.ErrClassCanceled,
	}
	for got, want := range classWire {
		if got != want {
			t.Errorf("error class %q != runtelemetry %q", got, want)
		}
	}
	if DaemonErrorClassTimeout != "timeout" {
		t.Errorf("DaemonErrorClassTimeout = %q, want stable pre-merge wire %q", DaemonErrorClassTimeout, "timeout")
	}
	if DaemonErrorClass(DaemonErrorClassTimeout) == runtelemetry.ErrClassDeadlineExceeded {
		t.Error("Timeout must NOT equal canonical deadline_exceeded; server map owns that normalisation")
	}
}

func TestClassifyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		isPanic   bool
		wantState DaemonRunState
		wantClass DaemonErrorClass
	}{
		{"nil error → success", nil, false, DaemonRunSucceeded, DaemonErrorClassNone},
		{"panic", fmt.Errorf("panicked: x"), true, DaemonRunFailed, DaemonErrorClassPanic},
		{"deadline → timeout", fmt.Errorf("wrap: %w", context.DeadlineExceeded), false, DaemonRunTimedOut, DaemonErrorClassTimeout},
		{"canceled → canceled", fmt.Errorf("wrap: %w", context.Canceled), false, DaemonRunCanceled, DaemonErrorClassCanceled},
		{"validation sentinel → validation", fmt.Errorf("title rejected: %w", ErrValidation), false, DaemonRunFailed, DaemonErrorClassValidation},
		{"random error → upstream", errors.New("kaboom"), false, DaemonRunFailed, DaemonErrorClassUpstream},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			gotState, gotClass := classifyError(c.err, c.isPanic)
			if gotState != c.wantState {
				t.Errorf("state = %q, want %q", gotState, c.wantState)
			}
			if gotClass != c.wantClass {
				t.Errorf("class = %q, want %q", gotClass, c.wantClass)
			}
		})
	}
}

func TestNewRunID_Format(t *testing.T) {
	t.Parallel()
	a, b := newRunID(), newRunID()
	if a == b {
		t.Errorf("newRunID returned identical values: %q", a)
	}
	if len(a) == 0 {
		t.Error("newRunID returned empty string")
	}
}
