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
// notify and front blocks (handle). How often a session's lines go, and
// what waits meanwhile, is the Planner's side (agent.Agent.TellWatch): it
// alone knows when a line went. It lives here, not in custody's Ended:
// the reply reaches the gateway only through the result sink, and the
// receipt of a session in custody, which this must not double, is sent
// from here as well.

const (
	// watchQueue bounds the watched sessions' turn ends waiting for a
	// check. A channel of their own: a burst of sessions ending together
	// must not push a dispatched session's check out of endCh.
	watchQueue = 64
	// watchLastRunes caps the session's last reply's first sentence in the
	// line.
	watchLastRunes = 200
	// watchDigestWait bounds how long a line waits for its session's digest
	// to show the turn, checked every watchDigestPoll: a session the health
	// table has not judged yet has none written, and the line goes without.
	watchDigestWait = time.Minute
	watchDigestPoll = time.Second
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
// (checkWatch) once reportGrace passes. One in custody is left to custody's
// own receipt (custodyEnded), and a reply that is an API error's envelope to
// custody's retry: the result sink takes only the turns that ended without
// an error, but the CLI reports such an error as a reply. It runs on the
// read loop and never blocks.
func (r *Runner) watchEnded(key, text string) {
	if !r.watching(key) || apierr.IsEnvelope(text) {
		return
	}
	last := textutil.TruncateRunes(digest.FirstSentence(textutil.TruncateRunes(text, endTextRunes)), watchLastRunes)
	e := ended{key: key, text: last, at: r.cfg.Now()}
	r.after(r.reportGrace, func() {
		// The Planner is told to read the digest, whose writes are MinGap
		// apart: one written while the turn ran would show the turn before.
		// Asked for here, not at the turn's end: the sink runs before the
		// result frame is in the event log the digest's replies are read
		// from.
		e.asked = r.cfg.Now()
		r.cfg.Digests.TouchNow(key)
		r.queueWatch(e)
	})
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

// checkWatch hands the Planner watched session e.key's turn end once its
// digest shows the turn (up to watchDigestWait). It keeps nothing and does
// not wait for an idle Planner: TellWatch holds the line while a turn runs,
// the user is chatting or the session was told of within its window, a later
// turn end replacing it. A Planner that takes none (stopped, off) is told
// nothing later either.
func (r *Runner) checkWatch(e ended) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("planner actions: watched turn-end check panicked", "panic", p)
		}
	}()
	// The switch may have been turned off, or custody on, meanwhile.
	if !r.watching(e.key) {
		return
	}
	now := r.cfg.Now()
	if d := r.cfg.Digests; d != nil && d.Settled(e.key).Before(e.asked) && now.Before(e.asked.Add(watchDigestWait)) {
		r.after(watchDigestPoll, func() { r.queueWatch(e) })
		return
	}
	l := watchLine{Key: e.key, Ended: e.at.In(now.Location()).Format("15:04"), Last: e.text}
	r.watchState(&l)
	if err := r.cfg.TellWatch(e.key, l.encode(), l.Outside); err != nil {
		slog.Debug("planner actions: a watched session's turn end not told the Planner", "key", e.key, "err", err)
	}
}

// WatchNow is agent.Agent.SetWatchNow's check of a line the Planner held:
// line as key's session stands now, whether it has read outside content,
// and false once its turn ends are no longer told (观察 turned off, in
// custody, the session gone). Safe from any goroutine: it reads the
// gateway's state and none of the runner's own.
func (r *Runner) WatchNow(key, line string) (string, bool, bool) {
	var l watchLine
	if !r.watching(key) || json.Unmarshal([]byte(line), &l) != nil || l.Key != key {
		return "", false, false
	}
	r.watchState(&l)
	return l.encode(), l.Outside, true
}

// watchState fills in what l says of its session as it stands now: all but
// the turn's own (Key, Ended, Last). Outside is never taken back.
func (r *Runner) watchState(l *watchLine) {
	l.Digest, l.Nearby, l.Background = r.cfg.Digests.Path(l.Key), r.cfg.Custody.Nearby(l.Key), r.background(l.Key).Count()
	l.Name = ""
	if name := r.cfg.Custody.Name(l.Key); name != l.Key {
		l.Name = name
	}
	if r.cfg.SessionDir != nil {
		l.Workspace = r.cfg.SessionDir(l.Key)
		if r.cfg.Workspaces != nil && l.Workspace != "" {
			if w, ok := r.cfg.Workspaces.Lookup(l.Workspace); ok {
				l.Workspace = w.Name
			}
		}
	}
	if r.cfg.Sessions != nil {
		m, _ := r.cfg.Sessions.Get(l.Key)
		l.Outside = l.Outside || m.Taint != nil
	}
	l.Waiting = r.cfg.Asking != nil && r.cfg.Asking(l.Key)
}

// encode is l as the line the Planner reads.
func (l watchLine) encode() string {
	// Strings, bools and an int: Marshal does not fail on them.
	line, _ := json.Marshal(l)
	return string(line)
}
