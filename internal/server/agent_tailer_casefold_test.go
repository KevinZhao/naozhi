package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJsonlPathUnderAllowedRoot_CaseInsensitiveFS: on a case-insensitive
// filesystem (macOS default) a transcript path whose projects-root component
// is spelled in another case is still under the root, while that spelling of
// the root itself and a look-alike sibling are not. agentevents'
// jsonlPathUnderAllowedRoot is pinned by the same table, so WS and HTTP
// drill-in reach the same verdict.
func TestJsonlPathUnderAllowedRoot_CaseInsensitiveFS(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "projects")
	jsonl := filepath.Join(root, "-ws", "agent-a1.jsonl")
	if err := os.MkdirAll(filepath.Dir(jsonl), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonl, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(base, strings.ToUpper("projects"))
	if _, err := os.Stat(upper); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	if err := os.Mkdir(filepath.Join(base, "PROJECTSX"), 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(upper, "-ws", "agent-a1.jsonl"), true},
		{filepath.Join(upper, "-ws", "agent-not-yet-written.jsonl"), true},
		{upper, false},
		{filepath.Join(base, "PROJECTSX", "agent-a1.jsonl"), false},
	}
	for _, c := range cases {
		if got := jsonlPathUnderAllowedRoot(c.path, root); got != c.want {
			t.Errorf("jsonlPathUnderAllowedRoot(%q, %q) = %v, want %v", c.path, root, got, c.want)
		}
	}
}
