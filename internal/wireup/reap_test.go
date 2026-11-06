package wireup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The cwds a transcript ran in, each once in the order first seen; lines
// without one, relative ones and garbage are skipped.
func TestTranscriptCwds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","cwd":"/r"}
{"type":"assistant","cwd":"/r/.claude/worktrees/x"}
not json "cwd":
{"type":"summary"}
{"cwd":"rel"}
{"type":"user","cwd":"/r"}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := transcriptCwds(p); !slices.Equal(got, []string{"/r", "/r/.claude/worktrees/x"}) {
		t.Fatalf("transcriptCwds = %v", got)
	}
	if got := transcriptCwds(filepath.Join(t.TempDir(), "none")); got != nil {
		t.Fatalf("missing transcript = %v", got)
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
	} {
		if got := within(c.path, c.dir); got != c.want {
			t.Errorf("within(%q, %q) = %v", c.path, c.dir, got)
		}
	}
}
