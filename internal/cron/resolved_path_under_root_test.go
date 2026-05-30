package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolvedPathUnderRoot pins R20260527122801-ARCH-4 (#1316): the
// EvalSymlinks(allowedRoot)→equality-or-prefix tail extracted from
// workDirResolveUnderRoot into resolvedPathUnderRoot. The helper is the
// single named seam the cron↔server dedup will lift to a shared package, so
// its contract (equality match, child-prefix match, sibling-prefix reject,
// symlink-root fallback) is locked here.
func TestResolvedPathUnderRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// EvalSymlinks the root once so the comparison side matches what the
	// helper computes internally (macOS /var → /private/var etc.).
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks root: %v", err)
	}
	child := filepath.Join(rootResolved, "sub")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	// A sibling directory that shares the root as a string prefix without the
	// separator boundary ("rootX" vs "root") must be rejected.
	sibling := rootResolved + "X"
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}

	cases := []struct {
		name     string
		resolved string
		want     bool
		wantPath string
	}{
		{"root-equals", rootResolved, true, rootResolved},
		{"child-under-root", child, true, child},
		{"sibling-prefix-reject", sibling, false, ""},
		{"unrelated-reject", "/definitely/not/under", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolvedPathUnderRoot(tc.resolved, root, "")
			if ok != tc.want || got != tc.wantPath {
				t.Fatalf("resolvedPathUnderRoot(%q, %q) = (%q,%v) want (%q,%v)",
					tc.resolved, root, got, ok, tc.wantPath, tc.want)
			}
		})
	}
}

// TestResolvedPathUnderRoot_CachedRootFallback pins the R243-SEC-9 (#795)
// fallback: when allowedRoot cannot be EvalSymlinks'd live but a cached
// resolution is supplied, the helper uses the cached value rather than
// hard-rejecting or comparing against the raw (unresolved) root string.
func TestResolvedPathUnderRoot_CachedRootFallback(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks root: %v", err)
	}
	child := filepath.Join(rootResolved, "sub")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	// Point allowedRoot at a path that does not exist so the live
	// EvalSymlinks fails, forcing the cached-resolution branch.
	missingRoot := filepath.Join(root, "gone")

	// No cache → hard reject.
	if got, ok := resolvedPathUnderRoot(child, missingRoot, ""); ok || got != "" {
		t.Fatalf("missing root, no cache: got (%q,%v) want (\"\",false)", got, ok)
	}
	// Cached resolution supplied → admit the child under the cached root.
	if got, ok := resolvedPathUnderRoot(child, missingRoot, rootResolved); !ok || got != child {
		t.Fatalf("missing root, cached resolved: got (%q,%v) want (%q,true)", got, ok, child)
	}
}
