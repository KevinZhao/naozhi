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
//   - keys explicitly listed in allowlist
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
	allowed := make(map[string]struct{}, len(allowlist)+len(envAlwaysPassthrough))
	for k := range envAlwaysPassthrough {
		allowed[k] = struct{}{}
	}
	for _, k := range allowlist {
		allowed[k] = struct{}{}
	}

	parent := os.Environ()
	out := make([]string, 0, len(allowed))
	for _, kv := range parent {
		// Split on first '=' only; values may contain '='.
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := kv[:idx]
		if _, ok := allowed[key]; !ok {
			continue
		}
		out = append(out, kv)
	}
	return out
}
