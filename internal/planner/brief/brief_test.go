package brief

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// at is a local time in the week of Monday 2026-11-02.
func at(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, time.Local) }

type fixture struct {
	t      *testing.T
	j      *journal.Journal
	now    time.Time
	snap   health.Snapshot
	inbox  []notify.Item
	tasks  []task.Task
	pushes []notify.Push
	// routines and midway feed Config.Routines and Config.Midway; carried
	// collects Config.Carried's IDs.
	routines []Routine
	midway   time.Time
	carried  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	j, err := journal.Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return &fixture{t: t, j: j}
}

func (f *fixture) briefer(names map[string]string, reg *workspace.Registry) *Briefer {
	return New(Config{
		Now:        func() time.Time { return f.now },
		Journal:    f.j,
		Health:     func() health.Snapshot { return f.snap },
		Inbox:      func() []notify.Item { return f.inbox },
		Tasks:      func() []task.Task { return f.tasks },
		Push:       func(p notify.Push) bool { f.pushes = append(f.pushes, p); return true },
		Name:       func(u string) string { return names[u] },
		Workspaces: reg,
		Routines:   func(time.Time, time.Time) []Routine { return f.routines },
		Midway:     func() time.Time { return f.midway },
		Carried:    func(ids []string) { f.carried = append(f.carried, ids...) },
	})
}

func (f *fixture) health(ts time.Time, unit, ws string, from, to health.State, kind health.Kind, class health.ErrorClass) {
	f.t.Helper()
	data, _ := json.Marshal(map[string]any{"from": from, "to": to, "kind": kind, "error_class": class})
	if err := f.j.Append(journal.Entry{TS: ts, WS: ws, Kind: journal.KindHealth, Unit: unit, Data: data}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) output(ts time.Time, unit, ws string, a activity.Activity) {
	f.t.Helper()
	data, _ := json.Marshal(a)
	if err := f.j.Append(journal.Entry{TS: ts, WS: ws, Kind: journal.KindActivity, Unit: unit, Data: data}); err != nil {
		f.t.Fatal(err)
	}
}

func TestNothingToReportSendsNothing(t *testing.T) {
	f := newFixture(t)
	// Monday's brief went out, so Tuesday's window starts there.
	f.now = at(2, 8, 30)
	f.inbox = []notify.Item{{Kind: notify.KindQuestion, State: notify.StateOpen, Title: "a 在等你回复", Unit: "a", WS: "ws1", Created: at(2, 8, 0)}}
	f.briefer(nil, nil).Check()
	if len(f.pushes) != 1 {
		t.Fatalf("Monday: %d pushes, want 1", len(f.pushes))
	}
	f.pushes, f.inbox = nil, nil

	// Noise only: a routine cron run, a cron job's failure outside every
	// workspace, the Planner's own output, a session still running, and an
	// item the user closed.
	f.health(at(2, 23, 0), "cron:nightly", "ws1", health.Active, health.Idle, health.KindCron, "")
	f.health(at(2, 23, 5), "cron:orphan", "", health.Active, health.Failed, health.KindCron, health.ErrAuth)
	f.output(at(3, 1, 0), "planner:main", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "a.md"})
	f.output(at(3, 2, 0), "busy", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "b.md"})
	f.snap = health.Snapshot{At: at(3, 9, 0), Units: []health.Unit{{ID: "busy", Kind: health.KindSession, Workspace: "ws1", State: health.Active, LastActive: at(3, 8, 59)}}}
	f.inbox = []notify.Item{{Kind: notify.KindError, State: notify.StateClosed, Title: "old", Created: at(1, 9, 0)}}
	f.now = at(3, 9, 0)
	b := f.briefer(nil, nil)
	b.Check()
	if len(f.pushes) != 0 || len(b.Recent(10)) != 1 {
		t.Fatalf("Tuesday: pushes %v, recent %d; want none and only Monday's", f.pushes, len(b.Recent(10)))
	}

	// The empty judgment holds for the day, across a restart too.
	f.health(at(3, 9, 30), "s1", "ws1", health.Idle, health.Failed, health.KindSession, health.ErrRateLimit)
	f.now = at(3, 10, 0)
	b.Check()
	f.briefer(nil, nil).Check()
	if len(f.pushes) != 0 {
		t.Fatalf("Tuesday after the judgment: %d pushes, want 0", len(f.pushes))
	}

	// Wednesday starts from Tuesday's empty judgment: the 09:30 failure is
	// in, the noise before it is not.
	f.now = at(4, 8, 31)
	b = f.briefer(nil, nil)
	b.Check()
	got := b.Recent(1)
	if len(f.pushes) != 1 || len(got) != 1 || got[0].Counts != (Counts{Failed: 1}) || !got[0].Since.Equal(at(3, 9, 0)) {
		t.Fatalf("Wednesday: pushes %d, brief %+v; want one with one failure since Tuesday 09:00", len(f.pushes), got)
	}
}

func TestDue(t *testing.T) {
	cases := []struct {
		name    string
		now     time.Time
		lastDay string
		want    bool
	}{
		{"monday before 08:30", at(2, 8, 29), "", false},
		{"monday at 08:30", at(2, 8, 30), "", true},
		{"monday, already judged", at(2, 15, 0), "2026-11-02", false},
		{"tuesday, monday judged", at(3, 8, 30), "2026-11-02", true},
		{"woke late on friday", at(6, 23, 50), "2026-11-05", true},
		{"saturday", at(7, 9, 0), "2026-11-06", false},
		{"sunday", at(8, 9, 0), "2026-11-06", false},
	}
	for _, c := range cases {
		if got := due(c.now, c.lastDay); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}

	now := at(10, 8, 30)
	for _, c := range []struct {
		name       string
		last, want time.Time
	}{
		{"first brief", time.Time{}, now.Add(-firstLookback)},
		{"after a weekend", at(6, 8, 30), at(6, 8, 30)},
		{"after weeks off", at(2, 8, 30).AddDate(0, 0, -14), now.Add(-maxLookback)},
	} {
		if got := windowStart(now, c.last); !got.Equal(c.want) {
			t.Errorf("windowStart %s = %v, want %v", c.name, got, c.want)
		}
	}

	// Asleep through 08:30: the first check after waking sends today's
	// brief, once.
	f := newFixture(t)
	f.inbox = []notify.Item{{Kind: notify.KindQuestion, State: notify.StateOpen, Title: "q", Created: at(2, 7, 0)}}
	b := f.briefer(nil, nil)
	for _, now := range []time.Time{at(2, 8, 0), at(2, 11, 40), at(2, 11, 41), at(2, 18, 0)} {
		f.now = now
		b.Check()
	}
	if len(f.pushes) != 1 || b.Recent(1)[0].At != at(2, 11, 40) {
		t.Fatalf("pushes %d, brief at %v; want one, at 11:40", len(f.pushes), b.Recent(1))
	}
}

func TestRender(t *testing.T) {
	f := newFixture(t)
	reg := workspace.Load("", "node")
	dir := filepath.Join(t.TempDir(), "naozhi")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	naozhi, _ := reg.Ensure(dir)
	ws := naozhi.ID
	names := map[string]string{
		"dashboard:pj:abc:general":  "修复 [登录] *重试*",
		"cron:nightly":              "夜间构建",
		"feishu:direct:bob:general": "周报",
	}
	f.now = at(2, 8, 30)
	f.briefer(names, reg).Check() // Monday: nothing yet

	// What the Planner held for the brief: a thing held twice is listed
	// once, as last held.
	hb := f.briefer(names, reg)
	for _, h := range []struct {
		at time.Time
		m  Mention
	}{
		{at(2, 18, 0), Mention{Key: "k1", Title: "周报已发布", Why: "不急"}},
		{at(2, 19, 0), Mention{Key: "k2", Title: "[设计] 评审", Link: "https://example.com/d(1)"}},
		{at(2, 19, 30), Mention{Key: "k1", Title: "周报已发布", Why: "下午再看"}},
	} {
		f.now = h.at
		if err := hb.Hold(h.m); err != nil {
			t.Fatal(err)
		}
	}

	f.health(at(2, 20, 0), "dashboard:pj:abc:general", ws, health.Idle, health.Active, health.KindSession, "")
	f.output(at(2, 20, 30), "dashboard:pj:abc:general", ws, activity.Activity{Type: activity.TypeOutput, Path: "docs/a.md", Dir: "/w"})
	f.output(at(2, 20, 31), "dashboard:pj:abc:general", ws, activity.Activity{Type: activity.TypeOutput, Path: "docs/a.md", Dir: "/w"})
	f.output(at(2, 20, 40), "dashboard:pj:abc:general", ws, activity.Activity{Type: activity.TypePRCreate, URL: "https://github.com/o/r/pull/12"})
	f.health(at(2, 21, 0), "dashboard:pj:abc:general", ws, health.Active, health.Idle, health.KindSession, "")
	f.health(at(3, 2, 0), "cron:nightly", ws, health.Idle, health.Failed, health.KindCron, health.ErrCredentialExpired)
	f.health(at(3, 3, 0), "node2/dashboard:direct:x:general", ws, health.Active, health.Idle, health.KindRemote, "")
	f.snap = health.Snapshot{At: at(3, 9, 0), Units: []health.Unit{
		{ID: "dashboard:pj:abc:general", Kind: health.KindSession, Workspace: ws, State: health.Idle, LastActive: at(2, 21, 0)},
		{ID: "cron:nightly", Kind: health.KindCron, Workspace: ws, State: health.Idle, LastActive: at(3, 4, 0)},
		{ID: "feishu:direct:bob:general", Kind: health.KindSession, Workspace: ws, State: health.Waiting, LastActive: at(3, 6, 0)},
	}}
	f.inbox = []notify.Item{
		{Kind: notify.KindQuestion, State: notify.StateOpen, Unit: "feishu:direct:bob:general", WS: ws, Source: "waiting:feishu:direct:bob:general",
			Title: "周报 在等你回复", Why: "有提问等你回答超过 10 分钟", Created: at(3, 6, 10)},
		{Kind: notify.KindError, State: notify.StateOpen, Unit: "cron:nightly", WS: ws, Source: "failed:cron:nightly",
			Title: "夜间构建 失败", Why: "失败 · 凭证过期", Created: at(3, 2, 1)},
		{Kind: notify.KindError, State: notify.StateOpen, Source: "systemic:rate_limit",
			Title: "系统性故障：限流", Why: "2 个单元出现同一类错误：a、b", Created: at(1, 9, 0)},
		// A condition closes itself: the brief does not carry it.
		{Kind: notify.KindNotice, State: notify.StateOpen, Source: "disk", AutoClose: true,
			Title: "磁盘剩余不足", Why: "剩余 8%", Created: at(3, 7, 0)},
		{ID: "in_n1", Kind: notify.KindNotice, State: notify.StateOpen, Unit: "planner:main", Source: "planner:k3",
			Title: "Asana 有 2 条过期", Why: "Planner 提醒你", Created: at(2, 22, 0)},
		{Kind: notify.KindNotice, State: notify.StateOpen, Source: "promise:pr_1",
			Title: "到点提醒：CI 绿了告诉你", Created: at(3, 8, 0)},
	}
	f.tasks = []task.Task{
		{ID: "task:ab12", Kind: task.KindSession, Title: "iso 写 hello", State: task.Waiting, Created: at(2, 9, 0)},
		{ID: "pr_1", Kind: task.KindPromise, Title: "CI 绿了告诉你", State: task.Doing, Due: at(3, 8, 0), Created: at(1, 9, 0)},
		{ID: "pr_2", Kind: task.KindPromise, Title: "明早提醒你", State: task.Remind, Created: at(3, 8, 0)},
		{ID: "task:cd34", Kind: task.KindSession, Title: "iso 跑测试", State: task.Done, Created: at(2, 9, 0)},
		// Not news: an undated promise, one not due yet, a session at work
		// and one that failed and retired.
		{ID: "pr_3", Kind: task.KindPromise, Title: "盯着 CI", State: task.Doing, CloseWhen: "CI 绿", Created: at(1, 9, 0)},
		{ID: "pr_4", Kind: task.KindPromise, Title: "下午看看", State: task.Doing, Due: at(3, 15, 0), Created: at(1, 9, 0)},
		{ID: "task:ef56", Kind: task.KindSession, Title: "iso 在跑", State: task.Doing, Created: at(2, 9, 0)},
		{ID: "task:gh78", Kind: task.KindSession, Title: "iso 挂了", State: task.Failed, Retired: at(2, 10, 0), Created: at(1, 9, 0)},
	}
	f.now = at(3, 9, 0)
	b := f.briefer(names, reg)
	b.Check()

	// The cron job recovered: no longer 失败的, its failure's item still
	// open. 等你 is one line, counted as the badge (the question and both
	// errors); the Planner's notice is carried, the condition and the
	// promise's reminder are not.
	want := "自 11/2 08:30 以来：完成 2 · 通知 1 · 未完成的任务 2 · Planner 记下 2\n" +
		"\n等你 3 条（提问 1 · 错误 2），见「等你」\n" +
		"\n### 完成的（2）\n" +
		"- [node2/dashboard:direct:x:general](#node=node2&session=dashboard%3Adirect%3Ax%3Ageneral) · 工作区 naozhi · 11/3 03:00\n" +
		"- [修复 ［登录］ ＊重试＊](#session=dashboard%3Apj%3Aabc%3Ageneral) · 工作区 naozhi · 11/2 21:00 · 产出 1 个文件、[PR #12](https://github.com/o/r/pull/12)\n" +
		"\n### 通知（1）\n" +
		"- Asana 有 2 条过期 · Planner 提醒你 · 11/2 22:00\n" +
		"\n### 未完成的任务（2）\n" +
		"- [iso 写 hello](#session=task%3Aab12) · 派出的会话 · 等你 · 已 24 小时\n" +
		"- CI 绿了告诉你 · 承诺 · 进行中 · 到期 11/3 08:00 · 已 2 天\n" +
		"\n### Planner 记下的（2）\n" +
		"- [［设计］ 评审](https://example.com/d%281%29) · 11/2 19:00\n" +
		"- 周报已发布 · 下午再看 · 11/2 19:30\n"
	got := b.Recent(1)
	if len(got) != 1 {
		t.Fatalf("no brief")
	}
	if got[0].Markdown != want {
		t.Errorf("markdown:\n%s\nwant:\n%s", got[0].Markdown, want)
	}
	if len(f.pushes) != 1 || f.pushes[0].Title != "早报" || f.pushes[0].Body != "完成 2 · 等你 3 · 通知 1 · 未完成的任务 2 · Planner 记下 2" {
		t.Errorf("pushes = %+v", f.pushes)
	}
	if !slices.Equal(f.carried, []string{"in_n1"}) {
		t.Errorf("carried = %v, want the notice the brief listed", f.carried)
	}
}

// TestElsewhereSendsNothing: a node whose brief another node sends (several
// nodes, the scout not here) neither sends one nor holds a mention for one.
func TestElsewhereSendsNothing(t *testing.T) {
	f := newFixture(t)
	f.now = at(2, 8, 30)
	f.inbox = []notify.Item{{Kind: notify.KindQuestion, State: notify.StateOpen, Title: "a 在等你回复", Unit: "a", WS: "ws1", Created: at(2, 8, 0)}}
	b := f.briefer(nil, nil)
	b.cfg.Elsewhere = func() bool { return true }
	b.Check()
	if len(f.pushes) != 0 || len(b.Recent(10)) != 0 {
		t.Fatalf("pushes %v, recent %d; want none", f.pushes, len(b.Recent(10)))
	}
	if err := b.Hold(Mention{Title: "x"}); err != ErrElsewhere {
		t.Fatalf("Hold: %v, want ErrElsewhere", err)
	}
	b.cfg.Elsewhere = func() bool { return false }
	b.Check()
	if len(f.pushes) != 1 {
		t.Fatalf("on the scout's node: %d pushes, want 1", len(f.pushes))
	}
}
