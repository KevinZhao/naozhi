package digest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// rig is a Writer over a temp dir whose Facts return the reply set, on a
// clock the test moves.
type rig struct {
	w       *Writer
	dir     string
	now     time.Time
	mu      sync.Mutex
	reply   string
	seen    string
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
			return Facts{Title: "t", Replies: []Reply{{At: at(9, 0), Text: r.reply}}, GoalSeen: r.seen}, true
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
	if w.Path("k") != "" {
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
