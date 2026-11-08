package digest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// rig is a Writer over a temp dir whose Facts return the reply set, on a
// clock the test moves.
type rig struct {
	w     *Writer
	dir   string
	now   time.Time
	mu    sync.Mutex
	reply string
	seen  string
	born  time.Time
	// replies, when set, are the replies in view instead of reply; none
	// says there are none in view.
	replies []Reply
	none    bool
	state   health.State
	ready   map[string]bool
	written []string
	live    map[string]bool
	retired map[string]time.Time
}

func newRig(t *testing.T) *rig {
	r := &rig{dir: t.TempDir(), now: at(10, 0), reply: "r1", live: map[string]bool{}, retired: map[string]time.Time{}}
	r.w = New(r.config())
	return r
}

func (r *rig) config() Config {
	return Config{
		Dir: r.dir,
		Now: func() time.Time { return r.now },
		Facts: func(key string) (Facts, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if strings.HasPrefix(key, "cron:") {
				return Facts{}, false
			}
			f := Facts{Title: "t", Replies: []Reply{{At: at(9, 0), Text: r.reply}}, GoalSeen: r.seen, Born: r.born, Health: r.state}
			switch {
			case r.none:
				f.Replies = nil
			case r.replies != nil:
				f.Replies = r.replies
			}
			return f, true
		},
		Ready: func(key string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			ready, ok := r.ready[key]
			return !ok || ready
		},
		Written: func(key string) {
			r.mu.Lock()
			r.written = append(r.written, key)
			r.mu.Unlock()
		},
		Live:    func(key string) bool { return r.live[key] },
		Retired: func(key string) time.Time { return r.retired[key] },
	}
}

func (r *rig) set(fn func(*rig)) {
	r.mu.Lock()
	fn(r)
	r.mu.Unlock()
}

func (r *rig) writes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.written)
}

func (r *rig) setReply(s string) {
	r.mu.Lock()
	r.reply = s
	r.mu.Unlock()
}

func (r *rig) read(t *testing.T, key string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, FileName(key)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestThrottle: Touches within MinGap of a write are written once, MinGap
// after it, with the latest content (the trailing write).
func TestThrottle(t *testing.T) {
	r := newRig(t)
	const key = "task:a"
	r.w.Touch(key)
	if next := r.w.step(r.now); !next.IsZero() || r.writes() != 1 {
		t.Fatalf("first touch: next=%v writes=%d, want written at once", next, r.writes())
	}
	for i, reply := range []string{"r2", "r3", "r4"} {
		r.now = r.now.Add(10 * time.Second)
		r.setReply(reply)
		r.w.Touch(key)
		next := r.w.step(r.now)
		if want := at(10, 1); !next.Equal(want) {
			t.Fatalf("touch %d: next = %v, want %v", i, next, want)
		}
	}
	if r.writes() != 1 {
		t.Fatalf("writes within MinGap = %d, want 1", r.writes())
	}
	r.now = at(10, 1)
	if next := r.w.step(r.now); !next.IsZero() {
		t.Fatalf("after the trailing write next = %v", next)
	}
	if r.writes() != 2 || !strings.Contains(r.read(t, key), "> r4") {
		t.Fatalf("trailing write: writes=%d content=%s", r.writes(), r.read(t, key))
	}
	// A Touch long after writes at once.
	r.now = at(10, 5)
	r.setReply("r5")
	r.w.Touch(key)
	r.w.step(r.now)
	if r.writes() != 3 {
		t.Fatalf("touch past MinGap: writes = %d, want 3", r.writes())
	}
}

// TestUnchangedNotWritten: a digest whose content is the same is not
// rewritten, nor is its updated time moved; nor after a restart.
func TestUnchangedNotWritten(t *testing.T) {
	r := newRig(t)
	const key = "dashboard:direct:u:general"
	r.w.Touch(key)
	r.w.step(r.now)
	first := r.read(t, key)
	r.now = r.now.Add(5 * time.Minute)
	r.w.Touch(key)
	r.w.step(r.now)
	if r.writes() != 1 || r.read(t, key) != first {
		t.Fatalf("unchanged content rewritten: writes=%d", r.writes())
	}
	r.w = New(r.config())
	r.w.Touch(key)
	r.w.step(r.now)
	if r.writes() != 1 {
		t.Fatalf("unchanged content rewritten after New: writes=%d", r.writes())
	}
}

// TestNotCovered: a key Facts declines gets no digest.
func TestNotCovered(t *testing.T) {
	r := newRig(t)
	r.w.Touch("cron:j1")
	r.w.step(r.now)
	if r.writes() != 0 || r.w.Path("cron:j1") != "" {
		t.Fatalf("cron key digested")
	}
}

// TestNewRebuildsPath: Path is "" until a digest exists, and a new Writer
// over the same directory finds the digests and states by their
// frontmatter.
func TestNewRebuildsPath(t *testing.T) {
	r := newRig(t)
	const key = "task:b"
	if p := r.w.Path(key); p != "" {
		t.Fatalf("Path before a write = %q", p)
	}
	// File names it before it is written, Path only after.
	if p := r.w.File(key); p != filepath.Join(r.dir, FileName(key)) {
		t.Fatalf("File = %q", p)
	}
	if err := r.w.SetGoal(key, "目标一\n\n细节"); err != nil {
		t.Fatal(err)
	}
	if err := r.w.SetReport(key, Report{State: "done", Summary: "好了。", Text: "全文", At: at(9, 30)}); err != nil {
		t.Fatal(err)
	}
	r.w.step(r.now)
	want := filepath.Join(r.dir, FileName(key))
	if p := r.w.Path(key); p != want {
		t.Fatalf("Path = %q, want %q", p, want)
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("digest mode = %v, %v", fi, err)
	}
	w2 := New(r.config())
	if p := w2.Path(key); p != want {
		t.Fatalf("rebuilt Path = %q, want %q", p, want)
	}
	st, ok := w2.State(key)
	if !ok || st.Goal != "目标一" || st.Report == nil || st.Report.Summary != "好了。" || st.Report.Text != "全文" {
		t.Fatalf("rebuilt State = %+v %v", st, ok)
	}
	updated, md, ok := w2.Read(key)
	if !ok || !updated.Equal(r.now) || !strings.Contains(string(md), "## 最新汇报（完成 · 2026-11-08 09:30）") || !strings.Contains(string(md), "> 目标一") {
		t.Fatalf("Read = %v %v\n%s", updated, ok, md)
	}
	// A file under another key's name is not taken for it.
	if err := os.WriteFile(filepath.Join(r.dir, FileName("task:other")), md, 0o600); err != nil {
		t.Fatal(err)
	}
	if p := New(r.config()).Path("task:other"); p != "" {
		t.Fatalf("misnamed digest taken: %q", p)
	}
}

// TestGoalKeptOnce: the goal first seen is kept; a later first message in
// view (the earlier one fell out of the ring) does not replace it.
func TestGoalKeptOnce(t *testing.T) {
	r := newRig(t)
	const key = "dashboard:direct:u:general"
	r.seen = "第一条消息"
	r.w.Touch(key)
	r.w.step(r.now)
	r.seen = "后来的消息"
	r.setReply("r2")
	r.now = r.now.Add(time.Hour)
	r.w.Touch(key)
	r.w.step(r.now)
	if st, _ := r.w.State(key); st.Goal != "第一条消息" {
		t.Fatalf("goal = %q", st.Goal)
	}
	if !strings.Contains(r.read(t, key), "> 第一条消息") {
		t.Fatalf("digest lacks the goal:\n%s", r.read(t, key))
	}
}

// TestSweep: a live session keeps its digest however old; a gone one loses
// it Retention after its sidecar saw it go, or after it was last written.
func TestSweep(t *testing.T) {
	r := newRig(t)
	for _, k := range []string{"task:live", "task:retired", "task:recent", "dashboard:direct:gone:general"} {
		r.w.Touch(k)
	}
	r.w.step(r.now)
	if err := r.w.SetGoal("task:stateonly", "g"); err != nil {
		t.Fatal(err)
	}
	r.live["task:live"] = true
	r.retired["task:retired"] = r.now.Add(time.Hour)
	r.retired["task:recent"] = r.now.Add(10 * 24 * time.Hour)
	r.now = r.now.Add(Retention + 2*time.Hour)
	recent := r.now.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(r.dir, stateName("task:stateonly")), recent, recent); err != nil {
		t.Fatal(err)
	}
	r.w.Sweep()
	for k, want := range map[string]bool{"task:live": true, "task:retired": false, "task:recent": true, "dashboard:direct:gone:general": false} {
		_, err := os.Stat(filepath.Join(r.dir, FileName(k)))
		if got := err == nil; got != want {
			t.Errorf("%s kept = %v, want %v", k, got, want)
		}
		if got := r.w.Path(k) != ""; got != want {
			t.Errorf("%s Path kept = %v, want %v", k, got, want)
		}
	}
	// The state file's own time dates a state with no digest: just saved.
	if _, ok := r.w.State("task:stateonly"); !ok {
		t.Errorf("fresh state swept")
	}
	if _, ok := r.w.State("task:retired"); ok {
		t.Errorf("retired state kept")
	}
}

// TestRun drives the real loop: a Touch is written, the startup keys are
// digested, and ctx ends it.
func TestRun(t *testing.T) {
	r := newRig(t)
	cfg := r.config()
	cfg.Now = time.Now
	cfg.MinGap = 20 * time.Millisecond
	cfg.Keys = func() []string { return []string{"task:boot"} }
	w := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	testhelper.Eventually(t, func() bool { return w.Path("task:boot") != "" }, 2*time.Second, "startup key digested")
	w.Touch("task:x")
	testhelper.Eventually(t, func() bool { return w.Path("task:x") != "" }, 2*time.Second, "touch written")
	r.setReply("again")
	w.Touch("task:x")
	w.Touch("task:x")
	testhelper.Eventually(t, func() bool {
		data, _ := os.ReadFile(filepath.Join(r.dir, FileName("task:x")))
		return strings.Contains(string(data), "> again")
	}, 2*time.Second, "trailing write")
	cancel()
	<-done
}

// TestNilWriter: the digests off, every method is a no-op.
func TestNilWriter(t *testing.T) {
	var w *Writer
	w.Touch("k")
	w.Sweep()
	w.Run(context.Background())
	if w.Path("k") != "" || w.File("k") != "" {
		t.Fatal("nil Path")
	}
	if _, ok := w.State("k"); ok {
		t.Fatal("nil State")
	}
	if err := w.SetGoal("k", "g"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetReport("k", Report{}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := w.Read("k"); ok {
		t.Fatal("nil Read")
	}
}

// TestNotReadyRetried: a key whose sources are not all in is not written
// but tried again RetryGap later, and written once ready.
func TestNotReadyRetried(t *testing.T) {
	r := newRig(t)
	const key = "task:wait"
	r.set(func(r *rig) { r.ready = map[string]bool{key: false} })
	r.w.Touch(key)
	if next := r.w.step(r.now); !next.Equal(r.now.Add(RetryGap)) || r.writes() != 0 {
		t.Fatalf("not ready: next=%v writes=%d, want a retry at %v and no write", next, r.writes(), r.now.Add(RetryGap))
	}
	r.now = r.now.Add(RetryGap)
	if next := r.w.step(r.now); next.IsZero() || r.writes() != 0 {
		t.Fatalf("still not ready: next=%v writes=%d", next, r.writes())
	}
	r.set(func(r *rig) { r.ready[key] = true })
	r.now = r.now.Add(RetryGap)
	if next := r.w.step(r.now); !next.IsZero() || r.writes() != 1 {
		t.Fatalf("ready: next=%v writes=%d, want written", next, r.writes())
	}
}

// TestRestartKeepsDigest: after a restart, while the health table has not
// judged the session and its history is not loaded (not ready), the digest
// on disk is left alone; once ready with the replies still out of view, the
// kept replies stay in it.
func TestRestartKeepsDigest(t *testing.T) {
	r := newRig(t)
	const key = "task:done"
	r.set(func(r *rig) {
		r.state = health.Idle
		r.replies = []Reply{{At: at(9, 0), Text: "回复一"}, {At: at(9, 10), Text: "结论在这"}}
	})
	r.w.Touch(key)
	r.w.step(r.now)
	before := r.read(t, key)
	if !strings.Contains(before, "> 结论在这") || !strings.Contains(before, "健康：空闲") {
		t.Fatalf("precondition: digest = %s", before)
	}

	// Restart: nothing judged, no events in view.
	r.set(func(r *rig) {
		r.state, r.replies, r.none = "", nil, true
		r.ready = map[string]bool{key: false}
	})
	r.w = New(r.config())
	r.now = r.now.Add(time.Hour)
	r.w.Touch(key)
	r.w.step(r.now)
	if got := r.read(t, key); got != before {
		t.Fatalf("digest rewritten before its sources were in:\n%s", got)
	}
	// Ready, the health judged, the history still without those turns.
	r.set(func(r *rig) { r.state, r.ready[key] = health.Idle, true })
	r.now = r.now.Add(RetryGap)
	r.w.step(r.now)
	if got := r.read(t, key); got != before {
		t.Fatalf("digest changed with nothing new:\n%s", got)
	}
}

// TestRepliesKept: a view with no finished turn (one turn longer than the
// events in view) keeps the replies; a view with fewer than MaxReplies
// fills up with the older kept ones.
func TestRepliesKept(t *testing.T) {
	r := newRig(t)
	const key = "task:long"
	r.set(func(r *rig) {
		r.replies = []Reply{{At: at(9, 0), Text: "一"}, {At: at(9, 1), Text: "二"}, {At: at(9, 2), Text: "三"}}
	})
	r.w.Touch(key)
	r.w.step(r.now)
	r.set(func(r *rig) { r.none, r.state = true, health.Stalled })
	r.now = r.now.Add(time.Hour)
	r.w.Touch(key)
	r.w.step(r.now)
	got := r.read(t, key)
	if !strings.Contains(got, "健康：停滞") || !strings.Contains(got, "## 最近回复（3）") || !strings.Contains(got, "> 三") {
		t.Fatalf("replies blanked by a view with none:\n%s", got)
	}
	r.set(func(r *rig) { r.none, r.replies = false, []Reply{{At: at(11, 0), Text: "四"}} })
	r.now = r.now.Add(time.Hour)
	r.w.Touch(key)
	r.w.step(r.now)
	st, _ := r.w.State(key)
	var texts []string
	for _, rp := range st.Replies {
		texts = append(texts, rp.Text)
	}
	if strings.Join(texts, ",") != "二,三,四" {
		t.Fatalf("replies = %q, want the latest three of kept and seen", texts)
	}
}

// TestNewConversation: a session created anew under the same key (/new, a
// delete and a new start) drops the old goal and replies; a task keeps its
// dispatch's goal.
func TestNewConversation(t *testing.T) {
	r := newRig(t)
	const chat, task = "dashboard:direct:u:general", "task:again"
	if err := r.w.SetGoal(task, "派活的目标"); err != nil {
		t.Fatal(err)
	}
	r.set(func(r *rig) { r.seen, r.born, r.reply = "旧话题", at(8, 0), "旧回复" })
	r.w.Touch(chat)
	r.w.Touch(task)
	r.w.step(r.now)
	r.set(func(r *rig) {
		r.seen, r.born, r.replies = "新话题", at(10, 30), []Reply{{At: at(10, 40), Text: "新回复"}}
	})
	r.now = r.now.Add(time.Hour)
	r.w.Touch(chat)
	r.w.Touch(task)
	r.w.step(r.now)
	got := r.read(t, chat)
	if !strings.Contains(got, "> 新话题") || strings.Contains(got, "旧") {
		t.Fatalf("chat after /new:\n%s", got)
	}
	if st, _ := r.w.State(chat); !st.Born.Equal(at(10, 30)) || len(st.Replies) != 1 {
		t.Fatalf("chat state = %+v", st)
	}
	got = r.read(t, task)
	if !strings.Contains(got, "> 派活的目标") || strings.Contains(got, "旧回复") {
		t.Fatalf("task after a reset:\n%s", got)
	}
	// The same Born again, as after a restart, keeps everything.
	r.w = New(r.config())
	if st, _ := r.w.State(chat); st.Goal != "新话题" || !st.Born.Equal(at(10, 30)) {
		t.Fatalf("state after a restart = %+v", st)
	}
}

// TestCorruptState: a state file that does not parse is moved aside and the
// session starts a new one; one that cannot be read as its own (misnamed)
// is never saved over.
func TestCorruptState(t *testing.T) {
	dir := t.TempDir()
	const key, other = "task:bad", "task:other"
	bad := filepath.Join(dir, stateName(key))
	if err := os.WriteFile(bad, []byte(`{"v":1,"key":"task:bad","goal":"半截`), 0o600); err != nil {
		t.Fatal(err)
	}
	misnamed := filepath.Join(dir, stateName(other))
	alien := []byte(`{"v":1,"key":"task:someone-else","goal":"别人的"}`)
	if err := os.WriteFile(misnamed, alien, 0o600); err != nil {
		t.Fatal(err)
	}
	w := New(Config{Dir: dir})
	aside, _ := filepath.Glob(bad + ".corrupt.*")
	if len(aside) != 1 {
		t.Fatalf("corrupt state not moved aside: %v", aside)
	}
	if data, _ := os.ReadFile(aside[0]); !strings.Contains(string(data), "半截") {
		t.Fatalf("moved-aside copy = %s", data)
	}
	if err := w.SetGoal(key, "新目标"); err != nil {
		t.Fatal(err)
	}
	if st, ok := New(Config{Dir: dir}).State(key); !ok || st.Goal != "新目标" {
		t.Fatalf("state after the corrupt one = %+v %v", st, ok)
	}
	if err := w.SetGoal(other, "盖掉"); err == nil {
		t.Fatal("a state file left unread was saved over")
	}
	if data, _ := os.ReadFile(misnamed); string(data) != string(alien) {
		t.Fatalf("misnamed state changed: %s", data)
	}
}

// TestTouchDuringSave: Touch, Path and State do not wait on a state save in
// progress, so a turn's end on a read loop never waits on the disk.
func TestTouchDuringSave(t *testing.T) {
	r := newRig(t)
	r.w.saveMu.Lock()
	done := make(chan struct{})
	go func() {
		r.w.Touch("task:x")
		r.w.Path("task:x")
		r.w.State("task:x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Touch waited on a save")
	}
	r.w.saveMu.Unlock()
}
