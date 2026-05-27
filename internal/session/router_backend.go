// Package session router backend selection methods.
//
// Extracted from router.go on 2026-05-19 as part of the router-split
// refactor (docs/design/router-split-design.md). For history prior to
// commit 1ec3b3cf058ccbdca6283bdf713160e13e7b0489, see:
//
//	git log --follow internal/session/router.go
//
// This file holds backend wrapper selection: wrapperFor / managerFor /
// BackendIDs / BackendWrapper / per-session backend overrides + the
// validators (validateModel / validateBackend) that gate per-request input.
package session

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// R188-SEC-M2: model identifiers flow into the `--model` argv of the CLI child
// process. An authenticated dashboard user (or a malicious IM planner reply)
// could inject additional flags via a whitespace-containing model string. The
// project package's plannerModelRe enforces the same pattern for planner
// config; keep the regex in sync if either changes.
const maxModelBytes = 128

// modelRe constrains the `--model` argument to a charset that is non-flag-like.
// The leading anchor `^[A-Za-z0-9]` (no leading `-`) prevents flag injection
// (e.g. `--model -rce` could otherwise be parsed by the CLI as a separate flag).
// `:` and `/` are allowed because AWS Bedrock model IDs and inference profile
// ARNs use them (e.g. `arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0`).
// R218-SEC-3 / R218B-SEC-3: keep the leading char gate strict; relaxing it to
// allow `:` or `/` at the start would re-open the flag-injection surface.
var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\-]*$`)

// validateModel returns nil for empty (use router default) or any string
// matching modelRe under the byte cap; otherwise returns ErrInvalidModel.
func validateModel(model string) error {
	if model == "" {
		return nil
	}
	if len(model) > maxModelBytes {
		return fmt.Errorf("%w: exceeds %d bytes", ErrInvalidModel, maxModelBytes)
	}
	if !modelRe.MatchString(model) {
		return fmt.Errorf("%w: must be alphanumeric with optional dots, colons, hyphens or underscores", ErrInvalidModel)
	}
	return nil
}

// ErrInvalidModel is returned when AgentOpts.Model fails validateModel.
// Callers should map it to an HTTP 400 or IM error reply.
var ErrInvalidModel = errors.New("invalid model identifier")

// backendRe mirrors the model identifier pattern but with a tighter 64-byte
// cap since backend IDs are short tags ("claude", "kiro", "gemini"). Value
// flows into slog attrs, session state JSON, and shim state file; without
// this gate a WS client could land C0/C1 bytes into structured logs.
var backendRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]*$`)

const maxBackendBytes = 64

// ErrInvalidBackend is returned when AgentOpts.Backend fails validateBackend.
var ErrInvalidBackend = errors.New("invalid backend identifier")

// validateBackend returns nil for empty (router default) or any string
// matching backendRe under the byte cap; otherwise ErrInvalidBackend. The
// actual backend->wrapper resolution still goes through wrapperFor which
// falls back to the default wrapper when the backend is unknown; this gate
// only stops shape-invalid input, not unknown backends.
func validateBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if len(backend) > maxBackendBytes {
		return fmt.Errorf("%w: exceeds %d bytes", ErrInvalidBackend, maxBackendBytes)
	}
	if !backendRe.MatchString(backend) {
		// Don't echo the regex pattern itself — this error surfaces in IM
		// replies and slog attrs where the cryptic literal pattern adds
		// noise without helping users self-diagnose. Mirrors validateModel.
		return fmt.Errorf("%w: must be alphanumeric with optional dots, hyphens or underscores", ErrInvalidBackend)
	}
	return nil
}

// CLIName exposes the wrapper's CLI display name for status endpoints.
// Returns empty when no wrapper is wired (tests, early boot).
func (r *Router) CLIName() string {
	if r.wrapper != nil {
		return r.wrapper.CLIName
	}
	return ""
}

// CLIVersion exposes the wrapper's detected CLI version for status endpoints.
// Returns empty when no wrapper is wired.
func (r *Router) CLIVersion() string {
	if r.wrapper != nil {
		return r.wrapper.CLIVersion
	}
	return ""
}

// wrapperFor selects the wrapper for the requested backend ID.
// Empty backend picks the router default. Returns (wrapper, effectiveID).
// Callers must treat a nil wrapper as "no backend available" and fail fast.
func (r *Router) wrapperFor(backend string) (*cli.Wrapper, string) {
	if len(r.wrappers) == 0 {
		id := backend
		if id == "" && r.wrapper != nil {
			id = r.wrapper.BackendID
		}
		return r.wrapper, id
	}
	if backend != "" {
		if w, ok := r.wrappers[backend]; ok {
			return w, backend
		}
	}
	if r.defaultBackend != "" {
		if w, ok := r.wrappers[r.defaultBackend]; ok {
			return w, r.defaultBackend
		}
	}
	// Last-resort fallback: return r.wrapper paired with its own
	// BackendID (not r.defaultBackend) so callers never see a non-empty
	// ID paired with a nil wrapper — that combination produced confusing
	// error messages like `spawn process (backend "claude"): no wrapper`.
	if r.wrapper != nil {
		return r.wrapper, r.wrapper.BackendID
	}
	return nil, ""
}

// managerFor returns the shim.Manager associated with the given backend ID.
// Empty backend picks the router default via wrapperFor's fallback rules.
// Used by reconnectShims's ENOENT-cleanup path (F6) to purge zombies
// without having to thread a manager reference through every call site.
// Returns nil when no wrapper/manager is configured, so callers must guard.
func (r *Router) managerFor(backend string) *shim.Manager {
	w, _ := r.wrapperFor(backend)
	if w == nil {
		return nil
	}
	return w.ShimManager
}

// BackendIDs returns the list of backend IDs the router can spawn against,
// with the default backend first. Suitable for UI enumeration.
//
// Returns a defensive copy of the cached r.backendIDs slice so callers
// cannot mutate the cache (no caller in the tree mutates today, but the
// dashboard enumeration handler does iterate without an explicit copy).
// Test routers built by struct literal skip computeBackendIDs and fall
// through to the legacy compute path below.
func (r *Router) BackendIDs() []string {
	if r.backendIDs != nil {
		out := make([]string, len(r.backendIDs))
		copy(out, r.backendIDs)
		return out
	}
	return computeBackendIDs(r.wrapper, r.wrappers, r.defaultBackend)
}

// DefaultBackend returns the backend ID used when no explicit backend is
// requested. May be empty for test-only routers without a wrapper.
func (r *Router) DefaultBackend() string {
	if r.defaultBackend != "" {
		return r.defaultBackend
	}
	if r.wrapper != nil {
		return r.wrapper.BackendID
	}
	return ""
}

// BackendWrapper returns the wrapper registered for the given backend ID,
// or nil if the router has no matching backend. Intended for callers that
// need read-only metadata (CLIName, CLIVersion, CLIPath) per backend.
func (r *Router) BackendWrapper(id string) *cli.Wrapper {
	if len(r.wrappers) == 0 {
		if id == "" || r.wrapper == nil || r.wrapper.BackendID == id || (id == "claude" && r.wrapper.BackendID == "") {
			return r.wrapper
		}
		return nil
	}
	if id == "" {
		id = r.defaultBackend
	}
	return r.wrappers[id]
}

// computeBackendIDs builds the dashboard-stable ordering used by BackendIDs:
// default backend first, remaining IDs sorted ascending. wrappers is
// constructed once in NewRouter and never mutated, so the slice is computed
// here once and cached on r.backendIDs.
func computeBackendIDs(wrapper *cli.Wrapper, wrappers map[string]*cli.Wrapper, defaultBackend string) []string {
	if len(wrappers) == 0 {
		if wrapper != nil {
			id := wrapper.BackendID
			if id == "" {
				id = "claude"
			}
			return []string{id}
		}
		return nil
	}
	out := make([]string, 0, len(wrappers))
	if defaultBackend != "" {
		if _, ok := wrappers[defaultBackend]; ok {
			out = append(out, defaultBackend)
		}
	}
	rest := make([]string, 0, len(wrappers))
	for id := range wrappers {
		if id == defaultBackend {
			continue
		}
		rest = append(rest, id)
	}
	slices.Sort(rest)
	out = append(out, rest...)
	return out
}

// maxBackendOverrides caps the per-key backend override map so an
// authenticated dashboard user cannot exhaust memory by POSTing unique keys.
// backendOverrides entries are cleared on first spawnSession / Reset /
// Remove / ResetChat for the key; abandoned picks (key chosen then never
// spawned) would otherwise accumulate indefinitely — the 30/min send-limiter
// bounds burst rate but not cumulative growth. Pick a limit that comfortably
// exceeds realistic outstanding picks (a single operator seldom has >100
// unresolved picks) while making the DoS surface trivially bounded.
const maxBackendOverrides = 1024

// SetSessionBackend remembers the backend the dashboard picked for a new
// session keyed by its full session key (including agent suffix). Only
// applied the next time spawnSession runs — existing live sessions are not
// migrated. Empty backend clears the override.
func (r *Router) SetSessionBackend(key, backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if backend == "" {
		delete(r.backendOverrides, key)
		return
	}
	// Allow existing keys to be updated without bumping against the cap
	// (operator changing their mind mid-flow) — only reject when inserting
	// a brand-new key after the limit is hit.
	if _, existing := r.backendOverrides[key]; !existing && len(r.backendOverrides) >= maxBackendOverrides {
		slog.Warn("backendOverrides at capacity; dropping override",
			"key", key, "cap", maxBackendOverrides)
		return
	}
	r.backendOverrides[key] = backend
}

// GetSessionBackend returns the backend override for key, or "" if none.
func (r *Router) GetSessionBackend(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backendOverrides[key]
}

// CLIPath returns the CLI binary path for health checks.
func (r *Router) CLIPath() string {
	if r.wrapper == nil {
		return ""
	}
	return r.wrapper.CLIPath
}

// backendDefaultsFor returns the merged (model, extraArgs) the router uses
// when spawning under backendID. Precedence:
//
//	router-level r.model / r.extraArgs (base)
//	← r.backendModels[backendID]   (replace, when non-empty)
//	← r.backendExtraArgs[backendID] (replace, when non-empty)
//
// extraArgs is returned without copying — callers that mutate (append per-
// request flags) must copy first; callers that only forward the slice may
// use it directly.
//
// R222-ARCH-14 (#739): the same lookup pattern previously appeared inline
// in resolveSpawnParamsLocked AND router_shim.classifyShimState (drift
// detection). Centralising the merge here means a future migration to a
// single Backend struct can change one helper instead of grep-replacing
// across two hot paths.
func (r *Router) backendDefaultsFor(backendID string) (string, []string) {
	model := r.model
	if bm, ok := r.backendModels[backendID]; ok && bm != "" {
		model = bm
	}
	args := r.extraArgs
	if ba, ok := r.backendExtraArgs[backendID]; ok && len(ba) > 0 {
		args = ba
	}
	return model, args
}
