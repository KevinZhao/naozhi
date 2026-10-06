package budget

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// cst is UTC+8: its day starts at 16:00 UTC, inside a UTC ledger day file.
var cst = time.FixedZone("UTC+8", 8*3600)

// now is 03:00 on 6 Sep in cst, 19:00 on 5 Sep in UTC.
var now = time.Date(2026, 9, 6, 3, 0, 0, 0, cst)

func usd(ts time.Time, key, job string, amt float64) costledger.Entry {
	return costledger.Entry{TS: ts, Source: costledger.SourceSession, Kind: costledger.KindTurn,
		SessionKey: key, JobID: job, Backend: "claude", Unit: costledger.UnitUSD, Amount: amt}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestSubjectForKey(t *testing.T) {
	cases := []struct {
		key  string
		want Subject
	}{
		{"feishu:group:oc_1:general", "chat:feishu:group:oc_1"},
		{"feishu:group:oc_1:reviewer", "chat:feishu:group:oc_1"},
		{"slack:direct:U1:general", "chat:slack:direct:U1"},
		{"cron:0123456789abcdef", "job:0123456789abcdef"},
		{"cron:", ""},
		{"project:naozhi:planner", "project:naozhi"},
		{"project::planner", ""},
		{"dashboard:direct:abc:general", ""},
		{"dashboard:pj:0123456789abcdef:general", ""},
		{"local:takeover:Users-me-src:general", ""},
		{"sys:auto-titler", ""},
		{"scratch:6f1c", ""},
		{"feishu:group:oc_1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := SubjectForKey(tc.key); got != tc.want {
			t.Errorf("SubjectForKey(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// Every USD entry dated today in loc counts toward Global and its subject; a
// row carrying a JobID counts toward the job even on a non-cron key.
func TestIndex_SumsTodaysUSDPerSubject(t *testing.T) {
	x := NewIndex(cst, (&clock{now}).now)
	todayEarly := time.Date(2026, 9, 5, 17, 0, 0, 0, time.UTC) // 01:00 on 6 Sep in cst
	yesterday := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)  // 23:00 on 5 Sep in cst
	credits := usd(now, "feishu:group:oc_1:general", "", 50)
	credits.Unit = costledger.UnitCredits
	adjust := usd(now, "feishu:group:oc_1:general", "", -0.5)
	adjust.Kind = costledger.KindAdjust
	for _, e := range []costledger.Entry{
		usd(todayEarly, "feishu:group:oc_1:general", "", 1),
		usd(now, "feishu:group:oc_1:reviewer", "", 2),
		usd(yesterday, "feishu:group:oc_1:general", "", 100),
		usd(now.Add(24*time.Hour), "feishu:group:oc_1:general", "", 100),
		credits,
		adjust,
		usd(now, "cron:j1", "j1", 4),
		usd(now, "feishu:p2p:u1:general", "j2", 8),
		usd(now, "dashboard:direct:abc:general", "", 16),
	} {
		x.Add(e)
	}
	for s, want := range map[Subject]float64{
		"chat:feishu:group:oc_1": 2.5,
		"job:j1":                 4,
		"job:j2":                 8,
		"chat:feishu:p2p:u1":     0,
		Global:                   30.5,
	} {
		if got := x.Spent(s); got != want {
			t.Errorf("Spent(%s) = %v, want %v", s, got, want)
		}
	}
}

// The sums and the notice marks reset at midnight in loc, not in UTC.
func TestIndex_RollsOverAtLocalMidnight(t *testing.T) {
	c := &clock{time.Date(2026, 9, 6, 23, 59, 0, 0, cst)}
	x := NewIndex(cst, c.now)
	x.Add(usd(c.t, "feishu:group:oc_1:general", "", 3))
	if !x.firstNotice(NoticeWarn, Global) || x.firstNotice(NoticeWarn, Global) {
		t.Fatal("firstNotice must report true once per day")
	}
	c.t = c.t.Add(2 * time.Minute) // 00:01 on 7 Sep in cst, still 6 Sep in UTC
	if got := x.Spent(Global); got != 0 {
		t.Errorf("after local midnight Spent(Global) = %v, want 0", got)
	}
	if !x.firstNotice(NoticeWarn, Global) {
		t.Error("the notice mark must reset with the day")
	}
	x.Add(usd(c.t.Add(-2*time.Minute), "feishu:group:oc_1:general", "", 5)) // late row for 6 Sep
	x.Add(usd(c.t, "feishu:group:oc_1:general", "", 1))
	if got := x.Spent("chat:feishu:group:oc_1"); got != 1 {
		t.Errorf("Spent after rollover = %v, want 1 (yesterday's late row ignored)", got)
	}
}

func TestSubject_KindAndName(t *testing.T) {
	cases := []struct {
		s          Subject
		kind, name string
	}{
		{Global, "global", ""},
		{"chat:feishu:group:oc_1", "chat", "feishu:group:oc_1"},
		{"project:naozhi", "project", "naozhi"},
		{JobSubject("j1"), "job", "j1"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if k, n := tc.s.Kind(), tc.s.Name(); k != tc.kind || n != tc.name {
			t.Errorf("%q: Kind, Name = %q, %q; want %q, %q", tc.s, k, n, tc.kind, tc.name)
		}
	}
}

func TestStartOfDay(t *testing.T) {
	got := StartOfDay(time.Date(2026, 9, 5, 17, 0, 0, 0, time.UTC), cst)
	if want := time.Date(2026, 9, 6, 0, 0, 0, 0, cst); !got.Equal(want) {
		t.Errorf("StartOfDay = %v, want %v", got, want)
	}
}
