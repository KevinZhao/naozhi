package textutil

import (
	"strings"
	"testing"
)

// TestRedactSecrets verifies each well-known secret-prefix family gets
// swapped for [REDACTED] while surrounding text stays intact, and that short
// tails below minTail / bare-prefix prose are left alone. R234-SEC-7 (#1006),
// R164930-SEC-3, R20260531-SEC-5, R20260602-SEC-4, R164029-SEC-5.
//
// PEM headers (R20260613-SEC-9): the tail scanner stops at the first space,
// so only the `-----BEGIN` token itself is replaced. `eyJ` (JWT) is
// deliberately NOT a prefix: eyJ == base64(`{"`) would false-positive on any
// base64-encoded JSON object.
func TestRedactSecrets(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"Anthropic API key", "found token sk-ant-api03-abcDEF123_xyz-456 trailing", "found token [REDACTED] trailing"},
		{"sk-ant beats sk- fallback", "sk-ant-api03-abcDEF123 ok", "[REDACTED] ok"},
		{"OpenAI project key", "export OPENAI_API_KEY=sk-proj-abcdef0123456789ABCDEF done", "export OPENAI_API_KEY=[REDACTED] done"},
		{"OpenAI legacy key", "key sk-abcdefghij0123456789ABCDEFGHIJ0123456789xyz here", "key [REDACTED] here"},
		{"GitHub PAT", "git remote: ghp_abcdef0123456789 access denied", "git remote: [REDACTED] access denied"},
		{"GitHub OAuth", "header X-Token: gho_qrstuv0123456789", "header X-Token: [REDACTED]"},
		{"GitHub fine-grained PAT", "token github_pat_11ABCDEFG0abcdefghij1234567890 rejected", "token [REDACTED] rejected"},
		{"GitLab PAT", "token=glpat-abcdefghij0123456789 push", "token=[REDACTED] push"},
		{"AWS access key", "key=AKIAIOSFODNN7EXAMPLE region=us-east-1", "key=[REDACTED] region=us-east-1"},
		{"AWS STS access key", "ASIAQRSTUVWXYZ012345 used by role", "[REDACTED] used by role"},
		{"Slack bot", "secret xoxb-1234567890-abcdefghij and", "secret [REDACTED] and"},
		{"multiple in one line", "ghp_abcdef0123456789 and AKIAIOSFODNN7EXAMPLE", "[REDACTED] and [REDACTED]"},
		{"HuggingFace token", "HF_TOKEN=hf_abcdefghij0123456789 set", "HF_TOKEN=[REDACTED] set"},
		{"hf_ short tail not redacted", "see hf_short for details", "see hf_short for details"},
		{"npm token", "//registry: npm_abcdefghij0123456789 used", "//registry: [REDACTED] used"},
		{"GCP access token", "Authorization: Bearer ya29.AAAA0123456789abcdef-_ZZ done", "Authorization: Bearer [REDACTED] done"},
		{"GCP token at line start", "ya29.a0AfH6SMBx1234567890abcdef rejected", "[REDACTED] rejected"},
		{"ya29. short tail not redacted", "ya29.short here", "ya29.short here"},
		{"bare ya29 prefix prose not redacted", "the ya29. prefix marks Google OAuth tokens", "the ya29. prefix marks Google OAuth tokens"},
		{"Databricks PAT", "token dapi1234567890abcdef1234567890ab done", "token [REDACTED] done"},
		{"Databricks short tail not redacted", "see dapishort for details", "see dapishort for details"},
		{"HCP Vault service token", "auth hvs.AAAAABBBBBCCCCCDDDDDEEEEEFFFFFF1234 ok", "auth [REDACTED] ok"},
		{"HCP Vault short tail not redacted", "see hvs.short here", "see hvs.short here"},
		{"Stripe live secret key", "STRIPE_SECRET_KEY=sk_live_abcdefghij0123456789 used", "STRIPE_SECRET_KEY=[REDACTED] used"},
		{"Stripe test secret key", "STRIPE_TEST_KEY=sk_test_abcdefghij0123456789 used", "STRIPE_TEST_KEY=[REDACTED] used"},
		{"Stripe secret short tail not redacted", "sk_live_short is not a key", "sk_live_short is not a key"},
		{"Stripe restricted live key", "STRIPE_KEY=rk_live_abcdefghij0123456789 used", "STRIPE_KEY=[REDACTED] used"},
		{"Stripe restricted test key", "STRIPE_KEY=rk_test_abcdefghij0123456789 used", "STRIPE_KEY=[REDACTED] used"},
		{"rk_ alongside sk_live_ in one line", "live=sk_live_abcdefghij0123456789 restricted=rk_live_abcdefghij0123456789", "live=[REDACTED] restricted=[REDACTED]"},
		{"RSA private key header", "-----BEGIN RSA PRIVATE KEY-----", "[REDACTED] RSA PRIVATE KEY-----"},
		{"EC private key header", "-----BEGIN EC PRIVATE KEY-----", "[REDACTED] EC PRIVATE KEY-----"},
		{"PKCS8 private key header", "-----BEGIN PRIVATE KEY-----", "[REDACTED] PRIVATE KEY-----"},
		{"certificate header (same prefix fires)", "-----BEGIN CERTIFICATE-----", "[REDACTED] CERTIFICATE-----"},
		{"PEM header embedded in env dump", "PRIVATE_KEY=\"-----BEGIN RSA PRIVATE KEY-----\\nMIIEo...", "PRIVATE_KEY=\"[REDACTED] RSA PRIVATE KEY-----\\nMIIEo..."},
		{"eyJ prefix NOT redacted", "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.sig", "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.sig"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactSecrets(tc.in); got != tc.want {
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
	tests := []struct{ name, in, want string }{
		{"custom secret with non-prefix value", "panic: MY_CUSTOM_SECRET=password123 leaked in traceback", "panic: MY_CUSTOM_SECRET=[REDACTED] leaked in traceback"},
		{"DB password", "connect failed DB_PASSWORD=s3cr3t!val host=db", "connect failed DB_PASSWORD=[REDACTED] host=db"},
		{"generic token key", "env TOKEN=abc.def.ghi set", "env TOKEN=[REDACTED] set"},
		{"API_KEY underscore variant", "MY_API_KEY=opaque-value-not-a-prefix done", "MY_API_KEY=[REDACTED] done"},
		{"APIKEY no underscore", "SOMEAPIKEY=zzz used", "SOMEAPIKEY=[REDACTED] used"},
		{"credential key", "AWS_CREDENTIAL=raw123 ok", "AWS_CREDENTIAL=[REDACTED] ok"},
		{"spaces around equals", "GITHUB_TOKEN = ghx_rawvalue here", "GITHUB_TOKEN = [REDACTED] here"},
		{"multiple sensitive assignments", "DB_PASSWORD=p1 and API_TOKEN=t2 both", "DB_PASSWORD=[REDACTED] and API_TOKEN=[REDACTED] both"},
		{"value already a known prefix still single redacted", "STRIPE_SECRET=sk_live_abcdefghij0123456789 used", "STRIPE_SECRET=[REDACTED] used"},
		{"quoted value with spaces fully masked", `cfg PASSWORD="my long secret" loaded`, `cfg PASSWORD="[REDACTED]" loaded`},
		{"single-quoted value with spaces fully masked", "DB_PASSWORD='p a s s' ok", "DB_PASSWORD='[REDACTED]' ok"},
		{"infix marker SECRET_KEY_BASE", "SECRET_KEY_BASE=abc123def456 boot", "SECRET_KEY_BASE=[REDACTED] boot"},
		{"run-on MYSECRETKEY", "MYSECRETKEY=opaque used", "MYSECRETKEY=[REDACTED] used"},
		{"infix PASSWORD with suffix", "USER_PASSWORD_HASH=deadbeef stored", "USER_PASSWORD_HASH=[REDACTED] stored"},
		{"TOKENIZER not masked (suffix anchor)", "TOKENIZER=bpe model", "TOKENIZER=bpe model"},
		{"AUTHOR not masked (suffix anchor)", "AUTHOR=alice commit", "AUTHOR=alice commit"},
		// Negative: benign config keys must NOT be masked.
		{"LOG_LEVEL not masked", "LOG_LEVEL=debug verbose", "LOG_LEVEL=debug verbose"},
		{"PATH not masked", "PATH=/usr/local/bin:/usr/bin running", "PATH=/usr/local/bin:/usr/bin running"},
		{"generic KEY config not masked", "sort KEY=name order", "sort KEY=name order"},
		{"no equals untouched", "the SECRET word appears in prose", "the SECRET word appears in prose"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactSecrets(tc.in); got != tc.want {
				t.Errorf("RedactSecrets(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
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
// already-scrubbed output (prefix tokens and #2165 env assignments) is a
// no-op. R234-SEC-7 (#1006), R202606-SEC-008b.
func TestRedactSecrets_Idempotent(t *testing.T) {
	for _, tc := range []struct {
		src    string
		leaked []string
	}{
		{"leaked sk-ant-api03-abcdef0123456789 inside ghp_abcdef0123456789 mid-line", []string{"sk-ant-", "ghp_"}},
		{"MY_CUSTOM_SECRET=password123 and DB_PASSWORD=hunter2 leaked", []string{"password123", "hunter2"}},
	} {
		once := RedactSecrets(tc.src)
		if twice := RedactSecrets(once); once != twice {
			t.Fatalf("not idempotent:\n  once  = %q\n  twice = %q", once, twice)
		}
		for _, s := range tc.leaked {
			if strings.Contains(once, s) {
				t.Fatalf("redactor left %q intact: %q", s, once)
			}
		}
	}
}
