package project

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
)

// projectsIndexMaxBytes caps the index read; one entry is an absolute path
// plus an int64, so 1 MiB holds several thousand projects.
const projectsIndexMaxBytes = 1 << 20

// projectsIndexVersion is the format this build reads and writes.
const projectsIndexVersion = 1

// indexFile is the on-disk shape of projects-index.json.
type indexFile struct {
	Version   int              `json:"version"`
	CreatedAt map[string]int64 `json:"created_at"`
}

// projectIndex is naozhi's own per-project bookkeeping — today the sidebar
// CreatedAt — keyed by absolute project path, so Scan can remember order
// without writing into a project directory. An empty path keeps it in memory
// only. Not goroutine-safe: the Manager touches it under m.mu.
type projectIndex struct {
	path      string
	createdAt map[string]int64
	// saved is the map last read from or written to disk; save is skipped
	// while createdAt still equals it.
	saved map[string]int64
	// readOnly is set when the file is on disk but could not be used (I/O
	// error, over the cap, symlink, newer version): overwriting it would
	// destroy state this build cannot read, so order stays in memory.
	readOnly bool
}

// loadProjectIndex reads path. A corrupt file is moved aside and the index
// starts empty; only an unreadable file that is still in place makes it readOnly.
func loadProjectIndex(path string) *projectIndex {
	idx := &projectIndex{path: path, createdAt: map[string]int64{}}
	if path == "" {
		return idx
	}
	f, out, err := jsonfile.Load[indexFile](path, jsonfile.Options{
		MaxBytes: projectsIndexMaxBytes,
		Label:    "projects index",
		Corrupt:  jsonfile.PreserveCorrupt,
	})
	if err != nil {
		slog.Warn("projects index unusable; sidebar order kept in memory only", "path", path, "err", err)
		idx.readOnly = true
		return idx
	}
	if out == jsonfile.CorruptPreserved {
		slog.Warn("projects index was corrupt and moved aside; starting empty", "path", path)
	}
	if out == jsonfile.Parsed {
		if f.Version > projectsIndexVersion {
			slog.Warn("projects index written by a newer naozhi; not overwriting it",
				"path", path, "version", f.Version)
			idx.readOnly = true
		}
		for p, ms := range f.CreatedAt {
			if ms != 0 {
				idx.createdAt[p] = ms
			}
		}
	}
	idx.saved = maps.Clone(idx.createdAt)
	return idx
}

// replace swaps in next and persists it when it differs from what is on
// disk. A failed save is logged and retried on the next replace.
func (idx *projectIndex) replace(next map[string]int64) {
	idx.createdAt = next
	if idx.path == "" || idx.readOnly || maps.Equal(next, idx.saved) {
		return
	}
	if err := idx.save(); err != nil {
		slog.Warn("persist projects index failed", "path", idx.path, "err", err)
		return
	}
	idx.saved = maps.Clone(next)
}

func (idx *projectIndex) save() error {
	data, err := json.Marshal(indexFile{Version: projectsIndexVersion, CreatedAt: idx.createdAt})
	if err != nil {
		return fmt.Errorf("marshal projects index: %w", err)
	}
	if err := datadir.EnsureDir(filepath.Dir(idx.path)); err != nil {
		return fmt.Errorf("ensure projects index dir: %w", err)
	}
	if err := osutil.WriteFileAtomic(idx.path, data, 0600); err != nil {
		return fmt.Errorf("save projects index: %w", err)
	}
	return nil
}
