package action

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/textutil"
)

// A session the user opened (观察中, custody's watch switch) ending a turn
// normally wakes the Planner too (docs/rfc/planner.md §15.22 ①, D138): the
// gateway tells it one JSON line of the session in a <naozhi-watch> turn,
// whether or not the user is at the session, and that turn runs only note,
// notify and front blocks (handle). It lives here, not in custody's Ended:
// the reply reaches the gateway only through the result sink, and the
// receipt of a session in custody, which this must not double, is sent
// from here as well.

const (
	// watchEvery is how often one session's turn ends wake the Planner: the
	// turns ending inside the window after one told are told as one, the
	// latest, at its end.
	watchEvery = 10 * time.Minute
	// watchQueue bounds the watched sessions' turn ends waiting for a
	// check. A channel of their own: a burst of sessions ending together
	// must not push a dispatched session's check out of endCh.
	watchQueue = 64
	// watchLastRunes caps the session's last reply's first sentence in the
	// line.
	watchLastRunes = 200
)

// watchLine is one session's line in a <naozhi-watch> turn. encoding/json
// escapes angle brackets, so no field can close the tag it rides in.
type watchLine struct {
	Key       string `json:"key"`
	Name      string `json:"name,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// Digest is the session's digest file, left out until one is written.
	Digest string `json:"digest,omitempty"`
	Ended  string `json:"ended"`
	// Last is the first sentence of the session's last reply: its output.
	Last string `json:"last,omitempty"`
	// Outside is a session that read outside content (its digest headed
	// 含外部内容).
	Outside bool `json:"outside,omitempty"`
	// Nearby is the user at the session as the line goes out.
	Nearby bool `json:"nearby,omitempty"`
	// Waiting is an AskUserQuestion of the session's unanswered.
	Waiting    bool `json:"waiting,omitempty"`
	Background int  `json:"background,omitempty"`
}

// watchEnded has the turn end of a watched session told the Planner
// (checkWatch) once reportGrace passes, by when its digest is written. One
// in custody is left to custody's own receipt (custodyEnded), and a reply
// that is an API error's envelope to custody's retry: the result sink takes
// only the turns that ended without an error, but the CLI reports such an
// error as a reply. It runs on the read loop and never blocks.
func (r *Runner) watchEnded(key, text string) {
	if !r.watching(key) || apierr.IsEnvelope(text) {
		return
	}
	last := textutil.TruncateRunes(digest.FirstSentence(textutil.TruncateRunes(text, endTextRunes)), watchLastRunes)
	e := ended{key: key, text: last, at: r.cfg.Now()}
	r.after(r.reportGrace, func() { r.queueWatch(e) })
}

// watching reports whether key's turn ends are told the Planner: a session
// the gateway observes, not one in custody.
func (r *Runner) watching(key string) bool {
	c := r.cfg.Custody
	return c != nil && r.cfg.TellWatch != nil && c.Watched(key) && !c.On(key)
}

// queueWatch queues e for checkWatch, dropping it when the queue is full.
func (r *Runner) queueWatch(e ended) {
	select {
	case r.watchCh <- e:
	default:
		slog.Warn("planner actions: queue full; a watched session's turn end not told", "key", e.key)
	}
}

// checkWatch tells the Planner of watched session e.key's turn end, at most
// once in watchEvery: one ending inside the window of the last told is held,
// a later one replacing it, and told when the window ends (one timer per
// session). It does not wait for an idle Planner: TellWatch holds the line
// while a turn runs or the user is chatting. A Planner that takes none
// (stopped, off) is told nothing later either: the turn end is dropped and
// no window starts. The windows are in memory: a restart tells the next
// turn end afresh.
func (r *Runner) checkWatch(e ended) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("planner actions: watched turn-end check panicked", "panic", p)
		}
	}()
	if e.wake {
		held, ok := r.watchHeld[e.key]
		if !ok {
			return
		}
		delete(r.watchHeld, e.key)
		e = held
	}
	// The switch may have been turned off, or custody on, meanwhile.
	if !r.watching(e.key) {
		delete(r.watchHeld, e.key)
		return
	}
	now := r.cfg.Now()
	if end := r.watchTold[e.key].Add(watchEvery); now.Before(end) {
		if _, armed := r.watchHeld[e.key]; !armed {
			key := e.key
			r.after(end.Sub(now), func() { r.queueWatch(ended{key: key, wake: true}) })
		}
		r.watchHeld[e.key] = e
		return
	}
	delete(r.watchHeld, e.key)
	line, outside := r.watchLine(e, now)
	if err := r.cfg.TellWatch(e.key, line, outside); err != nil {
		slog.Debug("planner actions: a watched session's turn end not told the Planner", "key", e.key, "err", err)
		return
	}
	for k, at := range r.watchTold {
		if !now.Before(at.Add(watchEvery)) {
			delete(r.watchTold, k)
		}
	}
	r.watchTold[e.key] = now
}

// watchLine is e's session's line as it stands now, and whether the session
// read outside content.
func (r *Runner) watchLine(e ended, now time.Time) (string, bool) {
	l := watchLine{Key: e.key, Ended: e.at.In(now.Location()).Format("15:04"), Last: e.text, Digest: r.cfg.Digests.Path(e.key),
		Nearby: r.cfg.Custody.Nearby(e.key), Background: r.background(e.key).Count()}
	if name := r.cfg.Custody.Name(e.key); name != e.key {
		l.Name = name
	}
	if r.cfg.SessionDir != nil {
		l.Workspace = r.cfg.SessionDir(e.key)
		if r.cfg.Workspaces != nil && l.Workspace != "" {
			if w, ok := r.cfg.Workspaces.Lookup(l.Workspace); ok {
				l.Workspace = w.Name
			}
		}
	}
	if r.cfg.Sessions != nil {
		m, _ := r.cfg.Sessions.Get(e.key)
		l.Outside = m.Taint != nil
	}
	if r.cfg.Asking != nil {
		l.Waiting = r.cfg.Asking(e.key)
	}
	// Strings, bools and an int: Marshal does not fail on them.
	line, _ := json.Marshal(l)
	return string(line), l.Outside
}
