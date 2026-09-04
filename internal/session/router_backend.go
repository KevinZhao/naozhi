// Package session router backend selection: wrapperFor / managerFor /
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

// backendStore groups the backend/policy fields of Router (#383). It carries
// NO lock of its own: config fields are read-only after NewRouter and read
// lock-free; the override / manifest maps are mutated only under r.mu write
// lock. The lint recurses one level so each inner field carries its own
// per-domain annotation.
type backendStore struct {
	// 读写: backend (wrapperFor/CLIName/CLIVersion/CLIPath), core (init), lifecycle (spawn), shim (shimManagers)
	wrapper *cli.Wrapper // default (legacy single-backend) wrapper
	// 读写: backend (wrapperFor/managerFor/BackendIDs/BackendWrapper), core (init), lifecycle (spawn), shim (shimManagers)
	wrappers map[string]*cli.Wrapper // backend ID → wrapper (nil in legacy mode)
	// 读写: backend (DefaultBackend/wrapperFor/BackendWrapper/BackendIDs), core (init), lifecycle (resolveSpawnParams)
	defaultBackend string // backend ID used when AgentOpts.Backend is empty
	// backendIDs caches BackendIDs' ordering; computed once in NewRouter.
	// 读写: backend (BackendIDs), core (init)
	backendIDs []string
	// 读写: backend (backendDefaultsFor base), core (init)
	model string
	// 读写: backend (backendDefaultsFor base), core (init)
	extraArgs []string
	// backendModels / backendExtraArgs override model and args per backend ID.
	// 读写: backend (backendDefaultsFor override), core (init)
	backendModels map[string]string
	// 读写: backend (backendDefaultsFor override), core (init)
	backendExtraArgs map[string][]string
	// backendEfforts holds the thinking-effort tier per backend ID. No
	// router-wide base on purpose: the composition root already folded
	// cli.effort in and dropped tier-less backends. AgentOpts.Effort layers
	// above it unfiltered (harmless). docs/rfc/kiro-effort-control.md
	// 读写: backend (backendDefaultsFor), core (init)
	backendEfforts map[string]string
	// backendOverrides: per-session backend picks keyed by full session key
	// (with agent suffix) so two sessions on one chat can run different backends.
	// 读写: backend (Set/GetSessionBackend), core (init), lifecycle (unregisterSessionLocked / resolveSpawnParams consume / RenameSession)
	backendOverrides map[string]string
	// accessProfileOverrides: per-session access-profile picks (RFC
	// project-access-profile §8.2), one-shot like backendOverrides. Empty
	// value = global default.
	// 读写: backend (Set/GetSessionAccessProfile), core (init), lifecycle (unregisterSessionLocked / resolveSpawnParams consume)
	accessProfileOverrides map[string]string
	// configuredModelLists: operator-declared manifest per backend ID
	// (cli.backends[].models). docs/rfc/dashboard-model-effort-control.md §4.2.
	// 读写: backend (BackendModelManifest), core (init)
	configuredModelLists map[string][]string
	// modelManifests caches the agent-reported model list per backend ID;
	// survives process death. Mutated under r.mu write lock.
	// 读写: backend (BackendModelManifest), core (init)
	modelManifests map[string][]cli.ModelInfo
}

// maxModelBytes caps model identifiers, which flow into the CLI child's
// `--model` argv. Keep in sync with project's plannerModelRe.
const maxModelBytes = 128

// modelRe constrains `--model` to a non-flag-like charset. The leading
// `^[A-Za-z0-9]` prevents flag injection (`--model -rce`); relaxing it would
// re-open that surface. `:` and `/` are allowed inside for Bedrock IDs / ARNs;
// `[` `]` for the claude CLI's context-window suffix (`…-fable-5-1[1m]`),
// which the CLI reports back as the session model (see tuningspec.modelNameRe).
var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]\-]*$`)

// validateModel returns nil for empty (router default) or any string matching
// modelRe under the byte cap; otherwise ErrInvalidModel.
func validateModel(model string) error {
	if model == "" {
		return nil
	}
	if len(model) > maxModelBytes {
		return fmt.Errorf("%w: exceeds %d bytes", ErrInvalidModel, maxModelBytes)
	}
	if !modelRe.MatchString(model) {
		return fmt.Errorf("%w: must be alphanumeric with optional dots, colons, slashes, brackets, hyphens or underscores", ErrInvalidModel)
	}
	return nil
}

// ValidateModelID is the exported form of validateModel for callers that
// pre-flight a model identifier before GetOrCreate (#2433).
func ValidateModelID(model string) error { return validateModel(model) }

// ErrInvalidModel is returned when AgentOpts.Model fails validateModel.
// Callers should map it to an HTTP 400 or IM error reply.
var ErrInvalidModel = errors.New("invalid model identifier")

// backendRe mirrors modelRe with a tighter cap. The value flows into slog attrs
// and state files; without this gate a WS client could land C0/C1 bytes in logs.
var backendRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]*$`)

const maxBackendBytes = 64

// ErrInvalidBackend is returned when AgentOpts.Backend fails validateBackend.
var ErrInvalidBackend = errors.New("invalid backend identifier")

// validateBackend returns nil for empty or any string matching backendRe under
// the cap. Unknown-but-well-formed backends still fall back via wrapperFor.
func validateBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if len(backend) > maxBackendBytes {
		return fmt.Errorf("%w: exceeds %d bytes", ErrInvalidBackend, maxBackendBytes)
	}
	if !backendRe.MatchString(backend) {
		// Don't echo the regex: this surfaces in IM replies and slog attrs.
		return fmt.Errorf("%w: must be alphanumeric with optional dots, hyphens or underscores", ErrInvalidBackend)
	}
	return nil
}

// CLIName exposes the wrapper's CLI display name for status endpoints.
// Returns empty when no wrapper is wired (tests, early boot).
func (r *Router) CLIName() string {
	if r.bkStore.wrapper != nil {
		return r.bkStore.wrapper.CLIName
	}
	return ""
}

// CLIVersion exposes the default backend's CLI version, preferring the live
// version observed from a spawned process so host upgrades show without restart.
func (r *Router) CLIVersion() string {
	if r.bkStore.wrapper != nil {
		return r.bkStore.wrapper.EffectiveVersion()
	}
	return ""
}

// wrapperFor selects the wrapper for the requested backend ID (empty = router
// default) and returns (wrapper, effectiveID). Callers must treat a nil
// wrapper as "no backend available" and fail fast.
func (r *Router) wrapperFor(backend string) (*cli.Wrapper, string) {
	if len(r.bkStore.wrappers) == 0 {
		id := backend
		if id == "" && r.bkStore.wrapper != nil {
			id = r.bkStore.wrapper.BackendID
		}
		return r.bkStore.wrapper, id
	}
	if backend != "" {
		if w, ok := r.bkStore.wrappers[backend]; ok {
			return w, backend
		}
	}
	if r.bkStore.defaultBackend != "" {
		if w, ok := r.bkStore.wrappers[r.bkStore.defaultBackend]; ok {
			return w, r.bkStore.defaultBackend
		}
	}
	// Last resort pairs r.bkStore.wrapper with its OWN BackendID so callers
	// never see a non-empty ID alongside a nil wrapper.
	if r.bkStore.wrapper != nil {
		return r.bkStore.wrapper, r.bkStore.wrapper.BackendID
	}
	return nil, ""
}

// managerFor returns the shim.Manager for the given backend ID (empty = router
// default). Returns nil when none is configured, so callers must guard.
func (r *Router) managerFor(backend string) *shim.Manager {
	w, _ := r.wrapperFor(backend)
	if w == nil {
		return nil
	}
	return w.ShimManager
}

// BackendIDs returns the backend IDs the router can spawn against, default
// first. Returns a defensive copy so callers cannot mutate the cache.
func (r *Router) BackendIDs() []string {
	if r.bkStore.backendIDs != nil {
		out := make([]string, len(r.bkStore.backendIDs))
		copy(out, r.bkStore.backendIDs)
		return out
	}
	return computeBackendIDs(r.bkStore.wrapper, r.bkStore.wrappers, r.bkStore.defaultBackend)
}

// DefaultBackend returns the backend ID used when no explicit backend is
// requested. May be empty for test-only routers without a wrapper.
func (r *Router) DefaultBackend() string {
	if r.bkStore.defaultBackend != "" {
		return r.bkStore.defaultBackend
	}
	if r.bkStore.wrapper != nil {
		return r.bkStore.wrapper.BackendID
	}
	return ""
}

// BackendWrapper returns the wrapper registered for the given backend ID, or
// nil if none matches. For read-only metadata (CLIName, CLIVersion, CLIPath).
func (r *Router) BackendWrapper(id string) *cli.Wrapper {
	if len(r.bkStore.wrappers) == 0 {
		if id == "" || r.bkStore.wrapper == nil || r.bkStore.wrapper.BackendID == id || (id == "claude" && r.bkStore.wrapper.BackendID == "") {
			return r.bkStore.wrapper
		}
		return nil
	}
	if id == "" {
		id = r.bkStore.defaultBackend
	}
	return r.bkStore.wrappers[id]
}

// computeBackendIDs builds the dashboard-stable ordering used by BackendIDs:
// default backend first, remaining IDs sorted ascending.
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

// maxBackendOverrides caps the per-key override maps so an authenticated
// dashboard user cannot exhaust memory by POSTing unique keys: abandoned picks
// are only cleared on spawn / Reset / Remove and the send-limiter bounds burst
// rate, not cumulative growth.
const maxBackendOverrides = 1024

// SetSessionBackend remembers the backend picked for a new session. Applied on
// the next spawnSession only; live sessions are not migrated. Empty clears.
func (r *Router) SetSessionBackend(key, backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if backend == "" {
		delete(r.bkStore.backendOverrides, key)
		return
	}
	// Updating an existing key never hits the cap; only brand-new inserts do.
	if _, existing := r.bkStore.backendOverrides[key]; !existing && len(r.bkStore.backendOverrides) >= maxBackendOverrides {
		slog.Warn("backendOverrides at capacity; dropping override",
			"key", key, "cap", maxBackendOverrides)
		return
	}
	r.bkStore.backendOverrides[key] = backend
}

// SessionBackend returns the backend override for key, or "" if none.
func (r *Router) SessionBackend(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bkStore.backendOverrides[key]
}

// SetSessionAccessProfile remembers the access profile picked for a new
// session (RFC project-access-profile §8.2). One-shot: consumed on the next
// spawnSession. Empty clears. Mirrors SetSessionBackend including the cap.
func (r *Router) SetSessionAccessProfile(key, profile string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bkStore.accessProfileOverrides == nil {
		r.bkStore.accessProfileOverrides = make(map[string]string)
	}
	if profile == "" {
		delete(r.bkStore.accessProfileOverrides, key)
		return
	}
	if _, existing := r.bkStore.accessProfileOverrides[key]; !existing && len(r.bkStore.accessProfileOverrides) >= maxBackendOverrides {
		slog.Warn("accessProfileOverrides at capacity; dropping override",
			"key", key, "cap", maxBackendOverrides)
		return
	}
	r.bkStore.accessProfileOverrides[key] = profile
}

// SessionAccessProfile returns the access-profile override for key, or "".
func (r *Router) SessionAccessProfile(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bkStore.accessProfileOverrides[key]
}

// CLIPath returns the CLI binary path for health checks.
func (r *Router) CLIPath() string {
	if r.bkStore.wrapper == nil {
		return ""
	}
	return r.bkStore.wrapper.CLIPath
}

// backendDefaults is the merged per-backend spawn configuration.
type backendDefaults struct {
	Model string
	// Args is returned WITHOUT copying — callers that mutate must copy first.
	Args []string
	// Effort is "" for backends that report no tier support.
	Effort string
}

// backendDefaultsFor returns the merged spawn configuration for backendID:
// router-level model / extraArgs as base, replaced by the per-backend
// backendModels / backendExtraArgs entry when non-empty. Effort has no base
// (see backendEfforts). Both resolveSpawnParamsLocked and the shim drift
// detector must use this helper, or every restart would read surviving kiro
// shims as arg-drift and needlessly restart them (#739).
func (r *Router) backendDefaultsFor(backendID string) backendDefaults {
	model := r.bkStore.model
	if bm, ok := r.bkStore.backendModels[backendID]; ok && bm != "" {
		model = bm
	}
	args := r.bkStore.extraArgs
	if ba, ok := r.bkStore.backendExtraArgs[backendID]; ok && len(ba) > 0 {
		args = ba
	}
	return backendDefaults{
		Model: model, Args: args,
		Effort: r.bkStore.backendEfforts[backendID],
	}
}

// BackendModelManifest returns the model list the dashboard popover offers for
// a backend ("" = router default). Tiers: (1) runtime manifest from any LIVE
// process, cached in bkStore.modelManifests; (2) configured
// cli.backends[].models; (3) observedModelsLocked. Nil when no tier has data.
// Takes r.mu for WRITING because a runtime hit updates the cache.
func (r *Router) BackendModelManifest(backendID string) []cli.ModelInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	if backendID == "" {
		backendID = r.bkStore.defaultBackend
	}
	for _, s := range r.ss.sessions {
		sb := s.Backend()
		if sb == "" {
			sb = r.bkStore.defaultBackend
		}
		if sb != backendID {
			continue
		}
		proc := s.loadProcess()
		if proc == nil || !proc.Alive() {
			continue
		}
		// Optional facet: TestProcess fakes and non-ACP processes miss it.
		am, ok := proc.(interface{ AvailableModels() []cli.ModelInfo })
		if !ok {
			continue
		}
		if models := am.AvailableModels(); len(models) > 0 {
			r.bkStore.modelManifests[backendID] = models
			break
		}
	}
	if m := r.bkStore.modelManifests[backendID]; len(m) > 0 {
		return m
	}
	if lst := r.bkStore.configuredModelLists[backendID]; len(lst) > 0 {
		out := make([]cli.ModelInfo, 0, len(lst))
		for _, id := range lst {
			out = append(out, cli.ModelInfo{ID: id})
		}
		return out
	}
	return r.observedModelsLocked(backendID)
}

// observedModelsLocked returns the deduped model ids seen for backendID in a
// stable order (router default first, then sessions' Model() / TuningModel()
// sorted). Caller holds r.mu. Nil when nothing observed.
func (r *Router) observedModelsLocked(backendID string) []cli.ModelInfo {
	seen := make(map[string]bool)
	var out []cli.ModelInfo
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, cli.ModelInfo{ID: id})
	}
	add(r.backendDefaultsFor(backendID).Model)
	var rest []string
	for _, s := range r.ss.sessions {
		sb := s.Backend()
		if sb == "" {
			sb = r.bkStore.defaultBackend
		}
		if sb != backendID {
			continue
		}
		rest = append(rest, s.Model(), s.TuningModel())
	}
	slices.Sort(rest)
	for _, id := range rest {
		add(id)
	}
	return out
}
