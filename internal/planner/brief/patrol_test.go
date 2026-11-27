package brief

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/front"
)

// patrolled is a briefer with the project patrol's part wired: rep is what
// the patrol has, since the window starts it was asked for, briefed the
// stopped kinds it was told a brief named.
type patrolled struct {
	rep     ProjectPatrol
	since   []time.Time
	briefed [][]string
}

func (f *fixture) patrolBriefer(p *patrolled) *Briefer {
	b := f.briefer(nil, nil)
	b.cfg.Projects = func(since time.Time) ProjectPatrol { p.since = append(p.since, since); return p.rep }
	b.cfg.ProjectsBriefed = func(kinds []string) { p.briefed = append(p.briefed, kinds) }
	return b
}

// TestPatrolSection (§15.23 ⑤): after the lines of work the brief lists the
// pull requests the patrol had made that wait on the user, each with its
// link and checks, then the new findings, three of them and a count of the
// rest, then, once, the kinds it stopped reporting.
func TestPatrolSection(t *testing.T) {
	f := newFixture(t)
	f.output(at(2, 22, 0), "task:ab12", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "out.md"})
	f.now = at(3, 8, 30)
	p := &patrolled{rep: ProjectPatrol{
		PRs: []ProjectPR{
			{Title: "修正重连时的[会话]恢复", URL: "https://github.com/owner/repo/pull/31", CI: "通过", Workspace: "网关仓库", Fresh: true},
			{Title: "去掉重复的退避实现", URL: "https://github.com/owner/repo/pull/29", Workspace: "网关仓库"},
		},
		New: []ProjectFinding{
			{Title: "配置热加载丢了 watchdog 的新值", Kind: "bug", Workspace: "网关仓库"},
			{Title: "README 的安装步骤与脚本不符", Kind: "doc", Workspace: "文档"},
			{Title: "过期的 TODO", Kind: "stale"},
			{Title: "第四条", Kind: "debt", Workspace: "网关仓库"},
			{Title: "第五条", Kind: "debt", Workspace: "网关仓库"},
		},
		Stopped: []ProjectStopped{{Kind: "debt", Adopted: 3, Of: 10}},
	}}
	b := f.patrolBriefer(p)
	b.cfg.Fronts = func() []front.Front {
		return []front.Front{{ID: "gw", Name: "网关重构", State: "进行中", Updated: at(3, 8, 0)}}
	}
	b.Check()
	got := b.Recent(1)
	if len(got) != 1 || len(f.pushes) != 1 {
		t.Fatalf("briefs %d, pushes %d", len(got), len(f.pushes))
	}
	want := "自 11/2 08:30 以来：工作线 1 · 巡查 8 · 完成 1\n" +
		"\n### 工作线（1）\n" +
		"- **网关重构**：进行中\n" +
		"\n### 巡查（8）\n" +
		"- 已备好的 PR：[修正重连时的［会话］恢复](https://github.com/owner/repo/pull/31) · CI 通过 · 工作区 网关仓库\n" +
		"- 已备好的 PR：[去掉重复的退避实现](https://github.com/owner/repo/pull/29) · 工作区 网关仓库\n" +
		"- 新发现：配置热加载丢了 watchdog 的新值 · bug · 工作区 网关仓库\n" +
		"- 新发现：README 的安装步骤与脚本不符 · doc · 工作区 文档\n" +
		"- 新发现：过期的 TODO · stale\n" +
		"- …另有 2 条\n" +
		"- debt 类你处置过的最近 10 条只采纳了 3 条，已停报；要恢复，从 planner/patrol/declined.json 的 kinds 里删掉它\n" +
		"\n### 完成的（1）\n"
	if !strings.HasPrefix(got[0].Markdown, want) {
		t.Errorf("markdown:\n%s\nwant it to start:\n%s", got[0].Markdown, want)
	}
	if c := got[0].Counts; c.Patrol != 8 || c.PatrolNews != 7 {
		t.Errorf("counts = %+v, want 8 listed and 7 of them news", c)
	}
	if f.pushes[0].Body != "工作线 1 · 巡查 8 · 完成 1" {
		t.Errorf("push body %q", f.pushes[0].Body)
	}
	// The patrol was asked for the brief's own window, and told which
	// stopped kinds the brief named.
	if len(p.since) == 0 || !p.since[len(p.since)-1].Equal(f.now.Add(-firstLookback)) {
		t.Errorf("asked for the window since %v, want %s", p.since, f.now.Add(-firstLookback))
	}
	if len(p.briefed) != 1 || !slices.Equal(p.briefed[0], []string{"debt"}) {
		t.Errorf("told of the stopped kinds %q", p.briefed)
	}
}

// TestPatrolSectionAbsent: with nothing from the patrol the brief is, byte
// for byte, the brief without it.
func TestPatrolSectionAbsent(t *testing.T) {
	briefOf := func(wire func(*fixture) *Briefer) (string, Counts) {
		f := newFixture(t)
		f.output(at(2, 22, 0), "task:ab12", "ws1", activity.Activity{Type: activity.TypeOutput, Path: "out.md"})
		f.now = at(3, 8, 30)
		b := wire(f)
		b.Check()
		got := b.Recent(1)
		if len(got) != 1 {
			t.Fatalf("%d briefs", len(got))
		}
		return got[0].Markdown, got[0].Counts
	}
	p := &patrolled{}
	plain, plainCounts := briefOf(func(f *fixture) *Briefer { return f.briefer(nil, nil) })
	with, withCounts := briefOf(func(f *fixture) *Briefer { return f.patrolBriefer(p) })
	if with != plain || withCounts != plainCounts || strings.Contains(with, "巡查") {
		t.Errorf("brief with an empty patrol:\n%s\nwant the brief without it:\n%s", with, plain)
	}
	if len(p.briefed) != 0 {
		t.Errorf("told of stopped kinds %q with none", p.briefed)
	}
}

// TestPatrolWorthABrief: what is new from the patrol is worth a brief of
// its own — a finding, a pull request that became ready, a kind stopped —
// and a pull request an earlier brief listed, still waiting, is not: it is
// listed in a brief that has something else to say.
func TestPatrolWorthABrief(t *testing.T) {
	pr := ProjectPR{Title: "修重连", URL: "https://github.com/owner/repo/pull/31", CI: "通过", Workspace: "网关仓库"}
	fresh := pr
	fresh.Fresh = true
	for name, tc := range map[string]struct {
		rep  ProjectPatrol
		sent bool
	}{
		"a pull request that still waits": {ProjectPatrol{PRs: []ProjectPR{pr}}, false},
		"a pull request newly ready":      {ProjectPatrol{PRs: []ProjectPR{fresh}}, true},
		"a new finding":                   {ProjectPatrol{New: []ProjectFinding{{Title: "重连时崩", Kind: "bug"}}}, true},
		"a kind stopped":                  {ProjectPatrol{Stopped: []ProjectStopped{{Kind: "doc", Adopted: 0, Of: 10}}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.now = at(3, 8, 30)
			p := &patrolled{rep: tc.rep}
			b := f.patrolBriefer(p)
			b.Check()
			if got := len(b.Recent(1)) == 1; got != tc.sent || len(f.pushes) == 1 != tc.sent {
				t.Fatalf("sent = %v (pushes %d), want %v", got, len(f.pushes), tc.sent)
			}
			// A stopped kind is marked said only by a brief that said it.
			if said := len(p.briefed) == 1; said != (tc.sent && len(tc.rep.Stopped) > 0) {
				t.Errorf("told of stopped kinds %q", p.briefed)
			}
			if tc.sent && !strings.Contains(b.Recent(1)[0].Markdown, "\n### 巡查（1）\n") {
				t.Errorf("markdown:\n%s", b.Recent(1)[0].Markdown)
			}
		})
	}
}

// TestPatrolSectionCaps: the pull requests are listed up to a section's
// cap, the findings up to three.
func TestPatrolSectionCaps(t *testing.T) {
	var p ProjectPatrol
	for i := range maxItems + 2 {
		p.PRs = append(p.PRs, ProjectPR{Title: "PR", URL: "https://github.com/owner/repo/pull/" + string(rune('a'+i))})
	}
	p.New = []ProjectFinding{{Title: "一", Kind: "bug"}, {Title: "二", Kind: "bug"}, {Title: "三", Kind: "bug"}}
	var b strings.Builder
	patrolSection(&b, p)
	got := b.String()
	if strings.Count(got, "- 已备好的 PR：") != maxItems || !strings.Contains(got, "- …另有 2 个 PR\n") ||
		strings.Count(got, "- 新发现：") != 3 || strings.Contains(got, "另有 0 条") || !strings.HasPrefix(got, "\n### 巡查（15）\n") {
		t.Errorf("section:\n%s", got)
	}
}
