package sysession

import (
	"log/slog"
	"os"
	"strings"

	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/osutil"
)

// envBaseURLKeys are always-passthrough keys whose value is an API endpoint
// URL. Values are validated by validateBaseURLValue so a tampered parent env
// cannot point the CLI at an internal/IMDS address over plain http and tunnel
// an SSRF past the settings.json-side guard (#1687).
var envBaseURLKeys = map[string]struct{}{
	"ANTHROPIC_BASE_URL":         {},
	"ANTHROPIC_BEDROCK_BASE_URL": {},
	"ANTHROPIC_VERTEX_BASE_URL":  {},
}

// validateBaseURLValue delegates to envpolicy.ValidateBaseURLValue, shared
// with cmd/naozhi's settings.json guard (#891).
func validateBaseURLValue(v string) error {
	return envpolicy.ValidateBaseURLValue(v)
}

// envProfileKeys are always-passthrough keys carrying an AWS profile *name*
// (not credentials); values are validated by isSafeProfileValue (#1617).
var envProfileKeys = map[string]struct{}{
	"AWS_PROFILE":         {},
	"AWS_DEFAULT_PROFILE": {},
}

// isSafeProfileValue delegates to envpolicy.IsSafeProfileValue (#891).
func isSafeProfileValue(v string) bool {
	return envpolicy.IsSafeProfileValue(v)
}

// envAlwaysPassthrough is the small set of NON-SECRET variables every Runner
// subprocess gets; anything else MUST be opted in via RunnerConfig.EnvAllowlist.
// sysession Runners build their own cmd.Env and never inherit access-profile
// overlays. Backend selectors (USE_BEDROCK/USE_VERTEX, base URLs, regions,
// model pins) must pass because Runner uses `--setting-sources ""` (skips
// settings.json) — without them the CLI falls back to direct-Anthropic OAuth
// and dies "Not logged in" every Tick, tripping the breaker. Raw credentials
// (ANTHROPIC_API_KEY, AWS_SECRET_ACCESS_KEY, GOOGLE_APPLICATION_CREDENTIALS,
// …) are deliberately NOT here: envCredsForBackend gates them per detected
// backend so a sibling backend's secret never reaches prompt-driven Bash (#1400).
var envAlwaysPassthrough = map[string]struct{}{
	"PATH": {},
	"HOME": {},

	// Backend selection — which provider the CLI talks to.
	"CLAUDE_CODE_USE_BEDROCK":       {},
	"CLAUDE_CODE_USE_VERTEX":        {},
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH": {},

	// Non-secret endpoint/region/profile-name plumbing.
	"ANTHROPIC_BASE_URL":         {},
	"ANTHROPIC_BEDROCK_BASE_URL": {},
	"ANTHROPIC_VERTEX_BASE_URL":  {},
	"AWS_REGION":                 {},
	"AWS_DEFAULT_REGION":         {},
	"AWS_PROFILE":                {},
	"AWS_DEFAULT_PROFILE":        {},

	// Vertex non-secret plumbing (GOOGLE_APPLICATION_CREDENTIALS is gated separately).
	"ANTHROPIC_VERTEX_PROJECT_ID": {},
	"CLOUD_ML_REGION":             {},

	// Model overrides — the daemon's transient claude -p must match the parent's pinning.
	"ANTHROPIC_MODEL":                {},
	"ANTHROPIC_SMALL_FAST_MODEL":     {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":  {},
	"ANTHROPIC_DEFAULT_SONNET_MODEL": {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL":   {},
}

// backendMode aliases envpolicy.BackendMode (#891). Only the credential set of
// the detected backend is layered onto envAlwaysPassthrough; every other
// backend's secrets are stripped even if present in the parent env (#1400).
type backendMode = envpolicy.BackendMode

// detectBackendFromEnv delegates to envpolicy.DetectBackendFromEnv (#891).
func detectBackendFromEnv(parent []string) backendMode {
	return envpolicy.DetectBackendFromEnv(parent)
}

// envCredsForBackend delegates to envpolicy.EnvCredsForBackend (#891).
func envCredsForBackend(mode backendMode) []string {
	return envpolicy.EnvCredsForBackend(mode)
}

// filterEnv returns the exec.Cmd.Env slice for a Runner: the always-passthrough
// keys, the raw-credential keys of the *detected* backend only, allowlist
// exact matches, and prefix matches for allowlist entries ending in "_"
// ("ANTHROPIC_" matches every ANTHROPIC_* var; "ANTHROPIC" only the bare key).
// Credentials of NON-active backends are stripped unconditionally — even when
// a broad prefix such as "ANTHROPIC_" / "AWS_" would re-admit them — so a
// Bedrock-only deployment never hands ANTHROPIC_API_KEY (or Vertex's
// GOOGLE_APPLICATION_CREDENTIALS) to CLI tool subprocesses where prompt
// content could exfiltrate them (#1400). Everything else in the parent env is
// dropped. Matching is case-sensitive; a nil allowlist is fine.
func filterEnv(allowlist []string) []string {
	parent := os.Environ()

	mode := detectBackendFromEnv(parent)
	allowedCreds := make(map[string]struct{}, len(envCredsForBackend(mode)))
	for _, k := range envCredsForBackend(mode) {
		allowedCreds[k] = struct{}{}
	}
	// Inactive-backend credentials are dropped regardless of allowlist so a
	// broad prefix entry can't tunnel a sibling-backend secret through.
	blockedCreds := make(map[string]struct{}, len(allCredKeys))
	for _, k := range allCredKeys {
		if _, ok := allowedCreds[k]; !ok {
			blockedCreds[k] = struct{}{}
		}
	}

	exact := make(map[string]struct{}, len(allowlist))
	var prefixes []string
	for _, k := range allowlist {
		if strings.HasSuffix(k, "_") {
			prefixes = append(prefixes, k)
			continue
		}
		exact[k] = struct{}{}
	}

	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		// Split on first '=' only; values may contain '='.
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := kv[:idx]
		// Hard gate: a non-active-backend credential is never emitted, even if
		// the allowlist or always-set would admit it.
		if _, blocked := blockedCreds[key]; blocked {
			continue
		}
		if _, ok := allowedCreds[key]; ok {
			out = append(out, kv)
			continue
		}
		if _, ok := envAlwaysPassthrough[key]; ok {
			// An unsafe profile name could redirect credential_process to a
			// malicious profile (#1617).
			if _, isProfile := envProfileKeys[key]; isProfile {
				val := kv[idx+1:]
				if !isSafeProfileValue(val) {
					slog.Warn("sysession: AWS profile env var rejected (unsafe value)",
						"key", key, "value", osutil.SanitizeForLog(val, 128))
					continue
				}
			}
			// A tampered base URL could point the CLI at an IMDS/internal http
			// endpoint and tunnel an SSRF past the settings.json guard (#1687).
			if _, isBaseURL := envBaseURLKeys[key]; isBaseURL {
				val := kv[idx+1:]
				if err := validateBaseURLValue(val); err != nil {
					slog.Warn("sysession: base-URL env var rejected (unsafe value)",
						"key", key, "value", osutil.SanitizeForLog(val, 128), "err", err)
					continue
				}
			}
			out = append(out, kv)
			continue
		}
		if _, ok := exact[key]; ok {
			out = append(out, kv)
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// allCredKeys is the union of every backend's raw-credential keys; filterEnv
// builds the inactive-backend deny set from it (#891).
var allCredKeys = envpolicy.AllCredKeys
