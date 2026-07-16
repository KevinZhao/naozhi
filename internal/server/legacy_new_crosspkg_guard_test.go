package server

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLegacyServerNew_ZeroCrossPkgCallers complements
// TestServerNew_NotReintroduced (in new_options_test.go) by widening
// the scan from the server package alone to the entire repo's Go
// source tree. The package-local test catches re-introduction inside
// internal/server; this one catches outside-package reintroduction
// (cmd/, internal/wireup, internal/embed/test fixtures, etc.) where
// a future caller might write `server.New(...)` if the wrapper were
// ever resurrected via revert.
//
// Two complementary scans:
//
//  1. Verify zero `server.New(` callers exist anywhere in the tree.
//     Previously this was the implicit "production has no callers"
//     claim the deletion relied on; the test makes the claim
//     enforceable across drift.
//  2. Verify zero `func New(addr string` definitions exist outside
//     server.go. Catches accidental "I'll just add it back here"
//     refactors that mirror the deleted shape elsewhere.
//
// Enumeration is hermetic: the primary path asks git for the file
// list (tracked + untracked-but-not-ignored), so gitignored artifacts
// — stale full checkouts (`/naozhi`), nested worktrees under
// .claude/worktrees, build output — can never surface as false
// offenders. An earlier WalkDir-only version scanned everything under
// the repo root and went permanently red on any workspace carrying a
// stale ignored copy of the tree; hard-coding directory names into a
// prune list does not generalise, so the walk is now only a fallback
// for non-git contexts (source tarballs) and additionally prunes any
// nested Go module.
//
// R237-ARCH-14 / #614 (post-deletion follow-up).
func TestLegacyServerNew_ZeroCrossPkgCallers(t *testing.T) {
	t.Parallel()
	root, err := repoRootFromHere()
	if err != nil {
		t.Skipf("repo root probe failed (%v); skipping cross-pkg scan", err)
	}

	const callerNeedle = "server.New("
	const defNeedlePrefix = "\nfunc New(addr string"

	// Allow-list: files that legitimately reference these tokens in
	// commentary, godoc, or guard-test needles. Keep this list minimal —
	// an entry is only warranted while the file actually contains a
	// token; stale entries are dead weight that hides real regressions.
	allow := map[string]bool{
		"internal/server/server.go":                         true, // deletion-rationale godoc quotes the retired shape
		"internal/server/new_options_test.go":               true, // sibling guard: needle constants + error messages
		"internal/server/legacy_new_crosspkg_guard_test.go": true, // this file: needle constants + error messages
	}

	files, enumErr := gitListGoFiles(root)
	if enumErr != nil {
		// Not a git checkout or no git binary (e.g. source tarball,
		// hermetic build env). Fall back to a filesystem walk that
		// prunes nested modules and known non-source directories.
		files, enumErr = walkGoFiles(root)
	}
	if enumErr != nil {
		t.Fatalf("enumerate Go files under %s: %v", root, enumErr)
	}

	var callerOffenders, defOffenders []string
	for _, rel := range files {
		if allow[rel] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue // best-effort: a vanished file is not our concern
		}
		body := string(data)
		if strings.Contains(body, callerNeedle) {
			callerOffenders = append(callerOffenders, rel)
		}
		// Definition shape: only flag a top-of-line definition, never
		// inline references to the literal in commentary.
		if strings.Contains("\n"+body, defNeedlePrefix) {
			defOffenders = append(defOffenders, rel)
		}
	}
	if len(callerOffenders) > 0 {
		t.Errorf("`server.New(` callers re-introduced after R237-ARCH-14 (#614) deletion in: %v — use server.NewWithOptions(ServerOptions{...}) instead", callerOffenders)
	}
	if len(defOffenders) > 0 {
		t.Errorf("`func New(addr string ...)` definitions re-introduced outside server.go after R237-ARCH-14 (#614) in: %v — that shape was retired; do not resurrect it under a different package", defOffenders)
	}
}

// gitListGoFiles enumerates .go files the way the repository sees them:
// tracked files plus untracked-but-not-ignored ones (so a brand-new
// offender is caught before it is ever committed). Paths are returned
// slash-separated relative to root. Returns an error when git is
// unavailable or root is not inside a work tree — callers fall back to
// walkGoFiles.
func gitListGoFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard", "-z", "--", "*.go")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for f := range strings.SplitSeq(string(out), "\x00") {
		if f == "" {
			continue
		}
		files = append(files, filepath.ToSlash(f))
	}
	return files, nil
}

// walkGoFiles is the non-git fallback enumeration: it walks the tree,
// skipping vendored / non-source directories and — critically — any
// directory that carries its own go.mod. A nested go.mod marks an
// independent module checkout (stale tree copy, embedded fixture,
// worktree); its sources belong to that module, not this one, and
// scanning them produced the false offenders this test was once
// famous for. Paths are slash-separated relative to root.
func walkGoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".claude" || name == "node_modules" || name == "vendor" || name == "dist" || name == "build" {
				return filepath.SkipDir
			}
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// TestWalkGoFiles_PrunesNestedModules pins the fallback enumerator's
// hermeticity contract: a subdirectory carrying its own go.mod is an
// independent checkout and must be pruned wholesale, while regular
// subdirectories are scanned. Guards the guard — without this, the
// go.mod prune could silently regress and re-open the stale-copy
// false-offender hole whenever the primary git path is unavailable.
func TestWalkGoFiles_PrunesNestedModules(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module example.com/primary\n")
	mustWrite("pkg/keep.go", "package pkg\n")
	mustWrite("stale/go.mod", "module example.com/stale\n")
	mustWrite("stale/cmd/offender.go", "package main // server.New(\n")
	mustWrite("vendor/dep/dep.go", "package dep\n")

	files, err := walkGoFiles(root)
	if err != nil {
		t.Fatalf("walkGoFiles: %v", err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["pkg/keep.go"] {
		t.Errorf("expected pkg/keep.go in enumeration, got %v", files)
	}
	if got["stale/cmd/offender.go"] {
		t.Errorf("nested-module file stale/cmd/offender.go must be pruned, got %v", files)
	}
	if got["vendor/dep/dep.go"] {
		t.Errorf("vendor/ must be pruned, got %v", files)
	}
}

// repoRootFromHere walks up from the cwd until it finds a directory
// containing go.mod (the canonical worktree root marker for this
// project). Returns an error if it walks all the way to / without
// finding one — the caller treats that as "skip" rather than failing
// the build because the test runs from the package directory under
// `go test` and finds the root one or two parents up under normal
// layouts. R237-ARCH-14 / #614.
func repoRootFromHere() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
