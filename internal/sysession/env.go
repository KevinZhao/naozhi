package sysession

import (
	"os"
	"strings"
)

// envAlwaysPassthrough is the small set of variables every Runner
// subprocess gets, regardless of the daemon-side EnvAllowlist:
//
//   - PATH:  required to find auxiliary tools the CLI may shell out to.
//   - HOME:  required for the CLI's own config discovery (and JSONL
//     storage path under ~/.claude).
//   - Backend selectors (CLAUDE_CODE_USE_BEDROCK / _USE_VERTEX /
//     ANTHROPIC_API_KEY / ANTHROPIC_BEDROCK_BASE_URL / AWS_REGION /
//     ANTHROPIC_MODEL etc.):  Runner is "claude -p" with
//     `--setting-sources ""`, which skips user/project/local
//     settings.json.  Without these env vars Claude falls back to
//     direct-Anthropic OAuth and the daemon dies with "Not logged in"
//     on every Tick, tripping the breaker after 5 attempts.  The
//     daemon must speak whatever backend the parent naozhi is
//     configured for, so they pass through transparently.
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

	// Anthropic direct-API auth (when used).  We pass it through but
	// the parent should NOT set it if running on Bedrock/Vertex.
	"ANTHROPIC_API_KEY":    {},
	"ANTHROPIC_AUTH_TOKEN": {},
	"ANTHROPIC_BASE_URL":   {},

	// Bedrock-specific.
	"ANTHROPIC_BEDROCK_BASE_URL": {},
	"AWS_REGION":                 {},
	"AWS_DEFAULT_REGION":         {},
	"AWS_PROFILE":                {},
	// AWS creds — when running on EC2 with an instance role these are
	// empty in the parent (IMDS provides creds inside the SDK), so the
	// passthrough is a no-op there.  Pass them through anyway for
	// non-EC2 deployments.
	"AWS_ACCESS_KEY_ID":     {},
	"AWS_SECRET_ACCESS_KEY": {},
	"AWS_SESSION_TOKEN":     {},

	// Vertex-specific.
	"ANTHROPIC_VERTEX_PROJECT_ID":    {},
	"CLOUD_ML_REGION":                {},
	"GOOGLE_APPLICATION_CREDENTIALS": {},

	// Model overrides — when parent has explicit model pinning, the
	// daemon's transient claude -p must use the same one or the title
	// extractor's "haiku-class" expectations break.
	"ANTHROPIC_MODEL":                {},
	"ANTHROPIC_SMALL_FAST_MODEL":     {},
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":  {},
	"ANTHROPIC_DEFAULT_SONNET_MODEL": {},
	"ANTHROPIC_DEFAULT_OPUS_MODEL":   {},
}

// filterEnv returns a "KEY=value" slice suitable for exec.Cmd.Env that
// contains only:
//   - the always-passthrough keys (see envAlwaysPassthrough)
//   - keys whose name exactly matches an entry in allowlist
//   - keys whose name has a prefix listed in allowlist that ends with "_"
//     (a trailing underscore in an allowlist entry is the prefix-mode
//     opt-in — e.g. "ANTHROPIC_" matches every ANTHROPIC_* var, while
//     "ANTHROPIC" matches only the bare key "ANTHROPIC")
//
// Everything else from the parent environment is dropped.  This is the
// security-minded default for a daemon framework that may exec a CLI
// the user can influence (via prompt content):  even though we don't
// invoke a shell, environment leakage into the CLI's tool subprocesses
// is a real concern we'd rather pre-empt.
//
// allowlist is matched case-sensitively (matching POSIX env semantics).
// nil/empty allowlist is fine — only the always-passthrough set flows
// through.
func filterEnv(allowlist []string) []string {
	// envAlwaysPassthrough is immutable; consult it directly during
	// the per-key lookup below instead of copying its 2 entries into
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

	parent := os.Environ()
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		// Split on first '=' only; values may contain '='.
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := kv[:idx]
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
