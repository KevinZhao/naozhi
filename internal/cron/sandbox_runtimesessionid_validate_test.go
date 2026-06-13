package cron

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestIsValidRuntimeSessionID covers valid and invalid RuntimeSessionID values.
// Production format produced by sandboxRuntimeSessionID: "run-<hex>-<unixnano>".
// [R20260613-SEC-2 / #2065]
func TestIsValidRuntimeSessionID(t *testing.T) {
	t.Parallel()

	// Generate a canonical example matching the production generator.
	canonical := sandboxRuntimeSessionID("0123456789abcdef", time.Unix(1718000000, 123456789))

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		// --- valid ---
		{
			name:  "canonical production format",
			input: canonical,
			want:  true,
		},
		{
			name:  "minimal hex segment",
			input: "run-a-1718000000000000000",
			want:  true,
		},
		{
			name:  "long hex run-id",
			input: "run-0123456789abcdef-1718000000123456789",
			want:  true,
		},

		// --- invalid ---
		{
			name:  "empty string",
			input: "",
			want:  false,
		},
		{
			name:  "missing run- prefix",
			input: "0123456789abcdef-1718000000000000000",
			want:  false,
		},
		{
			name:  "uppercase hex",
			input: "run-0123456789ABCDEF-1718000000000000000",
			want:  false,
		},
		{
			name:  "missing unixnano suffix",
			input: "run-0123456789abcdef",
			want:  false,
		},
		{
			name:  "extra path traversal component",
			input: "run-0123456789abcdef-1718000000000000000/../etc/passwd",
			want:  false,
		},
		{
			name:  "null byte injection",
			input: "run-0123456789abcdef-1718000000000000000\x00",
			want:  false,
		},
		{
			name:  "control character injection",
			input: "run-0123456789abcdef-1718000000000000000\n",
			want:  false,
		},
		{
			name:  "excessively long (>256 chars)",
			input: "run-" + strings.Repeat("a", 300) + "-1718000000000000000",
			want:  false,
		},
		{
			name:  "non-hex character in run-id segment",
			input: "run-xyz-1718000000000000000",
			want:  false,
		},
		{
			name:  "decimal in nano segment replaced with hex",
			input: "run-0123456789abcdef-0xDEADBEEF",
			want:  false,
		},
		{
			name:  "space character",
			input: "run-0123456789abcdef -1718000000000000000",
			want:  false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isValidRuntimeSessionID(tc.input)
			if got != tc.want {
				t.Errorf("isValidRuntimeSessionID(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestSandboxRuntimeSessionID_MatchesValidator confirms that every id
// produced by sandboxRuntimeSessionID passes isValidRuntimeSessionID, so
// the validator is a strict subset of the production format — not tighter.
func TestSandboxRuntimeSessionID_MatchesValidator(t *testing.T) {
	t.Parallel()
	runs := []struct {
		runID     string
		startedAt time.Time
	}{
		{"0123456789abcdef", time.Unix(1718000000, 0)},
		{"aaaaaaaaaaaaaaaa", time.Unix(0, 1)},
		{"fedcba9876543210", time.Now()},
	}
	for _, r := range runs {
		sid := sandboxRuntimeSessionID(r.runID, r.startedAt)
		if !isValidRuntimeSessionID(sid) {
			t.Errorf("sandboxRuntimeSessionID(%q, %v) = %q did not pass isValidRuntimeSessionID",
				r.runID, r.startedAt, sid)
		}
		// Also confirm the expected prefix structure.
		expected := fmt.Sprintf("run-%s-%d", r.runID, r.startedAt.UnixNano())
		if sid != expected {
			t.Errorf("sandboxRuntimeSessionID = %q, want %q", sid, expected)
		}
	}
}
