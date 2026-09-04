// schedulers.go owns cron + sysession construction/Start. wireup owns exactly
// the boot-time CONSTRUCTION + REGISTRATION set (cli-backends,
// history-backends, schedulers); runtime lifecycle and shutdown ordering of
// router / server / platforms stay in cmd/naozhi (#1372).

package wireup

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
)

// SysessionBuilder constructs the sysession Manager + work dir. Caller-supplied
// so wireup does not pull in cmd/naozhi's osutil / cli.Wrapper plumbing.
// Returns (nil, "", nil) when disabled, (nil, "", err) when enabled but broken.
type SysessionBuilder func() (*sysession.Manager, string, error)

// SchedulersDeps groups inputs for WireSchedulers.
type SchedulersDeps struct {
	// Cfg is the parsed config.
	Cfg *config.Config

	// Router is the live session router; WireSchedulers wraps it in the
	// cron.SessionRouter adapter so main never names cron.SessionRouter.
	Router *session.Router

	// Platforms is the live IM platform map for cron notification delivery.
	Platforms map[string]platform.Platform

	// Agents is the cron-local agent map (after toCronAgentOpts).
	Agents map[string]cron.AgentOpts

	// Workspace is the operator's allowed-root for cron WorkDir validation.
	Workspace string

	// CronStorePath is the cron_jobs.json path, already ExpandHome-resolved.
	CronStorePath string

	// ParentCtx becomes SchedulerConfig.ParentCtx and sysession.Manager.Start's ctx.
	ParentCtx context.Context

	// Telemetry receives run-started / run-ended events; nil disables broadcast.
	Telemetry runtelemetry.Broadcaster

	// BuildSysession constructs the sysession Manager (see SysessionBuilder).
	BuildSysession SysessionBuilder
}

// Schedulers holds the constructed subsystem instances for caller-side
// shutdown wiring; this package installs no coordinator.
type Schedulers struct {
	Cron      *cron.Scheduler
	Sysession *sysession.Manager
	// SysessionWorkDir is the resolved sysession work dir; empty when
	// disabled or build failed.
	SysessionWorkDir string
	// SysessionBuildErr is non-nil ONLY when sysession was enabled but the
	// build failed (nil when disabled or succeeded), so every caller observes
	// the failure-vs-disabled distinction via the return contract (#1588).
	// Degradable: caller should warn + continue.
	SysessionBuildErr error
}

// WireSchedulers constructs and Starts cron.Scheduler and (when enabled)
// sysession.Manager; downstream dashboard wiring assumes the scheduler is
// already running on return. A cron.Start error is terminal; a sysession
// build failure is returned via Schedulers.SysessionBuildErr with nil err.
// The caller records StartupPhaseSchedulerMs (wireup has no metrics dep).
func WireSchedulers(deps SchedulersDeps) (Schedulers, error) {
	out := Schedulers{}
	if deps.Cfg == nil {
		return out, fmt.Errorf("WireSchedulers: nil Cfg")
	}
	if deps.ParentCtx == nil {
		return out, fmt.Errorf("WireSchedulers: nil ParentCtx")
	}

	// A nil router would only panic at first job execution; catch it at startup.
	if deps.Router == nil {
		return out, fmt.Errorf("WireSchedulers: nil Router")
	}

	cronLoc := deps.Cfg.ParseCronTimezone()
	notifyDefault := cron.NotifyTarget{
		Platform: deps.Cfg.Cron.NotifyDefault.Platform,
		ChatID:   deps.Cfg.Cron.NotifyDefault.ChatID,
	}

	// Degradable: a bad sandbox config must not break startup; placement=sandbox
	// jobs then fail per-run with ErrClassSandboxUnavailable.
	sandboxRunner, sandboxErr := newAgentcoreSandboxRunner(deps.ParentCtx,
		deps.Cfg.Cron.Sandbox.RuntimeARN, deps.Cfg.Cron.Sandbox.Region)
	if sandboxErr != nil {
		slog.Warn("cron sandbox placement unavailable; placement=sandbox jobs will fail until fixed",
			"err", sandboxErr)
	}

	scheduler := cron.NewScheduler(cron.SchedulerConfig{
		StorePath:     deps.CronStorePath,
		MaxJobs:       deps.Cfg.Cron.MaxJobs,
		ExecTimeout:   deps.Cfg.ParseExecutionTimeout(),
		Location:      cronLoc,
		NotifyDefault: notifyDefault,
		AllowedRoot:   deps.Workspace,
		JitterMax:     deps.Cfg.ParseCronJitterMax(),
		ParentCtx:     deps.ParentCtx,
	}, cron.SchedulerDeps{
		Router:        newCronRouterAdapter(deps.Router),
		NotifySender:  newPlatformNotifySender(deps.Platforms),
		Agents:        deps.Agents,
		AgentCommands: deps.Cfg.AgentCommands,
		Telemetry:     deps.Telemetry,
		Sandbox:       sandboxRunner,
	})
	if err := scheduler.Start(); err != nil {
		return out, fmt.Errorf("start cron scheduler: %w", err)
	}
	out.Cron = scheduler

	// Recorded after a successful cron.Start so the audit step reflects a live
	// scheduler; not in requiredBootSteps because sysession is degradable (#2314).
	recordBootStep("schedulers", BootStep{
		Kind:   "schedulers",
		Detail: "cron scheduler + sysession construction/Start",
	})

	// Degradable: a broken claude binary must not break startup.
	if deps.BuildSysession != nil {
		sysMgr, sysWorkDir, sysErr := deps.BuildSysession()
		if sysMgr != nil {
			sysMgr.Start(deps.ParentCtx)
		}
		out.Sysession = sysMgr
		out.SysessionWorkDir = sysWorkDir
		out.SysessionBuildErr = sysErr
	}
	return out, nil
}
