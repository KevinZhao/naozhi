package config

import "time"

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

// SysessionDaemonDurations are one daemon's parsed duration knobs. A zero
// field was unset or unusable, and the consumer's default applies.
type SysessionDaemonDurations struct {
	Tick, MinRenameInterval, UploadTTL, RefTTL time.Duration
}

// sysessionDurations caches the sysession strings resolveFallbackValues parsed.
type sysessionDurations struct {
	tickTimeout   time.Duration
	jsonlMaxAge   time.Duration
	jsonlSweepOff bool
	daemons       map[string]SysessionDaemonDurations
}

// SysessionTickTimeout is the cap on one daemon Tick (default 30s).
func (c *Config) SysessionTickTimeout() time.Duration {
	return orDefault(c.cachedSysession.tickTimeout, defaultSysessionTick)
}

// SysessionDaemonDurations returns daemon name's knobs with Tick defaulted to
// 30s; the other fields stay zero when unset.
func (c *Config) SysessionDaemonDurations(name string) SysessionDaemonDurations {
	d := c.cachedSysession.daemons[name]
	d.Tick = orDefault(d.Tick, defaultSysessionTick)
	return d
}

// SysessionJSONLMaxAge is the sys-sessions/*.jsonl retention window: 7 days by
// default, 0 when jsonl_max_age is "0" (the sweep is off).
func (c *Config) SysessionJSONLMaxAge() time.Duration {
	if c.cachedSysession.jsonlSweepOff {
		return 0
	}
	return orDefault(c.cachedSysession.jsonlMaxAge, defaultSysessionJSONLMaxAge)
}
