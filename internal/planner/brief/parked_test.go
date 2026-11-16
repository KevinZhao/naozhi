package brief

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/parked"
)

// TestParkedWeekly: the week's first brief lists the 搁置区, Monday's or,
// with Monday slept through, Tuesday's; the other briefs of the week do
// not (§15.14 ⑤).
func TestParkedWeekly(t *testing.T) {
	f := newFixture(t)
	b := f.briefer(nil, nil)
	list := []parked.Entry{
		{Text: "HYD 选型（出处：#3800）", Day: "2026-10-30"},
		{Text: "家里网络（出处：11-01 10:00 用户说）", Day: "2026-11-01", ByUser: true},
	}
	b.cfg.Parked = func() []parked.Entry { return list }

	f.now = at(2, 8, 31) // Monday
	b.Check()
	last := b.Recent(1)
	if len(last) != 1 || last[0].Day != "2026-11-02" || last[0].Counts.Parked != 2 {
		t.Fatalf("Monday brief = %+v", last)
	}
	md := last[0].Markdown
	for _, want := range []string{"### 搁置区（2）", "- HYD 选型（出处：#3800） · 已搁置 3 天", "家里网络（出处：11-01 10:00 用户说） · 已搁置 1 天 · 你说放下"} {
		if !strings.Contains(md, want) {
			t.Errorf("Monday brief lacks %q:\n%s", want, md)
		}
	}
	if len(f.pushes) != 1 || !strings.Contains(f.pushes[0].Body, "搁置 2") {
		t.Errorf("push = %+v", f.pushes)
	}

	f.now = at(3, 8, 31) // Tuesday: not again this week
	b.Check()
	if b.last.Day != "2026-11-03" || b.last.Counts.Parked != 0 || b.last.Sent {
		t.Fatalf("Tuesday listed the 搁置区 again: %+v", b.last)
	}

	f.now = at(11, 8, 31) // Tuesday 11-11, Monday slept through
	b.Check()
	if r := b.Recent(1); len(r) != 1 || r[0].Day != "2026-11-11" || r[0].Counts.Parked != 2 {
		t.Fatalf("first brief of the week after = %+v", r)
	}
}
