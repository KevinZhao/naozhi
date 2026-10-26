package envpolicy

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTable_SysessionGuardKinds pins which sysession keys carry a value check
// and which check it is. The kinds matter: a profile name gets the charset
// guard (credential_process injection), a base URL gets ValidateBaseURLValue —
// NOT the shim's stricter endpoint guard, which would reject the private-IP
// endpoints NAOZHI_ALLOW_PRIVATE_BASE_URL exists to permit. Every other
// sysession-allowed key must carry no check, or the daemon loses env it needs.
func TestTable_SysessionGuardKinds(t *testing.T) {
	t.Parallel()
	want := map[string]guardKind{
		"AWS_PROFILE":                guardProfile,
		"AWS_DEFAULT_PROFILE":        guardProfile,
		"ANTHROPIC_BASE_URL":         guardBaseURL,
		"ANTHROPIC_BEDROCK_BASE_URL": guardBaseURL,
		"ANTHROPIC_VERTEX_BASE_URL":  guardBaseURL,
	}
	for key := range want {
		if _, ok := Allowed(key, SourceSysession); !ok {
			t.Errorf("%q is not allowed for SourceSysession", key)
		}
	}
	for _, r := range Table {
		if r.Allowed&SourceSysession == 0 {
			continue
		}
		if got := guardKindFor(r.Pattern, SourceSysession); got != want[r.Pattern] {
			t.Errorf("%q: sysession guard kind %v, want %v", r.Pattern, got, want[r.Pattern])
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

// The sysession column must not have moved any other column's verdict. This
// pins the specific shape that could regress: an exact sysession-only rule
// shadowing a namespace rule another source relies on.
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
