package sysession

import (
	"testing"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

func TestMapSysessionTrigger(t *testing.T) {
	cases := []struct {
		in   DaemonTriggerKind
		want runtelemetry.TriggerKind
	}{
		{DaemonTriggerScheduled, runtelemetry.TriggerScheduled},
		{DaemonTriggerManual, runtelemetry.TriggerManual},
		{DaemonTriggerKind("bogus"), runtelemetry.TriggerScheduled},
		{DaemonTriggerKind(""), runtelemetry.TriggerScheduled},
	}
	for _, c := range cases {
		if got := mapSysessionTrigger(c.in); got != c.want {
			t.Errorf("mapSysessionTrigger(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMapSysessionRunState(t *testing.T) {
	cases := []struct {
		in   DaemonRunState
		want runtelemetry.RunState
	}{
		{DaemonRunSucceeded, runtelemetry.RunStateSucceeded},
		{DaemonRunFailed, runtelemetry.RunStateFailed},
		{DaemonRunTimedOut, runtelemetry.RunStateTimedOut},
		{DaemonRunCanceled, runtelemetry.RunStateCanceled},
		{DaemonRunState("bogus"), runtelemetry.RunStateFailed},
		{DaemonRunState(""), runtelemetry.RunStateFailed},
	}
	for _, c := range cases {
		if got := mapSysessionRunState(c.in); got != c.want {
			t.Errorf("mapSysessionRunState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMapSysessionErrorClass(t *testing.T) {
	cases := []struct {
		in   DaemonErrorClass
		want runtelemetry.ErrorClass
	}{
		{DaemonErrorClassNone, runtelemetry.ErrClassNone},
		{DaemonErrorClassValidation, runtelemetry.ErrClassSysessionValidation},
		{DaemonErrorClassUpstream, runtelemetry.ErrClassSysessionUpstream},
		// The load-bearing normalisation: sysession "timeout" must map to
		// the runtelemetry canonical "deadline_exceeded", which dashboard.js
		// renders as "超时". A bare cast would have leaked "timeout".
		{DaemonErrorClassTimeout, runtelemetry.ErrClassDeadlineExceeded},
		{DaemonErrorClassPanic, runtelemetry.ErrClassPanic},
		{DaemonErrorClassCanceled, runtelemetry.ErrClassCanceled},
		{DaemonErrorClass("bogus"), runtelemetry.ErrClassNone},
	}
	for _, c := range cases {
		if got := mapSysessionErrorClass(c.in); got != c.want {
			t.Errorf("mapSysessionErrorClass(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMapSysessionErrorClassTimeoutWireDivergence pins the exact symptom
// the issue (#1486) describes: sysession and runtelemetry use different
// wire strings for "timed out", so the previous bare cast produced a
// runtelemetry.ErrorClass whose string was NOT a defined runtelemetry
// constant. This guards against anyone "simplifying" the map back to a
// passthrough.
func TestMapSysessionErrorClassTimeoutWireDivergence(t *testing.T) {
	if string(DaemonErrorClassTimeout) == string(runtelemetry.ErrClassDeadlineExceeded) {
		t.Skip("wire strings converged; bare cast would now be safe and this guard is moot")
	}
	bare := runtelemetry.ErrorClass(DaemonErrorClassTimeout)
	mapped := mapSysessionErrorClass(DaemonErrorClassTimeout)
	if bare == mapped {
		t.Fatalf("expected map to differ from bare cast: bare=%q mapped=%q", bare, mapped)
	}
	if mapped != runtelemetry.ErrClassDeadlineExceeded {
		t.Fatalf("timeout must normalise to deadline_exceeded, got %q", mapped)
	}
}
