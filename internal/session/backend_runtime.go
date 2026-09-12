package session

import "github.com/naozhi/naozhi/internal/cli"

// backend_runtime.go — one backend's configuration as one value (G2 #2666).
//
// backendStore kept six parallel map[backendID]→property tables. Adding a
// backend meant inserting a row into each, and reading "this backend's
// configuration" meant reading six maps in the right order — which is how #2668
// happened: a second consumer read a subset with different precedence and
// reported healthy sessions as drifted.
//
// The row view already existed as the transient BackendDefaults that
// mergeArgvLayers consumes; it was recomputed from the columns on every call and
// covered only three of the properties. BackendRuntime is that row made the
// stored representation.

// BackendRuntime is everything the router knows about one backend.
//
// Model and ExtraArgs are the PER-BACKEND overrides (cli.backends[].model /
// .args); empty means "inherit the router-level cli.model / cli.args", and
// MergeBackendDefaults is what applies that precedence. Storing the override
// rather than the resolved value keeps the two tiers distinguishable, which the
// drift view needs.
type BackendRuntime struct {
	// Wrapper is nil for a backend that is configured but has no spawnable
	// wrapper; callers must treat that as "no backend available".
	Wrapper *cli.Wrapper

	Model     string
	ExtraArgs []string
	// Effort has no router-wide base on purpose: the composition root already
	// folded cli.effort in and dropped tier-less backends.
	// docs/rfc/kiro-effort-control.md
	Effort string

	// ConfiguredModels is the operator-declared manifest (cli.backends[].models).
	// docs/rfc/dashboard-model-effort-control.md §4.2.
	ConfiguredModels []string
	// Manifest caches the agent-reported model list; survives process death.
	// Mutated under r.mu's write lock via backendStore.runtimeMut.
	Manifest []cli.ModelInfo
}

// runtime returns id's row, or the ZERO value when the backend is unknown.
//
// A value copy, not the pointer, because every reader wants a snapshot and the
// zero value is exactly what the six per-property maps yielded for a missing key.
// Returning a pointer would put a nil deref between the caller and that
// behaviour — the typed-nil shape that produced #377, #2551 and #2561.
//
// Caller holds r.mu (read is enough).
func (b *backendStore) runtime(id string) BackendRuntime {
	if rt := b.runtimes[id]; rt != nil {
		return *rt
	}
	return BackendRuntime{}
}

// runtimeMut returns the stored pointer for in-place mutation, allocating the row
// when the backend has none yet. Only the model-manifest cache uses this, and
// only under r.mu's WRITE lock.
func (b *backendStore) runtimeMut(id string) *BackendRuntime {
	if b.runtimes == nil {
		b.runtimes = make(map[string]*BackendRuntime)
	}
	rt := b.runtimes[id]
	if rt == nil {
		rt = &BackendRuntime{}
		b.runtimes[id] = rt
	}
	return rt
}

// initRuntimes builds the per-backend rows from the composition root's
// per-property maps. The key set is their UNION plus every wrapper: a backend can
// be configured without a wrapper (its model list is still reachable from the
// dashboard) and a wrapper can exist with no config overrides.
//
// perBackendWrappers records whether wrappers was non-empty, separately from
// len(runtimes). wrapperFor's legacy single-wrapper branch keys on "were there
// per-backend wrappers", and runtimes can be non-empty from config alone — so
// deriving that from len(runtimes) would silently take the wrong branch.
func (b *backendStore) initRuntimes(
	wrappers map[string]*cli.Wrapper,
	models map[string]string,
	extraArgs map[string][]string,
	efforts map[string]string,
	modelLists map[string][]string,
) {
	b.perBackendWrappers = len(wrappers) > 0
	b.runtimes = make(map[string]*BackendRuntime, len(wrappers))
	for id, w := range wrappers {
		b.runtimeMut(id).Wrapper = w
	}
	for id, m := range models {
		b.runtimeMut(id).Model = m
	}
	for id, a := range extraArgs {
		b.runtimeMut(id).ExtraArgs = a
	}
	for id, e := range efforts {
		b.runtimeMut(id).Effort = e
	}
	for id, l := range modelLists {
		b.runtimeMut(id).ConfiguredModels = l
	}
}

// backendWrappers rebuilds the id→wrapper view for the few callers that iterate
// or index wrappers specifically (computeBackendIDs, shimManagers). Kept as a
// projection rather than a stored second copy so the two cannot drift.
//
// Caller holds r.mu.
func (b *backendStore) backendWrappers() map[string]*cli.Wrapper {
	if len(b.runtimes) == 0 {
		return nil
	}
	out := make(map[string]*cli.Wrapper, len(b.runtimes))
	for id, rt := range b.runtimes {
		if rt.Wrapper != nil {
			out[id] = rt.Wrapper
		}
	}
	return out
}
