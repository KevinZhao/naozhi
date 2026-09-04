// Package gitinfo reads the git branch / worktree state of a directory by
// parsing the on-disk git layout directly — no `git` subprocess. The
// dashboard resolves this for an operator-supplied workspace path; spawning a
// child there would put an attacker-influenced cwd on a process boundary
// (git honours .git/config knobs), while reading two small files has none.
package gitinfo

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// State describes the git checkout a directory belongs to; zero value = not a repo.
type State struct {
	// Root is the working tree root (the dir holding .git), not the dir passed to Detect.
	Root string
	// Branch is the short branch name; empty when HEAD is detached.
	Branch string
	// HeadSHA is the abbreviated (7 hex) commit id, set only for a detached HEAD.
	HeadSHA string
	// Detached is true when HEAD points at a commit rather than a branch.
	Detached bool
	// Worktree is the linked-worktree name (.git/worktrees/<name>); empty for the main tree.
	Worktree string
	// Repo is the main working tree's directory name, shared by linked worktrees.
	Repo string
}

// maxWalkUp bounds Detect's ancestor walk (and syscalls) for a deep path outside any repo.
const maxWalkUp = 40

// maxHeadBytes caps the HEAD read so a hostile/corrupt repo cannot make us read a large file.
const maxHeadBytes = 4 << 10

// maxRefNameBytes caps the reported branch name (rendered in a fixed-width chip).
const maxRefNameBytes = 255

// Detect resolves the git state of dir, walking up to the nearest ancestor
// holding a `.git` entry; ok is false outside a working tree or on a malformed
// layout. dir must be absolute and already validated; Detect only reads.
// root, when non-empty, bounds every path touched (walk-up and gitdir
// pointers); it must be absolute and symlink-resolved. "" disables the bound.
func Detect(dir, root string) (State, bool) {
	if dir == "" || !filepath.IsAbs(dir) {
		return State{}, false
	}
	if root != "" {
		root = filepath.Clean(root)
	}
	cur := filepath.Clean(dir)
	for i := 0; i < maxWalkUp; i++ {
		// Checked at every cursor position: validateWorkspace proves the
		// WORKSPACE is inside root, but the walk-up would otherwise disclose a
		// parent repo's path + branch outside the operator's declared tree.
		if !containedIn(cur, root) {
			return State{}, false
		}
		if st, ok := detectAt(cur, root); ok {
			return st, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break // reached the filesystem root
		}
		cur = parent
	}
	return State{}, false
}

// containedIn reports whether p is root or a descendant of it; empty root
// means unbounded. Inputs are Clean and symlink-resolved, so this is a pure
// lexical check (osutil.PathContainedInRoot's inode walk would add syscalls here).
func containedIn(p, root string) bool {
	if root == "" {
		return true
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// detectAt inspects one candidate tree root; allowedRoot bounds where a .git pointer may send us.
func detectAt(treeRoot, allowedRoot string) (State, bool) {
	dotGit := filepath.Join(treeRoot, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return State{}, false
	}

	var gitDir string
	switch {
	case info.IsDir():
		gitDir = dotGit
	case info.Mode().IsRegular():
		// Linked worktree / submodule: a one-line "gitdir: <path>" pointer.
		gitDir, err = readGitDirPointer(dotGit, treeRoot)
		if err != nil {
			return State{}, false
		}
		// The pointer is agent-writable workspace content that could name a
		// path outside the tree; containment stops it being a path-existence
		// probe, readHead's shape check stops it being a file-content oracle.
		if !containedIn(gitDir, allowedRoot) {
			return State{}, false
		}
	default:
		// Symlink or irregular: do not follow; a .git symlink could redirect reads outside the tree.
		return State{}, false
	}

	st := State{Root: treeRoot, Repo: filepath.Base(treeRoot)}
	st.Worktree, st.Repo = worktreeIdentity(gitDir, st.Repo)

	branch, sha, ok := readHead(gitDir)
	if !ok {
		// A .git with no parseable HEAD is corrupt/foreign, not a checkout we can describe.
		return State{}, false
	}
	st.Branch, st.HeadSHA = branch, sha
	st.Detached = branch == ""
	return st, true
}

// readGitDirPointer parses the "gitdir: <path>" line of a .git file into an
// absolute git-dir path; relative pointers resolve against the tree root.
func readGitDirPointer(path, root string) (string, error) {
	data, err := readFileCapped(path, maxHeadBytes)
	if err != nil {
		return "", err
	}
	line := firstLine(data)
	rest, ok := cutPrefix(line, "gitdir:")
	if !ok {
		return "", errors.New("gitinfo: .git file has no gitdir pointer")
	}
	p := strings.TrimSpace(rest)
	if p == "" {
		return "", errors.New("gitinfo: empty gitdir pointer")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return filepath.Clean(p), nil
}

// worktreeIdentity derives the linked-worktree name and shared repository
// name from a git-dir path; for a main tree or submodule it returns
// ("", fallbackRepo). The "<common>/worktrees/<name>" path shape alone is not
// a safe test (a main tree in a directory literally named "worktrees" would
// misread as a worktree named ".git"); the authoritative marker is the
// "commondir" file git writes into every linked worktree's git dir.
func worktreeIdentity(gitDir, fallbackRepo string) (worktree, repo string) {
	parent := filepath.Dir(gitDir)
	if filepath.Base(parent) != "worktrees" {
		return "", fallbackRepo
	}
	commonDir, ok := readCommonDir(gitDir)
	if !ok {
		// "worktrees" in the path but no commondir → not a linked worktree.
		return "", fallbackRepo
	}
	name := sanitizeName(filepath.Base(gitDir))
	if name == "" {
		return "", fallbackRepo
	}
	return name, sanitizeName(repoNameFromCommonDir(commonDir, fallbackRepo))
}

// repoNameFromCommonDir turns a common git dir into the repository's display
// name: "<main-tree>/.git" → "<main-tree>", bare "<name>.git" → "<name>",
// submodule "<super>/.git/modules/<p>" → "<p>". Otherwise it falls back to the
// caller's own directory name rather than surfacing an unrelated parent.
func repoNameFromCommonDir(commonDir, fallback string) string {
	base := filepath.Base(commonDir)
	if base == ".git" {
		parent := filepath.Base(filepath.Dir(commonDir))
		if parent == "" || parent == "." || parent == string(filepath.Separator) {
			return fallback
		}
		return parent
	}
	if strings.HasSuffix(base, ".git") && len(base) > len(".git") {
		return strings.TrimSuffix(base, ".git")
	}
	if base == "" || base == "." || base == string(filepath.Separator) {
		return fallback
	}
	return base
}

// readCommonDir resolves <gitDir>/commondir (relative to gitDir; git writes
// "../.."). ok=false when absent — the signal that this is not a linked worktree.
func readCommonDir(gitDir string) (string, bool) {
	data, err := readFileCapped(filepath.Join(gitDir, "commondir"), maxHeadBytes)
	if err != nil {
		return "", false
	}
	p := strings.TrimSpace(firstLine(data))
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(gitDir, p)
	}
	return filepath.Clean(p), true
}

// readHead parses <gitDir>/HEAD into a branch name or an abbreviated sha.
// Only git-shaped forms ("ref: refs/…" or a 40/64-hex object id) are accepted:
// a `.git` FILE lets a workspace point gitDir anywhere, so echoing an arbitrary
// file's first line into the dashboard would be a file-content oracle.
func readHead(gitDir string) (branch, sha string, ok bool) {
	data, err := readFileCapped(filepath.Join(gitDir, "HEAD"), maxHeadBytes)
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(firstLine(data))
	if rest, isRef := cutPrefix(line, "ref:"); isRef {
		ref := strings.TrimSpace(rest)
		if !strings.HasPrefix(ref, "refs/") || len(ref) > maxRefNameBytes {
			return "", "", false
		}
		name := strings.TrimPrefix(ref, "refs/heads/")
		name = sanitizeName(name)
		if name == "" {
			return "", "", false
		}
		return name, "", true
	}
	if isHexObjectID(line) {
		return "", line[:7], true
	}
	return "", "", false
}

// isHexObjectID reports whether s is a full sha1 (40) or sha256 (64) hex id.
func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

// sanitizeName drops rendering-hostile runes and caps length so a refname or
// directory name cannot spoof the chip or inject terminal escapes into logs;
// "" when nothing printable survives. Bidi/zero-width runes matter here: a
// branch named "feat/<U+202E>gnp.evil" renders reversed and misleads the
// operator about which checkout they are on. The rune class mirrors escJs in
// static/nz_util.js (#2344); printables pass through since this is display content.
func sanitizeName(s string) string {
	if len(s) > maxRefNameBytes {
		s = s[:maxRefNameBytes]
	}
	var b strings.Builder
	for _, r := range s {
		if isUnsafeNameRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// isUnsafeNameRune reports whether r must not survive into a displayed name.
func isUnsafeNameRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f: // C0 + DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1
		return true
	case r >= 0x200b && r <= 0x200f: // ZWSP, ZWNJ, ZWJ, LRM, RLM
		return true
	case r >= 0x202a && r <= 0x202e: // LRE, RLE, PDF, LRO, RLO
		return true
	case r == 0x2060: // word joiner
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0x2028, r == 0x2029: // LS / PS
		return true
	case r == 0xfeff: // BOM
		return true
	}
	return false
}

// readFileCapped reads at most max bytes from path, rejecting non-regular
// files. openGitMeta's O_NONBLOCK is load-bearing (see open_unix.go): without
// it a `.git/HEAD` fifo blocks open(2) before this check can run. The mode
// check uses fstat on the open fd so a check/open swap cannot slip a fifo past.
func readFileCapped(path string, max int) ([]byte, error) {
	f, err := openGitMeta(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max)))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// firstLine returns the first LF-delimited line of data, CR trimmed.
func firstLine(data []byte) string {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		data = data[:i]
	}
	return string(bytes.TrimRight(data, "\r"))
}

// cutPrefix is strings.CutPrefix restricted to the prefix-present case.
func cutPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	return s[len(prefix):], true
}
