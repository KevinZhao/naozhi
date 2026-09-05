package envpolicy

import (
	"strings"
	"testing"
)

// Copies of the five retired allow/deny lists exactly as they stood before
// the single-table migration (#2531), plus their resolution logic. The
// equivalence tests below pin every Table cell against these: if an edit to
// Table changes any verdict the old lists produced, the tests fail. Policy
// CHANGES must update both the Table and these fixtures, in the same PR, on
// purpose.

var retiredShimEnvAllowedPrefixes = []string{
	"HOME=", "USER=", "LOGNAME=", "PATH=", "SHELL=",
	"TERM=", "TMPDIR=", "TMP=", "TEMP=",
	"LANG=", "LC_", "TZ=",
	"XDG_",
	"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=",
	"CLAUDE_CODE_OAUTH_TOKEN=",
	"ANTHROPIC_MODEL=", "ANTHROPIC_BASE_URL=",
	"ANTHROPIC_BEDROCK_BASE_URL=",
	"CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=",
	"CLAUDE_BIN=", "CLAUDE_MODEL=",
	"AWS_REGION=", "AWS_DEFAULT_REGION=",
	"AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=",
	"AWS_PROFILE=", "AWS_SHARED_CREDENTIALS_FILE=", "AWS_CONFIG_FILE=",
	"AWS_ROLE_ARN=", "AWS_WEB_IDENTITY_TOKEN_FILE=",
	"AWS_ENDPOINT_URL=", "AWS_BEDROCK_ENDPOINT=",
	"SSH_AUTH_SOCK=",
	"GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=",
	"GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL=",
	"GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
	"GIT_DIR=", "GIT_WORK_TREE=",
	"GOPATH=", "GOROOT=", "GOBIN=",
	"CARGO_HOME=", "RUSTUP_HOME=",
	"NODE_ENV=",
	"NPM_CONFIG_REGISTRY=", "NPM_TOKEN=",
	"PYTHONDONTWRITEBYTECODE=", "PYTHONUNBUFFERED=",
	"CONDA_PREFIX=", "CONDA_DEFAULT_ENV=", "CONDA_SHLVL=",
	"JAVA_HOME=",
}

func retiredShimKeyAllowed(kv string) bool {
	for _, prefix := range retiredShimEnvAllowedPrefixes {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

var retiredOverlayAllowedKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":            true,
	"ANTHROPIC_BEDROCK_BASE_URL":    true,
	"ANTHROPIC_MODEL":               true,
	"ANTHROPIC_AUTH_TOKEN":          true,
	"ANTHROPIC_API_KEY":             true,
	"CLAUDE_CODE_OAUTH_TOKEN":       true,
	"CLAUDE_CODE_USE_BEDROCK":       true,
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH": true,
	"AWS_REGION":                    true,
	"AWS_DEFAULT_REGION":            true,
}

var retiredOverlayFileKeys = map[string]bool{
	"ANTHROPIC_AUTH_TOKEN_FILE":    true,
	"ANTHROPIC_API_KEY_FILE":       true,
	"CLAUDE_CODE_OAUTH_TOKEN_FILE": true,
}

func retiredOverlayKeyAllowed(key string) bool {
	return retiredOverlayAllowedKeys[key] || retiredOverlayFileKeys[key]
}

var retiredClaudeEnvAllowedPrefixes = []string{
	"ANTHROPIC_",
	"CLAUDE_",
	"AWS_",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

var retiredAWSEnvDenyList = map[string]bool{
	"AWS_ROLE_ARN":                true,
	"AWS_WEB_IDENTITY_TOKEN_FILE": true,
	"AWS_SHARED_CREDENTIALS_FILE": true,
	"AWS_CONFIG_FILE":             true,
	"AWS_PROFILE":                 true,
	"AWS_DEFAULT_PROFILE":         true,
	"AWS_CA_BUNDLE":               true,
	"AWS_ENDPOINT_URL":            true,
}

var retiredClaudeEnvDenyList = map[string]bool{
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true,
	"CLAUDE_CODE_USE_MOCK_RESPONSES":           true,
}

func retiredSettingsKeyAllowed(key string) bool {
	matched := false
	for _, p := range retiredClaudeEnvAllowedPrefixes {
		if strings.HasSuffix(p, "_") {
			if strings.HasPrefix(key, p) {
				matched = true
				break
			}
		} else if key == p {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	return !retiredAWSEnvDenyList[key] && !retiredClaudeEnvDenyList[key]
}

var retiredEnvExpansionDenyPrefixes = []string{
	"ANTHROPIC_", "CLAUDE_", "AWS_", "AZURE_", "GCP_", "GOOGLE_", "OCI_",
	"OPENAI_", "GITHUB_TOKEN", "GH_TOKEN", "OPENROUTER_", "MISTRAL_",
	"HUGGINGFACE_", "HUGGING_FACE_", "SECRET_", "PASSWORD_",
}

var retiredEnvExpansionDenySuffixes = []string{
	"_SECRET", "_PASSWORD", "_PASSWD", "_TOKEN", "_APIKEY", "_API_KEY",
	"_ACCESS_KEY", "_SECRET_KEY", "_PRIVATE_KEY", "_CREDENTIALS",
}

var retiredEnvExpansionAllowPrefixes = []string{
	"NAOZHI_", "FEISHU_", "SLACK_", "PC_", "IM_",
}

func retiredExpansionAllowed(key string) bool {
	upper := strings.ToUpper(key)
	for _, p := range retiredEnvExpansionDenyPrefixes {
		if strings.HasPrefix(upper, p) {
			return false
		}
	}
	for _, p := range retiredEnvExpansionAllowPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, s := range retiredEnvExpansionDenySuffixes {
		if strings.HasSuffix(upper, s) {
			return false
		}
	}
	return true
}

// equivalenceCorpus derives probe keys from every pattern in Table plus
// every key the retired lists named, plus hand-picked edge shapes: for each
// prefix pattern a bare and an extended probe, for each suffix pattern a
// prefixed probe, lowercase variants, and the incident keys.
func equivalenceCorpus() []string {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for _, r := range Table {
		switch {
		case strings.HasSuffix(r.Pattern, "*"):
			p := strings.TrimSuffix(r.Pattern, "*")
			add(p)
			add(p + "PROBE")
		case strings.HasPrefix(r.Pattern, "*"):
			s := strings.TrimPrefix(r.Pattern, "*")
			add("PROBE" + s)
			add("NAOZHI" + s) // allow-prefix vs deny-suffix precedence
			add("LC" + s)     // shim namespace vs expansion suffix isolation
		default:
			add(r.Pattern)
		}
	}
	for _, p := range retiredShimEnvAllowedPrefixes {
		add(strings.TrimSuffix(p, "="))
	}
	for k := range retiredOverlayAllowedKeys {
		add(k)
	}
	for k := range retiredOverlayFileKeys {
		add(k)
	}
	for k := range retiredAWSEnvDenyList {
		add(k)
	}
	for k := range retiredClaudeEnvDenyList {
		add(k)
	}
	for _, p := range retiredEnvExpansionDenyPrefixes {
		add(p)
		add(p + "PROBE")
	}
	for _, k := range []string{
		"AWS_BEDROCK_ENDPOINT", "AWS_MFA_TOKEN", "AWS_PROFILE2",
		"ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_SMALL_FAST_MODEL",
		"CLAUDE_CODE_EXTRA", "NODE_OPTIONS", "NPM_CONFIG_PREFIX",
		"GIT_SSH_COMMAND", "PYTHONPATH", "NVM_DIR", "VIRTUAL_ENV",
		"HOME", "HOMEBREW_PREFIX", "PATHEXT", "SECRET", "MY_DATABASE_PASSWORD",
		"NAOZHI_DASHBOARD_TOKEN", "FEISHU_APP_SECRET", "GITHUB_TOKEN",
		"github_token", "aws_profile", "anthropic_api_key", "no_proxy",
		"NO_PROXY", "http_proxy", "HTTP_PROXY", "REDIS_URL", "MYSQL_PWD",
	} {
		add(k)
	}
	return keys
}

// TestTableEquivalence_Shim pins the Table's shim column against the retired
// prefix allowlist for every corpus key, in the "KEY=value" shape the filter
// actually sees.
func TestTableEquivalence_Shim(t *testing.T) {
	t.Parallel()
	for _, key := range equivalenceCorpus() {
		// Both shapes: "KEY=value" and a malformed entry without '=' — the
		// retired list stored exact keys as "KEY=" prefixes, so namespace
		// rules match without '=' but exact-key rules must not.
		for _, kv := range []string{key + "=x", key} {
			got := shimKeyAllowed(kv)
			want := retiredShimKeyAllowed(kv)
			if got != want {
				t.Errorf("shim column: entry %q allowed=%v, retired allowlist says %v", kv, got, want)
			}
		}
	}
}

// TestTableEquivalence_Overlay pins the overlay column against the retired
// OverlayAllowedKeys + *_FILE indirection set.
func TestTableEquivalence_Overlay(t *testing.T) {
	t.Parallel()
	for _, key := range equivalenceCorpus() {
		_, got := Allowed(key, SourceOverlay)
		want := retiredOverlayKeyAllowed(key)
		if got != want {
			t.Errorf("overlay column: key %q allowed=%v, retired allowlist says %v", key, got, want)
		}
	}
}

// TestTableEquivalence_Settings pins the settings column against the retired
// prefix allowlist minus the AWS/CLAUDE deny lists.
func TestTableEquivalence_Settings(t *testing.T) {
	t.Parallel()
	for _, key := range equivalenceCorpus() {
		_, got := Allowed(key, SourceSettings)
		want := retiredSettingsKeyAllowed(key)
		if got != want {
			t.Errorf("settings column: key %q allowed=%v, retired lists say %v", key, got, want)
		}
	}
}

// TestTableEquivalence_Expansion pins the expansion column against the
// retired deny-prefix > allow-prefix > deny-suffix > default-allow chain.
func TestTableEquivalence_Expansion(t *testing.T) {
	t.Parallel()
	for _, key := range equivalenceCorpus() {
		_, got := Allowed(key, SourceExpansion)
		want := retiredExpansionAllowed(key)
		if got != want {
			t.Errorf("expansion column: key %q allowed=%v, retired chain says %v", key, got, want)
		}
	}
}

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
