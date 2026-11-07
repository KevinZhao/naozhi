package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/monitor/beat"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/monitor/wake"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/action"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/planner/custody"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/routerrelay"
	"github.com/naozhi/naozhi/internal/selfupdate"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/transcribe"
	"github.com/naozhi/naozhi/internal/upstream"
	"github.com/naozhi/naozhi/internal/webhook"

	// Side-effect import: history-source factory registration lives in wireup
	// so internal/session stays backend-agnostic.
	"github.com/naozhi/naozhi/internal/wireup"
)

var version = "dev"

func main() {
	// Subcommand dispatch (before any flag parsing). A word that is not in
	// the registry — and a bare `naozhi` — prints the generated listing and
	// exits non-zero; the server path requires explicit flags (every service
	// unit passes --config), so a typoed subcommand can no longer silently
	// boot a server.
	if len(os.Args) <= 1 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	if !strings.HasPrefix(os.Args[1], "-") {
		sc := findSubcmd(os.Args[1])
		if sc == nil {
			fmt.Fprintf(os.Stderr, "naozhi: unknown command %q\n\n", os.Args[1])
			printUsage(os.Stderr)
			os.Exit(2)
		}
		sc.run(os.Args[2:])
		return
	}

	fs, configPath := newSubFlagSet("naozhi", "config.yaml")
	fs.Parse(os.Args[1:])

	// Created before applyClaudeEnvSettings so readJSONWithRetry's sleeps
	// honour ctx.Done() from the first use of the settings file.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, settingsErr, err := loadStartupConfig(ctx, *configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	logLevel := setupLogging(cfg)
	configFP := server.NewConfigFingerprint(cfg.Fingerprint.SHA256, cfg.Fingerprint.LoadedAt)
	reloader := newConfigReloader(absConfigPath(*configPath), cfg, logLevel, configFP)

	if settingsErr != nil {
		switch claudeSettingsErrSeverity(settingsErr) {
		case settingsErrSeverityCancel:
			slog.Warn("apply ~/.claude/settings.json env: aborted by ctx cancel", "err", settingsErr)
		case settingsErrSeverityMissing:
			slog.Warn("apply ~/.claude/settings.json env: file missing", "err", settingsErr)
		default:
			slog.Error("apply ~/.claude/settings.json env: read or parse failed", "err", settingsErr)
		}
	}
	// cc loads ~/.claude/settings.json itself via `--setting-sources user`; the
	// parent-process env injection above mainly feeds transcribe + sysession
	// Runner Bedrock auth (docs/rfc/direct-user-settings.md §7.1); keys the
	// shim column also allows reach spawned CLIs via baselineShimEnv.
	slog.Info("claude settings: loading user settings directly", "mode", "user")

	// Register built-in backend profiles before any consumer looks them up, and
	// install the history thumbnail hook (nil silently drops images in history).
	wireup.RegisterCLIBackends()
	wireup.WireHistoryThumbnails()

	// Fail-soft: error-level diags do NOT abort startup (docs/rfc/multi-backend.md §11.1).
	logConfigValidationDiagnostics(cfg)

	// One shim manager for all backends — each shim records its Backend in
	// state, so reconnect routing needs no per-backend state directories.
	shimMgr, err := shim.NewManager(shimManagerConfig(cfg))
	if err != nil {
		slog.Error("init shim manager", "err", err)
		os.Exit(1)
	}

	bws, ok := initBackendWrappers(ctx, cfg, shimMgr)
	if !ok {
		if bws.Default == nil {
			slog.Error("no usable cli backend configured")
		} else {
			// Default backend's --version probe failed: point the operator at
			// the config field instead of "spawn failed" on the first message.
			slog.Error("default cli backend is unavailable",
				"id", bws.Default.BackendID, "path", bws.Default.CLIPath,
				"hint", "fix the binary path in cli.backends or set cli.backend to an available backend")
		}
		os.Exit(1)
	}
	backendRuntimes := bws.Runtimes
	defaultBackend := bws.DefaultID
	wrapper := bws.Default

	noOutputTimeout, totalTimeout := cfg.ParseWatchdog()
	storePath := osutil.ExpandHome(cfg.Session.StorePath)
	workspace := osutil.ExpandHome(cfg.Session.CWD)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		slog.Error("create workspace dir", "path", workspace, "err", err)
		os.Exit(1)
	}
	// One Layout for the session store's directory; every sibling path below
	// comes from it instead of being re-derived (#2641).
	sessionLayout := datadir.ForStore(storePath)
	warnIfStateDirLarge(sessionLayout.Root())

	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	// Event log sits next to sessions.json; empty StorePath (test harnesses)
	// disables the persister via the same empty-string guard in NewRouter.
	eventLogDir := sessionLayout.EventsRoot()
	// Session-layer view of config.AccessProfiles (session must not import
	// config). Nil when none configured — sessions run on the global baseline.
	accessProfiles := buildAccessProfiles(cfg.AccessProfiles)

	// Opt-in naozhi-owned isolated Claude settings file; "" keeps the
	// `--setting-sources user` path (RFC naozhi-owned-settings-v3).
	naozhiSettingsFile := resolveNaozhiSettingsFile(cfg, storePath, claudeDir)

	// The claude model popover reads availableModels off a live process; seed the
	// same list from the settings file so a cold start offers it too.
	seedClaudeModelManifest(backendRuntimes, naozhiSettingsFile, claudeDir)

	// Opt-in operator MCP server set (RFC cli-mcp-config); "" omits --mcp-config.
	// Validated first: cc refuses to start on a bad --mcp-config, so an
	// unvalidated path would turn a typo into a total spawn outage.
	mcpConfigFile := resolveMCPConfigFile(cfg)

	// The router's notification consumers (dashboard hub, session handler) need
	// the router to exist first; they bind to this relay once built.
	routerEvents := &routerrelay.Relay{}
	router := session.NewRouter(session.RouterConfig{
		Observer:       routerEvents,
		Wrapper:        wrapper,
		DefaultBackend: defaultBackend,
		MaxProcs:       cfg.Session.MaxProcs,
		TTL:            cfg.ParseTTL(),
		PruneTTL:       cfg.ParsePruneTTL(),
		Model:          cfg.CLI.Model,
		ExtraArgs:      cfg.CLI.Args,
		// No router-wide Effort: initBackendWrappers already dropped the tier for
		// backends that cannot accept one; a router default would re-add it and
		// make the arg-drift comparison disagree with the real spawn.
		// One row per backend instead of five parallel columns (G2 #2666).
		BackendRuntimes:      backendRuntimes,
		AccessProfiles:       accessProfiles,
		DefaultAccessProfile: cfg.DefaultAccessProfile,
		NaozhiSettingsFile:   naozhiSettingsFile,
		MCPConfigFile:        mcpConfigFile,
		PermissionMode:       cliPermissionMode(cfg),
		Workspace:            workspace,
		StorePath:            storePath,
		CostLedger: session.CostLedgerConfig{
			Disabled:      !cfg.Cost.IsEnabled(),
			RetentionDays: cfg.Cost.RetentionDays,
		},
		NoOutputTimeout: noOutputTimeout,
		TotalTimeout:    totalTimeout,
		ClaudeDir:       claudeDir,
		// BackendDirs feeds the jsonl history factories so "load earlier"
		// survives a naozhi restart. Read from the backend profiles rather than
		// repeated as literals here: `naozhi doctor` already reports
		// backend.Profile.HistoryDir as where a backend keeps its transcripts,
		// and two independent derivations of one fact drift (G2 #2666, and
		// #2668 for what that costs).
		BackendDirs:       backendHistoryDirs(),
		EventLogDir:       eventLogDir,
		EventLogGenerator: "naozhi",
	})

	// What the start took back is announced with an upgrade (notify.Start).
	reattached, shimSessions := router.ReconnectShimsCtx(ctx)

	// Critical-loop heartbeats; /health degrades when one goes stale. Its
	// startup grace starts here, before the first loop registers.
	beats := beat.New(wake.SystemClock())
	router.StartCleanupLoop(ctx, cfg.ParseTTL()/2, beats)
	router.StartShimReconcileLoop(ctx, 30*time.Second)

	// Parallel init: transcriber and project scan can overlap
	var (
		stt        transcribe.Service
		sttErr     error
		projectMgr *project.Manager
		projErr    error
		initWg     sync.WaitGroup
	)
	if cfg.Transcribe != nil && cfg.Transcribe.Enabled {
		initWg.Add(1)
		go func() {
			defer initWg.Done()
			stt, sttErr = transcribe.New(ctx, transcribe.Config{
				Region:       cfg.Transcribe.Region,
				LanguageCode: cfg.Transcribe.Language,
			})
			if sttErr == nil {
				if strings.Contains(cfg.Transcribe.Language, ",") {
					slog.Info("transcribe enabled", "region", cfg.Transcribe.Region, "mode", "multi-language", "languages", cfg.Transcribe.Language)
				} else {
					slog.Info("transcribe enabled", "region", cfg.Transcribe.Region, "language", cfg.Transcribe.Language)
				}
			}
		}()
	}
	if cfg.Projects.Root != "" {
		initWg.Add(1)
		go func() {
			defer initWg.Done()
			mgr, err := newProjectManager(cfg, sessionLayout)
			if err != nil {
				projErr = fmt.Errorf("init project manager: %w", err)
				return
			}
			if err := mgr.Scan(); err != nil {
				projErr = fmt.Errorf("scan projects: %w", err)
				return
			}
			projectMgr = mgr
			slog.Info("projects enabled", "root", osutil.ExpandHome(cfg.Projects.Root), "count", len(mgr.All()))
		}()
	}
	initWg.Wait()
	if sttErr != nil {
		slog.Error("init transcriber", "err", sttErr)
		os.Exit(1)
	}
	if projErr != nil {
		slog.Error("init failed", "err", projErr)
		os.Exit(1)
	}
	// After the project scan so the first start can register projects as
	// workspaces. Opening it here runs the journal retention and the
	// first-start seed once per process.
	plannerState := wireup.OpenPlannerState(sessionLayout, cfg.Workspace.ID, projectMgr)
	// Sessions' activities go to the journal from here on, those reconnected
	// above included; the outputs index is rebuilt from it first.
	activities := activity.NewRecorder(plannerState.Journal, plannerState.Workspaces)
	if err := activities.Load(time.Now()); err != nil {
		slog.Warn("planner outputs index partly rebuilt", "err", err)
	}
	router.SetActivitySink(activities)
	// The Planner's promises (among the 任务); their reminder exits are
	// bound once the inbox and the server exist.
	promises := promise.New(promise.Config{Path: sessionLayout.PlannerPromisesPath(), Journal: plannerState.Journal})
	// Sidecar files of the sessions the Planner dispatches (lineage and
	// purpose, §3.7); swept below once the router has its sessions.
	sidecars := sidecar.Open(sessionLayout.PlannerSessionsRoot(), nil)
	// The global Planner (planner:main); Start below, once the turn entry and
	// the health machine exist. Built even when planner.enabled is off, so
	// /api/planner answers "off" rather than 404.
	home, _ := os.UserHomeDir()
	plannerAgent := agent.New(agent.Config{
		Enabled:    cfg.Planner.Enabled,
		Layout:     sessionLayout,
		Home:       home,
		DenyPaths:  []string{absConfigPath(*configPath), shimMgr.StateDir()},
		Sessions:   wireup.PlannerSessions{Router: router},
		Workspaces: plannerState.Workspaces,
		Journal:    plannerState.Journal,
		Promises:   wireup.PlannerPromises(promises),
	})
	// The Planner's patrol (§15.6): weekday scout runs over the mail, Slack
	// and Asana MCP servers, read tools only; new findings wake the Planner.
	// It runs whether or not the Planner does, keeping them on disk; the
	// pre-meeting briefs run only for the Planner, which pushes each with a
	// notify block (§6.2).
	scouter := scout.New(scout.Config{
		Layout:    sessionLayout,
		MCPConfig: mcpConfigFile,
		Session:   wireup.ScoutSession(router),
		Wake:      wireup.ScoutWake(plannerAgent, agent.SourceScout),
		Brief:     wireup.ScoutWake(plannerAgent, agent.SourceBriefing),
		PlannerOn: wireup.PlannerOn(plannerAgent),
		Journal:   plannerState.Journal,
		Beats:     beats,
	})
	// Attachments live under each workspace root, usually outside the state dir
	// checked above; walking every root must not hold up startup.
	go warnIfAttachmentsLarge(workspaceRootLister{router: router, projectMgr: projectMgr}, attachmentGCMode(cfg))

	platforms, err := initPlatforms(cfg, stt)
	if err != nil {
		slog.Error("init platforms failed", "err", err)
		os.Exit(1)
	}

	if len(platforms) == 0 {
		slog.Warn("no platforms configured, running in dashboard-only mode")
	}

	agents, cronAgents := buildAgentOpts(cfg)

	// Cron + sysession orchestration lives in wireup.WireSchedulers; main keeps
	// the operator-facing logs.
	cronLoc := cfg.ParseCronTimezone()
	slog.Info("cron timezone", "location", cronLoc.String())
	if cfg.Cron.NotifyDefault.Platform != "" && cfg.Cron.NotifyDefault.ChatID != "" {
		// Truncated chat_id suffix only, so log aggregators never carry the
		// full group/user identifier.
		slog.Info("cron notify default configured",
			"platform", cfg.Cron.NotifyDefault.Platform,
			"chat_id_suffix", chatIDSuffix(cfg.Cron.NotifyDefault.ChatID))
	}
	// Outbound webhooks receive the same run events as the dashboard Hub
	// (docs/rfc/outbound-webhooks.md); nil when none is configured.
	var webhooks *webhook.Sender
	if eps := cfg.WebhookEndpoints(); len(eps) > 0 {
		webhooks = webhook.New(eps, webhook.WithNode(cfg.Workspace.ID))
		slog.Info("outbound webhooks enabled", "endpoints", len(eps))
	}
	// Webhooks are bound first in wireup.RunTelemetryRelays; the server binds the Hub to hubTelemetry.
	runTelemetry, hubTelemetry := wireup.RunTelemetryRelays(webhooks)
	// One gate for IM and cron, fed by the ledger; nil when cost.budget is off.
	budgetGate := budget.Attach(router.Runs().CostLedger(), cfg.BudgetLimits(), cfg.BudgetLocation(), nil)
	if budgetGate != nil {
		slog.Info("cost budget enforced", "location", cfg.BudgetLocation().String(), "action", cmp.Or(cfg.Cost.Budget.Action, string(budget.ActionBlock)))
	}
	schedulers, err := wireup.WireSchedulers(wireup.SchedulersDeps{
		Cfg:           cfg,
		Router:        router,
		Platforms:     platforms,
		Agents:        cronAgents,
		Workspace:     workspace,
		CronStorePath: osutil.ExpandHome(cfg.Cron.StorePath),
		ParentCtx:     ctx,
		Telemetry:     runTelemetry,
		BuildSysession: func() (*sysession.Manager, string, error) {
			return buildSysessionManager(cfg, router, projectMgr, wrapper, storePath, runTelemetry)
		},
		Budget: budgetGate,
		Beats:  beats,
	})
	if err != nil {
		slog.Error("start cron scheduler", "err", err)
		os.Exit(1)
	}
	// Degradable: warn + continue without daemons.
	if schedulers.SysessionBuildErr != nil {
		slog.Warn("sysession manager unavailable; daemons disabled", "err", schedulers.SysessionBuildErr)
	}
	scheduler := schedulers.Cron
	sysMgr := schedulers.Sysession
	sysWorkDir := schedulers.SysessionWorkDir
	// With sysession disabled SysessionWorkDir is empty, but the vision runner
	// below still needs its JSONLs to land where the history panel filters them.
	if sysWorkDir == "" {
		sysWorkDir = sysSessionsWorkDir(cfg, storePath)
	}

	nodes := buildRemoteNodes(cfg)
	if len(nodes) > 0 {
		slog.Info("multi-node configured", "nodes", len(nodes))
	}

	// Configure reverse-connecting nodes (NAT traversal)
	var rns *node.ReverseServer
	if len(cfg.ReverseNodes) > 0 {
		rns = node.NewReverseServer(buildReverseNodeAuth(cfg), cfg.Server.TrustedProxy)
		slog.Info("reverse node auth configured", "nodes", len(cfg.ReverseNodes))
	}

	// Image auto-orientation uses a dedicated image-capable runner (the sysession
	// Runner is text-only and may be disabled); a build failure turns the
	// feature off rather than failing startup. WorkDir MUST be the sys-sessions
	// dir: the claude CLI writes a transcript JSONL per invocation, and only
	// that dir is filtered out of the history panel.
	orientEnabled := cfg.ImageOrientEnabled()
	var orientRunner server.VisionOrienter
	if orientEnabled {
		binPath, orientBackendID := "", ""
		if wrapper != nil {
			binPath = wrapper.CLIPath
			orientBackendID = wrapper.BackendID
		}
		orientWorkDir, wdErr := sysession.EnsureWorkDir(sysWorkDir)
		if wdErr != nil {
			slog.Warn("image auto-orient disabled: sys-sessions workdir unusable", "err", wdErr, "dir", sysWorkDir)
			orientEnabled = false
		} else if vr, err := sysession.NewVisionRunner(sysession.RunnerConfig{
			BinPath:   binPath,
			BackendID: orientBackendID,
			WorkDir:   orientWorkDir,
			Model:     cfg.ImageOrient.Model,
		}); err != nil {
			slog.Warn("image auto-orient disabled: vision runner build failed", "err", err)
			orientEnabled = false
		} else {
			orientRunner = vr
		}
	}

	// Built before the server so the HTTP layer (reader) and the background
	// checker (writer) share one Status. updateChecker is nil when auto-update
	// is disabled; the endpoint then reports the running version only.
	updateStatus := selfupdate.NewStatus(version)
	var updateChecker *selfupdate.Checker
	if cfg.UpdateEnabled() {
		updateChecker = newUpdateChecker(ctx, cfg, platforms, updateStatus)
	}
	updateDashboardInstall := cfg.UpdateDashboardInstall()

	// Units are named once the health machine runs, so srv (whose node
	// cache names remote sessions) is set by then.
	var srv *server.Server
	unitNames := wireup.UnitNames(router, scheduler, func() (map[string][]map[string]any, map[string]string) {
		return srv.RemoteSessions()
	})
	// "等你" inbox and push pipeline (docs/rfc/planner.md §15.1); its
	// sources are bound once the health machine exists, below.
	notifier := notify.New(notify.Config{
		Clock:   wake.SystemClock(),
		Layout:  sessionLayout,
		Journal: plannerState.Journal,
		Beats:   beats,
		Name:    unitNames,
		Link:    wireup.DashboardLink(cfg.Server.Addr),
		Start:   notify.Start{Version: version, Shims: shimSessions, Reattached: reattached},
		Midway:  wireup.MidwayExpiry,
	})
	// Side notes in the session view (§15.3 ③), read back from the journal
	// and pushed as the health machine and the inbox write it.
	notes := wireup.SessionNotes(plannerState.Journal, notifier, sidecars, router)

	// Morning brief (早报, §15.1): judged after the first health reconcile
	// past 08:30 on a weekday, pushed once through the notifier. Only the
	// health machine's Watch pokes it, so healthMon is set by then.
	var healthMon *health.Machine
	// The 任务 (§15.4): the dispatched sessions and the promises, their
	// states by health, the Planner's status blocks and the user, who can
	// stop a dispatched session.
	tasks := task.New(task.Config{
		Sessions: sidecars,
		Promises: promises,
		Health:   func() health.Snapshot { return healthMon.Snapshot() },
		Journal:  plannerState.Journal,
		Stop:     wireup.StopTask(router, func(key string) { srv.DropPlannerQueue(key) }, plannerAgent.Tell),
	})
	briefer := brief.New(brief.Config{
		Journal: plannerState.Journal,
		Health:  func() health.Snapshot { return healthMon.Snapshot() },
		Inbox: func() []notify.Item {
			open, _ := notifier.List(0)
			return open
		},
		Tasks:      tasks.List,
		Push:       notifier.Announce,
		Name:       unitNames,
		Workspaces: plannerState.Workspaces,
		Calendar:   scouter.Calendar,
		Patrol:     scouter.Since,
		Routines:   wireup.RoutineResults(scheduler),
		Carried:    notifier.Carried,
		// With several nodes the scout's node sends the brief, each node's
		// Planner covering its own sessions (docs/rfc/remote-planner.md §2.1).
		Elsewhere: func() bool { return multiNode(cfg) && !scouter.Enabled() },
	})

	// Custody (§5.4, D93): the gateway watching sessions for the user. It
	// sends through srv's own send path and reads the health machine, both
	// set before Run below. It runs only with the Planner on.
	sources := wireup.CustodySources{Router: router, Outputs: activities}
	// actions, built below, takes the cards with no recommended option to
	// the Planner; custody runs only after it is set.
	var actions *action.Runner
	custodian := custody.New(custody.Config{
		Sessions: sidecars,
		Send: func(key, text string, reason custody.Reason, n int) error {
			return srv.SubmitCustody(key, text, string(reason), n)
		},
		Inbox:     notifier,
		Journal:   plannerState.Journal,
		Health:    func() health.Snapshot { return healthMon.Snapshot() },
		Local:     sources.Local,
		Workflows: sources.Workflows,
		LastUser:  sources.LastUser,
		Progress:  sources.Progress,
		FailText:  sources.FailText,
		Settled:   sources.Settled,
		Name:      unitNames,
		Events:    sources.Events,
		HandOff:   func(key, what string) error { return actions.HandOff(key, what) },
		SendUser:  func(key, text string) error { return srv.SubmitUser(key, text) },
		// The one pause (D94): the inbox's holds custody too.
		PausedUntil: notifier.PausedUntil,
		Pause:       notifier.Pause,
		CredExpiry:  wireup.MidwayExpiry,
		Woke:        wake.NewDetector(wake.SystemClock()).Woke,
		Path:        sessionLayout.PlannerCustodyPath(),
		Disabled:    !cfg.Planner.Enabled,
	})

	// Meeting recording (docs/rfc/meeting-notes.md), macOS with
	// meeting.enabled; nil otherwise.
	meetings := wireup.OpenMeetings(wireup.MeetingDeps{
		Enabled:  cfg.Meeting.Enabled,
		Layout:   sessionLayout,
		Router:   router,
		Promises: promises,
		Notifier: notifier,
		Journal:  plannerState.Journal,
		Calendar: scouter.Calendar,
	})

	routing := buildRouting(cfg, agents, projectMgr, scheduler)
	// The options of the sessions the Planner dispatches: at dispatch, and
	// rebuilt from their sidecar when one respawns from the dashboard.
	taskOpts := wireup.TaskOpts{Base: agents["general"], Sessions: sidecars, Planner: plannerAgent.Name()}
	// The health monitor reads srv's node and discovery caches, so it is built
	// after srv; srv only reads it once it serves.
	srv = server.NewWithOptions(server.ServerOptions{
		Addr:             cfg.Server.Addr,
		Router:           router,
		Platforms:        platforms,
		Routing:          routing,
		Scheduler:        scheduler,
		Backend:          defaultBackend,
		Beats:            beats,
		Outputs:          activities,
		Notes:            notes,
		Planner:          plannerAgent,
		Scout:            scouter,
		Briefs:           briefer,
		Health:           func() health.Snapshot { return healthMon.Snapshot() },
		Workspaces:       plannerState.Workspaces,
		UnitName:         unitNames,
		Notify:           notifier,
		Tasks:            tasks,
		Meetings:         meetings,
		Custody:          custodian,
		Purpose:          sidecars.Purpose,
		TaskOpts:         taskOpts.ForKey,
		AllowedRoot:      workspace,
		IMAccess:         cfg.IMAccessPolicy(),
		IMRateLimit:      dispatch.RateLimit{MsgsPerMin: cfg.IMRateLimit.MsgsPerMin, Burst: cfg.IMRateLimit.Burst},
		IMBudget:         budgetGate,
		IMGroupScope:     dispatch.GroupScope(cfg.Session.GroupScope),
		IMThreadAutoOpen: cfg.Session.ThreadAutoOpen,
		StateDir:         sessionLayout.Root(),
		Config: server.ConfigOptions{
			// Path enables the access-profile create endpoint; absolute so the
			// write target survives cwd changes. Secrets dir holds *_FILE
			// tokens (0600).
			Path:                    absConfigPath(*configPath),
			SHA256:                  cfg.Fingerprint.SHA256,
			LoadedAt:                cfg.Fingerprint.LoadedAt,
			Live:                    configFP,
			Reload:                  reloader.Reload,
			AccessProfileSecretsDir: sessionLayout.AccessProfileSecretsRoot(),
		},
		Queue: server.QueueOptions{
			MaxDepth:     cfg.QueueMaxDepth(),
			CollectDelay: cfg.ParseCollectDelay(),
			Mode:         cfg.QueueMode(),
		},
		DashboardToken: cfg.Server.DashboardToken,
		TrustedProxy:   cfg.Server.TrustedProxy,
		ProjectManager: projectMgr,
		Transcriber:    stt,
		Update: server.UpdateOptions{
			Status:           updateStatus,
			Checker:          updateChecker,
			DashboardInstall: &updateDashboardInstall,
		},
		Sysession: server.SysessionOptions{
			Manager: sysMgr,
			WorkDir: sysWorkDir,
		},
		ImageOrient: server.ImageOrientOptions{
			Enabled: orientEnabled,
			Model:   cfg.ImageOrient.Model,
			Runner:  orientRunner,
		},
		Identity: server.IdentityOptions{
			WorkspaceID:   cfg.Workspace.ID,
			WorkspaceName: cfg.Workspace.Name,
			Version:       version,
		},
		Watchdog: server.WatchdogOptions{
			NoOutput: noOutputTimeout,
			Total:    totalTimeout,
		},
		Lifecycle: server.LifecycleOptions{
			StartupCtx: ctx,
			OnReady: func() {
				if err := osutil.SdNotify("READY=1"); err != nil {
					slog.Warn("sd_notify READY failed", "err", err)
				}
			},
		},
		Remote: server.RemoteOptions{
			Nodes:         nodes,
			ReverseServer: rns,
		},
		Relays: server.RelayOptions{
			Router:       routerEvents,
			RunTelemetry: hubTelemetry,
		},
	})

	// Session ledger and health state machine (docs/rfc/planner.md §3.8). It
	// only reads; with no planner state root it still runs, journaling and
	// writing index.json nothing. Its loop ends with ctx.
	healthMon = health.New(health.Config{
		Clock: wake.SystemClock(),
		Collect: wireup.HealthSources{
			Router:      router,
			Cron:        scheduler,
			Sysession:   sysMgr,
			Remote:      srv.RemoteSessions,
			Discovered:  srv.DiscoveredSessions,
			EventLogDir: eventLogDir,
			Sidecars:    sidecars,
			Scout:       scouter,
		}.Collect,
		Journal:    plannerState.Journal,
		Workspaces: plannerState.Workspaces,
		IndexPath:  plannerState.IndexPath,
		Beats:      beats,
		Watch: func() {
			notifier.WatchPeer()
			briefer.Poke()
		},
	})
	routerEvents.BindMonitor(healthMon.Poke)
	healthMon.Subscribe(tasks.Observe)
	healthMon.Subscribe(custodian.HandleHealth)
	go healthMon.Run(ctx)
	plannerAgent.Start(ctx, srv, healthMon.Snapshot)
	notifier.SetHealth(healthMon)
	notifier.SetCustody(custodian.Covers)
	go notifier.Run(ctx)
	promises.SetReminder(wireup.PromiseReminder{Notify: notifier, Note: plannerAgent.Note})
	go promises.Run(ctx)
	// The Planner's action blocks (§6.2): parsed from planner:main's turn
	// results; a dispatch starts a task: session at once (D88), and a task left
	// 进行中 after it reported, or that ended a turn without reporting, gets
	// the Planner a receipt.
	actions = action.New(action.Config{
		Planner:     agent.Key,
		Inbox:       notifier,
		Promises:    promises,
		Workspaces:  plannerState.Workspaces,
		Sessions:    sidecars,
		Tasks:       tasks,
		Brief:       briefer.Hold,
		Spawn:       wireup.TaskSpawner{Router: router, Opts: taskOpts, Submit: srv.SubmitPlanner}.Spawn,
		Note:        plannerAgent.Note,
		Tell:        plannerAgent.Tell,
		Reports:     wireup.PlannerReports(router),
		Outside:     plannerAgent.OutsideTurn,
		TellRefused: plannerAgent.TellRefused,
		TellUnrun:   plannerAgent.TellUnrun,
		UnrunTurn:   plannerAgent.UnrunTurn,
		PlannerBusy: plannerAgent.Busy,
		Custody:     custodian,
		Ctx:         ctx,
	})
	// The result is judged by the turn's sources before the ledger lets go
	// of a request carried over from the last run.
	resultSink := func(key, text string) {
		actions.Result(key, text)
		srv.PlannerResult(key)
	}
	router.SetResultSink(resultSink)
	// A Planner turn the restart cut across: its reply, if it came while
	// naozhi was down, is acted on now, and the requests nothing answers no
	// longer judge the next turn.
	srv.SettlePlannerTurns(router.ReattachedTurn, resultSink)
	go actions.Run(ctx)
	go custodian.Run(ctx)
	go sidecars.Run(ctx, func(key string) bool { return router.SessionFor(key) != nil })
	go briefer.Run(ctx)
	go scouter.Run(ctx)
	go meetings.Run(ctx)

	// Upstream connector: this node connects to a primary.
	if cfg.Upstream != nil {
		// srv runs relayed sends on the Orchestrator IM and the dashboard use.
		conn := upstream.New(buildUpstreamConfig(cfg), wireup.UpstreamRouter(router), projectMgr,
			upstreamDiscovery(claudeDir, router, projectMgr), srv)
		conn.ServeFiles(srv.NodeFiles())
		conn.ServePlanner(srv.NodePlanner())
		go conn.Run(ctx)
		slog.Info("upstream connector starting", "url", cfg.Upstream.URL, "node_id", cfg.Upstream.NodeID)
	}

	// runShutdown is idempotent via shutdownOnce so the signal path and the
	// server-exit path (select below) each run it exactly once; otherwise a
	// srv.Start error would skip scheduler/router teardown and a clean server
	// exit without a signal would deadlock on <-shutdownDone.
	shutdownDone := make(chan struct{})
	var shutdownOnce sync.Once
	runShutdown := func(reason string) {
		shutdownOnce.Do(func() {
			defer close(shutdownDone)
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic during shutdown", "panic", r)
				}
			}()
			// Per-phase timing makes a hung subsystem attributable from
			// journalctl alone (grep `phase=`).
			shutdownT0 := time.Now()
			slog.Info("shutdown starting", "reason", reason)
			// The run is stopping normally from here: a service manager that
			// cuts the teardown short is not a crash for the next start to
			// report. Only a run that never got here leaves its marker.
			notifier.Close()
			if err := osutil.SdNotify("STOPPING=1"); err != nil {
				slog.Warn("sd_notify STOPPING failed", "err", err)
			}
			cancel()
			// Ordered teardown contract: sysmgr → scheduler → http-drain → router
			// (runshutdown_order_test.go pins it; a new subsystem MUST take the
			// correct slot). Stop-overflow policies deliberately differ and MUST
			// NOT be harmonised (#1169): sysession force-exits (its daemons run
			// user-prompt-derived strings through a CLI, and a leaked goroutine
			// on a torn-down router could echo excerpts into another session);
			// cron budget-then-leaks (deliveries re-resolve via dispatch retry, so
			// leaking is safe and force-exit would kill long jobs).
			//
			// schedStopBudget caps a wedged cron drain at ~5s instead of the
			// scheduler's ~35s internal budget so later phases still run (#1897).
			const schedStopBudget = 5 * time.Second
			runShutdownSteps([]shutdownStep{
				// Sysession first: daemon Tick paths call into router, so racing
				// Scheduler.Stop / Router.Shutdown would be unsafe. Manager.Stop is
				// a hard wg.Wait — a daemon ignoring ctx panics the process rather
				// than leaking. nil Manager (degraded mode) keeps the slot.
				{name: "sysmgr", run: func() {
					if sysMgr == nil {
						return
					}
					sysStopCtx, sysStopCancel := context.WithTimeout(context.Background(), 5*time.Second)
					sysMgr.Stop(sysStopCtx)
					sysStopCancel()
				}},
				// Scheduler before router: in-flight cron jobs still call
				// GetOrCreate/Send. StopContext (not bare Stop) honours the host
				// deadline; persistOnShutdown ALWAYS runs so no snapshot is lost.
				{name: "scheduler", run: func() {
					schedStopCtx, schedStopCancel := context.WithTimeout(context.Background(), schedStopBudget)
					scheduler.StopContext(schedStopCtx)
					schedStopCancel()
				}},
				// The scout beside the scheduler, for the same reason: its run's
				// session calls into the router. A run cut short leaves no record.
				{name: "scout", run: scouter.Stop},
				// The meeting recorder likewise: its summary session calls into
				// the router, and its helper closes the segment it is writing.
				{name: "meeting", run: meetings.Stop},
				// ShutdownComplete closes after srv.Shutdown's 30s drain, i.e. after
				// every in-flight handler finished, so router.Shutdown never races a
				// half-cleaned session map. Already closed on server-exit paths.
				// After the scheduler: its last run.ended must still go out;
				// before http-drain so the budget is not spent on it.
				{name: "webhooks", run: func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					webhooks.Close(ctx)
				}},
				{name: "http-drain", run: func() { <-srv.ShutdownComplete() }},
				{name: "router", run: router.Shutdown},
			})
			slog.Info("shutdown complete", "reason", reason, "total_ms", time.Since(shutdownT0).Milliseconds())
		})
	}

	reloader.bindApply(srv.ApplyHotConfig, router.Backends().AccessProfiles, budgetGate)

	watchSignals(func() {
		if _, err := reloader.Reload(ctx); err != nil {
			slog.Error("SIGHUP config reload failed", "err", err)
		}
	}, runShutdown)

	slog.Info("naozhi starting",
		"version", version,
		"addr", cfg.Server.Addr,
		"workspace_id", cfg.Workspace.ID,
		"workspace_name", cfg.Workspace.Name,
		"backend", defaultBackend,
		"model", cfg.CLI.Model,
		"max_procs", cfg.Session.MaxProcs,
		"platforms", len(platforms),
	)
	// Operators copy these URLs into the IM console; WS-only platforms omitted.
	logWebhookEndpoints(cfg, platforms)
	// A profile default_backend silently moves new sessions off the router
	// default, so name each one that does.
	logProfileDefaultBackends(cfg, defaultBackend)

	// config.Load already logged this, but before setupLogging installed the
	// configured handler, so it went to stderr rather than the service log.
	config.WarnDashboardToken(cfg.Server.DashboardToken)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Start(ctx)
	}()

	// Retention for the data-dir trees no writer prunes for itself (J6 of #2548):
	// one ticker, three passes. Everything else that trims does so as a side
	// effect of writing, and stays where it is.
	go newDataDirSweeper(cfg, sessionLayout, shimMgr, sysWorkDir).Run(ctx)

	startWatchdogLoop(ctx, router.HealthCheck)

	// Best-effort and error-swallowing so a failed check never disturbs the
	// gateway; dev builds self-skip.
	startUpdateChecker(ctx, updateChecker)

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			runShutdown("server-error")
			<-shutdownDone
			os.Exit(1)
		}
		// Clean server exit without a signal still needs scheduler/router drain.
		runShutdown("server-exit")
		<-shutdownDone
	case <-shutdownDone:
		<-serverErr
	}
}

// multiNode reports whether this naozhi is one of several nodes: it polls
// nodes, takes reverse nodes, or dials a primary.
func multiNode(cfg *config.Config) bool {
	return len(cfg.Workspaces) > 0 || len(cfg.ReverseNodes) > 0 || cfg.Upstream != nil
}
