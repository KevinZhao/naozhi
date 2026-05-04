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
	"github.com/naozhi/naozhi/internal/transcribe"
	"github.com/naozhi/naozhi/internal/upstream"
)

var version = "dev"

// claudeEnvAllowedPrefixes restricts which env vars from
// ~/.claude/settings.json are allowed to leak into the naozhi parent process.
// Historically every key was injected, which meant arbitrary keys set by a
// third-party Claude extension would become part of naozhi's attack surface
// (and downstream shim/CLI env) with no audit. Limit to the prefixes that
// Claude CLI and its AWS/Anthropic model plumbing actually consume.
var claudeEnvAllowedPrefixes = []string{
	"ANTHROPIC_",
	"CLAUDE_",
	"AWS_",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// readClaudeSettingsRaw reads ~/.claude/settings.json and returns its raw bytes,
// retrying a few times if JSON parsing fails. The retry handles the race where
// another process (Claude CLI, a VS Code extension, etc.) is rewriting the file
// non-atomically: we may observe a truncated view, but 100ms later the writer
// has finished and we see a complete document.
//
// Returns (data, nil) on success. Returns a non-nil error if the file cannot be
// read, or if every retry yielded invalid JSON — callers must treat the error
// as "could not determine a trustworthy settings snapshot", NOT as "file is empty".
func readClaudeSettingsRaw() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	return readJSONWithRetry(path, 3, 100*time.Millisecond)
}

// readJSONWithRetry reads path and verifies the content is valid JSON. If the
// read succeeds but parsing fails, retries up to attempts-1 more times with the
// given sleep in between. If the file doesn't exist, returns the os.Open error
// immediately (no retry — missing is a different failure mode than truncated).
func readJSONWithRetry(path string, attempts int, sleep time.Duration) ([]byte, error) {
	var lastParseErr error
	for i := 0; i < attempts; i++ {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if json.Valid(data) {
			return data, nil
		}
		lastParseErr = fmt.Errorf("invalid JSON (attempt %d/%d, %d bytes)", i+1, attempts, len(data))
		if i < attempts-1 {
			time.Sleep(sleep)
		}
	}
	return nil, lastParseErr
}

// applyClaudeEnvSettings reads ~/.claude/settings.json and applies any env section
// to the current process so spawned CC child processes inherit them via os.Environ().
// Only sets vars not already present (shell-set vars take precedence) and only
// for keys matching claudeEnvAllowedPrefixes.
//
// Returns an error when the settings file cannot be read or parsed so callers
// can surface the failure. A nil return with zero env applied (e.g. no `env`
// section or all keys filtered) is NOT treated as an error.
func applyClaudeEnvSettings() error {
	data, err := readClaudeSettingsRaw()
	if err != nil {
		return err
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal env section: %w", err)
	}
	for k, v := range s.Env {
		if !matchesAnyPrefix(k, claudeEnvAllowedPrefixes) {
			continue
		}
		// R188-SEC-M1: the prefix allowlist restricts key namespace but puts
		// no constraint on the value. A malicious ~/.claude/settings.json
		// could set ANTHROPIC_BASE_URL to an attacker-controlled host or
		// inject NUL/newline into the process env that child processes
		// inherit via execve. Gate the value size + reject NUL/newline.
		if strings.ContainsAny(v, "\x00\n\r") || len(v) > 4096 {
			slog.Warn("claude settings env: rejecting unsafe value", "key", k, "len", len(v))
			continue
		}
		if _, exists := os.LookupEnv(k); !exists {
			if err := os.Setenv(k, v); err != nil {
				slog.Warn("claude settings env: setenv failed", "key", k, "err", err)
			}
		}
	}
	return nil
}

// matchesAnyPrefix reports whether s starts with any of the given prefixes.
// Prefixes ending in "_" match a namespace; prefixes without "_" match the
// full name (e.g. "HTTP_PROXY" matches only the exact env name).
func matchesAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasSuffix(p, "_") {
			if strings.HasPrefix(s, p) {
				return true
			}
		} else if s == p {
			return true
		}
	}
	return false
}

// writeClaudeSettingsOverride generates ~/.naozhi/claude-settings.json by copying
// ~/.claude/settings.json verbatim, but filtering out only the hook entries that
// would call back into naozhi (causing infinite loops). Safe hooks such as
// formatters and linters are preserved as-is.
//
// Returns the file path on success. On transient read/parse failures (common when
// Claude CLI is concurrently rewriting settings.json), RETAINS any existing
// ~/.naozhi/claude-settings.json from a prior successful run instead of overwriting
// it with `{}` — that empty file would strip the `env` block and break Bedrock auth
// for every spawned CLI process (the whole reason for --setting-sources "").
func writeClaudeSettingsOverride(serverAddr string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".naozhi")
	path := filepath.Join(dir, "claude-settings.json")

	data, err := readClaudeSettingsRaw()
	if err != nil {
		// Read/parse failed. Do NOT overwrite an existing override — the last
		// known-good copy still lets Claude CLI authenticate. Report via logs so
		// the operator notices the degraded mode.
		slog.Warn("read ~/.claude/settings.json failed; keeping previous override", "err", err)
		if _, statErr := os.Stat(path); statErr == nil {
			return path
		}
		// No prior override exists either. Fall through to writing an empty
		// object so --settings has *something* to point at; Claude will then
		// complain loudly ("Not logged in") and the log warn above is the clue.
		data = []byte("{}")
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		// data came from readClaudeSettingsRaw which validates JSON, so this
		// path only fires on the {} fallback or a non-object top level.
		settings = make(map[string]json.RawMessage)
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
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	// Atomic write: claude reads this file on startup; a truncated write
	// could cause it to launch with empty config and disable hook filtering,
	// risking feedback loops.
	if err := osutil.WriteFileAtomic(path, out, 0600); err != nil {
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
					slog.Info("dropping hook to prevent naozhi callback loop", "event", eventName, "command", sanitizeLogCmd(cmd))
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
				newRaw, err := json.Marshal(safeEntries)
				if err != nil {
					continue // skip corrupted group
				}
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
// sanitizeLogCmd scrubs control characters from a hook command string so
// attacker-controlled content in ~/.claude/settings.json cannot inject fake
// log lines (newlines, ANSI escapes) when log.format is text. Also truncates
// to 80 chars so the log line stays readable.
func sanitizeLogCmd(cmd string) string {
	if len(cmd) > 80 {
		cmd = cmd[:80] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '.'
		}
		return r
	}, cmd)
}

func isNaozhiCallbackHook(cmd, port string) bool {
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	if strings.Contains(lower, "naozhi") {
		return true
	}
	if port != "" {
		for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0", "[::1]", "::1"} {
			if strings.Contains(lower, host+":"+port) {
				return true
			}
		}
		// Match any 127.x.x.x address (entire 127/8 loopback block)
		if strings.Contains(lower, "127.") && strings.Contains(lower, ":"+port) {
			return true
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
		case "doctor":
			runDoctor(os.Args[2:])
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
	if err := applyClaudeEnvSettings(); err != nil {
		// Non-fatal: Bedrock / Anthropic env may already be set via systemd
		// EnvironmentFile or exported by the shell. Warn so operators notice
		// if the only source was settings.json and that read failed.
		slog.Warn("apply ~/.claude/settings.json env failed", "err", err)
	}
	settingsFile := writeClaudeSettingsOverride(cfg.Server.Addr)

	backendsCfg := cfg.EnabledBackends()
	defaultBackend := cfg.DefaultBackendID()
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

	wrappers := make(map[string]*cli.Wrapper, len(backendsCfg))
	backendModels := make(map[string]string, len(backendsCfg))
	backendExtraArgs := make(map[string][]string, len(backendsCfg))
	var defaultWrapper *cli.Wrapper
	for _, b := range backendsCfg {
		var proto cli.Protocol
		switch b.ID {
		case "kiro":
			proto = &cli.ACPProtocol{}
		case "", "claude":
			proto = &cli.ClaudeProtocol{SettingsFile: settingsFile}
		default:
			slog.Warn("skipping unknown cli.backends entry", "id", b.ID)
			continue
		}
		w := cli.NewWrapper(b.Path, proto, b.ID)
		w.ShimManager = shimMgr
		wrappers[w.BackendID] = w
		if b.Model != "" {
			backendModels[w.BackendID] = b.Model
		}
		if len(b.Args) > 0 {
			backendExtraArgs[w.BackendID] = b.Args
		}
		if defaultWrapper == nil || w.BackendID == defaultBackend {
			defaultWrapper = w
		}
		// Empty CLIVersion means `--version` failed (binary missing, wrong
		// path, or crash). The wrapper is still registered so the dashboard
		// surfaces the configuration intent, but spawn attempts will fail.
		// Log at Warn so operators notice during startup instead of
		// discovering the breakage only when the first user message lands.
		// R55-QUAL-001.
		if w.CLIVersion == "" {
			slog.Warn("cli backend version probe failed",
				"id", w.BackendID, "name", w.CLIName, "path", w.CLIPath,
				"hint", "binary missing or --version crashed; spawns will fail until resolved")
		} else {
			slog.Info("cli backend enabled",
				"id", w.BackendID, "name", w.CLIName,
				"path", w.CLIPath, "version", w.CLIVersion)
		}
	}
	if defaultWrapper == nil {
		slog.Error("no usable cli backend configured")
		os.Exit(1)
	}
	// Exit non-zero when the default backend failed its version probe. A
	// healthy non-default sibling isn't useful — wrapperFor falls back to
	// defaultWrapper when no explicit backend is requested, and the per-spawn
	// error would say "wrapper selected, spawn failed" without surfacing the
	// root cause. Fail fast so operators can fix the config rather than have
	// every IM message bounce. R55-QUAL-001.
	if defaultWrapper.CLIVersion == "" {
		slog.Error("default cli backend is unavailable",
			"id", defaultWrapper.BackendID, "path", defaultWrapper.CLIPath,
			"hint", "fix the binary path in cli.backends or set cli.default to an available backend")
		os.Exit(1)
	}
	wrapper := defaultWrapper

	// Parse watchdog and store path
	noOutputTimeout, totalTimeout := cfg.ParseWatchdog()
	storePath := osutil.ExpandHome(cfg.Session.StorePath)
	workspace := osutil.ExpandHome(cfg.Session.CWD)
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
	})

	// Reconnect to surviving shim processes from previous naozhi run
	router.ReconnectShims()

	// Context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	cronLoc := cfg.ParseCronTimezone()
	slog.Info("cron timezone", "location", cronLoc.String())
	notifyDefault := cron.NotifyTarget{
		Platform: cfg.Cron.NotifyDefault.Platform,
		ChatID:   cfg.Cron.NotifyDefault.ChatID,
	}
	if notifyDefault.IsSet() {
		// Log only the platform and a truncated chat_id suffix so log
		// aggregators don't carry the full group/user identifier. The
		// dashboard still exposes the full value to authenticated operators.
		slog.Info("cron notify default configured",
			"platform", notifyDefault.Platform,
			"chat_id_suffix", chatIDSuffix(notifyDefault.ChatID))
	}
	scheduler := cron.NewScheduler(cron.SchedulerConfig{
		Router:        router,
		Platforms:     platforms,
		Agents:        agents,
		AgentCommands: cfg.AgentCommands,
		StorePath:     osutil.ExpandHome(cfg.Cron.StorePath),
		MaxJobs:       cfg.Cron.MaxJobs,
		ExecTimeout:   cfg.ParseExecutionTimeout(),
		Location:      cronLoc,
		NotifyDefault: notifyDefault,
		AllowedRoot:   workspace,
		ParentCtx:     ctx,
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
		OnReady: func() {
			if err := osutil.SdNotify("READY=1"); err != nil {
				slog.Warn("sd_notify READY failed", "err", err)
			}
		},
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
			slog.Info("shutdown starting", "reason", reason)
			if err := osutil.SdNotify("STOPPING=1"); err != nil {
				slog.Warn("sd_notify STOPPING failed", "err", err)
			}
			cancel()
			// Scheduler must stop fully before router.Shutdown: in-flight cron
			// jobs still call into router (GetOrCreate/Send), so tearing the
			// router down in parallel would race against those calls.
			scheduler.Stop()
			router.Shutdown()
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
