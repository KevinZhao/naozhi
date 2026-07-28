// Package gitinfo reads the git branch / worktree state of a directory by
// parsing the on-disk git layout directly — no `git` subprocess.
//
// Why no exec: the dashboard resolves this for whatever workspace a session
// runs in, i.e. an operator-supplied path. Spawning a child process per
// lookup would add ~5-15 ms and put an attacker-influenced cwd on a process
// boundary (git honours `.git/config` hooks-ish knobs, core.fsmonitor,
// include.path …). Reading two small files is faster and has no execution
// surface. `DetectGitHubRemote` already sets this precedent for `.git/config`.
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

// State describes the git checkout a directory belongs to. The zero value
// means "not a git repository" (callers check the bool that Detect returns).
type State struct {
	// Root is the absolute path of the working tree the directory belongs to
	// (the dir that holds `.git`), not the dir passed to Detect.
	Root string
	// Branch is the short branch name ("master", "feat/x"). Empty when HEAD
	// is detached.
	Branch string
	// HeadSHA is the abbreviated (7 hex) commit id. Only set for a detached
	// HEAD — on a branch the sha carries no extra signal for the operator and
	// reading it would cost another file read (packed-refs walk).
	HeadSHA string
	// Detached is true when HEAD points at a commit rather than a branch
	// (rebase, bisect, `git checkout <sha>`).
	Detached bool
	// Worktree is the linked-worktree name (the `.git/worktrees/<name>`
	// segment). Empty for the main working tree.
	Worktree string
	// Repo is the main working tree's directory name — the repository
	// identity that a linked worktree shares with its parent. Equal to
	// filepath.Base(Root) for the main tree.
	Repo string
}

// maxWalkUp caps how many parent directories Detect inspects before giving
// up. A session cwd is normally the repo root or one or two levels in; 40
// is far beyond any real layout while keeping the syscall count bounded for
// a path like "/a/b/c/…/z" that is not in a repo at all.
const maxWalkUp = 40

// maxHeadBytes caps the HEAD read. A well-formed HEAD is <100 bytes; the cap
// keeps a hostile/corrupt repo from making us read a large file into memory.
const maxHeadBytes = 4 << 10

// maxRefNameBytes caps the reported branch name. git itself has no hard
// refname limit beyond the filesystem's, so cap defensively — the value is
// rendered in a fixed-width dashboard chip.
const maxRefNameBytes = 255

// Detect resolves the git state of dir, walking up to the nearest ancestor
// that holds a `.git` entry. The bool is false when dir is not inside a git
// working tree, or when the layout is unreadable / malformed.
//
// dir must be absolute and already validated by the caller (the dashboard path
// funnels through validateWorkspace). Detect only ever reads.
//
// root, when non-empty, bounds every path Detect will touch: the walk-up stops
// before leaving it, and a `.git` file's gitdir pointer that resolves outside
// it is refused. It must be an absolute, already-symlink-resolved path (the
// same form validateWorkspace produces). Passing "" disables the bound — only
// appropriate for callers with no containment policy at all.
func Detect(dir, root string) (State, bool) {
	if dir == "" || !filepath.IsAbs(dir) {
		return State{}, false
	}
	if root != "" {
		root = filepath.Clean(root)
	}
	cur := filepath.Clean(dir)
	for i := 0; i < maxWalkUp; i++ {
		// Containment is checked on every cursor position, not just the input:
		// validateWorkspace proves the WORKSPACE is inside root, but the
		// ancestor walk would otherwise sail past the boundary and report a
		// parent repo's path + branch — data outside the operator's declared
		// tree. E.g. allowed_root=<repo>/docs would still disclose <repo> and
		// its current branch.
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

// containedIn reports whether p is root or a descendant of it. An empty root
// means "unbounded" and always passes.
//
// Both sides are expected to be Clean and symlink-resolved by the caller, so
// this is a pure lexical check — deliberately NOT osutil.PathContainedInRoot,
// whose inode-walk fallback would add syscalls to a per-cursor hot path and
// pull a dependency into what is otherwise a leaf package.
func containedIn(p, root string) bool {
	if root == "" {
		return true
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// detectAt inspects exactly one candidate working-tree root (no walk-up).
// allowedRoot bounds where a `.git` pointer file may send us; "" disables it.
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
		// The pointer is workspace-controlled content — files inside a workspace
		// are agent-writable by design, so a `.git` file could name an absolute
		// path or "../.." its way out of the tree. readHead's strict shape check
		// keeps that from becoming a file-content oracle, but the containment
		// check is what stops it being a path-existence probe at all.
		if !containedIn(gitDir, allowedRoot) {
			return State{}, false
		}
	default:
		// Symlink or irregular type: do not follow. A `.git` symlink is not a
		// layout git itself creates, and following it would let a workspace
		// redirect our reads outside the tree.
		return State{}, false
	}

	st := State{Root: treeRoot, Repo: filepath.Base(treeRoot)}
	st.Worktree, st.Repo = worktreeIdentity(gitDir, st.Repo)

	branch, sha, ok := readHead(gitDir)
	if !ok {
		// A `.git` that exists but has no parseable HEAD is not a checkout we
		// can describe (freshly `git init`-ed dirs do have HEAD, so this is
		// the corrupt/foreign case).
		return State{}, false
	}
	st.Branch, st.HeadSHA = branch, sha
	st.Detached = branch == ""
	return st, true
}

// readGitDirPointer parses the "gitdir: <path>" line of a `.git` file and
// returns the absolute git-dir path. Relative pointers resolve against the
// working tree root, which is what git does.
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

// worktreeIdentity derives the linked-worktree name and the shared repository
// name from a git-dir path. For anything that is not a linked worktree — a main
// tree, a submodule — it returns ("", fallbackRepo), i.e. "this checkout stands
// on its own".
//
// A linked worktree's git dir is "<common>/worktrees/<name>", but the path
// shape alone is not a safe test: a main tree in a directory literally named
// "worktrees" has gitDir "<…>/worktrees/.git", whose parent is also named
// "worktrees" — that misread a plain checkout as a worktree named ".git", and
// this repo keeps its worktrees under ".claude/worktrees/" so one `git init`
// there would trigger it.
//
// The authoritative marker is the "commondir" file git writes into every linked
// worktree's git dir (verified present in every real worktree; it holds the
// relative path back to the common git dir). Requiring it both rejects the
// coincidental-path case and yields the true common git dir, which is what
// makes the repo name right for a worktree of a bare repo or of a submodule —
// layouts where "two levels above worktrees" is not a working tree at all.
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
// name. Three shapes, all observed with real git:
//
//	<main-tree>/.git          → "<main-tree>"  (normal repo: strip the .git)
//	<name>.git                → "<name>"       (bare repo: strip the suffix)
//	<super>/.git/modules/<p>  → "<p>"          (submodule: the module path tail)
//
// Falls back to the caller's own directory name when none applies, so a layout
// we don't recognise degrades to "name this checkout after itself" rather than
// surfacing an unrelated parent directory.
func repoNameFromCommonDir(commonDir, fallback string) string {
	base := filepath.Base(commonDir)
	if base == ".git" {
		parent := filepath.Base(filepath.Dir(commonDir))
		// A submodule's common dir is "<super>/.git/modules/<path>", so a
		// ".git" base means we are at a real working tree's git dir.
		if parent == "" || parent == "." || parent == string(filepath.Separator) {
			return fallback
		}
		return parent
	}
	if strings.HasSuffix(base, ".git") && len(base) > len(".git") {
		return strings.TrimSuffix(base, ".git") // bare repo
	}
	if base == "" || base == "." || base == string(filepath.Separator) {
		return fallback
	}
	return base // submodule module dir, or any other named git dir
}

// readCommonDir resolves <gitDir>/commondir. The stored path is relative to
// gitDir (git writes "../.." for a standard worktree). Returns ok=false when
// the file is absent or unreadable — the signal that this is not a linked
// worktree.
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

// readHead parses <gitDir>/HEAD into either a branch name or an abbreviated
// sha. The bool is false when HEAD is missing or its content matches neither
// shape.
//
// Only the two well-formed shapes are accepted ("ref: refs/…" or a 40/64-hex
// object id). That strictness is deliberate: a `.git` FILE lets a workspace
// point gitDir anywhere, so echoing an arbitrary file's first line back into
// the dashboard would turn this into a file-content oracle. Requiring a
// git-shaped value keeps the reported string to data git itself wrote.
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
// directory name cannot spoof the chip or inject terminal escapes into logs.
// Returns "" when nothing printable survives.
//
// Dropping bidi and zero-width runes matters specifically here: the chip exists
// to answer "which checkout am I editing", so a branch named
// "feat/<U+202E>gnp.evil" that renders right-to-left-reversed would make the
// operator read a different branch than the session is on — defeating the
// feature rather than merely looking odd. A zero-width space inside "master"
// likewise renders identically to the real thing.
//
// The rune class mirrors escJs in static/nz_util.js (R202606j-SEC-9, #2344),
// which established this policy for filesystem-derived names reaching the
// dashboard: C0/C1 controls, ZWSP..RLM, the bidi embeddings/overrides, WJ, the
// bidi isolates, LS/PS, and the BOM. Printables (CJK, emoji, accented latin)
// pass through — this is display content, not a log-only sink, so the policy is
// "drop the dangerous class", not osutil.SanitizeForLog's lossy "_" mapping.
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
// Kept as a named predicate so the class is greppable next to its JS twin.
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

// readFileCapped reads at most max bytes from path, rejecting anything that is
// not a regular file.
//
// The open goes through openGitMeta, whose unix build adds O_NONBLOCK — that
// flag is load-bearing, not an optimisation: opening a FIFO for reading blocks
// inside open(2) until a writer arrives, so a `.git/HEAD` fifo would wedge the
// handler goroutine forever and the regular-file check below would never run.
//
// The mode check happens on the OPEN descriptor (f.Stat, i.e. fstat) rather
// than on the path (os.Lstat) so a swap between check and open cannot slip a
// fifo/device past it.
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

// cutPrefix is strings.CutPrefix restricted to the case we need (prefix
// present → remainder), kept local so the parse sites read as one-liners.
func cutPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	return s[len(prefix):], true
}
