package wireup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/sysession"
)

// stubSessionRouter is a minimal cron.SessionRouter for tests that do not
// exercise cron job execution. R20260603040203-GO-6.
type stubSessionRouter struct{}

func (stubSessionRouter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string) {
}
func (stubSessionRouter) Reset(key string) {}
func (stubSessionRouter) GetOrCreate(ctx context.Context, key string, opts cron.AgentOpts) (cron.Session, cron.SessionStatus, error) {
	return nil, 0, nil
}

// baseDeps builds the minimal SchedulersDeps that lets cron.Scheduler.Start
// succeed (a writable, empty store path) so the tests can focus on the
// sysession build-error surfacing contract (#1588).
func baseDeps(t *testing.T) SchedulersDeps {
	t.Helper()
	return SchedulersDeps{
		Cfg:                  &config.Config{},
		CronStorePath:        filepath.Join(t.TempDir(), "cron_jobs.json"),
		ParentCtx:            context.Background(),
		SessionRouterAdapter: stubSessionRouter{},
	}
}

// TestWireSchedulers_SysessionDisabled: when no builder is supplied
// (sysession disabled), the helper must report no build error and no
// manager. Disabled must be distinguishable from a failed build.
func TestWireSchedulers_SysessionDisabled(t *testing.T) {
	deps := baseDeps(t)
	deps.BuildSysession = nil

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
	if out.Sysession != nil {
		t.Errorf("expected nil Sysession when disabled, got %v", out.Sysession)
	}
	if out.SysessionBuildErr != nil {
		t.Errorf("expected nil SysessionBuildErr when disabled, got %v", out.SysessionBuildErr)
	}
}

// TestWireSchedulers_SysessionBuildFailure: when the builder returns an
// error, the helper must surface it via the Schedulers struct return
// contract — not require a caller-managed closure side-channel — while
// keeping startup degradable (no terminal error, nil manager).
func TestWireSchedulers_SysessionBuildFailure(t *testing.T) {
	deps := baseDeps(t)
	wantErr := errors.New("claude binary missing")
	deps.BuildSysession = func() (*sysession.Manager, string, error) {
		return nil, "", wantErr
	}

	out, err := WireSchedulers(deps)
	if err != nil {
		t.Fatalf("build failure must be degradable, got terminal error: %v", err)
	}
	t.Cleanup(func() {
		if out.Cron != nil {
			out.Cron.Stop()
		}
		if out.Sysession != nil {
			out.Sysession.Stop(context.Background())
		}
	})
	if out.Sysession != nil {
		t.Errorf("expected nil Sysession on build failure, got %v", out.Sysession)
	}
	if !errors.Is(out.SysessionBuildErr, wantErr) {
		t.Errorf("expected SysessionBuildErr=%v, got %v", wantErr, out.SysessionBuildErr)
	}
	if out.SysessionWorkDir != "" {
		t.Errorf("expected empty SysessionWorkDir on build failure, got %q", out.SysessionWorkDir)
	}
}

// TestWireSchedulers_SysessionSuccessNoErr: a builder that returns a nil
// error (even with a nil manager, e.g. disabled-at-builder) must leave
// SysessionBuildErr nil so the caller does not log a spurious warning.
func TestWireSchedulers_SysessionSuccessNoErr(t *testing.T) {
	deps := baseDeps(t)
	deps.BuildSysession = func() (*sysession.Manager, string, error) {
		return nil, "", nil
	}

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
	if out.SysessionBuildErr != nil {
		t.Errorf("expected nil SysessionBuildErr on success, got %v", out.SysessionBuildErr)
	}
}

// TestWireSchedulers_NilSessionRouterAdapter verifies that a nil
// SessionRouterAdapter is rejected at startup with a clear error rather than
// panicking at first job execution. R20260603040203-GO-6.
func TestWireSchedulers_NilSessionRouterAdapter(t *testing.T) {
	deps := baseDeps(t)
	deps.SessionRouterAdapter = nil // deliberately nil

	_, err := WireSchedulers(deps)
	if err == nil {
		t.Fatal("WireSchedulers must return error when SessionRouterAdapter is nil")
	}
	if !errors.Is(err, err) { // always true; just validate it's non-nil
		t.Fatalf("unexpected error type: %v", err)
	}
	const want = "nil SessionRouterAdapter"
	if msg := err.Error(); len(msg) == 0 {
		t.Errorf("error message is empty, want %q substring", want)
	}
}
