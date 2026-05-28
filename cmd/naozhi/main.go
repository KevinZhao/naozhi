package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	discordplatform "github.com/naozhi/naozhi/internal/platform/discord"
	"github.com/naozhi/naozhi/internal/platform/feishu"
	slackplatform "github.com/naozhi/naozhi/internal/platform/slack"
	weixinplatform "github.com/naozhi/naozhi/internal/platform/weixin"
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

	// Setup logging
	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var handler slog.Handler
	if cfg.Log.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(handler))

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
	settingsFile := writeClaudeSettingsOverride(ctx, cfg.Server.Addr)

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
	// to initBackendWrappers. RefreshSettings closes over cfg.Server.Addr so
	// every spawn regenerates ~/.naozhi/claude-settings.json from the live
	// ~/.claude/settings.json. Without this, edits made after naozhi start
	// (adding ANTHROPIC_BEDROCK_BASE_URL, swapping models, etc.) are
	// invisible to dashboard / cron / IM-spawned sessions until restart.
	// claude profile copies these into its own ProtocolDeps; kiro profile
	// ignores them (and Sprint 6a seeds BackendID="kiro" inside the kiro
	// profile factory itself).
	serverAddr := cfg.Server.Addr
	bws, ok := initBackendWrappers(ctx, cfg, shimMgr, settingsFile, func() string {
		return writeClaudeSettingsOverride(ctx, serverAddr)
	})
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

	// Build agent opts from config — kept as session.AgentOpts so the
	// router-side spawn path uses the operator-trusted shape; cron's
	// scheduler receives a translated view via toCronAgentOpts (see
	// cron_router_adapter.go) so internal/cron does not import session.
	agents := make(map[string]session.AgentOpts)
	for id, ac := range cfg.Agents {
		agents[id] = session.AgentOpts{
			Model:     ac.Model,
			ExtraArgs: ac.Args,
		}
	}
	cronAgents := make(map[string]cron.AgentOpts, len(agents))
	for id, a := range agents {
		cronAgents[id] = toCronAgentOpts(a)
	}

	// Validate agent_commands reference existing agents
	for cmd, agentID := range cfg.AgentCommands {
		if _, ok := agents[agentID]; !ok {
			slog.Error("agent_commands references undefined agent", "command", cmd, "agent", agentID)
			os.Exit(1)
		}
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
			m, wd, e := buildSysessionManager(cfg, router, wrapper, storePath)
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
			conn.SetDiscoverFunc(func() (json.RawMessage, error) {
				pids, sids, cwds := router.ManagedExcludeSets()
				sessions, err := discovery.Scan(claudeDir, pids, sids, cwds)
				if err != nil {
					return json.Marshal([]any{})
				}
				if sessions == nil {
					sessions = []discovery.DiscoveredSession{}
				}
				if projectMgr != nil && len(sessions) > 0 {
					cwds := make([]string, len(sessions))
					for i, d := range sessions {
						cwds[i] = d.CWD
					}
					cwdMap := projectMgr.ResolveWorkspaces(cwds)
					for i := range sessions {
						sessions[i].Project = cwdMap[sessions[i].CWD]
					}
				}
				return json.Marshal(sessions)
			})
			conn.SetPreviewFunc(func(sessionID string) (json.RawMessage, error) {
				entries, err := discovery.LoadHistory(claudeDir, sessionID, "")
				if err != nil {
					return json.Marshal([]cli.EventEntry{})
				}
				if entries == nil {
					entries = []cli.EventEntry{}
				}
				return json.Marshal(entries)
			})
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

	// Systemd watchdog: periodically signal liveness so WatchdogSec can detect hangs.
	// Always send WATCHDOG=1 unconditionally — its purpose is OS-level liveness.
	// The HealthCheck (TryRLock) result is logged as a diagnostic signal only;
	// it must not suppress the heartbeat since normal write-lock activity
	// (cleanup, spawn) would cause false negatives.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !router.HealthCheck() {
					slog.Warn("router mutex contended at watchdog tick")
				}
				_ = osutil.SdNotify("WATCHDOG=1")
			}
		}
	}()

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

// initPlatforms wires each configured IM platform adapter into a map.
// Extracted from main() for testability + readability (CQ1). Callers
// still own lifecycle — initPlatforms neither starts goroutines nor
// touches globals; it just constructs the adapters and returns them.
// The transcribe service is threaded through so Feishu can accept voice
// messages; other adapters do not need it today.
func initPlatforms(cfg *config.Config, stt transcribe.Service) (map[string]platform.Platform, error) {
	platforms := make(map[string]platform.Platform)
	if cfg.Platforms.Feishu != nil {
		f := feishu.New(feishu.Config{
			AppID:             cfg.Platforms.Feishu.AppID,
			AppSecret:         cfg.Platforms.Feishu.AppSecret,
			ConnectionMode:    cfg.Platforms.Feishu.ConnectionMode,
			VerificationToken: cfg.Platforms.Feishu.VerificationToken,
			EncryptKey:        cfg.Platforms.Feishu.EncryptKey,
			MaxReplyLen:       cfg.Platforms.Feishu.MaxReplyLength,
		}, stt)
		platforms["feishu"] = f
	}
	if cfg.Platforms.Slack != nil {
		s := slackplatform.New(slackplatform.Config{
			BotToken:    cfg.Platforms.Slack.BotToken,
			AppToken:    cfg.Platforms.Slack.AppToken,
			MaxReplyLen: cfg.Platforms.Slack.MaxReplyLength,
		})
		platforms["slack"] = s
	}
	if cfg.Platforms.Discord != nil {
		d := discordplatform.New(discordplatform.Config{
			BotToken:    cfg.Platforms.Discord.BotToken,
			MaxReplyLen: cfg.Platforms.Discord.MaxReplyLength,
		})
		platforms["discord"] = d
	}
	if cfg.Platforms.Weixin != nil {
		wx := weixinplatform.New(weixinplatform.Config{
			Token:       cfg.Platforms.Weixin.Token,
			BaseURL:     cfg.Platforms.Weixin.BaseURL,
			MaxReplyLen: cfg.Platforms.Weixin.MaxReplyLength,
		})
		platforms["weixin"] = wx
	}
	return platforms, nil
}

// parseDurationOrDefault parses a duration string, returning def on empty or error.
func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// parseBytesOrDefault parses a human-readable byte size string (e.g. "50MB", "1GB").
// Returns def on empty or unrecognized format.
func parseBytesOrDefault(s string, def int64) int64 {
	if s == "" {
		return def
	}
	s = strings.TrimSpace(s)
	s = strings.ToUpper(s)

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return def
	}
	return n * multiplier
}

// stateDirWarnMB is the soft ceiling for ~/.naozhi/ total size; see
// docs/ops/disk-budget.md. RNEW-OPS-415 tracks quota enforcement.
const stateDirWarnMB = 500

// warnIfStateDirLarge walks stateDir once at startup and warns if total
// bytes exceed stateDirWarnMB. First-run / permission errors are silent;
// a truncated scan still warns using the partial total as a lower bound.
func warnIfStateDirLarge(stateDir string) {
	if stateDir == "" || stateDir == "." {
		return
	}
	bytes, err := osutil.StateDirSize(stateDir)
	truncated := errors.Is(err, osutil.ErrStateDirScanTruncated)
	if err != nil && !truncated {
		return
	}
	sizeMB := bytes / (1024 * 1024)
	if sizeMB < stateDirWarnMB {
		return
	}
	slog.Warn("state directory large",
		"path", stateDir, "size_mb", sizeMB, "threshold_mb", stateDirWarnMB,
		"truncated", truncated,
		"hint", "prune attachments/events; see docs/ops/disk-budget.md")
}

// chatIDSuffix returns the last 8 characters of a chat ID for logging,
// prefixed with "…" so a grep on full IDs does not match. Empty input
// returns an empty string. Kept local to this file since it is log-only
// and does not need to round-trip.
func chatIDSuffix(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-8:]
}

// logWebhookEndpoints prints a one-line summary of the webhook URLs operators
// need to paste into the IM vendor console. Platforms that do not expose a
// webhook route (e.g. feishu websocket mode) are skipped.
func logWebhookEndpoints(cfg *config.Config, platforms map[string]platform.Platform) {
	addr := cfg.Server.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "0.0.0.0" + addr
	}
	for name := range platforms {
		switch name {
		case "feishu":
			if cfg.Platforms.Feishu != nil && cfg.Platforms.Feishu.ConnectionMode == "webhook" {
				slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/feishu", "addr", addr)
			}
		case "slack":
			// slack events api + socket mode: route is only exposed when not using socket mode
			if cfg.Platforms.Slack != nil && cfg.Platforms.Slack.AppToken == "" {
				slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/slack", "addr", addr)
			}
		case "weixin":
			slog.Info("platform webhook endpoint", "platform", name, "path", "/webhook/weixin", "addr", addr)
		}
	}
}

// buildSysessionManager wires sysession.Manager from cfg.Sysession.
//
// Returns (nil, nil) when the framework is disabled — that's the
// happy path for deployments that don't want background daemons yet.
//
// Returns (nil, err) when enabled but unusable (e.g. work dir cannot
// be chmodded 0700, default backend has no binary path).  Caller
// should log the error and proceed without daemons rather than
// aborting startup — sysession is opt-in infrastructure, not a
// release-critical path.
//
// Step 11 will replace the nil OnRunStarted/OnRunEnded with WS-hub
// callbacks; Phase 1 ships without them so the dashboard reads fall
// back to polling /api/system/daemons.
func buildSysessionManager(cfg *config.Config, router *session.Router,
	defaultWrapper *cli.Wrapper, storePath string,
) (*sysession.Manager, string, error) {
	if !cfg.Sysession.Enabled {
		// Return nil rather than a no-op Manager so the caller's nil
		// guard is meaningful — main.go's Start/Stop loops both check
		// `if sysMgr != nil`, and a stubbed always-non-nil result would
		// turn that into dead code.
		return nil, "", nil
	}

	// Resolve work dir: explicit override first, then a sibling of
	// sessions.json (= dataDir/sys-sessions/).  Empty storePath means
	// the operator opted out of state persistence; fall back to ~/.naozhi
	// to keep the directory under user control.
	workDir := osutil.ExpandHome(cfg.Sysession.Runner.WorkDir)
	if workDir == "" {
		base := filepath.Dir(storePath)
		if base == "" || base == "." {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".naozhi")
		}
		workDir = filepath.Join(base, "sys-sessions")
	}
	resolvedWorkDir, err := sysession.EnsureWorkDir(workDir)
	if err != nil {
		return nil, "", fmt.Errorf("ensure sys-sessions dir: %w", err)
	}

	// Startup sweep — non-fatal; a busted directory should not block
	// daemon startup.  Default 7 days when unset; "0" disables.
	jsonlMaxAge := 7 * 24 * time.Hour
	if v := cfg.Sysession.Runner.JSONLMaxAge; v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			slog.Warn("sysession: bad jsonl_max_age; using default 7d", "err", err, "value", v)
		} else {
			jsonlMaxAge = parsed
		}
	}
	if _, err := sysession.SweepOldJSONL(resolvedWorkDir, jsonlMaxAge); err != nil {
		slog.Warn("sysession: startup sweep failed", "err", err, "dir", resolvedWorkDir)
	}

	// Build Runner from the default backend's binary.
	binPath := ""
	if defaultWrapper != nil {
		binPath = defaultWrapper.CLIPath
	}
	runner, err := sysession.NewRunner(sysession.RunnerConfig{
		BinPath: binPath,
		WorkDir: resolvedWorkDir,
		Model:   cfg.Sysession.Runner.Model,
		// claude -p needs the same Bedrock / Anthropic / proxy plumbing
		// the main session-spawn path uses (applyClaudeEnvSettings
		// pre-populated naozhi's own os.Environ from
		// ~/.claude/settings.json at startup).  Trailing underscore =
		// prefix match, see internal/sysession/env.go's filterEnv.
		// AWS_ is bounded by the same denylist filterClaudeEnv uses for
		// the parent — auth-source vars never make it into naozhi's
		// env in the first place.
		EnvAllowlist: []string{
			"ANTHROPIC_",
			"CLAUDE_",
			"AWS_",
			"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "no_proxy",
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("new runner: %w", err)
	}

	tickTimeout := 30 * time.Second
	if v := cfg.Sysession.TickTimeout; v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			slog.Warn("sysession: bad tick_timeout; using default 30s", "err", err, "value", v)
		} else {
			tickTimeout = parsed
		}
	}

	// Translate per-daemon configs.
	daemons := make(map[string]sysession.DaemonRuntimeConfig, len(cfg.Sysession.Daemons))
	for name, dcfg := range cfg.Sysession.Daemons {
		tick := 30 * time.Second
		if dcfg.Tick != "" {
			parsed, err := time.ParseDuration(dcfg.Tick)
			if err != nil {
				slog.Warn("sysession: bad daemon tick; using default 30s",
					"daemon", name, "err", err, "value", dcfg.Tick)
			} else {
				tick = parsed
			}
		}
		specific := sysession.DaemonConfig{}
		if dcfg.MinUserTurns > 0 {
			specific["min_user_turns"] = dcfg.MinUserTurns
		}
		if dcfg.MinRenameInterval != "" {
			parsed, err := time.ParseDuration(dcfg.MinRenameInterval)
			if err != nil {
				slog.Warn("sysession: bad min_rename_interval",
					"daemon", name, "err", err, "value", dcfg.MinRenameInterval)
			} else {
				specific["min_rename_interval"] = parsed
			}
		}
		if dcfg.BatchPerTick > 0 {
			specific["batch_per_tick"] = dcfg.BatchPerTick
		}
		specific["include_group_chat"] = dcfg.IncludeGroupChat
		daemons[name] = sysession.DaemonRuntimeConfig{
			Enabled:  dcfg.Enabled,
			Tick:     tick,
			Specific: specific,
		}
	}

	mgr, err := sysession.NewManager(sysession.Config{
		Enabled:     true,
		TickTimeout: tickTimeout,
		Runner:      runner,
		Router:      router,
		Daemons:     daemons,
		// OnRunStarted/OnRunEnded are wired in Step 11 (WS broadcast).
	})
	if err != nil {
		return nil, "", fmt.Errorf("new manager: %w", err)
	}
	return mgr, resolvedWorkDir, nil
}
