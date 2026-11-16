package agent

import (
	"log/slog"
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
)

// heldReceipt is a receipt waiting for the next one (agent.json). Along is
// a line that never starts a turn of its own (TellAlong); the rest wait for
// the user to go quiet (TellLater).
type heldReceipt struct {
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
	Along bool      `json:"along,omitempty"`
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

// withHeld is text after the receipts waiting, and how many it took; one
// receipt carries them all, oldest first, each with when it came, since it
// may go minutes after.
func (a *Agent) withHeld(text string) (string, int) {
	a.mu.Lock()
	held := a.state.Held
	a.mu.Unlock()
	if len(held) == 0 {
		return text, 0
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
		return "", 0 // lines along start no turn of their own
	}
	for _, h := range held {
		parts = append(parts, "（"+h.At.In(a.cfg.Now().Location()).Format("15:04")+"）"+h.Text)
	}
	if text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n"), len(held)
}

// dropHeld forgets the first n receipts waiting, sent.
func (a *Agent) dropHeld(n int) {
	if n == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Held = append([]heldReceipt(nil), a.state.Held[min(n, len(a.state.Held)):]...)
	a.saveLocked()
}

// flushHeld sends the receipts held for the user going quiet once they
// have been and the Planner is idle in its current conversation. Lines
// riding along only wait: they start no turn.
func (a *Agent) flushHeld(now time.Time) {
	a.mu.Lock()
	quiet := !now.Before(a.userAt.Add(chatQuiet))
	due, shifting := false, a.shifting
	for _, h := range a.state.Held {
		due = due || !h.Along
	}
	a.mu.Unlock()
	if !due || !quiet || shifting {
		return
	}
	if st := a.cfg.Sessions.State(Key); !st.Alive || st.Running {
		return
	}
	if err := a.Tell(""); err != nil {
		slog.Warn("planner: held receipts not sent", "err", err)
	}
}
