package apiwatch

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// keepDays is how long the day files stay on disk.
	keepDays = 14
	// memDays is how far back the store answers from memory.
	memDays = 7
	// maxMem bounds the records held in memory, whatever memDays allows.
	maxMem = 100_000
	// maxSeen bounds the frame ids remembered to drop a frame seen twice.
	maxSeen = 4096
	// dayLayout names the day files: <dir>/2006-01-02.jsonl, in local time.
	dayLayout = "2006-01-02"
)

// Store holds the records: in memory for queries, appended to one JSONL
// file per day. A nil *Store records nothing and reports empty.
type Store struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	recs    []Record // oldest first
	seen    map[string]struct{}
	seenQ   []string
	file    *os.File
	fileDay string
}

// Open returns a store writing under dir, loaded with the last memDays of
// records and with files past keepDays removed. An empty dir keeps records
// in memory only.
func Open(dir string) *Store {
	s := &Store{dir: dir, now: time.Now, seen: map[string]struct{}{}}
	if dir == "" {
		return s
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("apiwatch: create dir", "dir", dir, "err", err)
		s.dir = ""
		return s
	}
	s.prune()
	s.load()
	return s
}

// Record keeps r. It runs on a CLI read loop: it only appends to memory and
// to the day file, and never fails the caller.
func (s *Store) Record(r Record) {
	if s == nil {
		return
	}
	if r.At.IsZero() {
		r.At = s.now()
	}
	r.Error = clip(r.Error)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.UUID != "" {
		if _, dup := s.seen[r.UUID]; dup {
			return
		}
		s.seen[r.UUID] = struct{}{}
		s.seenQ = append(s.seenQ, r.UUID)
		if len(s.seenQ) > maxSeen {
			delete(s.seen, s.seenQ[0])
			s.seenQ = s.seenQ[1:]
		}
	}
	s.recs = append(s.recs, r)
	s.trimLocked()
	s.appendLocked(r)
}

// trimLocked drops records past memDays or past maxMem.
func (s *Store) trimLocked() {
	cut := s.now().Add(-memDays * 24 * time.Hour)
	i := 0
	for i < len(s.recs) && (s.recs[i].At.Before(cut) || len(s.recs)-i > maxMem) {
		i++
	}
	if i > 0 {
		s.recs = append(s.recs[:0:0], s.recs[i:]...)
	}
}

func (s *Store) appendLocked(r Record) {
	if s.dir == "" {
		return
	}
	day := r.At.Local().Format(dayLayout)
	if s.file == nil || s.fileDay != day {
		if s.file != nil {
			_ = s.file.Close()
			s.file = nil
			s.prune()
		}
		f, err := os.OpenFile(filepath.Join(s.dir, day+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			slog.Warn("apiwatch: open day file", "err", err)
			return
		}
		s.file, s.fileDay = f, day
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	if _, err := s.file.Write(append(b, '\n')); err != nil {
		slog.Warn("apiwatch: append", "err", err)
	}
}

// Close closes the day file.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// dayFiles lists the day files under dir with their day, oldest first.
func (s *Store) dayFiles() (names []string, days []time.Time) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		d, err := time.ParseInLocation(dayLayout, strings.TrimSuffix(name, ".jsonl"), time.Local)
		if err != nil {
			continue
		}
		names, days = append(names, name), append(days, d)
	}
	return names, days
}

// prune removes the day files past keepDays.
func (s *Store) prune() {
	cut := s.now().AddDate(0, 0, -keepDays)
	names, days := s.dayFiles()
	for i, d := range days {
		if d.Before(cut) {
			_ = os.Remove(filepath.Join(s.dir, names[i]))
		}
	}
}

// load reads the day files of the last memDays into memory; a line that
// does not decode is skipped.
func (s *Store) load() {
	cut := s.now().Add(-memDays * 24 * time.Hour)
	names, days := s.dayFiles()
	for i, d := range days {
		if d.AddDate(0, 0, 1).Before(cut) {
			continue
		}
		f, err := os.Open(filepath.Join(s.dir, names[i]))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.At.Before(cut) {
				continue
			}
			s.recs = append(s.recs, r)
		}
		_ = f.Close()
	}
	sort.SliceStable(s.recs, func(a, b int) bool { return s.recs[a].At.Before(s.recs[b].At) })
	s.trimLocked()
	for _, r := range s.recs {
		if r.UUID != "" {
			s.seen[r.UUID] = struct{}{}
			s.seenQ = append(s.seenQ, r.UUID)
		}
	}
	if n := len(s.seenQ) - maxSeen; n > 0 {
		for _, id := range s.seenQ[:n] {
			delete(s.seen, id)
		}
		s.seenQ = s.seenQ[n:]
	}
}

// since copies the records at or after t.
func (s *Store) since(t time.Time) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := sort.Search(len(s.recs), func(i int) bool { return !s.recs[i].At.Before(t) })
	return append([]Record(nil), s.recs[i:]...)
}
