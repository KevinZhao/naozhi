package cli

import (
	"encoding/json"
	"log/slog"
	"testing"
)

// TestDispatchProtocolEvent_InitCapturesLiveVersion pins R20260612-live-version:
// a system/init frame carrying claude_code_version must populate the process's
// LiveVersion() so the dashboard reflects the binary THIS process exec'd rather
// than the spawn-time Wrapper.CLIVersion (stale after a host claude upgrade).
func TestDispatchProtocolEvent_InitCapturesLiveVersion(t *testing.T) {
	p := &Process{
		eventLog: NewEventLog(8),
		eventCh:  make(chan Event, 4),
		killCh:   make(chan struct{}),
	}

	if got := p.LiveVersion(); got != "" {
		t.Fatalf("LiveVersion before init = %q, want empty", got)
	}

	ev := Event{Type: "system", SubType: "init", SessionID: "s1", ClaudeCodeVersion: "2.1.174"}
	p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))

	if got := p.LiveVersion(); got != "2.1.174" {
		t.Fatalf("LiveVersion after init = %q, want 2.1.174", got)
	}
}

// TestDispatchProtocolEvent_InitWithoutVersionKeepsEmpty guards the field-absent
// case: an init frame from a CLI build that drops claude_code_version (or the
// ACP path, which never sets it) must not clobber LiveVersion to a junk value —
// it stays empty so the session layer falls back to the spawn-time version.
func TestDispatchProtocolEvent_InitWithoutVersionKeepsEmpty(t *testing.T) {
	p := &Process{
		eventLog: NewEventLog(8),
		eventCh:  make(chan Event, 4),
		killCh:   make(chan struct{}),
	}

	ev := Event{Type: "system", SubType: "init", SessionID: "s1"}
	p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))

	if got := p.LiveVersion(); got != "" {
		t.Fatalf("LiveVersion after version-less init = %q, want empty", got)
	}
}

// TestSetLiveVersion_IgnoresEmpty pins that setLiveVersion never overwrites a
// previously captured version with "" — a defensive guard mirroring the init
// hook's non-empty gate.
func TestSetLiveVersion_IgnoresEmpty(t *testing.T) {
	p := &Process{}
	p.setLiveVersion("2.1.174")
	p.setLiveVersion("")
	if got := p.LiveVersion(); got != "2.1.174" {
		t.Fatalf("LiveVersion = %q after empty set, want 2.1.174 retained", got)
	}
}

// TestEvent_ClaudeCodeVersionWireDecode pins the JSON tag so a CLI protocol
// change to the field name is caught by CI rather than silently disabling the
// live-version capture.
func TestEvent_ClaudeCodeVersionWireDecode(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"type":"system","subtype":"init","claude_code_version":"2.1.174"}`), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.ClaudeCodeVersion != "2.1.174" {
		t.Fatalf("ClaudeCodeVersion = %q, want 2.1.174", ev.ClaudeCodeVersion)
	}
}
