package config

import (
	"log/slog"
	"strings"
	"time"
)

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
