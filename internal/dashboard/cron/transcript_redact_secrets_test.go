package cron

// Tests for R20260603-SEC-4/8/9/3: secrets must be redacted from all
// dashboard transcript wire fields before they reach the HTTP response.
// Covers: tool_result Output, assistant Text, user Text, summariseToolInput.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTranscript_RedactsSecretsInToolResult verifies that a sk-ant-api03-…
// token embedded in a tool_result Output is replaced with [REDACTED] before
// it reaches the wire response (R20260603-SEC-4).
func TestTranscript_RedactsSecretsInToolResult(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	secret := "sk-ant-api03-" + strings.Repeat("A", 40)
	lines := []string{
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"output contains ` + secret + ` here","is_error":false}]}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)

	w := callTranscript(h, jobID, runID)
	body := w.Body.String()

	if strings.Contains(body, secret) {
		t.Errorf("tool_result secret leaked into wire response (R20260603-SEC-4): found %q in body", secret)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("tool_result: expected [REDACTED] marker in body, got: %s", body)
	}

	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, tr := range resp.Turns {
		if tr.Kind == "tool_result" && strings.Contains(tr.Output, secret) {
			t.Errorf("tool_result turn Output contains raw secret: %q", tr.Output)
		}
	}
}

// TestTranscript_RedactsSecretsInAssistantText verifies that a ghp_… token
// embedded in an assistant text block is replaced with [REDACTED] before the
// wire response (R20260603-SEC-8).
func TestTranscript_RedactsSecretsInAssistantText(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	secret := "ghp_" + strings.Repeat("B", 20)
	lines := []string{
		`{"type":"assistant","timestamp":"` + now + `","message":{"role":"assistant","content":[{"type":"text","text":"here is ` + secret + ` in my reply"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)

	w := callTranscript(h, jobID, runID)
	body := w.Body.String()

	if strings.Contains(body, secret) {
		t.Errorf("assistant text secret leaked into wire response (R20260603-SEC-8): found %q in body", secret)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("assistant text: expected [REDACTED] marker in body, got: %s", body)
	}
}

// TestTranscript_RedactsSecretsInUserText verifies that a secret in a plain
// user text message is redacted before the wire response (R20260603-SEC-9).
func TestTranscript_RedactsSecretsInUserText(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	secret := "AKIA" + strings.Repeat("C", 20)
	lines := []string{
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":"my key is ` + secret + ` done"}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)

	w := callTranscript(h, jobID, runID)
	body := w.Body.String()

	if strings.Contains(body, secret) {
		t.Errorf("user text secret leaked into wire response (R20260603-SEC-9): found %q in body", secret)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("user text: expected [REDACTED] marker in body, got: %s", body)
	}
}

// TestTranscript_RedactsSecretsInSummariseToolInput verifies that
// summariseToolInput redacts secrets from both the priority-key path and the
// fallback raw-bytes path (R20260603-SEC-3).
func TestTranscript_RedactsSecretsInSummariseToolInput(t *testing.T) {
	t.Parallel()
	secret := "sk-ant-api03-" + strings.Repeat("D", 40)

	// Priority-key path: secret embedded in the "command" field.
	priorityInput := json.RawMessage(`{"command":"export TOKEN=` + secret + `"}`)
	got := summariseToolInput("Bash", priorityInput)
	if strings.Contains(got, secret) {
		t.Errorf("summariseToolInput priority path leaked secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("summariseToolInput priority path: expected [REDACTED], got: %q", got)
	}

	// Fallback raw-bytes path: no recognised key, secret in an unknown field.
	fallbackInput := json.RawMessage(`{"token":"` + secret + `"}`)
	got2 := summariseToolInput("CustomTool", fallbackInput)
	if strings.Contains(got2, secret) {
		t.Errorf("summariseToolInput fallback path leaked secret: %q", got2)
	}
	if !strings.Contains(got2, "[REDACTED]") {
		t.Errorf("summariseToolInput fallback path: expected [REDACTED], got: %q", got2)
	}
}

// TestTranscript_RedactsSecretsInToolUseInput verifies that a secret embedded
// in the raw tool_use Input JSON is redacted before it reaches the wire — the
// Summary one-liner was already redacted but the full Input RawMessage went out
// verbatim (R20260607-SEC-8 / #1914).
func TestTranscript_RedactsSecretsInToolUseInput(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	secret := "sk-ant-api03-" + strings.Repeat("F", 40)
	// Use an unrecognised field name so summariseToolInput's priority-key path
	// does NOT surface it — the secret can then only reach the wire through the
	// raw Input field, isolating the fix under test.
	lines := []string{
		`{"type":"assistant","timestamp":"` + now + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_9","name":"CustomTool","input":{"apiKey":"` + secret + `"}}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}
	h, jobID, runID, _ := fixtureRunWithJSONL(t, lines)

	w := callTranscript(h, jobID, runID)
	body := w.Body.String()

	if strings.Contains(body, secret) {
		t.Errorf("tool_use Input secret leaked into wire response (R20260607-SEC-8): found %q in body", secret)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("tool_use Input: expected [REDACTED] marker in body, got: %s", body)
	}

	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, tr := range resp.Turns {
		if tr.Kind == "tool_use" && strings.Contains(string(tr.Input), secret) {
			t.Errorf("tool_use turn Input contains raw secret: %q", string(tr.Input))
		}
	}
}

// TestRedactToolInput_PreservesCleanInput verifies the helper aliases clean
// input unchanged (no spurious re-encoding) and leaves a nil input nil.
func TestRedactToolInput_PreservesCleanInput(t *testing.T) {
	t.Parallel()
	clean := json.RawMessage(`{"path":"/etc/hosts"}`)
	if got := redactToolInput(clean); string(got) != string(clean) {
		t.Errorf("clean input mutated: got %q want %q", string(got), string(clean))
	}
	if got := redactToolInput(nil); got != nil {
		t.Errorf("nil input should stay nil, got %q", string(got))
	}

	secret := "ghp_" + strings.Repeat("G", 20)
	dirty := json.RawMessage(`{"token":"` + secret + `"}`)
	got := redactToolInput(dirty)
	if strings.Contains(string(got), secret) {
		t.Errorf("redactToolInput leaked secret: %q", string(got))
	}
	if !strings.Contains(string(got), "[REDACTED]") {
		t.Errorf("redactToolInput: expected [REDACTED], got %q", string(got))
	}
	// Result must still be valid JSON.
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Errorf("redacted input is not valid JSON: %v (%q)", err, string(got))
	}
}

// TestSanitizeWireText_RedactsSecrets verifies the low-level helper directly —
// both the fast (clean ASCII) path and the slow (dirty / multibyte) path must
// both redact secrets.
func TestSanitizeWireText_RedactsSecrets(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("E", 20)

	// Fast path: pure ASCII printable, no control bytes.
	fastIn := "prefix " + secret + " suffix"
	fastOut := sanitizeWireText(fastIn)
	if strings.Contains(fastOut, secret) {
		t.Errorf("sanitizeWireText fast path leaked secret: %q", fastOut)
	}

	// Slow path: contains a C0 control byte forcing the strings.Map branch.
	slowIn := "prefix\x01" + secret + " suffix"
	slowOut := sanitizeWireText(slowIn)
	if strings.Contains(slowOut, secret) {
		t.Errorf("sanitizeWireText slow path leaked secret: %q", slowOut)
	}
}
