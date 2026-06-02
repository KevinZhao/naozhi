// Package wireup — schedulers.go owns cron + sysession orchestration
// previously living as a 60-line imperative blob in
// cmd/naozhi/main.go:715-789. Closes #1031 (R240-ARCH-12).
//
// Why this lives in wireup and not cmd/naozhi: the orchestration
// (cron.NewScheduler config translation, NotifyDefault target build,
// sysession Start, metrics phase tag)
// is pure construction with implicit ordering constraints
// (sysession.Start must run after cron is ready). Pulling it out of
// main.go makes the entry point a graph
// of explicit constructor calls (the same pattern wireup.RegisterCLIBackends
// and the history_backends.go blank-imports already established).
//
// Ownership scope (R260528-ARCH-11 / #1372 — tightened, not expanded):
// wireup deliberately owns exactly the boot-time CONSTRUCTION + REGISTRATION
// set, and the boot.go bootRegistry now names that set inspectably:
//
//	cli-backends      backends.go     (backend.RegisterDefaults)
//	history-backends  history_backends.go (blank-import init() factories)
//	schedulers        this file       (cron + sysession construction/Start)
//
// It does NOT own — and was never meant to own — the runtime LIFECYCLE of
// router / server / platforms / upstream / shim: those are long-lived
// services constructed and shut down by cmd/naozhi against ctx, not
// init()-style wiring. The package name stays "wireup" (a boot-wiring sink)
// rather than a broader "lifecycle"/"runtime" name precisely because the
// lifecycle.Manager RFC was rejected (see below); conflating the two scopes
// is what #1372 warns against. Validate() asserts the construction set ran;
// it makes no claim about service lifecycle.
//
// What this DOES NOT do (deliberately):
//   - it does NOT touch shutdown ordering (still owned by main.go's
//     runShutdown — the lifecycle.Manager RFC was rejected after two
//     reviewer rounds; current 4-line ctx-cancel + 3 explicit Stop
//     pattern is production-correct)
//   - it does NOT inline the buildSysessionManager helper (which
//     stays in cmd/naozhi/main.go because it depends on osutil
//     ExpandHome + main-package slog patterns + cli.Wrapper local
//     binPath plumbing). Caller passes it as a function pointer
//     (SysessionBuilder) so wireup stays free of those internal
//     coupling points.
//   - it does NOT change cron / sysession internal Stop behaviour
//     (Sec-LOW-2 deliberately preserves cron leak vs sysession osExit
//     divergence)

package wireup

import (
	"context"
	"fmt"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
)

// SysessionBuilder constructs the sysession Manager + work dir for a
// given config. cmd/naozhi keeps its `buildSysessionManager` private
// helper (depends on osutil + main-pkg slog patterns + cli.Wrapper
// binPath plumbing); wireup receives it as a function pointer so we
// don't transitively pull those into the wireup package.
//
// Returns (nil, "", nil) when sysession is disabled in config.
// Returns (nil, "", err) when sysession is enabled but build failed
// (caller should slog.Warn + continue with disabled sysession).
type SysessionBuilder func() (*sysession.Manager, string, error)

// SchedulersDeps groups inputs for WireSchedulers. Mirrors arguments
// cmd/naozhi/main.go currently builds inline.
type SchedulersDeps struct {
	// Cfg is the parsed config.
	Cfg *config.Config

	// Router is the live session router. Used by cron for history-panel
	// filtering via IsExcluded / RecentSessionsFilter.
	Router *session.Router

	// SessionRouterAdapter is the cmd-side cronRouterAdapter that
	// translates cron.AgentOpts ↔ session.AgentOpts. Caller (cmd/naozhi)
	// constructs it because the adapter type itself lives in main pkg
	// (cron-sysession-merge RFC §3.3.3 keeps subsystem-agnostic boundary).
	SessionRouterAdapter cron.SessionRouter

	// Platforms is the live IM platform map; cron's NotifyDefault
	// resolution + cron job IM delivery use it.
	Platforms map[string]platform.Platform

	// Agents is the cron-local agent map (after toCronAgentOpts
	// translation in main).
	Agents map[string]cron.AgentOpts

	// Workspace is the operator's allowed-root for cron WorkDir
	// validation.
	Workspace string

	// CronStorePath is the operator-resolved cron_jobs.json path
	// (i.e. AFTER osutil.ExpandHome). Caller resolves $HOME because
	// that's a main-pkg responsibility (osutil is a leaf used at
	// boot for path expansion); wireup just passes the resolved path
	// through to cron.SchedulerConfig.
	CronStorePath string

	// ParentCtx becomes Scheduler.SchedulerConfig.ParentCtx and the ctx
	// argument to sysession.Manager.Start.
	ParentCtx context.Context

	// Telemetry is the runtelemetry.Broadcaster cron's Phase D uses
	// for run-started / run-ended events. nil disables broadcast (test).
	Telemetry runtelemetry.Broadcaster

	// BuildSysession constructs the sysession Manager. Caller-supplied
	// because the helper depends on main-pkg internals (see
	// SysessionBuilder godoc).
	BuildSysession SysessionBuilder
}

// Schedulers holds the constructed subsystem instances for caller-side
// shutdown wiring. cmd/naozhi keeps its existing runShutdown blob
// referencing these fields; this package does NOT install a coordinator.
type Schedulers struct {
	Cron      *cron.Scheduler
	Sysession *sysession.Manager
	// SysessionWorkDir is the resolved work dir for sysession daemons.
	// Empty when sysession is disabled or build failed.
	SysessionWorkDir string
	// SysessionBuildErr carries the sysession build failure (if any) back
	// to the caller as part of the helper's return contract — NOT via a
	// caller-managed closure side-channel (R20260602141221-ARCH-3 / #1588).
	// It is non-nil ONLY when sysession was enabled in config but the build
	// failed; it is nil both when sysession is disabled AND when the build
	// succeeded. This keeps the failure-vs-disabled distinction (documented
	// on SysessionBuilder) inside WireSchedulers' return contract so any
	// caller — not just one that replicated the closure trick — observes it.
	// Caller should slog.Warn + continue (sysession is degradable; a broken
	// claude binary must not break naozhi startup).
	SysessionBuildErr error
}

// WireSchedulers constructs cron.Scheduler + sysession.Manager in the
// correct order and starts both subsystems.
//
// Side-effects (matches what the inlined main.go code did):
//   - cron.Scheduler.Start() is called — cron is ready to tick on return
//   - sysession.Manager.Start(ParentCtx) is called when enabled
//
// Caller is responsible for the metrics.StartupPhaseSchedulerMs.Set
// call after WireSchedulers returns (kept out of wireup to avoid
// pulling internal/metrics into this package's dependency surface;
// the metrics call is two lines at the call site).
//
// Why Start happens inside this helper (vs returning pre-Start
// instances): the original main.go inline code calls Start
// immediately after construction, and dashboard wiring downstream
// assumes the scheduler is already running. Preserving order keeps
// the cutover invisible to callers.
//
// Returned error wraps the cron.Start error (terminal — caller
// should os.Exit). sysession build failure does NOT error out: it
// returns nil-Sysession + nil err so the caller can log + continue
// (matches the existing main.go pattern of "sysession unavailable;
// daemons disabled" slog warn).
func WireSchedulers(deps SchedulersDeps) (Schedulers, error) {
	out := Schedulers{}
	if deps.Cfg == nil {
		return out, fmt.Errorf("WireSchedulers: nil Cfg")
	}
	if deps.ParentCtx == nil {
		return out, fmt.Errorf("WireSchedulers: nil ParentCtx")
	}

	cronLoc := deps.Cfg.ParseCronTimezone()
	notifyDefault := cron.NotifyTarget{
		Platform: deps.Cfg.Cron.NotifyDefault.Platform,
		ChatID:   deps.Cfg.Cron.NotifyDefault.ChatID,
	}

	scheduler := cron.NewScheduler(cron.SchedulerConfig{
		Router:        deps.SessionRouterAdapter,
		Platforms:     deps.Platforms,
		Agents:        deps.Agents,
		AgentCommands: deps.Cfg.AgentCommands,
		StorePath:     deps.CronStorePath,
		MaxJobs:       deps.Cfg.Cron.MaxJobs,
		ExecTimeout:   deps.Cfg.ParseExecutionTimeout(),
		Location:      cronLoc,
		NotifyDefault: notifyDefault,
		AllowedRoot:   deps.Workspace,
		JitterMax:     deps.Cfg.ParseCronJitterMax(),
		ParentCtx:     deps.ParentCtx,
		Telemetry:     deps.Telemetry,
	})
	if err := scheduler.Start(); err != nil {
		return out, fmt.Errorf("start cron scheduler: %w", err)
	}
	out.Cron = scheduler

	// (auto-workspace-chain SessionIDExcluder registration removed — RFC
	// docs/rfc/project-stable-session-key.md §9.1. The cron Scheduler's
	// IsExcluded / RecentSessionsFilter is retained because the history
	// panel still uses it to hide cron-owned sessionIDs.)

	// Build sysession Manager when enabled. Failure is degradable:
	// missing/broken claude binary should not break naozhi startup.
	// We surface the build error via out.SysessionBuildErr (part of the
	// return contract, not a caller closure side-channel — #1588) and
	// return out.Sysession=nil so caller's nil-guard is meaningful.
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
