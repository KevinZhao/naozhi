package wireup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// Session carries the session's last activity, which planner/reap's idle
// check counts from; a key not in the router is not found.
func TestReapSessionLastActive(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	before := time.Now()
	r.InjectSession("task:aaaaaaaaaaaa", session.NewTestProcess())
	src := ReapSources{Router: r}
	s, ok := src.Session("task:aaaaaaaaaaaa")
	if !ok || s.LastActive.Before(before) || s.LastActive.After(time.Now()) {
		t.Fatalf("Session = %+v, %v; want LastActive at the injection", s, ok)
	}
	if _, ok := src.Session("task:bbbbbbbbbbbb"); ok {
		t.Fatal("a key not in the router found")
	}
}

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
