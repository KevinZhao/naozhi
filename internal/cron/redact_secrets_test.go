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
		{
			name: "OpenAI project key",
			in:   "export OPENAI_API_KEY=sk-proj-abcdef0123456789ABCDEF done",
			want: "export OPENAI_API_KEY=[REDACTED] done",
		},
		{
			name: "OpenAI legacy key",
			in:   "key sk-abcdefghij0123456789ABCDEFGHIJ0123456789xyz here",
			want: "key [REDACTED] here",
		},
		{
			name: "HuggingFace token",
			in:   "HF_TOKEN=hf_abcdefghij0123456789 set",
			want: "HF_TOKEN=[REDACTED] set",
		},
		{
			name: "npm token",
			in:   "//registry: npm_abcdefghij0123456789 used",
			want: "//registry: [REDACTED] used",
		},
		{
			name: "sk-ant beats sk- fallback",
			in:   "sk-ant-api03-abcDEF123 ok",
			want: "[REDACTED] ok",
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

// TestRedactSecretsInResult_GCP verifies the GCP / Google OAuth access
// token prefix (ya29.) added in R20260531-SEC-5 is redacted, including the
// base64url body that follows the dot, and that short tails / bare prefix
// prose are left intact. [R20260531-SEC-5].
func TestRedactSecretsInResult_GCP(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "GCP access token",
			in:   "Authorization: Bearer ya29.AAAA0123456789abcdef-_ZZ done",
			want: "Authorization: Bearer [REDACTED] done",
		},
		{
			name: "GCP token at line start",
			in:   "ya29.a0AfH6SMBx1234567890abcdef rejected",
			want: "[REDACTED] rejected",
		},
		{
			name: "ya29. short tail not redacted",
			in:   "ya29.short here",
			want: "ya29.short here",
		},
		{
			name: "bare ya29 prefix prose not redacted",
			in:   "the ya29. prefix marks Google OAuth tokens",
			want: "the ya29. prefix marks Google OAuth tokens",
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

// TestRedactSecretsInResult_R20260602SEC4 verifies the four prefixes added in
// R20260602-SEC-4: Databricks (dapi), HCP Vault (hvs.), Stripe live
// (sk_live_), and Stripe test (sk_test_). Also confirms short tails below
// minTail are left intact, and that the fast-path mayContainSecretPrefix
// recognises the new 'd' first byte.
func TestRedactSecretsInResult_R20260602SEC4(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Databricks PAT",
			in:   "token dapi1234567890abcdef1234567890ab done",
			want: "token [REDACTED] done",
		},
		{
			name: "Databricks short tail not redacted",
			in:   "see dapishort for details",
			want: "see dapishort for details",
		},
		{
			name: "HCP Vault service token",
			in:   "auth hvs.AAAAABBBBBCCCCCDDDDDEEEEEFFFFFF1234 ok",
			want: "auth [REDACTED] ok",
		},
		{
			name: "HCP Vault short tail not redacted",
			in:   "see hvs.short here",
			want: "see hvs.short here",
		},
		{
			name: "Stripe live key",
			in:   "STRIPE_SECRET_KEY=sk_live_abcdefghij0123456789 used",
			want: "STRIPE_SECRET_KEY=[REDACTED] used",
		},
		{
			name: "Stripe test key",
			in:   "STRIPE_TEST_KEY=sk_test_abcdefghij0123456789 used",
			want: "STRIPE_TEST_KEY=[REDACTED] used",
		},
		{
			name: "Stripe key short tail not redacted",
			in:   "sk_live_short is not a key",
			want: "sk_live_short is not a key",
		},
		{
			name: "dapi first-byte fast-path detected",
			in:   "dapi1234567890abcdef1234567890ab",
			want: "[REDACTED]",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactSecretsInResult(tc.in)
			if got != tc.want {
				t.Errorf("redactSecretsInResult(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMayContainSecretPrefix_DapiFirstByte pins that the 'd' first byte
// added for Databricks tokens is recognised by the fast-path pre-scan.
func TestMayContainSecretPrefix_DapiFirstByte(t *testing.T) {
	t.Parallel()
	if !mayContainSecretPrefix("dapi1234567890abcdef") {
		t.Error("mayContainSecretPrefix('dapi...') = false, want true")
	}
	// Confirm purely 'd'-starting strings without any other trigger byte
	// are still detected.
	if !mayContainSecretPrefix("ddddd") {
		t.Error("mayContainSecretPrefix('ddddd') = false, want true (d is in set)")
	}
}

// TestSanitiseRunErrMsg_RedactsSecrets is the integration coverage for the
// error path: sanitiseRunErrMsg must scrub well-known secret prefixes so a
// leaked token in an error string (LastError) never lands on disk or the
// dashboard WS broadcast. [R20260531-SEC-8].
func TestSanitiseRunErrMsg_RedactsSecrets(t *testing.T) {
	cases := []string{
		"exec failed: auth header sk-ant-api03-abcdef0123456789 rejected",
		"git push denied with ghp_abcdef0123456789 token",
	}
	for _, in := range cases {
		got := sanitiseRunErrMsg(in)
		if strings.Contains(got, "sk-ant-") || strings.Contains(got, "ghp_") {
			t.Errorf("sanitiseRunErrMsg left token intact for %q → %q", in, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("sanitiseRunErrMsg did not insert [REDACTED] for %q → %q", in, got)
		}
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
		// sk- legacy: short tail under minTail (40) must not be redacted,
		// so ordinary "sk-" prose / short identifiers stay intact.
		"sk-abc is not a key",
		"use sk-short123 placeholder",
		"hf_ and npm_ are reserved prefixes",
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

// TestMayContainSecretPrefix_FirstBytes verifies the fast-path pre-scan
// recognises the first byte of every registered prefix family — in
// particular the newly added 'h' (hf_) and 'n' (npm_) which would
// otherwise short-circuit to false and disable redaction.
// [R030056-SEC-005].
func TestMayContainSecretPrefix_FirstBytes(t *testing.T) {
	truthy := []string{
		"hf_abcdefghij0123456789",
		"npm_abcdefghij0123456789",
		"sk-proj-abcdef0123456789ABCDEF",
		"ghp_abcdef0123456789",
		"AKIAIOSFODNN7EXAMPLE",
		"xoxb-1234567890",
		"ya29.AAAA0123456789abcdef",
	}
	for _, in := range truthy {
		if !mayContainSecretPrefix(in) {
			t.Errorf("mayContainSecretPrefix(%q) = false, want true", in)
		}
	}
	falsy := []string{
		"",
		"plai  u", // no s/g/A/x/h/n/y/d first bytes (was "plai du" pre-R20260602-SEC-4 before 'd' was added)
		"402",
	}
	for _, in := range falsy {
		if mayContainSecretPrefix(in) {
			t.Errorf("mayContainSecretPrefix(%q) = true, want false", in)
		}
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
