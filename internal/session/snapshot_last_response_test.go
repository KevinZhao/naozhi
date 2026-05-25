package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSnapshot_LastResponse_LiveProcess verifies the live-process branch of
// Snapshot pulls the response preview from proc.LastResponseSummary so the
// dashboard sidebar's R110-P1 dim second-line preview reflects the freshest
// assistant text, not a stale s.lastResponse cache value.
func TestSnapshot_LastResponse_LiveProcess(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}
	proc := NewTestProcess()
	s.storeProcess(proc)

	// No text appended yet → empty preview (omitempty hides the line).
	if got := s.Snapshot().LastResponse; got != "" {
		t.Fatalf("pre-text snapshot LastResponse = %q, want \"\"", got)
	}

	// Append a user prompt + assistant text via the canonical event path.
	proc.EventLog.Append(cli.EventEntry{Type: "user", Summary: "hi"})
	proc.EventLog.Append(cli.EventEntry{Type: "text", Summary: "hello there"})

	if got, want := s.Snapshot().LastResponse, "hello there"; got != want {
		t.Errorf("after text Snapshot.LastResponse = %q, want %q", got, want)
	}
}

// TestSnapshot_LastResponse_NoProcess covers the suspended/dead-session
// fallback: when proc is nil the cached s.lastResponse atomic feeds the
// snapshot. This mirrors the LastPrompt resolution path so the sidebar
// preview survives shim reconnect / restart windows.
func TestSnapshot_LastResponse_NoProcess(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}

	// Cache pre-seeded (e.g. by extractLastPromptFromProcess after replay).
	storeAtomicString(&s.lastResponse, "cached reply")

	if got, want := s.Snapshot().LastResponse, "cached reply"; got != want {
		t.Errorf("no-proc Snapshot.LastResponse = %q, want %q (cache fallback)", got, want)
	}
}

// TestSnapshot_LastResponse_LiveOverridesCache locks the priority order:
// when both the live process AND the cached field carry a value, the live
// process value wins. Without this the dashboard would show stale replies
// after the shim re-attached and produced a fresh assistant turn.
func TestSnapshot_LastResponse_LiveOverridesCache(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}
	storeAtomicString(&s.lastResponse, "old cached reply")

	proc := NewTestProcess()
	proc.EventLog.Append(cli.EventEntry{Type: "text", Summary: "fresh reply"})
	s.storeProcess(proc)

	if got, want := s.Snapshot().LastResponse, "fresh reply"; got != want {
		t.Errorf("live-vs-cache Snapshot.LastResponse = %q, want %q (live must win)", got, want)
	}
}

// TestScanLastSummaries_Response covers the InjectHistory / shim-reconnect
// replay path: scanLastSummaries must surface the most recent "text" entry
// alongside prompt + activity so post-restart sessions populate the sidebar
// preview without waiting for a fresh live event.
func TestScanLastSummaries_Response(t *testing.T) {
	t.Parallel()

	entries := []cli.EventEntry{
		{Type: "user", Summary: "first question"},
		{Type: "thinking", Summary: "..."},
		{Type: "text", Summary: "first answer"},
		{Type: "user", Summary: "follow up"},
		{Type: "tool_use", Summary: "Read"},
		{Type: "text", Summary: "final answer"},
	}

	prompt, activity, response := scanLastSummaries(entries)
	if prompt != "follow up" {
		t.Errorf("prompt = %q, want \"follow up\"", prompt)
	}
	if activity != "Read" {
		t.Errorf("activity = %q, want \"Read\"", activity)
	}
	if response != "final answer" {
		t.Errorf("response = %q, want \"final answer\"", response)
	}
}

// TestScanLastSummaries_NoResponse confirms the contract for histories with
// no assistant text yet (tool-only turn): response stays empty so the
// sidebar's omitempty path hides the dim preview line rather than echoing
// stale data.
func TestScanLastSummaries_NoResponse(t *testing.T) {
	t.Parallel()

	entries := []cli.EventEntry{
		{Type: "user", Summary: "do work"},
		{Type: "tool_use", Summary: "Bash"},
		{Type: "thinking", Summary: "..."},
	}
	prompt, activity, response := scanLastSummaries(entries)
	if prompt != "do work" {
		t.Errorf("prompt = %q, want \"do work\"", prompt)
	}
	if activity == "" {
		t.Errorf("activity should be set, got empty")
	}
	if response != "" {
		t.Errorf("response = %q, want \"\" (no assistant text in history)", response)
	}
}
