package session

import (
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
)

// ResultSink receives the reply text of every turn a session's CLI completes,
// whoever started it: the Planner's action blocks are parsed from it
// (docs/rfc/planner.md §6.2).
type ResultSink func(key, text string)

// resultNotifier is the optional hook a process offers for its turns' reply
// text; see bookResults.
type resultNotifier interface {
	SetOnResult(fn func(text string))
}

var _ resultNotifier = (*cli.Process)(nil)

// earlyResultsMax bounds the results held for a sink not set yet; only turns
// that end during startup land there.
const earlyResultsMax = 64

// earlyResults holds the results that arrive before the first SetResultSink:
// the shims reconnect long before the sink's consumers are built, and a turn
// a shim carried over can end in between.
type earlyResults struct {
	mu sync.Mutex
	// set: SetResultSink ran; from then on nothing is held.
	set     bool
	pending []heldResult
}

type heldResult struct{ key, text string }

// SetResultSink routes every session's turn results to sink, including those
// of processes bound before the call; the first call hands sink the results
// that arrived before it, in order. nil drops them.
func (r *Router) SetResultSink(sink ResultSink) {
	r.early.mu.Lock()
	defer r.early.mu.Unlock()
	pending := r.early.pending
	r.early.set, r.early.pending = true, nil
	if sink == nil {
		r.results.Store(nil)
		return
	}
	// Under the lock and before the store: a result arriving meanwhile waits
	// for the lock, so it cannot overtake the held ones.
	for _, h := range pending {
		sink(h.key, h.text)
	}
	r.results.Store(&sink)
}

// resultHook reports a turn result of s's to the sink. It runs on the read
// loop, as does the sink, and must not block.
func (r *Router) resultHook(s *ManagedSession) func(text string) {
	return func(text string) {
		if sink := r.results.Load(); sink != nil {
			(*sink)(s.key, text)
			return
		}
		r.early.mu.Lock()
		defer r.early.mu.Unlock()
		if sink := r.results.Load(); sink != nil {
			(*sink)(s.key, text)
			return
		}
		if r.early.set {
			return
		}
		if len(r.early.pending) >= earlyResultsMax {
			slog.Warn("result sink not set yet; turn result dropped", "key", s.key)
			return
		}
		r.early.pending = append(r.early.pending, heldResult{s.key, text})
	}
}

// bookResults reports proc's turn results as s's. Bound wherever
// bookActivity is.
func (r *Router) bookResults(s *ManagedSession, proc processIface) {
	if n, ok := proc.(resultNotifier); ok {
		n.SetOnResult(r.resultHook(s))
	}
}

// DeliverReplayedResult hands the result sink the reply of s's turn that
// ended while naozhi was down, found in the shim's backlog at reconnect; after
// is the watermark taken as that turn began (cli.Process.DeliverReplayedResult).
// True when it delivered.
func (s *ManagedSession) DeliverReplayedResult(after cli.TurnWatermark, known bool) bool {
	p, ok := s.loadProcess().(*cli.Process)
	return ok && p != nil && p.DeliverReplayedResult(after, known)
}
