package reap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
)

const key = "task:aaaaaaaaaaaa"

type fakeGit struct {
	mu      sync.Mutex
	st      map[string]Status
	pr      map[string]PR
	prErr   error
	removed []string
	added   []string
}

func (g *fakeGit) Linked(_ context.Context, dir string) (string, string, string, bool) {
	if filepath.Base(dir) == "main" {
		return "", "", "", false
	}
	return dir, "/repo/.git", "feat-" + filepath.Base(dir), true
}

func (g *fakeGit) Status(_ context.Context, top string) (Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st[top], nil
}

func (g *fakeGit) PR(_ context.Context, top, _ string) (PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pr[top], g.prErr
}

func (g *fakeGit) Remove(_ context.Context, _, top string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removed = append(g.removed, top)
	return os.Remove(top)
}

func (g *fakeGit) Add(_ context.Context, _, top, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.added = append(g.added, top)
	return os.Mkdir(top, 0o755)
}

type fakeInbox struct{ items []notify.Item }

func (f *fakeInbox) Add(it notify.Item) (notify.Item, bool) {
	f.items = append(f.items, it)
	return it, true
}

type fixture struct {
	r        *Reaper
	sc       *sidecar.Store
	git      *fakeGit
	inbox    *fakeInbox
	j        *journal.Journal
	now      time.Time
	sess     Session
	live     bool
	nearby   bool
	inUse    bool
	archived int
	restored []string
	dirs     []string
}

func newFixture(t *testing.T, dirs ...string) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 11, 6, 10, 0, 0, 0, time.Local), live: true, git: &fakeGit{st: map[string]Status{}, pr: map[string]PR{}}, inbox: &fakeInbox{}, dirs: dirs}
	clock := func() time.Time { return f.now }
	f.sc = sidecar.Open(t.TempDir(), clock)
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.j = j
	if err := f.sc.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Title: "修 bug", Parent: "planner:main"}); err != nil {
		t.Fatal(err)
	}
	f.r = New(Config{
		Sessions: f.sc,
		Session:  func(string) (Session, bool) { return f.sess, f.live },
		Nearby:   func(string) bool { return f.nearby },
		Archive: func(string) (Archived, bool) {
			if !f.live {
				return Archived{}, false
			}
			f.archived++
			f.live = false
			return Archived{SessionID: "sid-1", Workspace: "/x/main", LastPrompt: "修一下", Chain: []string{"sid-0"}}, true
		},
		Restore: func(k, sid, ws, lp string) error {
			f.restored = append(f.restored, k+" "+sid+" "+ws+" "+lp)
			f.live = true
			return nil
		},
		Dirs:    func(string, []string) []string { return f.dirs },
		InUse:   func(string, string) bool { return f.inUse },
		Git:     f.git,
		Inbox:   f.inbox,
		Journal: f.j,
		Now:     clock,
	})
	return f
}

func (f *fixture) markDone(t *testing.T) {
	t.Helper()
	if _, err := f.sc.SetMark(key, sidecar.Mark{State: string(task.Done), By: string(task.ByPlanner), Why: "PR 已开", At: f.now}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) reap() *sidecar.Reap {
	m, _ := f.sc.Get(key)
	return m.Reap
}

func (f *fixture) records(t *testing.T) []record {
	t.Helper()
	var out []record
	entries, err := f.j.Read(time.Time{}, f.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == journal.KindReap {
			var r record
			if err := json.Unmarshal(e.Data, &r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
	}
	return out
}

// Each check that fails holds the session: nothing is put away and the
// reason is said; once it clears the session is put away.
func TestHeldUntilEveryCheckPasses(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fixture)
		why  string
		// at is when it is expected to be put away: only while idleness
		// alone holds it.
		at time.Duration
	}{
		{"running", func(f *fixture) { f.sess.Running = true }, "这一轮还没结束", 0},
		{"failed", func(f *fixture) { f.sess.Failed = "API Error: 500" }, "这一轮出错结束", 0},
		{"asking", func(f *fixture) { f.sess.Asking = true }, "有提问没回答", 0},
		{"background", func(f *fixture) { f.sess.Background = true }, "后台还有任务在跑", 0},
		{"nearby", func(f *fixture) { f.nearby = true }, "你正看着它，或 5 分钟内给它发过消息", 0},
		{"idle under two hours", func(f *fixture) { f.sess.LastActive = f.now.Add(-time.Hour) }, "空闲不到 2 小时", time.Hour},
		{"active just now", func(f *fixture) { f.sess.LastActive = f.now }, "空闲不到 2 小时", IdleFor},
		// A more specific reason comes first and says no time.
		{"nearby and idle under two hours", func(f *fixture) {
			f.nearby, f.sess.LastActive = true, f.now.Add(-time.Minute)
		}, "你正看着它，或 5 分钟内给它发过消息", 0},
		{"running and idle under two hours", func(f *fixture) {
			f.sess.Running, f.sess.LastActive = true, f.now
		}, "这一轮还没结束", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.markDone(t)
			c.set(f)
			f.r.Tick(context.Background())
			if f.archived != 0 || f.reap() != nil {
				t.Fatal("put away while held")
			}
			why, at := f.r.Wait(key)
			if why != c.why {
				t.Errorf("Wait = %q, want %q", why, c.why)
			}
			if want := f.now.Add(c.at); c.at == 0 && !at.IsZero() || c.at != 0 && !at.Equal(want) {
				t.Errorf("Wait at = %v, want now+%v (zero unless only idleness holds it)", at, c.at)
			}
			f.sess, f.nearby = Session{}, false
			f.r.Tick(context.Background())
			if f.archived != 1 || f.reap() == nil {
				t.Fatal("not put away once the checks passed")
			}
			if why, at := f.r.Wait(key); why != "" || !at.IsZero() {
				t.Errorf("Wait after put away = %q %v", why, at)
			}
		})
	}
}

// Only a done mark puts a session away, and only one still listed in 任务.
func TestOnlyRecentDoneMarks(t *testing.T) {
	f := newFixture(t)
	f.r.Tick(context.Background())
	if f.archived != 0 {
		t.Fatal("a task with no mark put away")
	}
	if _, err := f.sc.SetMark(key, sidecar.Mark{State: string(task.Doing), By: "user", At: f.now}); err != nil {
		t.Fatal(err)
	}
	f.r.Tick(context.Background())
	if f.archived != 0 {
		t.Fatal("a task in progress put away")
	}
	f.markDone(t)
	f.now = f.now.Add(task.Archive)
	f.r.Tick(context.Background())
	if f.archived != 0 {
		t.Fatal("a task done past the list's archive put away")
	}
	// Activity before the mark does not stretch its window.
	f.sess.LastActive = f.now.Add(-task.Archive - time.Minute)
	f.r.Tick(context.Background())
	if why, _ := f.r.Wait(key); f.archived != 0 || why != "" {
		t.Fatalf("a task done and idle past the list's archive put away or held (%q)", why)
	}
}

// A done task talked to for most of a day after its mark is not left past
// its window, still listed but never put away: the window counts from its
// last activity, and once it idles IdleFor it is put away.
func TestWindowFromLastActivity(t *testing.T) {
	f := newFixture(t)
	f.markDone(t)
	f.sess.LastActive = f.now.Add(23 * time.Hour)
	f.now = f.now.Add(task.Archive)
	f.r.Tick(context.Background())
	why, at := f.r.Wait(key)
	if f.archived != 0 || why != "空闲不到 2 小时" || !at.Equal(f.sess.LastActive.Add(IdleFor)) {
		t.Fatalf("a day after the mark, active an hour ago: archived %d, Wait = %q %v", f.archived, why, at)
	}
	f.now = at
	f.r.Tick(context.Background())
	if f.archived != 1 || f.reap() == nil {
		t.Fatal("not put away once idle IdleFor, a day past the mark")
	}
}

// Putting away records what reopening needs and journals the steps; a
// session not in a worktree skips the worktree step. Reopening puts it back
// under its key, to resume its session ID.
func TestPutAwayAndReopen(t *testing.T) {
	f := newFixture(t)
	f.markDone(t)
	f.r.Tick(context.Background())
	rp := f.reap()
	if rp == nil || rp.SessionID != "sid-1" || rp.By != "planner" || rp.Why != "PR 已开" || len(rp.Worktrees) != 0 {
		t.Fatalf("Reap = %+v", rp)
	}
	recs := f.records(t)
	if len(recs) != 1 || recs[0].Event != "reaped" || recs[0].Trigger != "planner" || len(recs[0].Steps) != 1 ||
		len(recs[0].Skipped) != 1 || recs[0].Skipped[0].Why != "不在 worktree 里干活" {
		t.Fatalf("journal = %+v", recs)
	}
	f.r.Tick(context.Background())
	if f.archived != 1 {
		t.Fatal("put away twice")
	}

	f.r.OnState(key, task.Doing)
	if len(f.restored) != 1 || f.restored[0] != key+" sid-1 /x/main 修一下" {
		t.Fatalf("Restore = %v", f.restored)
	}
	if f.reap() != nil {
		t.Fatal("Reap kept after reopening")
	}
	if recs := f.records(t); len(recs) != 2 || recs[1].Event != "reopened" {
		t.Fatalf("journal = %+v", recs)
	}
	f.r.OnState(key, task.Doing)
	if len(f.restored) != 1 {
		t.Fatal("reopened twice")
	}
	// Done again: put away again.
	f.markDone(t)
	f.r.Tick(context.Background())
	if f.archived != 2 {
		t.Fatal("reopened task not put away when done again")
	}
}

func mkdirs(t *testing.T, names ...string) []string {
	t.Helper()
	root := t.TempDir()
	var out []string
	for _, n := range names {
		d := filepath.Join(root, n)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func wtState(f *fixture, dir string) sidecar.Worktree {
	for _, w := range f.reap().Worktrees {
		if w.Dir == dir {
			return w
		}
	}
	return sidecar.Worktree{}
}

// A worktree with work not committed or not pushed is never removed and the
// user is told once; one whose PR is open waits; a clean, pushed one whose
// PR merged or closed is removed, and checked out again on reopening.
func TestWorktrees(t *testing.T) {
	d := mkdirs(t, "dirty", "unpushed", "open", "merged", "closed")
	dirty, unpushed, open, merged, closed := d[0], d[1], d[2], d[3], d[4]
	f := newFixture(t, d...)
	f.git.st[dirty] = Status{Dirty: true, Head: "h", Pushed: true}
	f.git.st[unpushed] = Status{Head: "h2"}
	f.git.pr[unpushed] = PR{State: "MERGED", Head: "h1"}
	for _, dir := range []string{open, merged, closed} {
		f.git.st[dir] = Status{Head: "h", Pushed: true}
	}
	f.git.pr[dirty] = PR{State: "MERGED", Head: "h"}
	f.git.pr[open] = PR{State: "OPEN", Head: "h"}
	f.git.pr[merged] = PR{State: "MERGED", Head: "h"}
	f.git.pr[closed] = PR{State: "CLOSED", Head: "h"}
	f.markDone(t)
	f.r.Tick(context.Background())

	want := map[string]string{dirty: WTKept, unpushed: WTKept, open: WTPending, merged: WTRemoved, closed: WTRemoved}
	for dir, st := range want {
		if got := wtState(f, dir); got.State != st {
			t.Errorf("%s: state %q (%s), want %q", filepath.Base(dir), got.State, got.Why, st)
		}
	}
	for _, dir := range []string{dirty, unpushed, open} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s removed", filepath.Base(dir))
		}
	}
	if len(f.inbox.items) != 2 {
		t.Fatalf("told %d times, want 2 (dirty, unpushed): %+v", len(f.inbox.items), f.inbox.items)
	}
	recs := f.records(t)
	// The session put away and two worktrees removed; three left in place.
	if len(recs) != 1 || len(recs[0].Steps) != 3 || len(recs[0].Skipped) != 3 {
		t.Fatalf("journal = %+v", recs)
	}

	// Not looked at again before recheckAfter; then the open PR merged and
	// the user's commit pushed: removed, and the user told nothing more.
	f.git.mu.Lock()
	f.git.pr[open] = PR{State: "MERGED", Head: "h"}
	f.git.st[unpushed] = Status{Head: "h2", Pushed: true}
	f.git.mu.Unlock()
	f.r.Tick(context.Background())
	if wtState(f, open).State != WTPending {
		t.Fatal("looked at again before recheckAfter")
	}
	f.now = f.now.Add(recheckAfter)
	f.r.Tick(context.Background())
	if wtState(f, open).State != WTRemoved || wtState(f, unpushed).State != WTRemoved {
		t.Fatalf("after recheck: open %+v, unpushed %+v", wtState(f, open), wtState(f, unpushed))
	}
	if wtState(f, dirty).State != WTKept || len(f.inbox.items) != 2 {
		t.Fatalf("dirty %+v, told %d", wtState(f, dirty), len(f.inbox.items))
	}

	f.r.OnState(key, task.Doing)
	if len(f.git.added) != 4 {
		t.Fatalf("checked out again: %v", f.git.added)
	}
	if _, err := os.Stat(merged); err != nil {
		t.Error("merged worktree not back")
	}
}

// A worktree another session works in stays; so does one whose checks do
// not answer and whose HEAD is not on the remote.
func TestWorktreeHeld(t *testing.T) {
	d := mkdirs(t, "inuse", "noanswer", "noanswer-pushed")
	f := newFixture(t, d...)
	f.inUse = true
	f.git.st[d[0]] = Status{Head: "h", Pushed: true}
	f.git.pr[d[0]] = PR{State: "MERGED", Head: "h"}
	f.markDone(t)
	f.git.prErr = errors.New("gh: not logged in")
	f.git.st[d[1]] = Status{Head: "h"}
	f.git.st[d[2]] = Status{Head: "h", Pushed: true}
	f.r.Tick(context.Background())
	if got := wtState(f, d[0]); got.State != WTPending {
		t.Errorf("in use: %+v", got)
	}
	if got := wtState(f, d[1]); got.State != WTKept {
		t.Errorf("no answer, not pushed: %+v", got)
	}
	if got := wtState(f, d[2]); got.State != WTPending {
		t.Errorf("no answer, pushed: %+v", got)
	}
	if len(f.git.removed) != 0 {
		t.Fatalf("removed %v", f.git.removed)
	}
}

// A done session no longer in the session list has nothing to put away.
func TestGoneSessionLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.live = false
	f.markDone(t)
	f.r.Tick(context.Background())
	if why, _ := f.r.Wait(key); f.archived != 0 || f.reap() != nil || why != "" {
		t.Fatal("a session not in the list handled")
	}
}
