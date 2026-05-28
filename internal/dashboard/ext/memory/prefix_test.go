package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryHandler_ResolvedPrefixCachedAtConstruction pins R242-SEC-7 (#635):
// the lexical-prefix gate inside tryRead must use the construction-time
// resolved-and-cached prefix, not a per-request rebuild from h.projectsDir.
// Both prefix forms (with and without trailing separator) must be exposed so
// the post-EvalSymlinks recheck can equality-match the projects root itself.
//
// We don't reach into private fields from external callers — the test lives
// in the same package and asserts the contract directly. The fix's payoff is
// that any future code that mutates projectsDir (or fails to set it) cannot
// silently drift the two gates apart.
func TestMemoryHandler_ResolvedPrefixCachedAtConstruction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// memoryTestHandler mirrors New's prefix derivation so the
	// fields populate the same way production code would.
	h := memoryTestHandler(t, dir, "")

	if h.resolvedPrefixNoSep == "" {
		t.Fatalf("resolvedPrefixNoSep empty; want canonicalised projects dir")
	}
	wantNoSep := strings.TrimRight(filepath.Clean(h.projectsDir), string(filepath.Separator))
	if h.resolvedPrefixNoSep != wantNoSep {
		t.Errorf("resolvedPrefixNoSep = %q, want %q", h.resolvedPrefixNoSep, wantNoSep)
	}
	wantWithSep := wantNoSep + string(filepath.Separator)
	if h.resolvedPrefix != wantWithSep {
		t.Errorf("resolvedPrefix = %q, want %q", h.resolvedPrefix, wantWithSep)
	}
	if !strings.HasSuffix(h.resolvedPrefix, string(filepath.Separator)) {
		t.Errorf("resolvedPrefix must end with separator; got %q", h.resolvedPrefix)
	}
}

// TestMemoryHandler_PrefixUnchangedByProjectsDirMutation locks the contract
// that a post-construction mutation of projectsDir cannot make the gates
// disagree. The handler captures the resolved prefix once; reading a slug
// after we deliberately repoint projectsDir to an unrelated string still
// uses the cached prefix and rejects the lexical check before any IO.
//
// Without the construction-time cache, the prefix would be rederived from
// the mutated projectsDir on every call, and a future code path that
// updated projectsDir without re-resolving would silently shift the
// trust boundary.
func TestMemoryHandler_PrefixUnchangedByProjectsDirMutation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMemoryFile(t, dir, "-cur", "slug_a", "body\n")
	h := memoryTestHandler(t, dir, "-cur")

	// Sanity: slug currently reachable.
	if _, resp, _ := callMemoryHandler(t, h, "slug_a"); !resp.Found {
		t.Fatalf("slug_a unreachable before mutation: %+v", resp)
	}

	// Mutate projectsDir to something nonsensical. The cached prefix
	// should still pin the lexical gate to the original root.
	origProjectsDir := h.projectsDir
	h.projectsDir = "/nonexistent/garbage/dir"
	t.Cleanup(func() { h.projectsDir = origProjectsDir })

	// The prefix is still anchored on the original resolved dir, so the
	// Join inside tryRead now lands at /nonexistent/... which fails the
	// HasPrefix gate against the cached resolvedPrefix → errMemoryPathEscape.
	got, err := h.tryRead("-cur", "slug_a")
	if err == nil {
		t.Fatalf("expected errMemoryPathEscape after projectsDir mutation; got hit=%+v", got)
	}
	if got != nil {
		t.Errorf("expected nil hit on path-escape; got %+v", got)
	}
}
