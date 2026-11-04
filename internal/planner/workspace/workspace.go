// Package workspace is the Planner's workspace registry,
// <planner root>/workspaces.json (docs/rfc/planner.md §3.7, §15.1).
//
// A workspace is a stable ID ("ws_" + 8 random [a-z0-9]) for one body of work
// that survives renames, moves and extra nodes. Names change and collide, and
// path hashes break on a move or a case-folded spelling, so the ID is what
// other Planner state (journal entries, knowledge, charters) stores. A session
// belongs to the registered path that contains its working directory; Ensure
// registers the directory itself when none does, unless it is a container
// (the projects root or one of its ancestors) that holds workspaces rather
// than being one.
package workspace

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
)

// Version is the registry format this build reads and writes.
const Version = 1

// maxBytes caps the registry read; an entry is a name and a few paths, so
// 4 MiB is tens of thousands of workspaces.
const maxBytes = 4 << 20

const (
	idPrefix   = "ws_"
	idLen      = 8
	idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// Workspace is one registry entry. Paths maps a node ID (config workspace.id)
// to the absolute directories the workspace covers on that node.
type Workspace struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Paths   map[string][]string `json:"paths"`
	Created time.Time           `json:"created"`
}

func (w Workspace) clone() Workspace {
	w.Paths = maps.Clone(w.Paths)
	for k, v := range w.Paths {
		w.Paths[k] = slices.Clone(v)
	}
	return w
}

type file struct {
	V          int         `json:"v"`
	Workspaces []Workspace `json:"workspaces"`
}

// Seed is a directory to register on first start (a project).
type Seed struct {
	Name string
	Path string
}

// Registry is the loaded workspaces.json for one node. Safe for concurrent
// use. Persistence failures are logged, not returned: the in-memory registry
// stays authoritative for the run and the next change retries the save.
type Registry struct {
	path string
	node string
	// containers are resolved directories that are never registered: a
	// workspace there would contain every project created below it later.
	containers []string

	mu sync.Mutex
	ws []Workspace
	// readOnly is set when the file is on disk but could not be used (corrupt,
	// unreadable) or was written by a newer naozhi: saving would replace IDs
	// that cannot be regenerated, so changes stay in memory.
	readOnly    bool
	saveFailing bool
}

// Load reads the registry at path for node. containers are directories (the
// projects root) that Ensure and SeedIfEmpty never register. A missing file
// starts empty. A corrupt or unreadable one is left in place and the registry
// runs in memory only until it is fixed: its IDs cannot be regenerated, so
// starting over would orphan everything stored under them. An empty path
// keeps the registry in memory only.
func Load(path, node string, containers ...string) *Registry {
	r := &Registry{path: path, node: node}
	for _, c := range containers {
		if c, ok := resolve(c); ok {
			r.containers = append(r.containers, c)
		}
	}
	if path == "" {
		return r
	}
	f, out, err := jsonfile.Load[file](path, jsonfile.Options{
		MaxBytes: maxBytes,
		Label:    "workspace registry",
		Corrupt:  jsonfile.LeaveCorrupt,
	})
	if err != nil {
		slog.Warn("workspace registry unusable; changes kept in memory only", "path", path, "err", err)
		r.readOnly = true
		return r
	}
	switch out {
	case jsonfile.CorruptLeft:
		slog.Warn("workspace registry is corrupt; changes kept in memory only until it is fixed and naozhi restarted", "path", path)
		r.readOnly = true
	case jsonfile.Parsed:
		if f.V > Version {
			slog.Warn("workspace registry written by a newer naozhi; not overwriting it", "path", path, "version", f.V)
			r.readOnly = true
		}
		r.ws = f.Workspaces
	}
	return r
}

// List returns every workspace in registration order.
func (r *Registry) List() []Workspace {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Workspace, len(r.ws))
	for i, w := range r.ws {
		out[i] = w.clone()
	}
	return out
}

// LocalPaths returns every directory a workspace covers on this node.
func (r *Registry) LocalPaths() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, w := range r.ws {
		out = append(out, w.Paths[r.node]...)
	}
	return out
}

// Lookup returns the workspace whose path on this node most deeply contains
// dir (an absolute path).
func (r *Registry) Lookup(dir string) (Workspace, bool) {
	dir, ok := resolve(dir)
	if !ok {
		return Workspace{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := r.lookupLocked(dir); i >= 0 {
		return r.ws[i].clone(), true
	}
	return Workspace{}, false
}

// Find returns the workspace ref names — its ID, else a name (any case) no
// other workspace shares — and its first path on this node, "" when it has
// none here.
func (r *Registry) Find(ref string) (w Workspace, dir string, ok bool) {
	ref = strings.TrimSpace(ref)
	r.mu.Lock()
	defer r.mu.Unlock()
	found := slices.IndexFunc(r.ws, func(w Workspace) bool { return w.ID == ref })
	for i := 0; found < 0 && i < len(r.ws); i++ {
		if !strings.EqualFold(r.ws[i].Name, ref) {
			continue
		}
		for j := i + 1; j < len(r.ws); j++ {
			if strings.EqualFold(r.ws[j].Name, ref) {
				return Workspace{}, "", false // a name two workspaces share
			}
		}
		found = i
	}
	if found < 0 || ref == "" {
		return Workspace{}, "", false
	}
	w = r.ws[found].clone()
	if p := w.Paths[r.node]; len(p) > 0 {
		dir = p[0]
	}
	return w, dir, true
}

// Ensure returns the workspace containing dir, registering dir as a new
// workspace named after its base name when no registered path contains it.
// ok is false when dir is not absolute, or is a container (or an ancestor of
// one) outside every workspace: such a dir belongs to none.
func (r *Registry) Ensure(dir string) (Workspace, bool) {
	resolved, ok := resolve(dir)
	if !ok {
		return Workspace{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := r.lookupLocked(resolved); i >= 0 {
		return r.ws[i].clone(), true
	}
	if r.isContainer(resolved) {
		return Workspace{}, false
	}
	w := r.addLocked(filepath.Base(resolved), resolved)
	r.saveLocked()
	return w.clone(), true
}

// SeedIfEmpty registers one workspace per seed when the registry holds none —
// the first start, before any session directory has been seen — and returns
// how many it added. Seeds that are not absolute, repeat a path or are a
// container are skipped.
func (r *Registry) SeedIfEmpty(seeds []Seed) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ws) > 0 {
		return 0
	}
	seen := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		p, ok := resolve(s.Path)
		if !ok || seen[p] || r.isContainer(p) {
			continue
		}
		seen[p] = true
		r.addLocked(cmp.Or(s.Name, filepath.Base(p)), p)
	}
	if len(r.ws) > 0 {
		r.saveLocked()
	}
	return len(r.ws)
}

// lookupLocked returns the index of the workspace with the longest path on
// this node containing dir, or -1. Byte-wise prefix first; then the inode
// walk of osutil.PathContainedInRoot, which matches a case-folded spelling
// (~/Workspace vs ~/workspace) on macOS, only for paths longer than the
// byte-wise best so a shallow exact match cannot hide a deeper folded one.
func (r *Registry) lookupLocked(dir string) int {
	best, bestLen := -1, 0
	for i, w := range r.ws {
		for _, p := range w.Paths[r.node] {
			if len(p) > bestLen && (dir == p || strings.HasPrefix(dir, p+string(filepath.Separator))) {
				best, bestLen = i, len(p)
			}
		}
	}
	for i, w := range r.ws {
		for _, p := range w.Paths[r.node] {
			if len(p) > bestLen && osutil.PathContainedInRoot(dir, p) {
				best, bestLen = i, len(p)
			}
		}
	}
	return best
}

// isContainer reports whether dir is a container or an ancestor of one.
func (r *Registry) isContainer(dir string) bool {
	return slices.ContainsFunc(r.containers, func(c string) bool {
		return osutil.PathContainedInRoot(c, dir)
	})
}

func (r *Registry) addLocked(name, path string) Workspace {
	w := Workspace{
		ID:      r.newIDLocked(),
		Name:    name,
		Paths:   map[string][]string{r.node: {path}},
		Created: time.Now().Truncate(time.Second),
	}
	r.ws = append(r.ws, w)
	return w
}

func (r *Registry) newIDLocked() string {
	for {
		b := make([]byte, idLen)
		for i := range b {
			b[i] = idAlphabet[rand.IntN(len(idAlphabet))]
		}
		id := idPrefix + string(b)
		if !slices.ContainsFunc(r.ws, func(w Workspace) bool { return w.ID == id }) {
			return id
		}
	}
}

// saveLocked persists the registry. Indented because the owner may rename a
// workspace by hand.
func (r *Registry) saveLocked() {
	if r.path == "" || r.readOnly {
		return
	}
	err := r.writeLocked()
	if err != nil {
		level := slog.LevelWarn
		if r.saveFailing {
			level = slog.LevelDebug
		}
		slog.Log(context.Background(), level, "persist workspace registry failed", "path", r.path, "err", err)
		r.saveFailing = true
		return
	}
	if r.saveFailing {
		slog.Info("persist workspace registry recovered", "path", r.path)
		r.saveFailing = false
	}
}

func (r *Registry) writeLocked() error {
	data, err := json.MarshalIndent(file{V: Version, Workspaces: r.ws}, "", "  ")
	if err != nil {
		return err
	}
	if err := datadir.EnsureDir(filepath.Dir(r.path)); err != nil {
		return err
	}
	return osutil.WriteFileAtomic(r.path, data, 0o600)
}

// resolve cleans an absolute path and resolves its symlinks, the form
// osutil.PathContainedInRoot requires. A path that no longer exists keeps its
// cleaned spelling.
func resolve(dir string) (string, bool) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", false
	}
	dir = filepath.Clean(dir)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return dir, true
}
