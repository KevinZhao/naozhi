package wireup

import (
	"fmt"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/gitinfo"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/session"
)

// userScan is how many trailing events are searched for the user's last
// message; one further back is long past custody's NearbyFor.
const userScan = 256

// CustodySources are the router's signals custody reads
// (planner/custody.Config), all deterministic: no reply text is judged.
type CustodySources struct {
	Router *session.Router
	// Outputs is the outputs index; nil leaves outputs out of progress.
	Outputs *activity.Recorder
}

// Local is custody.Config.Local: a session of this node's router.
func (c CustodySources) Local(key string) bool { return c.Router.SessionFor(key) != nil }

// Workflows is custody.Config.Workflows: the sessions with a workflow
// running.
func (c CustodySources) Workflows() []string {
	var out []string
	for _, snap := range c.Router.ListSessions() {
		if runningWorkflow(snap.Workflows) != "" {
			out = append(out, snap.Key)
		}
	}
	return out
}

// LastUser is custody.Config.LastUser: the latest "user" entry the user
// sent, not custody or the Planner.
func (c CustodySources) LastUser(key string) time.Time {
	s := c.Router.SessionFor(key)
	if s == nil {
		return time.Time{}
	}
	return lastUserAt(s.EventLastN(userScan))
}

// UserSince is task.Config.UserSince: whether the user sent key a message
// after t. The session's own record says it however long the turn since
// ran; the event log answers for a message from before a restart.
func (c CustodySources) UserSince(key string, t time.Time) bool {
	s := c.Router.SessionFor(key)
	if s == nil {
		return false
	}
	if s.LastUserSent().After(t) {
		return true
	}
	return !lastUserAt(s.EventEntriesSince(t.UnixMilli())).IsZero()
}

// lastUserAt is when the latest entry the user sent was written.
func lastUserAt(entries []clievent.EventEntry) time.Time {
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.Type == clievent.KindUser && e.Source == "" {
			return unixMilli(e.Time)
		}
	}
	return time.Time{}
}

// Progress is custody.Config.Progress: the workspace's git HEAD, the
// session's newest output, its workflows' phases and the user's last
// message, as one string that changes when any of them does.
func (c CustodySources) Progress(key string) string {
	s := c.Router.SessionFor(key)
	if s == nil {
		return ""
	}
	snap := s.Snapshot()
	var b strings.Builder
	b.WriteString("head=" + gitinfo.Commit(snap.Workspace))
	if c.Outputs != nil {
		if o := c.Outputs.Outputs(key, 1); len(o) > 0 {
			fmt.Fprintf(&b, ";out=%s%s@%d", o[0].Path, o[0].URL, o[0].Time.UnixMilli())
		}
	}
	for _, w := range snap.Workflows {
		fmt.Fprintf(&b, ";wf=%s:%s:%s", w.TaskID, w.CurrentPhase, w.Status)
	}
	if at := c.LastUser(key); !at.IsZero() {
		fmt.Fprintf(&b, ";user=%d", at.UnixMilli())
	}
	return b.String()
}

// FailText is custody.Config.FailText: the API error the last turn ended
// in, else the process's death detail.
func (c CustodySources) FailText(key string) string {
	s := c.Router.SessionFor(key)
	if s == nil {
		return ""
	}
	snap := s.Snapshot()
	if apierr.IsEnvelope(snap.LastResponse) {
		return snap.LastResponse
	}
	return snap.DeathDetail
}

// Settled is custody.Config.Settled: no turn running, no AskUserQuestion
// unanswered, no workflow or background task running.
func (c CustodySources) Settled(key string) bool {
	s := c.Router.SessionFor(key)
	if s == nil {
		return false
	}
	snap := s.Snapshot()
	if snap.State == "running" || runningWorkflow(snap.Workflows) != "" || askPending(s.EventLastN(askScan)) {
		return false
	}
	if at := s.ProcessAttachedAt(); !at.IsZero() && snap.State != "dead" {
		if t, _ := openTask(s.EventLastN(taskScan), at, true); t != nil {
			return false
		}
	}
	return true
}

// Asking is custody.Config.Asking: an AskUserQuestion of key unanswered.
func (c CustodySources) Asking(key string) bool {
	s := c.Router.SessionFor(key)
	return s != nil && askPending(s.EventLastN(askScan))
}

// Events is custody.Config.Events: the session's latest n events.
func (c CustodySources) Events(key string, n int) []clievent.EventEntry {
	s := c.Router.SessionFor(key)
	if s == nil {
		return nil
	}
	return s.EventLastN(n)
}
