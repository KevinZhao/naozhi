package config

import (
	"os"
	"strings"
	"testing"
)

// TestExpandEnvVars_RefusesYAMLBreakingValues pins R237-SEC-4 / #637:
// an env value containing newline / carriage return / tab / other ASCII
// control bytes must NOT be substituted raw into the YAML payload — that
// would let an attacker who controls an env var inject arbitrary YAML
// keys (e.g. forge a dashboard_token field). The placeholder is left
// intact so downstream containsEnvPlaceholder validation fails loudly.
func TestExpandEnvVars_RefusesYAMLBreakingValues(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"newline_lf", "value\ndashboard_token: pwned"},
		{"newline_crlf", "value\r\ninjected: yes"},
		{"bare_cr", "v\rinjected: yes"},
		{"tab", "v\tinjected"},
		{"nul", "v\x00injected"},
		{"vertical_tab", "v\x0binjected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("NAOZHI_INJECTION_TEST_VAR", tc.val)
			defer os.Unsetenv("NAOZHI_INJECTION_TEST_VAR")

			placeholder := "${NAOZHI_INJECTION_TEST_VAR}"
			got := string(expandEnvVars([]byte(placeholder)))
			if got != placeholder {
				t.Errorf("expandEnvVars(%q) = %q, want placeholder preserved (got expansion of YAML-breaking value)", placeholder, got)
			}
			// Defence-in-depth: even if expansion happened, the dangerous
			// payload must not appear verbatim in the output.
			if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
				t.Errorf("expandEnvVars output %q contains control char from env value", got)
			}
		})
	}
}

// TestExpandEnvVars_AllowsBenignValues sanity-checks that the
// containsYAMLBreakingByte filter doesn't over-block ordinary printable
// values that are common in config (URLs, base64, mixed punctuation,
// non-ASCII UTF-8).
func TestExpandEnvVars_AllowsBenignValues(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"plain_word", "hello"},
		{"url", "https://example.com/path?x=y"},
		{"base64_token", "QUJDREVGR0g="},
		{"utf8_chinese", "你好世界"},
		{"punctuation_heavy", "a:b/c@d=e_f-g.h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("NAOZHI_BENIGN_TEST_VAR", tc.val)
			defer os.Unsetenv("NAOZHI_BENIGN_TEST_VAR")
			placeholder := "${NAOZHI_BENIGN_TEST_VAR}"
			got := string(expandEnvVars([]byte(placeholder)))
			if got != tc.val {
				t.Errorf("expandEnvVars(%q) = %q, want %q (benign value should expand)", placeholder, got, tc.val)
			}
		})
	}
}
