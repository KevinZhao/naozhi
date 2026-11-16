package wireup

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/action"
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

// A done task's session waits on the user while its need_user report or a
// Planner ask naming it is open in "等你": the reaper keeps it.
func TestReapWaiting(t *testing.T) {
	inbox := notify.New(notify.Config{System: func(string, string) error { return nil }})
	waiting := ReapWaiting(inbox)
	const a, b = "task:aaaaaaaaaaaa", "task:bbbbbbbbbbbb"
	if waiting(a) || ReapWaiting(nil)(a) {
		t.Fatal("waiting with nothing open")
	}
	inbox.Add(notify.Item{Kind: notify.KindQuestion, Unit: a, Title: "要你决定", Source: action.ReportSource(a)})
	inbox.Add(notify.Item{Kind: notify.KindQuestion, Unit: "planner:main", Title: "拆不拆？", Task: b, Source: "ask:1"})
	if !waiting(a) || !waiting(b) {
		t.Fatalf("waiting = %v, %v; want both held", waiting(a), waiting(b))
	}
	if waiting("task:cccccccccccc") {
		t.Fatal("a session nothing names waits")
	}
	inbox.CloseSource("ask:1", notify.ClosedByUser)
	if waiting(b) {
		t.Fatal("a closed ask still holds the session")
	}
}
