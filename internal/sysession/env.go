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
//
// Anything else (IM tokens, dashboard secrets, DB creds) MUST be
// explicitly opted in through RunnerConfig.EnvAllowlist.
var envAlwaysPassthrough = map[string]struct{}{
	"PATH": {},
	"HOME": {},
}

// filterEnv returns a "KEY=value" slice suitable for exec.Cmd.Env that
// contains only:
//   - the always-passthrough keys (PATH, HOME)
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
// nil/empty allowlist is fine — only PATH and HOME flow through.
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
