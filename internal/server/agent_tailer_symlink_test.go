package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestJsonlPathUnderAllowedRootSymlinkResolution pins R260528-SEC-3:
// jsonlPathUnderAllowedRoot must EvalSymlinks both arguments before the
// prefix check so a symlinked component (Docker bind-mount, AMI layout
// differences, macOS /var → /private/var) does not produce a false
// reject for legitimate paths inside allowedRoot.
//
// Pre-fix the function did filepath.Clean only and rejected paths that
// arrived with a symlinked root component because the lexical prefix
// HasPrefix("/private/var/.../projects/<hex>/foo.jsonl",
// "/var/.../projects/") returns false on macOS.
func TestJsonlPathUnderAllowedRootSymlinkResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	// Real root: <tmp>/real/projects.
	realRoot := filepath.Join(tmp, "real", "projects")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Symlink: <tmp>/link → <tmp>/real.
	linkBase := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Join(tmp, "real"), linkBase); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	// JSONL inside the real projects dir.
	jsonlPath := filepath.Join(realRoot, "abc123", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(jsonlPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// allowedRoot expressed via the symlink — both sides resolve to the
	// same physical inode, so the gate must accept.
	allowed := filepath.Join(linkBase, "projects")
	if !jsonlPathUnderAllowedRoot(jsonlPath, allowed) {
		t.Errorf("symlinked allowedRoot rejected legitimate jsonlPath\n  jsonlPath=%q\n  allowedRoot=%q", jsonlPath, allowed)
	}

	// Inverse: the jsonlPath expressed via the symlink, allowedRoot raw.
	jsonlVia := filepath.Join(linkBase, "projects", "abc123", "session.jsonl")
	if !jsonlPathUnderAllowedRoot(jsonlVia, realRoot) {
		t.Errorf("symlinked jsonlPath rejected against real allowedRoot\n  jsonlPath=%q\n  allowedRoot=%q", jsonlVia, realRoot)
	}

	// Negative: a path outside allowedRoot must still be rejected even
	// after symlink resolution.
	outside := filepath.Join(tmp, "elsewhere", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if jsonlPathUnderAllowedRoot(outside, allowed) {
		t.Errorf("path outside allowedRoot was incorrectly accepted: %q", outside)
	}

	// Broken-symlink fallthrough: a non-existent jsonlPath must not be
	// rejected solely because EvalSymlinks fails — the lexical Clean +
	// HasPrefix on the original input still gates it. Production must
	// not deny on a path that has not yet been materialised.
	notYet := filepath.Join(realRoot, "future", "session.jsonl")
	if !jsonlPathUnderAllowedRoot(notYet, allowed) {
		t.Errorf("non-existent jsonlPath inside allowedRoot wrongly rejected\n  jsonlPath=%q\n  allowedRoot=%q", notYet, allowed)
	}

	// Non-absolute jsonlPath must always reject (sanity).
	if jsonlPathUnderAllowedRoot("relative/path.jsonl", allowed) {
		t.Errorf("relative jsonlPath wrongly accepted")
	}
	// jsonlPath equal to allowedRoot must reject (no /sep suffix).
	if jsonlPathUnderAllowedRoot(allowed, allowed) {
		t.Errorf("jsonlPath==allowedRoot wrongly accepted")
	}
}

// TestJsonlPathUnderAllowedRoot_NonExistentLeafThroughSymlinkedRoot pins
// the R260528-SEC-3 followup contract (PR #1383 review CHANGES finding):
// the asymmetric resolve where `allowedRoot` evaluates to a different
// canonical form than `jsonlPath` simply because the leaf doesn't exist
// must not flip a legitimate path into a HasPrefix mismatch.
//
// macOS reproduces this naturally because `/var/folders/.../<temp>` is a
// symlink to `/private/var/folders/.../<temp>`. We force the same shape
// here on every OS by routing allowedRoot through a symlink whose target
// is a sibling-named real directory; the unresolved jsonlPath retains
// the symlink form while the resolved allowedRoot canonicalises through
// the link, and the pre-fix code rejected the legitimate path.
//
// The progressive parent-resolve (resolveExistingAncestor) closes the
// asymmetry: jsonlPath's nearest existing ancestor canonicalises through
// the same symlink so both sides land in the resolved-form world before
// the prefix check.
func TestJsonlPathUnderAllowedRoot_NonExistentLeafThroughSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	t.Parallel()
	tmp := t.TempDir()

	// Real layout: <tmp>/real/projects exists.
	realProjects := filepath.Join(tmp, "real", "projects")
	if err := os.MkdirAll(realProjects, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Linked layout: <tmp>/linked → <tmp>/real (so <tmp>/linked/projects
	// is the symlink-traversed path operators wire as allowedRoot).
	if err := os.Symlink(filepath.Join(tmp, "real"), filepath.Join(tmp, "linked")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Operator-supplied allowedRoot uses the symlink form.
	allowed := filepath.Join(tmp, "linked", "projects")
	// jsonlPath references a session that hasn't materialised yet, also
	// expressed via the symlink form so EvalSymlinks(jsonlPath) fails on
	// the leaf "future/session.jsonl" segment.
	jsonlPath := filepath.Join(tmp, "linked", "projects", "future", "session.jsonl")

	if !jsonlPathUnderAllowedRoot(jsonlPath, allowed) {
		t.Errorf("non-existent jsonlPath inside symlinked allowedRoot wrongly rejected\n  jsonlPath=%q\n  allowedRoot=%q",
			jsonlPath, allowed)
	}

	// And the inverse — allowedRoot as the resolved real form, jsonlPath
	// via the symlink + non-existent leaf. Pre-fix this is the macOS CI
	// shape that produced the original failure.
	if !jsonlPathUnderAllowedRoot(jsonlPath, realProjects) {
		t.Errorf("non-existent jsonlPath via symlink rejected against real allowedRoot\n  jsonlPath=%q\n  allowedRoot=%q",
			jsonlPath, realProjects)
	}

	// Negative: a non-existent path OUTSIDE the allowedRoot must still
	// reject — the parent walk only canonicalises the prefix, it doesn't
	// allow the path to escape.
	outside := filepath.Join(tmp, "elsewhere", "future", "session.jsonl")
	if jsonlPathUnderAllowedRoot(outside, allowed) {
		t.Errorf("non-existent jsonlPath outside allowedRoot wrongly accepted: %q", outside)
	}
}
