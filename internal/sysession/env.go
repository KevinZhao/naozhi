package sysession

import (
	"os"
	"strings"
)

// envAlwaysPassthrough is the small set of NON-SECRET variables every
// Runner subprocess gets, regardless of the daemon-side EnvAllowlist:
//
//   - PATH:  required to find auxiliary tools the CLI may shell out to.
//   - HOME:  required for the CLI's own config discovery (and JSONL
//     storage path under ~/.claude).
//   - Backend selectors (CLAUDE_CODE_USE_BEDROCK / _USE_VERTEX /
//     ANTHROPIC_BEDROCK_BASE_URL / AWS_REGION / ANTHROPIC_MODEL etc.):
//     Runner is "claude -p" with `--setting-sources ""`, which skips
//     user/project/local settings.json.  Without these env vars Claude
//     falls back to direct-Anthropic OAuth and the daemon dies with
//     "Not logged in" on every Tick, tripping the breaker after 5
//     attempts.  The daemon must speak whatever backend the parent
//     naozhi is configured for, so they pass through transparently.
//
// Raw credential material (ANTHROPIC_API_KEY, AWS_SECRET_ACCESS_KEY,
// GOOGLE_APPLICATION_CREDENTIALS, …) is deliberately NOT in this set.
// R040034-SEC-4 (#1400): those are gated per detected backend by
// envCredsForBackend so a Bedrock-only deployment never leaks
// ANTHROPIC_API_KEY (and vice-versa) into the CLI's tool subprocesses,
// where attacker-crafted prompt content could otherwise exfiltrate them
// via a Bash invocation.
//
// Anything else (IM tokens, dashboard secrets, DB creds) MUST be
// explicitly opted in through RunnerConfig.EnvAllowlist.
var envAlwaysPassthrough = map[string]struct{}{
	"PATH": {},
	"HOME": {},

	// Backend selection — which provider the CLI talks to.
	"CLAUDE_CODE_USE_BEDROCK":       {},
	"CLAUDE_CODE_USE_VERTEX":        {},
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH": {},

	// Non-secret endpoint/region/profile plumbing (base URLs, regions,
	// AWS profile *name* — not the creds the profile resolves to).
	"ANTHROPIC_BASE_URL":         {},
	"ANTHROPIC_BEDROCK_BASE_URL": {},
	"AWS_REGION":                 {},
	"AWS_DEFAULT_REGION":         {},
	"AWS_PROFILE":                {},

	// Vertex non-secret plumbing (project id + region; the credentials
	// file path GOOGLE_APPLICATION_CREDENTIALS is gated separately).
	"ANTHROPIC_VERTEX_PROJECT_ID": {},
	"CLOUD_ML_REGION":             {},

	// Model overrides — when parent has explicit model pinning, the
	// daemon's transient claude -p must use the same one or the title
	// extractor's "haiku-class" expectations break.
	"ANTHROPIC_MODEL":                {},
	"ANTHROPIC_SMALL_FAST_MODEL":     {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":  {},
	"ANTHROPIC_DEFAULT_SONNET_MODEL": {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL":   {},
}

// Per-backend raw-credential key sets. R040034-SEC-4 (#1400). Only the
// set matching the detected backend is layered onto envAlwaysPassthrough;
// every other backend's secrets are stripped even if present in the
// parent env. This shrinks the blast radius if a future system session
// generalises its prompt source and a CLI Bash tool inherits the runner
// env.
var (
	// envCredsAnthropic — direct-Anthropic API auth.
	envCredsAnthropic = []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
	}
	// envCredsAWS — Bedrock static creds. On EC2 instance-role
	// deployments these are empty in the parent (IMDS supplies creds
	// inside the SDK) so the passthrough is a no-op there; they matter
	// for non-EC2 Bedrock setups that export static keys.
	envCredsAWS = []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
	}
	// envCredsVertex — GCP service-account credential file path.
	envCredsVertex = []string{
		"GOOGLE_APPLICATION_CREDENTIALS",
	}
)

// backendMode is the credential-gating dimension derived from the parent
// env's backend selectors. R040034-SEC-4 (#1400).
type backendMode int

const (
	backendAnthropic backendMode = iota // direct Anthropic API (default)
	backendBedrock                      // CLAUDE_CODE_USE_BEDROCK truthy
	backendVertex                       // CLAUDE_CODE_USE_VERTEX truthy
)

// envTruthy reports whether a CLAUDE_CODE_USE_* selector value enables
// that backend. Matches the CLI's own loose truthiness (any non-empty,
// non-"0"/"false" value). We intentionally treat "0"/"false"/"" as off
// so an explicitly-disabled selector doesn't mis-route the gate.
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// detectBackendFromEnv inspects the parent env ("KEY=value" slice) for
// the CLAUDE_CODE_USE_* selectors. Bedrock wins over Vertex if both are
// somehow set (matches the CLI's precedence: it checks Bedrock first).
// Absence of both selectors means direct Anthropic.
func detectBackendFromEnv(parent []string) backendMode {
	var bedrock, vertex string
	for _, kv := range parent {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_USE_BEDROCK="); ok {
			bedrock = v
		} else if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_USE_VERTEX="); ok {
			vertex = v
		}
	}
	switch {
	case envTruthy(bedrock):
		return backendBedrock
	case envTruthy(vertex):
		return backendVertex
	default:
		return backendAnthropic
	}
}

// envCredsForBackend returns the raw-credential keys that may pass
// through for the given backend. Keys outside this set are stripped even
// when present in the parent env. R040034-SEC-4 (#1400).
func envCredsForBackend(mode backendMode) []string {
	switch mode {
	case backendBedrock:
		return envCredsAWS
	case backendVertex:
		return envCredsVertex
	default:
		return envCredsAnthropic
	}
}

// filterEnv returns a "KEY=value" slice suitable for exec.Cmd.Env that
// contains only:
//   - the always-passthrough keys (see envAlwaysPassthrough)
//   - the raw-credential keys for the *detected* backend only
//     (envCredsForBackend — R040034-SEC-4 / #1400)
//   - keys whose name exactly matches an entry in allowlist
//   - keys whose name has a prefix listed in allowlist that ends with "_"
//     (a trailing underscore in an allowlist entry is the prefix-mode
//     opt-in — e.g. "ANTHROPIC_" matches every ANTHROPIC_* var, while
//     "ANTHROPIC" matches only the bare key "ANTHROPIC")
//
// Raw-credential keys for the NON-active backends are stripped
// unconditionally — even when a broad prefix allowlist entry (e.g. the
// production "ANTHROPIC_" / "AWS_" prefixes) would otherwise re-admit
// them. This is the R040034-SEC-4 gate: a Bedrock-only deployment never
// hands ANTHROPIC_API_KEY (or Vertex's GOOGLE_APPLICATION_CREDENTIALS)
// to the CLI's tool subprocesses, where attacker-crafted prompt content
// could exfiltrate it via a Bash invocation. The active backend's own
// creds still pass.
//
// Everything else from the parent environment is dropped.  This is the
// security-minded default for a daemon framework that may exec a CLI
// the user can influence (via prompt content):  even though we don't
// invoke a shell, environment leakage into the CLI's tool subprocesses
// is a real concern we'd rather pre-empt.
//
// allowlist is matched case-sensitively (matching POSIX env semantics).
// nil/empty allowlist is fine — the always-passthrough set plus the
// active backend's creds flow through.
func filterEnv(allowlist []string) []string {
	parent := os.Environ()

	// Detect the backend from the same parent env so the credential
	// gate matches whatever provider this naozhi is actually configured
	// for. R040034-SEC-4 (#1400).
	mode := detectBackendFromEnv(parent)
	allowedCreds := make(map[string]struct{}, len(envCredsForBackend(mode)))
	for _, k := range envCredsForBackend(mode) {
		allowedCreds[k] = struct{}{}
	}
	// blockedCreds = every raw-credential key NOT belonging to the
	// active backend. These are dropped regardless of allowlist so a
	// broad prefix entry can't tunnel a sibling-backend secret through.
	blockedCreds := make(map[string]struct{}, len(allCredKeys))
	for _, k := range allCredKeys {
		if _, ok := allowedCreds[k]; !ok {
			blockedCreds[k] = struct{}{}
		}
	}

	// envAlwaysPassthrough is immutable; consult it directly during
	// the per-key lookup below instead of copying its entries into
	// the per-call exact map.
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
		// Hard gate: a non-active-backend credential is never emitted,
		// even if exact/prefix allowlist or always-set would admit it.
		if _, blocked := blockedCreds[key]; blocked {
			continue
		}
		if _, ok := allowedCreds[key]; ok {
			out = append(out, kv)
			continue
		}
		if _, ok := envAlwaysPassthrough[key]; ok {
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

// allCredKeys is the union of every backend's raw-credential keys. Used
// by filterEnv to build the per-call deny set for the inactive backends.
// R040034-SEC-4 (#1400).
var allCredKeys = func() []string {
	keys := make([]string, 0, len(envCredsAnthropic)+len(envCredsAWS)+len(envCredsVertex))
	keys = append(keys, envCredsAnthropic...)
	keys = append(keys, envCredsAWS...)
	keys = append(keys, envCredsVertex...)
	return keys
}()
