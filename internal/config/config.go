package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig                `yaml:"server"`
	CLI           CLIConfig                   `yaml:"cli"`
	Session       SessionConfig               `yaml:"session"`
	Platforms     PlatformConfigs             `yaml:"platforms"`
	Agents        map[string]AgentConfig      `yaml:"agents"`
	AgentCommands map[string]string           `yaml:"agent_commands"`
	Nodes         map[string]NodeConfig       `yaml:"nodes"`
	Workspaces    map[string]NodeConfig       `yaml:"workspaces"` // alias for nodes (preferred name)
	ReverseNodes  map[string]ReverseNodeEntry `yaml:"reverse_nodes"`
	Upstream      *UpstreamConfig             `yaml:"upstream"`
	Workspace     WorkspaceConfig             `yaml:"workspace"` // local workspace identity
	Transcribe    *TranscribeConfig           `yaml:"transcribe"`
	Cron          CronConfig                  `yaml:"cron"`
	Log           LogConfig                   `yaml:"log"`
	Projects      ProjectsConfig              `yaml:"projects"`

	// Cached parsed durations (populated once in Load, avoids repeated ParseDuration)
	cachedTTL             time.Duration `yaml:"-"`
	cachedPruneTTL        time.Duration `yaml:"-"`
	cachedNoOutputTimeout time.Duration `yaml:"-"`
	cachedTotalTimeout    time.Duration `yaml:"-"`
	cachedExecTimeout     time.Duration `yaml:"-"`
	cachedCollectDelay    time.Duration `yaml:"-"`
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
	Backend string   `yaml:"backend"` // "claude" (default) | "kiro"
	Path    string   `yaml:"path"`
	Model   string   `yaml:"model"`
	Args    []string `yaml:"args"`
}

type SessionConfig struct {
	MaxProcs  int            `yaml:"max_procs"`
	TTL       string         `yaml:"ttl"`
	PruneTTL  string         `yaml:"prune_ttl"` // how long dead/suspended sessions stay in the list before removal
	Watchdog  WatchdogConfig `yaml:"watchdog"`
	Queue     QueueConfig    `yaml:"queue"`
	StorePath string         `yaml:"store_path"`
	CWD       string         `yaml:"cwd"`       // default working directory for CLI processes
	Workspace string         `yaml:"workspace"` // deprecated alias for cwd (backward compat)
	Shim      ShimConfig     `yaml:"shim"`
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
}

type CronConfig struct {
	StorePath        string `yaml:"store_path"`
	MaxJobs          int    `yaml:"max_jobs"`
	ExecutionTimeout string `yaml:"execution_timeout"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // "json" (default) | "text"
	File   string `yaml:"file"`
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
	Provider string `yaml:"provider"` // "aws" (default)
	Region   string `yaml:"region"`
	Language string `yaml:"language"` // BCP-47, default: zh-CN
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	// Reject config file if readable by group or others BEFORE reading its
	// contents into memory — the file may contain secrets (app_secret, tokens).
	if fi, statErr := os.Stat(path); statErr == nil {
		if fi.Mode()&0o044 != 0 {
			return nil, fmt.Errorf("config file %s is group/world-readable (mode %04o); restrict with: chmod 0600 %s",
				path, fi.Mode().Perm(), path)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Expand ${VAR} environment variables
	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
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

func applyDefaults(cfg *Config) {
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = ":8080"
	}
	if cfg.Session.MaxProcs <= 0 {
		cfg.Session.MaxProcs = 3
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
	if cfg.Session.CWD != "" {
		if cfg.Session.Workspace != "" && cfg.Session.Workspace != cfg.Session.CWD {
			slog.Warn("both 'session.cwd' and deprecated 'session.workspace' configured; using 'cwd'")
		}
		cfg.Session.Workspace = cfg.Session.CWD
	} else {
		cfg.Session.CWD = cfg.Session.Workspace
	}

	if len(cfg.Workspaces) > 0 && len(cfg.Nodes) == 0 {
		cfg.Nodes = cfg.Workspaces
	} else if len(cfg.Nodes) > 0 && len(cfg.Workspaces) == 0 {
		slog.Warn("'nodes' config key is deprecated, please rename to 'workspaces'")
		cfg.Workspaces = cfg.Nodes
	} else if len(cfg.Workspaces) > 0 && len(cfg.Nodes) > 0 {
		slog.Warn("both 'nodes' and 'workspaces' configured; using 'workspaces', ignoring 'nodes'")
		cfg.Nodes = cfg.Workspaces
	}

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
		if !strings.HasPrefix(cfg.Upstream.URL, "wss://") && !strings.HasPrefix(cfg.Upstream.URL, "ws://") {
			return fmt.Errorf("upstream.url must use ws:// or wss:// scheme")
		}
		if strings.HasPrefix(cfg.Upstream.URL, "ws://") {
			return fmt.Errorf("upstream.url must use wss:// — refusing to send bearer token over plaintext ws://")
		}
		if cfg.Upstream.NodeID == "" {
			return fmt.Errorf("upstream.node_id is required")
		}
		if cfg.Upstream.Token == "" {
			return fmt.Errorf("upstream.token is required")
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

// ParseCollectDelay returns the queue collect delay (cached after Load).
func (c *Config) ParseCollectDelay() time.Duration {
	return c.cachedCollectDelay
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

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

func expandEnvVars(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		key := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return match
	})
}

func containsEnvPlaceholder(s string) bool {
	return strings.Contains(s, "${")
}
