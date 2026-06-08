package server

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestResolveExistingAncestor_MaxDepthBounded pins R20260607-SEC-11 (#1891):
// resolveExistingAncestor must NOT walk filepath.Dir + EvalSymlinks for an
// unbounded number of parents. An authenticated caller supplying a
// pathologically deep, non-existent JSONLPath (e.g. hundreds of nested
// components) could otherwise drive O(depth) syscalls per agent-transcript
// request. A path far deeper than the 64-level cap whose ancestors do not
// exist must return resolved=false (the same verdict as hitting the FS root)
// so jsonlPathUnderAllowedRoot rejects it rather than spinning per component.
func TestResolveExistingAncestor_MaxDepthBounded(t *testing.T) {
	t.Parallel()
	// Build an absolute path ~500 components deep under a non-existent root so
	// no ancestor ever EvalSymlinks-resolves. Pre-fix the loop walked every
	// one of the 500 parents; post-fix it stops after 64.
	parts := make([]string, 0, 512)
	for i := 0; i < 500; i++ {
		parts = append(parts, "x")
	}
	deep := string(filepath.Separator) + "nonexistent-root-20260607" +
		string(filepath.Separator) + strings.Join(parts, string(filepath.Separator))

	got, resolved := resolveExistingAncestor(deep)
	if resolved {
		t.Fatalf("deep non-existent path must report resolved=false, got resolved=true (%q)", got)
	}
	if got != filepath.Clean(deep) {
		t.Fatalf("unresolved path must be returned cleaned and unchanged; got %q", got)
	}
}

// TestResolveExistingAncestor_ShallowStillResolves guards that the depth cap
// does not break the legitimate "tail-before-write" case the function exists
// to serve: a real existing ancestor a few levels up must still resolve and
// re-join the unwritten leaf tail. R20260607-SEC-11 (#1891) must not regress
// R20260531070014-SEC-6 (#1533).
func TestResolveExistingAncestor_ShallowStillResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink/path semantics differ on windows")
	}
	t.Parallel()
	tmp := t.TempDir() // exists
	// Leaf does not exist yet, only a couple components below an existing dir.
	leaf := filepath.Join(tmp, "session-abc", "transcript.jsonl")
	got, resolved := resolveExistingAncestor(leaf)
	if !resolved {
		t.Fatalf("ancestor %q exists; resolveExistingAncestor must report resolved=true", tmp)
	}
	// The resolved tmp may differ (e.g. /var → /private/var) so compare suffix.
	if !strings.HasSuffix(got, filepath.Join("session-abc", "transcript.jsonl")) {
		t.Fatalf("resolved path lost the unwritten tail: %q", got)
	}
}
