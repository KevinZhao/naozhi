package session

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/backendstore"
)

// Test-only setters for what used to be six map[backendID]→property fields on
// backendStore (G2 #2666). Tests assembled those maps directly; these fan the
// same map literals into the per-backend rows, so the fixtures did not have to be
// rewritten one property at a time.
//
// REPLACE semantics, matching the field assignment they stand in for: every
// setter first clears its property on all existing rows, then applies the map.
// Each rebuilds the backend table (editBackendsForTest).
// An additive version passed the suite except for the fixtures that assign an
// EMPTY map to clear a property — those became silent no-ops and
// TestBackendModelManifest_ObservedTier caught it.
//
// In a _test.go file on purpose: production builds the table once through
// backendstore.New from the composition root.

// editBackendsForTest rebuilds r's backend table from its current config with
// edit applied. The table is fixed once built, so a fixture that changes one
// property builds a new one; hand-built routers start from the empty config.
func (r *Router) editBackendsForTest(edit func(c *backendstore.Config)) {
	c := r.bk.Config()
	if c.Runtimes == nil {
		c.Runtimes = map[string]BackendRuntime{}
	}
	edit(&c)
	manifests := map[string][]cli.ModelInfo{}
	for id := range c.Runtimes {
		if m := r.bk.Manifest(id); m != nil {
			manifests[id] = m
		}
	}
	r.bk = backendstore.New(c)
	for id, m := range manifests {
		r.bk.SetManifest(id, m)
	}
}

// setRowsForTest applies set to every row (clearing the property first) and
// then to each id in ids, allocating rows as needed.
func setRowsForTest[V any](r *Router, m map[string]V, set func(rt *BackendRuntime, v V)) {
	r.editBackendsForTest(func(c *backendstore.Config) {
		var zero V
		for id, rt := range c.Runtimes {
			set(&rt, zero)
			c.Runtimes[id] = rt
		}
		for id, v := range m {
			rt := c.Runtimes[id]
			set(&rt, v)
			c.Runtimes[id] = rt
		}
	})
}

func (r *Router) setWrappersForTest(m map[string]*cli.Wrapper) {
	setRowsForTest(r, m, func(rt *BackendRuntime, v *cli.Wrapper) { rt.Wrapper = v })
}

func (r *Router) setBackendModelsForTest(m map[string]string) {
	setRowsForTest(r, m, func(rt *BackendRuntime, v string) { rt.Model = v })
}

func (r *Router) setBackendExtraArgsForTest(m map[string][]string) {
	setRowsForTest(r, m, func(rt *BackendRuntime, v []string) { rt.ExtraArgs = v })
}

func (r *Router) setBackendEffortsForTest(m map[string]string) {
	setRowsForTest(r, m, func(rt *BackendRuntime, v string) { rt.Effort = v })
}

func (r *Router) setConfiguredModelListsForTest(m map[string][]string) {
	setRowsForTest(r, m, func(rt *BackendRuntime, v []string) { rt.ConfiguredModels = v })
}

func (r *Router) setModelManifestsForTest(m map[string][]cli.ModelInfo) {
	r.editBackendsForTest(func(*backendstore.Config) {})
	for id := range r.bk.Config().Runtimes {
		r.bk.SetManifest(id, nil)
	}
	for id, v := range m {
		r.bk.SetManifest(id, v)
	}
}

// setAccessProfiles publishes m as r's access-profile registry.
func setAccessProfiles(r *Router, m map[string]AccessProfile) {
	r.accessProfiles.Store(&m)
}
