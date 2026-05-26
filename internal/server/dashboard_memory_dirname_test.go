package server

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMemoryHandler_RejectsMalformedProjectDir pins R241-SEC-6 (#467):
// directory entries that don't look like Claude-encoded project dirs
// must be skipped during the external-project lookup pass. Without
// this filter, a stray entry whose name embedded `..` separators or
// control bytes could reach filepath.Join and depend on the lexical
// HasPrefix check alone to stay rooted.
//
// The test plants two siblings under projectsDir:
//   - `-good-project` with a real memory file (should be discoverable)
//   - `..stray` with a sibling memory file (must be skipped despite
//     containing the slug — the regex rejects the leading `..`).
//
// We deliberately use entry names disk-write would actually accept
// (no `/` in them, since that would split into a subdir on Linux).
// The point is that ent.Name() returns the literal byte-sequence of
// the on-disk dir name, and our filter must not pass `..stray` to
// tryRead.
func TestMemoryHandler_RejectsMalformedProjectDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Plant a legitimate project dir.
	good := filepath.Join(root, "-good-project", "memory")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatalf("mkdir good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(good, "shared-slug.md"),
		[]byte("# good\nbody-from-good-project\n"), 0o644); err != nil {
		t.Fatalf("write good: %v", err)
	}

	// Plant an entry whose name leads with `..` — semantically benign
	// (it's just a literal name in the dir listing), but the filter
	// must not let it through. Lexicographically `..stray` sorts before
	// `-good-project` (`..` < `-` is FALSE actually: `.` is 0x2E,
	// `-` is 0x2D, so `-` < `.`). Because the dashboard iterates in
	// sort order and returns on first hit, the legitimate entry will
	// be visited first — but the test still asserts the stray entry's
	// shared-slug.md is unreachable to lock the filter behaviour.
	stray := filepath.Join(root, "..stray", "memory")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir stray: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stray, "stray-only.md"),
		[]byte("# stray\nbody-from-stray\n"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	h := memoryTestHandler(t, root, "-no-current-project")

	// Slug present only under the stray entry must be unreachable.
	w, resp, _ := callMemoryHandler(t, h, "stray-only")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if resp.Found {
		t.Fatalf("stray entry should not be discoverable; got %+v", resp)
	}

	// Slug under the legitimate entry remains reachable.
	w2, resp2, _ := callMemoryHandler(t, h, "shared-slug")
	if w2.Code != 200 {
		t.Fatalf("good status = %d, want 200", w2.Code)
	}
	if !resp2.Found || resp2.Project != "-good-project" {
		t.Fatalf("good entry not found or wrong project: %+v", resp2)
	}
}

// TestTryRead_RejectsExplicitMalformedDir double-pins the in-function
// guard inside tryRead — even if a future caller routed user input
// through projectDir without going through the iteration filter, the
// regex check at the top of tryRead must still bail without touching
// disk.
//
// The guard has two response shapes: traversal-bearing inputs raise
// errMemoryPathEscape (matches the lexical prefix check downstream),
// while merely-malformed inputs return (nil, nil) so a benign non-
// Claude entry doesn't surface an error to the dashboard.
func TestTryRead_RejectsExplicitMalformedDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	h := memoryTestHandler(t, root, "")

	traversalCases := []string{
		"..",
		"../etc",
		"foo/bar",        // contains forward slash
		"foo\\backslash", // contains backslash
		"-..-injected",   // embedded `..` inside otherwise-valid name
	}
	for _, dir := range traversalCases {
		got, err := h.tryRead(dir, "any-slug")
		if err == nil {
			t.Fatalf("tryRead(%q): expected errMemoryPathEscape, got nil", dir)
		}
		if got != nil {
			t.Fatalf("tryRead(%q): expected nil hit on error, got %+v", dir, got)
		}
	}

	silentSkipCases := []string{
		"",                // empty
		"NoLeadingDash",   // missing the `-` prefix Claude's encoder uses
		"-name\x00null",   // embedded NUL byte
		"-bad\x01control", // embedded control byte
	}
	for _, dir := range silentSkipCases {
		got, err := h.tryRead(dir, "any-slug")
		if err != nil {
			t.Fatalf("tryRead(%q): unexpected error %v", dir, err)
		}
		if got != nil {
			t.Fatalf("tryRead(%q): expected nil hit, got %+v", dir, got)
		}
	}
}
