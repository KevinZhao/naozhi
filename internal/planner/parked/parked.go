// Package parked is the Planner's 搁置区 (docs/rfc/planner.md §15.14 ⑤,
// D116): planner/parked.md, the "等你定" items set aside so the handoff
// stops copying them shift after shift. An item moves in when it has been
// written unchanged in the handoffs for more than StaleDays days (Sweep), or
// at once when the user says to put it down (Park, the Planner's park
// block). Each line carries the item's source and the day it moved in. The
// file is plain Markdown the Planner reads and the user may edit: deleting
// a line takes the item out; lines the gateway did not write are kept as
// they are.
package parked

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// StaleDays is how many days an item may sit unchanged in "等你定" before
// it moves in: more than this many calendar days since it was first written
// as it reads now.
const StaleDays = 3

// Caps.
const (
	// TextRunes caps an item as kept, its source included.
	TextRunes = 400
	// SourceRunes caps a parked item's source.
	SourceRunes = 120
	// maxDecide bounds the items tracked: a handoff lists far fewer.
	maxDecide = 200
)

// header opens a new parked.md.
const header = "# 搁置区\n\n超过 3 天没动的\"等你定\"和你说放下的事，每条带出处和移入日期，交接里不再重抄。要重新拿起来就跟 Planner 说；不想再看到的删掉那一行。\n"

// byUserMark ends the line of an item the user put down rather than one
// that went stale.
const byUserMark = "你说放下"

// lineRE is a line the gateway wrote: "- <item> · 移入 <YYYY-MM-DD>", the
// item put down by the user marked after the day.
var lineRE = regexp.MustCompile(`^- (.+) · 移入 (\d{4}-\d{2}-\d{2})( · ` + byUserMark + `)?$`)

// Entry is one parked item.
type Entry struct {
	// Text is the item as written, its source in it.
	Text string `json:"text"`
	// Day is the local day it moved in (YYYY-MM-DD).
	Day string `json:"day"`
	// ByUser is set for an item the user put down.
	ByUser bool `json:"by_user,omitempty"`
}

// decided is decide.json: when each "等你定" item was first written as it
// reads now, by its normalized text.
type decided struct {
	V     int                  `json:"v"`
	Items map[string]time.Time `json:"items"`
}

// Store is one parked.md and the tracking of the handoffs' "等你定" items
// beside it. Safe for concurrent use.
type Store struct {
	path, decidePath string
	now              func() time.Time
	mu               sync.Mutex
}

// New returns the Store of the 搁置区 at path, tracking the items in
// decidePath. nil now is time.Now.
func New(path, decidePath string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, decidePath: decidePath, now: now}
}

// Path is the 搁置区's file.
func (s *Store) Path() string { return s.path }

// List returns the parked items in the file's order; none when it is
// missing.
func (s *Store) List() ([]Entry, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lines, err := s.read()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, l := range lines {
		if e, ok := parseLine(l); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// Park puts an item the user put down into the 搁置区: text is the item,
// source where it came from (a task key, a promise ID, an #issue, the time
// the user said it). The same item parked again, from any source, is not
// listed twice.
func (s *Store) Park(text, source string) (Entry, error) {
	text, source = Clean(text), Clean(source)
	switch {
	case s == nil || s.path == "":
		return Entry{}, errors.New("没有状态目录，搁置不了")
	case text == "":
		return Entry{}, errors.New("缺少要搁置的事")
	case source == "":
		return Entry{}, errors.New("缺少出处")
	}
	// Only the text is cut, so the source the line must carry stays whole.
	src := "（出处：" + textutil.TruncateRunes(source, SourceRunes) + "）"
	if room := TextRunes - utf8.RuneCountInString(src); utf8.RuneCountInString(text) > room {
		text = textutil.TruncateRunesNoEllipsis(text, room-1) + "…"
	}
	e := Entry{Text: text + src, Day: s.now().Format(time.DateOnly), ByUser: true}
	s.mu.Lock()
	defer s.mu.Unlock()
	lines, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	for _, l := range lines {
		if p, ok := parseLine(l); ok && core(p.Text) == core(e.Text) {
			return p, nil
		}
	}
	return e, s.write(append(lines, renderLine(e)))
}

// Sweep takes the "等你定" items of a fresh handoff, each one line, and
// returns which to keep in it (keep[i] for items[i]) and how many the
// 搁置区 lists after it. An item already parked is not kept; one written
// unchanged for more than StaleDays days moves in. The tracking forgets the
// items the handoff no longer lists: those were decided or dropped.
func (s *Store) Sweep(items []string) (keep []bool, moved, total int, err error) {
	keep = make([]bool, len(items))
	if s == nil || s.path == "" {
		for i := range keep {
			keep[i] = true
		}
		return keep, 0, 0, nil
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	lines, err := s.read()
	if err != nil {
		return nil, 0, 0, err
	}
	// An item is parked by its words without the source it ends with, so
	// one the Planner copies back writing its source its own way stays out.
	parked := map[string]bool{}
	for _, l := range lines {
		if e, ok := parseLine(l); ok {
			parked[core(e.Text)] = true
			total++
		}
	}
	prev := s.readDecided()
	next := decided{V: 1, Items: map[string]time.Time{}}
	var add []string
	for i, it := range items {
		it = textutil.TruncateRunes(Clean(it), TextRunes)
		k := norm(it)
		switch {
		case k == "":
			keep[i] = true
			continue
		case parked[core(it)]:
			continue
		}
		since, ok := prev.Items[k]
		if !ok || since.After(now) {
			since = now
		}
		if days(since, now) > StaleDays {
			parked[core(it)] = true
			add = append(add, renderLine(Entry{Text: it, Day: now.Format(time.DateOnly)}))
			moved++
			continue
		}
		keep[i] = true
		if len(next.Items) < maxDecide {
			next.Items[k] = since
		}
	}
	if len(add) > 0 {
		if err := s.write(append(lines, add...)); err != nil {
			return nil, 0, total, err
		}
	}
	if err := s.writeDecided(next); err != nil {
		return keep, moved, total + moved, err
	}
	return keep, moved, total + moved, nil
}

// read is parked.md's lines, nil when it is missing.
func (s *Store) read() ([]string, error) {
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("读不了搁置区：%w", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n"), nil
}

// write replaces parked.md with lines, a new file starting with the header.
func (s *Store) write(lines []string) error {
	text := strings.Join(lines, "\n")
	if !hasText(lines) {
		text = header + "\n" + text
	}
	if err := datadir.EnsureDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	if err := osutil.WriteFileAtomic(s.path, []byte(strings.TrimLeft(text, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("写不了搁置区：%w", err)
	}
	return nil
}

// hasText reports whether lines hold text other than entries (the
// header of a file the gateway started, the user's own notes), so no
// second header goes in front of them.
func hasText(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if _, ok := parseLine(l); !ok {
			return true
		}
	}
	return false
}

// readDecided is the tracking; an unreadable one starts afresh, which only
// delays a move.
func (s *Store) readDecided() decided {
	var d decided
	if data, err := os.ReadFile(s.decidePath); err == nil {
		_ = json.Unmarshal(data, &d)
	}
	if d.Items == nil {
		d.Items = map[string]time.Time{}
	}
	return d
}

func (s *Store) writeDecided(d decided) error {
	if s.decidePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := datadir.EnsureDir(filepath.Dir(s.decidePath)); err != nil {
		return err
	}
	return osutil.WriteFileAtomic(s.decidePath, data, 0o600)
}

func renderLine(e Entry) string {
	l := "- " + e.Text + " · 移入 " + e.Day
	if e.ByUser {
		l += " · " + byUserMark
	}
	return l
}

func parseLine(l string) (Entry, bool) {
	m := lineRE.FindStringSubmatch(strings.TrimSpace(l))
	if m == nil {
		return Entry{}, false
	}
	return Entry{Text: m[1], Day: m[2], ByUser: m[3] != ""}, true
}

// Clean makes s one line without control characters.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// norm is an item's identity: its words without the list bullet, spacing
// or case.
func norm(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"- ", "* ", "• "} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// sourceTailRE is the source an item ends with: a bracketed note, or an
// 出处 after the words.
var sourceTailRE = regexp.MustCompile(`(?:[（(][^（）()]*[）)]|[，,;；]?出处[:：].*)$`)

// core is an item's identity without the source it ends with: two lines
// differing only in how their source is written are one item. An item that
// is nothing but a source keeps it.
func core(s string) string {
	n := norm(s)
	if c := sourceTailRE.ReplaceAllString(n, ""); c != "" {
		return c
	}
	return n
}

// days is how many local calendar days lie between since and now.
func days(since, now time.Time) int {
	loc := now.Location()
	a := since.In(loc)
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc)
	db := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return int(math.Round(db.Sub(da).Hours() / 24))
}
