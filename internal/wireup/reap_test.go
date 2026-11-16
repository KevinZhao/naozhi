package wireup

import (
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
	if la, ok := src.LastActive("task:aaaaaaaaaaaa"); !ok || !la.Equal(s.LastActive) {
		t.Fatalf("LastActive = %v, %v; want Session's %v", la, ok, s.LastActive)
	}
	if _, ok := src.Session("task:bbbbbbbbbbbb"); ok {
		t.Fatal("a key not in the router found")
	}
	if _, ok := src.LastActive("task:bbbbbbbbbbbb"); ok {
		t.Fatal("LastActive found a key not in the router")
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
