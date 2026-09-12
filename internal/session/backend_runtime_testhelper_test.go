package session

import "github.com/naozhi/naozhi/internal/cli"

// Test-only setters for what used to be six map[backendID]→property fields on
// backendStore (G2 #2666). Tests assembled those maps directly; these fan the
// same map literals into the per-backend rows, so the fixtures did not have to be
// rewritten one property at a time.
//
// REPLACE semantics, matching the field assignment they stand in for: every
// setter first clears its property on all existing rows, then applies the map.
// An additive version passed the suite except for the fixtures that assign an
// EMPTY map to clear a property — those became silent no-ops and
// TestBackendModelManifest_ObservedTier caught it.
//
// In a _test.go file on purpose: production builds rows through
// backendStore.initRuntimes from the composition root, and nothing outside tests
// should set one column at a time.

func (b *backendStore) setWrappersForTest(m map[string]*cli.Wrapper) {
	// perBackendWrappers is what wrapperFor's legacy branch keys on, and it must
	// track this map rather than len(runtimes) — config alone can populate rows.
	b.perBackendWrappers = len(m) > 0
	for _, rt := range b.runtimes {
		rt.Wrapper = nil
	}
	for id, w := range m {
		b.runtimeMut(id).Wrapper = w
	}
}

func (b *backendStore) setBackendModelsForTest(m map[string]string) {
	for _, rt := range b.runtimes {
		rt.Model = ""
	}
	for id, v := range m {
		b.runtimeMut(id).Model = v
	}
}

func (b *backendStore) setBackendExtraArgsForTest(m map[string][]string) {
	for _, rt := range b.runtimes {
		rt.ExtraArgs = nil
	}
	for id, v := range m {
		b.runtimeMut(id).ExtraArgs = v
	}
}

func (b *backendStore) setBackendEffortsForTest(m map[string]string) {
	for _, rt := range b.runtimes {
		rt.Effort = ""
	}
	for id, v := range m {
		b.runtimeMut(id).Effort = v
	}
}

func (b *backendStore) setConfiguredModelListsForTest(m map[string][]string) {
	for _, rt := range b.runtimes {
		rt.ConfiguredModels = nil
	}
	for id, v := range m {
		b.runtimeMut(id).ConfiguredModels = v
	}
}

func (b *backendStore) setModelManifestsForTest(m map[string][]cli.ModelInfo) {
	for _, rt := range b.runtimes {
		rt.Manifest = nil
	}
	for id, v := range m {
		b.runtimeMut(id).Manifest = v
	}
}
