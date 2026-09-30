package envpolicy

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Copies of the three retired sysession lists exactly as they stood in
// internal/sysession/env.go before this column existed, plus the value checks
// they drove. The equivalence tests below pin the SourceSysession column
// against them key by key: if a Table edit changes any verdict the old maps
// produced, this fails. Policy CHANGES must update the Table and these fixtures
// in the same PR, on purpose. Same discipline as the four columns migrated in
// #2531 — this is the security-relevant one, since a Runner's `claude -p` runs
// prompt-driven Bash with whatever env survives here.

var retiredSysessionAlwaysPassthrough = map[string]bool{
	"PATH": true,
	"HOME": true,

	// Backend selection — which provider the CLI talks to.
	"CLAUDE_CODE_USE_BEDROCK":       true,
	"CLAUDE_CODE_USE_VERTEX":        true,
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH": true,

	// Non-secret endpoint/region/profile-name plumbing.
	"ANTHROPIC_BASE_URL":         true,
	"ANTHROPIC_BEDROCK_BASE_URL": true,
	"ANTHROPIC_VERTEX_BASE_URL":  true,
	"AWS_REGION":                 true,
	"AWS_DEFAULT_REGION":         true,
	"AWS_PROFILE":                true,
	"AWS_DEFAULT_PROFILE":        true,

	// Vertex non-secret plumbing (GOOGLE_APPLICATION_CREDENTIALS is gated separately).
	"ANTHROPIC_VERTEX_PROJECT_ID": true,
	"CLOUD_ML_REGION":             true,

	// Model overrides — the daemon's transient claude -p must match the parent's pinning.
	"ANTHROPIC_MODEL":                true,
	"ANTHROPIC_SMALL_FAST_MODEL":     true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":  true,
	"ANTHROPIC_DEFAULT_SONNET_MODEL": true,
	"ANTHROPIC_DEFAULT_OPUS_MODEL":   true,
}

var retiredSysessionProfileKeys = map[string]bool{
	"AWS_PROFILE":         true,
	"AWS_DEFAULT_PROFILE": true,
}

var retiredSysessionBaseURLKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":         true,
	"ANTHROPIC_BEDROCK_BASE_URL": true,
	"ANTHROPIC_VERTEX_BASE_URL":  true,
}

// TestTableEquivalence_Sysession pins the sysession column against the retired
// always-passthrough map for the whole corpus, both directions.
func TestTableEquivalence_Sysession(t *testing.T) {
	t.Parallel()
	corpus := equivalenceCorpus()
	// The corpus is built from the other columns' fixtures, so make sure every
	// retired sysession key is actually exercised.
	inCorpus := map[string]bool{}
	for _, k := range corpus {
		inCorpus[k] = true
	}
	for k := range retiredSysessionAlwaysPassthrough {
		if !inCorpus[k] {
			corpus = append(corpus, k)
		}
	}

	for _, key := range corpus {
		_, got := Allowed(key, SourceSysession)
		want := retiredSysessionAlwaysPassthrough[key]
		if got != want {
			t.Errorf("sysession column: key %q allowed=%v, retired always-passthrough says %v", key, got, want)
		}
	}
}

// TestTableEquivalence_SysessionGuards pins which keys carry a value check and
// which check it is. The kinds matter: a profile name gets the charset guard
// (credential_process injection), a base URL gets ValidateBaseURLValue — NOT
// the shim's stricter endpoint guard, which would reject the private-IP
// endpoints NAOZHI_ALLOW_PRIVATE_BASE_URL exists to permit.
func TestTableEquivalence_SysessionGuards(t *testing.T) {
	t.Parallel()
	for key := range retiredSysessionAlwaysPassthrough {
		kind := guardKindFor(key, SourceSysession)
		switch {
		case retiredSysessionProfileKeys[key]:
			if kind != guardProfile {
				t.Errorf("%q: sysession guard kind %v, want guardProfile", key, kind)
			}
		case retiredSysessionBaseURLKeys[key]:
			if kind != guardBaseURL {
				t.Errorf("%q: sysession guard kind %v, want guardBaseURL", key, kind)
			}
		default:
			if kind != guardNone {
				t.Errorf("%q: sysession guard kind %v, want none — an unexpected value check would reject env the daemon needs", key, kind)
			}
		}
	}
}

// Raw credentials must never be allowed by the key-level column: they are gated
// per detected backend by EnvCredsForBackend, so a sibling backend's secret
// never reaches a Runner's prompt-driven Bash.
func TestTable_SysessionRefusesRawCredentials(t *testing.T) {
	t.Parallel()
	for _, key := range AllCredKeys {
		if _, ok := Allowed(key, SourceSysession); ok {
			t.Errorf("%q is allowed for SourceSysession; raw credentials must come from EnvCredsForBackend only", key)
		}
	}
}

// The new column must not have moved any other column's verdict. The four
// per-column equivalence tests already cover the corpus, so this pins the
// specific shape that could regress: an exact sysession-only rule shadowing a
// namespace rule another source relies on.
func TestTable_SysessionRulesDoNotShadowOtherColumns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key  string
		from Source
		want bool
	}{
		// Inside the settings-allowed CLAUDE_ / ANTHROPIC_ namespaces.
		{"CLAUDE_CODE_USE_VERTEX", SourceSettings, true},
		{"ANTHROPIC_SMALL_FAST_MODEL", SourceSettings, true},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL", SourceSettings, true},
		{"ANTHROPIC_VERTEX_PROJECT_ID", SourceSettings, true},
		// Expansion still refuses the credential namespaces.
		{"CLAUDE_CODE_USE_VERTEX", SourceExpansion, false},
		{"ANTHROPIC_SMALL_FAST_MODEL", SourceExpansion, false},
		// Not forwarded by the shim just because sysession takes them.
		{"CLAUDE_CODE_USE_VERTEX", SourceShim, false},
		{"CLOUD_ML_REGION", SourceShim, false},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL", SourceShim, false},
		// AWS_DEFAULT_PROFILE gained a sysession allow; the shim still refuses.
		{"AWS_DEFAULT_PROFILE", SourceShim, false},
		{"AWS_DEFAULT_PROFILE", SourceSysession, true},
		{"AWS_DEFAULT_PROFILE", SourceSettings, false},
	}
	for _, tc := range cases {
		if _, got := Allowed(tc.key, tc.from); got != tc.want {
			t.Errorf("Allowed(%q, %04b) = %v, want %v", tc.key, tc.from, got, tc.want)
		}
	}
}

// GuardErrorReason is what stands between a guard error and the log, so its
// value-collapsing is worth pinning directly: url.Parse errors carry the whole
// input, which for a settings.json value can be 4 KiB with control bytes in it.
func TestGuardErrorReason_CollapsesTheQuotedValue(t *testing.T) {
	t.Parallel()
	long := "http://internal-host\r\nFORGED=evil/" + strings.Repeat("x", 400)
	got := GuardErrorReason(ValidateBaseURLValue(long))

	if strings.Contains(got, "FORGED") || strings.Contains(got, strings.Repeat("x", 20)) {
		t.Errorf("reason echoes the offending value: %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("reason carries raw CR/LF: %q", got)
	}
	if len(got) > 200 {
		t.Errorf("reason is %d bytes, want bounded: %q", len(got), got)
	}
	// The classification has to survive, or the report says nothing useful.
	if !strings.Contains(got, "invalid control character") {
		t.Errorf("reason lost the guard's classification: %q", got)
	}

	// A guard error that quotes only a host keeps the sentence readable.
	plain := GuardErrorReason(ValidateBaseURLValue("http://evil.test"))
	if strings.Contains(plain, "evil.test") {
		t.Errorf("reason echoes the host from the value: %q", plain)
	}
	if !strings.Contains(plain, "non-loopback") {
		t.Errorf("reason lost the guard's classification: %q", plain)
	}
}

// The three credential sets are derived from the Table's Cred marks, so this
// pins the derivation against literals: a mark added to or removed from a row
// shows up here as a set that no longer matches, rather than as a backend that
// silently starts or stops receiving a secret. CLAUDE_CODE_OAUTH_TOKEN is the
// subscription login, a direct-Anthropic credential like the API key: without
// the mark the Runner's CLAUDE_ prefix handed it to a Bedrock daemon's
// prompt-driven Bash (#1400).
func TestCredSets_MatchTheRetiredLiterals(t *testing.T) {
	t.Parallel()
	retired := map[BackendMode][]string{
		BackendAnthropic: {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"},
		BackendBedrock:   {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"},
		BackendVertex:    {"GOOGLE_APPLICATION_CREDENTIALS"},
	}
	for mode, want := range retired {
		got := EnvCredsForBackend(mode)
		if len(got) != len(want) {
			t.Errorf("mode %v: %d cred keys, retired literal had %d: %v vs %v", mode, len(got), len(want), got, want)
			continue
		}
		gotSet := map[string]bool{}
		for _, k := range got {
			gotSet[k] = true
		}
		for _, k := range want {
			if !gotSet[k] {
				t.Errorf("mode %v: %q missing from the derived set %v", mode, k, got)
			}
		}
	}
	// AllCredKeys is the union the inactive-backend deny set is built from; a
	// key that fell out of it would stop being stripped for other backends.
	if got, want := len(AllCredKeys), 7; got != want {
		t.Errorf("AllCredKeys has %d keys, want %d: %v", got, want, AllCredKeys)
	}
}

// %q escapes a quote inside the value as `\"`; the collapser used to treat that
// as the closing quote, so everything after it leaked into the reason (#2973).
// The three escape shapes: an escaped quote, an escaped backslash, and a value
// whose last byte is a backslash.
func TestGuardErrorReason_HonoursEscapesInsideTheQuotedValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"escaped quote", `ht"tpSECRET`, `scheme "…" not allowed; use https://`},
		{"escaped backslash", `ht\SECRET`, `scheme "…" not allowed; use https://`},
		{"trailing backslash", `htSECRET\`, `scheme "…" not allowed; use https://`},
		{"escaped quote then quote", `a"b"SECRET`, `scheme "…" not allowed; use https://`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("scheme %q not allowed; use https://", tc.value)
			got := GuardErrorReason(err)
			if got != tc.want {
				t.Errorf("GuardErrorReason(%s) = %q, want %q", err.Error(), got, tc.want)
			}
			if strings.Contains(got, "SECRET") {
				t.Errorf("reason leaks the value: %q", got)
			}
		})
	}

	// url.Parse renders the whole input with %q too, so the end-to-end guard
	// path has the same exposure.
	got := GuardErrorReason(ValidateBaseURLValue("http://x\"y\rSECRET.example/"))
	if strings.Contains(got, "SECRET") {
		t.Errorf("reason leaks the value through url.Parse: %q", got)
	}
}

// The bound is applied per rune, not per byte: the ellipsis the collapser
// writes is three bytes, so a byte cut could land inside it and emit invalid
// UTF-8 into a log line.
func TestGuardErrorReason_TruncatesOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	msg := strings.Repeat("é", 159) + `"v"tail`
	got := GuardErrorReason(fmt.Errorf("%s", msg))
	if !utf8.ValidString(got) {
		t.Fatalf("reason is not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != 161 { // 160 kept + the trailing ellipsis
		t.Errorf("rune count = %d, want 161: %q", utf8.RuneCountInString(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated reason lacks the ellipsis: %q", got)
	}
}
