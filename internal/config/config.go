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

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/sessionconst"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// Config is the top-level naozhi configuration loaded from config.yaml.
//
// Three distinct concepts share the word "workspace" and are NOT
// interchangeable: Config.Workspace (this instance's identity),
// Config.Workspaces (remote-nodes map, alias of Nodes) and
// SessionConfig.Workspace (deprecated alias of Session.CWD).
type Config struct {
	// SchemaVersion pins the config schema this file targets; absent/0 is
	// normalized to CurrentSchemaVersion, newer than the binary is rejected.
	SchemaVersion int `yaml:"schema_version,omitempty"`

	Server        ServerConfig           `yaml:"server"`
	CLI           CLIConfig              `yaml:"cli"`
	Session       SessionConfig          `yaml:"session"`
	Platforms     PlatformConfigs        `yaml:"platforms"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	AgentCommands map[string]string      `yaml:"agent_commands"`
	// AccessProfiles are named auth/upstream overlays a project or agent may
	// reference by name; empty keeps every session on the global settings.json
	// baseline (RFC project-access-profile).
	AccessProfiles map[string]AccessProfile `yaml:"access_profiles,omitempty"`
	// DefaultAccessProfile applies to any session with NO explicit profile
	// (per-request, dashboard, project or resume-locked). Empty = global
	// baseline. Must name a key in AccessProfiles (validated at load).
	DefaultAccessProfile string `yaml:"default_access_profile,omitempty"`
	// NaozhiSettings opts in to a naozhi-owned isolated Claude settings file
	// (seeded once from ~/.claude/settings.json, then decoupled); disabled keeps
	// `--setting-sources user` (RFC naozhi-owned-settings-v3).
	NaozhiSettings NaozhiSettingsConfig `yaml:"naozhi_settings,omitempty"`

	// Nodes (legacy key) and Workspaces (preferred) are two spellings of the
	// remote-instance map. Consumers read cfg.Nodes; Normalize() (called by
	// Load) makes both point at the same entries — a Config literal built in
	// tests MUST call it too or Workspaces-only entries are silently skipped.
	Nodes        map[string]NodeConfig       `yaml:"nodes"`
	Workspaces   map[string]NodeConfig       `yaml:"workspaces"`
	ReverseNodes map[string]ReverseNodeEntry `yaml:"reverse_nodes"`
	Upstream     *UpstreamConfig             `yaml:"upstream"`
	// Workspace identifies THIS naozhi instance (not Workspaces / Session.Workspace).
	Workspace   WorkspaceConfig   `yaml:"workspace"`
	Transcribe  *TranscribeConfig `yaml:"transcribe"`
	Cron        CronConfig        `yaml:"cron"`
	Log         LogConfig         `yaml:"log"`
	Projects    ProjectsConfig    `yaml:"projects"`
	Sysession   SysessionConfig   `yaml:"sysession,omitempty"`
	Update      UpdateConfig      `yaml:"update,omitempty"`
	ImageOrient ImageOrientConfig `yaml:"image_orient,omitempty"`
	Cost        CostConfig        `yaml:"cost,omitempty"`

	// Parsed durations, populated once in Load.
	cachedTTL             time.Duration `yaml:"-"`
	cachedPruneTTL        time.Duration `yaml:"-"`
	cachedNoOutputTimeout time.Duration `yaml:"-"`
	cachedTotalTimeout    time.Duration `yaml:"-"`
	cachedExecTimeout     time.Duration `yaml:"-"`
	cachedCollectDelay    time.Duration `yaml:"-"`
	cachedJitterMax       time.Duration `yaml:"-"`
	cachedInterval        time.Duration `yaml:"-"`
}

// WorkspaceConfig identifies this naozhi instance.
type WorkspaceConfig struct {
	ID   string `yaml:"id"`   // unique identifier (default: hostname)
	Name string `yaml:"name"` // display name (default: id)
}

type ProjectsConfig struct {
	Root            string          `yaml:"root"`                       // projects root directory
	PlannerDefaults PlannerDefaults `yaml:"planner_defaults,omitempty"` // global planner defaults
	// IncludeRoot also registers the projects root itself as a project so files
	// directly under root get preview/download buttons. Default false.
	// SECURITY: the root project spans the whole tree (sibling projects
	// included) and the dashboard token is the only barrier — SINGLE-OPERATOR
	// feature. The file endpoints treat it like the __public_tmp__ pseudo-project
	// (UID / denied-name / irregular-type / credential-name gates, audit log).
	IncludeRoot bool `yaml:"include_root,omitempty"`
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

// LogValue implements slog.LogValuer so the bearer Token never lands in logs.
func (c NodeConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", c.URL),
		slog.String("token", redactSecret(c.Token)),
		slog.String("display_name", c.DisplayName),
		slog.Bool("insecure", c.Insecure),
	)
}

// UpstreamConfig configures this node to connect as a reverse node to a primary.
type UpstreamConfig struct {
	URL         string `yaml:"url"`
	NodeID      string `yaml:"node_id"`
	Token       string `yaml:"token"`
	DisplayName string `yaml:"display_name"`
	Insecure    bool   `yaml:"insecure"`
}

// LogValue implements slog.LogValuer so the bearer Token never lands in logs.
func (c UpstreamConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", c.URL),
		slog.String("node_id", c.NodeID),
		slog.String("token", redactSecret(c.Token)),
		slog.String("display_name", c.DisplayName),
		slog.Bool("insecure", c.Insecure),
	)
}

// redactSecret returns a fixed placeholder for non-empty secrets and "" for
// unset ones, so logs distinguish "configured" from "absent" without leaking length.
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "[REDACTED]"
}

type AgentConfig struct {
	Model string   `yaml:"model"`
	Args  []string `yaml:"args"`
	// Backend pins the default CLI backend ("claude" | "kiro" | …) for this
	// agent's sessions. Empty = router default.
	Backend string `yaml:"backend,omitempty"`
	// AccessProfile names the default access profile for this agent's
	// sessions. Empty = global default.
	AccessProfile string `yaml:"access_profile,omitempty"`
	// Effort overrides the thinking-effort tier for this agent's sessions.
	// Empty = inherit cli.backends[].effort, then cli.effort.
	Effort string `yaml:"effort,omitempty"`
	// SystemPrompt is appended to the CLI system prompt for every session of
	// this agent (`--append-system-prompt`); planner prompts and scratch
	// context stack on top ("\n\n"-separated). Multi-line is fine; CR/NUL/C0/
	// DEL/C1/bidi and a leading '-' are rejected, capped at
	// MaxAgentSystemPromptBytes. Claude backend only. Do NOT put the flag under
	// `args` — it is denylisted there; Load lifts a legacy occurrence (#2493).
	SystemPrompt string `yaml:"system_prompt,omitempty"`
}

// AccessProfile is a NAMED bundle of "how to reach the model": a whitelisted
// env overlay (auth chain / upstream), default backend and default model.
// Orthogonal to backend — claude can run on 1P direct or Bedrock proxy. The env
// values live only in this operator-authored file; project.yaml (which may
// sync from git) carries only the NAME (RFC project-access-profile §2/§6.1).
type AccessProfile struct {
	// DisplayName is the operator-facing label (dashboard chip / picker).
	DisplayName string `yaml:"display_name,omitempty"`
	// ChipColor is a CSS colour for the dashboard chip (e.g. "#d97757").
	ChipColor string `yaml:"chip_color,omitempty"`
	// Env is the whitelisted overlay (envpolicy.ValidateOverlayEntry); *_FILE
	// keys name a host path whose contents become the secret at spawn time.
	// Merged onto the shim baseline and STILL re-filtered by the shim.
	Env map[string]string `yaml:"env,omitempty"`
	// DefaultModel sits below an explicit per-request / PlannerModel choice and
	// above backend.DefaultModel.
	DefaultModel string `yaml:"default_model,omitempty"`
	// DefaultBackend optionally pins a backend; a project's `backend` still wins.
	DefaultBackend string `yaml:"default_backend,omitempty"`
}

type ServerConfig struct {
	Addr           string `yaml:"addr"`
	DashboardToken string `yaml:"dashboard_token,omitempty"`
	TrustedProxy   bool   `yaml:"trusted_proxy,omitempty"` // trust X-Forwarded-For for client IP (enable behind ALB/CloudFront)
}

// NaozhiSettingsConfig configures the naozhi-owned isolated Claude settings
// file (RFC naozhi-owned-settings-v3). Zero value = disabled.
type NaozhiSettingsConfig struct {
	// Enabled turns on the naozhi-owned settings file (default false).
	Enabled bool `yaml:"enabled,omitempty"`
	// Path overrides the file location; empty = default under the data root.
	Path string `yaml:"path,omitempty"`
}

type CLIConfig struct {
	// Backend names the default backend ("claude" (default) | "kiro"), used
	// when the dashboard does not pick one for a new session.
	Backend string `yaml:"backend"`
	Path    string `yaml:"path"`
	// Backends enumerates every backend to enable; empty = single-backend mode
	// using Backend/Path/Model/Args.
	Backends []CLIBackendConfig `yaml:"backends,omitempty"`
	Model    string             `yaml:"model"`
	Args     []string           `yaml:"args"`
	// MCPConfig is an absolute path to an MCP server definition file passed via
	// `--mcp-config`; empty passes no flag. Needed when NaozhiSettings is
	// enabled (that path suppresses ~/.claude.json mcpServers). Must be an
	// existing JSON file with an `mcpServers` object — cc refuses to start
	// otherwise, so cmd wiring validates and degrades to "no MCP". Inline JSON
	// is not supported. Recommended mode 0600: writers get arbitrary command
	// execution in every session. Deliberately global, not per-backend/agent.
	MCPConfig string `yaml:"mcp_config,omitempty"`
	// Effort is the default thinking-effort tier for backends that accept one
	// (kiro: low/medium/high/xhigh/max). Empty passes no flag so the backend
	// keeps its own default (docs/rfc/kiro-effort-control.md).
	Effort string `yaml:"effort,omitempty"`
}

// CLIBackendConfig configures one backend in a multi-backend deployment.
// ID is required; Path/Model/Args/Effort fall back to the top-level cli.* values.
type CLIBackendConfig struct {
	ID    string   `yaml:"id"`              // "claude" | "kiro"
	Path  string   `yaml:"path,omitempty"`  // overrides cli.path for this backend
	Model string   `yaml:"model,omitempty"` // overrides cli.model for this backend
	Args  []string `yaml:"args,omitempty"`  // overrides cli.args for this backend
	// Effort overrides cli.effort for this backend. On a backend without a
	// tier flag it is warned and dropped at startup, not a hard error, because
	// cli.effort propagates to EVERY backend via EnabledBackends.
	Effort string `yaml:"effort,omitempty"`
	// Models optionally declares the dashboard model-popover list for this
	// backend (mainly claude; kiro's agent-reported list wins). Each entry is
	// validated like `model`.
	Models []string `yaml:"models,omitempty"`
}

type SessionConfig struct {
	MaxProcs  int            `yaml:"max_procs"`
	TTL       string         `yaml:"ttl"`
	PruneTTL  string         `yaml:"prune_ttl"` // how long dead/suspended sessions stay in the list before removal
	Watchdog  WatchdogConfig `yaml:"watchdog"`
	Queue     QueueConfig    `yaml:"queue"`
	StorePath string         `yaml:"store_path"`
	CWD       string         `yaml:"cwd"` // default working directory for CLI processes
	// Deprecated: use CWD instead. Still parsed for existing config files.
	Workspace string     `yaml:"workspace"`
	Shim      ShimConfig `yaml:"shim"`
	// Deprecated: auto_chain has no effect (see AutoChainYAMLConfig); still
	// parsed so existing files load, with a one-line warning if set.
	AutoChain        AutoChainYAMLConfig        `yaml:"auto_chain,omitempty"`
	ProjectStableKey ProjectStableKeyYAMLConfig `yaml:"project_stable_key,omitempty"`
}

// ProjectStableKeyYAMLConfig controls the project-level stable session key
// (docs/rfc/project-stable-session-key.md). Default-on; when disabled the
// dashboard falls back to the timestamp-key path for "continue". Enabled is
// *bool so an absent key defaults true while `enabled: false` stays expressible.
type ProjectStableKeyYAMLConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// ResolvedEnabled returns the effective on/off flag.
func (c ProjectStableKeyYAMLConfig) ResolvedEnabled(def bool) bool {
	if c.Enabled == nil {
		return def
	}
	return *c.Enabled
}

// AutoChainYAMLConfig is the DEPRECATED, no-effect auto-workspace-chain block
// (docs/rfc/project-stable-session-key.md §9). Fields are parsed so old files
// load; nothing consumes them. Do NOT wire them back: "same slug + time window"
// chained unrelated sessions as each other's history. Enabled is *bool so the
// deprecation warn can tell an absent key from an explicit `enabled: false`.
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
	// MaxDepth is the max queued messages per session: nil = default (20),
	// 0 = disable queuing (drop + "please wait"), negative = 0.
	MaxDepth *int `yaml:"max_depth"`
	// CollectDelay is the wait after a turn completes before draining the
	// queue, so fast follow-ups batch together. Default "500ms".
	CollectDelay string `yaml:"collect_delay"`
	// Mode handles messages arriving mid-turn: "collect" (default) waits for
	// the turn; "interrupt" aborts it via control_request and sends the
	// coalesced follow-ups next. Only stream-json honours "interrupt"; ACP
	// falls back to "collect".
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

// hasPlatform reports whether the named platform has a configured section;
// unknown names are false rather than silently accepted.
func (c *Config) hasPlatform(name string) bool {
	switch name {
	case "feishu":
		return c.Platforms.Feishu != nil
	case "slack":
		return c.Platforms.Slack != nil
	case "discord":
		return c.Platforms.Discord != nil
	case "weixin":
		return c.Platforms.Weixin != nil
	default:
		return false
	}
}

type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	ConnectionMode    string `yaml:"connection_mode"` // "websocket" (default) | "webhook"
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLength    int    `yaml:"max_reply_length"`
	// AllowInsecureWebhook opts in to verification_token-only webhook mode (no
	// encrypt_key HMAC), which is replay/forgery-prone if the token leaks;
	// without it such a webhook refuses to start (#1507).
	AllowInsecureWebhook bool `yaml:"allow_insecure_webhook"`
}

// LogValue implements slog.LogValuer so the Feishu credentials never land in logs.
func (c FeishuConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("app_id", c.AppID),
		slog.String("app_secret", redactSecret(c.AppSecret)),
		slog.String("connection_mode", c.ConnectionMode),
		slog.String("verification_token", redactSecret(c.VerificationToken)),
		slog.String("encrypt_key", redactSecret(c.EncryptKey)),
		slog.Int("max_reply_length", c.MaxReplyLength),
		slog.Bool("allow_insecure_webhook", c.AllowInsecureWebhook),
	)
}

type CronConfig struct {
	StorePath        string `yaml:"store_path"`
	MaxJobs          int    `yaml:"max_jobs"`
	ExecutionTimeout string `yaml:"execution_timeout"`
	// Timezone is the IANA name (e.g. "Asia/Shanghai") for cron expressions.
	// Empty or "Local" uses local time (respects $TZ); "UTC" forces UTC.
	Timezone string `yaml:"timezone"`
	// NotifyDefault is the fallback IM target for jobs with Notify=true but no
	// per-job target; empty fields disable the default.
	NotifyDefault CronNotifyTarget `yaml:"notify_default,omitempty"`
	// JitterMax caps the random delay before each tick to flatten on-the-hour
	// bursts. Default 2m; "0" disables. Effective jitter is min(JitterMax,
	// period/4); TriggerNow bypasses it (docs/rfc/cron-v2-polish.md §3.2).
	JitterMax string `yaml:"jitter_max,omitempty"`
	// Sandbox enables AgentCore cloud-sandbox placement for cron jobs
	// (docs/rfc/agentcore-cloud-sandbox.md); both fields required. AWS
	// credentials come from the standard chain, never from this file.
	Sandbox CronSandboxConfig `yaml:"sandbox,omitempty"`
}

// CronSandboxConfig points cron's sandbox placement at an AgentCore Runtime
// (its container must run the naozhi bootstrap handler).
type CronSandboxConfig struct {
	RuntimeARN string `yaml:"runtime_arn"`
	Region     string `yaml:"region"`
}

// CronNotifyTarget identifies an IM channel used as the fallback delivery
// target for cron job completion notifications.
type CronNotifyTarget struct {
	Platform string `yaml:"platform"` // "feishu" / "slack" / "discord" / "weixin"
	ChatID   string `yaml:"chat_id"`
}

// UpdateConfig configures the in-process auto-update checker (GitHub
// Releases). Default Enabled=true, Mode="download": stage new releases but do
// NOT surprise-restart live sessions. Shares the selfupdate flow with `naozhi upgrade`.
type UpdateConfig struct {
	// Enabled is the master switch; nil defaults to true.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Mode on a newer release: "notify" (log + IM only), "download" (default;
	// replace binary, apply on next boot), "auto" (replace AND restart).
	// Unknown values fall back to "download".
	Mode string `yaml:"mode,omitempty"`

	// Interval between checks (Go duration). Default 6h; clamped up to the 1h
	// floor to protect GitHub from tight loops.
	Interval string `yaml:"interval,omitempty"`

	// CheckOnStart runs one check shortly after startup. Default false so a
	// restart loop on a bad release cannot immediately re-trigger an update.
	CheckOnStart bool `yaml:"check_on_start,omitempty"`

	// Notify is the IM target for update notices; empty disables IM delivery.
	Notify CronNotifyTarget `yaml:"notify,omitempty"`

	// DashboardInstall gates the dashboard "apply now" button; nil defaults to
	// true, false makes the apply endpoint 403 while the version chip keeps
	// working. Separate from Enabled: "no background install, but let me click
	// it" is a coherent policy.
	DashboardInstall *bool `yaml:"dashboard_install,omitempty"`
}

// UpdateEnabled reports whether the auto-update checker should run (nil = true).
func (c *Config) UpdateEnabled() bool {
	return c.Update.Enabled == nil || *c.Update.Enabled
}

// UpdateDashboardInstall reports whether the dashboard may trigger an
// install/restart (nil = true).
func (c *Config) UpdateDashboardInstall() bool {
	return c.Update.DashboardInstall == nil || *c.Update.DashboardInstall
}

// ImageOrientConfig configures auto-orientation of uploaded images lacking an
// EXIF orientation flag via a side vision call; best-effort and fail-safe (an
// unclear verdict leaves the image untouched).
type ImageOrientConfig struct {
	// Enabled gates the feature; nil defaults to TRUE.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Model overrides --model for the side vision call; empty lets the CLI
	// pick its Haiku-class default. Keep vendor-neutral — no Bedrock ARNs.
	Model string `yaml:"model,omitempty"`
}

// ImageOrientEnabled reports whether image auto-orientation should run (nil = true).
func (c *Config) ImageOrientEnabled() bool {
	return c.ImageOrient.Enabled == nil || *c.ImageOrient.Enabled
}

// SysessionConfig configures the system-session daemon framework
// (docs/rfc/system-session.md).
type SysessionConfig struct {
	// Enabled is the master switch; false (default) spins up no daemon
	// regardless of per-daemon flags.
	Enabled bool `yaml:"enabled,omitempty"`

	// TickTimeout caps a single Tick (DaemonRunTimedOut beyond it). Default 30s.
	TickTimeout string `yaml:"tick_timeout,omitempty"`

	// Runner configures the shared LLM-call abstraction; empty values fall
	// back to runtime defaults.
	Runner SysessionRunnerConfig `yaml:"runner,omitempty"`

	// Daemons holds per-daemon knobs keyed by compiled-in daemon name; unknown
	// keys are ignored for forward compatibility.
	Daemons map[string]SysessionDaemonConfig `yaml:"daemons,omitempty"`
}

// SysessionRunnerConfig configures the transient-system-session Runner.
type SysessionRunnerConfig struct {
	// Model overrides --model; empty leaves it off.
	Model string `yaml:"model,omitempty"`

	// WorkDir is the cwd for spawned subprocesses; empty defaults to
	// <dataDir>/sys-sessions/. MUST be 0700 — Runner enforces.
	WorkDir string `yaml:"work_dir,omitempty"`

	// JSONLMaxAge is the startup sweep retention window. Empty = 168h; "0"
	// disables the sweep.
	JSONLMaxAge string `yaml:"jsonl_max_age,omitempty"`
}

// SysessionDaemonConfig holds the common daemon fields plus daemon-private
// knobs; each daemon reads only the keys it understands.
type SysessionDaemonConfig struct {
	Enabled bool   `yaml:"enabled,omitempty"`
	Tick    string `yaml:"tick,omitempty"`

	// AutoTitler-specific fields.
	MinFirstTurns     int    `yaml:"min_first_turns,omitempty"`
	MinUserTurns      int    `yaml:"min_user_turns,omitempty"`
	MinRenameInterval string `yaml:"min_rename_interval,omitempty"`
	BatchPerTick      int    `yaml:"batch_per_tick,omitempty"`
	IncludeGroupChat  bool   `yaml:"include_group_chat,omitempty"`

	// attachment-gc fields (docs/rfc/attachment-gc-daemon.md). UploadTTL/RefTTL
	// "0" or unset = daemon default (NOT disable); PerRootCap 0 = 500; DryRun
	// logs would-removes; RunOnStart fires one sweep at startup.
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

// LogValue implements slog.LogValuer so the Slack tokens never land in logs.
func (c SlackConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("bot_token", redactSecret(c.BotToken)),
		slog.String("app_token", redactSecret(c.AppToken)),
		slog.Int("max_reply_length", c.MaxReplyLength),
	)
}

type DiscordConfig struct {
	BotToken       string `yaml:"bot_token"`
	MaxReplyLength int    `yaml:"max_reply_length"`
}

// LogValue implements slog.LogValuer so the Discord bot token never lands in logs.
func (c DiscordConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("bot_token", redactSecret(c.BotToken)),
		slog.Int("max_reply_length", c.MaxReplyLength),
	)
}

type WeixinConfig struct {
	Token          string `yaml:"token"`
	BaseURL        string `yaml:"base_url"`
	MaxReplyLength int    `yaml:"max_reply_length"`
}

// LogValue implements slog.LogValuer so the WeCom token never lands in logs.
func (c WeixinConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("token", redactSecret(c.Token)),
		slog.String("base_url", c.BaseURL),
		slog.Int("max_reply_length", c.MaxReplyLength),
	)
}

type TranscribeConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Region   string `yaml:"region"`
	Language string `yaml:"language"` // BCP-47, default: zh-CN
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	// The file carries secrets: reject symlinks (Lstat, so a link to a 0644
	// file cannot bypass the mode gate) and any group/world bit BEFORE reading;
	// the fd re-check below closes the Lstat→open TOCTOU window.
	if fi, statErr := os.Lstat(path); statErr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("config file %s is a symlink; refusing to load (resolve the link or point --config at the target directly)",
				path)
		}
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
	// Re-check on the open fd with the SAME 0o077 mask so a symlink swap in
	// the Lstat→OpenFile gap cannot load a permissive target. fd-stat failure
	// is fatal: skipping the gates would let an attacker who can interrupt
	// Fstat bypass the second check.
	fi, ferr := f.Stat()
	if ferr != nil {
		return nil, fmt.Errorf("stat config fd: %w", ferr)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("config file %s is not a regular file", path)
	}
	if fi.Mode()&0o077 != 0 {
		return nil, fmt.Errorf("config file %s is group/world-accessible (mode %04o); restrict with: chmod 0600 %s",
			path, fi.Mode().Perm(), path)
	}
	// 1 MiB cap: a runaway or hostile file must not be read whole into memory.
	const maxConfigBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("config file %s exceeds %d bytes", path, maxConfigBytes)
	}

	expanded := expandEnvVars(data)

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		// yaml.v3 echoes the offending line, which after ${VAR} expansion may
		// contain secrets; keep the detail in logs only.
		slog.Debug("config yaml parse failed", "err", err)
		return nil, fmt.Errorf("parse config: yaml syntax error (check naozhi logs for details)")
	}

	applyDefaults(&cfg)
	if err := parseDurations(&cfg); err != nil {
		return nil, err
	}
	// Before validation so the lifted value is validated as system_prompt
	// and the (now removed) flag is not reported by validateArgvStrings.
	if err := liftLegacySystemPromptArgs(&cfg); err != nil {
		return nil, err
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Normalize reconciles the Nodes / Workspaces YAML aliases so consumers read
// cfg.Nodes regardless of spelling. Load calls it; programmatic Config
// construction MUST call it too. When both are set Workspaces wins (with a
// warning). Idempotent.
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
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = CurrentSchemaVersion
	}
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = defaultServerAddr
	}
	if cfg.Session.MaxProcs <= 0 {
		cfg.Session.MaxProcs = sessionconst.DefaultMaxProcs
	}
	if cfg.Session.TTL == "" {
		cfg.Session.TTL = defaultSessionTTL.String()
	}
	if cfg.Session.PruneTTL == "" {
		cfg.Session.PruneTTL = defaultSessionPruneTTL.String()
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = defaultLogLevel
	}
	if cfg.Session.Queue.MaxDepth == nil {
		defaultDepth := defaultQueueMaxDepth
		cfg.Session.Queue.MaxDepth = &defaultDepth
	}
	if cfg.Session.Queue.CollectDelay == "" {
		cfg.Session.Queue.CollectDelay = defaultQueueCollectDelay.String()
	}
	if cfg.Session.Queue.Mode == "" {
		cfg.Session.Queue.Mode = defaultQueueMode
	}
	// Reconcile cwd / deprecated workspace on the operator's raw input — the
	// default must NOT be pre-filled first or a pure-default deployment would
	// trip the deprecation warning.
	if cfg.Session.CWD != "" {
		if cfg.Session.Workspace != "" && cfg.Session.Workspace != cfg.Session.CWD {
			slog.Warn("both 'session.cwd' and deprecated 'session.workspace' configured; using 'cwd'")
		}
		cfg.Session.Workspace = cfg.Session.CWD
	} else if cfg.Session.Workspace != "" {
		slog.Warn("'session.workspace' is deprecated, please rename to 'session.cwd'")
		cfg.Session.CWD = cfg.Session.Workspace
	} else {
		// Mirror the default into the alias so readers of either field work.
		cfg.Session.CWD = defaultSessionCWD
		cfg.Session.Workspace = defaultSessionCWD
	}

	if cfg.Session.AutoChain.Enabled != nil || cfg.Session.AutoChain.WindowHours != 0 || cfg.Session.AutoChain.Cap != 0 {
		slog.Warn("'session.auto_chain' is deprecated and has no effect; the feature was replaced by project-stable session keys — remove this block from config")
	}

	if cfg.UpdateEnabled() {
		if cfg.Update.Mode == "" {
			cfg.Update.Mode = "download"
		}
		switch cfg.Update.Mode {
		case "notify", "download", "auto":
		default:
			slog.Warn("update.mode unrecognized, falling back to download",
				"mode", cfg.Update.Mode)
			cfg.Update.Mode = "download"
		}
		if cfg.Update.Interval == "" {
			cfg.Update.Interval = "6h"
		}
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

	// Lowercase agent_commands keys: CJK mobile IMEs auto-capitalize "/Review".
	// Case conflicts keep the last-written value with a warning.
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
	if cfg.cachedTTL, err = parseDurationRequired(cfg.Session.TTL, "session.ttl", defaultSessionTTL); err != nil {
		return err
	}
	if cfg.cachedPruneTTL, err = parseDurationRequired(cfg.Session.PruneTTL, "session.prune_ttl", defaultSessionPruneTTL); err != nil {
		return err
	}
	if cfg.cachedNoOutputTimeout, err = parseDurationRequired(cfg.Session.Watchdog.NoOutputTimeout, "session.watchdog.no_output_timeout", defaultNoOutputTimeout); err != nil {
		return err
	}
	if cfg.cachedTotalTimeout, err = parseDurationRequired(cfg.Session.Watchdog.TotalTimeout, "session.watchdog.total_timeout", defaultTotalTimeout); err != nil {
		return err
	}
	if cfg.cachedExecTimeout, err = parseDurationRequired(cfg.Cron.ExecutionTimeout, "cron.execution_timeout", defaultCronExecTimeout); err != nil {
		return err
	}
	if cfg.cachedCollectDelay, err = parseDurationRequired(cfg.Session.Queue.CollectDelay, "session.queue.collect_delay", defaultQueueCollectDelay); err != nil {
		return err
	}
	if cfg.cachedJitterMax, err = parseDurationNonNegative(cfg.Cron.JitterMax, "cron.jitter_max", defaultCronJitterMax); err != nil {
		return err
	}
	// 硬上限 10m：clamp 并 warn，不把配置错误升成启动失败。
	if cfg.cachedJitterMax > cronJitterMaxHardCap {
		slog.Warn("cron.jitter_max exceeds 10m hard cap, clamping",
			"requested", cfg.cachedJitterMax, "cap", cronJitterMaxHardCap)
		cfg.cachedJitterMax = cronJitterMaxHardCap
	}
	if cfg.UpdateEnabled() {
		if cfg.cachedInterval, err = parseDurationRequired(cfg.Update.Interval, "update.interval", 6*time.Hour); err != nil {
			return err
		}
		// 1h floor protects GitHub; clamp + warn rather than fail.
		if cfg.cachedInterval < time.Hour {
			slog.Warn("update.interval below 1h floor, clamping",
				"requested", cfg.cachedInterval, "floor", time.Hour)
			cfg.cachedInterval = time.Hour
		}
	}
	return nil
}

// UpdateInterval returns the parsed, clamped auto-update check interval.
// Valid only when Update.Enabled; returns 0 otherwise.
func (c *Config) UpdateInterval() time.Duration { return c.cachedInterval }

func validateConfig(cfg *Config) error {
	// A newer schema would be silently mis-parsed (unknown keys dropped);
	// fail loud instead.
	if cfg.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("config schema_version %d is newer than this binary supports (max %d); upgrade naozhi or lower schema_version",
			cfg.SchemaVersion, CurrentSchemaVersion)
	}
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
		// verification_token-only auth lets a leaked token forge arbitrary
		// feishu events, so HARD FAIL unless NAOZHI_ALLOW_INSECURE_WEBHOOK=true
		// (CI/testing only). No loopback exemption: a webhook behind a tunnel
		// still receives internet-originating events (#1735).
		if cfg.Platforms.Feishu.ConnectionMode == "webhook" &&
			cfg.Platforms.Feishu.AllowInsecureWebhook &&
			cfg.Platforms.Feishu.EncryptKey == "" {
			if os.Getenv("NAOZHI_ALLOW_INSECURE_WEBHOOK") != "true" {
				return fmt.Errorf("feishu allow_insecure_webhook=true with no encrypt_key accepts forged events if the verification_token leaks (webhooks are reachable from the public internet, including loopback binds behind a tunnel); configure encrypt_key (recommended) or set NAOZHI_ALLOW_INSECURE_WEBHOOK=true to accept this risk (CI/testing only)")
			}
			slog.Error("SECURITY: feishu allow_insecure_webhook=true with no encrypt_key — webhook runs in verification_token-only mode (no HMAC); events are replay/forgery-prone if the token leaks. Running only because NAOZHI_ALLOW_INSECURE_WEBHOOK=true (CI/testing escape hatch).")
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
		// base_url flows into every platform HTTP call; pointing it at IMDS or
		// an internal service would turn long-poll/send into SSRF.
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
			// Literal-IP guard only; DNS-based SSRF needs a runtime Dialer hook
			// and the redirect variant is blocked by CheckRedirect elsewhere.
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
		if cfg.Upstream.Token == "your-secret-token" {
			return fmt.Errorf("upstream.token is set to the example placeholder \"your-secret-token\" — replace it with a real secret")
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

	// A notify platform typo would otherwise only surface when a notification
	// actually fires. Empty is legal (disables the default).
	if np := cfg.Cron.NotifyDefault.Platform; np != "" {
		if !cfg.hasPlatform(np) {
			return fmt.Errorf("cron.notify_default.platform %q is not a configured platform (set platforms.%s or clear notify_default)", np, np)
		}
	}
	if np := cfg.Update.Notify.Platform; np != "" {
		if !cfg.hasPlatform(np) {
			return fmt.Errorf("update.notify.platform %q is not a configured platform (set platforms.%s or clear update.notify)", np, np)
		}
	}

	// Configured argv never passes the user-input validators, so the
	// NUL/C0 and flag-injection gates are applied here for every field that
	// reaches exec.Command via BuildArgs (args, model, effort, prompts).
	if err := validateArgvStrings("cli.args", cfg.CLI.Args); err != nil {
		return err
	}
	if err := validateModelString("cli.model", cfg.CLI.Model); err != nil {
		return err
	}
	if err := validateEffortString("cli.effort", cfg.CLI.Effort); err != nil {
		return err
	}
	for _, b := range cfg.CLI.Backends {
		if err := validateArgvStrings(fmt.Sprintf("cli.backends[%s].args", b.ID), b.Args); err != nil {
			return err
		}
		if err := validateModelString(fmt.Sprintf("cli.backends[%s].model", b.ID), b.Model); err != nil {
			return err
		}
		if err := validateEffortString(fmt.Sprintf("cli.backends[%s].effort", b.ID), b.Effort); err != nil {
			return err
		}
		for i, m := range b.Models {
			if m == "" {
				return fmt.Errorf("cli.backends[%s].models[%d] is empty", b.ID, i)
			}
			if err := validateModelString(fmt.Sprintf("cli.backends[%s].models[%d]", b.ID, i), m); err != nil {
				return err
			}
		}
	}
	for id, a := range cfg.Agents {
		if err := validateArgvStrings(fmt.Sprintf("agents[%s].args", id), a.Args); err != nil {
			return err
		}
		if err := validateModelString(fmt.Sprintf("agents[%s].model", id), a.Model); err != nil {
			return err
		}
		if err := validateEffortString(fmt.Sprintf("agents[%s].effort", id), a.Effort); err != nil {
			return err
		}
		if err := validateSystemPrompt(fmt.Sprintf("agents[%s].system_prompt", id), a.SystemPrompt); err != nil {
			return err
		}
	}
	if err := validateModelString("projects.planner_defaults.model", cfg.Projects.PlannerDefaults.Model); err != nil {
		return err
	}
	if err := validateModelString("image_orient.model", cfg.ImageOrient.Model); err != nil {
		return err
	}
	if err := validateModelString("sysession.runner.model", cfg.Sysession.Runner.Model); err != nil {
		return err
	}
	// 与 project.PlannerPrompt 同源进 --append-system-prompt argv；LF/CR 同样
	// 禁止，多行 prompt 须经 CLAUDE.md 引入。
	if err := validatePlannerPrompt("projects.planner_defaults.prompt", cfg.Projects.PlannerDefaults.Prompt); err != nil {
		return err
	}

	if err := validateAccessProfiles(cfg); err != nil {
		return err
	}

	return nil
}

// knownBackendIDs returns the set of enabled backend IDs (never empty) for
// referential validation of `backend` fields.
func (c *Config) knownBackendIDs() map[string]bool {
	ids := make(map[string]bool)
	for _, b := range c.EnabledBackends() {
		ids[b.ID] = true
	}
	return ids
}

// validateAccessProfiles checks env overlays (envpolicy.ValidateOverlayEntry)
// and that every backend / access_profile reference names an enabled or
// defined entry. Unknown names are ERRORS: a typo silently billing a personal
// project to a company account is the mis-charge this feature prevents.
func validateAccessProfiles(cfg *Config) error {
	backends := cfg.knownBackendIDs()
	for name, ap := range cfg.AccessProfiles {
		for k, v := range ap.Env {
			if err := envpolicy.ValidateOverlayEntry(k, v); err != nil {
				return fmt.Errorf("access_profiles[%s].env: %w", name, err)
			}
		}
		if ap.DefaultBackend != "" && !backends[ap.DefaultBackend] {
			return fmt.Errorf("access_profiles[%s].default_backend %q is not an enabled backend", name, ap.DefaultBackend)
		}
		if err := validateModelString(fmt.Sprintf("access_profiles[%s].default_model", name), ap.DefaultModel); err != nil {
			return err
		}
	}
	for id, a := range cfg.Agents {
		if a.Backend != "" && !backends[a.Backend] {
			return fmt.Errorf("agents[%s].backend %q is not an enabled backend", id, a.Backend)
		}
		if a.AccessProfile != "" {
			if _, ok := cfg.AccessProfiles[a.AccessProfile]; !ok {
				return fmt.Errorf("agents[%s].access_profile %q is not defined in access_profiles", id, a.AccessProfile)
			}
		}
	}
	if cfg.DefaultAccessProfile != "" {
		if _, ok := cfg.AccessProfiles[cfg.DefaultAccessProfile]; !ok {
			return fmt.Errorf("default_access_profile %q is not defined in access_profiles", cfg.DefaultAccessProfile)
		}
	}
	return nil
}

// validatePlannerPrompt mirrors project.ValidateConfig's PlannerPrompt policy:
// reject NUL, all C0 (incl. LF/CR), DEL, C1, bidi and LS/PS; empty is allowed.
// Shares project.MaxPlannerPromptBytes so the caps cannot drift.
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

// validateArgvStrings rejects empty elements (YAML "- " typo), NUL and control
// bytes in argv. It WARNS (not errors) on flags cli.BuildArgs would strip as
// denied, naming the field so the operator can move the value to its dedicated
// config key (#2493); `--append-system-prompt` under agents[].args is lifted
// by liftLegacySystemPromptArgs before this runs.
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
		if cli.IsDeniedExtraFlag(a) {
			slog.Warn("config: args contains a flag the spawn pipeline strips; it will NOT reach the CLI — use the dedicated config field instead",
				"field", fmt.Sprintf("%s[%d]", field, i), "flag", a)
		}
	}
	return nil
}

// The validators live in internal/tuningspec (leaf) so the session layer can
// reuse them without importing config (which would cycle).

// validateEffortString gates a configured thinking-effort tier; empty means
// "pass no flag".
func validateEffortString(field, value string) error {
	return tuningspec.ValidateEffort(field, value)
}

// validateModelString gates a configured model identifier; empty is allowed
// (caller-defined fallback).
func validateModelString(field, value string) error {
	return tuningspec.ValidateModel(field, value)
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

// parseDurationNonNegative 允许 "0" 作为显式关闭的合法值；空字符串返回 fallback。
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

// EnabledBackends returns the normalized list of backends to enable: cli.backends
// if set, else the single cli.backend (default "claude"). The default backend is
// always at position 0; duplicate IDs collapse to the first occurrence.
func (c *Config) EnabledBackends() []CLIBackendConfig {
	// Must resolve identically to DefaultBackendID so [0].ID agrees with it.
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
			ID:     defaultID,
			Path:   c.CLI.Path,
			Model:  c.CLI.Model,
			Args:   c.CLI.Args,
			Effort: c.CLI.Effort,
		}}
	}

	seen := make(map[string]bool, len(c.CLI.Backends))
	out := make([]CLIBackendConfig, 0, len(c.CLI.Backends))
	for _, b := range c.CLI.Backends {
		if b.ID == "" || seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		if b.Model == "" {
			b.Model = c.CLI.Model
		}
		if len(b.Args) == 0 {
			b.Args = c.CLI.Args
		}
		if b.Effort == "" {
			b.Effort = c.CLI.Effort
		}
		out = append(out, b)
	}

	// All entries had empty IDs: fall back to single-backend mode.
	if len(out) == 0 {
		return []CLIBackendConfig{{
			ID:     defaultID,
			Path:   c.CLI.Path,
			Model:  c.CLI.Model,
			Args:   c.CLI.Args,
			Effort: c.CLI.Effort,
		}}
	}

	// Default backend floats to position 0 regardless of YAML order.
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

// QueueMaxDepth returns the resolved queue max depth; negative values clamp
// to 0 so Enqueue's `len(msgs) >= maxDepth` guard stays sane.
func (c *Config) QueueMaxDepth() int {
	if c.Session.Queue.MaxDepth == nil {
		return 20
	}
	if d := *c.Session.Queue.MaxDepth; d > 0 {
		return d
	}
	return 0
}

// QueueMode returns the raw queue mode string; callers normalise via
// dispatch.ParseQueueMode (a string here avoids a config → dispatch cycle).
func (c *Config) QueueMode() string {
	return c.Session.Queue.Mode
}

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// allowEnvExpansion reports whether key is safe to expand in config.yaml; on
// false the caller leaves the placeholder intact so the secret never reaches
// the in-memory Config. Policy lives in the envpolicy Table
// (SourceExpansion): upstream-credential and secret-suffixed names are
// refused, naozhi-owned namespaces are the legitimate config inputs,
// everything else expands.
func allowEnvExpansion(key string) bool {
	_, ok := envpolicy.Allowed(key, envpolicy.SourceExpansion)
	return ok
}

// expandEnvVars resolves ${VAR} placeholders in the YAML payload. Denied names
// (allowEnvExpansion) and values containing control bytes (which could inject
// sibling YAML keys, #637) are left as literal placeholders so the bad config
// fails containsEnvPlaceholder validation loudly instead of leaking or forging.
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

// containsYAMLBreakingByte reports whether s has a byte that could break a
// YAML token's scope when substituted raw: newlines are the vector, other
// control bytes and tab (illegal in YAML indentation) are refused defensively.
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

// CostConfig tunes the cost ledger (docs/rfc/cost-ledger.md §9). Enabled
// defaults to true; the ledger lives beside session.store_path, so it is also
// off when that is empty. Out-of-range day counts are clamped by the ledger.
type CostConfig struct {
	Enabled       *bool `yaml:"enabled,omitempty"`
	RetentionDays int   `yaml:"retention_days,omitempty"`
	RollupDays    int   `yaml:"rollup_days,omitempty"`
}

// IsEnabled resolves the tri-state Enabled flag (nil = true).
func (c CostConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }
