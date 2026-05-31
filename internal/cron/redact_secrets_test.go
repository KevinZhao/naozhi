package cron

import (
	"strings"
	"testing"
)

// TestRedactSecretsInResult_Patterns verifies each well-known secret-prefix
// gets swapped for [REDACTED] while surrounding text stays intact.
// R234-SEC-7 (#1006).
func TestRedactSecretsInResult_Patterns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Anthropic API key",
			in:   "found token sk-ant-api03-abcDEF123_xyz-456 trailing",
			want: "found token [REDACTED] trailing",
		},
		{
			name: "GitHub PAT",
			in:   "git remote: ghp_abcdef0123456789 access denied",
			want: "git remote: [REDACTED] access denied",
		},
		{
			name: "GitHub OAuth",
			in:   "header X-Token: gho_qrstuv0123456789",
			want: "header X-Token: [REDACTED]",
		},
		{
			name: "AWS access key",
			in:   "key=AKIAIOSFODNN7EXAMPLE region=us-east-1",
			want: "key=[REDACTED] region=us-east-1",
		},
		{
			name: "AWS STS access key",
			in:   "ASIAQRSTUVWXYZ012345 used by role",
			want: "[REDACTED] used by role",
		},
		{
			name: "GitLab PAT",
			in:   "token=glpat-abcdefghij0123456789 push",
			want: "token=[REDACTED] push",
		},
		{
			name: "Slack bot",
			in:   "secret xoxb-1234567890-abcdefghij and",
			want: "secret [REDACTED] and",
		},
		{
			name: "multiple in one line",
			in:   "ghp_abcdef0123456789 and AKIAIOSFODNN7EXAMPLE",
			want: "[REDACTED] and [REDACTED]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecretsInResult(tc.in)
			if got != tc.want {
				t.Errorf("redactSecretsInResult(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSecretsInResult_NewPrefixes verifies the three additional
// prefixes added in R164930-SEC-3 (sk-proj-, github_pat_, hf_) are
// redacted, and that short hf_ values below minTail are left intact.
func TestRedactSecretsInResult_NewPrefixes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "OpenAI project key",
			in:   "key sk-proj-abcdefghij1234567890 used",
			want: "key [REDACTED] used",
		},
		{
			name: "GitHub fine-grained PAT",
			in:   "token github_pat_11ABCDEFG0abcdefghij1234567890 rejected",
			want: "token [REDACTED] rejected",
		},
		{
			name: "HuggingFace token",
			in:   "auth hf_abcdefghij1234567890abcdef ok",
			want: "auth [REDACTED] ok",
		},
		{
			name: "hf_ short tail not redacted",
			in:   "see hf_short for details",
			want: "see hf_short for details",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecretsInResult(tc.in)
			if got != tc.want {
				t.Errorf("redactSecretsInResult(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSecretsInResult_Negative ensures benign Claude output is
// returned aliased (no allocation, no spurious matches). R234-SEC-7
// (#1006).
func TestRedactSecretsInResult_Negative(t *testing.T) {
	cases := []string{
		"",
		"plain ascii output",
		"日本語のテキスト出力", // unicode w/o secrets
		"go test ./...",
		"writing to disk",
		// Short literals that are NOT secrets — minTail floor blocks
		// these so doc/help text mentioning the prefix is not corrupted.
		"the prefix sk-ant- is reserved",
		"see ghp_ for personal access tokens",
		"AKIA is the AWS marker",
	}
	for _, in := range cases {
		got := redactSecretsInResult(in)
		if got != in {
			t.Errorf("redactSecretsInResult(%q) altered benign input → %q", in, got)
		}
	}
}

// TestRedactSecretsInResult_Idempotent confirms re-running the redactor on
// already-scrubbed output is a no-op so finishRun's persistence pipeline
// (sanitiseRunResult → file write → re-read → re-sanitise) cannot drift.
// R234-SEC-7 (#1006).
func TestRedactSecretsInResult_Idempotent(t *testing.T) {
	src := "leaked sk-ant-api03-abcdef0123456789 inside ghp_abcdef0123456789 mid-line"
	once := redactSecretsInResult(src)
	twice := redactSecretsInResult(once)
	if once != twice {
		t.Fatalf("not idempotent:\n  once  = %q\n  twice = %q", once, twice)
	}
	if strings.Contains(once, "sk-ant-") || strings.Contains(once, "ghp_") {
		t.Fatalf("redactor left a known prefix intact: %q", once)
	}
}

// TestSanitiseRunResult_RedactsSecrets is the integration coverage: the
// production sanitiseRunResult path (used by skipPersist branches and
// recordTerminalResult) must scrub secrets before the SanitizeForLog
// log-injection pass. R234-SEC-7 (#1006).
func TestSanitiseRunResult_RedactsSecrets(t *testing.T) {
	in := "claude said: sk-ant-api03-abcdef0123456789 is your key"
	got := sanitiseRunResult(in)
	if strings.Contains(got, "sk-ant-") {
		t.Errorf("sanitiseRunResult left token intact: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("sanitiseRunResult did not insert [REDACTED]: %q", got)
	}
}
