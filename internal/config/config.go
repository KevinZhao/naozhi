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
	Addr string `yaml:"addr"`
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
	Level string `yaml:"level"`
	File  string `yaml:"file"`
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
	if cfg.CLI.Model == "" {
		cfg.CLI.Model = "sonnet"
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

	// Merge workspaces → nodes (workspaces is the preferred name)
	if len(cfg.Workspaces) > 0 && len(cfg.Nodes) == 0 {
		cfg.Nodes = cfg.Workspaces
	} else if len(cfg.Workspaces) > 0 && len(cfg.Nodes) > 0 {
		slog.Warn("both 'nodes' and 'workspaces' configured; using 'nodes', ignoring 'workspaces'")
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

	// Validate duration fields
	if cfg.Session.TTL != "" {
		if _, err := time.ParseDuration(cfg.Session.TTL); err != nil {
			return nil, fmt.Errorf("invalid session.ttl %q: %w", cfg.Session.TTL, err)
		}
	}
	if cfg.Session.Watchdog.NoOutputTimeout != "" {
		if _, err := time.ParseDuration(cfg.Session.Watchdog.NoOutputTimeout); err != nil {
			return nil, fmt.Errorf("invalid session.watchdog.no_output_timeout %q: %w", cfg.Session.Watchdog.NoOutputTimeout, err)
		}
	}
	if cfg.Session.Watchdog.TotalTimeout != "" {
		if _, err := time.ParseDuration(cfg.Session.Watchdog.TotalTimeout); err != nil {
			return nil, fmt.Errorf("invalid session.watchdog.total_timeout %q: %w", cfg.Session.Watchdog.TotalTimeout, err)
		}
	}

	// Warn if config values contain unexpanded env var placeholders
	if cfg.Platforms.Feishu != nil {
		if containsEnvPlaceholder(cfg.Platforms.Feishu.AppID) || containsEnvPlaceholder(cfg.Platforms.Feishu.AppSecret) {
			return nil, fmt.Errorf("feishu app_id or app_secret contains unexpanded ${VAR} — check environment variables")
		}
	}
	if cfg.Platforms.Slack != nil {
		if containsEnvPlaceholder(cfg.Platforms.Slack.BotToken) {
			return nil, fmt.Errorf("slack bot_token contains unexpanded ${VAR} — check environment variables")
		}
	}
	if cfg.Platforms.Discord != nil {
		if containsEnvPlaceholder(cfg.Platforms.Discord.BotToken) {
			return nil, fmt.Errorf("discord bot_token contains unexpanded ${VAR} — check environment variables")
		}
	}
	if cfg.Platforms.Weixin != nil {
		if containsEnvPlaceholder(cfg.Platforms.Weixin.Token) {
			return nil, fmt.Errorf("weixin token contains unexpanded ${VAR} — check environment variables")
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
	}

	return &cfg, nil
}

// ParseTTL parses the TTL string into a time.Duration.
func (c *Config) ParseTTL() time.Duration {
	d, err := time.ParseDuration(c.Session.TTL)
	if err != nil {
		return 30 * time.Minute
	}
	return d
}

// ParseWatchdog returns the watchdog timeout durations.
func (c *Config) ParseWatchdog() (noOutputTimeout, totalTimeout time.Duration) {
	if c.Session.Watchdog.NoOutputTimeout != "" {
		var err error
		noOutputTimeout, err = time.ParseDuration(c.Session.Watchdog.NoOutputTimeout)
		if err != nil {
			slog.Warn("invalid watchdog.no_output_timeout, using default", "value", c.Session.Watchdog.NoOutputTimeout, "err", err)
		}
	}
	if noOutputTimeout <= 0 {
		noOutputTimeout = 2 * time.Minute
	}
	if c.Session.Watchdog.TotalTimeout != "" {
		var err error
		totalTimeout, err = time.ParseDuration(c.Session.Watchdog.TotalTimeout)
		if err != nil {
			slog.Warn("invalid watchdog.total_timeout, using default", "value", c.Session.Watchdog.TotalTimeout, "err", err)
		}
	}
	if totalTimeout <= 0 {
		totalTimeout = 5 * time.Minute
	}
	return
}

// ParseExecutionTimeout returns the cron execution timeout duration.
func (c *Config) ParseExecutionTimeout() time.Duration {
	if c.Cron.ExecutionTimeout == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(c.Cron.ExecutionTimeout)
	if err != nil {
		return 5 * time.Minute
	}
	return d
}

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

func expandEnvVars(s string) string {
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
