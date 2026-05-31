package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/sysession"
)

func TestMapSysessionTrigger(t *testing.T) {
	cases := []struct {
		in   sysession.DaemonTriggerKind
		want runtelemetry.TriggerKind
	}{
		{sysession.DaemonTriggerScheduled, runtelemetry.TriggerScheduled},
		{sysession.DaemonTriggerManual, runtelemetry.TriggerManual},
		{sysession.DaemonTriggerKind("bogus"), runtelemetry.TriggerScheduled},
		{sysession.DaemonTriggerKind(""), runtelemetry.TriggerScheduled},
	}
	for _, c := range cases {
		if got := mapSysessionTrigger(c.in); got != c.want {
			t.Errorf("mapSysessionTrigger(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMapSysessionRunState(t *testing.T) {
	cases := []struct {
		in   sysession.DaemonRunState
		want runtelemetry.RunState
	}{
		{sysession.DaemonRunSucceeded, runtelemetry.RunStateSucceeded},
		{sysession.DaemonRunFailed, runtelemetry.RunStateFailed},
		{sysession.DaemonRunTimedOut, runtelemetry.RunStateTimedOut},
		{sysession.DaemonRunCanceled, runtelemetry.RunStateCanceled},
		{sysession.DaemonRunState("bogus"), runtelemetry.RunStateFailed},
		{sysession.DaemonRunState(""), runtelemetry.RunStateFailed},
	}
	for _, c := range cases {
		if got := mapSysessionRunState(c.in); got != c.want {
			t.Errorf("mapSysessionRunState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMapSysessionErrorClass(t *testing.T) {
	cases := []struct {
		in   sysession.DaemonErrorClass
		want runtelemetry.ErrorClass
	}{
		{sysession.DaemonErrorClassNone, runtelemetry.ErrClassNone},
		{sysession.DaemonErrorClassValidation, runtelemetry.ErrClassSysessionValidation},
		{sysession.DaemonErrorClassUpstream, runtelemetry.ErrClassSysessionUpstream},
		// The load-bearing normalisation: sysession "timeout" must map to
		// the runtelemetry canonical "deadline_exceeded", which dashboard.js
		// renders as "超时". A bare cast would have leaked "timeout".
		{sysession.DaemonErrorClassTimeout, runtelemetry.ErrClassDeadlineExceeded},
		{sysession.DaemonErrorClassPanic, runtelemetry.ErrClassPanic},
		{sysession.DaemonErrorClassCanceled, runtelemetry.ErrClassCanceled},
		{sysession.DaemonErrorClass("bogus"), runtelemetry.ErrClassNone},
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
	if string(sysession.DaemonErrorClassTimeout) == string(runtelemetry.ErrClassDeadlineExceeded) {
		t.Skip("wire strings converged; bare cast would now be safe and this guard is moot")
	}
	bare := runtelemetry.ErrorClass(sysession.DaemonErrorClassTimeout)
	mapped := mapSysessionErrorClass(sysession.DaemonErrorClassTimeout)
	if bare == mapped {
		t.Fatalf("expected map to differ from bare cast: bare=%q mapped=%q", bare, mapped)
	}
	if mapped != runtelemetry.ErrClassDeadlineExceeded {
		t.Fatalf("timeout must normalise to deadline_exceeded, got %q", mapped)
	}
}
