package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestScanLastSummaries_ResponseStripsMarkdown pins #2435 item 1 on the
// replay-seeded path: after shim reconnect / restart the sidebar preview
// comes from scanLastSummaries, which must strip markdown exactly like the
// live EventLog store so the card does not flip between "判分" and "## 判分".
func TestScanLastSummaries_ResponseStripsMarkdown(t *testing.T) {
	t.Parallel()
	_, _, response := scanLastSummaries([]cli.EventEntry{
		{Type: "user", Summary: "打分"},
		{Type: "text", Summary: "## 判分\n**≈129.5/150** `b466411`"},
	})
	if want := "判分 ≈129.5/150 b466411"; response != want {
		t.Errorf("scanLastSummaries response = %q, want %q", response, want)
	}
}

// TestSnapshot_LastResponse_LiveStripsMarkdown covers the end-to-end live
// branch: text appended through the canonical event path surfaces in
// SessionSnapshot.LastResponse without markdown notation.
func TestSnapshot_LastResponse_LiveStripsMarkdown(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}
	proc := NewTestProcess()
	s.storeProcess(proc)
	proc.EventLog.Append(cli.EventEntry{Type: "text", Summary: "### 结论\n> **完成**"})
	if got, want := s.Snapshot().LastResponse, "结论 完成"; got != want {
		t.Errorf("Snapshot.LastResponse = %q, want %q", got, want)
	}
}
