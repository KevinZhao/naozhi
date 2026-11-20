package memory

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// The 懂你 section reaches exactly <projects>/<project>/memory/<name>.md:
// project is one directory name the CLI encodes, name one file stem, and the
// path they make is resolved and checked to still have that shape inside the
// resolved projects root, so a symlink can neither lead a read or a write out
// of it nor onto a transcript or any other file inside it.

// indexName is the name parameter that means a project's MEMORY.md.
const indexName = "MEMORY"

// claudeMDName is the name parameter, with no project, that means the user's
// ~/.claude/CLAUDE.md (read only).
const claudeMDName = "CLAUDE"

var (
	errBadProject = errors.New("bad project")
	errBadName    = errors.New("bad name")
	// errOutside is a path that resolves out of the projects root or off the
	// <project>/memory/<file>.md shape (a symlink).
	errOutside = errors.New("outside the memory directories")
	// errGone is a memory file, or its project, that does not exist.
	errGone = errors.New("no such memory")
)

// validProject reports whether project may name a projects-root directory.
func validProject(project string) bool {
	return memoryProjectDirRE.MatchString(project) && !strings.Contains(project, "..")
}

// validName reports whether name may name a memory file (or the index).
// The index goes by its exact name only: on a case-insensitive file system
// (macOS) "memory" opens MEMORY.md too, and would slip past the index's
// rules (it cannot be deleted).
func validName(name string) bool {
	if strings.EqualFold(name, indexName) {
		return name == indexName
	}
	return memorySlugRE.MatchString(name)
}

// memoryDir resolves project's memory directory: it must exist and resolve
// to <root>/<dir>/memory.
func (h *Handler) memoryDir(project string) (string, error) {
	if h.projectsDir == "" {
		return "", errGone
	}
	if !validProject(project) {
		return "", errBadProject
	}
	lexical := filepath.Join(h.projectsDir, project, "memory")
	if !strings.HasPrefix(lexical, h.resolvedPrefix) {
		return "", errOutside
	}
	resolved, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errGone
		}
		return "", err
	}
	if !h.shaped(resolved, 2) {
		return "", errOutside
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.IsDir() {
		return "", errGone
	}
	return resolved, nil
}

// entryFile resolves the file a (project, name) pair names: the memory
// directory resolved, and the file in it resolved again, both kept to the
// memory shape. It returns the resolved directory (where the entry's own
// directory entry is) and the resolved file, which must be a regular file
// that exists.
func (h *Handler) entryFile(project, name string) (dir, file string, err error) {
	if !validName(name) {
		return "", "", errBadName
	}
	dir, err = h.memoryDir(project)
	if err != nil {
		return "", "", err
	}
	if !hasExactEntry(dir, name+".md") {
		return "", "", errGone
	}
	file, err = h.resolveIn(dir, name+".md")
	return dir, file, err
}

// hasExactEntry reports whether dir holds an entry spelled exactly base. A
// case-insensitive file system opens "Foo.md" for foo.md, and a delete by
// that spelling would then miss the index line linking "(foo.md)"; the name
// the page sends is the one the list read from disk.
func hasExactEntry(dir, base string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if e.Name() == base {
			return true
		}
	}
	return false
}

// resolveIn resolves base in the resolved memory directory dir, kept to the
// memory shape; a missing file is errGone.
func (h *Handler) resolveIn(dir, base string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, base))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errGone
		}
		return "", err
	}
	if !h.shaped(resolved, 3) || !strings.HasSuffix(resolved, ".md") {
		return "", errOutside
	}
	st, err := os.Lstat(resolved)
	if err != nil {
		return "", errGone
	}
	if !st.Mode().IsRegular() {
		return "", errOutside
	}
	return resolved, nil
}

// shaped reports whether resolved is n path elements under the resolved
// projects root with "memory" second: <dir>/memory (2) or
// <dir>/memory/<file> (3), <dir> a name the CLI could have encoded.
func (h *Handler) shaped(resolved string, n int) bool {
	if h.resolvedPrefix == "" || !strings.HasPrefix(resolved, h.resolvedPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(resolved, h.resolvedPrefix), string(filepath.Separator))
	return len(parts) == n && memoryProjectDirRE.MatchString(parts[0]) && parts[1] == "memory" &&
		(n == 2 || (parts[2] != "" && parts[2] != "." && parts[2] != ".."))
}

// directLoopback reports whether r reached this node's own listener straight
// from a loopback address: the one kind of request the 懂你 section takes a
// write from (D126, decision B; httputil.DirectLoopback).
func directLoopback(r *http.Request) bool { return httputil.DirectLoopback(r) }

// plannerOwn reports whether project is the Planner's own memory project.
func (h *Handler) plannerOwn(project string) bool {
	return h.plannerProject != "" && strings.EqualFold(project, h.plannerProject)
}
