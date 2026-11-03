package activity

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// maxIndexed caps the outputs index; the oldest fall out first. The journal
// keeps them for its retention either way.
const maxIndexed = 2000

// sessionRootPrefix is the file API's root= namespace for a session's working
// tree (internal/dashboard/project files_root.go).
const sessionRootPrefix = "session:"

// Output is one entry of the outputs index: a file a session wrote or a PR it
// opened, merged or linked to.
type Output struct {
	Key  string    `json:"key"`
	WS   string    `json:"ws,omitempty"`
	Time time.Time `json:"time"`
	Activity
	// Root is the file-API root Path is relative to (root=session:<key>);
	// empty for a PR.
	Root string `json:"root,omitempty"`
}

// same reports whether o and n are the same product: one file, or one PR.
func (o Output) same(n Output) bool {
	if o.Key != n.Key {
		return false
	}
	if n.Path != "" {
		return o.Path == n.Path
	}
	return n.URL != "" && o.URL == n.URL
}

// Recorder writes activities to the journal and keeps the outputs index.
// Safe for concurrent use; a nil journal or registry only loses that part.
type Recorder struct {
	j  *journal.Journal
	ws *workspace.Registry

	mu   sync.Mutex
	outs []Output // oldest first, one per product
}

// NewRecorder returns a Recorder over j and ws, either of which may be nil.
func NewRecorder(j *journal.Journal, ws *workspace.Registry) *Recorder {
	return &Recorder{j: j, ws: ws}
}

// Load rebuilds the outputs index from the journal's retained activity
// entries.
func (r *Recorder) Load(now time.Time) error {
	if r.j == nil {
		return nil
	}
	entries, err := r.j.Read(now.Add(-journal.Retention), now.Add(time.Hour))
	for _, e := range entries {
		if e.Kind != journal.KindActivity {
			continue
		}
		var a Activity
		if json.Unmarshal(e.Data, &a) != nil || !a.IsOutput() {
			continue
		}
		r.index(Output{Key: e.Unit, WS: e.WS, Time: e.TS, Activity: a})
	}
	return err
}

// Record journals a as an activity of the session key working in dir and
// indexes it when it is an output. ws is the workspace registered for dir.
func (r *Recorder) Record(key, dir string, a Activity) {
	now := time.Now()
	var wsID string
	if r.ws != nil {
		if w, ok := r.ws.Lookup(dir); ok {
			wsID = w.ID
		}
	}
	if r.j != nil {
		data, err := json.Marshal(a)
		if err == nil {
			err = r.j.Append(journal.Entry{TS: now, WS: wsID, Kind: journal.KindActivity, Unit: key, Data: data})
		}
		if err != nil {
			slog.Warn("activity: journal append failed", "type", a.Type, "err", err)
		}
	}
	if a.IsOutput() {
		r.index(Output{Key: key, WS: wsID, Time: now, Activity: a})
	}
}

// index adds o, replacing the entry for the same product. A PR mentioned
// again without a state keeps the state and type it was recorded with.
func (r *Recorder) index(o Output) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, old := range r.outs {
		if !old.same(o) {
			continue
		}
		if o.State == "" {
			o.State, o.Type = old.State, old.Type
		}
		r.outs = append(r.outs[:i], r.outs[i+1:]...)
		break
	}
	r.outs = append(r.outs, o)
	if n := len(r.outs) - maxIndexed; n > 0 {
		r.outs = append(r.outs[:0:0], r.outs[n:]...)
	}
}

// Outputs returns up to limit outputs, newest first: the session key's, or
// every session's when key is "" (the outputs library). A file's Root is set
// so Root and Path go to the file API as they are.
func (r *Recorder) Outputs(key string, limit int) []Output {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Output{}
	for i := len(r.outs) - 1; i >= 0 && len(out) < limit; i-- {
		o := r.outs[i]
		if key != "" && o.Key != key {
			continue
		}
		if o.Path != "" {
			o.Root = sessionRootPrefix + o.Key
		}
		out = append(out, o)
	}
	return out
}
