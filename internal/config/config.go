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
	cachedNoOutputTimeout time.Duration `yaml:"-"`
	cachedTotalTimeout    time.Duration `yaml:"-"`
	cachedExecTimeout     time.Duration `yaml:"-"`
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
	Watchdog  WatchdogConfig `yaml:"watchdog"`
	StorePath string         `yaml:"store_path"`
	CWD       string         `yaml:"cwd"`       // default working directory for CLI processes
	Workspace string         `yaml:"workspace"` // deprecated alias for cwd (backward compat)
	Shim      ShimConfig     `yaml:"shim"`
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

	// Apply defaults
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = ":8080"
	}
	if cfg.Session.MaxProcs <= 0 {
		cfg.Session.MaxProcs = 3
	}
	if cfg.Session.TTL == "" {
		cfg.Session.TTL = "30m"
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Session.Workspace == "" {
		cfg.Session.Workspace = "~/.naozhi/workspace"
	}
	// cwd takes precedence over deprecated workspace field
	if cfg.Session.CWD != "" {
		if cfg.Session.Workspace != "" && cfg.Session.Workspace != cfg.Session.CWD {
			slog.Warn("both 'session.cwd' and deprecated 'session.workspace' configured; using 'cwd'")
		}
		cfg.Session.Workspace = cfg.Session.CWD
	} else {
		cfg.Session.CWD = cfg.Session.Workspace
	}

	// Merge workspaces → nodes (workspaces is the preferred name; nodes is deprecated)
	if len(cfg.Workspaces) > 0 && len(cfg.Nodes) == 0 {
		cfg.Nodes = cfg.Workspaces
	} else if len(cfg.Nodes) > 0 && len(cfg.Workspaces) == 0 {
		slog.Warn("'nodes' config key is deprecated, please rename to 'workspaces'")
		cfg.Workspaces = cfg.Nodes
	} else if len(cfg.Workspaces) > 0 && len(cfg.Nodes) > 0 {
		slog.Warn("both 'nodes' and 'workspaces' configured; using 'workspaces', ignoring 'nodes'")
		cfg.Nodes = cfg.Workspaces
	}

	// Workspace identity defaults
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

	// Parse and cache durations (validates and caches in one step)
	var durErr error
	if cfg.cachedTTL, durErr = parseDurationRequired(cfg.Session.TTL, "session.ttl", 30*time.Minute); durErr != nil {
		return nil, durErr
	}
	if cfg.cachedNoOutputTimeout, durErr = parseDurationRequired(cfg.Session.Watchdog.NoOutputTimeout, "session.watchdog.no_output_timeout", 2*time.Minute); durErr != nil {
		return nil, durErr
	}
	if cfg.cachedTotalTimeout, durErr = parseDurationRequired(cfg.Session.Watchdog.TotalTimeout, "session.watchdog.total_timeout", 5*time.Minute); durErr != nil {
		return nil, durErr
	}
	if cfg.cachedExecTimeout, durErr = parseDurationRequired(cfg.Cron.ExecutionTimeout, "cron.execution_timeout", 5*time.Minute); durErr != nil {
		return nil, durErr
	}

	// Warn if config values contain unexpanded env var placeholders
	if cfg.Platforms.Feishu != nil {
		if containsEnvPlaceholder(cfg.Platforms.Feishu.AppID) || containsEnvPlaceholder(cfg.Platforms.Feishu.AppSecret) {
			return nil, fmt.Errorf("feishu app_id or app_secret contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Feishu.AppID == "" || cfg.Platforms.Feishu.AppSecret == "" {
			return nil, fmt.Errorf("feishu app_id and app_secret are required")
		}
		// Webhook mode requires at least one auth mechanism to prevent unauthenticated access
		if cfg.Platforms.Feishu.ConnectionMode == "webhook" &&
			cfg.Platforms.Feishu.VerificationToken == "" && cfg.Platforms.Feishu.EncryptKey == "" {
			return nil, fmt.Errorf("feishu webhook mode requires at least one of verification_token or encrypt_key to be set")
		}
	}
	if cfg.Platforms.Slack != nil {
		if containsEnvPlaceholder(cfg.Platforms.Slack.BotToken) {
			return nil, fmt.Errorf("slack bot_token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Slack.BotToken == "" {
			return nil, fmt.Errorf("slack bot_token is required")
		}
	}
	if cfg.Platforms.Discord != nil {
		if containsEnvPlaceholder(cfg.Platforms.Discord.BotToken) {
			return nil, fmt.Errorf("discord bot_token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Discord.BotToken == "" {
			return nil, fmt.Errorf("discord bot_token is required")
		}
	}
	if cfg.Platforms.Weixin != nil {
		if containsEnvPlaceholder(cfg.Platforms.Weixin.Token) {
			return nil, fmt.Errorf("weixin token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Weixin.Token == "" {
			return nil, fmt.Errorf("weixin token is required")
		}
	}

	// Validate node configs
	for id, nc := range cfg.Nodes {
		if nc.URL == "" {
			return nil, fmt.Errorf("node %q: url is required", id)
		}
		if strings.HasSuffix(nc.URL, "/") {
			return nil, fmt.Errorf("node %q: url must not have trailing slash", id)
		}
		u, err := url.Parse(nc.URL)
		if err != nil {
			return nil, fmt.Errorf("node %q: invalid url: %w", id, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("node %q: url must be http or https", id)
		}
		if u.Scheme == "http" && nc.Token != "" {
			return nil, fmt.Errorf("node %q: refusing to send bearer token over plaintext HTTP — use HTTPS", id)
		}
	}

	if cfg.Upstream != nil {
		if cfg.Upstream.URL == "" {
			return nil, fmt.Errorf("upstream.url is required")
		}
		if !strings.HasPrefix(cfg.Upstream.URL, "wss://") && !strings.HasPrefix(cfg.Upstream.URL, "ws://") {
			return nil, fmt.Errorf("upstream.url must use ws:// or wss:// scheme")
		}
		if cfg.Upstream.NodeID == "" {
			return nil, fmt.Errorf("upstream.node_id is required")
		}
		if cfg.Upstream.Token == "" {
			return nil, fmt.Errorf("upstream.token is required")
		}
	}

	for id, entry := range cfg.ReverseNodes {
		if entry.Token == "" {
			return nil, fmt.Errorf("reverse_nodes %q: token is required", id)
		}
		if containsEnvPlaceholder(entry.Token) {
			return nil, fmt.Errorf("reverse_nodes %q: token contains unexpanded ${VAR} — check environment variables", id)
		}
	}

	return &cfg, nil
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

// ParseWatchdog returns the watchdog timeout durations (cached after Load).
func (c *Config) ParseWatchdog() (noOutputTimeout, totalTimeout time.Duration) {
	return c.cachedNoOutputTimeout, c.cachedTotalTimeout
}

// ParseExecutionTimeout returns the cron execution timeout duration (cached after Load).
func (c *Config) ParseExecutionTimeout() time.Duration {
	return c.cachedExecTimeout
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
