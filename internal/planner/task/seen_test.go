package task

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
)

// A result is new while it is later than seen_at: the latest report with a
// conclusion (done, blocked; not progress nor need_user), or the session
// turning done or failed — not when the user stopped or marked it.
func TestResultNew(t *testing.T) {
	rep := func(state string, d time.Duration) *sidecar.Report {
		return &sidecar.Report{State: state, Summary: "s", At: t0.Add(d)}
	}
	mark := func(s State, by By, why string, d time.Duration) *sidecar.Mark {
		return &sidecar.Mark{State: string(s), By: string(by), Why: why, At: t0.Add(d)}
	}
	h := time.Hour
	cases := []struct {
		name    string
		report  *sidecar.Report
		mark    *sidecar.Mark
		retired time.Time
		health  health.State
		seen    time.Duration // seen_at = t0+seen; 0 is never seen
		result  time.Duration // want result_at = t0+result; -1 for none
		new     bool
	}{
		{"running, nothing yet", nil, nil, time.Time{}, health.Active, 0, -1, false},
		{"done report, never seen", rep("done", h), nil, time.Time{}, health.Idle, 0, h, true},
		{"done report seen after", rep("done", h), nil, time.Time{}, health.Idle, 2 * h, h, false},
		{"done report seen before", rep("done", h), nil, time.Time{}, health.Idle, h / 2, h, true},
		{"seen at the very time", rep("done", h), nil, time.Time{}, health.Idle, h, h, false},
		{"blocked report", rep("blocked", h), nil, time.Time{}, health.Idle, 0, h, true},
		{"progress report only updates the card", rep("progress", h), nil, time.Time{}, health.Active, 0, -1, false},
		{"need_user report is 等你's", rep("need_user", h), nil, time.Time{}, health.Waiting, 0, -1, false},
		{"done by the Planner", nil, mark(Done, ByPlanner, "PR 已合并", h), time.Time{}, health.Idle, 0, h, true},
		{"done by the Planner after a seen report", rep("done", h), mark(Done, ByPlanner, "", 2*h), time.Time{}, health.Idle, h + h/2, 2 * h, true},
		{"failed by health", nil, nil, time.Time{}, health.Failed, 0, 0, true},
		{"failed, seen since", nil, nil, time.Time{}, health.Failed, h, 0, false},
		{"ended by itself", nil, nil, t0.Add(3 * h), health.Unknown, 0, 3 * h, true},
		{"stopped by the user", nil, mark(Done, ByUser, StoppedWhy, h), time.Time{}, health.Idle, 0, -1, false},
		{"marked done by the user", nil, mark(Done, ByUser, "", h), time.Time{}, health.Idle, 0, -1, false},
		{"user's mark keeps an earlier report", rep("done", h), mark(Done, ByUser, "", 2*h), time.Time{}, health.Idle, 0, h, true},
		{"reopened: the report stays the result", rep("done", h), mark(Doing, ByUser, "", 2*h), time.Time{}, health.Idle, 3 * h, h, false},
		{"waiting is no result", nil, nil, time.Time{}, health.Waiting, 0, -1, false},
	}
	for _, c := range cases {
		m := sidecar.Meta{Created: t0.Add(-h), Report: c.report, Mark: c.mark, Retired: c.retired}
		if c.seen != 0 {
			m.SeenAt = t0.Add(c.seen)
		}
		st, by, _, at := sessionState(m, health.Unit{State: c.health, Since: t0})
		got := resultTime(m, st, by, at)
		var want time.Time
		if c.result >= 0 {
			want = t0.Add(c.result)
		}
		if !got.Equal(want) {
			t.Errorf("%s: result_at = %v, want %v", c.name, got, want)
		}
		if n := isNew(got, m.SeenAt); n != c.new {
			t.Errorf("%s: new = %v, want %v", c.name, n, c.new)
		}
	}
}

// seenBoard is a board over two dispatched sessions with a done report
// (one already seen), one running, a promise and a dashboard session.
func seenBoard(t *testing.T, now *time.Time) (*Board, *sidecar.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	clock := func() time.Time { return *now }
	sc := sidecar.Open(dir, clock)
	for _, m := range []sidecar.Meta{
		{Key: "task:aaaaaaaaaaaa", Purpose: sidecar.PurposeTask, Title: "a", Created: t0.Add(-2 * time.Hour),
			Report: &sidecar.Report{State: "done", Summary: "做完了。", At: t0.Add(-time.Hour)}},
		{Key: "task:bbbbbbbbbbbb", Purpose: sidecar.PurposeTask, Title: "b", Created: t0.Add(-2 * time.Hour),
			Report: &sidecar.Report{State: "blocked", Summary: "卡住了。", At: t0.Add(-30 * time.Minute)}},
		{Key: "task:cccccccccccc", Purpose: sidecar.PurposeTask, Title: "c", Created: t0.Add(-2 * time.Hour),
			Report: &sidecar.Report{State: "done", Summary: "早看过。", At: t0.Add(-time.Hour)}, SeenAt: t0.Add(-time.Minute)},
		{Key: "task:dddddddddddd", Purpose: sidecar.PurposeTask, Title: "d", Created: t0.Add(-time.Hour)},
		{Key: "dashboard:direct:x:general", Purpose: "other"},
	} {
		if err := sc.Put(m); err != nil {
			t.Fatal(err)
		}
	}
	proms := promise.New(promise.Config{Now: clock})
	p, err := proms.Add(promise.Promise{Text: "提醒用户看结果"})
	if err != nil {
		t.Fatal(err)
	}
	b := New(Config{Sessions: sc, Promises: proms, Now: clock, Health: func() health.Snapshot {
		return health.Snapshot{Units: []health.Unit{{ID: "task:dddddddddddd", State: health.Active, Since: t0}}}
	}})
	return b, sc, p.ID, dir
}

func newIDs(b *Board) []string {
	var ids []string
	for _, tk := range b.List() {
		if tk.New {
			ids = append(ids, tk.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// Seen marks the IDs given (skipping what is no dispatched session or has
// nothing new), or with all every new one listed; again it marks nothing.
func TestSeen(t *testing.T) {
	now := t0
	b, sc, prom, _ := seenBoard(t, &now)
	if got := newIDs(b); !slices.Equal(got, []string{"task:aaaaaaaaaaaa", "task:bbbbbbbbbbbb"}) {
		t.Fatalf("new = %v", got)
	}
	for _, tk := range b.List() {
		if tk.ID == "task:aaaaaaaaaaaa" && !tk.ResultAt.Equal(t0.Add(-time.Hour)) {
			t.Errorf("result_at = %v", tk.ResultAt)
		}
	}
	n, err := b.Seen([]string{"task:aaaaaaaaaaaa", "task:cccccccccccc", "task:dddddddddddd", prom, "task:none", "dashboard:direct:x:general"}, false)
	if err != nil || n != 1 {
		t.Fatalf("Seen(ids) = %d, %v; want 1 marked", n, err)
	}
	if m, _ := sc.Get("task:aaaaaaaaaaaa"); !m.SeenAt.Equal(now) {
		t.Errorf("seen_at = %v, want %v", m.SeenAt, now)
	}
	if m, _ := sc.Get("task:cccccccccccc"); !m.SeenAt.Equal(t0.Add(-time.Minute)) {
		t.Errorf("seen one's seen_at moved to %v", m.SeenAt)
	}
	if _, ok := sc.Get("task:none"); ok {
		t.Error("an unknown ID got a sidecar")
	}
	if got := newIDs(b); !slices.Equal(got, []string{"task:bbbbbbbbbbbb"}) {
		t.Fatalf("new after Seen(ids) = %v", got)
	}

	now = t0.Add(time.Minute)
	if n, err := b.Seen(nil, true); err != nil || n != 1 {
		t.Fatalf("Seen(all) = %d, %v; want 1", n, err)
	}
	if got := newIDs(b); len(got) != 0 {
		t.Fatalf("new after Seen(all) = %v", got)
	}
	if n, err := b.Seen(nil, true); err != nil || n != 0 {
		t.Fatalf("Seen(all) again = %d, %v; want 0", n, err)
	}

	// A later result is new again.
	now = t0.Add(time.Hour)
	if _, err := sc.Update("task:aaaaaaaaaaaa", func(m *sidecar.Meta) bool {
		m.Report = &sidecar.Report{State: "done", Summary: "又做完一件。", At: now}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if got := newIDs(b); !slices.Equal(got, []string{"task:aaaaaaaaaaaa"}) {
		t.Fatalf("new after a later report = %v", got)
	}
}

// Opening a session (a viewing report) marks its new result read, once:
// the tab's renewals every 30 seconds write nothing more; a session with
// nothing new, a viewing that ends and a session with no sidecar are left
// alone.
func TestSetViewing(t *testing.T) {
	now := t0
	b, sc, _, dir := seenBoard(t, &now)
	const key = "task:aaaaaaaaaaaa"
	b.SetViewing(key, "ws1", false)
	if m, _ := sc.Get(key); !m.SeenAt.IsZero() {
		t.Fatal("viewing=false marked it")
	}
	b.SetViewing(key, "ws1", true)
	if m, _ := sc.Get(key); !m.SeenAt.Equal(t0) {
		t.Fatalf("seen_at = %v after viewing", m.SeenAt)
	}
	// The renewals: the file is not rewritten (a write would bring back
	// what is changed under it here).
	p := sidecarPath(dir, key)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		now = t0.Add(time.Duration(i) * 30 * time.Second)
		b.SetViewing(key, "ws1", true)
	}
	if got, _ := os.ReadFile(p); string(got) != "{}" {
		t.Errorf("a renewal rewrote the sidecar: %s", got)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if m, _ := sc.Get(key); !m.SeenAt.Equal(t0) {
		t.Fatalf("a renewal moved seen_at to %v", m.SeenAt)
	}

	for _, k := range []string{"task:cccccccccccc", "task:dddddddddddd", "dashboard:direct:x:general", "feishu:direct:u:general"} {
		before, had := sc.Get(k)
		b.SetViewing(k, "ws1", true)
		after, has := sc.Get(k)
		if had != has || !after.SeenAt.Equal(before.SeenAt) {
			t.Errorf("%s: viewing changed seen_at %v -> %v (sidecar %v -> %v)", k, before.SeenAt, after.SeenAt, had, has)
		}
	}
	b.DropViewer("ws1")
}

// A sidecar a newer naozhi wrote takes no seen_at, so its result is not
// new here: no dot that never goes out, no error from 全部已读 or from
// opening it.
func TestSeenNewerSidecar(t *testing.T) {
	now := t0
	dir := t.TempDir()
	const key = "task:eeeeeeeeeeee"
	data := []byte(`{"v":99,"key":"` + key + `","purpose":"task","title":"e","created":"2026-11-04T08:00:00Z",` +
		`"report":{"state":"done","summary":"新版写的。","at":"2026-11-04T09:00:00Z"}}`)
	p := sidecarPath(dir, key)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sc := sidecar.Open(dir, func() time.Time { return now })
	b := New(Config{Sessions: sc, Now: func() time.Time { return now }})
	list := b.List()
	if len(list) != 1 || list[0].ID != key || list[0].ResultAt.IsZero() {
		t.Fatalf("list = %+v", list)
	}
	if list[0].New {
		t.Error("a newer sidecar's result is new")
	}
	if n, err := b.Seen([]string{key}, false); n != 0 || err != nil {
		t.Errorf("Seen(ids) = %d, %v; want 0, nil", n, err)
	}
	if n, err := b.Seen(nil, true); n != 0 || err != nil {
		t.Errorf("Seen(all) = %d, %v; want 0, nil", n, err)
	}
	b.SetViewing(key, "ws1", true)
	if got, err := os.ReadFile(p); err != nil || string(got) != string(data) {
		t.Errorf("newer sidecar changed: %q, %v", got, err)
	}
}

// sidecarPath is key's file in store directory dir (sidecar's naming).
func sidecarPath(dir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// Upgrading to a naozhi that keeps seen_at: the sidecars written before it
// (no seen_at) get the first start's time as their baseline, so a result
// they had is no new one, one after it is, and looking still marks read.
// A later start does not baseline a sidecar written since.
func TestSeenBaselineOnUpgrade(t *testing.T) {
	h := time.Hour
	dir := t.TempDir()
	now := t0
	clock := func() time.Time { return now }
	// Written by v0.7.0 or earlier: done before the upgrade, no seen_at.
	old := []string{"task:aaaaaaaaaaaa", "task:bbbbbbbbbbbb"}
	pre := sidecar.Open(dir, clock)
	for _, k := range old {
		if err := pre.Put(sidecar.Meta{Key: k, Purpose: sidecar.PurposeTask, Title: k, Created: t0.Add(-3 * h),
			Report: &sidecar.Report{State: "done", Summary: "升级前做完的。", At: t0.Add(-2 * h)}}); err != nil {
			t.Fatal(err)
		}
	}
	// Still running at the upgrade.
	if err := pre.Put(sidecar.Meta{Key: "task:cccccccccccc", Purpose: sidecar.PurposeTask, Title: "c", Created: t0.Add(-h)}); err != nil {
		t.Fatal(err)
	}
	if got := newIDs(New(Config{Sessions: pre, Now: clock})); len(got) != 3-1 {
		t.Fatalf("before the baseline new = %v, want the two done ones", got)
	}

	// The first start of the new build.
	sc := sidecar.Open(dir, clock)
	if n, err := sc.BaselineSeen(); n != 3 || err != nil {
		t.Fatalf("BaselineSeen = %d, %v; want 3, nil", n, err)
	}
	b := New(Config{Sessions: sc, Now: clock})
	if got := newIDs(b); len(got) != 0 {
		t.Fatalf("after the baseline new = %v, want none", got)
	}

	// After the upgrade: c finishes, and a new task d finishes too.
	now = t0.Add(h)
	if _, err := sc.Update("task:cccccccccccc", func(m *sidecar.Meta) bool {
		m.Report = &sidecar.Report{State: "done", Summary: "升级后做完的。", At: now}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := sc.Put(sidecar.Meta{Key: "task:dddddddddddd", Purpose: sidecar.PurposeTask, Title: "d", Created: now,
		Report: &sidecar.Report{State: "blocked", Summary: "卡住了。", At: now.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(2 * h)
	if got := newIDs(b); !slices.Equal(got, []string{"task:cccccccccccc", "task:dddddddddddd"}) {
		t.Fatalf("after the upgrade new = %v", got)
	}

	// A restart does not baseline d, which has no seen_at yet.
	sc2 := sidecar.Open(dir, clock)
	if n, err := sc2.BaselineSeen(); n != 0 || err != nil {
		t.Fatalf("second BaselineSeen = %d, %v; want 0, nil", n, err)
	}
	b2 := New(Config{Sessions: sc2, Now: clock})
	if got := newIDs(b2); !slices.Equal(got, []string{"task:cccccccccccc", "task:dddddddddddd"}) {
		t.Fatalf("after a restart new = %v", got)
	}

	// Looking still marks read.
	if n, err := b2.Seen([]string{"task:cccccccccccc"}, false); n != 1 || err != nil {
		t.Fatalf("Seen = %d, %v", n, err)
	}
	b2.SetViewing("task:dddddddddddd", "ws1", true)
	if got := newIDs(b2); len(got) != 0 {
		t.Fatalf("after looking new = %v", got)
	}
}
