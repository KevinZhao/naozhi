package wireup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/sysession"
)

// baseDeps builds the minimal SchedulersDeps that lets cron.Scheduler.Start
// succeed (a writable, empty store path) so the tests can focus on the
// sysession build-error surfacing contract (#1588).
func baseDeps(t *testing.T) SchedulersDeps {
	t.Helper()
	return SchedulersDeps{
		Cfg:           &config.Config{},
		CronStorePath: filepath.Join(t.TempDir(), "cron_jobs.json"),
		ParentCtx:     context.Background(),
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
