package config

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/sessionconst"
)

// Config is the top-level naozhi configuration loaded from config.yaml.
//
// "workspace" naming caveat (R216-CR-5 / R217-CR-6): three distinct
// concepts share the word "workspace" across Go fields and YAML keys —
// they are NOT interchangeable, do not refactor without checking all
// three:
//
//  1. Config.Workspace (yaml:"workspace") — this naozhi instance's own
//     identity (ID / display name); see WorkspaceConfig.
//  2. Config.Workspaces (yaml:"workspaces") — REMOTE-NODES map, an alias
//     of Config.Nodes used for cross-node IM bridging. Plural.
//  3. SessionConfig.Workspace (yaml:"workspace" inside `session:`) — a
//     deprecated alias for SessionConfig.CWD (default cwd for spawned
//     CLI processes). Single string, kept for backward compat.
//
// Consumers should read Workspace (1) for identity, Nodes (2) for the
// remote-instance pool, and Session.CWD (3) for the spawn directory.
type Config struct {
	Server        ServerConfig           `yaml:"server"`
	CLI           CLIConfig              `yaml:"cli"`
	Session       SessionConfig          `yaml:"session"`
	Platforms     PlatformConfigs        `yaml:"platforms"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	AgentCommands map[string]string      `yaml:"agent_commands"`
	// Nodes and Workspaces are two accepted YAML spellings for the same
	// concept — the set of remote naozhi instances this node polls. Nodes is
	// the legacy key; Workspaces is the preferred name. Consumers read from
	// cfg.Nodes; after Load (or an explicit cfg.Normalize() call) both maps
	// point to the same entries. Tests that build a Config literal directly
	// MUST call cfg.Normalize() before handing it to downstream code, or
	// validateConfig / main.go will silently skip the entries set only on
	// Workspaces. R71-ARCH-L1.
	//
	// NOT to be confused with Config.Workspace (singular, this-instance
	// identity) or Session.Workspace (deprecated CWD alias). See the
	// Config godoc above.
	Nodes        map[string]NodeConfig       `yaml:"nodes"`
	Workspaces   map[string]NodeConfig       `yaml:"workspaces"`
	ReverseNodes map[string]ReverseNodeEntry `yaml:"reverse_nodes"`
	Upstream     *UpstreamConfig             `yaml:"upstream"`
	// Workspace identifies THIS naozhi instance. Distinct from Workspaces
	// (remote nodes) and Session.Workspace (deprecated CWD alias).
	Workspace  WorkspaceConfig   `yaml:"workspace"`
	Transcribe *TranscribeConfig `yaml:"transcribe"`
	Cron       CronConfig        `yaml:"cron"`
	Log        LogConfig         `yaml:"log"`
	Projects   ProjectsConfig    `yaml:"projects"`
	Sysession  SysessionConfig   `yaml:"sysession,omitempty"`

	// Cached parsed durations (populated once in Load, avoids repeated ParseDuration)
	cachedTTL             time.Duration `yaml:"-"`
	cachedPruneTTL        time.Duration `yaml:"-"`
	cachedNoOutputTimeout time.Duration `yaml:"-"`
	cachedTotalTimeout    time.Duration `yaml:"-"`
	cachedExecTimeout     time.Duration `yaml:"-"`
	cachedCollectDelay    time.Duration `yaml:"-"`
	cachedJitterMax       time.Duration `yaml:"-"`
}

// WorkspaceConfig identifies this naozhi instance.
type WorkspaceConfig struct {
	ID   string `yaml:"id"`   // unique identifier (default: hostname)
	Name string `yaml:"name"` // display name (default: id)
}

type ProjectsConfig struct {
	Root            string          `yaml:"root"`                       // projects root directory
	PlannerDefaults PlannerDefaults `yaml:"planner_defaults,omitempty"` // global planner defaults
}

type PlannerDefaults struct {
	Model  string `yaml:"model,omitempty"`
	Prompt string `yaml:"prompt,omitempty"`
}

type NodeConfig struct {
	URL         string `yaml:"url"`
	Token       string `yaml:"token"`
	DisplayName string `yaml:"display_name"`
	Insecure    bool   `yaml:"insecure"` // allow plaintext HTTP without authentication
}

// UpstreamConfig configures this node to connect as a reverse node to a primary.
type UpstreamConfig struct {
	URL         string `yaml:"url"`
	NodeID      string `yaml:"node_id"`
	Token       string `yaml:"token"`
	DisplayName string `yaml:"display_name"`
	Insecure    bool   `yaml:"insecure"`
}

type AgentConfig struct {
	Model string   `yaml:"model"`
	Args  []string `yaml:"args"`
}

type ServerConfig struct {
	Addr           string `yaml:"addr"`
	DashboardToken string `yaml:"dashboard_token,omitempty"`
	TrustedProxy   bool   `yaml:"trusted_proxy,omitempty"` // trust X-Forwarded-For for client IP (enable behind ALB/CloudFront)
}

type CLIConfig struct {
	// Backend names the primary/default backend ("claude" (default) | "kiro").
	// When Backends is set, Backend is the one chosen when the dashboard
	// does not explicitly pick a backend for a new session.
	Backend string `yaml:"backend"`
	Path    string `yaml:"path"`
	// Backends enumerates every backend the server should enable. When
	// empty, naozhi falls back to the legacy single-backend mode using
	// only Backend/Path/Model/Args.
	Backends []CLIBackendConfig `yaml:"backends,omitempty"`
	Model    string             `yaml:"model"`
	Args     []string           `yaml:"args"`
}

// CLIBackendConfig configures one backend in a multi-backend deployment.
// ID is required; Path/Model/Args fall back to the top-level cli.* values.
type CLIBackendConfig struct {
	ID    string   `yaml:"id"`              // "claude" | "kiro"
	Path  string   `yaml:"path,omitempty"`  // overrides cli.path for this backend
	Model string   `yaml:"model,omitempty"` // overrides cli.model for this backend
	Args  []string `yaml:"args,omitempty"`  // overrides cli.args for this backend
}

type SessionConfig struct {
	MaxProcs  int            `yaml:"max_procs"`
	TTL       string         `yaml:"ttl"`
	PruneTTL  string         `yaml:"prune_ttl"` // how long dead/suspended sessions stay in the list before removal
	Watchdog  WatchdogConfig `yaml:"watchdog"`
	Queue     QueueConfig    `yaml:"queue"`
	StorePath string         `yaml:"store_path"`
	CWD       string         `yaml:"cwd"` // default working directory for CLI processes
	// Deprecated: use CWD instead. Preserved for backward compatibility
	// with existing config files; new fields should write only `cwd`.
	Workspace string              `yaml:"workspace"`
	Shim      ShimConfig          `yaml:"shim"`
	AutoChain AutoChainYAMLConfig `yaml:"auto_chain,omitempty"`
}

// AutoChainYAMLConfig represents the auto-workspace-chain feature
// settings (docs/rfc/auto-workspace-chain.md). Default-on with
// 7-day window / 32-cap; only operators who want to opt out or
// shrink the window need to set this block.
//
// Enabled is *bool so an absent key falls back to def-true while
// preserving the ability to explicitly write enabled: false.
type AutoChainYAMLConfig struct {
	Enabled     *bool `yaml:"enabled,omitempty"`
	WindowHours int   `yaml:"window_hours,omitempty"` // 0 → 168 (7d)
	Cap         int   `yaml:"cap,omitempty"`          // 0 → 32
}

// ResolvedEnabled returns the effective on/off flag.
func (c AutoChainYAMLConfig) ResolvedEnabled(def bool) bool {
	if c.Enabled == nil {
		return def
	}
	return *c.Enabled
}

// ResolvedWindowHours returns the effective window in hours.
func (c AutoChainYAMLConfig) ResolvedWindowHours(def int) int {
	if c.WindowHours <= 0 {
		return def
	}
	return c.WindowHours
}

// ResolvedCap returns the effective chain length cap.
func (c AutoChainYAMLConfig) ResolvedCap(def int) int {
	if c.Cap <= 0 {
		return def
	}
	return c.Cap
}

// QueueConfig controls IM message queuing when a session is busy.
type QueueConfig struct {
	// MaxDepth is the maximum number of messages to queue per session.
	// nil (omitted) = use default (20).
	// 0 = disable queuing (drop + "please wait", backward-compatible).
	// Negative values are treated as 0.
	MaxDepth *int `yaml:"max_depth"`
	// CollectDelay is the time to wait after the current turn completes
	// before draining queued messages. Allows capturing fast follow-up
	// messages into the same batch. Default: "500ms".
	CollectDelay string `yaml:"collect_delay"`
	// Mode selects how new messages arriving during an active turn are
	// handled. "collect" (default, backward-compatible) waits for the turn
	// to finish naturally. "interrupt" sends an in-band control_request to
	// the CLI so the active turn aborts promptly; the queued follow-ups are
	// then coalesced and sent as the next prompt on the same live process.
	// Only "stream-json" protocol sessions honour "interrupt"; other
	// protocols (ACP) silently fall back to "collect".
	Mode string `yaml:"mode"`
}

type ShimConfig struct {
	BufferSize      int    `yaml:"buffer_size"`         // ring buffer max lines (default: 10000)
	MaxBufferBytes  string `yaml:"max_buffer_bytes"`    // ring buffer max bytes (default: "50MB")
	IdleTimeout     string `yaml:"idle_timeout"`        // shim exits after no connection (default: "4h")
	WatchdogTimeout string `yaml:"disconnect_watchdog"` // disconnect no-output timeout (default: "30m")
	MaxShims        int    `yaml:"max_shims"`           // max concurrent shims (default: 6)
	StateDir        string `yaml:"state_dir"`           // shim state directory (default: ~/.naozhi/shims)
}

type WatchdogConfig struct {
	NoOutputTimeout string `yaml:"no_output_timeout"`
	TotalTimeout    string `yaml:"total_timeout"`
}

type PlatformConfigs struct {
	Feishu  *FeishuConfig  `yaml:"feishu"`
	Slack   *SlackConfig   `yaml:"slack"`
	Discord *DiscordConfig `yaml:"discord"`
	Weixin  *WeixinConfig  `yaml:"weixin"`
}

type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	ConnectionMode    string `yaml:"connection_mode"` // "websocket" (default) | "webhook"
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLength    int    `yaml:"max_reply_length"`
	// AllowInsecureWebhook opts in to verification_token-only webhook mode
	// (no encrypt_key HMAC). R250531-SEC-1 (#1507): without this flag, a
	// webhook configured with only verification_token refuses to start
	// because plaintext-token-only auth is replay/forgery-prone if the
	// token leaks. encrypt_key (HMAC) is the recommended secure config.
	AllowInsecureWebhook bool `yaml:"allow_insecure_webhook"`
}

type CronConfig struct {
	StorePath        string `yaml:"store_path"`
	MaxJobs          int    `yaml:"max_jobs"`
	ExecutionTimeout string `yaml:"execution_timeout"`
	// Timezone is the IANA name (e.g. "Asia/Shanghai") used to interpret cron
	// schedule expressions. Empty or "Local" uses the machine's local time
	// (respects $TZ). "UTC" forces UTC. Default: Local.
	Timezone string `yaml:"timezone"`
	// NotifyDefault is the fallback IM target used when a job has Notify=true
	// but no per-job NotifyPlatform / NotifyChatID. Empty fields disable the
	// default, in which case only per-job targets deliver notifications.
	NotifyDefault CronNotifyTarget `yaml:"notify_default,omitempty"`
	// JitterMax caps the randomized delay applied before each scheduled
	// tick fires, to flatten the "burst on the hour" CPU / API peak when
	// many jobs share a schedule. Default 2m. "0" disables jitter entirely.
	// The effective per-job jitter is min(JitterMax, period/4) so short
	// schedules don't get swallowed by a long window. TriggerNow (manual
	// "run now") bypasses jitter for responsiveness. See
	// docs/rfc/cron-v2-polish.md §3.2.
	JitterMax string `yaml:"jitter_max,omitempty"`
}

// CronNotifyTarget identifies an IM channel used as the fallback delivery
// target for cron job completion notifications.
type CronNotifyTarget struct {
	Platform string `yaml:"platform"` // "feishu" / "slack" / "discord" / "weixin"
	ChatID   string `yaml:"chat_id"`
}

// SysessionConfig configures the system-session daemon framework
// (docs/rfc/system-session.md).  Phase 1 ships AutoTitler only;
// future daemons land under Daemons[<name>].
type SysessionConfig struct {
	// Enabled is the master switch.  When false (default), no daemon
	// goroutines spin up regardless of per-daemon Enabled flags — a
	// quick kill-switch operators can flip without losing per-daemon
	// configuration.
	Enabled bool `yaml:"enabled,omitempty"`

	// TickTimeout caps a single Tick.  Daemons that exceed it return
	// DaemonRunTimedOut.  Default 30s.
	TickTimeout string `yaml:"tick_timeout,omitempty"`

	// Runner configures the shared LLM-call abstraction.  Empty values
	// fall back to runtime defaults computed from the data dir +
	// router.defaultBackend.
	Runner SysessionRunnerConfig `yaml:"runner,omitempty"`

	// Daemons enumerates per-daemon enable+tick+specific knobs.
	// Keys must match a compiled-in daemon name; unknown keys are
	// silently ignored to keep config forward-compatible.
	Daemons map[string]SysessionDaemonConfig `yaml:"daemons,omitempty"`
}

// SysessionRunnerConfig configures the transient-system-session Runner.
type SysessionRunnerConfig struct {
	// Model overrides --model.  Empty leaves --model off so the binary
	// uses its own default.
	Model string `yaml:"model,omitempty"`

	// WorkDir is the cwd for spawned subprocesses.  Empty defaults to
	// <dataDir>/sys-sessions/.  MUST be 0700 — Runner enforces.
	WorkDir string `yaml:"work_dir,omitempty"`

	// JSONLMaxAge controls the startup sweep retention window.  Empty
	// defaults to 168h (7 days).  Set "0" to disable sweep entirely
	// (operators with their own gardening process).
	JSONLMaxAge string `yaml:"jsonl_max_age,omitempty"`
}

// SysessionDaemonConfig holds the common-shape fields every daemon
// consumes plus daemon-private knobs. Decoded into each daemon's
// DaemonConfig in main_helpers.go's wiring. Concrete fields (not an
// untyped map) because YAML decoding into Go works better that way;
// each daemon reads only the keys it understands.
type SysessionDaemonConfig struct {
	Enabled bool   `yaml:"enabled,omitempty"`
	Tick    string `yaml:"tick,omitempty"`

	// AutoTitler-specific fields.
	MinUserTurns      int    `yaml:"min_user_turns,omitempty"`
	MinRenameInterval string `yaml:"min_rename_interval,omitempty"`
	BatchPerTick      int    `yaml:"batch_per_tick,omitempty"`
	IncludeGroupChat  bool   `yaml:"include_group_chat,omitempty"`

	// attachment-gc-specific fields (docs/rfc/attachment-gc-daemon.md).
	// UploadTTL/RefTTL: "0" or unset → daemon default (NOT "disable" —
	// contrast Runner.JSONLMaxAge where "0" disables). PerRootCap: 0 →
	// default 500. DryRun: when true, log would-removes without
	// deleting. RunOnStart: fire one sweep at startup (recommended for
	// this low-frequency daemon so a restart-churning process still
	// makes progress).
	UploadTTL  string `yaml:"upload_ttl,omitempty"`
	RefTTL     string `yaml:"ref_ttl,omitempty"`
	PerRootCap int    `yaml:"per_root_cap,omitempty"`
	DryRun     bool   `yaml:"dry_run,omitempty"`
	RunOnStart bool   `yaml:"run_on_start,omitempty"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // "json" (default) | "text"
}

type SlackConfig struct {
	BotToken       string `yaml:"bot_token"`
	AppToken       string `yaml:"app_token"` // xapp- token for Socket Mode
	MaxReplyLength int    `yaml:"max_reply_length"`
}

type DiscordConfig struct {
	BotToken       string `yaml:"bot_token"`
	MaxReplyLength int    `yaml:"max_reply_length"`
}

type WeixinConfig struct {
	Token          string `yaml:"token"`
	BaseURL        string `yaml:"base_url"`
	MaxReplyLength int    `yaml:"max_reply_length"`
}

type TranscribeConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Region   string `yaml:"region"`
	Language string `yaml:"language"` // BCP-47, default: zh-CN
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	// Reject config file if readable by group or others BEFORE reading its
	// contents into memory — the file may contain secrets (app_secret, tokens).
	// Use Lstat (not Stat) so a symlink pointing at a 0644 file cannot bypass
	// the check via a strict-mode symlink. Then open + Fstat to confirm the
	// file we read is the same we Lstat'd (closes the TOCTOU window between
	// the symlink/mode check and ReadFile).
	if fi, statErr := os.Lstat(path); statErr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("config file %s is a symlink; refusing to load (resolve the link or point --config at the target directly)",
				path)
		}
		// Reject any group/world permission bit (read/write/execute).
		// 0o044 only covered group-read + world-read; this widens to
		// 0o077 so a chmod 0710 (group-execute) cannot bypass the
		// gate, matching the policy used for shim state files.
		if fi.Mode()&0o077 != 0 {
			return nil, fmt.Errorf("config file %s is group/world-accessible (mode %04o); restrict with: chmod 0600 %s",
				path, fi.Mode().Perm(), path)
		}
	}

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	defer f.Close()
	// Re-check on the open fd: if the path was swapped to a symlink between
	// Lstat and OpenFile, Fstat sees the resolved target's mode. We cannot
	// detect the symlink swap directly via Fstat, but a 0644 swap-target
	// would trip the same world-readable gate. Treat fd-stat failure as
	// fatal — silently skipping the regular-file / mode gates would let an
	// attacker who can interrupt Fstat (signal storm, deliberate EBADF race)
	// bypass the second permission check.
	fi, ferr := f.Stat()
	if ferr != nil {
		return nil, fmt.Errorf("stat config fd: %w", ferr)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("config file %s is not a regular file", path)
	}
	// R237-SEC-8 / #654: align the fd-stat mask with the Lstat path's 0o077.
	// The previous 0o044 only covered group-read + world-read, so a 0o650
	// (group rwx + group-write only) symlink-swap target Lstat would have
	// flagged via the wider mask still squeaked through this fd-stat
	// re-check — a TOCTOU window where an attacker who can interrupt the
	// Lstat→OpenFile gap with a symlink swap to a 0o650 file gets the file
	// loaded into memory anyway. Widen to 0o077 so the two gates apply the
	// SAME policy and the second check is a real backstop, not a narrower
	// (and therefore weaker) one. Error string keeps "group/world-readable"
	// because read-only is still the most common operator misstep; the
	// extra coverage rejects rarer write/execute bits that have no business
	// on a credential-bearing config file regardless of the read bit.
	if fi.Mode()&0o077 != 0 {
		return nil, fmt.Errorf("config file %s is group/world-accessible (mode %04o); restrict with: chmod 0600 %s",
			path, fi.Mode().Perm(), path)
	}
	// Cap config reads at 1 MiB; the on-disk shape is ~hundreds of
	// lines of YAML and a runaway file (or a hostile symlink to a
	// large unrelated file) would otherwise force us to allocate the
	// whole content unnecessarily. expandEnvVars now accepts []byte
	// directly (R231-PERF-10) so we no longer pay an extra string
	// copy on the data path before YAML unmarshal.
	const maxConfigBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config file %s exceeds %d bytes", path, maxConfigBytes)
	}

	// Expand ${VAR} environment variables
	expanded := expandEnvVars(data)

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		// yaml.v3 echoes the offending line in its error, which after
		// ${VAR} expansion may contain decrypted secrets (app_secret,
		// dashboard_token, etc.). Return a generic error to callers but
		// keep the raw detail in logs, where operators can read it
		// without the secret leaking into HTTP responses or monitoring.
		slog.Debug("config yaml parse failed", "err", err)
		return nil, fmt.Errorf("parse config: yaml syntax error (check naozhi logs for details)")
	}

	applyDefaults(&cfg)
	if err := parseDurations(&cfg); err != nil {
		return nil, err
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Normalize reconciles the two-key YAML aliases (Nodes / Workspaces) so every
// consumer downstream can read from cfg.Nodes without caring which spelling
// the operator used. Load() calls this automatically; external code paths
// (tests, programmatic config construction) MUST call it before handing the
// Config to validateConfig or main.go, otherwise entries set only on
// Workspaces are silently ignored.
//
// Precedence: when both are set, Workspaces wins and Nodes is overwritten.
// A conflict log is emitted via slog.Warn so an operator who set both by
// mistake sees the drop. Safe to call repeatedly.
//
// R71-ARCH-L1.
func (cfg *Config) Normalize() {
	switch {
	case len(cfg.Workspaces) > 0 && len(cfg.Nodes) == 0:
		cfg.Nodes = cfg.Workspaces
	case len(cfg.Nodes) > 0 && len(cfg.Workspaces) == 0:
		slog.Warn("'nodes' config key is deprecated, please rename to 'workspaces'")
		cfg.Workspaces = cfg.Nodes
	case len(cfg.Workspaces) > 0 && len(cfg.Nodes) > 0:
		slog.Warn("both 'nodes' and 'workspaces' configured; using 'workspaces', ignoring 'nodes'")
		cfg.Nodes = cfg.Workspaces
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = ":8080"
	}
	if cfg.Session.MaxProcs <= 0 {
		cfg.Session.MaxProcs = sessionconst.DefaultMaxProcs
	}
	if cfg.Session.TTL == "" {
		cfg.Session.TTL = "30m"
	}
	if cfg.Session.PruneTTL == "" {
		cfg.Session.PruneTTL = "72h"
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Session.Workspace == "" {
		cfg.Session.Workspace = "~/.naozhi/workspace"
	}
	if cfg.Session.Queue.MaxDepth == nil {
		defaultDepth := 20
		cfg.Session.Queue.MaxDepth = &defaultDepth
	}
	if cfg.Session.Queue.CollectDelay == "" {
		cfg.Session.Queue.CollectDelay = "500ms"
	}
	if cfg.Session.Queue.Mode == "" {
		cfg.Session.Queue.Mode = "collect"
	}
	if cfg.Session.CWD != "" {
		if cfg.Session.Workspace != "" && cfg.Session.Workspace != cfg.Session.CWD {
			slog.Warn("both 'session.cwd' and deprecated 'session.workspace' configured; using 'cwd'")
		}
		cfg.Session.Workspace = cfg.Session.CWD
	} else if cfg.Session.Workspace != "" {
		// Deprecation alias: warn symmetrically with the nodes→workspaces
		// message below so users who only set `session.workspace` get the
		// same nudge to migrate, instead of silently depending on the
		// promotion forever. R71-CONFIG-M1.
		slog.Warn("'session.workspace' is deprecated, please rename to 'session.cwd'")
		cfg.Session.CWD = cfg.Session.Workspace
	}

	cfg.Normalize()

	if cfg.Workspace.ID == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Workspace.ID = h
		} else {
			cfg.Workspace.ID = "local"
		}
	}
	if cfg.Workspace.Name == "" {
		cfg.Workspace.Name = cfg.Workspace.ID
	}

	// Normalize agent_commands keys to lowercase so dispatch can match against
	// a user-typed "/Review" (CJK mobile IMEs auto-capitalize the first letter
	// of a line). Conflicting keys ("review" and "Review") keep the last-
	// written value with a warning.
	if len(cfg.AgentCommands) > 0 {
		normalized := make(map[string]string, len(cfg.AgentCommands))
		for cmd, agentID := range cfg.AgentCommands {
			lower := strings.ToLower(cmd)
			if existing, dup := normalized[lower]; dup && existing != agentID {
				slog.Warn("agent_commands key case conflict after normalize",
					"command", lower, "previous_agent", existing, "new_agent", agentID)
			}
			normalized[lower] = agentID
		}
		cfg.AgentCommands = normalized
	}
}

func parseDurations(cfg *Config) error {
	var err error
	if cfg.cachedTTL, err = parseDurationRequired(cfg.Session.TTL, "session.ttl", 30*time.Minute); err != nil {
		return err
	}
	if cfg.cachedPruneTTL, err = parseDurationRequired(cfg.Session.PruneTTL, "session.prune_ttl", 72*time.Hour); err != nil {
		return err
	}
	if cfg.cachedNoOutputTimeout, err = parseDurationRequired(cfg.Session.Watchdog.NoOutputTimeout, "session.watchdog.no_output_timeout", 2*time.Minute); err != nil {
		return err
	}
	if cfg.cachedTotalTimeout, err = parseDurationRequired(cfg.Session.Watchdog.TotalTimeout, "session.watchdog.total_timeout", 5*time.Minute); err != nil {
		return err
	}
	if cfg.cachedExecTimeout, err = parseDurationRequired(cfg.Cron.ExecutionTimeout, "cron.execution_timeout", 5*time.Minute); err != nil {
		return err
	}
	if cfg.cachedCollectDelay, err = parseDurationRequired(cfg.Session.Queue.CollectDelay, "session.queue.collect_delay", 500*time.Millisecond); err != nil {
		return err
	}
	if cfg.cachedJitterMax, err = parseDurationNonNegative(cfg.Cron.JitterMax, "cron.jitter_max", 2*time.Minute); err != nil {
		return err
	}
	// 硬上限 10m：抖动比大多数任务周期还长就毫无意义，clamp 并 warn，
	// 不把配置错误升成启动失败。
	if cfg.cachedJitterMax > 10*time.Minute {
		slog.Warn("cron.jitter_max exceeds 10m hard cap, clamping",
			"requested", cfg.cachedJitterMax, "cap", 10*time.Minute)
		cfg.cachedJitterMax = 10 * time.Minute
	}
	return nil
}

func validateConfig(cfg *Config) error {
	if cfg.Platforms.Feishu != nil {
		if containsEnvPlaceholder(cfg.Platforms.Feishu.AppID) || containsEnvPlaceholder(cfg.Platforms.Feishu.AppSecret) {
			return fmt.Errorf("feishu app_id or app_secret contains unexpanded ${VAR} — check environment variables")
		}
		if containsEnvPlaceholder(cfg.Platforms.Feishu.VerificationToken) {
			return fmt.Errorf("feishu verification_token contains unexpanded ${VAR} — check environment variables")
		}
		if containsEnvPlaceholder(cfg.Platforms.Feishu.EncryptKey) {
			return fmt.Errorf("feishu encrypt_key contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Feishu.AppID == "" || cfg.Platforms.Feishu.AppSecret == "" {
			return fmt.Errorf("feishu app_id and app_secret are required")
		}
		if cfg.Platforms.Feishu.ConnectionMode == "webhook" &&
			cfg.Platforms.Feishu.VerificationToken == "" && cfg.Platforms.Feishu.EncryptKey == "" {
			return fmt.Errorf("feishu webhook mode requires at least one of verification_token or encrypt_key to be set")
		}
	}
	if cfg.Platforms.Slack != nil {
		if containsEnvPlaceholder(cfg.Platforms.Slack.BotToken) {
			return fmt.Errorf("slack bot_token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Slack.BotToken == "" {
			return fmt.Errorf("slack bot_token is required")
		}
	}
	if cfg.Platforms.Discord != nil {
		if containsEnvPlaceholder(cfg.Platforms.Discord.BotToken) {
			return fmt.Errorf("discord bot_token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Discord.BotToken == "" {
			return fmt.Errorf("discord bot_token is required")
		}
	}
	if cfg.Platforms.Weixin != nil {
		if containsEnvPlaceholder(cfg.Platforms.Weixin.Token) {
			return fmt.Errorf("weixin token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Weixin.Token == "" {
			return fmt.Errorf("weixin token is required")
		}
		// R191-SEC-M3: base_url flows into every platform HTTP call. Without
		// this validation an operator could point it at http://169.254.169.254
		// (EC2 IMDS) or http://localhost:9200 (internal services) and every
		// long-poll/send request is weaponised into SSRF. node config already
		// enforces scheme+host (see validateNodeURL); weixin was the gap.
		if bu := cfg.Platforms.Weixin.BaseURL; bu != "" {
			u, err := url.Parse(bu)
			if err != nil {
				return fmt.Errorf("weixin base_url invalid: %w", err)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("weixin base_url must use http or https (got %q)", u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("weixin base_url must have a host")
			}
			// Literal-IP SSRF guard: a hostname like `wechat.example.com`
			// still resolves at dial time via the OS resolver, so DNS-based
			// SSRF requires a runtime Dialer hook; this stanza only blocks
			// the cheap-and-common case of putting a raw private/loopback
			// /link-local IP directly in config. CheckRedirect elsewhere
			// blocks the redirect variant.
			if host := u.Hostname(); host != "" {
				if ip := net.ParseIP(host); ip != nil {
					if ip.IsLoopback() || ip.IsPrivate() ||
						ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
						ip.IsUnspecified() {
						return fmt.Errorf("weixin base_url host %q is a loopback/private/link-local address; refusing (SSRF guard)", host)
					}
				}
			}
		}
	}

	for id, nc := range cfg.Nodes {
		if nc.URL == "" {
			return fmt.Errorf("node %q: url is required", id)
		}
		if strings.HasSuffix(nc.URL, "/") {
			return fmt.Errorf("node %q: url must not have trailing slash", id)
		}
		u, err := url.Parse(nc.URL)
		if err != nil {
			return fmt.Errorf("node %q: invalid url: %w", id, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("node %q: url must be http or https", id)
		}
		if u.Scheme == "http" && nc.Token != "" {
			return fmt.Errorf("node %q: refusing to send bearer token over plaintext HTTP — use HTTPS", id)
		}
		if u.Scheme == "http" && nc.Token == "" {
			if !nc.Insecure {
				return fmt.Errorf("node %q: plaintext HTTP without authentication is unsafe — set insecure: true to allow", id)
			}
			slog.Warn("node uses plaintext HTTP without authentication — session data is exposed to network attackers", "node", id)
		}
	}

	if cfg.Upstream != nil {
		if cfg.Upstream.URL == "" {
			return fmt.Errorf("upstream.url is required")
		}
		if containsEnvPlaceholder(cfg.Upstream.URL) {
			return fmt.Errorf("upstream.url contains unexpanded ${VAR} — check environment variables")
		}
		if !strings.HasPrefix(cfg.Upstream.URL, "wss://") && !strings.HasPrefix(cfg.Upstream.URL, "ws://") {
			return fmt.Errorf("upstream.url must use ws:// or wss:// scheme")
		}
		if strings.HasPrefix(cfg.Upstream.URL, "ws://") && !cfg.Upstream.Insecure {
			return fmt.Errorf("upstream.url must use wss:// — refusing to send bearer token over plaintext ws:// (set insecure: true to allow)")
		}
		if cfg.Upstream.NodeID == "" {
			return fmt.Errorf("upstream.node_id is required")
		}
		if containsEnvPlaceholder(cfg.Upstream.NodeID) {
			return fmt.Errorf("upstream.node_id contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Upstream.Token == "" {
			return fmt.Errorf("upstream.token is required")
		}
		if containsEnvPlaceholder(cfg.Upstream.Token) {
			return fmt.Errorf("upstream.token contains unexpanded ${VAR} — check environment variables")
		}
	}

	for id, entry := range cfg.ReverseNodes {
		if entry.Token == "" {
			return fmt.Errorf("reverse_nodes %q: token is required", id)
		}
		if containsEnvPlaceholder(entry.Token) {
			return fmt.Errorf("reverse_nodes %q: token contains unexpanded ${VAR} — check environment variables", id)
		}
	}

	if cfg.Server.DashboardToken == "" {
		slog.Warn("SECURITY: dashboard_token is empty — all dashboard API endpoints are accessible without authentication",
			"hint", "set NAOZHI_DASHBOARD_TOKEN or dashboard_token in config")
	} else if containsEnvPlaceholder(cfg.Server.DashboardToken) {
		// Refuse to start with a literal "${VAR}" string as the dashboard
		// credential: the placeholder is readable in the repository, so
		// anyone who ever sees the config knows the login token.
		return fmt.Errorf("server.dashboard_token contains unexpanded ${VAR} — check environment variables (refusing to run with a guessable token)")
	}

	// R191-ARCH-L12: cross-reference cron.notify_default.platform against
	// configured platform sections. Without this check a typo silently falls
	// into the per-tick warning path ("cron notify: platform not found") and
	// operators only discover the misconfig once a notification actually
	// fires. An empty platform disables the default entirely and is legal.
	if np := cfg.Cron.NotifyDefault.Platform; np != "" {
		ok := false
		switch np {
		case "feishu":
			ok = cfg.Platforms.Feishu != nil
		case "slack":
			ok = cfg.Platforms.Slack != nil
		case "discord":
			ok = cfg.Platforms.Discord != nil
		case "weixin":
			ok = cfg.Platforms.Weixin != nil
		}
		if !ok {
			return fmt.Errorf("cron.notify_default.platform %q is not a configured platform (set platforms.%s or clear notify_default)", np, np)
		}
	}

	// CLI argv flows directly to exec.Command; reject NUL / C0 control bytes
	// here so a compromised or mistaken config file cannot smuggle delimiters
	// into argv that the OS would misinterpret. Primary argv injection gate
	// lives in protocol/shim/session validators (R195-SEC), this adds a
	// config-layer equivalent for configured args that never pass through
	// user input validators.
	if err := validateArgvStrings("cli.args", cfg.CLI.Args); err != nil {
		return err
	}
	if err := validateModelString("cli.model", cfg.CLI.Model); err != nil {
		return err
	}
	for _, b := range cfg.CLI.Backends {
		if err := validateArgvStrings(fmt.Sprintf("cli.backends[%s].args", b.ID), b.Args); err != nil {
			return err
		}
		if err := validateModelString(fmt.Sprintf("cli.backends[%s].model", b.ID), b.Model); err != nil {
			return err
		}
	}
	// agents[*].args 同样会被拼进 exec.Command（session/router.go 按 agentID
	// 合并 AgentOpts.ExtraArgs），配置层漏过等于放弃 NUL/控制字符防御。
	// agents[*].model 走 SpawnOptions.Model 进 BuildArgs；同步 allowlist
	// 防御 leading-`-` 等再被 flag-parser 误解析。R215-SEC-P2-1.
	for id, a := range cfg.Agents {
		if err := validateArgvStrings(fmt.Sprintf("agents[%s].args", id), a.Args); err != nil {
			return err
		}
		if err := validateModelString(fmt.Sprintf("agents[%s].model", id), a.Model); err != nil {
			return err
		}
	}
	// projects.planner_defaults.model 同走 BuildArgs（spawnSession 在 planner
	// 路径覆盖 opts.Model），配置层 allowlist 与 cli.model 对齐。
	if err := validateModelString("projects.planner_defaults.model", cfg.Projects.PlannerDefaults.Model); err != nil {
		return err
	}

	// projects.planner_defaults.prompt 走 --append-system-prompt argv，
	// 与 project.PlannerPrompt 完全同源。validateConfig 之前漏检此字段：
	// 配置文件里写入 NUL / C0 / C1 / bidi / LS-PS 会绕过所有验证直接进 argv。
	// 镜像 project.ValidateConfig 的同名规则；LF/CR 同样禁止——多行 prompt
	// 必须经 CLAUDE.md 文件路径引入。
	if err := validatePlannerPrompt("projects.planner_defaults.prompt", cfg.Projects.PlannerDefaults.Prompt); err != nil {
		return err
	}

	return nil
}

// validatePlannerPrompt enforces the same character-set policy as
// project.ValidateConfig on PlannerPrompt: reject NUL, all C0 controls
// (including LF/CR — multi-line prompts must come from CLAUDE.md), DEL,
// C1 controls, bidi overrides/isolates, and LS/PS. Empty is accepted (means
// "no global default"). R224-SEC-4 / R224-SEC-3.
//
// Reuses project.MaxPlannerPromptBytes so both paths share one cap and
// can never drift silently.
func validatePlannerPrompt(field, prompt string) error {
	if prompt == "" {
		return nil
	}
	if len(prompt) > project.MaxPlannerPromptBytes {
		return fmt.Errorf("%s exceeds %d-byte limit", field, project.MaxPlannerPromptBytes)
	}
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			return fmt.Errorf("%s contains invalid control characters (NUL/C0/DEL — argv corruption guard)", field)
		}
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%s contains invalid unicode controls (C1/bidi/LS-PS)", field)
		}
	}
	return nil
}

// validateArgvStrings rejects control characters and NUL bytes inside argv
// elements. Empty elements are rejected too so an accidental YAML "- " does
// not silently pass an empty argument to the CLI. Kept narrow and local to
// the config package; broader runtime validators (session.ValidateSessionKey,
// shim.validateKeyForShim) cover user-controlled strings.
func validateArgvStrings(field string, args []string) error {
	for i, a := range args {
		if a == "" {
			return fmt.Errorf("%s[%d] is empty — refusing (likely YAML typo)", field, i)
		}
		for _, r := range a {
			if r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
				return fmt.Errorf("%s[%d] contains control byte (0x%02x) — refusing (argv injection guard)", field, i, r)
			}
		}
	}
	return nil
}

// modelNameRe restricts model identifiers to characters that can never be
// re-interpreted by Claude / Kiro / Gemini CLIs as a flag boundary. Real
// Anthropic / vendor model strings are alphanumerics plus `.`, `_`, `-`
// (e.g. "claude-opus-4-5", "kiro-1.0", "us.anthropic.claude-3-5-sonnet").
// Empty is accepted by the caller (means "use backend default"); this regex
// only validates non-empty strings.
//
// Why the leading character must be alphanumeric: BuildArgs appends
// `--model <opts.Model>` to the argv, but a future refactor or alternate
// backend that drops the `--model` flag and passes the model as a positional
// could let "-foo" be re-parsed by the CLI's flag parser. The first char is
// pinned to [A-Za-z0-9] so a leading `-` (or `.` ambiguous on some shells)
// is rejected outright, eliminating that class of misuse at the
// config-validation layer rather than relying on per-backend argv assembly
// to stay correct forever.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateModelString gates a configured model identifier. Empty is allowed
// (caller-defined fallback semantics: cli.model empty → backend default,
// agent.model empty → cli.model). Non-empty values must match modelNameRe.
// R215-SEC-P2-1.
func validateModelString(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 128 {
		return fmt.Errorf("%s %q is too long (max 128 chars)", field, value)
	}
	if !modelNameRe.MatchString(value) {
		return fmt.Errorf("%s %q must match [A-Za-z0-9._-]+ — refusing (flag-injection guard)", field, value)
	}
	return nil
}

// parseDurationRequired parses s as a positive duration.
// Returns fallback if s is empty, or an error if s is non-empty but invalid or non-positive.
func parseDurationRequired(s, name string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be positive", name, s)
	}
	return d, nil
}

// parseDurationNonNegative 允许 "0" / "0s" 作为显式关闭的合法值，
// 非零值必须为正。空字符串返回 fallback。用于 cron.jitter_max 这类
// "默认开启、允许关闭" 的可选配置。
func parseDurationNonNegative(s, name string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid %s %q: must be zero or positive", name, s)
	}
	return d, nil
}

// ParseTTL returns the TTL duration (cached after Load).
func (c *Config) ParseTTL() time.Duration {
	return c.cachedTTL
}

// ParsePruneTTL returns the prune TTL duration (cached after Load).
func (c *Config) ParsePruneTTL() time.Duration {
	return c.cachedPruneTTL
}

// ParseWatchdog returns the watchdog timeout durations (cached after Load).
func (c *Config) ParseWatchdog() (noOutputTimeout, totalTimeout time.Duration) {
	return c.cachedNoOutputTimeout, c.cachedTotalTimeout
}

// ParseExecutionTimeout returns the cron execution timeout duration (cached after Load).
func (c *Config) ParseExecutionTimeout() time.Duration {
	return c.cachedExecTimeout
}

// ParseCronTimezone returns the *time.Location used for cron schedule evaluation.
// Empty or "Local" returns time.Local (respects $TZ or the system tz).
// An invalid zone falls back to time.Local with a warning.
func (c *Config) ParseCronTimezone() *time.Location {
	name := strings.TrimSpace(c.Cron.Timezone)
	if name == "" || strings.EqualFold(name, "Local") {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		slog.Warn("invalid cron.timezone, falling back to Local", "value", name, "err", err)
		return time.Local
	}
	return loc
}

// ParseCollectDelay returns the queue collect delay (cached after Load).
func (c *Config) ParseCollectDelay() time.Duration {
	return c.cachedCollectDelay
}

// ParseCronJitterMax returns the cron scheduling jitter cap (cached after Load).
// 0 means jitter is disabled. See cron.Scheduler.applyJitter.
func (c *Config) ParseCronJitterMax() time.Duration {
	return c.cachedJitterMax
}

// EnabledBackends returns the normalized list of backends to enable.
// When cli.backends is set it wins; otherwise the legacy single cli.backend
// (defaulting to "claude") is returned. The default backend is always first
// in the result so callers can treat position 0 as "pick when none chosen".
// Duplicate IDs collapse to the first occurrence.
func (c *Config) EnabledBackends() []CLIBackendConfig {
	// Resolve the default ID consistently with DefaultBackendID: explicit
	// cli.backend wins, then the first usable entry in cli.backends, then
	// legacy "claude". Previously this hard-coded "claude" which caused
	// EnabledBackends()[0].ID to disagree with DefaultBackendID() when
	// cli.backend was unset and the first configured backend wasn't claude.
	defaultID := c.CLI.Backend
	if defaultID == "" {
		for _, b := range c.CLI.Backends {
			if b.ID != "" {
				defaultID = b.ID
				break
			}
		}
	}
	if defaultID == "" {
		defaultID = "claude"
	}

	if len(c.CLI.Backends) == 0 {
		return []CLIBackendConfig{{
			ID:    defaultID,
			Path:  c.CLI.Path,
			Model: c.CLI.Model,
			Args:  c.CLI.Args,
		}}
	}

	seen := make(map[string]bool, len(c.CLI.Backends))
	out := make([]CLIBackendConfig, 0, len(c.CLI.Backends))
	for _, b := range c.CLI.Backends {
		if b.ID == "" || seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		// Per-backend fields fall back to the top-level cli.* values so
		// operators can keep a single Model/Args and only list IDs.
		if b.Model == "" {
			b.Model = c.CLI.Model
		}
		if len(b.Args) == 0 {
			b.Args = c.CLI.Args
		}
		out = append(out, b)
	}

	// All entries had empty IDs: fall back to legacy single-backend so the
	// server doesn't refuse to start with a confusing "no usable cli backend".
	if len(out) == 0 {
		return []CLIBackendConfig{{
			ID:    defaultID,
			Path:  c.CLI.Path,
			Model: c.CLI.Model,
			Args:  c.CLI.Args,
		}}
	}

	// Float the default backend to position 0 so UI defaults stay stable
	// regardless of YAML ordering.
	for i, b := range out {
		if b.ID == defaultID && i > 0 {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out
}

// DefaultBackendID reports the backend ID to use when a request does not
// specify one.
func (c *Config) DefaultBackendID() string {
	if id := c.CLI.Backend; id != "" {
		return id
	}
	if len(c.CLI.Backends) > 0 && c.CLI.Backends[0].ID != "" {
		return c.CLI.Backends[0].ID
	}
	return "claude"
}

// QueueMaxDepth returns the resolved queue max depth.
// Negative values are clamped to 0 (disables queuing, degrades to drop+wait)
// so a typo in config can't produce a negative cap that breaks Enqueue's
// `len(msgs) >= maxDepth` guard.
func (c *Config) QueueMaxDepth() int {
	if c.Session.Queue.MaxDepth == nil {
		return 20
	}
	if d := *c.Session.Queue.MaxDepth; d > 0 {
		return d
	}
	return 0
}

// QueueMode returns the raw queue mode string from config ("collect" or
// "interrupt"). Callers normalise via dispatch.ParseQueueMode so empty/unknown
// values fall back to the safe default (collect). Keeping this as a string at
// the config boundary avoids an import cycle (config → dispatch).
func (c *Config) QueueMode() string {
	return c.Session.Queue.Mode
}

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// envExpansionDenyPrefixes lists env-var name prefixes that we refuse to
// expand inside config.yaml even if the operator literally types the
// placeholder. The threat model (R240-SEC-16 / #1047) is an operator who
// mistakes ${ANTHROPIC_API_KEY} for a generic credential alias and
// inadvertently materialises the upstream Claude API key into a string
// field that may be echoed to logs (slog.Info("config loaded", "cfg",
// ...)) or reflected back via the dashboard config inspector. None of
// these prefixes have a legitimate naozhi-side use; if a future feature
// genuinely needs one, prefer renaming the env (NAOZHI_-prefixed alias)
// rather than relaxing this denylist. Match is case-insensitive against
// the *full* env name; partial substring matches are NOT considered (so
// the existing test fixture TEST_NAOZHI_VAR is unaffected).
var envExpansionDenyPrefixes = []string{
	"ANTHROPIC_",
	"AWS_",
	"AZURE_",
	"GCP_",
	"GOOGLE_",
	"OCI_",
	"OPENAI_",
	// Additional upstream-credential prefixes that have no legitimate
	// naozhi-side config use. GITHUB_TOKEN / GH_TOKEN are matched as
	// full names via this prefix (no partial suffixes follow). The
	// OPENROUTER_ / MISTRAL_ / HUGGINGFACE_ / HUGGING_FACE_ prefixes
	// guard against alternative-LLM provider keys that an operator
	// might confuse with a generic alias slot. R240-SEC-16 / #1047.
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"OPENROUTER_",
	"MISTRAL_",
	"HUGGINGFACE_",
	"HUGGING_FACE_",
}

// allowEnvExpansion reports whether key is safe to expand in config.yaml.
// Returns false for known-secret upstream credential prefixes; the caller
// leaves the placeholder un-expanded so the secret never reaches the
// in-memory Config (and therefore never reaches log/echo paths).
func allowEnvExpansion(key string) bool {
	upper := strings.ToUpper(key)
	for _, p := range envExpansionDenyPrefixes {
		if strings.HasPrefix(upper, p) {
			return false
		}
	}
	return true
}

// expandEnvVars resolves ${VAR} placeholders inside the YAML payload.
// It accepts []byte (R231-PERF-10) so Load can avoid materialising the
// whole config file as a string just to feed the regex API; when no
// placeholder is present we return data unchanged, and yaml.Unmarshal
// also accepts []byte without further conversion.
//
// R240-SEC-16 / #1047: env-var names matching envExpansionDenyPrefixes
// (ANTHROPIC_*, AWS_*, etc.) are deliberately NOT expanded, even if the
// env is set. This keeps upstream credentials from being mistakenly
// materialised into config string fields that downstream code may log or
// echo back over the dashboard API. The placeholder is preserved as-is
// so the bad config surfaces loudly (literal "${ANTHROPIC_API_KEY}"
// inside e.g. dashboard_token will fail containsEnvPlaceholder validation
// at startup) rather than silently leaking the key.
//
// R237-SEC-4 / #637: an env value containing a newline (or other control
// byte) was previously substituted raw into the YAML payload, so an
// attacker who controlled an env var could inject arbitrary YAML keys —
// e.g. `FOO=$'value\ndashboard_token: pwned'` would inject a
// `dashboard_token` field even into a single-line scalar. Refuse to
// expand any value that contains a control byte; the placeholder is left
// intact so the bad config surfaces loudly via downstream validation
// (containsEnvPlaceholder) rather than silently turning into a forged
// config.
func expandEnvVars(data []byte) []byte {
	if !bytes.Contains(data, []byte("${")) {
		return data
	}
	return envVarRe.ReplaceAllFunc(data, func(match []byte) []byte {
		key := string(bytes.TrimSuffix(bytes.TrimPrefix(match, []byte("${")), []byte("}")))
		if !allowEnvExpansion(key) {
			return match
		}
		if val, ok := os.LookupEnv(key); ok {
			if containsYAMLBreakingByte(val) {
				return match
			}
			return []byte(val)
		}
		return match
	})
}

// containsYAMLBreakingByte reports whether s contains any byte that, when
// substituted raw into a YAML payload, could break the original token's
// scope and inject a sibling/parent-level key. Newlines (\n, \r) are the
// primary vector; other ASCII control bytes are refused defensively (they
// have no legitimate place in a config scalar). Tab is treated as
// breaking too — YAML 1.2 forbids tabs inside indentation, so a tab inside
// an expanded value can confuse a downstream parser. R237-SEC-4 / #637.
func containsYAMLBreakingByte(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\n' || b == '\r' || b == '\t' {
			return true
		}
		if b < 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

func containsEnvPlaceholder(s string) bool {
	return strings.Contains(s, "${")
}
