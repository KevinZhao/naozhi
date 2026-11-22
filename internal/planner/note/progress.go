package note

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
)

// The gateway's progress notes (docs/rfc/planner.md §15.22 ④, D141): a
// session whose turn is over while its background tasks or workflows go on
// gets a note every ProgressEvery, and one when a workflow moves to another
// phase, so the user need not ask it how far it got. No model writes them and
// nothing is sent to the session or to the Planner. They are kind=note
// journal entries of their own event, so a restart keeps them and neither
// the Planner's limits nor "没用" count them.
const (
	// ProgressEvery is how long after the turn's end, and after each note,
	// the next one is written.
	ProgressEvery = 30 * time.Minute
	// progressGap is the least time between two notes of a session: phases
	// that flip back and forth are one note, not one a minute.
	progressGap = 5 * time.Minute
)

// EventProgress is the kind=note journal event of a progress note.
const EventProgress = "progress"

// Background is a session with its turn over and background work open, as
// Progress reads it. Turn is when its last turn ended.
type Background struct {
	Key   string
	Turn  time.Time
	State bgtask.State
}

// stretch is a session's background work since one turn's end. id names
// its notes (the view shows the latest of a stretch only), seen is when
// this process first saw it, at is its latest note, zero before the first,
// and phases are its workflows' as that note had them, one not in it as
// first seen since.
type stretch struct {
	id     string
	seen   time.Time
	at     time.Time
	phases map[string]string
}

// stretchID names the stretch after the turn that ended at turn: a turn's
// end is the same after a restart, so its notes stay one stretch's. A turn
// whose end is not known (zero) names one stretch too.
func stretchID(turn time.Time) string { return strconv.FormatInt(turn.UnixMilli(), 10) }

// Progress writes the progress notes due among list, the sessions whose
// turn is over with background work open now; a session no longer listed
// gets none (its work ended, or a turn runs and speaks for itself). A tick
// loop calls it. Nothing is written while pushes and custody are paused.
func (n *Notes) Progress(list []Background) {
	if n.cfg.Journal == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.cfg.Now()
	if n.stretches == nil {
		n.stretches = n.lastProgress(now)
	}
	paused := n.paused(now)
	listed := make(map[string]bool, len(list))
	for _, b := range list {
		listed[b.Key] = true
		id := stretchID(b.Turn)
		st := n.stretches[b.Key]
		if st == nil || st.id != id {
			// A turn ran since: what the session said in it is newer than
			// any note, and the wait starts over from its end.
			st = &stretch{id: id, seen: now}
			n.stretches[b.Key] = st
		}
		phases := b.State.Phases()
		// A workflow is first seen in a phase whenever its board first has
		// one, which may be ticks after the session is: it moves from there.
		if st.phases == nil {
			st.phases = map[string]string{}
		}
		for wf, p := range phases {
			if _, ok := st.phases[wf]; !ok {
				st.phases[wf] = p
			}
		}
		// A session adopted from a live shim has no turn's end on record:
		// its wait runs from the first sight of it, not from the year 1.
		from := b.Turn
		if from.IsZero() {
			from = st.seen
		}
		since := now.Sub(later(st.at, from))
		if since < progressGap || since < ProgressEvery && !moved(st.phases, phases) || paused {
			continue
		}
		text := b.State.Progress(now)
		if out := b.State.Output(); !out.IsZero() && !out.After(now) {
			text += " · 最近输出 " + bgtask.Span(now.Sub(out)) + "前"
		}
		e, err := noteEntry(now, b.Key, PlannerData{Event: EventProgress, ID: newID("g"), Text: text, Run: st.id})
		if err == nil {
			err = n.cfg.Journal.Append(e)
		}
		if err != nil {
			slog.Warn("session notes: progress note not written", "key", b.Key, "err", err)
			continue
		}
		st.at, st.phases = now, phases
	}
	// A stretch not listed is kept while its last note could still hold the
	// next one back: a session that drops out for a tick is not noted twice.
	for key, st := range n.stretches {
		if !listed[key] && now.Sub(st.at) >= ProgressEvery {
			delete(n.stretches, key)
		}
	}
}

// moved reports whether a workflow in both was and now is in another phase.
func moved(was, now map[string]string) bool {
	for id, p := range now {
		if old, ok := was[id]; ok && old != p {
			return true
		}
	}
	return false
}

// lastProgress is each session's latest progress note of the ProgressEvery
// before now, read from the journal: after a restart the next note waits
// out what is left of the interval instead of every session getting one at
// once.
func (n *Notes) lastProgress(now time.Time) map[string]*stretch {
	out := map[string]*stretch{}
	entries, err := n.cfg.Journal.Read(now.Add(-ProgressEvery), now.Add(ahead))
	if err != nil {
		slog.Warn("session notes: journal read", "err", err)
	}
	for _, e := range entries {
		if d, ok := plannerData(e); ok && d.Event == EventProgress && e.Unit != "" {
			out[e.Unit] = &stretch{id: d.Run, at: e.TS}
		}
	}
	return out
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
