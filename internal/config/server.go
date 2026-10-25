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
	// StdioMaxSize caps the files launchd/systemd redirect stdout and stderr
	// into, e.g. "64MB" (the default); "0" disables. See datadir.CapStdio.
	StdioMaxSize string `yaml:"stdio_max_size"`
}

// LogStdioMaxSize is log.stdio_max_size in bytes: 64MB by default, 0 when it
// is "0" (the cap is off).
func (c *Config) LogStdioMaxSize() int64 {
	if c.stdioCapOff {
		return 0
	}
	return orDefault(c.cachedStdioMaxSize, defaultStdioMaxSize)
}

// UpdateInterval returns the parsed, clamped auto-update check interval.
// Valid only when Update.Enabled; returns 0 otherwise.
func (c *Config) UpdateInterval() time.Duration { return c.cachedInterval }

// minDashboardTokenLen is the shortest dashboard_token Load accepts;
// tokens under weakDashboardTokenLen load with a warning.
const (
	minDashboardTokenLen  = 8
	weakDashboardTokenLen = 16
)

// validateServer refuses a dashboard token that is an unexpanded ${VAR} — it
// would be compared literally and any caller could guess it — or shorter than
// minDashboardTokenLen, and logs WarnDashboardToken for the rest. Split out of
// validateConfig (#2710).
func validateServer(cfg *Config) error {
	token := cfg.Server.DashboardToken
	if token != "" {
		if containsEnvPlaceholder(token) {
			// Refuse to start with a literal "${VAR}" string as the dashboard
			// credential: the placeholder is readable in the repository, so
			// anyone who ever sees the config knows the login token.
			return fmt.Errorf("server.dashboard_token contains unexpanded ${VAR} — check environment variables (refusing to run with a guessable token)")
		}
		if len(token) < minDashboardTokenLen {
			return fmt.Errorf("server.dashboard_token is too short — use at least %d characters", minDashboardTokenLen)
		}
	}
	WarnDashboardToken(token)
	return nil
}

// WarnDashboardToken logs a warning for an accepted dashboard_token that is
// empty or shorter than weakDashboardTokenLen. Load calls it before the
// server installs its log handler, so the server calls it again afterwards.
func WarnDashboardToken(token string) {
	switch {
	case token == "":
		slog.Warn("SECURITY: dashboard_token is empty — all dashboard API endpoints are accessible without authentication",
			"hint", "set NAOZHI_DASHBOARD_TOKEN or dashboard_token in config")
	case len(token) < weakDashboardTokenLen:
		slog.Warn("dashboard_token is short — consider using 16+ random characters for stronger security")
	}
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
			reportFallback("update.mode", "fallback", "not notify, download or auto; updates run in download mode")
			cfg.Update.Mode = "download"
		}
		if cfg.Update.Interval == "" {
			cfg.Update.Interval = "6h"
		}
	}
}
