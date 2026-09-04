package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/selfupdate"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/transcribe"
	"github.com/naozhi/naozhi/internal/upstream"

	// Side-effect import: history-source factory registration lives in wireup
	// so internal/session stays backend-agnostic.
	"github.com/naozhi/naozhi/internal/wireup"
)

var version = "dev"

func main() {
	// Subcommands (before flag.Parse)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			runSetup(os.Args[2:])
			return
		case "install":
			runInstall(os.Args[2:])
			return
		case "uninstall":
			runUninstall(os.Args[2:])
			return
		case "version", "--version":
			fmt.Println(version)
			return
		case "shim":
			runShim(os.Args[2:])
			return
		case "doctor":
			runDoctor(os.Args[2:])
			return
		case "upgrade":
			runUpgrade(os.Args[2:])
			return
		}
	}

	// t0 anchors every startup phase gauge; captured after subcommand dispatch
	// so setup/install/doctor do not pollute the boot histogram.
	t0 := time.Now()

	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	metrics.StartupPhaseConfigMs.Set(time.Since(t0).Milliseconds())

	setupLogging(cfg)

	// Created before applyClaudeEnvSettings so readJSONWithRetry's sleeps
	// honour ctx.Done() from the first use of the settings file.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := applyClaudeEnvSettings(ctx); err != nil {
		switch claudeSettingsErrSeverity(err) {
		case settingsErrSeverityCancel:
			slog.Warn("apply ~/.claude/settings.json env: aborted by ctx cancel", "err", err)
		case settingsErrSeverityMissing:
			slog.Warn("apply ~/.claude/settings.json env: file missing", "err", err)
		default:
			slog.Error("apply ~/.claude/settings.json env: read or parse failed", "err", err)
		}
	}
	// cc loads ~/.claude/settings.json itself via `--setting-sources user`; the
	// parent-process env injection above only feeds transcribe + sysession
	// Runner Bedrock auth (docs/rfc/direct-user-settings.md §7.1).
	slog.Info("claude settings: loading user settings directly", "mode", "user")

	// Register built-in backend profiles before any consumer looks them up.
	// Explicit rather than init()-driven so a missing import fails loudly (#1165).
	wireup.EnsureCLIBackends()

	// A dropped blank-import or no-op'd helper aborts startup here instead of
	// degrading silently to empty history / missing profiles.
	if err := wireup.Validate(); err != nil {
		slog.Error("wireup validation failed", "err", err)
		os.Exit(1)
	}

	// Fail-soft: error-level diags do NOT abort startup (docs/rfc/multi-backend.md §11.1).
	logConfigValidationDiagnostics(cfg)

	// One shim manager for all backends — each shim records its Backend in
	// state, so reconnect routing needs no per-backend state directories.
	shimMgr, err := shim.NewManager(shim.ManagerConfig{
		StateDir:        osutil.ExpandHome(cfg.Session.Shim.StateDir),
		IdleTimeout:     parseDurationOrDefault(cfg.Session.Shim.IdleTimeout, 4*time.Hour),
		WatchdogTimeout: parseDurationOrDefault(cfg.Session.Shim.WatchdogTimeout, 30*time.Minute),
		BufferSize:      cfg.Session.Shim.BufferSize,
		MaxBufBytes:     parseBytesOrDefault(cfg.Session.Shim.MaxBufferBytes, 50*1024*1024),
		MaxShims:        cfg.Session.Shim.MaxShims,
	})
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
				"hint", "fix the binary path in cli.backends or set cli.default to an available backend")
		}
		os.Exit(1)
	}
	wrappers := bws.Wrappers
	backendModels := bws.Models
	backendModelLists := bws.ModelLists
	backendExtraArgs := bws.ExtraArgs
	backendEfforts := bws.Efforts
	defaultBackend := bws.DefaultID
	wrapper := bws.Default

	noOutputTimeout, totalTimeout := cfg.ParseWatchdog()
	storePath := osutil.ExpandHome(cfg.Session.StorePath)
	workspace := osutil.ExpandHome(cfg.Session.CWD)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		slog.Error("create workspace dir", "path", workspace, "err", err)
		os.Exit(1)
	}
	warnIfStateDirLarge(filepath.Dir(storePath))

	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	// Event log sits next to sessions.json; empty StorePath (test harnesses)
	// disables the persister via the same empty-string guard in NewRouter.
	eventLogDir := ""
	if storePath != "" {
		eventLogDir = filepath.Join(filepath.Dir(storePath), "events")
	}
	// Session-layer view of config.AccessProfiles (session must not import
	// config). Nil when none configured — sessions run on the global baseline.
	accessProfiles := buildAccessProfiles(cfg.AccessProfiles)

	// Opt-in naozhi-owned isolated Claude settings file; "" keeps the
	// `--setting-sources user` path (RFC naozhi-owned-settings-v3).
	naozhiSettingsFile := resolveNaozhiSettingsFile(cfg, storePath, claudeDir)

	// Opt-in operator MCP server set (RFC cli-mcp-config); "" omits --mcp-config.
	// Validated first: cc refuses to start on a bad --mcp-config, so an
	// unvalidated path would turn a typo into a total spawn outage.
	mcpConfigFile := resolveMCPConfigFile(cfg)

	router := session.NewRouter(session.RouterConfig{
		Wrapper:          wrapper,
		Wrappers:         wrappers,
		DefaultBackend:   defaultBackend,
		MaxProcs:         cfg.Session.MaxProcs,
		TTL:              cfg.ParseTTL(),
		PruneTTL:         cfg.ParsePruneTTL(),
		Model:            cfg.CLI.Model,
		ExtraArgs:        cfg.CLI.Args,
		BackendModels:    backendModels,
		BackendExtraArgs: backendExtraArgs,
		// No router-wide Effort: initBackendWrappers already dropped the tier for
		// backends that cannot accept one; a router default would re-add it and
		// make the arg-drift comparison disagree with the real spawn.
		BackendEfforts:       backendEfforts,
		BackendModelLists:    backendModelLists,
		AccessProfiles:       accessProfiles,
		DefaultAccessProfile: cfg.DefaultAccessProfile,
		NaozhiSettingsFile:   naozhiSettingsFile,
		MCPConfigFile:        mcpConfigFile,
		Workspace:            workspace,
		StorePath:            storePath,
		NoOutputTimeout:      noOutputTimeout,
		TotalTimeout:         totalTimeout,
		ClaudeDir:            claudeDir,
		// KiroSessionsDir / CodexSessionsDir feed the jsonl history factories so
		// "load earlier" survives a naozhi restart (the CLIs' documented paths).
		KiroSessionsDir: osutil.ExpandHome("~/.kiro/sessions/cli"),
		// Codex rollout transcripts are date-bucketed under this root.
		CodexSessionsDir:  osutil.ExpandHome("~/.codex/sessions"),
		EventLogDir:       eventLogDir,
		EventLogGenerator: "naozhi",
	})
	metrics.StartupPhaseRouterMs.Set(time.Since(t0).Milliseconds())

	router.ReconnectShimsCtx(ctx)
	metrics.StartupPhaseShimReconnectMs.Set(time.Since(t0).Milliseconds())

	router.StartCleanupLoop(ctx, cfg.ParseTTL()/2)
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
			root := osutil.ExpandHome(cfg.Projects.Root)
			mgr, err := project.NewManager(root, project.PlannerDefaults{
				Model:  cfg.Projects.PlannerDefaults.Model,
				Prompt: cfg.Projects.PlannerDefaults.Prompt,
			}, project.WithIncludeRoot(cfg.Projects.IncludeRoot))
			if err != nil {
				projErr = fmt.Errorf("init project manager: %w", err)
				return
			}
			if err := mgr.Scan(); err != nil {
				projErr = fmt.Errorf("scan projects: %w", err)
				return
			}
			projectMgr = mgr
			slog.Info("projects enabled", "root", root, "count", len(mgr.All()))
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

	platforms, err := initPlatforms(cfg, stt)
	if err != nil {
		slog.Error("init platforms failed", "err", err)
		os.Exit(1)
	}

	if len(platforms) == 0 {
		slog.Warn("no platforms configured, running in dashboard-only mode")
	}

	agents, cronAgents := buildAgentOpts(cfg)

	if cmd, ok := firstUndefinedAgentCommand(cfg.AgentCommands, agents); !ok {
		slog.Error("agent_commands references undefined agent",
			"command", cmd, "agent", cfg.AgentCommands[cmd])
		os.Exit(1)
	}
	metrics.StartupPhasePlatformsMs.Set(time.Since(t0).Milliseconds())

	// Cron + sysession orchestration lives in wireup.WireSchedulers; main keeps
	// the operator-facing logs and the metrics (wireup has no metrics dep).
	cronLoc := cfg.ParseCronTimezone()
	slog.Info("cron timezone", "location", cronLoc.String())
	if cfg.Cron.NotifyDefault.Platform != "" && cfg.Cron.NotifyDefault.ChatID != "" {
		// Truncated chat_id suffix only, so log aggregators never carry the
		// full group/user identifier.
		slog.Info("cron notify default configured",
			"platform", cfg.Cron.NotifyDefault.Platform,
			"chat_id_suffix", chatIDSuffix(cfg.Cron.NotifyDefault.ChatID))
	}
	schedulers, err := wireup.WireSchedulers(wireup.SchedulersDeps{
		Cfg:           cfg,
		Router:        router,
		Platforms:     platforms,
		Agents:        cronAgents,
		Workspace:     workspace,
		CronStorePath: osutil.ExpandHome(cfg.Cron.StorePath),
		ParentCtx:     ctx,
		Telemetry:     nil, // wired post-Hub via dashboard.go SetTelemetry
		BuildSysession: func() (*sysession.Manager, string, error) {
			return buildSysessionManager(cfg, router, projectMgr, wrapper, storePath)
		},
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
	metrics.StartupPhaseSchedulerMs.Set(time.Since(t0).Milliseconds())

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
		binPath := ""
		if wrapper != nil {
			binPath = wrapper.CLIPath
		}
		orientWorkDir, wdErr := sysession.EnsureWorkDir(sysWorkDir)
		if wdErr != nil {
			slog.Warn("image auto-orient disabled: sys-sessions workdir unusable", "err", wdErr, "dir", sysWorkDir)
			orientEnabled = false
		} else if vr, err := sysession.NewVisionRunner(sysession.RunnerConfig{
			BinPath: binPath,
			WorkDir: orientWorkDir,
			Model:   cfg.ImageOrient.Model,
			EnvAllowlist: []string{
				"ANTHROPIC_",
				"CLAUDE_",
				"AWS_",
				"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
				"http_proxy", "https_proxy", "no_proxy",
			},
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

	srv := server.NewWithOptions(server.ServerOptions{
		Addr:          cfg.Server.Addr,
		Router:        router,
		Platforms:     platforms,
		Agents:        agents,
		AgentCommands: cfg.AgentCommands,
		Scheduler:     scheduler,
		Backend:       cfg.CLI.Backend,
		WorkspaceID:   cfg.Workspace.ID,
		WorkspaceName: cfg.Workspace.Name,
		AllowedRoot:   workspace,
		StateDir:      filepath.Dir(storePath),
		// ConfigPath enables the access-profile create endpoint; absolute so the
		// write target survives cwd changes. Secrets dir holds *_FILE tokens (0600).
		ConfigPath:              absConfigPath(*configPath),
		AccessProfileSecretsDir: filepath.Join(filepath.Dir(storePath), "access-profile-secrets"),
		NoOutputTimeout:         noOutputTimeout,
		TotalTimeout:            totalTimeout,
		QueueMaxDepth:           cfg.QueueMaxDepth(),
		QueueCollectDelay:       cfg.ParseCollectDelay(),
		QueueMode:               cfg.QueueMode(),
		DashboardToken:          cfg.Server.DashboardToken,
		TrustedProxy:            cfg.Server.TrustedProxy,
		ProjectManager:          projectMgr,
		Nodes:                   nodes,
		ReverseNodeServer:       rns,
		Transcriber:             stt,
		StartupCtx:              ctx,
		Version:                 version,
		UpdateStatus:            updateStatus,
		UpdateChecker:           updateChecker,
		UpdateDashboardInstall:  &updateDashboardInstall,
		SysessionManager:        sysMgr,
		SysWorkDir:              sysWorkDir,
		// Default-on; opt-out via session.project_stable_key.enabled: false.
		ProjectStableKeyEnabled: cfg.Session.ProjectStableKey.ResolvedEnabled(true),
		ImageOrientEnabled:      orientEnabled,
		ImageOrientModel:        cfg.ImageOrient.Model,
		ImageOrientRunner:       orientRunner,
		OnReady: func() {
			if err := osutil.SdNotify("READY=1"); err != nil {
				slog.Warn("sd_notify READY failed", "err", err)
			}
		},
	})
	metrics.StartupPhaseServerMs.Set(time.Since(t0).Milliseconds())

	// Upstream connector: this node connects to a primary.
	if cfg.Upstream != nil {
		// Own KeyResolver so reverse-RPC planner restart takes the same
		// ResolveForPlannerKey path as the dashboard handler without coupling
		// upstream to the server package.
		upstreamResolver := session.NewKeyResolver(agents, project.NewDataSource(projectMgr))
		conn := upstream.New(buildUpstreamConfig(cfg), router, projectMgr, upstreamResolver)
		if claudeDir != "" {
			conn.SetDiscoverFunc(newUpstreamDiscoverFunc(claudeDir, router, projectMgr))
			conn.SetPreviewFunc(newUpstreamPreviewFunc(claudeDir))
		}
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
				// ShutdownComplete closes after srv.Shutdown's 30s drain, i.e. after
				// every in-flight handler finished, so router.Shutdown never races a
				// half-cleaned session map. Already closed on server-exit paths.
				{name: "http-drain", run: func() { <-srv.ShutdownComplete() }},
				{name: "router", run: router.Shutdown},
			})
			slog.Info("shutdown complete", "reason", reason, "total_ms", time.Since(shutdownT0).Milliseconds())
		})
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		runShutdown("signal:" + sig.String())
	}()

	slog.Info("naozhi starting",
		"version", version,
		"addr", cfg.Server.Addr,
		"workspace_id", cfg.Workspace.ID,
		"workspace_name", cfg.Workspace.Name,
		"backend", cfg.CLI.Backend,
		"model", cfg.CLI.Model,
		"max_procs", cfg.Session.MaxProcs,
		"platforms", len(platforms),
	)
	// Operators copy these URLs into the IM console; WS-only platforms omitted.
	logWebhookEndpoints(cfg, platforms)

	if cfg.Server.DashboardToken == "" {
		slog.Warn("dashboard_token is not set — dashboard and WebSocket API are accessible without authentication. Set server.dashboard_token in config.yaml for production use.")
	} else if len(cfg.Server.DashboardToken) < 8 {
		slog.Error("dashboard_token is too short — use at least 8 characters")
		os.Exit(1)
	} else if len(cfg.Server.DashboardToken) < 16 {
		slog.Warn("dashboard_token is short — consider using 16+ random characters for stronger security")
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Start(ctx)
	}()

	startWatchdogLoop(ctx, router.HealthCheck)

	// Best-effort and error-swallowing so a failed check never disturbs the
	// gateway; dev builds self-skip.
	startUpdateChecker(ctx, updateChecker)

	metrics.StartupPhaseReadyMs.Set(time.Since(t0).Milliseconds())

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
