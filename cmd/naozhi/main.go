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

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/transcribe"
	"github.com/naozhi/naozhi/internal/upstream"

	// R239-ARCH-B: side-effect import for history-source factory
	// registration. Replaces the blank-imports that previously lived
	// inside internal/session/router_core.go; importing wireup here
	// keeps internal/session backend-agnostic and centralizes the
	// per-backend init() trigger list in one explicit place.
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

	// t0 anchors every startup phase gauge (RNEW-OPS-414). Captured after
	// the subcommand dispatch so setup/install/doctor invocations do not
	// pollute the naozhi boot histogram.
	t0 := time.Now()

	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	metrics.StartupPhaseConfigMs.Set(time.Since(t0).Milliseconds())

	// Setup logging (resolveLogLevel + newLogHandler in main_init.go).
	setupLogging(cfg)

	// Context with cancellation for graceful shutdown. Created here (before
	// applyClaudeEnvSettings) so retry sleeps in readJSONWithRetry respond to
	// ctx.Done() from the very first use of the settings file.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// CLI Protocol + Wrapper
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
	// docs/rfc/direct-user-settings.md PR1: naozhi-spawned cc now loads
	// ~/.claude/settings.json directly via `--setting-sources user` (set in
	// cli.ClaudeProtocol.BuildArgs). No settings-override copy is generated;
	// the parent-process env injection above (applyClaudeEnvSettings) is the
	// only remaining settings.json consumer in main (it feeds transcribe +
	// sysession Runner Bedrock auth, see RFC §7.1).
	slog.Info("claude settings: loading user settings directly", "mode", "user")

	// Register the cli/backend.Profile registry with the built-in profiles
	// (claude + kiro) before any consumer (discovery, main, server) looks
	// up DisplayName / DefaultTag / DetectInProc by id. Explicit, not init()-
	// driven, so missing imports fail loudly. docs/rfc/multi-backend.md §3.
	backend.RegisterDefaults()

	// CQ1 (#396): config validation diag fan-out extracted to
	// logConfigValidationDiagnostics so a future format change is
	// unit-testable. docs/rfc/multi-backend.md §11.1 fail-soft posture
	// preserved — error-level diags do NOT abort startup.
	logConfigValidationDiagnostics(cfg)

	// Shared shim manager across all backends — every shim records its own
	// Backend in state, so reconnect routing is backend-aware without
	// needing per-backend state directories.
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

	// CQ1 (#396): backend wrapper construction + default selection extracted
	// to initBackendWrappers. Post direct-user-settings PR1 there is no
	// settings-override path to plumb: the claude profile spawns cc with
	// `--setting-sources user` so live edits to ~/.claude/settings.json are
	// re-read by cc on every spawn with no naozhi involvement.
	bws, ok := initBackendWrappers(ctx, cfg, shimMgr)
	if !ok {
		if bws.Default == nil {
			slog.Error("no usable cli backend configured")
		} else {
			// Default backend's --version probe failed. R55-QUAL-001:
			// surface the operator-actionable hint so the journalctl line
			// points at the config field they need to fix instead of just
			// saying "spawn failed" on the first user message.
			slog.Error("default cli backend is unavailable",
				"id", bws.Default.BackendID, "path", bws.Default.CLIPath,
				"hint", "fix the binary path in cli.backends or set cli.default to an available backend")
		}
		os.Exit(1)
	}
	wrappers := bws.Wrappers
	backendModels := bws.Models
	backendExtraArgs := bws.ExtraArgs
	defaultBackend := bws.DefaultID
	wrapper := bws.Default

	// Parse watchdog and store path
	noOutputTimeout, totalTimeout := cfg.ParseWatchdog()
	storePath := osutil.ExpandHome(cfg.Session.StorePath)
	workspace := osutil.ExpandHome(cfg.Session.CWD)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		slog.Error("create workspace dir", "path", workspace, "err", err)
		os.Exit(1)
	}
	warnIfStateDirLarge(filepath.Dir(storePath))

	// Session Router
	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	// Event-log persistence directory sits next to sessions.json so
	// operators can co-locate state. Empty StorePath (test harnesses)
	// disables the event log persister via the same empty-string
	// guard inside NewRouter.
	eventLogDir := ""
	if storePath != "" {
		eventLogDir = filepath.Join(filepath.Dir(storePath), "events")
	}
	// Auto-workspace-chain policy: defaults to enabled=true / window=7d /
	// cap=32 per docs/rfc/auto-workspace-chain.md. Operators can disable
	// or tune via session.auto_chain in config.yaml.
	autoChainPolicy := session.GlobalAutoChainPolicy{
		EnabledFlag: cfg.Session.AutoChain.ResolvedEnabled(true),
		WindowDur:   time.Duration(cfg.Session.AutoChain.ResolvedWindowHours(7*24)) * time.Hour,
		CapValue:    cfg.Session.AutoChain.ResolvedCap(32),
	}
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
		Workspace:        workspace,
		StorePath:        storePath,
		NoOutputTimeout:  noOutputTimeout,
		TotalTimeout:     totalTimeout,
		ClaudeDir:        claudeDir,
		// KiroSessionsDir feeds the kirojsonl history factory so
		// Sprint 1c "load earlier" pages can fall back to the kiro
		// CLI's per-session jsonl after naozhi restart. Default path
		// is the kiro CLI's documented location; a config override is
		// a follow-up sprint.
		KiroSessionsDir:   osutil.ExpandHome("~/.kiro/sessions/cli"),
		EventLogDir:       eventLogDir,
		EventLogGenerator: "naozhi",
		AutoChainPolicy:   autoChainPolicy,
	})
	metrics.StartupPhaseRouterMs.Set(time.Since(t0).Milliseconds())

	// Reconnect to surviving shim processes from previous naozhi run
	router.ReconnectShimsCtx(ctx)
	metrics.StartupPhaseShimReconnectMs.Set(time.Since(t0).Milliseconds())

	// Start cleanup loop
	router.StartCleanupLoop(ctx, cfg.ParseTTL()/2)

	// Periodically reconcile shim liveness (reconnect dropped connections)
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
			})
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

	// Register platforms
	platforms, err := initPlatforms(cfg, stt)
	if err != nil {
		slog.Error("init platforms failed", "err", err)
		os.Exit(1)
	}

	if len(platforms) == 0 {
		slog.Warn("no platforms configured, running in dashboard-only mode")
	}

	// Build agent opts from config (buildAgentOpts in main_init.go) — the
	// session.AgentOpts map is the operator-trusted shape used by the
	// router-side spawn path; cronAgents is the internal/cron-import-free
	// translation via toCronAgentOpts (see cron_router_adapter.go).
	agents, cronAgents := buildAgentOpts(cfg)

	// Validate agent_commands reference existing agents.
	if cmd, ok := firstUndefinedAgentCommand(cfg.AgentCommands, agents); !ok {
		slog.Error("agent_commands references undefined agent",
			"command", cmd, "agent", cfg.AgentCommands[cmd])
		os.Exit(1)
	}
	metrics.StartupPhasePlatformsMs.Set(time.Since(t0).Milliseconds())

	// Cron + sysession orchestration moved into internal/wireup.WireSchedulers
	// (#1031 R240-ARCH-12). main.go retains:
	//   - notifyDefault configured-log (operator-facing visibility)
	//   - StartupPhaseSchedulerMs metric (wireup pkg has no
	//     internal/metrics dependency)
	//   - sysession build error logging (wireup returns nil-Sysession on
	//     build failure; main slog.Warn matches existing degraded-mode
	//     contract)
	cronLoc := cfg.ParseCronTimezone()
	slog.Info("cron timezone", "location", cronLoc.String())
	if cfg.Cron.NotifyDefault.Platform != "" && cfg.Cron.NotifyDefault.ChatID != "" {
		// Log only platform and truncated chat_id suffix so log aggregators
		// don't carry the full group/user identifier.
		slog.Info("cron notify default configured",
			"platform", cfg.Cron.NotifyDefault.Platform,
			"chat_id_suffix", chatIDSuffix(cfg.Cron.NotifyDefault.ChatID))
	}
	var sysBuildErr error
	schedulers, err := wireup.WireSchedulers(wireup.SchedulersDeps{
		Cfg:                  cfg,
		Router:               router,
		SessionRouterAdapter: cronRouterAdapter{r: router},
		Platforms:            platforms,
		Agents:               cronAgents,
		Workspace:            workspace,
		CronStorePath:        osutil.ExpandHome(cfg.Cron.StorePath),
		ParentCtx:            ctx,
		Telemetry:            nil, // wired post-Hub via dashboard.go SetTelemetry
		BuildSysession: func() (*sysession.Manager, string, error) {
			m, wd, e := buildSysessionManager(cfg, router, projectMgr, wrapper, storePath)
			sysBuildErr = e // capture for slog below
			return m, wd, e
		},
	})
	if err != nil {
		slog.Error("start cron scheduler", "err", err)
		os.Exit(1)
	}
	if sysBuildErr != nil {
		slog.Warn("sysession manager unavailable; daemons disabled", "err", sysBuildErr)
	}
	scheduler := schedulers.Cron
	sysMgr := schedulers.Sysession
	sysWorkDir := schedulers.SysessionWorkDir
	metrics.StartupPhaseSchedulerMs.Set(time.Since(t0).Milliseconds())

	// Configure remote nodes for multi-node aggregation
	var nodes map[string]node.Conn
	if len(cfg.Nodes) > 0 {
		nodes = make(map[string]node.Conn, len(cfg.Nodes))
		for id, nc := range cfg.Nodes {
			nodes[id] = node.NewHTTPClient(id, nc.URL, nc.Token, nc.DisplayName)
		}
		slog.Info("multi-node configured", "nodes", len(nodes))
	}

	// Configure reverse-connecting nodes (NAT traversal)
	var rns *node.ReverseServer
	if len(cfg.ReverseNodes) > 0 {
		rns = node.NewReverseServer(cfg.ReverseNodes, cfg.Server.TrustedProxy)
		slog.Info("reverse node auth configured", "nodes", len(cfg.ReverseNodes))
	}

	// Server
	srv := server.NewWithOptions(server.ServerOptions{
		Addr:              cfg.Server.Addr,
		Router:            router,
		Platforms:         platforms,
		Agents:            agents,
		AgentCommands:     cfg.AgentCommands,
		Scheduler:         scheduler,
		Backend:           cfg.CLI.Backend,
		WorkspaceID:       cfg.Workspace.ID,
		WorkspaceName:     cfg.Workspace.Name,
		AllowedRoot:       workspace,
		StateDir:          filepath.Dir(storePath),
		NoOutputTimeout:   noOutputTimeout,
		TotalTimeout:      totalTimeout,
		QueueMaxDepth:     cfg.QueueMaxDepth(),
		QueueCollectDelay: cfg.ParseCollectDelay(),
		QueueMode:         cfg.QueueMode(),
		DashboardToken:    cfg.Server.DashboardToken,
		TrustedProxy:      cfg.Server.TrustedProxy,
		ProjectManager:    projectMgr,
		Nodes:             nodes,
		ReverseNodeServer: rns,
		Transcriber:       stt,
		StartupCtx:        ctx,
		Version:           version,
		SysessionManager:  sysMgr,
		SysWorkDir:        sysWorkDir,
		OnReady: func() {
			if err := osutil.SdNotify("READY=1"); err != nil {
				slog.Warn("sd_notify READY failed", "err", err)
			}
		},
	})
	metrics.StartupPhaseServerMs.Set(time.Since(t0).Milliseconds())

	// Start upstream connector (this node connects to a primary)
	if cfg.Upstream != nil {
		// Build a KeyResolver for the connector so reverse-RPC planner
		// restart (#7) goes through the same ResolveForPlannerKey path
		// as the dashboard HTTP handler (#6). Independent instance from
		// the server's resolver — the agents map and project data are
		// the same source of truth, but wiring through main.go avoids
		// coupling upstream to the server package.
		upstreamResolver := session.NewKeyResolver(agents, project.NewDataSource(projectMgr))
		conn := upstream.New(cfg.Upstream, router, projectMgr, upstreamResolver)
		if claudeDir != "" {
			// Discover/preview closures extracted to main_upstream.go so the
			// scan-exclude + project-resolve + JSON-fallback logic is testable
			// in isolation (R237-ARCH-8 / #590).
			conn.SetDiscoverFunc(newUpstreamDiscoverFunc(claudeDir, router, projectMgr))
			conn.SetPreviewFunc(newUpstreamPreviewFunc(claudeDir))
		}
		go conn.Run(ctx)
		slog.Info("upstream connector starting", "url", cfg.Upstream.URL, "node_id", cfg.Upstream.NodeID)
	}

	// Graceful shutdown. runShutdown is idempotent via shutdownOnce so both the
	// signal path and the spontaneous server-exit path (see select below) run it
	// exactly once. Without this guard, a srv.Start error exit would skip
	// scheduler.Stop()/router.Shutdown() and drop the last cron snapshot + leak
	// shim state; conversely a clean server exit without a signal would
	// deadlock on <-shutdownDone.
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
			// R245-ARCH-38 (#893): emit per-phase timing at shutdown so a
			// hung subsystem is attributable from logs alone (operator can
			// grep `phase=` in journalctl output without an external metric
			// store). The sysMgr → scheduler → router order is a contract
			// (see comments below) — each phase is intentionally serial,
			// not topo-sort-derived, because the ordering is encoded in
			// upstream callgraphs that a runtime sort cannot infer.
			shutdownT0 := time.Now()
			slog.Info("shutdown starting", "reason", reason)
			if err := osutil.SdNotify("STOPPING=1"); err != nil {
				slog.Warn("sd_notify STOPPING failed", "err", err)
			}
			cancel()
			// Sysession Manager must stop FIRST: daemon Tick paths call into
			// router (VisitSessions / SetUserLabelWithOrigin); leaving them
			// running while Scheduler.Stop or Router.Shutdown tear down
			// downstream state would race.  Manager.Stop is hard wg.Wait
			// (RFC v2.1 §5.2) — a daemon that ignores ctx will panic the
			// process at shutdown rather than leak goroutines.  5s budget
			// is comfortable headroom for Runner subprocess teardown via
			// exec.CommandContext.
			sysT0 := time.Now()
			if sysMgr != nil {
				sysStopCtx, sysStopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				sysMgr.Stop(sysStopCtx)
				sysStopCancel()
			}
			slog.Info("shutdown phase complete", "phase", "sysmgr", "ms", time.Since(sysT0).Milliseconds())
			// Scheduler must stop fully before router.Shutdown: in-flight cron
			// jobs still call into router (GetOrCreate/Send), so tearing the
			// router down in parallel would race against those calls.
			schedT0 := time.Now()
			scheduler.Stop()
			slog.Info("shutdown phase complete", "phase", "scheduler", "ms", time.Since(schedT0).Milliseconds())
			// S11 (R194-COR): block on the real HTTP-drain barrier before
			// tearing down the router. cancel() above triggers Server.Start's
			// shutdown goroutine (srv.Shutdown 30s drain); ShutdownComplete
			// closes only after that drain returns, i.e. after every in-flight
			// GetOrCreate/Send handler has finished. Sequencing router.Shutdown
			// after this point guarantees no handler observes a half-cleaned
			// session map. On the server-error/server-exit paths Start has
			// already returned, so the channel is already closed and this is a
			// no-op. The drain has its own 30s ctx; this wait inherits that
			// bound rather than blocking forever.
			drainT0 := time.Now()
			<-srv.ShutdownComplete()
			slog.Info("shutdown phase complete", "phase", "http-drain", "ms", time.Since(drainT0).Milliseconds())
			routerT0 := time.Now()
			router.Shutdown()
			slog.Info("shutdown phase complete", "phase", "router", "ms", time.Since(routerT0).Milliseconds())
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
	// Surface the configured webhook endpoints so operators can copy the URL
	// into the IM provider console without having to grep routes. Routes for
	// WS-only platforms (feishu websocket mode) are intentionally omitted.
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

	// Systemd watchdog heartbeat (startWatchdogLoop in main_init.go).
	startWatchdogLoop(ctx, router.HealthCheck)

	metrics.StartupPhaseReadyMs.Set(time.Since(t0).Milliseconds())

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			runShutdown("server-error")
			<-shutdownDone
			os.Exit(1)
		}
		// Server exited cleanly without a signal (e.g. listener closed by
		// internal path) — still need to drain scheduler/router before return.
		runShutdown("server-exit")
		<-shutdownDone
	case <-shutdownDone:
		// Wait for HTTP server to finish draining in-flight requests
		<-serverErr
	}
}
