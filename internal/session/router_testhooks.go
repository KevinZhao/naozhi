package session

// Test-only hooks attached to *Router.
//
// R245-ARCH-29 (#882) flagged that these fields lived inside the main Router
// struct declaration in router_core.go (~50-field block) where they are
// indistinguishable from production state. Moving them here doesn't remove
// the field allocation cost in production builds (Go requires every field of
// a struct to be declared together — the language has no `//go:build !test`
// gate for individual fields), but it isolates the test-surface so:
//
//   1. Reviewers grepping router_core.go for "production state" no longer
//      have to skip past test-only fields.
//   2. A future contributor who wants to convert these to a build-tagged
//      gate (the issue's preferred end-state) has a single file to fold
//      into the build tag rather than excising fragments out of a 600-line
//      struct.
//   3. The fields' godoc lives next to the helper methods that fire them
//      (firePhase3SpawnHook / firePhase3BackfillHook below), so a reader
//      always sees the hook's contract together with its trigger site.
//
// Production cost of leaving the fields in place: two `func()` slots
// (~16 B each on amd64) per Router instance. There is exactly one Router
// per process, so the total cost is 32 B — negligible compared to the
// readability win of the dedicated file.

// firePhase3SpawnHook fires the spawn-path Phase-3 hook if any test installed
// one. Production callers (auto_chain_router.go::maybeAttachAutoChainOnSpawn)
// invoke this between Phase-2 candidate selection and Phase-3 r.mu
// re-acquisition; tests use SetTestHookBeforeSpawnPhase3 (defined in
// export_test.go) to install a synchronisation barrier that pins TOCTOU
// race orderings without time.Sleep.
//
// Returning a no-op when the hook is nil keeps the production hot path
// branch-predictable: a single nil compare on the function pointer.
func (r *Router) firePhase3SpawnHook() {
	if hook := r.testHookBeforeSpawnPhase3; hook != nil {
		hook()
	}
}

// firePhase3BackfillHook mirrors firePhase3SpawnHook for the
// runAutoChainBackfillOnce path. Same contract, same no-op fast path.
func (r *Router) firePhase3BackfillHook() {
	if hook := r.testHookBeforeBackfillPhase3; hook != nil {
		hook()
	}
}
