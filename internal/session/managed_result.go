package session

import "github.com/naozhi/naozhi/internal/cli"

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

// SetResultSink routes every session's turn results to sink, including those
// of processes bound before the call. nil drops them.
func (r *Router) SetResultSink(sink ResultSink) {
	if sink == nil {
		r.results.Store(nil)
		return
	}
	r.results.Store(&sink)
}

// bookResults reports proc's turn results as s's. Bound wherever
// bookActivity is; the sink runs on the read loop and must not block.
func (r *Router) bookResults(s *ManagedSession, proc processIface) {
	n, ok := proc.(resultNotifier)
	if !ok {
		return
	}
	n.SetOnResult(func(text string) {
		if sink := r.results.Load(); sink != nil {
			(*sink)(s.key, text)
		}
	})
}
