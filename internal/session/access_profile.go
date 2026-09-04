package session

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/envpolicy"
)

// AccessProfile is the session-layer view of a named auth/upstream overlay
// (RFC project-access-profile): a decoupled copy of config.AccessProfile
// (session must not import config) carrying only what spawn resolution needs.
// Handed to the router as RouterConfig.AccessProfiles by the cmd wiring.
type AccessProfile struct {
	// DisplayName / ChipColor are non-sensitive dashboard metadata, surfaced
	// via /api/access-profiles. Env values are NEVER surfaced.
	DisplayName string
	ChipColor   string
	// Env is the raw overlay straight from config; *_FILE values are host file
	// paths that resolveEnvOverlay expands at spawn time.
	Env map[string]string
	// DefaultModel participates in model resolution below an explicit
	// per-request / PlannerModel choice and above backend.DefaultModel.
	DefaultModel string
	// DefaultBackend optionally pins a backend inside the profile.
	DefaultBackend string
}

// AccessProfileInfo is the non-sensitive projection of an access profile the
// dashboard consumes (/api/access-profiles). It NEVER carries env values or
// secrets (RFC project-access-profile §8).
type AccessProfileInfo struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name,omitempty"`
	ChipColor      string `json:"chip_color,omitempty"`
	DefaultModel   string `json:"default_model,omitempty"`
	DefaultBackend string `json:"default_backend,omitempty"`
	// SecretOK reports whether every *_FILE the profile references exists and
	// is readable, so the picker can warn BEFORE a message is sent. Always
	// true for profiles with no *_FILE reference.
	SecretOK bool `json:"secret_ok"`
}

// AccessProfileInfos returns the non-sensitive projection of every configured
// access profile, sorted by ID for stable UI ordering; nil when none are
// configured. The SecretOK preflight stats each *_FILE (cheap, picker-open only).
func (r *Router) AccessProfileInfos() []AccessProfileInfo {
	// The registry is copy-on-write (AddAccessProfile swaps the whole map
	// pointer under the write lock), so an RLock reader sees either the old
	// or the new map whole — never a half-inserted entry.
	r.mu.RLock()
	profiles := r.accessProfiles
	r.mu.RUnlock()
	if len(profiles) == 0 {
		return nil
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AccessProfileInfo, 0, len(ids))
	for _, id := range ids {
		ap := profiles[id]
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

// DefaultAccessProfile returns the profile ID applied to a session that
// resolves to no explicit profile (RouterConfig.DefaultAccessProfile). Empty
// means the global-baseline fallthrough (no overlay). Read-only after
// NewRouter, so no lock is taken.
func (r *Router) DefaultAccessProfile() string {
	return r.defaultAccessProfile
}

// HasAccessProfile reports whether an access profile with the given id is
// registered. RLock, copy-on-write safe (see AccessProfileInfos).
func (r *Router) HasAccessProfile(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.accessProfiles[id]
	return ok
}

// AddAccessProfile registers a new access profile at runtime so a just-created
// profile works WITHOUT a naozhi restart. Copy-on-write: builds a fresh map and
// swaps the pointer under the write lock so RLock readers always observe a
// consistent whole map. Errors on a duplicate id — callers must never silently
// overwrite an operator's existing profile.
//
// The caller must persist config.yaml FIRST and fail the request if that
// write fails, so disk and memory cannot diverge on partial success.
func (r *Router) AddAccessProfile(id string, ap AccessProfile) error {
	if id == "" {
		return fmt.Errorf("access profile id is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.accessProfiles[id]; exists {
		return fmt.Errorf("access profile %q already exists", id)
	}
	next := make(map[string]AccessProfile, len(r.accessProfiles)+1)
	for k, v := range r.accessProfiles {
		next[k] = v
	}
	next[id] = ap
	r.accessProfiles = next
	return nil
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
// overlay handed to the shim: literals are copied; each *_FILE key is read,
// trailing newlines trimmed, and injected under the concrete secret key
// (ANTHROPIC_AUTH_TOKEN_FILE → ANTHROPIC_AUTH_TOKEN); the *_FILE key itself
// is NOT forwarded.
//
// FAIL-LOUD: a missing / unreadable *_FILE returns an error so the spawn fails
// instead of silently falling back to the global default (the exact mis-charge
// this feature prevents). The shim's filterShimEnv still enforces the allowlist.
// Reads files, so it MUST be called OUTSIDE r.mu.
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
