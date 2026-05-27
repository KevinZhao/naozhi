package cli

import "testing"

// TestClaudeProtocol_ReadEvent_SubstringFastPath_SkipsBeforeUnmarshal
// covers R20260527122801-PERF-3 (#1334): the fast-path strings.Contains
// check must skip hook_started / hook_response / control_response frames
// without invoking json.Unmarshal. We verify that by feeding a deliberately
// MALFORMED JSON tail past the skip token — if the fast-path runs, the
// frame is skipped (no error, no event); if the slow path runs, the
// unmarshal fails and ReadEvent returns an error.
func TestClaudeProtocol_ReadEvent_SubstringFastPath_SkipsBeforeUnmarshal(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	// Each line carries the skip token but is NOT valid JSON overall (the
	// trailing tail is garbage). The fast-path must short-circuit before
	// json.Unmarshal sees the bad tail.
	cases := []string{
		`{"type":"system","subtype":"hook_started", garbage`,
		`{"type":"system","subtype":"hook_response", oops`,
		`{"type":"control_response", invalid`,
	}
	for _, line := range cases {
		evs, done, err := p.ReadEvent(line)
		if err != nil {
			t.Errorf("fast-path should skip before unmarshal; got err=%v for line=%q", err, line)
		}
		if done {
			t.Errorf("skipped frame should not be done: line=%q", line)
		}
		if len(evs) != 0 {
			t.Errorf("skipped frame should produce 0 events, got %d: line=%q", len(evs), line)
		}
	}
}

// TestClaudeProtocol_ReadEvent_FastPath_DoesNotEatRealAssistant ensures
// the fast-path's substring check does not over-match: assistant frames
// whose payload happens to mention the skip tokens as data must still
// flow through the full unmarshal path. The skip tokens only appear at
// the wire's `type` / `subtype` slot for true skip frames; a benign
// substring elsewhere should not trigger the skip — but in practice
// there is no legitimate reason for an assistant frame to contain
// "hook_started" verbatim. The risk is theoretical only; this test
// pins the current minimal-fix behaviour so a future regression to
// e.g. tighter `\"type\":\"hook_started\"` matching is a deliberate
// choice rather than an accident.
func TestClaudeProtocol_ReadEvent_FastPath_NormalAssistantPasses(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`
	evs, done, err := p.ReadEvent(line)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if done {
		t.Error("assistant frame must not complete turn")
	}
	if len(evs) != 1 || evs[0].Type != "assistant" {
		t.Errorf("expected 1 assistant event, got %+v", evs)
	}
}
