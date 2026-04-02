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
	"syscall"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/connector"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/pathutil"
	"github.com/naozhi/naozhi/internal/platform"
	discordplatform "github.com/naozhi/naozhi/internal/platform/discord"
	"github.com/naozhi/naozhi/internal/platform/feishu"
	slackplatform "github.com/naozhi/naozhi/internal/platform/slack"
	weixinplatform "github.com/naozhi/naozhi/internal/platform/weixin"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
)

var version = "dev"

func main() {
	// Subcommands (before flag.Parse)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			runSetup(os.Args[2:])
			return
		case "version", "--version":
			fmt.Println(version)
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	// CLI Protocol + Wrapper
	var proto cli.Protocol
	switch cfg.CLI.Backend {
	case "kiro":
		proto = &cli.ACPProtocol{}
	default:
		proto = &cli.ClaudeProtocol{}
	}
	wrapper := cli.NewWrapper(cfg.CLI.Path, proto)

	// Parse watchdog and store path
	noOutputTimeout, totalTimeout := cfg.ParseWatchdog()
	storePath := pathutil.ExpandHome(cfg.Session.StorePath)
	workspace := pathutil.ExpandHome(cfg.Session.CWD)
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

	// Context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start cleanup loop
	router.StartCleanupLoop(ctx, cfg.ParseTTL()/2)

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
		})
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
		StorePath:     pathutil.ExpandHome(cfg.Cron.StorePath),
		MaxJobs:       cfg.Cron.MaxJobs,
		ExecTimeout:   cfg.ParseExecutionTimeout(),
	})
	if err := scheduler.Start(); err != nil {
		slog.Error("start cron scheduler", "err", err)
		os.Exit(1)
	}

	// Server
	srv := server.New(cfg.Server.Addr, router, platforms, agents, cfg.AgentCommands, scheduler, cfg.CLI.Backend)
	srv.SetWorkspaceIdentity(cfg.Workspace.ID, cfg.Workspace.Name)
	srv.SetWatchdogTimeouts(noOutputTimeout, totalTimeout)

	// Project Manager (optional, enabled when projects.root is configured)
	var projectMgr *project.Manager
	if cfg.Projects.Root != "" {
		root := pathutil.ExpandHome(cfg.Projects.Root)
		mgr, err := project.NewManager(root, project.PlannerDefaults{
			Model:  cfg.Projects.PlannerDefaults.Model,
			Prompt: cfg.Projects.PlannerDefaults.Prompt,
		})
		if err != nil {
			slog.Error("init project manager", "err", err)
			os.Exit(1)
		}
		if err := mgr.Scan(); err != nil {
			slog.Error("scan projects", "err", err)
			os.Exit(1)
		}
		projectMgr = mgr
		srv.SetProjectManager(mgr)
		slog.Info("projects enabled", "root", root, "count", len(mgr.All()))
	}

	// Configure remote nodes for multi-node aggregation
	if len(cfg.Nodes) > 0 {
		nodes := make(map[string]server.NodeConn, len(cfg.Nodes))
		for id, nc := range cfg.Nodes {
			nodes[id] = server.NewNodeClient(id, nc.URL, nc.Token, nc.DisplayName)
		}
		srv.SetNodes(nodes)
		slog.Info("multi-node configured", "nodes", len(nodes))
	}

	// Configure reverse-connecting nodes (NAT traversal)
	if len(cfg.ReverseNodes) > 0 {
		rns := server.NewReverseNodeServer(cfg.ReverseNodes)
		srv.SetReverseNodeServer(rns)
		slog.Info("reverse node auth configured", "nodes", len(cfg.ReverseNodes))
	}

	// Start upstream connector (this node connects to a primary)
	if cfg.Upstream != nil {
		connCfg := &connector.UpstreamConfig{
			URL:         cfg.Upstream.URL,
			NodeID:      cfg.Upstream.NodeID,
			Token:       cfg.Upstream.Token,
			DisplayName: cfg.Upstream.DisplayName,
		}
		conn := connector.New(connCfg, router, projectMgr)
		if claudeDir != "" {
			conn.SetDiscoverFunc(func() (json.RawMessage, error) {
				excludePIDs := router.ManagedPIDs()
				sessions, err := discovery.Scan(claudeDir, excludePIDs)
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

	if err := srv.Start(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
	<-shutdownDone
}
