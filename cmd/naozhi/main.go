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
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	discordplatform "github.com/naozhi/naozhi/internal/platform/discord"
	"github.com/naozhi/naozhi/internal/platform/feishu"
	slackplatform "github.com/naozhi/naozhi/internal/platform/slack"
	weixinplatform "github.com/naozhi/naozhi/internal/platform/weixin"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/transcribe"
	"github.com/naozhi/naozhi/internal/upstream"
)

var version = "dev"

// applyClaudeEnvSettings reads ~/.claude/settings.json and applies any env section
// to the current process so spawned CC child processes inherit them via os.Environ().
// Only sets vars not already present (shell-set vars take precedence).
func applyClaudeEnvSettings() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(data, &s) != nil || len(s.Env) == 0 {
		return
	}
	for k, v := range s.Env {
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v) //nolint:errcheck
		}
	}
}

// writeClaudeSettingsOverride generates ~/.naozhi/claude-settings.json by copying
// ~/.claude/settings.json verbatim, but filtering out only the hook entries that
// would call back into naozhi (causing infinite loops). Safe hooks such as
// formatters and linters are preserved as-is.
// Returns the file path, or empty string on failure.
func writeClaudeSettingsOverride(serverAddr string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	var settings map[string]json.RawMessage
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = make(map[string]json.RawMessage)
	}

	port := addrPort(serverAddr)
	if hooksRaw, ok := settings["hooks"]; ok {
		settings["hooks"] = filterHooks(hooksRaw, port)
	}

	out, err := json.Marshal(settings)
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".naozhi")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	path := filepath.Join(dir, "claude-settings.json")
	if err := os.WriteFile(path, out, 0600); err != nil {
		return ""
	}
	return path
}

// addrPort extracts the port number string from a listen address like ":8180" or "0.0.0.0:8180".
func addrPort(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

// filterHooks returns hooksRaw with any individual hook entries that would call back
// into naozhi removed. It works at the entry level: a group loses only its dangerous
// entries; if all entries in a group are removed the whole group is dropped.
// If parsing fails, returns an empty hooks object to be safe.
func filterHooks(hooksRaw json.RawMessage, serverPort string) json.RawMessage {
	// hooks shape: map[eventName] → []{ "matcher":..., "hooks": []{ "type":..., "command":... } }
	var byEvent map[string][]map[string]json.RawMessage
	if err := json.Unmarshal(hooksRaw, &byEvent); err != nil {
		empty, _ := json.Marshal(map[string]any{})
		return empty
	}

	changed := false
	for eventName, groups := range byEvent {
		var keptGroups []map[string]json.RawMessage
		for _, group := range groups {
			entriesRaw, ok := group["hooks"]
			if !ok {
				keptGroups = append(keptGroups, group)
				continue
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(entriesRaw, &entries); err != nil {
				keptGroups = append(keptGroups, group)
				continue
			}
			var safeEntries []map[string]json.RawMessage
			for _, e := range entries {
				var cmd string
				if raw, ok := e["command"]; ok {
					_ = json.Unmarshal(raw, &cmd)
				}
				if isNaozhiCallbackHook(cmd, serverPort) {
					changed = true
					slog.Info("dropping hook to prevent naozhi callback loop", "event", eventName, "command", cmd)
				} else {
					safeEntries = append(safeEntries, e)
				}
			}
			if len(safeEntries) == 0 {
				changed = true
				continue // drop group entirely
			}
			if len(safeEntries) != len(entries) {
				changed = true
				newRaw, _ := json.Marshal(safeEntries)
				group["hooks"] = newRaw
			}
			keptGroups = append(keptGroups, group)
		}
		byEvent[eventName] = keptGroups
	}

	if !changed {
		return hooksRaw
	}
	out, _ := json.Marshal(byEvent)
	return out
}

// isNaozhiCallbackHook reports whether a hook command appears to call back into
// naozhi's HTTP server (which would cause an infinite loop).
// It matches: any mention of "naozhi", or an HTTP call to localhost/127.0.0.1 on
// naozhi's listen port.
func isNaozhiCallbackHook(cmd, port string) bool {
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	if strings.Contains(lower, "naozhi") {
		return true
	}
	if port != "" {
		for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0"} {
			if strings.Contains(lower, host+":"+port) {
				return true
			}
		}
	}
	return false
}

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
		}
	}

	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

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

	// CLI Protocol + Wrapper
	applyClaudeEnvSettings()
	settingsFile := writeClaudeSettingsOverride(cfg.Server.Addr)
	var proto cli.Protocol
	switch cfg.CLI.Backend {
	case "kiro":
		proto = &cli.ACPProtocol{}
	default:
		proto = &cli.ClaudeProtocol{SettingsFile: settingsFile}
	}
	wrapper := cli.NewWrapper(cfg.CLI.Path, proto, cfg.CLI.Backend)

	// Initialize ShimManager
	shimMgr := shim.NewManager(shim.ManagerConfig{
		StateDir:        config.ExpandHome(cfg.Session.Shim.StateDir),
		CLIPath:         wrapper.CLIPath,
		IdleTimeout:     parseDurationOrDefault(cfg.Session.Shim.IdleTimeout, 4*time.Hour),
		WatchdogTimeout: parseDurationOrDefault(cfg.Session.Shim.WatchdogTimeout, 30*time.Minute),
		BufferSize:      cfg.Session.Shim.BufferSize,
		MaxBufBytes:     parseBytesOrDefault(cfg.Session.Shim.MaxBufferBytes, 50*1024*1024),
		MaxShims:        cfg.Session.Shim.MaxShims,
	})
	wrapper.ShimManager = shimMgr

	// Parse watchdog and store path
	noOutputTimeout, totalTimeout := cfg.ParseWatchdog()
	storePath := config.ExpandHome(cfg.Session.StorePath)
	workspace := config.ExpandHome(cfg.Session.CWD)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		slog.Error("create workspace dir", "path", workspace, "err", err)
		os.Exit(1)
	}

	// Session Router
	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	router := session.NewRouter(session.RouterConfig{
		Wrapper:         wrapper,
		MaxProcs:        cfg.Session.MaxProcs,
		TTL:             cfg.ParseTTL(),
		Model:           cfg.CLI.Model,
		ExtraArgs:       cfg.CLI.Args,
		Workspace:       workspace,
		StorePath:       storePath,
		NoOutputTimeout: noOutputTimeout,
		TotalTimeout:    totalTimeout,
		ClaudeDir:       claudeDir,
	})

	// Reconnect to surviving shim processes from previous naozhi run
	router.ReconnectShims()

	// Context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start cleanup loop
	router.StartCleanupLoop(ctx, cfg.ParseTTL()/2)

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
			root := config.ExpandHome(cfg.Projects.Root)
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

	if len(platforms) == 0 {
		slog.Warn("no platforms configured, running in dashboard-only mode")
	}

	// Build agent opts from config
	agents := make(map[string]session.AgentOpts)
	for id, ac := range cfg.Agents {
		agents[id] = session.AgentOpts{
			Model:     ac.Model,
			ExtraArgs: ac.Args,
		}
	}

	// Validate agent_commands reference existing agents
	for cmd, agentID := range cfg.AgentCommands {
		if _, ok := agents[agentID]; !ok {
			slog.Error("agent_commands references undefined agent", "command", cmd, "agent", agentID)
			os.Exit(1)
		}
	}

	// Cron Scheduler
	scheduler := cron.NewScheduler(cron.SchedulerConfig{
		Router:        router,
		Platforms:     platforms,
		Agents:        agents,
		AgentCommands: cfg.AgentCommands,
		StorePath:     config.ExpandHome(cfg.Cron.StorePath),
		MaxJobs:       cfg.Cron.MaxJobs,
		ExecTimeout:   cfg.ParseExecutionTimeout(),
	})
	if err := scheduler.Start(); err != nil {
		slog.Error("start cron scheduler", "err", err)
		os.Exit(1)
	}

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
		rns = node.NewReverseServer(cfg.ReverseNodes)
		slog.Info("reverse node auth configured", "nodes", len(cfg.ReverseNodes))
	}

	// Server
	srv := server.New(cfg.Server.Addr, router, platforms, agents, cfg.AgentCommands, scheduler, cfg.CLI.Backend, server.ServerOptions{
		WorkspaceID:       cfg.Workspace.ID,
		WorkspaceName:     cfg.Workspace.Name,
		AllowedRoot:       workspace,
		NoOutputTimeout:   noOutputTimeout,
		TotalTimeout:      totalTimeout,
		DashboardToken:    cfg.Server.DashboardToken,
		ProjectManager:    projectMgr,
		Nodes:             nodes,
		ReverseNodeServer: rns,
		Transcriber:       stt,
	})

	// Start upstream connector (this node connects to a primary)
	if cfg.Upstream != nil {
		conn := upstream.New(cfg.Upstream, router, projectMgr)
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
				entries, err := discovery.LoadHistory(claudeDir, sessionID)
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

	// Graceful shutdown
	shutdownDone := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("received signal", "signal", sig)
		cancel()
		scheduler.Stop()
		router.Shutdown()
		close(shutdownDone)
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

	if cfg.Server.DashboardToken == "" {
		slog.Warn("dashboard_token is not set — dashboard and WebSocket API are accessible without authentication. Set server.dashboard_token in config.yaml for production use.")
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Start(ctx)
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	case <-shutdownDone:
	}
	// Wait for both server and shutdown to complete
	<-shutdownDone
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
