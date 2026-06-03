package cron

import (
	"strings"
	"testing"
)

// The pure secret-redactor table tests now live in internal/textutil
// (secrets_test.go) since the scan logic moved there in
// R20260602-091302-ARCH-1 (#1571). What remains here is the cron-specific
// integration coverage: the scheduler sanitise paths must scrub secrets
// before persistence / WS broadcast / log-injection passes.

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

// TestRedactSecretsInResult_StripeRestrictedKey verifies that Stripe restricted
// keys (rk_live_/rk_test_) are scrubbed by RedactSecrets. These keys can
// initiate Stripe API calls and are equally sensitive to secret keys.
// [R164029-SEC-5].
func TestRedactSecretsInResult_StripeRestrictedKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Stripe restricted live key",
			in:   "STRIPE_KEY=rk_live_abcdefghij0123456789 used",
			want: "STRIPE_KEY=[REDACTED] used",
		},
		{
			name: "Stripe restricted test key",
			in:   "STRIPE_KEY=rk_test_abcdefghij0123456789 used",
			want: "STRIPE_KEY=[REDACTED] used",
		},
		{
			name: "rk_live_ short tail not redacted",
			in:   "rk_live_short is not a key",
			want: "rk_live_short is not a key",
		},
		{
			name: "rk_test_ short tail not redacted",
			in:   "rk_test_short is not a key",
			want: "rk_test_short is not a key",
		},
		{
			name: "rk_live_ first-byte fast-path detected",
			in:   "rk_live_abcdefghij0123456789",
			want: "[REDACTED]",
		},
		{
			name: "rk_ alongside sk_live_ in one line",
			in:   "live=sk_live_abcdefghij0123456789 restricted=rk_live_abcdefghij0123456789",
			want: "live=[REDACTED] restricted=[REDACTED]",
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

// TestRedactSecrets_AliasDelegates confirms the deprecated cron.RedactSecrets
// alias still scrubs (it now delegates to textutil.RedactSecrets). #1571.
func TestRedactSecrets_AliasDelegates(t *testing.T) {
	got := RedactSecrets("token sk-ant-api03-abcdef0123456789 here")
	if got != "token [REDACTED] here" {
		t.Errorf("cron.RedactSecrets alias diverged: %q", got)
	}
}
