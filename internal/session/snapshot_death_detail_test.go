package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

// A CLI that exited non-zero shows its cause next to death_reason; the typed
// exit error still books the process's own reason.
func TestSnapshot_DeathDetail(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "test:direct:alice:general"}
	if got := s.Snapshot().DeathDetail; got != "" {
		t.Fatalf("no process: DeathDetail = %q, want empty", got)
	}
	proc := NewTestProcess()
	proc.DeathReasonVal = "cli_exited_code_1"
	proc.DeathDetailVal = "No conversation found with session ID: abc"
	s.storeProcess(proc)
	s.mapSendError(proc, &clierr.ProcessExitedError{Code: 1, Class: clierr.ExitResumeNotFound})

	snap := s.Snapshot()
	if snap.DeathReason != "cli_exited_code_1" || snap.DeathDetail != proc.DeathDetailVal {
		t.Fatalf("snapshot death = (%q, %q), want (cli_exited_code_1, %q)", snap.DeathReason, snap.DeathDetail, proc.DeathDetailVal)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"death_detail":"No conversation found with session ID: abc"`) {
		t.Errorf("snapshot JSON lacks death_detail: %s", raw)
	}
}
