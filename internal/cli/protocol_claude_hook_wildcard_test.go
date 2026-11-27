package cli

import "testing"

// TestClaudeProtocol_ReadEvent_HookWildcard_SkipsUnknownSubtype pins
// R250531-PERF-9: after merging hook_started and hook_response into the
// shared `{"type":"system","subtype":"hook_` line prefix, any future hook_*
// subtype emitted by the CLI is also fast-path skipped — which is the correct
// conservative behaviour (unknown hook events are never useful to the event
// consumer).
func TestClaudeProtocol_ReadEvent_HookWildcard_SkipsUnknownSubtype(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}

	// A hypothetical future hook type the CLI might emit.
	line := `{"type":"system","subtype":"hook_future_unknown", invalid_tail`
	evs, done, err := p.ReadEvent(line)
	if err != nil {
		t.Errorf("fast-path should skip hook_future_unknown; got err=%v", err)
	}
	if done {
		t.Error("skipped frame must not be done")
	}
	if len(evs) != 0 {
		t.Errorf("skipped frame must produce 0 events, got %d", len(evs))
	}
}
