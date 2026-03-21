package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig           `yaml:"server"`
	CLI           CLIConfig              `yaml:"cli"`
	Session       SessionConfig          `yaml:"session"`
	Platforms     PlatformConfigs        `yaml:"platforms"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	AgentCommands map[string]string      `yaml:"agent_commands"`
	Log           LogConfig              `yaml:"log"`
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
}

type WatchdogConfig struct {
	NoOutputTimeout string `yaml:"no_output_timeout"`
	TotalTimeout    string `yaml:"total_timeout"`
}

type PlatformConfigs struct {
	Feishu  *FeishuConfig  `yaml:"feishu"`
	Slack   *SlackConfig   `yaml:"slack"`
	Discord *DiscordConfig `yaml:"discord"`
}

type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	ConnectionMode    string `yaml:"connection_mode"` // "websocket" (default) | "webhook"
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLength    int    `yaml:"max_reply_length"`
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
	noOutputTimeout, _ = time.ParseDuration(c.Session.Watchdog.NoOutputTimeout)
	if noOutputTimeout <= 0 {
		noOutputTimeout = 2 * time.Minute
	}
	totalTimeout, _ = time.ParseDuration(c.Session.Watchdog.TotalTimeout)
	if totalTimeout <= 0 {
		totalTimeout = 5 * time.Minute
	}
	return
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
