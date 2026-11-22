package agent

import (
	"log/slog"
	"slices"
	"strings"
	"time"
)

const (
	// chatQuiet is how long after the user's last message on the Planner
	// page a progress-class receipt waits (§15.14 ③, D114): one sent while
	// they are chatting would wake a turn between two of theirs.
	chatQuiet = 5 * time.Minute
	// heldMax bounds the receipts waiting, oldest dropped past it: a
	// Planner that never goes idle must not grow agent.json, nor one turn's
	// message, without end.
	heldMax = 40
	// watchEvery is how often one watched session's turn ends wake the
	// Planner (§15.22 ①, D138), counted from the turn the last went in.
	watchEvery = 10 * time.Minute
)

// heldReceipt is a receipt waiting for the next one (agent.json). Along is
// a line that never starts a turn of its own (TellAlong); the rest wait for
// the user to go quiet (TellLater). Watch is a watched session's turn end
// (TellWatch), Key its session and Outside whether that session read
// outside content: it waits as the rest do, for an idle Planner and for its
// session's watchEvery, but goes in a watch turn of its own, never in a
// receipt's.
type heldReceipt struct {
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Along   bool      `json:"along,omitempty"`
	Watch   bool      `json:"watch,omitempty"`
	Outside bool      `json:"outside,omitempty"`
	Key     string    `json:"key,omitempty"`
}

// TellLater is Tell for a progress-class receipt (a progress report, a
// dispatch's): while the user sent a message on the Planner page within
// chatQuiet it is held, kept in agent.json across a restart, and goes merged
// into one turn once they have been quiet chatQuiet and the Planner is idle,
// or rides on the next receipt Tell sends before then. Otherwise it is
// Tell.
func (a *Agent) TellLater(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if err := a.tellable(); err != nil {
		return err
	}
	now := a.cfg.Now()
	a.mu.Lock()
	quietAt := a.userAt.Add(chatQuiet)
	if !now.Before(quietAt) {
		a.mu.Unlock()
		return a.Tell(text)
	}
	a.holdLocked(heldReceipt{Text: text, At: now})
	a.mu.Unlock()
	a.pokeAt(quietAt.Sub(now))
	return nil
}

// TellWatch tells the Planner that session key, one the user opened (观察中),
// ended a turn normally (§15.22 ①, D138): line is the gateway's JSON line of
// it, outside whether the session read outside content. It goes as a turn
// from SourceWatch (SourceWatchOutside for outside), which runs only note,
// notify and front blocks; the lines of several sessions go in one. One
// session's lines go at most once in watchEvery, counted from the turn the
// last one went in: until then, and while the user is chatting on the
// Planner page (chatQuiet, D114), a Planner turn runs or the shift changes,
// the line waits in agent.json, a later line of the same session replacing
// it, so the turns that ended meanwhile are told as one, the latest. The
// error is why the Planner takes none (off, stopped, not ready): nothing is
// kept then.
func (a *Agent) TellWatch(key, line string, outside bool) error {
	line = strings.TrimSpace(line)
	if key == "" || line == "" {
		return nil
	}
	if err := a.tellable(); err != nil {
		return err
	}
	now := a.cfg.Now()
	a.mu.Lock()
	a.state.Held = slices.DeleteFunc(a.state.Held, func(h heldReceipt) bool { return h.Watch && h.Key == key })
	a.holdLocked(heldReceipt{Text: line, At: now, Watch: true, Outside: outside, Key: key})
	due := a.userAt.Add(chatQuiet)
	if at, ok := a.watchSent[key]; ok && at.Add(watchEvery).After(due) {
		due = at.Add(watchEvery)
	}
	a.mu.Unlock()
	if now.Before(due) {
		a.pokeAt(due.Sub(now))
		return nil
	}
	// A busy Planner's are sent by the loop's next reconcile.
	a.flushWatch(now)
	return nil
}

// SetWatchNow wires what a waiting watch line is checked against as it goes
// out (action.Runner.WatchNow); the runner is built after the Planner, so it
// comes in later. now returns key's line as its session stands then, and
// whether the session has read outside content; ok false is a session no
// longer told of (观察 turned off, in custody, gone), whose line is dropped.
// A line may wait minutes, and what it goes as decides what its turn may
// run, so the one judged as it was handed in is not sent unchecked.
func (a *Agent) SetWatchNow(now func(key, line string) (fresh string, outside, ok bool)) {
	a.mu.Lock()
	a.watchNow = now
	a.mu.Unlock()
}

// flushWatch sends the watch lines waiting once the user has been quiet
// chatQuiet and the Planner is idle in its current conversation, each as
// its session stands now (SetWatchNow): those of the sessions that read
// outside content in one turn, the rest in another, each dropped once its
// turn is submitted. A session told of within watchEvery keeps waiting.
func (a *Agent) flushWatch(now time.Time) {
	a.tellMu.Lock()
	defer a.tellMu.Unlock()
	a.mu.Lock()
	quiet, shifting, fresh := !now.Before(a.userAt.Add(chatQuiet)), a.shifting, a.watchNow
	var due []heldReceipt
	for _, h := range a.state.Held {
		if at, ok := a.watchSent[h.Key]; h.Watch && !(ok && now.Before(at.Add(watchEvery))) {
			due = append(due, h)
		}
	}
	a.mu.Unlock()
	if len(due) == 0 || !quiet || shifting {
		return
	}
	if st := a.cfg.Sessions.State(Key); !st.Alive || st.Running {
		return
	}
	type batch struct {
		src   Source
		held  []heldReceipt
		lines []string
	}
	batches := []*batch{{src: SourceWatch}, {src: SourceWatchOutside}}
	var gone []heldReceipt
	for _, h := range due {
		line, outside := h.Text, h.Outside
		if fresh != nil {
			l, out, ok := fresh(h.Key, h.Text)
			if !ok {
				gone = append(gone, h)
				continue
			}
			// The mark is never taken back: a line handed in as outside
			// stays so.
			line, outside = l, outside || out
		}
		b := batches[0]
		if outside {
			b = batches[1]
		}
		b.held, b.lines = append(b.held, h), append(b.lines, line)
	}
	a.dropHeld(gone)
	for _, b := range batches {
		if len(b.held) == 0 {
			continue
		}
		// No snapshot, as a receipt: the lines name what the turn is about.
		if err := a.submit(b.src, nil, watchBlock(b.lines)); err != nil {
			slog.Warn("planner: watched sessions' turn ends not sent", "source", b.src, "err", err)
			continue
		}
		a.mu.Lock()
		if a.watchSent == nil {
			a.watchSent = map[string]time.Time{}
		}
		for k, at := range a.watchSent {
			if !now.Before(at.Add(watchEvery)) {
				delete(a.watchSent, k)
			}
		}
		for _, h := range b.held {
			a.watchSent[h.Key] = now
		}
		a.mu.Unlock()
		a.dropHeld(b.held)
	}
}

// watchBlock is the watch turn's message: one JSON line per session.
func watchBlock(lines []string) string {
	return "<naozhi-watch>\n" + strings.Join(lines, "\n") + "\n</naozhi-watch>"
}

// dropWatchLocked forgets the watch lines waiting: a stopped Planner keeps
// none, and a turn end told once it is back on would be stale. a.mu held.
func (a *Agent) dropWatchLocked() {
	a.state.Held = slices.DeleteFunc(a.state.Held, func(h heldReceipt) bool { return h.Watch })
}

// TellAlong keeps text for the next receipt, never starting a turn for it:
// what the Planner already has another way (a report its session also sent
// by SendMessage, D114), so a turn of its own would only repeat it. It is
// dropped while the Planner cannot take receipts, as a Tell then is.
func (a *Agent) TellAlong(text string) {
	text = strings.TrimSpace(text)
	if text == "" || a.tellable() != nil {
		return
	}
	now := a.cfg.Now()
	a.mu.Lock()
	a.holdLocked(heldReceipt{Text: text, At: now, Along: true})
	a.mu.Unlock()
}

// ChatQuietAt is when the user's chat on the Planner page goes quiet,
// chatQuiet after their last message; zero before any since the start.
func (a *Agent) ChatQuietAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.userAt.IsZero() {
		return time.Time{}
	}
	return a.userAt.Add(chatQuiet)
}

// holdLocked appends h to the receipts waiting and saves them. a.mu held.
func (a *Agent) holdLocked(h heldReceipt) {
	a.state.Held = append(a.state.Held, h)
	if n := len(a.state.Held) - heldMax; n > 0 {
		a.state.Held = a.state.Held[n:]
	}
	a.saveLocked()
}

// pokeAt wakes the loop once d passes, for the held receipts to go.
func (a *Agent) pokeAt(d time.Duration) {
	if a.cfg.Enabled {
		time.AfterFunc(d, a.poke)
	}
}

// tellable is why a receipt cannot be submitted now, nil when it can.
func (a *Agent) tellable() error {
	if !a.cfg.Enabled {
		return ErrOff
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.state.Stopped:
		return ErrStopped
	case a.turns == nil:
		return ErrNotReady
	}
	return nil
}

// withHeld is text after the receipts waiting, and the ones it took; one
// receipt carries them all, oldest first, each with when it came, since it
// may go minutes after. The watch lines stay: in a receipt's turn they
// would be answered with blocks a watch turn may not run.
func (a *Agent) withHeld(text string) (string, []heldReceipt) {
	a.mu.Lock()
	held := slices.DeleteFunc(slices.Clone(a.state.Held), func(h heldReceipt) bool { return h.Watch })
	a.mu.Unlock()
	if len(held) == 0 {
		return text, nil
	}
	text = strings.TrimSpace(text)
	parts := make([]string, 0, len(held)+2)
	for _, h := range held {
		if !h.Along {
			parts = append(parts, "（以下几条是用户和你对话时网关攒着的回执，现在一起送）")
			break
		}
	}
	if text == "" && len(parts) == 0 {
		return "", nil // lines along start no turn of their own
	}
	for _, h := range held {
		parts = append(parts, "（"+h.At.In(a.cfg.Now().Location()).Format("15:04")+"）"+h.Text)
	}
	if text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n"), held
}

// dropHeld forgets the receipts waiting that were sent, each once: the
// list may have taken more, or lost its oldest past heldMax, since they
// were read.
func (a *Agent) dropHeld(sent []heldReceipt) {
	if len(sent) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := make([]heldReceipt, 0, len(a.state.Held))
	for _, h := range a.state.Held {
		if i := slices.IndexFunc(sent, h.same); i >= 0 {
			sent = slices.Delete(sent, i, i+1)
			continue
		}
		kept = append(kept, h)
	}
	a.state.Held = kept
	a.saveLocked()
}

// same reports whether h and o are the one receipt.
func (h heldReceipt) same(o heldReceipt) bool {
	return h.Text == o.Text && h.At.Equal(o.At) && h.Along == o.Along && h.Watch == o.Watch && h.Outside == o.Outside && h.Key == o.Key
}

// flushHeld sends the receipts held for the user going quiet once they
// have been and the Planner is idle in its current conversation. Lines
// riding along only wait: they start no turn. The watch lines go after, in
// turns of their own (flushWatch).
func (a *Agent) flushHeld(now time.Time) {
	a.mu.Lock()
	quiet := !now.Before(a.userAt.Add(chatQuiet))
	due, shifting := false, a.shifting
	for _, h := range a.state.Held {
		due = due || !h.Along && !h.Watch
	}
	a.mu.Unlock()
	if due && quiet && !shifting {
		if st := a.cfg.Sessions.State(Key); st.Alive && !st.Running {
			if err := a.Tell(""); err != nil {
				slog.Warn("planner: held receipts not sent", "err", err)
			}
		}
	}
	a.flushWatch(now)
}
