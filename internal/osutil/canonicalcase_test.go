package osutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCanonicalCase_PassthroughShapes pins the no-op contract for inputs the
// canonicalizer must never touch: empty, relative, and root paths.
func TestCanonicalCase_PassthroughShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"relative", "rel/path", "rel/path"},
		{"relative dot", "./x", "./x"},
		{"root", "/", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalCase(tc.in); got != tc.want {
				t.Errorf("CanonicalCase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalCase_FixesComponentCase creates <tmp>/workspace/proj and
// queries it as .../Workspace/Proj — every component must come back with its
// on-disk spelling. Behaviour is identical on case-sensitive and
// case-insensitive filesystems: ReadDir lists "workspace", the folded match
// rewrites "Workspace" → "workspace" either way.
func TestCanonicalCase_FixesComponentCase(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "workspace", "proj")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}

	query := filepath.Join(tmp, "Workspace", "Proj")
	got := CanonicalCase(query)
	if got != real {
		t.Errorf("CanonicalCase(%q) = %q, want %q", query, got, real)
	}

	// Exact-case input round-trips unchanged.
	if got := CanonicalCase(real); got != real {
		t.Errorf("CanonicalCase(%q) = %q, want unchanged", real, got)
	}
}

// TestCanonicalCase_MissingTailPreserved: components after the first missing
// one keep the caller's spelling — the target may legitimately not exist yet.
func TestCanonicalCase_MissingTailPreserved(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}

	query := filepath.Join(tmp, "Workspace", "NoSuchDir", "Child")
	want := filepath.Join(tmp, "workspace", "NoSuchDir", "Child")
	if got := CanonicalCase(query); got != want {
		t.Errorf("CanonicalCase(%q) = %q, want %q (existing prefix fixed, missing tail verbatim)",
			query, got, want)
	}
}

// TestCanonicalCase_ExactMatchWinsOverFold: on a case-sensitive filesystem
// where sibling entries differ only in case, the exact-case entry must win so
// the input spelling is preserved. Skipped when the filesystem is
// case-insensitive (creating the second entry collides with the first).
func TestCanonicalCase_ExactMatchWinsOverFold(t *testing.T) {
	tmp := t.TempDir()
	lower := filepath.Join(tmp, "aa")
	upper := filepath.Join(tmp, "AA")
	if err := os.Mkdir(lower, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(upper, 0o700); err != nil {
		// EEXIST → case-insensitive filesystem; the dual-entry scenario
		// cannot exist here.
		t.Skipf("case-insensitive filesystem (mkdir %q after %q: %v); dual-case siblings impossible", upper, lower, err)
	}

	if got := CanonicalCase(upper); got != upper {
		t.Errorf("CanonicalCase(%q) = %q, want exact-case entry preserved", upper, got)
	}
	if got := CanonicalCase(lower); got != lower {
		t.Errorf("CanonicalCase(%q) = %q, want exact-case entry preserved", lower, got)
	}
}

// TestCanonicalCase_SymlinkNameNotResolved: a symlink component's NAME is
// case-canonicalized but its target is not substituted — CanonicalCase is
// orthogonal to EvalSymlinks.
func TestCanonicalCase_SymlinkNameNotResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on windows")
	}
	tmp := t.TempDir()
	target := filepath.Join(tmp, "realdir")
	if err := os.MkdirAll(filepath.Join(target, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "linkdir")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	query := filepath.Join(tmp, "Linkdir", "Inner")
	want := filepath.Join(tmp, "linkdir", "inner")
	got := CanonicalCase(query)
	if got != want {
		t.Errorf("CanonicalCase(%q) = %q, want %q (link name kept, child resolved through link)",
			query, got, want)
	}
	if strings.Contains(got, "realdir") {
		t.Errorf("CanonicalCase resolved the symlink target: %q", got)
	}
}

// TestCanonicalCase_TrailingSlashCleaned documents that the result is
// filepath.Clean'd (trailing separators dropped) — callers downstream compare
// cleaned paths.
func TestCanonicalCase_TrailingSlashCleaned(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "ws")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := CanonicalCase(sub + string(filepath.Separator)); got != sub {
		t.Errorf("CanonicalCase(%q) = %q, want %q", sub+"/", got, sub)
	}
}
