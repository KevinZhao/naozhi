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

// TestTranscript_RedactsSecrets verifies each wire field a secret can reach
// — tool_result Output (R20260603-SEC-4), assistant Text (-SEC-8), user Text
// (-SEC-9) and the raw tool_use Input (R20260607-SEC-8 / #1914; an unknown
// field name so the Summary one-liner does not carry it) — is redacted.
func TestTranscript_RedactsSecrets(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	cases := []struct {
		name, secret, line string
	}{
		{"tool_result", "sk-ant-api03-" + strings.Repeat("A", 40),
			`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"output contains SECRET here","is_error":false}]}}`},
		{"assistant_text", "ghp_" + strings.Repeat("B", 20),
			`{"type":"assistant","timestamp":"` + now + `","message":{"role":"assistant","content":[{"type":"text","text":"here is SECRET in my reply"}],"usage":{"input_tokens":1,"output_tokens":1}}}`},
		{"user_text", "AKIA" + strings.Repeat("C", 20),
			`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":"my key is SECRET done"}}`},
		{"tool_use_input", "sk-ant-api03-" + strings.Repeat("F", 40),
			`{"type":"assistant","timestamp":"` + now + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_9","name":"CustomTool","input":{"apiKey":"SECRET"}}],"usage":{"input_tokens":1,"output_tokens":1}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, jobID, runID, _ := fixtureRunWithJSONL(t, []string{strings.ReplaceAll(tc.line, "SECRET", tc.secret)})
			body := callTranscript(h, jobID, runID).Body.String()
			if strings.Contains(body, tc.secret) {
				t.Errorf("secret leaked into wire response: %s", body)
			}
			if !strings.Contains(body, "[REDACTED]") {
				t.Errorf("expected [REDACTED] marker in body, got: %s", body)
			}
		})
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
