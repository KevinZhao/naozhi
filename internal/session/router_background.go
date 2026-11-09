package session

import "github.com/naozhi/naozhi/internal/cli"

// BackgroundCheck reports whether s's background tasks (a Bash
// run_in_background, a Monitor, an Agent subtask; bgtask's judgment, wired
// by wireup) still run. A session whose tasks run is not idle: the idle
// expiry and the eviction for max_procs leave its CLI alone, as closing it
// kills them. bgtask's quiet limits bound how long a lost end holds one.
// It may run inside a table transaction, so it must not call back into the
// Router.
type BackgroundCheck func(s *ManagedSession) bool

// SetBackgroundCheck sets the judgment of a session's background tasks; nil
// judges none running.
func (r *Router) SetBackgroundCheck(fn BackgroundCheck) {
	if fn == nil {
		r.background.Store(nil)
		return
	}
	r.background.Store(&fn)
}

// backgroundOpen reports whether s's background tasks still run.
func (r *Router) backgroundOpen(s *ManagedSession) bool {
	fn := r.background.Load()
	return fn != nil && (*fn)(s)
}

// EndSink receives the end of a session's CLI process, the CLI gone with
// whatever it ran in the background: key is the session, reason its
// deathReason (DeathReasonIdleTimeout, DeathReasonEvicted, a process-level
// reason, "" unknown). A CLI naozhi detached (it keeps running in its
// shim) or whose shim outlived the socket did not end and is not reported.
// It runs on the read loop's end, possibly inside a table transaction,
// and must not block nor call back into the Router.
type EndSink func(key, reason string)

// SetEndSink routes the ends of every session's processes to sink,
// including those bound before the call. nil drops them.
func (r *Router) SetEndSink(sink EndSink) {
	if sink == nil {
		r.ends.Store(nil)
		return
	}
	r.ends.Store(&sink)
}

// endFor is the end callback of s's process (bookProcessEnd).
func (r *Router) endFor(s *ManagedSession, proc processIface) func(cli.ProcessEnd) {
	return func(end cli.ProcessEnd) {
		if end.Detached || end.ShimLive {
			return
		}
		sink := r.ends.Load()
		if sink == nil {
			return
		}
		reason := loadAtomicString(&s.deathReason)
		if reason == "" && proc != nil {
			reason = proc.DeathReason()
		}
		(*sink)(s.key, reason)
	}
}
