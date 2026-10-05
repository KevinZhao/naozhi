package cli

import "testing"

// TestClaudeProtocol_ReadEvent_SubstringFastPath_SkipsBeforeUnmarshal
// covers R20260527122801-PERF-3 (#1334): the line-prefix fast path must
// skip hook_started / hook_response / control_response frames without
// invoking json.Unmarshal. We verify that by feeding a deliberately
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

// TestClaudeProtocol_ReadEvent_FastPath_NormalAssistantPasses ensures a plain
// assistant frame flows through the full unmarshal path.
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

// TestReadEventFastPathFalsePositive (R260528-GO-16): an assistant or user
// frame whose text quotes "hook_started" / "hook_response" /
// "control_response" must NOT be skipped — only a line that starts with the
// system hook or control_response prefix is.
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

// TestReadEvent_HookTokenInStructuredValue_Delivered pins the line-start
// anchoring of the fast path. Workflow labels, tool_use inputs and structured
// tool_use_result objects are unescaped JSON, so `:"hook_` and
// `:"control_response"` appear in them byte-for-byte; the frame must still be
// delivered rather than skipped.
func TestReadEvent_HookTokenInStructuredValue_Delivered(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	cases := []struct {
		name, line, wantType, wantSub string
	}{
		{
			name:     "workflow snapshot label",
			line:     `{"type":"system","subtype":"task_progress","task_id":"w1","workflow_progress":[{"type":"workflow_agent","index":1,"label":"hook_tests","state":"start"}]}`,
			wantType: "system", wantSub: "task_progress",
		},
		{
			name:     "tool_use input with hook subtype",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__x__emit","input":{"subtype":"hook_started"}}]}}`,
			wantType: "assistant",
		},
		{
			name:     "tool_use input with control_response type",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__x__emit","input":{"type":"control_response"}}]}}`,
			wantType: "assistant",
		},
		{
			name:     "structured tool_use_result",
			line:     `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},"tool_use_result":{"subtype":"hook_response","type":"control_response"}}`,
			wantType: "user",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs, done, err := p.ReadEvent(tc.line)
			if err != nil || done {
				t.Fatalf("ReadEvent = (_, %v, %v), want delivered", done, err)
			}
			if len(evs) != 1 || evs[0].Type != tc.wantType || evs[0].SubType != tc.wantSub {
				t.Fatalf("got %+v, want one %s/%s event", evs, tc.wantType, tc.wantSub)
			}
		})
	}
}

// TestReadEvent_ReorderedKeys_StructuralFallback covers frames whose keys do
// not start with the fast-path prefix: hook frames are still dropped and a
// control_response still yields the ack SetModel waits on.
func TestReadEvent_ReorderedKeys_StructuralFallback(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	for _, line := range []string{
		`{"subtype":"hook_started","type":"system","hook_id":"h1"}`,
		`{"subtype":"hook_progress","type":"system"}`,
		`{"response":{"subtype":"success"},"type":"control_response"}`,
	} {
		evs, done, err := p.ReadEvent(line)
		if err != nil || done || len(evs) != 0 {
			t.Errorf("ReadEvent(%s) = (%+v, %v, %v), want skipped", line, evs, done, err)
		}
	}

	line := `{"response":{"subtype":"error","request_id":"naozhi-setmodel-7","error":"unknown model"},"type":"control_response"}`
	evs, done, err := p.ReadEvent(line)
	if err != nil || done {
		t.Fatalf("ReadEvent = (_, %v, %v)", done, err)
	}
	if len(evs) != 1 || evs[0].Type != "control_ack" || evs[0].RPCRequestID != "naozhi-setmodel-7" ||
		evs[0].SubType != "error" || evs[0].Result != "unknown model" {
		t.Fatalf("got %+v, want control_ack for naozhi-setmodel-7", evs)
	}
}
