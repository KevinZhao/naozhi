package sysession

// onRunStartedHolder wraps an OnRunStarted callback for use with
// atomic.Pointer (which requires a concrete pointee type — function
// values can't go directly into atomic.Pointer because they are not
// comparable beyond nil/non-nil).
//
// The wrapping makes the "function-value through atomic pointer" idiom
// unmistakable to readers and aligns with the same shape used by
// internal/session/router_core.go (onChangeHolder /
// onKeyRetiredHolder) and internal/upstream/connector.go (discoverFn /
// previewFn). One pattern across naozhi for late-wired single-callback
// fields keeps reviewers from re-deriving the rationale per call site.
//
// R246-ARCH-6 / R242-GO-16 lineage: this struct exists only to satisfy
// atomic.Pointer's type constraints; the field is intentionally kept
// public-style (lowercase but stable) so future helpers can construct
// holders without going through SetCallbacks.
type onRunStartedHolder struct {
	fn func(DaemonRunStartedEvent)
}

// onRunEndedHolder mirrors onRunStartedHolder for the per-Tick end
// hook. Stored on Manager.onRunEnded; constructed by SetCallbacks and
// unwrapped by loadOnRunEnded. See onRunStartedHolder godoc for the
// shared atomic.Pointer rationale.
type onRunEndedHolder struct {
	fn func(DaemonRunEndedEvent)
}
