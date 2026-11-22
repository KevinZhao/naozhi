package note

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
)

// Limits on the Planner's notes (§15.22 ②): PerHour on one session within
// any 60 minutes, PerDay over all sessions in a local day, and UselessMute
// notes of one session marked useless in a day end that session's notes for
// the day.
const (
	PerHour     = 3
	PerDay      = 30
	UselessMute = 3
)

// ahead is how far past now the counts and "没用" read the journal: after
// the clock stepped back, the notes and marks written before it are stamped
// later than now, and must still hold a limit and be found.
const ahead = 24 * time.Hour

// Events of a kind=note journal entry.
const (
	EventWrite   = "write"
	EventUseless = "useless"
)

// PlannerData is a kind=note journal entry's data: the Planner writing note
// ID on the entry's unit, or the user marking it useless.
type PlannerData struct {
	Event string `json:"event"`
	ID    string `json:"id"`
	Text  string `json:"text,omitempty"`
	Why   string `json:"why,omitempty"`
}

// Why a note was not written. The texts are what the Planner reads in the
// receipt of the blocks not run.
var (
	ErrPaused   = errors.New("用户暂停了推送与托管，旁注没有写，不用重发")
	ErrMuted    = fmt.Errorf("用户今天在这个会话的旁注上点了 %d 次“没用”，今天不再给它写旁注", UselessMute)
	ErrHourFull = fmt.Errorf("这个会话一小时内已经写了 %d 条旁注，没有再写", PerHour)
	ErrDayFull  = fmt.Errorf("今天的旁注已到 %d 条上限，没有再写", PerDay)
	// ErrNoNote is a "没用" on a note this session does not have.
	ErrNoNote = errors.New("没有这条旁注")
)

var errNoJournal = errors.New("旁注没有接通")

// plannerData is a kind=note entry's data.
func plannerData(e journal.Entry) (PlannerData, bool) {
	var d PlannerData
	if e.Kind != journal.KindNote || json.Unmarshal(e.Data, &d) != nil || d.ID == "" {
		return PlannerData{}, false
	}
	return d, true
}

// tally is what the limits count, read from the journal so a restart keeps
// it: the notes written today, those of each session in the last hour, and
// each session's notes marked useless today.
type tally struct {
	day     int
	hour    map[string]int
	useless map[string]int
}

func (n *Notes) tally(now time.Time) (tally, error) {
	t := tally{hour: map[string]int{}, useless: map[string]int{}}
	y, m, d := now.Date()
	today, hourAgo := time.Date(y, m, d, 0, 0, 0, 0, now.Location()), now.Add(-time.Hour)
	since := today
	if hourAgo.Before(since) {
		since = hourAgo
	}
	entries, err := n.cfg.Journal.Read(since, now.Add(ahead))
	if err != nil {
		return t, err
	}
	for _, e := range entries {
		d, ok := plannerData(e)
		if !ok {
			continue
		}
		switch d.Event {
		case EventWrite:
			if !e.TS.Before(today) {
				t.day++
			}
			if e.TS.After(hourAgo) {
				t.hour[e.Unit]++
			}
		case EventUseless:
			// Useless writes one mark a note, so each is one click.
			if !e.TS.Before(today) {
				t.useless[e.Unit]++
			}
		}
	}
	return t, nil
}

// paused reports whether the pause of pushes and custody holds at now.
func (n *Notes) paused(now time.Time) bool {
	return n.cfg.PausedUntil != nil && n.cfg.PausedUntil().After(now)
}

// Write keeps text as the Planner's note on session key, why being what it
// goes by, and pushes it to the session's view (the journal's observer). It
// reaches the user only: nothing here sends to the session. A note past a
// limit is not written and not counted; the error says which, as the Planner
// is told.
func (n *Notes) Write(key, text, why string) (Note, error) {
	if n.cfg.Journal == nil {
		return Note{}, errNoJournal
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.cfg.Now()
	if n.paused(now) {
		return Note{}, ErrPaused
	}
	t, err := n.tally(now)
	switch {
	case err != nil:
		return Note{}, fmt.Errorf("旁注的记录读不出来，没有写：%w", err)
	case t.useless[key] >= UselessMute:
		return Note{}, ErrMuted
	case t.hour[key] >= PerHour:
		return Note{}, ErrHourFull
	case t.day >= PerDay:
		return Note{}, ErrDayFull
	}
	e, err := noteEntry(now, key, PlannerData{Event: EventWrite, ID: newID(), Text: text, Why: why})
	if err != nil {
		return Note{}, err
	}
	if err := n.cfg.Journal.Append(e); err != nil {
		return Note{}, err
	}
	nt, _ := FromEntry(e)
	return nt, nil
}

// Useless marks note id of session key useless for the user and tells the
// Planner which; the third of a session in a day ends its notes for the day
// (Write). Marking one again changes nothing and tells no one. The mark is
// kept even when the Planner cannot be told.
func (n *Notes) Useless(key, id string) (Note, error) {
	if n.cfg.Journal == nil {
		return Note{}, errNoJournal
	}
	n.mu.Lock()
	now := n.cfg.Now()
	nt, marked, err := n.find(key, id, now)
	if err != nil || marked {
		n.mu.Unlock()
		return nt, err
	}
	e, err := noteEntry(now, key, PlannerData{Event: EventUseless, ID: id})
	if err == nil {
		err = n.cfg.Journal.Append(e)
	}
	if err != nil {
		n.mu.Unlock()
		return Note{}, err
	}
	nt.Useless = true
	t, err := n.tally(now)
	n.mu.Unlock()
	if err != nil {
		slog.Warn("session notes: journal read", "key", key, "err", err)
	}
	if n.cfg.Tell == nil {
		return nt, nil
	}
	where := key
	if n.cfg.Name != nil {
		if name := n.cfg.Name(key); name != "" && name != key {
			where += "（" + name + "）"
		}
	}
	text := "用户在 " + where + " 里对你写的旁注点了“没用”：「" + nt.Text + "」。这类旁注对用户没有帮助，同类情形不要再写。"
	if c := t.useless[key]; c >= UselessMute {
		text += fmt.Sprintf("这是今天这个会话的第 %d 次，今天不再给它写旁注，再写网关不执行。", c)
	}
	// Outside mu: the Planner's send path is not this package's to hold a
	// lock across.
	if err := n.cfg.Tell(text); err != nil {
		slog.Warn("session notes: useless mark not told to the Planner", "key", key, "err", err)
	}
	return nt, nil
}

// find returns key's Planner note id of the Window before now and whether
// it is already marked useless; ErrNoNote when the session has none such.
func (n *Notes) find(key, id string, now time.Time) (nt Note, marked bool, err error) {
	entries, err := n.cfg.Journal.Read(now.Add(-Window), now.Add(ahead))
	if err != nil {
		return Note{}, false, err
	}
	found := false
	for _, e := range entries {
		d, ok := plannerData(e)
		if !ok || e.Unit != key || d.ID != id {
			continue
		}
		switch d.Event {
		case EventWrite:
			nt, found = FromEntry(e)
		case EventUseless:
			marked = true
		}
	}
	if !found {
		return Note{}, false, ErrNoNote
	}
	nt.Useless = marked
	return nt, marked, nil
}

func noteEntry(now time.Time, key string, d PlannerData) (journal.Entry, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return journal.Entry{}, err
	}
	return journal.Entry{TS: now, Kind: journal.KindNote, Unit: key, Data: data}, nil
}

// newID names a note. Random, not the entry's time: two notes written in
// the same instant must not share one, and "没用" names a note by it.
func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "p" + hex.EncodeToString(b[:])
}
