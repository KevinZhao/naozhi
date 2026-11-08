package wireup

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/action"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// digestScan is how many trailing events a digest is built from: the whole
// in-memory ring (ring.EventLog's cap), which bounds what is in view anyway.
const digestScan = 500

// DigestSources gather a session's digest (planner/digest.Facts) from the
// router, the health table, the sidecars, the outputs index and the inbox;
// nil fields leave their part out. Only this node's dispatched sessions and
// the sessions the user opened that the gateway observes get one (brief
// default 2): not cron, daemon, scratch, scout or Planner sessions, nor a
// remote node's.
type DigestSources struct {
	Router     *session.Router
	Health     func() health.Snapshot
	Sidecars   *sidecar.Store
	Outputs    *activity.Recorder
	Inbox      func() []notify.Item
	Workspaces *workspace.Registry
	// Watched reports whether the user left session key observed; nil
	// observes all (the switch is §15.12 ⑥'s). A task: session is always.
	Watched func(key string) bool
}

// Observed reports whether key is one the gateway observes: a session the
// user opened on this node, observing left on. A dispatched session is not
// (it is a task, not observed).
func (d DigestSources) Observed(key string) bool {
	return !sessionkey.IsTaskKey(key) && health.UserSession(key, health.KindSession) &&
		(d.Watched == nil || d.Watched(key))
}

// covers reports whether key gets a digest, as a key.
func (d DigestSources) covers(key string) bool {
	return sessionkey.IsTaskKey(key) || d.Observed(key)
}

// Covered reports whether key gets a digest: covered and in this node's
// router.
func (d DigestSources) Covered(key string) bool {
	return d.Router != nil && d.covers(key) && d.Router.SessionFor(key) != nil
}

// Keys are the sessions the router holds that get a digest.
func (d DigestSources) Keys() []string {
	if d.Router == nil {
		return nil
	}
	var out []string
	d.Router.VisitSessions(func(s session.SessionSnapshot) bool {
		if d.covers(s.Key) {
			out = append(out, s.Key)
		}
		return true
	})
	return out
}

// Ready is digest.Config.Ready: a covered key is ready once the health
// table has judged it and its history from before a restart is loaded.
// Before that its digest would say "health unknown, no replies" of a
// session that had both, and nothing would correct it: a session first
// judged healthy makes no transition, and an idle one ends no turn.
func (d DigestSources) Ready(key string) bool {
	if !d.Covered(key) {
		return true // Facts declines it
	}
	if d.Health != nil {
		snap := d.Health()
		if snap.At.IsZero() || !slices.ContainsFunc(snap.Units, func(u health.Unit) bool { return u.ID == key }) {
			return false
		}
	}
	s := d.Router.SessionFor(key)
	return s == nil || !s.HistoryLoading()
}

// Facts is digest.Config.Facts.
func (d DigestSources) Facts(key string) (digest.Facts, bool) {
	if !d.Covered(key) {
		return digest.Facts{}, false
	}
	s := d.Router.SessionFor(key)
	if s == nil {
		return digest.Facts{}, false
	}
	snap := s.Snapshot()
	f := digest.Facts{
		Key:        key,
		Title:      cmp.Or(snap.UserLabel, snap.Summary),
		Dir:        snap.Workspace,
		LastOutput: unixMilli(snap.LastActive),
		Born:       unixMilli(snap.CreatedAt),
	}
	if d.Observed(key) {
		f.Purpose = sidecar.PurposeChat
	}
	if d.Sidecars != nil {
		if m, ok := d.Sidecars.Get(key); ok {
			f.Purpose = cmp.Or(m.Purpose, f.Purpose)
			f.Title = cmp.Or(m.Title, f.Title)
		}
	}
	if d.Workspaces != nil && f.Dir != "" {
		if w, ok := d.Workspaces.Lookup(f.Dir); ok {
			f.Workspace = w.Name
		}
	}
	if d.Health != nil {
		for _, u := range d.Health().Units {
			if u.ID == key {
				f.Health, f.HealthSince = u.State, u.Since
				break
			}
		}
	}
	entries := s.EventLastN(digestScan)
	f.Replies = turnReplies(entries, digest.MaxReplies)
	f.GoalSeen = firstMessage(entries, sessionkey.IsTaskKey(key))
	if d.Outputs != nil {
		f.Outputs = d.Outputs.Outputs(key, digest.MaxOutputs)
	}
	f.Questions = pendingQuestions(entries)
	if d.Inbox != nil {
		f.Questions = append(f.Questions, inboxQuestions(d.Inbox(), key)...)
	}
	if len(f.Questions) > digest.MaxQuestions {
		f.Questions = f.Questions[:digest.MaxQuestions]
	}
	return f, true
}

// turnReplies is the last assistant text of each of the latest n turns
// that ended (a result frame), oldest first.
func turnReplies(entries []clievent.EventEntry, n int) []digest.Reply {
	var out []digest.Reply
	var last *clievent.EventEntry
	for i := range entries {
		switch e := &entries[i]; e.Type {
		case clievent.KindText:
			last = e
		case clievent.KindResult:
			if last != nil {
				out = append(out, digest.Reply{At: unixMilli(last.Time), Text: cmp.Or(last.Detail, last.Summary)})
			}
			last = nil
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// firstMessage is the first message in entries a goal is taken from: the
// user's own, or for a dispatched session the Planner's dispatch (the
// Planner's text of it, Brief). A digest keeps the first one it sees, so
// this matters only for a session that had none kept: one older than the
// digests, or opened by the user.
func firstMessage(entries []clievent.EventEntry, task bool) string {
	for _, e := range entries {
		if e.Type != clievent.KindUser {
			continue
		}
		text := cmp.Or(e.Detail, e.Summary)
		switch {
		case e.Source == "" && !task:
			return text
		case task && (e.Source == "" || e.Source == clievent.SourcePlanner):
			return action.Brief(text)
		}
	}
	return ""
}

// pendingQuestions are the questions of the latest AskUserQuestion card
// when no user message came after it.
func pendingQuestions(entries []clievent.EventEntry) []string {
	for i := len(entries) - 1; i >= 0; i-- {
		switch e := entries[i]; e.Type {
		case clievent.KindUser:
			return nil
		case clievent.KindAskQuestion:
			var out []string
			if e.AskQuestion != nil {
				for _, it := range e.AskQuestion.Items {
					if it.Question != "" {
						out = append(out, it.Question)
					}
				}
			}
			if len(out) == 0 && e.Summary != "" {
				out = append(out, e.Summary)
			}
			return out
		}
	}
	return nil
}

// inboxQuestions are the open "等你" items about key: its own, and the
// Planner's asks whose answer goes to it (their Unit is the Planner, the
// task in the card's payload). Not the item for a session waiting on its
// own card: pendingQuestions has the card's questions, the item only says
// it waits.
func inboxQuestions(items []notify.Item, key string) []string {
	var out []string
	for _, it := range items {
		if it.State != notify.StateOpen || !it.Kind.WaitsOnUser() || strings.HasPrefix(it.Source, notify.WaitingPrefix) {
			continue
		}
		if it.Unit == key || askTask(it) == key {
			out = append(out, it.Title)
		}
	}
	return out
}

// askTask is the task a Planner ask item's answer goes to, "" for none.
func askTask(it notify.Item) string {
	if it.Decision == nil || it.Decision.Handler != action.HandlerAsk {
		return ""
	}
	var card struct {
		Task string `json:"task"`
	}
	_ = json.Unmarshal(it.Decision.Payload, &card)
	return card.Task
}

// DigestSink is the router's ActivitySink with the digests: every turn's
// end, done, failed or aborted, asks for the session's digest.
type DigestSink struct {
	*activity.Recorder
	Digests *digest.Writer
}

// RecordTurn records the turn's end and asks for the digest.
func (s DigestSink) RecordTurn(key, dir string, outcome activity.TurnOutcome) {
	s.Recorder.RecordTurn(key, dir, outcome)
	s.Digests.Touch(key)
}

// DigestOnHealth is a health subscriber asking for a session's digest when
// its health moves (failed, waiting, stalled) with no turn ending.
func DigestOnHealth(w *digest.Writer) func(health.Event) {
	return func(ev health.Event) {
		if tr := ev.Transition; tr != nil && tr.Unit.Kind == health.KindSession {
			w.Touch(tr.Unit.ID)
		}
	}
}

// DigestRetired is digest.Config.Retired: when key's sidecar saw it gone.
func DigestRetired(sidecars *sidecar.Store) func(key string) time.Time {
	return func(key string) time.Time {
		m, _ := sidecars.Get(key)
		return m.Retired
	}
}
