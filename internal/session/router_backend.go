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

// backendStore groups the backend/policy fields of Router. Everything but the
// manifest cache is fixed once NewRouter returns and read without a lock; the
// manifest cache carries its own.
type backendStore struct {
	wrapper *cli.Wrapper // default (legacy single-backend) wrapper
	// runtimes holds one BackendRuntime per backend ID — the row that replaced
	// six parallel map[backendID]→property tables (G2 #2666). See
	// backend_runtime.go.
	runtimes map[string]*BackendRuntime
	// perBackendWrappers records whether the composition root supplied
	// per-backend wrappers. Distinct from len(runtimes), which config alone can
	// make non-empty — wrapperFor's legacy branch needs the former.
	perBackendWrappers bool
	defaultBackend     string // backend ID used when AgentOpts.Backend is empty
	// backendIDs caches BackendIDs' ordering; computed once in NewRouter.
	backendIDs []string
	model      string
	extraArgs  []string
	// manifests caches agent-reported model lists (BackendModelManifest).
	manifests manifestCache
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
	if !r.bkStore.perBackendWrappers {
		id := backend
		if id == "" && r.bkStore.wrapper != nil {
			id = r.bkStore.wrapper.BackendID
		}
		return r.bkStore.wrapper, id
	}
	if backend != "" {
		if w := r.bkStore.runtime(backend).Wrapper; w != nil {
			return w, backend
		}
	}
	if r.bkStore.defaultBackend != "" {
		if w := r.bkStore.runtime(r.bkStore.defaultBackend).Wrapper; w != nil {
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
	return computeBackendIDs(r.bkStore.wrapper, r.bkStore.backendWrappers(), r.bkStore.defaultBackend)
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
	if !r.bkStore.perBackendWrappers {
		if id == "" || r.bkStore.wrapper == nil || r.bkStore.wrapper.BackendID == id || (id == "claude" && r.bkStore.wrapper.BackendID == "") {
			return r.bkStore.wrapper
		}
		return nil
	}
	if id == "" {
		id = r.bkStore.defaultBackend
	}
	return r.bkStore.runtime(id).Wrapper
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
// the next spawn only; live sessions are not migrated. Empty clears.
func (r *Router) SetSessionBackend(key, backend string) {
	var ok bool
	r.ss.Update(func(tx sessTx) { ok = setPick(tx.Ext().picks.backend, key, backend) })
	if !ok {
		slog.Warn("backendOverrides at capacity; dropping override",
			"key", key, "cap", maxBackendOverrides)
	}
}

// SessionBackend returns the backend override for key, or "" if none.
func (r *Router) SessionBackend(key string) (backend string) {
	r.ss.View(func(v sessView) { backend = v.Ext().picks.backend[key] })
	return backend
}

// setPick sets (or, for "", clears) key's entry in a pick map. It reports
// false when a brand-new key is refused because the map is at
// maxBackendOverrides; updating an existing key never hits the cap.
func setPick(m map[string]string, key, value string) bool {
	if value == "" {
		delete(m, key)
		return true
	}
	if _, existing := m[key]; !existing && len(m) >= maxBackendOverrides {
		return false
	}
	m[key] = value
	return true
}

// SetSessionAccessProfile remembers the access profile picked for a new
// session (RFC project-access-profile §8.2). One-shot: consumed on the next
// spawn. Empty clears. Mirrors SetSessionBackend including the cap.
func (r *Router) SetSessionAccessProfile(key, profile string) {
	var ok bool
	r.ss.Update(func(tx sessTx) { ok = setPick(tx.Ext().picks.accessProfile, key, profile) })
	if !ok {
		slog.Warn("accessProfileOverrides at capacity; dropping override",
			"key", key, "cap", maxBackendOverrides)
	}
}

// SessionAccessProfile returns the access-profile override for key, or "".
func (r *Router) SessionAccessProfile(key string) (profile string) {
	r.ss.View(func(v sessView) { profile = v.Ext().picks.accessProfile[key] })
	return profile
}

// CLIPath returns the CLI binary path for health checks.
func (r *Router) CLIPath() string {
	if r.bkStore.wrapper == nil {
		return ""
	}
	return r.bkStore.wrapper.CLIPath
}

// BackendDefaults is the merged per-backend spawn configuration — one backend's
// row, assembled from the per-property maps.
//
// Exported so the shim drift path can be handed a VALUE instead of three loose
// strings. It used to be unexported and ShimListDrift took
// (defaultModel, defaultEffort, defaultArgs); cmd/naozhi then assembled those
// itself with the wrong precedence and reported healthy sessions as drifted
// (#2668). A typed seam makes the mismatch unrepresentable.
type BackendDefaults struct {
	Model string
	// Args is returned WITHOUT copying — callers that mutate must copy first.
	Args []string
	// Effort is "" for backends that report no tier support.
	Effort string
}

// MergeBackendDefaults is the precedence rule for a backend's spawn defaults:
// the router-level cli.model / cli.args are the base, replaced by the
// per-backend cli.backends[].model / .args when those are non-empty
// (config.go documents that relationship as "overrides cli.model for this
// backend"). Effort has no base tier.
//
// Pure and exported because TWO paths must agree: the live spawn (via
// Router.backendDefaultsFor, reading the router's maps) and the offline drift
// view (cmd/naozhi's `naozhi shim`, reading config directly). They did not —
// the CLI took the per-backend value with no fallback, so a backend inheriting
// the global cli.args produced a spurious DRIFT telling the operator to restart
// a healthy session (#2668). That is the same failure mode #739 and #2427 were,
// on a third code path; the fix is for both to call one function.
func MergeBackendDefaults(routerModel string, routerArgs []string, backendModel string, backendArgs []string, backendEffort string) BackendDefaults {
	model := routerModel
	if backendModel != "" {
		model = backendModel
	}
	args := routerArgs
	if len(backendArgs) > 0 {
		args = backendArgs
	}
	return BackendDefaults{Model: model, Args: args, Effort: backendEffort}
}

// backendDefaultsFor returns the merged spawn configuration for backendID.
// Both resolveSpawnParamsLocked and the shim drift detector must end up with
// the same values, which is why the precedence lives in MergeBackendDefaults
// rather than here (#739, #2668).
func (r *Router) backendDefaultsFor(backendID string) BackendDefaults {
	rt := r.bkStore.runtime(backendID)
	return MergeBackendDefaults(
		r.bkStore.model, r.bkStore.extraArgs,
		rt.Model, rt.ExtraArgs, rt.Effort,
	)
}

// BackendModelManifest returns the model list the dashboard popover offers for
// a backend ("" = router default). Tiers: (1) runtime manifest from any LIVE
// process, cached in bkStore.manifests; (2) configured
// cli.backends[].models; (3) observedModels. Nil when no tier has data.
// Reads the session table in one View; the cache has its own lock.
func (r *Router) BackendModelManifest(backendID string) (models []cli.ModelInfo) {
	if backendID == "" {
		backendID = r.bkStore.defaultBackend
	}
	r.ss.View(func(v sessView) { models = r.backendModelManifest(v, backendID) })
	return models
}

func (r *Router) backendModelManifest(v sessView, backendID string) []cli.ModelInfo {
	for _, s := range v.All() {
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
			r.bkStore.manifests.set(backendID, models)
			break
		}
	}
	if m := r.bkStore.manifests.get(backendID); len(m) > 0 {
		return m
	}
	if lst := r.bkStore.runtime(backendID).ConfiguredModels; len(lst) > 0 {
		out := make([]cli.ModelInfo, 0, len(lst))
		for _, id := range lst {
			out = append(out, cli.ModelInfo{ID: id})
		}
		return out
	}
	return r.observedModels(v, backendID)
}

// observedModels returns the deduped model ids seen for backendID in a
// stable order (router default first, then sessions' Model() / TuningModel()
// sorted). Nil when nothing observed.
func (r *Router) observedModels(v sessView, backendID string) []cli.ModelInfo {
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
	for _, s := range v.All() {
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
