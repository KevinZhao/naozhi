package wireup

import (
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// SessionNotes builds the session side notes (docs/rfc/planner.md §15.3 ③)
// over the journal, the inbox for item titles, the sidecars for dispatches
// and planner:main's history for reports.
func SessionNotes(j *journal.Journal, inbox *notify.Notifier, sidecars *sidecar.Store, router *session.Router) *note.Notes {
	return note.New(note.Config{
		Journal: j,
		Item: func(id string) (string, bool, bool) {
			it, ok := inbox.Item(id)
			return it.Title, it.State == notify.StateOpen, ok
		},
		Dispatched: func(key string) time.Time {
			m, ok := sidecars.Get(key)
			if !ok || !m.DispatchedByPlanner() {
				return time.Time{}
			}
			return m.Created
		},
		Reports: func(key string) []time.Time { return plannerReports(router, key) },
	})
}

// PlannerReports is action.Config.Reports over the router: the senders of
// the peer_message entries planner:main's history took in (from, to].
func PlannerReports(router *session.Router) func(from, to time.Time) []string {
	return func(from, to time.Time) []string {
		s := router.SessionFor(sessionkey.PlannerMainKey)
		if s == nil {
			return nil
		}
		lo, hi := from.UnixMilli(), to.UnixMilli()
		var out []string
		for _, e := range s.EventEntriesSince(lo) {
			if e.Type == clievent.KindPeerMessage && e.PeerKey != "" && e.Time <= hi {
				out = append(out, e.PeerKey)
			}
		}
		return out
	}
}

// plannerReports are the times key's cross-session messages reached
// planner:main: the peer_message entries of its history whose sender
// resolved to key on arrival.
func plannerReports(router *session.Router, key string) []time.Time {
	s := router.SessionFor(sessionkey.PlannerMainKey)
	if s == nil {
		return nil
	}
	var out []time.Time
	for _, e := range s.EventEntries() {
		if e.Type == clievent.KindPeerMessage && e.PeerKey == key {
			out = append(out, time.UnixMilli(e.Time))
		}
	}
	return out
}
