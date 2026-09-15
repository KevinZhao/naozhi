package config

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
