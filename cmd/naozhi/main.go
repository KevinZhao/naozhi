package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
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
	_ "github.com/naozhi/naozhi/internal/wireup"
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

// awsEnvDenyList 在 AWS_ 前缀允许之上再画一条"禁止跨入"的线：这些键会
// 改变 AWS 认证来源（角色切换、凭据文件重定向），~/.claude/settings.json
// 可以被 Claude tool 写入，放行等于给一个不受控的 env 注入通道对
// transcribe/S3 执行凭据劫持。默认只允许标准的 region/credentials/session
// 组合走进子进程。
var awsEnvDenyList = map[string]bool{
	"AWS_ROLE_ARN":                true,
	"AWS_WEB_IDENTITY_TOKEN_FILE": true,
	"AWS_SHARED_CREDENTIALS_FILE": true,
	"AWS_CONFIG_FILE":             true,
	"AWS_PROFILE":                 true,
	"AWS_DEFAULT_PROFILE":         true,
	"AWS_CA_BUNDLE":               true,
	"AWS_ENDPOINT_URL":            true,
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
func readClaudeSettingsRaw(ctx context.Context) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	return readJSONWithRetry(ctx, path, 3, 100*time.Millisecond)
}

// readJSONWithRetry reads path and verifies the content is valid JSON. If the
// read succeeds but parsing fails, retries up to attempts-1 more times with the
// given sleep in between. If the file doesn't exist, returns the os.Open error
// immediately (no retry — missing is a different failure mode than truncated).
// The ctx parameter allows callers to abort a retry sleep early on shutdown or
// timeout; ctx.Err() is returned when the context is cancelled mid-sleep.
func readJSONWithRetry(ctx context.Context, path string, attempts int, sleep time.Duration) ([]byte, error) {
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
			t := time.NewTimer(sleep)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
		}
	}
	return nil, lastParseErr
}

// filterClaudeEnv returns a copy of in containing only entries that pass the
// allowlist (claudeEnvAllowedPrefixes), the deny list (awsEnvDenyList), and the
// per-value safety check (no NUL/newline, ≤4096 bytes). Rejected keys are
// logged at WARN once per call so operators can spot a malicious or misconfigured
// ~/.claude/settings.json.
//
// Used by both applyClaudeEnvSettings (parent-process env injection) and
// writeClaudeSettingsOverride (child --settings file emission) so the two paths
// can never disagree on which keys are propagated. Historically AWS_PROFILE
// leaked into the override file (bypassing the parent deny list) and overrode
// the proxy-based bedrock auth chain; routing both paths through this helper
// closes that gap.
func filterClaudeEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !matchesAnyPrefix(k, claudeEnvAllowedPrefixes) {
			continue
		}
		if awsEnvDenyList[k] {
			slog.Warn("claude settings env: refusing to propagate auth-source AWS var", "key", k)
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
		out[k] = v
	}
	return out
}

// applyClaudeEnvSettings reads ~/.claude/settings.json and applies any env section
// to the current process so spawned CC child processes inherit them via os.Environ().
// Only sets vars not already present (shell-set vars take precedence) and only
// for keys passing filterClaudeEnv.
//
// Returns an error when the settings file cannot be read or parsed so callers
// can surface the failure. A nil return with zero env applied (e.g. no `env`
// section or all keys filtered) is NOT treated as an error.
func applyClaudeEnvSettings(ctx context.Context) error {
	data, err := readClaudeSettingsRaw(ctx)
	if err != nil {
		return err
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal env section: %w", err)
	}
	for k, v := range filterClaudeEnv(s.Env) {
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
func writeClaudeSettingsOverride(ctx context.Context, serverAddr string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".naozhi")
	path := filepath.Join(dir, "claude-settings.json")

	data, err := readClaudeSettingsRaw(ctx)
	if err != nil {
		// Read/parse failed. Do NOT overwrite an existing override — the last
		// known-good copy still lets Claude CLI authenticate. Report via logs so
		// the operator notices the degraded mode.
		//
		// R236-QA-13: file-missing is the normal first-run state and stays at
		// Warn; corrupt-JSON / unreadable means somebody actively broke the
		// settings file and gets logged at Error so it surfaces in alerting.
		if errors.Is(err, fs.ErrNotExist) {
			slog.Warn("read ~/.claude/settings.json: file missing; keeping previous override", "err", err)
		} else {
			slog.Error("read ~/.claude/settings.json: corrupt or unreadable; keeping previous override", "err", err)
		}
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

	// Filter env section through the same allowlist+deny+value-safety check
	// applied to the parent process. Without this, keys like AWS_PROFILE that
	// applyClaudeEnvSettings refuses can still leak into spawned claude via the
	// override file, silently overriding the proxy-based bedrock auth chain.
	if envRaw, ok := settings["env"]; ok {
		var envMap map[string]string
		if err := json.Unmarshal(envRaw, &envMap); err == nil {
			filtered := filterClaudeEnv(envMap)
			if filteredRaw, err := json.Marshal(filtered); err == nil {
				settings["env"] = filteredRaw
			}
		}
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
		// R236-QA-13: differentiate "file legitimately missing" from "file
		// exists but is corrupt JSON". The former is a normal first-run /
		// ops-not-configured state and stays at Warn; the latter is operator-
		// actionable (somebody hand-edited settings.json and broke it, or a
		// rewrite-in-place writer crashed mid-flush) and should surface at
		// Error so it shows up in the SLO log filter.
		if errors.Is(err, fs.ErrNotExist) {
			slog.Warn("apply ~/.claude/settings.json env: file missing", "err", err)
		} else {
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
	metrics.StartupPhasePlatformsMs.Set(time.Since(t0).Milliseconds())

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
		JitterMax:     cfg.ParseCronJitterMax(),
		ParentCtx:     ctx,
	})
	if err := scheduler.Start(); err != nil {
		slog.Error("start cron scheduler", "err", err)
		os.Exit(1)
	}
	metrics.StartupPhaseSchedulerMs.Set(time.Since(t0).Milliseconds())

	// Auto-workspace-chain (docs/rfc/auto-workspace-chain.md §4.3):
	// register cron Scheduler as a SessionIDExcluder so cron-spawned
	// sessionIDs are never folded into a user session's
	// prev_session_ids by the auto-chain spawn / backfill paths.
	// sysession does not need an excluder — sysWorkDir lives outside
	// any user workspace, so its JSONL files are excluded by path
	// matching at the workspace level.
	router.AddSessionIDExcluder(scheduler)

	// Build the system-session (background daemon) Manager.  Disabled
	// when cfg.Sysession.Enabled is false; degraded silently when the
	// runner can't be initialised so a missing/broken claude binary
	// doesn't break naozhi startup as a whole.
	sysMgr, sysWorkDir, err := buildSysessionManager(cfg, router, wrapper, storePath)
	if err != nil {
		slog.Warn("sysession manager unavailable; daemons disabled", "err", err)
	}
	if sysMgr != nil {
		sysMgr.Start(ctx)
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
