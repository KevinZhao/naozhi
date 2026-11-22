package brief

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/front"
)

// tidied is a briefer with the ledger and the morning tidy wired: fronts is
// what the ledger holds, started Tidy's answer, running the tidy turn's
// state; asked collects the texts Tidy got.
type tidied struct {
	fronts  []front.Front
	started bool
	running bool
	asked   []string
}

func (f *fixture) tidyBriefer(td *tidied) *Briefer {
	b := f.briefer(nil, nil)
	b.cfg.Fronts = func() []front.Front { return td.fronts }
	b.cfg.Tidy = func(text string) bool { td.asked = append(td.asked, text); return td.started }
	b.cfg.Tidying = func() bool { return td.running }
	return b
}

// TestMorningTidy (§15.18, D133): a due brief first sends the Planner the
// sessions that moved, by key, and waits for that turn; the brief written
// once it is over leads with the lines of work as the ledger then has them.
// The tidy is asked for once a day.
func TestMorningTidy(t *testing.T) {
	f := newFixture(t)
	f.output(at(2, 21, 0), "dashboard:direct:a:general", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "plan.md"})
	f.output(at(2, 22, 0), "task:ab12", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "out.md"})
	f.now = at(3, 8, 30)
	td := &tidied{started: true, running: true, fronts: []front.Front{
		{ID: "jd-tgw", Name: "JD TGW 迁移", State: "方案被否", Next: "改路线图", Waiting: "等客户", Updated: at(2, 18, 0)},
	}}
	b := f.tidyBriefer(td)

	b.Check()
	if len(td.asked) != 1 || len(f.pushes) != 0 {
		t.Fatalf("first check: asked %d, pushes %d", len(td.asked), len(f.pushes))
	}
	for _, want := range []string{
		`<naozhi-morning day="2026-11-03">`,
		"- task:ab12 · 工作区 ws1 · 派出的 · 最后输出 11-02 22:00\n",
		"- dashboard:direct:a:general · 工作区 ws1 · 你开的 · 最后输出 11-02 21:00\n",
		"用 front 块更新那条线",
		"</naozhi-morning>",
	} {
		if !strings.Contains(td.asked[0], want) {
			t.Errorf("tidy text lacks %q:\n%s", want, td.asked[0])
		}
	}

	// The turn is not yet counted as in flight: still inside the grace.
	td.running = false
	f.now = f.now.Add(tidyGrace - time.Second)
	b.Check()
	td.running = true
	f.now = at(3, 8, 32)
	b.Check()
	if len(f.pushes) != 0 || len(td.asked) != 1 {
		t.Fatalf("while the tidy runs: pushes %d, asked %d", len(f.pushes), len(td.asked))
	}

	// The Planner moved the line on and found a new one; its turn is over.
	td.running = false
	td.fronts = []front.Front{
		{ID: "jd-tgw", Name: "JD TGW 迁移", State: "路线图已改", Next: "发给客户", Updated: at(3, 8, 32)},
		{ID: "blog", Name: "trn2 blog", State: "草稿完成", Updated: at(1, 8, 0).AddDate(0, 0, -3)},
	}
	f.now = at(3, 8, 33)
	b.Check()
	got := b.Recent(1)
	if len(f.pushes) != 1 || len(got) != 1 {
		t.Fatalf("after the tidy: pushes %d, briefs %d", len(f.pushes), len(got))
	}
	want := "自 11/2 08:33 以来：工作线 2 · 完成 1 · 你开的会话有新输出 1\n" +
		"\n### 工作线（2）\n" +
		"- **JD TGW 迁移**：路线图已改 → 发给客户\n" +
		"- **trn2 blog**：草稿完成 · 5 天没动\n" +
		"\n### 完成的（1）\n"
	if !strings.HasPrefix(got[0].Markdown, want) {
		t.Errorf("markdown:\n%s\nwant it to start:\n%s", got[0].Markdown, want)
	}
	if got[0].Counts.Fronts != 2 || got[0].Counts.Yours != 1 || got[0].Counts.Done != 1 {
		t.Errorf("counts = %+v", got[0].Counts)
	}
	b.Check()
	if len(td.asked) != 1 || len(f.pushes) != 1 {
		t.Errorf("after the brief: asked %d, pushes %d", len(td.asked), len(f.pushes))
	}
}

// The brief does not hang on the Planner: one that cannot take the tidy, a
// tidy that outlasts its wait, and a morning with no session to look at all
// brief as before.
func TestMorningTidyNoWait(t *testing.T) {
	t.Run("the Planner is off", func(t *testing.T) {
		f := newFixture(t)
		f.output(at(2, 21, 0), "dashboard:direct:a:general", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "plan.md"})
		f.now = at(3, 8, 30)
		td := &tidied{started: false}
		f.tidyBriefer(td).Check()
		if len(td.asked) != 1 || len(f.pushes) != 1 {
			t.Fatalf("asked %d, pushes %d", len(td.asked), len(f.pushes))
		}
	})
	t.Run("the tidy outlasts its wait", func(t *testing.T) {
		f := newFixture(t)
		f.output(at(2, 21, 0), "dashboard:direct:a:general", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "plan.md"})
		f.now = at(3, 8, 30)
		td := &tidied{started: true, running: true}
		b := f.tidyBriefer(td)
		b.Check()
		f.now = f.now.Add(tidyWait - time.Second)
		b.Check()
		if len(f.pushes) != 0 {
			t.Fatal("briefed inside the wait")
		}
		f.now = f.now.Add(time.Second)
		b.Check()
		if len(f.pushes) != 1 || len(td.asked) != 1 {
			t.Fatalf("pushes %d, asked %d", len(f.pushes), len(td.asked))
		}
	})
	t.Run("nothing moved", func(t *testing.T) {
		f := newFixture(t)
		f.now = at(3, 8, 30)
		f.midway = at(3, 8, 45) // something to brief that is no session
		td := &tidied{started: true, running: true, fronts: []front.Front{{ID: "a", Name: "甲", Updated: at(2, 9, 0)}}}
		f.tidyBriefer(td).Check()
		if len(td.asked) != 0 || len(f.pushes) != 1 {
			t.Fatalf("asked %d, pushes %d", len(td.asked), len(f.pushes))
		}
	})
	t.Run("the lines of work alone send nothing", func(t *testing.T) {
		f := newFixture(t)
		f.now = at(3, 8, 30)
		td := &tidied{fronts: []front.Front{{ID: "a", Name: "甲", Updated: at(2, 9, 0)}}}
		b := f.tidyBriefer(td)
		b.Check()
		if len(f.pushes) != 0 || len(b.Recent(1)) != 0 {
			t.Fatalf("pushes %d, briefs %d", len(f.pushes), len(b.Recent(1)))
		}
	})
}

// A session that opened many PRs is one line that counts them; a few are
// linked as before.
func TestDonePRsCounted(t *testing.T) {
	f := newFixture(t)
	for i := range maxPRLinks + 2 {
		f.output(at(2, 21, i), "dashboard:direct:many:general", "ws1",
			activity.Activity{Type: activity.TypePR, URL: fmt.Sprintf("https://github.com/o/r/pull/%d", 100+i)})
	}
	f.output(at(2, 22, 0), "dashboard:direct:few:general", "ws1", activity.Activity{Type: activity.TypePR, URL: "https://github.com/o/r/pull/7"})
	f.now = at(3, 8, 30)
	b := f.briefer(nil, nil)
	b.Check()
	got := b.Recent(1)
	if len(got) != 1 {
		t.Fatal("no brief")
	}
	md := got[0].Markdown
	if !strings.Contains(md, fmt.Sprintf("· 产出 %d 个 PR\n", maxPRLinks+2)) || strings.Contains(md, "pull/100") {
		t.Errorf("many PRs not counted:\n%s", md)
	}
	if !strings.Contains(md, "· 产出 [PR #7](https://github.com/o/r/pull/7)\n") {
		t.Errorf("a few PRs not linked:\n%s", md)
	}
}

// The brief lists the first three open items of a line under it and counts
// the rest (§15.23, D143); a line with none reads as before, and the items
// under no line are left to the Planner page.
func TestFrontItems(t *testing.T) {
	f := newFixture(t)
	f.output(at(2, 22, 0), "task:ab12", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "out.md"})
	f.now = at(3, 8, 30)
	td := &tidied{fronts: []front.Front{
		{ID: "gateway-refactor", Name: "网关重构", State: "迁了一半", Updated: at(2, 18, 0)},
		{ID: "docs", Name: "文档", Updated: at(2, 17, 0)},
	}}
	b := f.tidyBriefer(td)
	var items []front.Item
	for i := 1; i <= 5; i++ {
		items = append(items, front.Item{ID: fmt.Sprintf("i000000%d", i), Text: fmt.Sprintf("第 %d 件 *没做完* 的事", i), Front: "gateway-refactor"})
	}
	items = append(items, front.Item{ID: "i0000009", Text: "没归线的", Workspace: "ws1"})
	b.cfg.Items = func() []front.Item { return items }
	b.Check()
	got := b.Recent(1)
	if len(got) != 1 {
		t.Fatalf("briefs = %d", len(got))
	}
	want := "\n### 工作线（2）\n" +
		"- **网关重构**：迁了一半\n" +
		"  - 未了：第 1 件 ＊没做完＊ 的事\n" +
		"  - 未了：第 2 件 ＊没做完＊ 的事\n" +
		"  - 未了：第 3 件 ＊没做完＊ 的事\n" +
		"  - …另有 2 条\n" +
		"- **文档**\n" +
		"\n### "
	if !strings.Contains(got[0].Markdown, want) {
		t.Errorf("markdown:\n%s\nwant it to hold:\n%s", got[0].Markdown, want)
	}
	if strings.Contains(got[0].Markdown, "没归线的") {
		t.Errorf("an item under no line listed:\n%s", got[0].Markdown)
	}
	if got[0].Counts.Fronts != 2 {
		t.Errorf("counts = %+v", got[0].Counts)
	}
}
