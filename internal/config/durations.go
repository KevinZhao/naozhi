package config

// Duration parsing: every `*.ttl` / `*.timeout` / `*.interval` string in the
// config becomes a time.Duration exactly once, at load, so no reader has to
// parse (and mis-handle) it later. Split out of config.go (#2710 J11).

import (
	"fmt"
	"log/slog"
	"time"
)

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
