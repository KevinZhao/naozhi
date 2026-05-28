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
// flow through the full unmarshal path. R260528-GO-16 anchored the
// match to the JSON key context (`:"hook_started"` etc.) so assistant
// text containing the literal magic word no longer triggers a false
// skip — see TestReadEventFastPathFalsePositive below.
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

// TestReadEventFastPathFalsePositive (R260528-GO-16) pins the JSON-key
// anchoring of the substring fast-path. An assistant frame whose text
// content contains the literal token "hook_started" / "hook_response" /
// "control_response" must NOT be skipped — only the wire-level
// `"subtype":"hook_started"` / `"type":"control_response"` keys count.
// Pre-fix, strings.Contains(line, `"hook_started"`) matched the quoted
// substring inside the message body and silently dropped the user's
// turn; the fix anchored the check to `:"hook_started"` so the colon
// boundary forces a JSON-key match.
func TestReadEventFastPathFalsePositive(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	cases := []string{
		// Assistant text quoting the magic word verbatim.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the \"hook_started\" event fires when ..."}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"see \"hook_response\" doc"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"matches \"control_response\" semantics"}]}}`,
		// User echo containing the token (e.g. asking the agent about it).
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"what is \"hook_started\"?"}]}}`,
	}
	for _, line := range cases {
		evs, done, err := p.ReadEvent(line)
		if err != nil {
			t.Errorf("ReadEvent err=%v for line=%q", err, line)
			continue
		}
		if done {
			t.Errorf("non-result frame should not be done: line=%q", line)
		}
		if len(evs) == 0 {
			t.Errorf("frame must NOT be skipped — token is in user/assistant text, not a JSON key. line=%q", line)
		}
	}
}
