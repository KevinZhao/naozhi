package notify

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
)

// The Midway session becomes a notice MidwayWarn before it expires, says
// so once it has, and closes itself once mwinit renews it; no session, no
// notice.
func TestMidwayNotice(t *testing.T) {
	n, _, _ := newTest(t, local(9, 0), datadir.Layout{})
	var exp time.Time
	n.cfg.Midway = func() time.Time { return exp }
	open := func() []Item {
		n.Check()
		o, _ := n.List(0)
		return o
	}
	for _, tc := range []struct {
		name  string
		exp   time.Time
		title string // "" for none open
	}{
		{"no session", time.Time{}, ""},
		{"later", local(9, 0).Add(MidwayWarn + time.Minute), ""},
		{"within the warning", local(9, 0).Add(MidwayWarn), "Midway 凭证 09:30 到期"},
		{"expired", local(8, 59), "Midway 凭证已过期"},
		{"renewed", local(9, 0).Add(20 * time.Hour), ""},
	} {
		exp = tc.exp
		got := open()
		switch {
		case tc.title == "" && len(got) != 0:
			t.Errorf("%s: open = %+v, want none", tc.name, got)
		case tc.title != "" && (len(got) != 1 || got[0].Source != midwaySource || got[0].Title != tc.title || got[0].Kind != KindNotice || !got[0].AutoClose):
			t.Errorf("%s: open = %+v, want %q", tc.name, got, tc.title)
		}
	}
	if _, closed := n.List(10); len(closed) != 1 || closed[0].ClosedBy != ClosedByAuto {
		t.Errorf("closed = %+v, want the notice closed by itself", closed)
	}
}

// A session's failure that is not retried for the user says what they do
// about it; a cron job's says only its class.
func TestFailHint(t *testing.T) {
	for _, c := range []struct {
		kind  health.Kind
		class health.ErrorClass
		want  string
	}{
		{health.KindSession, health.ErrRateLimit, "限流；托管不自动重试；过几分钟发「继续」"},
		{health.KindSession, health.ErrCredentialExpired, "凭证过期；需要你处理凭证后发「继续」"},
		{health.KindSession, health.ErrAuth, "凭证 / 权限拒绝；需要你处理凭证后发「继续」"},
		{health.KindSession, health.ErrRefusal, "模型拒答"},
		{health.KindCron, health.ErrRateLimit, "限流"},
	} {
		n := New(Config{System: func(string, string) error { return nil }})
		n.HandleHealth(health.Event{Transition: &health.Transition{From: health.Idle,
			Unit: health.Unit{ID: "u", Kind: c.kind, State: health.Failed, ErrClass: c.class}}})
		if open, _ := n.List(0); len(open) != 1 || open[0].Why != c.want {
			t.Errorf("%s %s: open = %+v, want why %q", c.kind, c.class, open, c.want)
		}
	}
}

func TestParsePause(t *testing.T) {
	now := local(10, 0)
	for _, c := range []struct {
		in   string
		want time.Time
		err  bool
	}{
		{"today", EndOfToday(now), false},
		{"1h", now.Add(time.Hour), false},
		{"168h", now.Add(MaxPause), false},
		{"169h", time.Time{}, true},
		{"", time.Time{}, true},
		{"-1h", time.Time{}, true},
	} {
		got, err := ParsePause(c.in, now)
		if (err != nil) != c.err || !got.Equal(c.want) {
			t.Errorf("ParsePause(%q) = %v, %v", c.in, got, err)
		}
	}
}
