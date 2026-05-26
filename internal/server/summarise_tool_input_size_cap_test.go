package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSummariseToolInput_OversizeShortCircuits pins the contract that
// summariseToolInput refuses to decode any input larger than
// maxSummariseToolInputBytes and returns "" without paying the
// json.Unmarshal allocation. R242-SEC-13 (#645): without the cap a
// pathological transcript line carrying a multi-megabyte
// tool_use.input would drive Unmarshal to allocate proportional
// memory just to extract a 200-byte label.
//
// We construct a JSON input that is well-formed but exceeds the cap
// (64 KB string in a "command" field) so the test specifically
// exercises the SIZE branch — a malformed-JSON branch would hit the
// pre-existing Unmarshal-error early-return and prove nothing about
// the new cap.
func TestSummariseToolInput_OversizeShortCircuits(t *testing.T) {
	// Build a 64KB+ command string and wrap it in JSON. That puts the
	// total payload well above maxSummariseToolInputBytes (32 KB).
	big := strings.Repeat("A", maxSummariseToolInputBytes+1024)
	raw := json.RawMessage(`{"command":"` + big + `"}`)
	if len(raw) <= maxSummariseToolInputBytes {
		t.Fatalf("test fixture too small: %d bytes; need > %d",
			len(raw), maxSummariseToolInputBytes)
	}
	if got := summariseToolInput("Bash", raw); got != "" {
		t.Errorf("oversize input: got %q, want empty (cap should short-circuit)", got)
	}
}

// TestSummariseToolInput_AtCapBoundary pins that an input EXACTLY at
// the cap is still decoded normally (cap is a hard ceiling, not a
// strict-less-than gate). Without this test, a future "<= cap" /
// "< cap" comparison drift would silently change behaviour at the
// boundary and the next cycle could not tell which side won.
func TestSummariseToolInput_AtCapBoundary(t *testing.T) {
	// "command" + JSON framing = 14 bytes; pad with a single repeated
	// char so total length is exactly maxSummariseToolInputBytes.
	frame := `{"command":"` // 12 bytes
	tail := `"}`            // 2 bytes
	padLen := maxSummariseToolInputBytes - len(frame) - len(tail)
	if padLen < 1 {
		t.Skip("cap too small for boundary test fixture")
	}
	cmd := strings.Repeat("A", padLen)
	raw := json.RawMessage(frame + cmd + tail)
	if len(raw) != maxSummariseToolInputBytes {
		t.Fatalf("fixture sized wrong: got %d bytes, want exactly %d",
			len(raw), maxSummariseToolInputBytes)
	}
	got := summariseToolInput("Bash", raw)
	if got == "" {
		t.Fatal("at-cap input returned empty; cap must accept exact-size inputs")
	}
	if !strings.HasPrefix(got, "AAA") {
		t.Errorf("at-cap input: got %q, want a string starting with the AAAA pad", got)
	}
}

// TestSummariseToolInput_SmallInputUnchanged pins that the new cap
// does not regress the common-case behaviour: a typical Bash tool
// call with a short command must still surface that command in the
// summary. This is the regression backstop for the fast path.
func TestSummariseToolInput_SmallInputUnchanged(t *testing.T) {
	raw := json.RawMessage(`{"command":"ls -la"}`)
	got := summariseToolInput("Bash", raw)
	if !strings.Contains(got, "ls -la") {
		t.Errorf("small input: got %q, want it to contain 'ls -la'", got)
	}
}
