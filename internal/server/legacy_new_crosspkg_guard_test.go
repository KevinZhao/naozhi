package server

import (
	"io/fs"
	"os"
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
// R237-ARCH-14 / #614 (post-deletion follow-up). The walk roots at
// the worktree root located via repoRoot below so it works under
// both `go test` and `go test ./...`.
func TestLegacyServerNew_ZeroCrossPkgCallers(t *testing.T) {
	t.Parallel()
	root, err := repoRootFromHere()
	if err != nil {
		t.Skipf("repo root probe failed (%v); skipping cross-pkg scan", err)
	}

	const callerNeedle = "server.New("
	const defNeedlePrefix = "\nfunc New(addr string"

	// Allow-list: files that legitimately reference these tokens in
	// commentary or godoc. The legacy `server.New(` literal appears in
	// the deletion-rationale godoc inside server.go and in the
	// regression-guard error messages here / in new_options_test.go.
	allow := map[string]bool{
		"internal/server/server.go":                         true,
		"internal/server/new_options_test.go":               true,
		"internal/server/legacy_new_crosspkg_guard_test.go": true,
		"internal/server/dashboard_cron.go":                 true, // godoc references "server.New" by name
		"internal/server/project_files.go":                  true, // ditto
		"internal/server/health_unauth_ratelimit_test.go":   true, // ditto
		"internal/server/health.go":                         true, // ditto
		"internal/server/project_files_test.go":             true, // ditto
	}

	var callerOffenders, defOffenders []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// Skip vendored / non-source directories. The test exists in
			// internal/server so a relative-rooted scan never reaches
			// node_modules / .git / dist anyway, but explicit pruning
			// keeps the walk fast on large worktrees.
			name := d.Name()
			// Skip nested git worktrees under .claude/worktrees — they are
			// independent checkouts whose copy of this very test file (and any
			// other server.New call sites) would otherwise be scanned as if it
			// lived in the primary tree, producing false offenders.
			if name == ".git" || name == ".claude" || name == "node_modules" || name == "vendor" || name == "dist" || name == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		// Use forward slashes so the allow-list keys are platform-invariant.
		rel = filepath.ToSlash(rel)
		if allow[rel] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // best-effort: a vanished file is not our concern
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
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	if len(callerOffenders) > 0 {
		t.Errorf("`server.New(` callers re-introduced after R237-ARCH-14 (#614) deletion in: %v — use server.NewWithOptions(ServerOptions{...}) instead", callerOffenders)
	}
	if len(defOffenders) > 0 {
		t.Errorf("`func New(addr string ...)` definitions re-introduced outside server.go after R237-ARCH-14 (#614) in: %v — that shape was retired; do not resurrect it under a different package", defOffenders)
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
