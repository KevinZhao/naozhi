package config

import (
	"fmt"
	"log/slog"
	"time"
)

type ServerConfig struct {
	Addr           string `yaml:"addr"`
	DashboardToken string `yaml:"dashboard_token,omitempty"`
	TrustedProxy   bool   `yaml:"trusted_proxy,omitempty"` // trust X-Forwarded-For for client IP (enable behind ALB/CloudFront)
	// DebugMode registers /api/debug/pprof and /api/debug/vars. Default false:
	// both 404 even for loopback+auth callers, so a leaked dashboard token
	// cannot enumerate goroutine stacks (which carry file paths and queue
	// contents) or expvar counters. Turn it on only while capturing a profile.
	//
	// R244-SEC-P3-1 specified this key and the review record said it shipped,
	// but only the ServerOptions field existed — nothing ever read a config
	// value into it, so the endpoints were unreachable regardless of config.
	// Wired for real in #2553's follow-up; the gates it feeds (requireAuth +
	// loopback-only + a refusal when dashboard_token is empty) were already
	// there.
	DebugMode bool `yaml:"debug_mode,omitempty"`
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

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // "json" (default) | "text"
}

// UpdateInterval returns the parsed, clamped auto-update check interval.
// Valid only when Update.Enabled; returns 0 otherwise.
func (c *Config) UpdateInterval() time.Duration { return c.cachedInterval }

// validateServer checks the dashboard token: empty is legal but logged as a
// SECURITY warning, while an unexpanded ${VAR} is refused — it would be compared
// literally and any caller could guess it. Split out of validateConfig (#2710).
func validateServer(cfg *Config) error {
	if cfg.Server.DashboardToken == "" {
		slog.Warn("SECURITY: dashboard_token is empty — all dashboard API endpoints are accessible without authentication",
			"hint", "set NAOZHI_DASHBOARD_TOKEN or dashboard_token in config")
	} else if containsEnvPlaceholder(cfg.Server.DashboardToken) {
		// Refuse to start with a literal "${VAR}" string as the dashboard
		// credential: the placeholder is readable in the repository, so
		// anyone who ever sees the config knows the login token.
		return fmt.Errorf("server.dashboard_token contains unexpanded ${VAR} — check environment variables (refusing to run with a guessable token)")
	}
	return nil
}

// applyServerDefaults fills the listen address and the log level. Split out of
// applyDefaults (#2710 J11); the call order in applyDefaults is unchanged.
func applyServerDefaults(cfg *Config) {
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = defaultServerAddr
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = defaultLogLevel
	}
}

// applyUpdateDefaults fills the self-update mode and interval, and falls back to
// "download" for an unrecognized mode rather than refusing to start. Only runs
// when updates are enabled. Split out of applyDefaults (#2710 J11).
func applyUpdateDefaults(cfg *Config) {
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
}
