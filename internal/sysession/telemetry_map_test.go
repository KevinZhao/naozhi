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
