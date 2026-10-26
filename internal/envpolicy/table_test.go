package envpolicy

import (
	"strings"
	"testing"
)

// TestOverlayFileKeysResolveToAllowedKeys guards the *_FILE indirection map
// against typos: every concrete key a file key expands into must itself be
// overlay-allowed in the Table (the pre-table ValidateOverlayEntry carried
// this as an inline defence-in-depth check).
func TestOverlayFileKeysResolveToAllowedKeys(t *testing.T) {
	t.Parallel()
	for fileKey, concrete := range overlayFileKeys {
		if _, ok := Allowed(concrete, SourceOverlay); !ok {
			t.Errorf("overlayFileKeys[%q] = %q, which the Table does not allow for SourceOverlay", fileKey, concrete)
		}
		if _, ok := Allowed(fileKey, SourceOverlay); !ok {
			t.Errorf("file key %q itself is not overlay-allowed in the Table", fileKey)
		}
	}
}

// TestTableInvariants checks structural properties every rule must hold:
// Allowed ⊆ Specified, guards only on specified sources, and patterns use at
// most one wildcard, at an end.
func TestTableInvariants(t *testing.T) {
	t.Parallel()
	seen := map[string]Source{}
	for _, r := range Table {
		if r.Allowed&^r.Specified != 0 {
			t.Errorf("rule %q: Allowed %04b not a subset of Specified %04b", r.Pattern, r.Allowed, r.Specified)
		}
		// Two rules with the same pattern deciding the same source would tie
		// on specificity and silently resolve by slice order.
		if overlap := seen[r.Pattern] & r.Specified; overlap != 0 {
			t.Errorf("rule %q: duplicate pattern with overlapping Specified %04b", r.Pattern, overlap)
		}
		seen[r.Pattern] |= r.Specified
		for src := range r.Guards {
			if r.Specified&src == 0 {
				t.Errorf("rule %q: guard for unspecified source %04b", r.Pattern, src)
			}
		}
		if n := strings.Count(r.Pattern, "*"); n > 1 {
			t.Errorf("rule %q: more than one wildcard", r.Pattern)
		} else if n == 1 && !strings.HasPrefix(r.Pattern, "*") && !strings.HasSuffix(r.Pattern, "*") {
			t.Errorf("rule %q: wildcard must be leading or trailing", r.Pattern)
		}
	}
}

// TestTable_KeyDecisions pins the three keys the migration cared most about,
// one cell at a time, so a Table edit that flips any of them names the exact
// regression.
func TestTable_KeyDecisions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key  string
		from Source
		want bool
	}{
		// AWS_PROFILE: shim forwards (value-guarded); everyone else refuses.
		{"AWS_PROFILE", SourceShim, true},
		{"AWS_PROFILE", SourceOverlay, false},
		{"AWS_PROFILE", SourceSettings, false},
		{"AWS_PROFILE", SourceExpansion, false},
		// AWS_SHARED_CREDENTIALS_FILE: same split.
		{"AWS_SHARED_CREDENTIALS_FILE", SourceShim, true},
		{"AWS_SHARED_CREDENTIALS_FILE", SourceOverlay, false},
		{"AWS_SHARED_CREDENTIALS_FILE", SourceSettings, false},
		{"AWS_SHARED_CREDENTIALS_FILE", SourceExpansion, false},
		// ANTHROPIC_BEDROCK_BASE_URL: allowed everywhere except expansion,
		// URL-guarded per source.
		{"ANTHROPIC_BEDROCK_BASE_URL", SourceShim, true},
		{"ANTHROPIC_BEDROCK_BASE_URL", SourceOverlay, true},
		{"ANTHROPIC_BEDROCK_BASE_URL", SourceSettings, true},
		{"ANTHROPIC_BEDROCK_BASE_URL", SourceExpansion, false},
		// Settings column: inside the AWS_ / CLAUDE_ namespaces but denied.
		// AWS_ENDPOINT_URL would redirect signed requests; the two CLAUDE_
		// keys are kill switches a tampered settings.json must not flip.
		{"AWS_ROLE_ARN", SourceSettings, false},
		{"AWS_WEB_IDENTITY_TOKEN_FILE", SourceSettings, false},
		{"AWS_CONFIG_FILE", SourceSettings, false},
		{"AWS_ENDPOINT_URL", SourceSettings, false},
		{"AWS_CA_BUNDLE", SourceSettings, false},
		{"AWS_DEFAULT_PROFILE", SourceSettings, false},
		{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", SourceSettings, false},
		{"CLAUDE_CODE_USE_MOCK_RESPONSES", SourceSettings, false},
		{"CLAUDE_CONFIG_DIR", SourceSettings, true},
		// Resolution edges: *_FILE indirection, prefix extension, a shim
		// code-loading key, allow-prefix beating deny-suffix, lowercase, exact
		// proxy match, a deny prefix.
		{"ANTHROPIC_API_KEY_FILE", SourceOverlay, true},
		{"LC_ALL", SourceShim, true},
		{"NODE_OPTIONS", SourceShim, false},
		{"NAOZHI_DASHBOARD_TOKEN", SourceExpansion, true},
		{"github_token", SourceExpansion, false},
		{"http_proxy", SourceSettings, true},
		{"HTTP_PROXY_X", SourceSettings, false},
		{"AZURE_CLIENT_SECRET", SourceExpansion, false},
		// Sysession column: plumbing passes, raw credentials never do.
		{"PATH", SourceSysession, true},
		{"AWS_PROFILE", SourceSysession, true},
		{"ANTHROPIC_API_KEY", SourceSysession, false},
	}
	for _, tc := range cases {
		if _, got := Allowed(tc.key, tc.from); got != tc.want {
			t.Errorf("Allowed(%q, %04b) = %v, want %v", tc.key, tc.from, got, tc.want)
		}
	}
	for _, from := range []Source{SourceShim, SourceOverlay, SourceSettings} {
		if GuardFor("ANTHROPIC_BEDROCK_BASE_URL", from) == nil {
			t.Errorf("ANTHROPIC_BEDROCK_BASE_URL must carry a URL guard for source %04b", from)
		}
	}
	if GuardFor("AWS_PROFILE", SourceShim) == nil {
		t.Error("AWS_PROFILE must carry the profile-name guard for the shim")
	}
	if GuardFor("AWS_SHARED_CREDENTIALS_FILE", SourceShim) == nil {
		t.Error("AWS_SHARED_CREDENTIALS_FILE must carry the credential-path guard for the shim")
	}
}
