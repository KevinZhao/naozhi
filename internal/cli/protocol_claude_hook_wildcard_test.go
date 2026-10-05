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

// TestClaudeProtocol_ReadEvent_HookWildcard_EmbeddedInText_NoFalsePositive
// ensures an assistant frame whose text quotes `:"hook_` is delivered: only a
// line that starts with the system hook prefix is fast-path skipped.
func TestClaudeProtocol_ReadEvent_HookWildcard_EmbeddedInText_NoFalsePositive(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"consider the pattern ` + "`" + `{\"key\":\"hook_` + `val\"}` + "`" + `"}]}}`
	evs, done, err := p.ReadEvent(line)
	if err != nil || done {
		t.Fatalf("ReadEvent = (_, %v, %v), want delivered", done, err)
	}
	if len(evs) != 1 || evs[0].Type != "assistant" {
		t.Fatalf("got %+v, want one assistant event", evs)
	}
}
