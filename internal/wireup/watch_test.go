package wireup

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/session"
)

// A session the user turned observing off is a plain session in index.json:
// no purpose "chat", no digest path or report pointed at (its last digest is
// not kept current). A dispatched session keeps its own either way.
func TestWatchOffIndex(t *testing.T) {
	r := newDigestRouter(t)
	const chat, task = "dashboard:direct:u:general", "task:aaaaaaaaaaaa"
	r.InjectSession(chat, session.NewTestProcess())
	r.InjectSession(task, session.NewTestProcess())
	sc := sidecar.Open(t.TempDir(), nil)
	if err := sc.Put(sidecar.Meta{Key: task, Purpose: sidecar.PurposeTask}); err != nil {
		t.Fatal(err)
	}
	src := DigestSources{Router: r, Sidecars: sc}
	w := digest.New(digest.Config{Dir: t.TempDir(), Facts: src.Facts})
	for _, k := range []string{chat, task} {
		if err := w.SetReport(k, digest.Report{State: "done", Summary: "好了。", At: time.UnixMilli(1000)}); err != nil {
			t.Fatal(err)
		}
	}
	rows := func() map[string]*health.IndexEntry {
		h := HealthSources{Router: r, Digests: w, Sidecars: sc, Observed: src.Observed}
		out := map[string]*health.IndexEntry{}
		for _, f := range h.Collect(time.Time{}) {
			if f.Index != nil {
				out[f.Index.Key] = f.Index
			}
		}
		return out
	}
	if e := rows()[chat]; e == nil || e.Purpose != sidecar.PurposeChat || e.Report == nil {
		t.Fatalf("observed chat row = %+v", e)
	}
	src.Watched = func(string) bool { return false }
	got := rows()
	if e := got[chat]; e == nil || e.Purpose != "" || e.Report != nil || e.Digest != "" {
		t.Errorf("unobserved chat row = %+v, want a plain row", e)
	}
	if e := got[task]; e == nil || e.Purpose != sidecar.PurposeTask || e.Report == nil {
		t.Errorf("task row = %+v, want its report kept", e)
	}
}

// TaskLive covers the user's own sessions, dispatched or opened: the Board
// picks the watched ones; a cron run's, a daemon's and the Planner's are
// not the user's.
func TestTaskLiveUserSessions(t *testing.T) {
	r := newDigestRouter(t)
	keys := []string{"task:aaaaaaaaaaaa", "dashboard:direct:u:general", "feishu:direct:u:general", "cron:j1", "sys:gc", "planner:main", "scratch:x"}
	for _, k := range keys {
		r.InjectSession(k, session.NewTestProcess())
	}
	var got []string
	for k := range TaskLive(r)() {
		got = append(got, k)
	}
	slices.Sort(got)
	if want := []string{"dashboard:direct:u:general", "feishu:direct:u:general", "task:aaaaaaaaaaaa"}; !slices.Equal(got, want) {
		t.Errorf("live = %v, want %v", got, want)
	}
}

// Asking is an AskUserQuestion of the session's left unanswered: a turn
// ending so is 等你, not a result.
func TestCustodyAsking(t *testing.T) {
	r := newDigestRouter(t)
	asking, answered := session.NewTestProcess(), session.NewTestProcess()
	for _, ty := range []string{clievent.KindUser, clievent.KindAskQuestion, clievent.KindText, clievent.KindResult} {
		asking.EventLog.Append(clievent.EventEntry{Type: ty})
	}
	for _, ty := range []string{clievent.KindAskQuestion, clievent.KindResult, clievent.KindUser, clievent.KindText, clievent.KindResult} {
		answered.EventLog.Append(clievent.EventEntry{Type: ty})
	}
	r.InjectSession("dashboard:direct:a:general", asking)
	r.InjectSession("dashboard:direct:b:general", answered)
	c := CustodySources{Router: r}
	if !c.Asking("dashboard:direct:a:general") || c.Asking("dashboard:direct:b:general") || c.Asking("dashboard:direct:gone:general") {
		t.Error("Asking: want only the session left asking")
	}
}

// Keys asks custody's switch, which reads the router, only after its visit:
// asked inside it, a second read lock queued behind a writer waiting on the
// visit's would hang the router for good.
func TestDigestKeysOutsideVisit(t *testing.T) {
	r := newDigestRouter(t)
	const chat = "dashboard:direct:u:general"
	r.InjectSession(chat, session.NewTestProcess())
	var once sync.Once
	src := DigestSources{Router: r, Watched: func(key string) bool {
		once.Do(func() {
			// A writer arrives while Keys runs, as a spawn or a shim
			// reattaching does; give it time to queue on the lock.
			wrote := make(chan struct{})
			go func() {
				r.InjectSession("dashboard:direct:v:general", session.NewTestProcess())
				close(wrote)
			}()
			select {
			case <-wrote:
			case <-time.After(50 * time.Millisecond):
			}
		})
		return r.SessionFor(key) != nil // custody.Engine.Watched's Local
	}}
	done := make(chan []string, 1)
	go func() { done <- src.Keys() }()
	select {
	case keys := <-done:
		if !slices.Contains(keys, chat) {
			t.Errorf("keys = %v, want %s", keys, chat)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Keys hung: the switch was asked inside the router's visit")
	}
}
