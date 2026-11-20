// Package front keeps the Planner's ledger of the user's lines of work
// (工作线台账, docs/rfc/planner.md §15.17, D132): one entry per thing the user
// is driving — its state now, the next step, who it waits on, the sessions
// it was worked in and what was decided. The Planner writes it with front
// action blocks; the open ones ride in every snapshot, so what is in
// progress is the same answer each time it is asked, across changes of
// shift.
//
// State is fronts.json in the Planner state root, written atomically and
// inside the Planner's read scope.
package front

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
)

// Version is the fronts.json format this build reads and writes.
const Version = 1

// Bounds.
const (
	// MaxOpen caps the open fronts: more than a person drives at once, and
	// what the snapshot can list.
	MaxOpen = 30
	// NameRunes, TextRunes and DecisionRunes cap a front's name, each of
	// its state / next / waiting, and one decision.
	NameRunes     = 40
	TextRunes     = 200
	DecisionRunes = 300
	// MaxSessions and MaxDecisions are what a front keeps, newest first.
	MaxSessions  = 8
	MaxDecisions = 20
	// sessionKeyRunes caps a session key named in a front.
	sessionKeyRunes = 200
	// closedRetention is how long a closed front is kept.
	closedRetention = 30 * 24 * time.Hour
	maxBytes        = 4 << 20
)

// Decision is something the user decided on a front.
type Decision struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Front is one line of work.
type Front struct {
	// ID is the Planner's own slug for it, stable across updates.
	ID   string `json:"id"`
	Name string `json:"name"`
	// State is where it stands, Next the next step, Waiting who or what it
	// waits on; each may be empty.
	State   string `json:"state,omitempty"`
	Next    string `json:"next,omitempty"`
	Waiting string `json:"waiting,omitempty"`
	// Sessions are the keys of the sessions it was worked in, newest first.
	Sessions []string `json:"sessions,omitempty"`
	// Decisions are what the user decided on it, newest first.
	Decisions []Decision `json:"decisions,omitempty"`
	Created   time.Time  `json:"created"`
	Updated   time.Time  `json:"updated"`
	// Closed is when it was closed; zero while open.
	Closed time.Time `json:"closed,omitzero"`
}

// Open reports whether f is still in progress.
func (f Front) Open() bool { return f.Closed.IsZero() }

// Update is one change to a front. A nil State, Next or Waiting leaves the
// field; one pointing at "" clears it.
type Update struct {
	ID   string
	Name string
	// State, Next, Waiting: nil leaves, "" clears.
	State, Next, Waiting *string
	// Sessions are keys to put first in the front's sessions.
	Sessions []string
	// Decision is one more decision, "" for none.
	Decision string
	// Close closes the front; an update without it reopens a closed one.
	Close bool
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// Validate trims u and reports what is wrong with it, in the words the
// Planner's receipt uses.
func (u *Update) Validate() error {
	u.ID, u.Name, u.Decision = strings.TrimSpace(u.ID), strings.TrimSpace(u.Name), strings.TrimSpace(u.Decision)
	if !idRE.MatchString(u.ID) {
		return errors.New("front 的 id 只能用小写字母、数字、- 和 _，以字母或数字开头，不超过 40 个")
	}
	if utf8.RuneCountInString(u.Name) > NameRunes {
		return fmt.Errorf("name 超过 %d 字", NameRunes)
	}
	for name, p := range map[string]*string{"state": u.State, "next": u.Next, "waiting": u.Waiting} {
		if p == nil {
			continue
		}
		if *p = strings.TrimSpace(*p); utf8.RuneCountInString(*p) > TextRunes {
			return fmt.Errorf("%s 超过 %d 字", name, TextRunes)
		}
	}
	if utf8.RuneCountInString(u.Decision) > DecisionRunes {
		return fmt.Errorf("decision 超过 %d 字", DecisionRunes)
	}
	if len(u.Sessions) > MaxSessions {
		return fmt.Errorf("sessions 最多 %d 个", MaxSessions)
	}
	for i, k := range u.Sessions {
		if k = strings.TrimSpace(k); k == "" || utf8.RuneCountInString(k) > sessionKeyRunes {
			return errors.New("sessions 里有空的或太长的会话 key")
		}
		u.Sessions[i] = k
	}
	return nil
}

type file struct {
	V      int     `json:"v"`
	Fronts []Front `json:"fronts"`
}

// Store is the ledger.
type Store struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	fronts []Front
	// readOnly keeps a file this build cannot read from being overwritten.
	readOnly bool
}

// New loads the ledger at path; "" keeps it in memory. now nil is time.Now.
func New(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	s := &Store{path: path, now: now}
	if path == "" {
		return s
	}
	f, out, err := jsonfile.Load[file](path, jsonfile.Options{MaxBytes: maxBytes, Label: "planner fronts"})
	switch {
	case err != nil:
		slog.Warn("planner fronts unreadable; changes kept in memory only", "err", err)
		s.readOnly = true
	case out == jsonfile.Parsed && f.V > Version:
		slog.Warn("planner fronts written by a newer naozhi; not overwriting them", "version", f.V)
		s.readOnly = true
	case out == jsonfile.Parsed:
		s.fronts = f.Fronts
	}
	return s
}

// ErrNoName is an update that would make a front with no name.
var ErrNoName = errors.New("新的工作线要有 name")

// ErrTooMany is a new front past MaxOpen.
var ErrTooMany = fmt.Errorf("进行中的工作线已有 %d 条，先关掉做完的", MaxOpen)

// Apply makes the change u (validated by its Validate) and returns the
// front as it now is. A front u.ID does not name yet is made, which needs a
// name.
func (s *Store) Apply(u Update) (Front, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.fronts, func(f Front) bool { return f.ID == u.ID })
	if i < 0 {
		if u.Name == "" {
			return Front{}, ErrNoName
		}
		if !u.Close && s.openLocked() >= MaxOpen {
			return Front{}, ErrTooMany
		}
		s.fronts = append(s.fronts, Front{ID: u.ID, Created: now})
		i = len(s.fronts) - 1
	}
	f := &s.fronts[i]
	if !u.Close && !f.Open() && s.openLocked() >= MaxOpen {
		return Front{}, ErrTooMany
	}
	if u.Name != "" {
		f.Name = u.Name
	}
	for _, p := range []struct {
		dst *string
		src *string
	}{{&f.State, u.State}, {&f.Next, u.Next}, {&f.Waiting, u.Waiting}} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	if len(u.Sessions) > 0 {
		keys := slices.Clone(u.Sessions)
		for _, k := range f.Sessions {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		f.Sessions = keys[:min(len(keys), MaxSessions)]
	}
	if u.Decision != "" {
		f.Decisions = append([]Decision{{At: now, Text: u.Decision}}, f.Decisions...)
		f.Decisions = f.Decisions[:min(len(f.Decisions), MaxDecisions)]
	}
	f.Updated = now
	f.Closed = time.Time{}
	if u.Close {
		f.Closed = now
	}
	out := *f
	s.saveLocked(now)
	return out, nil
}

func (s *Store) openLocked() int {
	n := 0
	for _, f := range s.fronts {
		if f.Open() {
			n++
		}
	}
	return n
}

// Open lists the fronts in progress, the latest updated first.
func (s *Store) Open() []Front {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Front
	for _, f := range s.fronts {
		if f.Open() {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b Front) int {
		return cmp.Or(b.Updated.Compare(a.Updated), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// saveLocked drops the fronts closed longer than closedRetention and writes
// the file.
func (s *Store) saveLocked(now time.Time) {
	s.fronts = slices.DeleteFunc(s.fronts, func(f Front) bool {
		return !f.Open() && now.Sub(f.Closed) > closedRetention
	})
	if s.readOnly || s.path == "" {
		return
	}
	data, err := json.MarshalIndent(file{V: Version, Fronts: s.fronts}, "", "  ")
	if err == nil {
		err = osutil.WriteFileAtomic(s.path, data, 0o600)
	}
	if err != nil {
		slog.Warn("planner fronts save failed", "err", err)
	}
}
