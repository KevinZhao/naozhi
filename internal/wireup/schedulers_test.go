package wireup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// baseDeps builds the minimal SchedulersDeps that lets cron.Scheduler.Start
// succeed (a writable, empty store path) so the tests can focus on the
// sysession build-error surfacing contract (#1588). A real (empty-config)
// *session.Router satisfies the deps.Router non-nil check; WireSchedulers only
// wraps it in the cron adapter and never calls its methods unless a cron job
// executes (these tests don't trigger one). R260528-ARCH-23 (#1382).
func baseDeps(t *testing.T) SchedulersDeps {
	t.Helper()
	return SchedulersDeps{
		Cfg:           &config.Config{},
		CronStorePath: filepath.Join(t.TempDir(), "cron_jobs.json"),
		ParentCtx:     context.Background(),
		Router:        session.NewRouter(session.RouterConfig{}),
	}
}

// TestWireSchedulers_SysessionBuild: a failed sysession build is surfaced
// through Schedulers.SysessionBuildErr while startup stays degradable (no
// terminal error, nil manager); sysession disabled (no builder) and a builder
// returning no error leave SysessionBuildErr nil, so the caller can tell
// disabled from a failed build and logs no spurious warning.
func TestWireSchedulers_SysessionBuild(t *testing.T) {
	boom := errors.New("claude binary missing")
	for _, c := range []struct {
		name  string
		build func() (*sysession.Manager, string, error)
		want  error
	}{
		{"disabled", nil, nil},
		{"builder returns no error", func() (*sysession.Manager, string, error) { return nil, "", nil }, nil},
		{"build failure", func() (*sysession.Manager, string, error) { return nil, "", boom }, boom},
	} {
		t.Run(c.name, func(t *testing.T) {
			deps := baseDeps(t)
			deps.BuildSysession = c.build
			out, err := WireSchedulers(deps)
			if err != nil {
				t.Fatalf("WireSchedulers returned terminal error: %v", err)
			}
			t.Cleanup(func() {
				if out.Cron != nil {
					out.Cron.Stop()
				}
				if out.Sysession != nil {
					out.Sysession.Stop(context.Background())
				}
			})
			if out.Sysession != nil || !errors.Is(out.SysessionBuildErr, c.want) || out.SysessionWorkDir != "" {
				t.Errorf("Sysession = %v, SysessionBuildErr = %v, SysessionWorkDir = %q; want nil, %v, empty",
					out.Sysession, out.SysessionBuildErr, out.SysessionWorkDir, c.want)
			}
		})
	}
}

// TestWireSchedulers_NilRouter: a nil Router is rejected at startup rather
// than panicking at the first job execution.
func TestWireSchedulers_NilRouter(t *testing.T) {
	deps := baseDeps(t)
	deps.Router = nil
	if _, err := WireSchedulers(deps); err == nil {
		t.Fatal("WireSchedulers must return error when Router is nil")
	}
}

// SchedulersDeps.Budget reaches the scheduler: with the machine's daily cap
// spent, a triggered run ends budget_exceeded instead of spawning.
func TestWireSchedulers_BudgetReachesScheduler(t *testing.T) {
	deps := baseDeps(t)
	idx := budget.NewIndex(time.UTC, nil)
	idx.Add(costledger.Entry{TS: time.Now(), Unit: costledger.UnitUSD, Amount: 1})
	deps.Budget = budget.NewGate(budget.Limits{DailyUSD: 1}, idx)
	out, err := WireSchedulers(deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(out.Cron.Stop)
	j := cron.NewJobFull(cron.JobInit{Schedule: "@every 10m", Prompt: "ping", IM: cron.JobIMContext{Platform: "feishu", ChatID: "c1"}})
	if err := out.Cron.AddJob(j); err != nil {
		t.Fatal(err)
	}
	if err := out.Cron.TriggerNow(j.ID); err != nil {
		t.Fatal(err)
	}
	testhelper.Eventually(t, func() bool {
		got, _ := out.Cron.GetJob(j.ID)
		return got.LastErrorClass == cron.ErrClassBudgetExceeded
	}, 5*time.Second, "the triggered run was not refused by the budget")
}
