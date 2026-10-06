package budget

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/testhelper"
)

func gateWith(lim Limits, entries ...costledger.Entry) *Gate {
	x := NewIndex(cst, (&clock{now}).now)
	for _, e := range entries {
		x.Add(e)
	}
	return NewGate(lim, x)
}

// No cap configured, or no gate at all, admits everything.
func TestGate_NilAdmits(t *testing.T) {
	if g := NewGate(Limits{WarnRatio: 0.5, Action: ActionBlock}, NewIndex(cst, nil)); g != nil {
		t.Fatal("NewGate without a cap must return nil")
	}
	var g *Gate
	if v := g.CheckKey("feishu:group:oc_1:general"); v != (Verdict{}) {
		t.Errorf("nil gate CheckKey = %+v, want the zero verdict", v)
	}
	if v := g.CheckJob("j1"); v != (Verdict{}) {
		t.Errorf("nil gate CheckJob = %+v", v)
	}
	if g.ShouldWarnOnce(Global) {
		t.Error("nil gate must never warn")
	}
}

// Each cron job has its own cap: one job at it is blocked, another is not,
// and the job's rows count whether they carry the JobID or only a cron key.
func TestGate_PerJobLimit(t *testing.T) {
	g := gateWith(Limits{PerJobDailyUSD: 10},
		usd(now, "cron:j1", "j1", 6),
		usd(now, "cron:j1", "", 2),
		usd(now, "cron:j2", "j2", 7.9),
	)
	cases := []struct {
		job                 string
		warn, over, blocked bool
		spent               float64
	}{
		{"j1", true, false, false, 8},
		{"j2", false, false, false, 7.9},
		{"j3", false, false, false, 0},
	}
	for _, tc := range cases {
		v := g.CheckJob(tc.job)
		if v.Warn != tc.warn || v.Over != tc.over || v.Blocked != tc.blocked || v.Spent != tc.spent || v.Limit != 10 {
			t.Errorf("CheckJob(%s) = %+v, want warn=%v over=%v blocked=%v spent=%v limit=10",
				tc.job, v, tc.warn, tc.over, tc.blocked, tc.spent)
		}
	}
	g.idx.Add(usd(now, "cron:j1", "j1", 2))
	if v := g.CheckJob("j1"); !v.Blocked || v.Subject != "job:j1" {
		t.Errorf("at the limit CheckJob = %+v, want blocked on the job", v)
	}
	if v := g.CheckKey("cron:j1"); !v.Blocked || v.Subject != "job:j1" {
		t.Errorf("CheckKey on the job's cron key = %+v, want blocked on the job", v)
	}
	if v := g.CheckJob("j2"); v.Blocked {
		t.Errorf("another job was blocked: %+v", v)
	}
}

// IM, planner and dashboard keys have no scoped cap: a per-job limit alone
// never applies to them, however much they spend.
func TestGate_NonCronKeysHaveNoScopedLimit(t *testing.T) {
	g := gateWith(Limits{PerJobDailyUSD: 5},
		usd(now, "feishu:group:oc_1:general", "", 50),
		usd(now, "project:naozhi:planner", "", 50),
		usd(now, "dashboard:direct:x:general", "", 50))
	for _, key := range []string{"feishu:group:oc_1:general", "project:naozhi:planner", "dashboard:direct:x:general"} {
		if v := g.CheckKey(key); v != (Verdict{}) {
			t.Errorf("CheckKey(%s) = %+v, want no limit", key, v)
		}
	}
}

// The global cap blocks every chat and job once the machine total reaches
// it, and the verdict names whichever subject is nearer its limit.
func TestGate_GlobalCapAndNearestSubject(t *testing.T) {
	g := gateWith(Limits{PerJobDailyUSD: 4, DailyUSD: 20},
		usd(now, "feishu:group:oc_1:general", "", 9),
		usd(now, "cron:j1", "j1", 3.5),
	)
	if v := g.CheckJob("j1"); v.Subject != "job:j1" || v.Spent != 3.5 || v.Limit != 4 || !v.Warn || v.Blocked {
		t.Errorf("job at 87.5%%, global at 62.5%%: %+v, want the job named, warn, not blocked", v)
	}
	if v := g.CheckKey("feishu:group:oc_1:general"); v.Subject != Global || v.Spent != 12.5 || v.Warn {
		t.Errorf("CheckKey on a chat = %+v, want global at 12.5/20", v)
	}
	g.idx.Add(usd(now, "dashboard:direct:x:general", "", 7.5))
	for _, v := range []Verdict{g.CheckKey("feishu:group:oc_9:general"), g.CheckJob("j1"), g.CheckJob("j2"), g.CheckKey("sys:auto-titler")} {
		if !v.Blocked || v.Subject != Global || v.Spent != 20 || v.Limit != 20 {
			t.Errorf("global at its cap: %+v, want blocked on global", v)
		}
	}
}

// warn never blocks; WarnRatio defaults to 0.8.
func TestGate_WarnActionAndDefaultRatio(t *testing.T) {
	g := gateWith(Limits{DailyUSD: 10, Action: ActionWarn}, usd(now, "cron:j1", "j1", 12))
	if v := g.CheckJob("j1"); !v.Over || !v.Warn || v.Blocked {
		t.Errorf("warn action over the cap = %+v, want over and warn, never blocked", v)
	}
	g = gateWith(Limits{DailyUSD: 10}, usd(now, "cron:j1", "j1", 7.9))
	if v := g.CheckJob("j1"); v.Warn {
		t.Errorf("7.9/10 = %+v, under the default 0.8 ratio", v)
	}
	g.idx.Add(usd(now, "cron:j1", "j1", 0.1))
	if v := g.CheckJob("j1"); !v.Warn || v.Blocked {
		t.Errorf("8/10 = %+v, want warn at the default ratio", v)
	}
	if !g.ShouldWarnOnce(Global) || g.ShouldWarnOnce(Global) || !g.ShouldWarnOnce("job:j1") {
		t.Error("ShouldWarnOnce must fire once per subject per day")
	}
}

// Attach counts today's entries already in the ledger (a cst day starts
// inside a UTC day file, and an entry stamped exactly at its midnight is
// today's) and then each new one, each once.
func TestAttach_WarmsFromLedgerThenFollows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cost")
	opts := costledger.Options{Now: func() time.Time { return now }}
	seed := costledger.NewStore(dir, opts)
	seed.Append(usd(time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC), "feishu:group:oc_1:general", "", 100)) // 23:00 5 Sep cst
	seed.Append(usd(time.Date(2026, 9, 5, 15, 59, 59, 0, time.UTC), "cron:j1", "j1", 200))               // 23:59:59 5 Sep cst
	seed.Append(usd(time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC), "cron:j1", "j1", 2))                   // 00:00 6 Sep cst
	seed.Append(usd(time.Date(2026, 9, 5, 17, 0, 0, 0, time.UTC), "feishu:group:oc_1:general", "", 3))   // 01:00 6 Sep cst
	seed.Close()

	store := costledger.NewStore(dir, opts)
	t.Cleanup(store.Close)
	g := Attach(store, Limits{DailyUSD: 100}, cst, (&clock{now}).now)
	if got, job := g.idx.Spent(Global), g.idx.Spent("job:j1"); got != 5 || job != 2 {
		t.Fatalf("warm Spent(Global) = %v, Spent(job:j1) = %v; want 5 and 2 (only today in cst, from its midnight)", got, job)
	}
	store.Append(usd(now, "feishu:group:oc_1:general", "", 4))
	testhelper.Eventually(t, func() bool { return g.idx.Spent(Global) == 9 }, 5*time.Second, "live entry never counted")
	store.Close()
	if v := g.CheckKey("feishu:group:oc_1:general"); v.Spent != 9 {
		t.Errorf("after close Spent = %v, want 9", v.Spent)
	}
}

func TestAttach_NoCapOrLedgerOffIsNil(t *testing.T) {
	if Attach(costledger.NewStore("", costledger.Options{}), Limits{DailyUSD: 1}, cst, nil) != nil {
		t.Error("a disabled ledger must give a nil gate")
	}
	store := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{})
	t.Cleanup(store.Close)
	if Attach(store, Limits{}, cst, nil) != nil {
		t.Error("no cap must give a nil gate")
	}
}
