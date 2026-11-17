package claudefs

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/naozhi/naozhi/internal/osutil"
)

// The one place naozhi writes into the CLI's auto-memory: the Planner's
// remember block (planner/agent) and the settings view's 懂你 section
// (dashboard/ext/memory) both edit <project>/memory/*.md and the MEMORY.md
// index there. The CLI's own sessions write the same files; that race is left
// to the 懂你 section's version check, but the gateway's two writers must not
// interleave a read-modify-write of one index, hence one lock for both.

// MemoryIndexFile is a project's auto-memory index, read by every session
// the CLI starts in that project.
const MemoryIndexFile = "MEMORY.md"

// MemorySourcePlanner is the frontmatter `source` of a memory file the
// Planner's remember block wrote: only those are replaced by another
// remember of the same name, or taken back from the 记下了 card.
const MemorySourcePlanner = "naozhi-planner"

var memoryMu sync.Mutex

// LockMemory takes the process's lock over auto-memory writes and returns
// its release. Hold it across a read-check-write of a memory file and its
// index, and for every EditMemoryIndex.
func LockMemory() (unlock func()) {
	memoryMu.Lock()
	return memoryMu.Unlock
}

// PlannerWrittenMemory reports whether a memory file's frontmatter says the
// Planner's remember block wrote it.
func PlannerWrittenMemory(data []byte) bool {
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		return false
	}
	front, _, ok := bytes.Cut(rest, []byte("\n---"))
	return ok && bytes.Contains(front, []byte("source: "+MemorySourcePlanner))
}

// EditMemoryIndex drops the lines of the index at path that link name's file
// ("(<name>.md)") and, when line is not "", appends line; every other line is
// kept as it was. It reports how many lines it dropped. A missing index is
// created only to add. The caller holds LockMemory.
func EditMemoryIndex(path, name, line string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if errors.Is(err, os.ErrNotExist) && line == "" {
		return 0, nil
	}
	link := "(" + name + ".md)"
	var out []string
	dropped := 0
	if trimmed := strings.TrimRight(string(data), "\n"); trimmed != "" {
		for _, l := range strings.Split(trimmed, "\n") {
			if strings.Contains(l, link) {
				dropped++
				continue
			}
			out = append(out, l)
		}
	}
	if line == "" && dropped == 0 {
		// Nothing to drop: leave the file untouched rather than rewrite
		// it, so a delete does not bump the index's version for nothing.
		return 0, nil
	}
	if line != "" {
		out = append(out, line)
	}
	text := strings.Join(out, "\n")
	if text != "" {
		text += "\n"
	}
	perm := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	return dropped, osutil.WriteFileAtomic(path, []byte(text), perm)
}
