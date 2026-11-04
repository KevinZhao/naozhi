package health

import (
	"bytes"
	"cmp"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// IndexVersion is the index.json format this build writes.
const IndexVersion = 1

// indexResultRunes caps an entry's result summary.
const indexResultRunes = 280

// IndexEntry is one session's row in index.json, which the Planner reads
// instead of asking every session how it is doing (docs/rfc/planner.md §6.2).
type IndexEntry struct {
	Key string `json:"key"`
	// Node is a remote session's node, "" for this one's.
	Node string `json:"node,omitempty"`
	// Name is the cross-session name the CLI was started with (--name);
	// "" for backends that take none and for remote sessions.
	Name       string    `json:"name"`
	Title      string    `json:"title"`
	Workspace  string    `json:"workspace"`
	Purpose    string    `json:"purpose"`
	Health     State     `json:"health"`
	LastActive time.Time `json:"last_active,omitzero"`
	EventLog   string    `json:"event_log,omitempty"`
	// Result is the latest assistant reply's summary, truncated.
	Result              string `json:"result"`
	DispatchedByPlanner bool   `json:"dispatched_by_planner"`
}

type indexFile struct {
	V        int          `json:"v"`
	Sessions []IndexEntry `json:"sessions"`
}

// writeIndex writes the facts' index rows with their units' health,
// atomically and only when the content changed.
func (m *Machine) writeIndex(facts []Fact) {
	if m.cfg.IndexPath == "" {
		return
	}
	f := indexFile{V: IndexVersion, Sessions: []IndexEntry{}}
	for i := range facts {
		e := facts[i].Index
		if e == nil {
			continue
		}
		row := *e
		row.Health = Unknown
		if u := m.units[facts[i].ID]; u != nil {
			row.Health, row.Workspace = u.State, u.Workspace
		} else {
			row.Workspace = m.workspaceFor(&facts[i])
		}
		row.Result = textutil.TruncateRunes(row.Result, indexResultRunes)
		f.Sessions = append(f.Sessions, row)
	}
	slices.SortFunc(f.Sessions, func(a, b IndexEntry) int {
		return cmp.Or(cmp.Compare(a.Node, b.Node), cmp.Compare(a.Key, b.Key))
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		slog.Warn("health: encode index.json", "err", err)
		return
	}
	if bytes.Equal(data, m.lastIdx) {
		return
	}
	err = datadir.EnsureDir(filepath.Dir(m.cfg.IndexPath))
	if err == nil {
		err = osutil.WriteFileAtomic(m.cfg.IndexPath, data, 0o600)
	}
	if err != nil {
		slog.Warn("health: write index.json", "path", m.cfg.IndexPath, "err", err)
		return
	}
	m.lastIdx = data
}
