package session

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// ResultSink receives the reply text of every turn a session's CLI completes,
// whoever started it: the Planner's action blocks are parsed from it
// (docs/rfc/planner.md §6.2).
type ResultSink func(key, text string)

// resultNotifier is the optional hook a process offers for its turns'
// results; see bookResults.
type resultNotifier interface {
	SetOnResult(fn func(clievent.SendResult))
}

var _ resultNotifier = (*cli.Process)(nil)

// heldResultsMax bounds the results held before the first sink; a router
// whose sink is never set (tests, a tool) must not grow without end.
const heldResultsMax = 64

// resultRoute is where turn results go. Until the first SetResultSink they
// are held: startup reattaches shims and their read loops run long before
// the sink's consumers exist, and a turn finishing in that window is still
// the Planner's to act on.
type resultRoute struct {
	mu   sync.Mutex
	set  bool
	sink ResultSink
	held []heldResult
}

type heldResult struct{ key, text string }

func (rr *resultRoute) deliver(key, text string) {
	rr.mu.Lock()
	if !rr.set {
		if len(rr.held) < heldResultsMax {
			rr.held = append(rr.held, heldResult{key, text})
		}
		rr.mu.Unlock()
		return
	}
	sink := rr.sink
	rr.mu.Unlock()
	if sink != nil {
		sink(key, text)
	}
}

// SetResultSink routes every session's turn results to sink, including those
// of processes bound before the call; the results that came before the first
// call are handed to sink first, in order. nil drops them.
func (r *Router) SetResultSink(sink ResultSink) {
	rr := &r.results
	rr.mu.Lock()
	// Drained before set, so a result arriving meanwhile queues behind them.
	for len(rr.held) > 0 {
		h := rr.held[0]
		rr.held = rr.held[1:]
		rr.mu.Unlock()
		if sink != nil {
			sink(h.key, h.text)
		}
		rr.mu.Lock()
	}
	rr.held = nil
	rr.set, rr.sink = true, sink
	rr.mu.Unlock()
}

// bookResults reports proc's turn results as s's. Bound wherever
// bookActivity is (a reattach binds resultFor through SpawnReconnect's
// hooks instead); the sink runs on the read loop and must not block.
func (r *Router) bookResults(s *ManagedSession, proc processIface) {
	if n, ok := proc.(resultNotifier); ok {
		n.SetOnResult(r.resultFor(s))
	}
}

// resultFor is the result callback of s's process. Every turn's end, failed
// or not, refreshes s's last activity: this is the one hook every result
// reaches, the turn a shim held across a restart and one the CLI started
// itself included, and it runs before the turn's end pushes the session list.
// Only a reply goes to the sink.
func (r *Router) resultFor(s *ManagedSession) func(clievent.SendResult) {
	return func(res clievent.SendResult) {
		s.touchLastActive()
		if !res.Aborted && !res.IsError && !res.CLIAborted() {
			r.results.deliver(s.key, res.Text)
		}
	}
}
