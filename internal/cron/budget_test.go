package cron

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/costledger"
)

// spawnCountingRouter counts GetOrCreate calls; every run it spawns succeeds.
type spawnCountingRouter struct{ spawns atomic.Int32 }

func (r *spawnCountingRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (r *spawnCountingRouter) Reset(string)                                               {}
func (r *spawnCountingRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	r.spawns.Add(1)
	return streakSession{}, SessionExisting, nil
}

// budgetDay is noon UTC, so the gate's day (UTC) cannot turn mid-test.
var budgetDay = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

type budgetFixture struct {
	s     *Scheduler
	r     *spawnCountingRouter
	rec   *recordingBroadcaster
	ns    *recordingNotifySender
	idx   *budget.Index
	jobID string
}

// spend books usd against job id ("" for spend outside cron) on today.
func (f *budgetFixture) spend(id string, usd float64) {
	f.idx.Add(costledger.Entry{TS: budgetDay, JobID: id, Unit: costledger.UnitUSD, Amount: usd})
}

// newBudgetFixture builds a scheduler with one IM job behind a gate with lim.
func newBudgetFixture(t *testing.T, lim budget.Limits) *budgetFixture {
	t.Helper()
	f := &budgetFixture{
		r:   &spawnCountingRouter{},
		rec: &recordingBroadcaster{},
		ns:  &recordingNotifySender{},
		idx: budget.NewIndex(time.UTC, func() time.Time { return budgetDay }),
	}
	f.s = NewScheduler(SchedulerConfig{
		MaxJobs:                5,
		StorePath:              filepath.Join(t.TempDir(), "cron_jobs.json"),
		AutoPauseAfterFailures: 2,
	}, SchedulerDeps{Router: f.r, Telemetry: f.rec, Budget: budget.NewGate(lim, f.idx)})
	f.s.configMapsPtr.Store(&cronConfigMaps{notifySender: f.ns})
	j := NewJob("@every 10m", "ping", JobIMContext{Platform: "feishu", ChatID: "chat-1"})
	if err := f.s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	f.jobID = j.ID
	return f
}

// A job at its daily cap is skipped before it spawns, scheduled or triggered:
// the skip is persisted as budget_exceeded, never counts toward auto-pause,
// and the job's chat is told once a day.
func TestBudget_BlockedRunSkipsWithoutSpawning(t *testing.T) {
	t.Parallel()
	f := newBudgetFixture(t, budget.Limits{PerJobDailyUSD: 2})
	f.spend(f.jobID, 1)
	runN(f.s, f.jobID, 1)
	if got := f.r.spawns.Load(); got != 1 {
		t.Fatalf("spawns under the cap = %d, want 1", got)
	}

	f.spend(f.jobID, 1)
	runN(f.s, f.jobID, 3)
	f.s.executeOpt(f.jobID, false)
	if got := f.r.spawns.Load(); got != 1 {
		t.Fatalf("spawns at the cap = %d, want still 1", got)
	}
	if f.rec.endedCount() != 5 {
		t.Fatalf("ended events = %d, want 5", f.rec.endedCount())
	}
	for i := 1; i < 5; i++ {
		if ev := f.rec.endedAtCron(i); ev.State != RunStateSkipped || ev.ErrorClass != ErrClassBudgetExceeded {
			t.Errorf("run %d ended (%q, %q), want (skipped, budget_exceeded)", i, ev.State, ev.ErrorClass)
		}
	}
	j := f.s.jobForTest(t, f.jobID)
	if j.Paused || j.ConsecutiveFailures != 0 || j.LastErrorClass != ErrClassBudgetExceeded {
		t.Errorf("job after 4 budget skips: paused=%v streak=%d class=%q, want active, 0, budget_exceeded",
			j.Paused, j.ConsecutiveFailures, j.LastErrorClass)
	}
	if runs := f.s.RecentRuns(f.jobID, 10); len(runs) != 5 {
		t.Errorf("history rows = %d, want 5 (the skips are recorded)", len(runs))
	}
	notices := f.ns.noticesAfter(f.s)
	var budgetNotices []string
	for _, n := range notices {
		if n != "[Cron ping] ok" {
			budgetNotices = append(budgetNotices, n)
		}
	}
	want := "[Cron ping] 今日费用预算已用尽（本任务 $2.00 / $2.00），本次已跳过；09-07 00:00 重置 · run "
	if len(budgetNotices) != 1 || len(budgetNotices[0]) != len(want)+8 || budgetNotices[0][:len(want)] != want {
		t.Errorf("budget notices = %q, want one %q<run id>", budgetNotices, want)
	}
}

// The machine-wide cap refuses every job, and each job's chat is told once
// (the notice is per job, not per spent subject). Another job under its own
// cap is not refused by a sibling's spend.
func TestBudget_GlobalCapAndSiblingJobs(t *testing.T) {
	t.Parallel()
	f := newBudgetFixture(t, budget.Limits{PerJobDailyUSD: 5, DailyUSD: 10})
	var others []string
	for _, prompt := range []string{"pong", "pang"} {
		j := NewJob("@every 10m", prompt, JobIMContext{Platform: "feishu", ChatID: "chat-" + prompt})
		if err := f.s.AddJob(j); err != nil {
			t.Fatal(err)
		}
		others = append(others, j.ID)
	}
	f.spend(f.jobID, 5)
	runN(f.s, others[0], 1)
	runN(f.s, f.jobID, 1)
	if got := f.r.spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d, want 1 (the sibling runs, the capped job does not)", got)
	}

	f.spend("", 5) // dashboard spend counts toward the machine total
	for _, id := range others {
		runN(f.s, id, 2)
	}
	if got := f.r.spawns.Load(); got != 1 {
		t.Fatalf("spawns after the global cap = %d, want still 1", got)
	}
	var budgetNotices []string
	for _, n := range f.ns.noticesAfter(f.s) {
		if n != "[Cron pong] ok" {
			budgetNotices = append(budgetNotices, n)
		}
	}
	if len(budgetNotices) != 3 {
		t.Errorf("budget notices = %q, want 3 (one per job)", budgetNotices)
	}
	if ev := f.rec.endedAtCron(f.rec.endedCount() - 1); ev.ErrorClass != ErrClassBudgetExceeded {
		t.Errorf("last run class = %q", ev.ErrorClass)
	}
}

// Under action warn the run goes ahead; the chat hears once when the job
// passes warn_ratio and once more when it passes the cap.
func TestBudget_WarnActionRunsAndNotifiesOncePerLevel(t *testing.T) {
	t.Parallel()
	f := newBudgetFixture(t, budget.Limits{PerJobDailyUSD: 10, Action: budget.ActionWarn})
	f.spend(f.jobID, 8)
	runN(f.s, f.jobID, 2)
	f.spend(f.jobID, 4)
	runN(f.s, f.jobID, 2)
	if got := f.r.spawns.Load(); got != 4 {
		t.Fatalf("spawns = %d, want 4: warn never refuses", got)
	}
	var budgetNotices []string
	for _, n := range f.ns.noticesAfter(f.s) {
		if n != "[Cron ping] ok" {
			budgetNotices = append(budgetNotices, n)
		}
	}
	want := []string{
		"[Cron ping] 今日费用已达预算的 80%（本任务 $8.00 / $10.00）",
		"[Cron ping] 今日费用已超出预算（本任务 $12.00 / $10.00），本次照常执行",
	}
	if len(budgetNotices) != 2 || budgetNotices[0] != want[0] || budgetNotices[1] != want[1] {
		t.Errorf("budget notices = %q, want %q", budgetNotices, want)
	}
}
