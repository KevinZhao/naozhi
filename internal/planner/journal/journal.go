// Package journal is the Planner's append-only event log: one JSON object per
// line in <planner root>/log/<YYYY-MM>.jsonl (docs/rfc/planner.md §3.7,
// §15.1). Health transitions, notifications, briefs, loop heartbeats and
// activity records are written here rather than into the per-session event
// logs, so an older naozhi reading its own logs never meets a record type it
// does not know. Readers skip kinds and versions they do not recognise for the
// same reason in the other direction.
//
// Retention is by whole month file: Prune removes the files whose month ended
// more than Retention ago.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
)

// Version is the line format this build writes and the only one it reads.
const Version = 1

// Retention is how long a month file is kept after its month ends.
const Retention = 180 * 24 * time.Hour

// maxLineBytes caps one encoded line. Append refuses anything larger so Read's
// line buffer never has to grow past it.
const maxLineBytes = 1 << 20

const (
	monthLayout = "2006-01"
	fileExt     = ".jsonl"
)

// Kind classifies an entry.
type Kind string

const (
	KindHealth    Kind = "health"
	KindNotify    Kind = "notify"
	KindBrief     Kind = "brief"
	KindHeartbeat Kind = "heartbeat"
	KindActivity  Kind = "activity"
	// KindDialog is a line the gateway wrote into the Planner conversation.
	KindDialog Kind = "dialog"
)

func (k Kind) known() bool {
	switch k {
	case KindHealth, KindNotify, KindBrief, KindHeartbeat, KindActivity, KindDialog:
		return true
	}
	return false
}

// Entry is one journal line. WS is the workspace ID ("" when the unit belongs
// to none); Unit is the session key or other unit ID the entry is about.
type Entry struct {
	V    int             `json:"v"`
	TS   time.Time       `json:"ts"`
	WS   string          `json:"ws"`
	Kind Kind            `json:"kind"`
	Unit string          `json:"unit"`
	Data json.RawMessage `json:"data"`
}

// Journal appends to and reads one log directory. Safe for concurrent use.
type Journal struct {
	dir string
	// loc picks the month file an entry lands in; fixed at Open so a TZ change
	// mid-run cannot split one month across two files.
	loc *time.Location

	mu    sync.Mutex
	f     *os.File // open month file, nil until the first Append
	month string   // month f was opened for
}

// Open creates dir (0700) and returns a Journal over it.
func Open(dir string) (*Journal, error) {
	if dir == "" {
		return nil, errors.New("journal: empty directory")
	}
	if err := datadir.EnsureDir(dir); err != nil {
		return nil, err
	}
	return &Journal{dir: dir, loc: time.Local}, nil
}

// Append writes e as one line to the file of e.TS's month. V is always set to
// Version, a zero TS becomes now, and empty Data becomes {}. Lines are not
// fsynced: a crash may lose the tail, and the torn line it leaves is skipped
// on read.
func (j *Journal) Append(e Entry) error {
	if !e.Kind.known() {
		return fmt.Errorf("journal: unknown kind %q", e.Kind)
	}
	e.V = Version
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage("{}")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("journal: encode %s entry: %w", e.Kind, err)
	}
	if len(line) >= maxLineBytes {
		return fmt.Errorf("journal: %s entry is %d bytes, cap %d", e.Kind, len(line), maxLineBytes)
	}
	line = append(line, '\n')
	month := e.TS.In(j.loc).Format(monthLayout)

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil || j.month != month {
		if err := j.openLocked(month); err != nil {
			return err
		}
	}
	if _, err := j.f.Write(line); err != nil {
		return fmt.Errorf("journal: append: %w", err)
	}
	return nil
}

func (j *Journal) openLocked(month string) error {
	if j.f != nil {
		_ = j.f.Close()
		j.f = nil
	}
	path := filepath.Join(j.dir, month+fileExt)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("journal: open %s: %w", filepath.Base(path), err)
	}
	// A crash mid-write leaves the last line without its newline; terminate it
	// so the next entry is not glued onto the torn one and lost with it.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				_ = f.Close()
				return fmt.Errorf("journal: repair %s: %w", filepath.Base(path), err)
			}
		}
	}
	j.f, j.month = f, month
	return nil
}

// Read returns the entries with since <= TS < until, in append order. Lines
// that do not parse, or carry another version or an unknown kind, are skipped.
func (j *Journal) Read(since, until time.Time) ([]Entry, error) {
	months, err := j.months()
	if err != nil {
		return nil, err
	}
	first := since.In(j.loc).Format(monthLayout)
	last := until.In(j.loc).Format(monthLayout)
	var out []Entry
	for _, m := range months {
		if m < first || m > last {
			continue
		}
		out, err = j.readFile(m, since, until, out)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func (j *Journal) readFile(month string, since, until time.Time, out []Entry) ([]Entry, error) {
	f, err := os.Open(filepath.Join(j.dir, month+fileExt))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil // pruned between listing and opening
		}
		return out, fmt.Errorf("journal: open %s: %w", month, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 2*maxLineBytes)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.V != Version || !e.Kind.known() {
			continue
		}
		if e.TS.Before(since) || !e.TS.Before(until) {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("journal: read %s: %w", month, err)
	}
	return out, nil
}

// Prune removes the month files whose month ended more than Retention before
// now and returns how many it removed.
func (j *Journal) Prune(now time.Time) (int, error) {
	months, err := j.months()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, m := range months {
		start, err := time.ParseInLocation(monthLayout, m, j.loc)
		if err != nil {
			continue
		}
		if now.Sub(start.AddDate(0, 1, 0)) <= Retention {
			continue
		}
		if err := os.Remove(filepath.Join(j.dir, m+fileExt)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("journal: prune %s: %w", m, err)
		}
		removed++
	}
	return removed, nil
}

// months lists the month names of the log files present, oldest first.
func (j *Journal) months() ([]string, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("journal: list: %w", err)
	}
	var out []string
	for _, de := range entries {
		m, ok := strings.CutSuffix(de.Name(), fileExt)
		if !ok || !de.Type().IsRegular() {
			continue
		}
		if _, err := time.Parse(monthLayout, m); err != nil {
			continue
		}
		out = append(out, m) // ReadDir sorts by name, and YYYY-MM sorts by time
	}
	return out, nil
}

// Close closes the open month file. A later Append reopens it.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}
