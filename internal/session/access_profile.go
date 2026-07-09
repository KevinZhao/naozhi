package session

import (
	"fmt"
	"os"
	"sort"
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
	// DisplayName / ChipColor are the operator-facing label + chip colour the
	// dashboard renders. Non-sensitive; surfaced verbatim via
	// /api/access-profiles. Env values are NEVER surfaced.
	DisplayName string
	ChipColor   string
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

// AccessProfileInfo is the non-sensitive projection of an access profile the
// dashboard consumes (/api/access-profiles). It NEVER carries env values or
// secrets — only the id, display metadata, and resolved defaults. RFC
// project-access-profile §8 (chip/picker read this; env stays server-side).
type AccessProfileInfo struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name,omitempty"`
	ChipColor      string `json:"chip_color,omitempty"`
	DefaultModel   string `json:"default_model,omitempty"`
	DefaultBackend string `json:"default_backend,omitempty"`
	// SecretOK reports whether every *_FILE the profile references currently
	// exists and is readable (preflight, RFC P1-f). False lets the picker gray
	// out / warn on a profile whose token file is missing BEFORE a message is
	// sent. Profiles with no *_FILE reference are always true.
	SecretOK bool `json:"secret_ok"`
}

// AccessProfileInfos returns the non-sensitive projection of every configured
// access profile, sorted by ID for stable UI ordering. Empty when none are
// configured (single-auth deployments). The SecretOK preflight stats each
// *_FILE reference — cheap (a handful of profiles × ≤2 files) and only called
// at picker-open time, mirroring /api/cli/backends.
func (r *Router) AccessProfileInfos() []AccessProfileInfo {
	if len(r.accessProfiles) == 0 {
		return nil
	}
	ids := make([]string, 0, len(r.accessProfiles))
	for id := range r.accessProfiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AccessProfileInfo, 0, len(ids))
	for _, id := range ids {
		ap := r.accessProfiles[id]
		out = append(out, AccessProfileInfo{
			ID:             id,
			DisplayName:    ap.DisplayName,
			ChipColor:      ap.ChipColor,
			DefaultModel:   ap.DefaultModel,
			DefaultBackend: ap.DefaultBackend,
			SecretOK:       accessProfileSecretsOK(ap.Env),
		})
	}
	return out
}

// accessProfileSecretsOK reports whether every *_FILE reference in the overlay
// exists and is readable. Non-*_FILE keys are ignored. Empty env → true.
func accessProfileSecretsOK(env map[string]string) bool {
	for k, v := range env {
		if _, ok := envpolicy.ResolvedFileKey(k); ok {
			if _, err := os.Stat(v); err != nil {
				return false
			}
		}
	}
	return true
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
