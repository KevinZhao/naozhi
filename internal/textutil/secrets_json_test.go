package textutil

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactSecrets_EnvAssignmentInsideJSONStringKeepsJSONValid pins the
// dashboard "200 empty body" regression: RedactSecrets is run over raw JSON
// (cron transcript tool_use.input, sandbox run-event NDJSON lines). The bare
// `KEY=value` branch used to consume `\S+`, which swallowed the closing `"}`
// of the enclosing JSON string and produced unencodable output.
func TestRedactSecrets_EnvAssignmentInsideJSONStringKeepsJSONValid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"bare value before closing quote", `{"command":"export ANTHROPIC_API_KEY=abc"}`},
		{"bare value mid-string", `{"command":"export ANTHROPIC_API_KEY=abc && echo ok"}`},
		{"json-escaped double quotes", `{"command":"export ANTHROPIC_API_KEY=\"abc\" && echo ok"}`},
		{"json-escaped newline follows value", `{"command":"export DB_PASSWORD=abc\necho done"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if !json.Valid([]byte(got)) {
				t.Fatalf("RedactSecrets broke JSON:\n in  = %s\n got = %s", tc.in, got)
			}
			if strings.Contains(got, "abc") {
				t.Errorf("secret value leaked: %s", got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("expected [REDACTED] marker, got %s", got)
			}
		})
	}
}

// TestRedactSecrets_BareValueKeepsEscapesAndWindowsPaths pins the review
// finding on PR #2439: the bare-value branch must not stop at a lone
// backslash. RedactSecrets also runs on plain IM / WS / self-update text, so
// a Windows path or an escaped byte inside a secret value has to be masked
// whole; only an unescaped quote (JSON string boundary) may end the run.
func TestRedactSecrets_BareValueKeepsEscapesAndWindowsPaths(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"windows path", `PASSWORD=C:\Users\bob\pw done`, `PASSWORD=[REDACTED] done`},
		{"single escape", `TOKEN=foo\bar done`, `TOKEN=[REDACTED] done`},
		{"leading escape", `TOKEN=\x done`, `TOKEN=[REDACTED] done`},
		{"double backslash inside JSON string", `{"c":"TOKEN=\\abc"}`, `{"c":"TOKEN=[REDACTED]"}`},
		{"escaped quote span inside JSON string", `{"c":"TOKEN=\"abc\" x"}`, `{"c":"TOKEN=\"[REDACTED]\" x"}`},
		// An unescaped quote is a JSON string boundary in the dashboard's
		// input and therefore ends the run; the leading fragment is masked.
		{"unescaped quote ends run", `SECRET=a"b`, `SECRET=[REDACTED]"b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
			if strings.HasPrefix(tc.in, "{") && !json.Valid([]byte(got)) {
				t.Errorf("JSON input produced invalid JSON: %s", got)
			}
		})
	}
}
