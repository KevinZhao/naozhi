// Package note derives the gateway's side notes on a session
// (docs/rfc/planner.md §15.3 ③): its health entering "waiting for you",
// "stalled" or "failed" and recovering, its "等你" items opening and closing,
// the Planner dispatching it and it reporting to the Planner. The notes are
// shown in the session view only; they never reach the CLI and are not
// events of the session's own log.
//
// Nothing is stored here: health and inbox notes are read back from the
// Planner journal (the health and notify entries already written there), the
// dispatch from the session's sidecar file and the reports from planner:main's
// history, so a restart keeps them. Nothing here calls a model.
package note

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
)

// Window and Limit bound one List: the notes of the last Window, at most
// Limit of them, the newest kept.
const (
	Window = 7 * 24 * time.Hour
	Limit  = 100
)

// Kind is what a note is about. Values are wire-stable.
type Kind string

const (
	KindHealth   Kind = "health"   // a health transition
	KindInbox    Kind = "inbox"    // a "等你" item opened or closed
	KindDispatch Kind = "dispatch" // the Planner dispatched the session
	KindReport   Kind = "report"   // the session's message reached the Planner
)

// Note is one side note on session Key. ID is stable across reads and
// unique within the session, the anchor a push opens to.
type Note struct {
	ID   string    `json:"id"`
	Key  string    `json:"key"`
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`
	// Health notes: the states left and entered, the error class and the
	// gateway's evidence.
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	ErrClass string `json:"error_class,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Inbox notes: open or close, the item and its kind, who closed it and
	// its title.
	Event    string `json:"event,omitempty"`
	Item     string `json:"item,omitempty"`
	ItemKind string `json:"item_kind,omitempty"`
	ClosedBy string `json:"closed_by,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Health states a note is written for: entering one, or leaving one for a
// healthy state. The values are the health machine's.
var (
	noteStates    = []string{"waiting", "stalled", "failed"}
	healthyStates = []string{"active", "idle"}
)

// healthData and notifyData are the fields of the journal entries a note
// reads (internal/monitor/health, internal/notify).
type healthData struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Evidence string `json:"evidence"`
	ErrClass string `json:"error_class"`
	Systemic bool   `json:"systemic"`
}

type notifyData struct {
	Event    string `json:"event"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	ClosedBy string `json:"closed_by"`
}

// FromEntry is the note a journal entry makes, if any.
func FromEntry(e journal.Entry) (Note, bool) {
	if e.Unit == "" {
		return Note{}, false
	}
	n := Note{Key: e.Unit, Time: e.TS}
	switch e.Kind {
	case journal.KindHealth:
		n.Kind = KindHealth
		var d healthData
		if json.Unmarshal(e.Data, &d) != nil || d.Systemic {
			return Note{}, false
		}
		entered := slices.Contains(noteStates, d.To)
		recovered := slices.Contains(noteStates, d.From) && slices.Contains(healthyStates, d.To)
		if !entered && !recovered {
			return Note{}, false
		}
		n.ID = fmt.Sprintf("h%d", e.TS.UnixNano())
		n.From, n.To, n.ErrClass, n.Evidence = d.From, d.To, d.ErrClass, d.Evidence
	case journal.KindNotify:
		var d notifyData
		if json.Unmarshal(e.Data, &d) != nil || d.ID == "" || d.Event != "open" && d.Event != "close" {
			return Note{}, false
		}
		n.Kind, n.ID = KindInbox, d.Event+"-"+d.ID
		n.Event, n.Item, n.ItemKind, n.ClosedBy = d.Event, d.ID, d.Kind, d.ClosedBy
	default:
		return Note{}, false
	}
	return n, true
}

// Config wires a Notes. Every field may be nil: that source adds nothing.
type Config struct {
	Journal *journal.Journal
	// Item returns an inbox item's title; ok is false once the inbox no
	// longer keeps it.
	Item func(id string) (title string, ok bool)
	// Dispatched returns when the Planner dispatched key; zero when it did
	// not.
	Dispatched func(key string) time.Time
	// Reports returns when key's messages reached the Planner.
	Reports func(key string) []time.Time
}

// Notes lists a session's side notes and pushes the new ones. Safe for
// concurrent use.
type Notes struct {
	cfg  Config
	push atomic.Pointer[func(Note)]
}

// New returns a Notes over cfg; it pushes once SetPush binds an exit.
func New(cfg Config) *Notes {
	n := &Notes{cfg: cfg}
	if cfg.Journal != nil {
		cfg.Journal.Observe(n.observe)
	}
	return n
}

// SetPush binds the exit new journal notes go to (the dashboard hub). fn
// runs on the journal writer's goroutine and must not block.
func (n *Notes) SetPush(fn func(Note)) { n.push.Store(&fn) }

func (n *Notes) observe(e journal.Entry) {
	fn := n.push.Load()
	if fn == nil {
		return
	}
	if nt, ok := FromEntry(e); ok {
		(*fn)(n.fill(nt))
	}
}

// fill adds an inbox note's title.
func (n *Notes) fill(nt Note) Note {
	if nt.Kind != KindInbox || n.cfg.Item == nil {
		return nt
	}
	if title, ok := n.cfg.Item(nt.Item); ok {
		nt.Title = title
	}
	return nt
}

// List returns key's notes of the Window before now, oldest first, at most
// Limit of them.
func (n *Notes) List(key string, now time.Time) []Note {
	since := now.Add(-Window)
	var out []Note
	if n.cfg.Journal != nil {
		entries, err := n.cfg.Journal.Read(since, now.Add(time.Minute))
		if err != nil {
			slog.Warn("session notes: journal read", "key", key, "err", err)
		}
		for _, e := range entries {
			if e.Unit != key {
				continue
			}
			if nt, ok := FromEntry(e); ok {
				out = append(out, n.fill(nt))
			}
		}
	}
	if n.cfg.Dispatched != nil {
		if at := n.cfg.Dispatched(key); !at.IsZero() && !at.Before(since) {
			out = append(out, Note{ID: "dispatch", Key: key, Time: at, Kind: KindDispatch})
		}
	}
	if n.cfg.Reports != nil {
		for _, at := range n.cfg.Reports(key) {
			if !at.Before(since) {
				out = append(out, Note{ID: fmt.Sprintf("r%d", at.UnixNano()), Key: key, Time: at, Kind: KindReport})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Note) int { return a.Time.Compare(b.Time) })
	if len(out) > Limit {
		out = out[len(out)-Limit:]
	}
	return out
}
