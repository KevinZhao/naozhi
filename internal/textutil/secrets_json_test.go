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
