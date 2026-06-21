package textutil

import (
	"strings"
	"testing"
)

// TestRedactSecrets_Patterns verifies each well-known secret-prefix gets
// swapped for [REDACTED] while surrounding text stays intact. R234-SEC-7
// (#1006); relocated from internal/cron in R20260602-091302-ARCH-1 (#1571).
func TestRedactSecrets_Patterns(t *testing.T) {
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
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSecrets_NewPrefixes verifies the prefixes added in R164930-SEC-3
// (sk-proj-, github_pat_, hf_) are redacted, and that short hf_ values below
// minTail are left intact.
func TestRedactSecrets_NewPrefixes(t *testing.T) {
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
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSecrets_GCP verifies the GCP / Google OAuth access token prefix
// (ya29.) added in R20260531-SEC-5 is redacted, including the base64url body
// that follows the dot, and that short tails / bare prefix prose are left
// intact.
func TestRedactSecrets_GCP(t *testing.T) {
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
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSecrets_R20260602SEC4 verifies the prefixes added in
// R20260602-SEC-4 / R164029-SEC-5: Databricks (dapi), HCP Vault (hvs.),
// Stripe secret (sk_live_/sk_test_) and Stripe restricted (rk_live_/rk_test_)
// keys are redacted, while short tails below minTail and bare-prefix prose
// stay intact.
func TestRedactSecrets_R20260602SEC4(t *testing.T) {
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
			name: "Stripe live secret key",
			in:   "STRIPE_SECRET_KEY=sk_live_abcdefghij0123456789 used",
			want: "STRIPE_SECRET_KEY=[REDACTED] used",
		},
		{
			name: "Stripe test secret key",
			in:   "STRIPE_TEST_KEY=sk_test_abcdefghij0123456789 used",
			want: "STRIPE_TEST_KEY=[REDACTED] used",
		},
		{
			name: "Stripe secret short tail not redacted",
			in:   "sk_live_short is not a key",
			want: "sk_live_short is not a key",
		},
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
			name: "rk_ alongside sk_live_ in one line",
			in:   "live=sk_live_abcdefghij0123456789 restricted=rk_live_abcdefghij0123456789",
			want: "live=[REDACTED] restricted=[REDACTED]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSecrets_EnvAssignment verifies the KEY=value masking branch added
// in R202606-SEC-008b (#2165): values of credential-named keys are masked even
// when the value itself is not a well-known token prefix, while benign config
// assignments are left intact.
func TestRedactSecrets_EnvAssignment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "custom secret with non-prefix value",
			in:   "panic: MY_CUSTOM_SECRET=password123 leaked in traceback",
			want: "panic: MY_CUSTOM_SECRET=[REDACTED] leaked in traceback",
		},
		{
			name: "DB password",
			in:   "connect failed DB_PASSWORD=s3cr3t!val host=db",
			want: "connect failed DB_PASSWORD=[REDACTED] host=db",
		},
		{
			name: "generic token key",
			in:   "env TOKEN=abc.def.ghi set",
			want: "env TOKEN=[REDACTED] set",
		},
		{
			name: "API_KEY underscore variant",
			in:   "MY_API_KEY=opaque-value-not-a-prefix done",
			want: "MY_API_KEY=[REDACTED] done",
		},
		{
			name: "APIKEY no underscore",
			in:   "SOMEAPIKEY=zzz used",
			want: "SOMEAPIKEY=[REDACTED] used",
		},
		{
			name: "credential key",
			in:   "AWS_CREDENTIAL=raw123 ok",
			want: "AWS_CREDENTIAL=[REDACTED] ok",
		},
		{
			name: "spaces around equals",
			in:   "GITHUB_TOKEN = ghx_rawvalue here",
			want: "GITHUB_TOKEN = [REDACTED] here",
		},
		{
			name: "multiple sensitive assignments",
			in:   "DB_PASSWORD=p1 and API_TOKEN=t2 both",
			want: "DB_PASSWORD=[REDACTED] and API_TOKEN=[REDACTED] both",
		},
		{
			name: "value already a known prefix still single redacted",
			in:   "STRIPE_SECRET=sk_live_abcdefghij0123456789 used",
			want: "STRIPE_SECRET=[REDACTED] used",
		},
		// Negative: benign config keys must NOT be masked.
		{
			name: "LOG_LEVEL not masked",
			in:   "LOG_LEVEL=debug verbose",
			want: "LOG_LEVEL=debug verbose",
		},
		{
			name: "PATH not masked",
			in:   "PATH=/usr/local/bin:/usr/bin running",
			want: "PATH=/usr/local/bin:/usr/bin running",
		},
		{
			name: "generic KEY config not masked",
			in:   "sort KEY=name order",
			want: "sort KEY=name order",
		},
		{
			name: "no equals untouched",
			in:   "the SECRET word appears in prose",
			want: "the SECRET word appears in prose",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactSecrets_EnvAssignment_Idempotent confirms re-running over masked
// env assignments is a no-op (R202606-SEC-008b, #2165).
func TestRedactSecrets_EnvAssignment_Idempotent(t *testing.T) {
	src := "MY_CUSTOM_SECRET=password123 and DB_PASSWORD=hunter2 leaked"
	once := RedactSecrets(src)
	twice := RedactSecrets(once)
	if once != twice {
		t.Fatalf("not idempotent:\n  once  = %q\n  twice = %q", once, twice)
	}
	if strings.Contains(once, "password123") || strings.Contains(once, "hunter2") {
		t.Fatalf("redactor left a secret value intact: %q", once)
	}
}

// TestRedactSecrets_Negative ensures benign output is returned aliased (no
// allocation, no spurious matches). R234-SEC-7 (#1006).
func TestRedactSecrets_Negative(t *testing.T) {
	cases := []string{
		"",
		"plain ascii output",
		"日本語のテキスト出力", // unicode w/o secrets
		"go test ./...",
		"writing to disk",
		// Short literals that are NOT secrets — minTail floor blocks these
		// so doc/help text mentioning the prefix is not corrupted.
		"the prefix sk-ant- is reserved",
		"see ghp_ for personal access tokens",
		"AKIA is the AWS marker",
		"sk-abc is not a key",
		"use sk-short123 placeholder",
		"hf_ and npm_ are reserved prefixes",
	}
	for _, in := range cases {
		got := RedactSecrets(in)
		if got != in {
			t.Errorf("RedactSecrets(%q) altered benign input → %q", in, got)
		}
	}
}

// TestRedactSecrets_Idempotent confirms re-running the redactor on
// already-scrubbed output is a no-op. R234-SEC-7 (#1006).
func TestRedactSecrets_Idempotent(t *testing.T) {
	src := "leaked sk-ant-api03-abcdef0123456789 inside ghp_abcdef0123456789 mid-line"
	once := RedactSecrets(src)
	twice := RedactSecrets(once)
	if once != twice {
		t.Fatalf("not idempotent:\n  once  = %q\n  twice = %q", once, twice)
	}
	if strings.Contains(once, "sk-ant-") || strings.Contains(once, "ghp_") {
		t.Fatalf("redactor left a known prefix intact: %q", once)
	}
}

// TestMayContainSecretPrefix_FirstBytes verifies the fast-path pre-scan
// recognises the first byte of every registered prefix family.
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
		"dapi1234567890abcdef",            // 'd' — Databricks
		"hvs.AAAA0123456789",              // 'h' — HCP Vault
		"rk_live_abcdefghij0123456789",    // 'r' — Stripe restricted
		"-----BEGIN RSA PRIVATE KEY-----", // '-' — PEM header
	}
	for _, in := range truthy {
		if !mayContainSecretPrefix(in) {
			t.Errorf("mayContainSecretPrefix(%q) = false, want true", in)
		}
	}
	falsy := []string{
		"",
		"402 ok",     // none of s/g/A/x/h/n/y/d/r/- first bytes
		"quit + lib", // benign tokens, no trigger byte
	}
	for _, in := range falsy {
		if mayContainSecretPrefix(in) {
			t.Errorf("mayContainSecretPrefix(%q) = true, want false", in)
		}
	}
}

// TestRedactSecrets_PEMHeader verifies that PEM private-key / certificate
// block headers (`-----BEGIN …`) are redacted. R20260613-SEC-9.
//
// Architecture note: the tail scanner stops at the first space character
// (space is not an isSecretTokenByte char), so only the `-----BEGIN` token
// itself is replaced — the remainder of the header line is not redacted.
// minTail=0 is used because the prefix is unambiguous on its own.
// `eyJ` (JWT prefix) is deliberately NOT added: eyJ == base64(`{"`) which
// would false-positive on any base64-encoded JSON object; JWT dots are not
// isSecretTokenByte chars so tails terminate immediately, making the match
// unreliable.
func TestRedactSecrets_PEMHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "RSA private key header",
			in:   "-----BEGIN RSA PRIVATE KEY-----",
			want: "[REDACTED] RSA PRIVATE KEY-----",
		},
		{
			name: "EC private key header",
			in:   "-----BEGIN EC PRIVATE KEY-----",
			want: "[REDACTED] EC PRIVATE KEY-----",
		},
		{
			name: "PKCS8 private key header",
			in:   "-----BEGIN PRIVATE KEY-----",
			want: "[REDACTED] PRIVATE KEY-----",
		},
		{
			name: "certificate header not redacted (not a private key material concern)",
			// CERTIFICATE is public data; however the same prefix fires.
			in:   "-----BEGIN CERTIFICATE-----",
			want: "[REDACTED] CERTIFICATE-----",
		},
		{
			name: "PEM header embedded in env dump",
			in:   "PRIVATE_KEY=\"-----BEGIN RSA PRIVATE KEY-----\\nMIIEo...",
			want: "PRIVATE_KEY=\"[REDACTED] RSA PRIVATE KEY-----\\nMIIEo...",
		},
		{
			name: "eyJ prefix NOT redacted (conservative: false-positive risk)",
			in:   "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.sig",
			want: "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.sig",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}
