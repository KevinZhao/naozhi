package session

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/planner/activity"
)

// ActivitySink receives the activities sessions' tool calls produce
// (planner/activity.Recorder): key is the session, dir its workspace.
type ActivitySink interface {
	Record(key, dir string, a activity.Activity)
}

// activityNotifier is the optional hook a process offers for the activities
// its tool calls produce; see bookActivity.
type activityNotifier interface {
	SetOnActivity(cwd string, fn func(activity.Activity))
}

var _ activityNotifier = (*cli.Process)(nil)

// SetActivitySink routes every session's activities to sink, including those
// of processes bound before the call (startup reconnects run before the
// Planner state is open). nil drops them.
func (r *Router) SetActivitySink(sink ActivitySink) {
	if sink == nil {
		r.activity.Store(nil)
		return
	}
	r.activity.Store(&sink)
}

// bookActivity reports the activities proc's tool calls produce in s's
// workspace as s's. Bound wherever bookCodeChanges is; no I/O, so it is safe
// inside a table transaction.
func (r *Router) bookActivity(s *ManagedSession, proc processIface) {
	n, ok := proc.(activityNotifier)
	if !ok {
		return
	}
	ws := s.Workspace()
	n.SetOnActivity(ws, func(a activity.Activity) {
		if sink := r.activity.Load(); sink != nil {
			(*sink).Record(s.key, ws, a)
		}
	})
}
