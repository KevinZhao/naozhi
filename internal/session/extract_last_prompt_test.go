package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestExtractLastPromptFromProcess_UsesLastN pins the R20260603000023-PERF-12
// optimisation: extractLastPromptFromProcess must populate lastPrompt /
// lastActivity / lastResponse from the tail of the process event log, not a
// full-ring copy. The test feeds more than extractLastPromptScanN entries into
// the process log (older entries beyond the window), with the three target
// entries near the tail, and verifies that all three atomics are correctly
// populated.
func TestExtractLastPromptFromProcess_UsesLastN(t *testing.T) {
	t.Parallel()
	proc := NewTestProcess()

	// Fill entries well beyond extractLastPromptScanN so a full-copy path would
	// read them, but a LastN(extractLastPromptScanN) tail would not.
	for i := 0; i < extractLastPromptScanN*2; i++ {
		proc.EventLog.Append(cli.EventEntry{
			Time:    int64(i + 1),
			Type:    "text",
			Summary: "old-response",
		})
	}

	// Append the target entries near the tail (within LastN window).
	base := int64(extractLastPromptScanN * 2)
	proc.EventLog.Append(cli.EventEntry{Time: base + 1, Type: "user", Summary: "my-prompt"})
	proc.EventLog.Append(cli.EventEntry{Time: base + 2, Type: "tool_use", Summary: "my-activity"})
	proc.EventLog.Append(cli.EventEntry{Time: base + 3, Type: "text", Summary: "my-response"})

	s := &ManagedSession{key: "test:extract"}
	s.storeProcess(proc)

	s.extractLastPromptFromProcess()

	if got := loadAtomicString(&s.lastPrompt); got != "my-prompt" {
		t.Errorf("lastPrompt = %q, want %q", got, "my-prompt")
	}
	if got := loadAtomicString(&s.lastActivity); got != "my-activity" {
		t.Errorf("lastActivity = %q, want %q", got, "my-activity")
	}
	if got := loadAtomicString(&s.lastResponse); got != "my-response" {
		t.Errorf("lastResponse = %q, want %q", got, "my-response")
	}
}

// TestExtractLastPromptFromProcess_DoesNotOverwrite ensures that already-set
// atomics are not clobbered when extractLastPromptFromProcess runs.
func TestExtractLastPromptFromProcess_DoesNotOverwrite(t *testing.T) {
	t.Parallel()
	proc := NewTestProcess()
	proc.EventLog.Append(cli.EventEntry{Time: 1, Type: "user", Summary: "proc-prompt"})
	proc.EventLog.Append(cli.EventEntry{Time: 2, Type: "tool_use", Summary: "proc-activity"})
	proc.EventLog.Append(cli.EventEntry{Time: 3, Type: "text", Summary: "proc-response"})

	s := &ManagedSession{key: "test:extract-nowrit"}
	storeAtomicString(&s.lastPrompt, "pre-set")
	storeAtomicString(&s.lastActivity, "pre-set-act")
	storeAtomicString(&s.lastResponse, "pre-set-resp")
	s.storeProcess(proc)

	s.extractLastPromptFromProcess()

	if got := loadAtomicString(&s.lastPrompt); got != "pre-set" {
		t.Errorf("lastPrompt overwritten: got %q, want %q", got, "pre-set")
	}
	if got := loadAtomicString(&s.lastActivity); got != "pre-set-act" {
		t.Errorf("lastActivity overwritten: got %q, want %q", got, "pre-set-act")
	}
	if got := loadAtomicString(&s.lastResponse); got != "pre-set-resp" {
		t.Errorf("lastResponse overwritten: got %q, want %q", got, "pre-set-resp")
	}
}

// TestExtractLastPromptFromProcess_NilProcess verifies that a nil proc is a
// no-op and does not panic.
func TestExtractLastPromptFromProcess_NilProcess(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:nil-proc"}
	s.extractLastPromptFromProcess() // must not panic
	if got := loadAtomicString(&s.lastPrompt); got != "" {
		t.Errorf("lastPrompt = %q after nil proc extract, want empty", got)
	}
}
