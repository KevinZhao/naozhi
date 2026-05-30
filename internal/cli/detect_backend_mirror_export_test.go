package cli

// Test-only export bridge for the backend mirror drift guard.
//
// R0530-ARCH-3 / #408: knownBackends + knownBackendBinaries in detect.go are a
// hand-maintained cli-side mirror of internal/cli/backend's authoritative
// Profile registry. The drift-guard test lives in package cli_test so it can
// import internal/cli/backend (the backend package imports cli, so a
// package-cli test importing backend would form an import cycle). package
// cli_test cannot reach the unexported knownBackends/knownBackendBinaries, so
// this same-directory package-cli test file re-exports them as test-only
// symbols. No production code is touched.
var (
	// ExportedKnownBackends mirrors the unexported knownBackends slice.
	ExportedKnownBackends = knownBackends
	// ExportedKnownBackendBinaries mirrors the unexported knownBackendBinaries map.
	ExportedKnownBackendBinaries = knownBackendBinaries
)
