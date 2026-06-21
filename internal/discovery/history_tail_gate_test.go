package discovery

import (
	"testing"
)

// TestParseHistoryLine_ByteGateSkipsNonUserAssistant pins R202606d-PERF-001:
// lines whose type is neither "user" nor "assistant" (tool_use, system,
// thinking, summary, …) are rejected by the cheap byte gate and never reach
// the JSON decode. We can only observe the outcome (ok=false), but the gate
// is the only path that returns false for these well-formed lines, so a
// regression that removed it would still pass this test only if the
// downstream switch also rejects them — which it does, preserving behaviour.
func TestParseHistoryLine_ByteGateSkipsNonUserAssistant(t *testing.T) {
	t.Parallel()
	skip := []string{
		`{"type":"tool_use","timestamp":"x","name":"Bash","input":{"command":"ls"}}`,
		`{"type":"system","timestamp":"x","content":"compaction"}`,
		`{"type":"thinking","timestamp":"x","message":{"content":"hmm"}}`,
		`{"type":"summary","summary":"a recap"}`,
		`{"foo":"bar"}`,
		``,
	}
	for _, line := range skip {
		if _, ok := parseHistoryLine([]byte(line)); ok {
			t.Errorf("expected line to be skipped, but it parsed: %s", line)
		}
	}
}

// TestParseHistoryLine_ByteGateLetsUserAssistantThrough verifies the gate
// is a pure fast-negative: valid user and assistant records still parse to
// the same entries they did before the gate existed.
func TestParseHistoryLine_ByteGateLetsUserAssistantThrough(t *testing.T) {
	t.Parallel()

	userLine := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello there"}}`
	got, ok := parseHistoryLine([]byte(userLine))
	if !ok {
		t.Fatal("user line rejected by gate — should parse")
	}
	if len(got) != 1 || got[0].Type != "user" || got[0].Summary != "hello there" {
		t.Fatalf("user line mis-parsed: %+v", got)
	}

	asstLine := `{"type":"assistant","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}]}}`
	got, ok = parseHistoryLine([]byte(asstLine))
	if !ok {
		t.Fatal("assistant line rejected by gate — should parse")
	}
	if len(got) != 1 || got[0].Type != "text" || got[0].Summary != "hi back" {
		t.Fatalf("assistant line mis-parsed: %+v", got)
	}
}

// TestParseHistoryLine_ByteGateToleratesSpacedTypeField ensures a producer
// that pretty-prints `"type": "user"` (space after the colon) is not
// silently dropped by the gate.
func TestParseHistoryLine_ByteGateToleratesSpacedTypeField(t *testing.T) {
	t.Parallel()
	spaced := `{"type": "user", "timestamp": "2026-01-01T00:00:00Z", "message": {"role": "user", "content": "spaced out"}}`
	got, ok := parseHistoryLine([]byte(spaced))
	if !ok {
		t.Fatal("spaced user line rejected by gate — should parse")
	}
	if len(got) != 1 || got[0].Summary != "spaced out" {
		t.Fatalf("spaced user line mis-parsed: %+v", got)
	}
}

// TestParseHistoryLine_GateAllowsMarkerInsideStringButSwitchRejects
// documents the deliberate fast-negative semantics: a non-user/assistant
// record that happens to embed the marker substring inside a nested value
// still passes the gate (wasted unmarshal) but is correctly rejected by the
// authoritative type switch — i.e. the gate never changes the result.
func TestParseHistoryLine_GateAllowsMarkerInsideStringButSwitchRejects(t *testing.T) {
	t.Parallel()
	// type is tool_use, but the embedded command text contains the marker.
	line := `{"type":"tool_use","name":"Bash","input":{"command":"echo '\"type\":\"user\"'"}}`
	if _, ok := parseHistoryLine([]byte(line)); ok {
		t.Error("tool_use line with embedded marker must still be rejected by the type switch")
	}
}
