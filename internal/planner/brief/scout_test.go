package brief

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/scout"
)

func (f *fixture) withScout(b *Briefer, cal []scout.Event, runs ...scout.Run) *Briefer {
	b.cfg.Calendar = func() []scout.Event { return cal }
	b.cfg.Patrol = func(since time.Time) []scout.Run {
		var out []scout.Run
		for _, r := range runs {
			if !r.Ended.Before(since) {
				out = append(out, r)
			}
		}
		return out
	}
	return b
}

func finding(source, kind, id, title, from, at, due string) scout.RunFinding {
	return scout.RunFinding{Finding: scout.Finding{Source: source, Kind: kind, ID: id, Title: title, From: from, At: at, Due: due}}
}

// The brief's scout sections: today's meetings not over, conflicts marked;
// who waits on a reply and the Asana tasks due today or overdue, from all
// the runs since the last brief (the overnight ones are in the morning's
// first run, not the newest); what the Planner held since the last brief.
func TestRenderScout(t *testing.T) {
	f := newFixture(t)
	f.now = at(2, 8, 30)
	f.briefer(nil, nil).Check() // Monday: nothing yet
	b := f.briefer(nil, nil)
	if err := b.Hold(Mention{Key: "k1", Title: "iso 周报 [草稿]", Why: "周五前要交", Link: "https://example.com/a"}); err != nil {
		t.Fatal(err)
	}
	f.now = at(3, 8, 31)
	meeting := func(id string, start, end time.Time) scout.Event {
		return scout.Event{ID: id, Title: "iso " + id, Start: start, End: end}
	}
	cal := []scout.Event{
		meeting("早会", at(3, 7, 0), at(3, 8, 0)), // over
		meeting("全天培训", at(3, 0, 0), at(4, 0, 0)),
		meeting("评审", at(3, 10, 0), at(3, 11, 0)),
		meeting("一对一", at(3, 10, 30), at(3, 11, 0)),
		meeting("午餐", at(3, 12, 0), at(3, 13, 0)),
		meeting("明天的会", at(4, 9, 0), at(4, 10, 0)),
	}
	runs := []scout.Run{
		{ID: "r0", State: scout.StateOK, Started: at(2, 16, 0), Ended: at(2, 16, 5), Findings: []scout.RunFinding{
			finding("slack", "reply_needed", "D1/1", "iso 问", "Bob", at(2, 12, 0).Format(time.RFC3339), ""),
		}},
		{ID: "r1", State: scout.StateOK, Started: at(3, 8, 0), Ended: at(3, 8, 5), Findings: []scout.RunFinding{
			finding("slack", "reply_needed", "D1/1", "iso 问进度", "Bob", at(2, 20, 0).Format(time.RFC3339), ""),
			finding("slack", "reply_needed", "D1/1", "iso 问进度", "Bob", at(2, 20, 0).Format(time.RFC3339), ""),
			finding("email", "reply_needed", "m1", "iso 预算确认", "", at(3, 6, 0).Format(time.RFC3339), ""),
			finding("asana", "deadline", "t1", "iso 今天交", "", "", "2026-11-03"),
			finding("asana", "deadline", "t2", "iso 上周的", "", "", at(1, 17, 0).Format(time.RFC3339)),
			finding("asana", "deadline", "t3", "iso 明天的", "", "", "2026-11-04"),
			finding("email", "deadline", "m2", "iso 邮件里的截止", "", "", "2026-11-03"),
			finding("email", "fyi", "m3", "iso 知道就好", "", "", ""),
		}},
		// The newest run: nothing new since 08:00.
		{ID: "r2", State: scout.StateOK, Started: at(3, 8, 15), Ended: at(3, 8, 20)},
	}
	b = f.withScout(f.briefer(nil, nil), cal, runs...)
	b.Check()
	want := "## 早报 · 11月3日 周二\n\n" +
		"自 11-02 08:30 以来：今天日程 4 · 等你回复 2 · Asana 到期 2 · Planner 记下 1\n" +
		"\n### 今天的日程（4）\n" +
		"- 全天 · iso 全天培训\n" +
		"- 10:00–11:00 · iso 评审 · 冲突\n" +
		"- 10:30–11:00 · iso 一对一 · 冲突\n" +
		"- 12:00–13:00 · iso 午餐\n" +
		"\n### 在等你回复的（2）\n" +
		"- Bob · iso 问进度 · 已等 12 小时\n" +
		"- 某人 · iso 预算确认 · 已等 2 小时\n" +
		"\n### 今天到期或已过期的 Asana 任务（2）\n" +
		"- iso 上周的 · 已过期 2 天\n" +
		"- iso 今天交 · 今天到期\n" +
		"\n### Planner 记下的（1）\n" +
		"- [iso 周报 ［草稿］](https://example.com/a) · 周五前要交 · 11-02 08:30\n"
	got := b.Recent(1)
	if len(got) != 1 || got[0].Markdown != want {
		t.Fatalf("markdown:\n%v\nwant:\n%s", got, want)
	}
	if c := got[0].Counts; c != (Counts{Meetings: 4, Replies: 2, Deadlines: 2, Planner: 1}) {
		t.Errorf("counts = %+v", c)
	}
}

// Nothing from the scout to report sends nothing: the meetings over or
// tomorrow's, a run from before the last brief, a mention held before it.
func TestScoutNothingToReport(t *testing.T) {
	f := newFixture(t)
	f.now = at(2, 8, 30)
	b := f.briefer(nil, nil)
	f.now = at(2, 8, 29)
	if err := b.Hold(Mention{Key: "k1", Title: "iso 昨天记的"}); err != nil {
		t.Fatal(err)
	}
	f.now = at(2, 8, 30)
	b.Check() // Monday's brief takes the note
	if len(f.pushes) != 1 || b.Recent(1)[0].Counts != (Counts{Planner: 1}) {
		t.Fatalf("Monday: pushes %d, %+v", len(f.pushes), b.Recent(1))
	}
	f.pushes = nil

	f.now = at(3, 8, 30)
	cal := []scout.Event{
		{ID: "a", Title: "iso 早会", Start: at(3, 7, 0), End: at(3, 8, 0)},
		{ID: "b", Title: "iso 明天", Start: at(4, 9, 0), End: at(4, 10, 0)},
	}
	stale := scout.Run{ID: "r0", State: scout.StateOK, Started: at(2, 8, 0), Ended: at(2, 8, 5), Findings: []scout.RunFinding{
		finding("email", "reply_needed", "m1", "iso 旧的", "", "", ""),
	}}
	b = f.withScout(f.briefer(nil, nil), cal, stale)
	b.Check()
	if len(f.pushes) != 0 || len(b.Recent(10)) != 1 {
		t.Fatalf("Tuesday: pushes %v, recent %d; want none", f.pushes, len(b.Recent(10)))
	}
}
