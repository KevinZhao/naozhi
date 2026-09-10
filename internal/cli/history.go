// History wiring for cli.Wrapper: the one thing internal/cli still owns here is
// resolving a factory, because the wrapper is what knows its backend ID.
//
// The contract and registry moved to internal/history (#2649 G1-d). This file's
// header used to explain why they could not live there — "internal/history and
// the backends import cli … so cli importing them would cycle" — which was the
// dependency inversion itself: disk readers imported the process manager to
// learn what shape to be.
package cli

import (
	"github.com/naozhi/naozhi/internal/history"
)

// NewHistorySource constructs a history.Source for the supplied session using
// the factory currently registered for the wrapper's BackendID. Always
// non-nil: a nil receiver, an unregistered backend, or a nil-returning factory
// all degrade to history.Noop.
//
// The registry is consulted on every call, not cached at NewWrapper time, so
// a RegisterFactory that lands after construction (per-t.Run tests, lazily
// imported backends) is honoured; the RWMutex read is ~30ns.
func (w *Wrapper) NewHistorySource(s history.SessionView, deps history.Wiring) history.Source {
	if w == nil {
		return history.Noop{}
	}
	fn := history.PickFactory(w.BackendID)
	if fn == nil {
		// Non-empty BackendID with no factory = missing wireup import; warn
		// once so "history is empty" has an operator-visible cause (#975).
		history.WarnMissingFactory(w.BackendID)
		return history.Noop{}
	}
	src := fn(s, deps)
	if src == nil {
		return history.Noop{}
	}
	return src
}
