package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/naozhi/naozhi/internal/envpolicy"
)

// AccessProfile is the session-layer view of a named auth/upstream overlay
// (RFC project-access-profile). It is a decoupled copy of config.AccessProfile
// — the session package must not import config — carrying only what spawn
// resolution needs: the raw env overlay (still holding *_FILE references) and
// the default model. The router is handed a map[id]AccessProfile at
// construction (RouterConfig.AccessProfiles), populated by the cmd wiring.
type AccessProfile struct {
	// Env is the raw overlay map straight from config: keys are the
	// overlay-settable env names (and *_FILE indirection keys); values are
	// literals or, for *_FILE keys, host file paths. resolveEnvOverlay expands
	// the file references at spawn time.
	Env map[string]string
	// DefaultModel participates in model resolution below an explicit
	// per-request / PlannerModel choice and above backend.DefaultModel.
	DefaultModel string
	// DefaultBackend optionally pins a backend inside the profile.
	DefaultBackend string
}

// resolveEnvOverlay materialises a profile's Env map into the concrete
// "KEY"→"value" overlay handed to the shim. It:
//
//   - copies literal (non-*_FILE) entries verbatim;
//   - for each *_FILE key, reads the host file, trims trailing newlines, and
//     injects the content under the concrete secret key (e.g.
//     ANTHROPIC_AUTH_TOKEN_FILE → ANTHROPIC_AUTH_TOKEN); the *_FILE key itself
//     is NOT forwarded to the subprocess.
//
// FAIL-LOUD: a missing / unreadable *_FILE returns an error so the spawn fails
// with a clear message instead of silently falling back to the global default
// (which would run e.g. a personal 1P project on the company Bedrock account —
// the exact mis-charge this feature prevents). The returned overlay is still
// re-gated by the shim's filterShimEnv, so this function does not itself
// enforce the allowlist — it only expands and reads.
//
// I/O NOTE: this reads files, so it MUST be called OUTSIDE r.mu (spawnSession
// invokes it after releasing the lock).
func resolveEnvOverlay(env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if concrete, ok := envpolicy.ResolvedFileKey(k); ok {
			data, err := os.ReadFile(v)
			if err != nil {
				return nil, fmt.Errorf("access profile: reading %s from %q: %w", k, v, err)
			}
			out[concrete] = strings.TrimRight(string(data), "\r\n")
			continue
		}
		out[k] = v
	}
	return out, nil
}
