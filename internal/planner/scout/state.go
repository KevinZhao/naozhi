package scout

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
)

// Retention of the seen findings and of the run records (§15.6 状态).
const (
	seenRetention = 14 * 24 * time.Hour
	runsRetention = 14 * 24 * time.Hour
	// calendarWindow is how far ahead the calendar cache reaches.
	calendarWindow = 24 * time.Hour
)

// Run states.
const (
	StateOK     = "ok"
	StateFailed = "failed"
)

// Run is one scout run's record, runs/<ID>.json.
type Run struct {
	ID string `json:"id"`
	// Trigger is "schedule" or "manual".
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	// Tools are the tools the session called, first call order.
	Tools []string `json:"tools,omitempty"`
	// Findings are all the run reported; New marks the ones not seen
	// before, the ones the Planner was woken with.
	Findings []RunFinding `json:"findings,omitempty"`
	New      int          `json:"new"`
	Events   int          `json:"events"`
	Woke     bool         `json:"woke"`
}

// RunFinding is a finding as a run recorded it.
type RunFinding struct {
	Finding
	New bool `json:"new"`
}

// store is the scout's files under the state root (§3.7): every write is
// atomic, and an unreadable file reads as empty with a warning.
type store struct{ dir string }

func (st store) path(name string) string { return filepath.Join(st.dir, name) }
func (st store) runsDir() string         { return filepath.Join(st.dir, "runs") }

type cursorsFile struct {
	V       int       `json:"v"`
	Cursors Cursors   `json:"cursors"`
	Updated time.Time `json:"updated,omitzero"`
}

type seenEntry struct {
	Source string    `json:"source"`
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
}

type seenFile struct {
	V    int         `json:"v"`
	Seen []seenEntry `json:"seen"`
}

type calendarFile struct {
	V int `json:"v"`
	// At is when the calendar was read.
	At     time.Time `json:"at"`
	Events []Event   `json:"events"`
}

func (st store) read(name string, v any) {
	data, err := os.ReadFile(st.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(data, v)
	}
	if err != nil {
		slog.Warn("scout: state file unreadable; starting it empty", "file", name, "err", err)
	}
}

func (st store) write(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := st.path(name)
	if err := datadir.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return osutil.WriteFileAtomic(path, data, 0o600)
}

func (st store) cursors() Cursors {
	var f cursorsFile
	st.read("cursors.json", &f)
	return f.Cursors
}

func (st store) saveCursors(c Cursors, now time.Time) error {
	return st.write("cursors.json", cursorsFile{V: 1, Cursors: c, Updated: now})
}

// fresh returns the findings not seen in the last seenRetention, in order,
// and the seen table with them added at now and older entries dropped, for
// saveSeen once they reached the Planner.
func (st store) fresh(findings []Finding, now time.Time) ([]Finding, seenFile) {
	var f seenFile
	st.read("seen.json", &f)
	seen := map[string]bool{}
	kept := f.Seen[:0]
	for _, e := range f.Seen {
		if now.Sub(e.At) <= seenRetention {
			kept = append(kept, e)
			seen[Finding{Source: e.Source, ID: e.ID, Kind: e.Kind}.key()] = true
		}
	}
	var fresh []Finding
	for _, x := range findings {
		if seen[x.key()] {
			continue
		}
		seen[x.key()] = true
		fresh = append(fresh, x)
		kept = append(kept, seenEntry{Source: x.Source, ID: x.ID, Kind: x.Kind, At: now})
	}
	return fresh, seenFile{V: 1, Seen: kept}
}

func (st store) saveSeen(f seenFile) error { return st.write("seen.json", f) }

// saveCalendar keeps the events of the next calendarWindow, by start. nil
// events is a run that did not read the calendar: the cache keeps what it
// had, less the events already over.
func (st store) saveCalendar(events *[]Event, now time.Time) (int, error) {
	var f calendarFile
	if events == nil {
		st.read("calendar.json", &f)
		events = &f.Events
	} else {
		f.At = now
	}
	var keep []Event
	for _, e := range *events {
		if e.End.After(now) && e.Start.Before(now.Add(calendarWindow)) {
			keep = append(keep, e)
		}
	}
	slices.SortStableFunc(keep, func(a, b Event) int { return a.Start.Compare(b.Start) })
	return len(keep), st.write("calendar.json", calendarFile{V: 1, At: f.At, Events: keep})
}

// Calendar is the cached calendar of the next 24 hours as of the last run
// that read it.
func (st store) Calendar() []Event {
	var f calendarFile
	st.read("calendar.json", &f)
	return f.Events
}

func (st store) saveRun(r Run) error {
	return st.write(filepath.Join("runs", r.ID+".json"), r)
}

// lastRun is the newest run record; run IDs sort by start time.
func (st store) lastRun() (Run, bool) { return st.newestRun(false) }

// newestRun is the newest run record, only among the runs that went
// through when ok.
func (st store) newestRun(ok bool) (Run, bool) {
	ents, _ := os.ReadDir(st.runsDir())
	for i := len(ents) - 1; i >= 0; i-- {
		name := ents[i].Name()
		if !strings.HasSuffix(name, ".json") || osutil.IsAtomicTempName(name) {
			continue
		}
		var r Run
		st.read(filepath.Join("runs", name), &r)
		if r.ID != "" && (!ok || r.State == StateOK) {
			return r, true
		}
	}
	return Run{}, false
}

// pruneRuns removes the run records older than runsRetention.
func (st store) pruneRuns(now time.Time) {
	ents, _ := os.ReadDir(st.runsDir())
	for _, e := range ents {
		started, err := time.ParseInLocation(runIDTime, strings.SplitN(e.Name(), "-", 2)[0], time.Local)
		if err == nil && now.Sub(started) > runsRetention {
			_ = os.Remove(filepath.Join(st.runsDir(), e.Name()))
		}
	}
}
