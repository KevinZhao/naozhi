package agent

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
)

// newJournaledAgent is newTestAgent with a journal.
func newJournaledAgent(t *testing.T, sess *fakeSessions, now *time.Time) (*Agent, *journal.Journal) {
	t.Helper()
	a, lay := newTestAgent(t, sess, now)
	j, err := journal.Open(lay.PlannerLogRoot())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.Journal = j
	return a, j
}

func handoffChecks(t *testing.T, j *journal.Journal, now time.Time) []handoffCheck {
	t.Helper()
	entries, err := j.Read(now.Add(-30*24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var out []handoffCheck
	for _, e := range entries {
		if e.Kind != journal.KindHandoff {
			continue
		}
		var ck handoffCheck
		if err := json.Unmarshal(e.Data, &ck); err != nil {
			t.Fatal(err)
		}
		out = append(out, ck)
	}
	return out
}

// The handoff prompt asks for a source on every item and for the 等你定
// section the gateway sweeps (§15.14 ⑤).
func TestHandoffPromptSources(t *testing.T) {
	for _, want := range []string{"五个小标题", "每条事项都要带出处：任务 key、承诺 ID、#issue，或用户那句话的时间", "## 等你定",
		"原样抄了超过 3 天的网关会移进 parked.md", `"搁置 N 条"那一行由网关写`} {
		if !strings.Contains(handoffPrompt, want) {
			t.Errorf("handoff prompt lacks %q", want)
		}
	}
}

// Every source form counts; a line naming none is journaled, not refused.
func TestHandoffSources(t *testing.T) {
	for _, c := range []struct {
		item string
		ok   bool
	}{
		{"- 修复批次（`task:4fb2`）在跑", true},
		{"- task:4fb2 等 CI", true},
		{"- 提醒交周报 pr_ab12cd34ef", true},
		{"- #3902 收起检查", true},
		{"- 用户 11-14 15:25 说先不做", true},
		{"- 2026-11-14 定的", true},
		{"- 11月14日说的", true},
		{"- 家里网络换不换（11-14 用户说）", true},
		{"- 09-03 定的", true},
		{"- 看 https://github.com/x/y/pull/1", true},
		{"- 出处：用户口头", true},
		{"- HYD 选型等用户定", false},
		{"- 要 2-3 天", false},
	} {
		if got := hasSource(c.item); got != c.ok {
			t.Errorf("hasSource(%q) = %v, want %v", c.item, got, c.ok)
		}
	}
}

// TestCheckHandoff: a fresh summary's items without a source are counted
// into the journal; the 等你定 items copied unchanged for more than three
// days move into parked.md and leave one line; a summary with nothing to
// change is kept byte for byte.
func TestCheckHandoff(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 14, 8, 30, 0, 0, loc)
	a, j := newJournaledAgent(t, &fakeSessions{}, &now)
	summary := "## 进行中\n- 修复批次 `task:4fb2`：等 CI\n  下一步合并\n- 会议录音的事还没头绪\n\n" +
		"## 等你定\n- HYD 选型（出处：11-12 10:00 用户说）\n- M1 要不要升级（出处：#3901）\n\n" +
		"## 答应用户的事\n无\n## 用户新说的偏好与决定\n无\n## 待跟进\n无"
	if got := a.checkHandoff("2026-11-14", summary); got != summary {
		t.Fatalf("nothing parked yet, summary changed:\n%s", got)
	}
	cks := handoffChecks(t, j, now)
	if len(cks) != 1 || cks[0].Items != 4 || cks[0].Unsourced != 1 || cks[0].Decide != 2 ||
		!slices.Equal(cks[0].Lines, []string{"- 会议录音的事还没头绪"}) {
		t.Fatalf("check = %+v", cks)
	}

	// Four days on, M1 rewritten on the way: HYD moves, M1 stays.
	now = time.Date(2026, 11, 16, 8, 30, 0, 0, loc)
	a.checkHandoff("2026-11-16", strings.Replace(summary, "M1 要不要升级", "M1 要不要升级：问过价格", 1))
	now = time.Date(2026, 11, 18, 8, 30, 0, 0, loc)
	got := a.checkHandoff("2026-11-18", strings.Replace(summary, "M1 要不要升级", "M1 要不要升级：问过价格", 1))
	want := "## 等你定\n- M1 要不要升级：问过价格（出处：#3901）\n- 搁置 1 条，见 parked.md\n\n## 答应用户的事"
	if !strings.Contains(got, want) || strings.Contains(got, "HYD") {
		t.Fatalf("summary:\n%s\nwant it to hold:\n%s", got, want)
	}
	data, err := os.ReadFile(a.cfg.Layout.PlannerParkedPath())
	if err != nil || !strings.Contains(string(data), "- HYD 选型（出处：11-12 10:00 用户说） · 移入 2026-11-18\n") {
		t.Fatalf("parked.md = %q, %v", data, err)
	}
	cks = handoffChecks(t, j, now)
	if last := cks[len(cks)-1]; last.Parked != 1 || last.ParkedTotal != 1 {
		t.Fatalf("check = %+v", last)
	}

	// The next shift copies the gateway's line and HYD again: one line, no HYD.
	now = now.Add(24 * time.Hour)
	got = a.checkHandoff("2026-11-19", strings.Replace(got, "- 搁置 1 条", "- HYD 选型（出处：11-12 10:00 用户说）\n- 搁置 1 条", 1))
	if strings.Count(got, "搁置 1 条") != 1 || strings.Contains(got, "HYD") {
		t.Fatalf("copied handoff:\n%s", got)
	}
}

// A section left empty by the sweep says 无 again only when nothing is
// parked; with no 等你定 section the line is added at the end, and the
// 无 of an empty section gives way to it.
func TestCheckHandoffSections(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.Local)
	a, _ := newJournaledAgent(t, &fakeSessions{}, &now)
	if err := a.Park("家里网络", "11-14 15:25 用户说"); err != nil {
		t.Fatal(err)
	}
	if got := a.checkHandoff("d", "## 进行中\n无"); got != "## 进行中\n无\n\n## 等你定\n- 搁置 1 条，见 parked.md" {
		t.Fatalf("no section: %q", got)
	}
	if got := a.checkHandoff("d", "## 等你定\n无\n## 待跟进\n无"); got != "## 等你定\n- 搁置 1 条，见 parked.md\n## 待跟进\n无" {
		t.Fatalf("empty section: %q", got)
	}
}

// TestShiftChangeParks: the check runs on the summary the shift change
// asks for, and the stored and handed-over summary is the checked one.
func TestShiftChangeParks(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 13, 20, 0, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, lay := newTestAgent(t, sess, &now)
	turns := &fakeTurns{reply: "## 等你定\n- HYD 选型（出处：#3800）"}
	a.turns = turns
	ctx := context.Background()
	a.reconcile(ctx)
	for day := 14; day <= 18; day++ {
		now = time.Date(2026, 11, day, 8, 31, 0, 0, loc)
		sess.mu.Lock()
		sess.st.SessionID = "s1" // the new conversation said something
		sess.mu.Unlock()
		a.reconcile(ctx)
	}
	saved, err := os.ReadFile(filepath.Join(lay.PlannerHandoffRoot(), "2026-11-18.md"))
	if err != nil || string(saved) != "## 等你定\n- 搁置 1 条，见 parked.md" {
		t.Fatalf("11-18 handoff = %q, %v", saved, err)
	}
	if last := turns.sent[len(turns.sent)-1]; !strings.Contains(last, "搁置 1 条") || strings.Contains(last, "HYD") {
		t.Fatalf("handed over: %q", last)
	}
	if list := a.Parked(); len(list) != 1 || list[0].Day != "2026-11-18" {
		t.Fatalf("parked = %+v", list)
	}
}

// parked.md is in the Planner's read scope: under its state root's allow
// rule, and no deny rule of the data root covers it.
func TestParkedReadable(t *testing.T) {
	now := time.Now()
	a, lay := newTestAgent(t, &fakeSessions{}, &now)
	p := lay.PlannerParkedPath()
	root := lay.PlannerRoot()
	if filepath.Dir(p) != root || !slices.Contains(a.Opts().PermissionsAllow, absRule("Read", filepath.Join(root, "**"))) {
		t.Fatalf("parked.md %s not under the allowed root %s", p, root)
	}
	rel, _ := filepath.Rel(lay.Root(), p)
	for _, pat := range []string{"*.yaml", "*.json", "*.pem", "*.key", ".env", ".env.*"} {
		if ok, _ := path.Match(pat, filepath.Base(rel)); ok {
			t.Errorf("deny pattern %s covers %s", pat, rel)
		}
	}
	for _, r := range a.Opts().PermissionsDeny {
		if strings.Contains(r, "parked") {
			t.Errorf("deny rule %s names parked.md", r)
		}
	}
}

// In 等你定 each line is an item, bulleted or not, and a line opening with
// an #issue is an item, not a heading: both are tracked and parked one by
// one.
func TestCheckHandoffDecideLines(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 14, 8, 30, 0, 0, loc)
	a, j := newJournaledAgent(t, &fakeSessions{}, &now)
	summary := "## 等你定\n#3902 收起检查要不要改\nHYD 选型（#1）\n- M1 升级（#2）\n  已问过价格\n## 待跟进\n无"
	a.checkHandoff("2026-11-14", summary)
	if ck := handoffChecks(t, j, now); len(ck) != 1 || ck[0].Items != 3 || ck[0].Decide != 3 || ck[0].Unsourced != 0 {
		t.Fatalf("check = %+v", ck)
	}
	now = time.Date(2026, 11, 18, 8, 30, 0, 0, loc)
	got := a.checkHandoff("2026-11-18", summary)
	if got != "## 等你定\n- 搁置 3 条，见 parked.md\n## 待跟进\n无" {
		t.Fatalf("summary:\n%s", got)
	}
	list := a.Parked()
	if len(list) != 3 || list[0].Text != "#3902 收起检查要不要改" || list[1].Text != "HYD 选型（#1）" ||
		list[2].Text != "M1 升级（#2） 已问过价格" {
		t.Fatalf("parked = %+v", list)
	}
}

// An item the user put down and the Planner copies back in its own
// wording of the source stays out of the handoff and is not parked twice.
func TestCheckHandoffUserParkedCopied(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 14, 15, 30, 0, 0, loc)
	a, _ := newJournaledAgent(t, &fakeSessions{}, &now)
	if err := a.Park("家里网络换不换", "11-14 15:25 用户说"); err != nil {
		t.Fatal(err)
	}
	summary := "## 等你定\n- 家里网络换不换（11-14 15:25 用户说）\n- M1 升级（#2）"
	for day := 15; day <= 19; day++ {
		now = time.Date(2026, 11, day, 8, 30, 0, 0, loc)
		got := a.checkHandoff("d", summary)
		want := "搁置 1 条" // M1 joins it once copied for more than three days
		if day == 19 {
			want = "搁置 2 条"
		}
		if strings.Contains(got, "家里网络") || !strings.Contains(got, want) {
			t.Fatalf("11-%d summary:\n%s", day, got)
		}
	}
	if list := a.Parked(); len(list) != 2 || strings.Count(list[0].Text+list[1].Text, "家里网络") != 1 {
		t.Fatalf("parked = %+v", list)
	}
}
