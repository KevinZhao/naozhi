package config

import (
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/sessionconst"
)

type SessionConfig struct {
	MaxProcs  int            `yaml:"max_procs"`
	TTL       string         `yaml:"ttl"`
	PruneTTL  string         `yaml:"prune_ttl"` // how long dead/suspended sessions stay in the list before removal
	Watchdog  WatchdogConfig `yaml:"watchdog"`
	Queue     QueueConfig    `yaml:"queue"`
	StorePath string         `yaml:"store_path"`
	CWD       string         `yaml:"cwd"` // default working directory for CLI processes
	// Deprecated: use CWD instead. Still parsed for existing config files.
	Workspace string     `yaml:"workspace"`
	Shim      ShimConfig `yaml:"shim"`
	// Deprecated: auto_chain has no effect (see AutoChainYAMLConfig); still
	// parsed so existing files load, with a one-line warning if set.
	AutoChain        AutoChainYAMLConfig        `yaml:"auto_chain,omitempty"`
	ProjectStableKey ProjectStableKeyYAMLConfig `yaml:"project_stable_key,omitempty"`
}

// ProjectStableKeyYAMLConfig controls the project-level stable session key
// (docs/rfc/project-stable-session-key.md). Default-on; when disabled the
// dashboard falls back to the timestamp-key path for "continue". Enabled is
// *bool so an absent key defaults true while `enabled: false` stays expressible.
type ProjectStableKeyYAMLConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// ResolvedEnabled returns the effective on/off flag.
func (c ProjectStableKeyYAMLConfig) ResolvedEnabled(def bool) bool {
	if c.Enabled == nil {
		return def
	}
	return *c.Enabled
}

// AutoChainYAMLConfig is the DEPRECATED, no-effect auto-workspace-chain block
// (docs/rfc/project-stable-session-key.md §9). Fields are parsed so old files
// load; nothing consumes them. Do NOT wire them back: "same slug + time window"
// chained unrelated sessions as each other's history. Enabled is *bool so the
// deprecation warn can tell an absent key from an explicit `enabled: false`.
type AutoChainYAMLConfig struct {
	Enabled     *bool `yaml:"enabled,omitempty"`
	WindowHours int   `yaml:"window_hours,omitempty"` // 0 → 168 (7d)
	Cap         int   `yaml:"cap,omitempty"`          // 0 → 32
}

// ResolvedEnabled returns the effective on/off flag.
func (c AutoChainYAMLConfig) ResolvedEnabled(def bool) bool {
	if c.Enabled == nil {
		return def
	}
	return *c.Enabled
}

// ResolvedWindowHours returns the effective window in hours.
func (c AutoChainYAMLConfig) ResolvedWindowHours(def int) int {
	if c.WindowHours <= 0 {
		return def
	}
	return c.WindowHours
}

// ResolvedCap returns the effective chain length cap.
func (c AutoChainYAMLConfig) ResolvedCap(def int) int {
	if c.Cap <= 0 {
		return def
	}
	return c.Cap
}

// QueueConfig controls IM message queuing when a session is busy.
type QueueConfig struct {
	// MaxDepth is the max queued messages per session: nil = default (20),
	// 0 = disable queuing (drop + "please wait"), negative = 0.
	MaxDepth *int `yaml:"max_depth"`
	// CollectDelay is the wait after a turn completes before draining the
	// queue, so fast follow-ups batch together. Default "500ms".
	CollectDelay string `yaml:"collect_delay"`
	// Mode handles messages arriving mid-turn: "collect" (default) waits for
	// the turn; "interrupt" aborts it via control_request and sends the
	// coalesced follow-ups next. Only stream-json honours "interrupt"; ACP
	// falls back to "collect".
	Mode string `yaml:"mode"`
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

// QueueMaxDepth returns the resolved queue max depth; negative values clamp
// to 0 so Enqueue's `len(msgs) >= maxDepth` guard stays sane.
func (c *Config) QueueMaxDepth() int {
	if c.Session.Queue.MaxDepth == nil {
		return 20
	}
	if d := *c.Session.Queue.MaxDepth; d > 0 {
		return d
	}
	return 0
}

// QueueMode returns the raw queue mode string; callers normalise via
// dispatch.ParseQueueMode (a string here avoids a config → dispatch cycle).
func (c *Config) QueueMode() string {
	return c.Session.Queue.Mode
}

// applySessionDefaults fills the session limits and queue knobs, reconciles
// session.cwd with the deprecated session.workspace alias, and reports a
// configured-but-dead auto_chain block. The cwd reconciliation runs on the
// operator's RAW input — pre-filling the default first would make a
// pure-default deployment trip the deprecation report. Split out of
// applyDefaults (#2710 J11).
func applySessionDefaults(cfg *Config) {
	if cfg.Session.MaxProcs <= 0 {
		cfg.Session.MaxProcs = sessionconst.DefaultMaxProcs
	}
	if cfg.Session.TTL == "" {
		cfg.Session.TTL = defaultSessionTTL.String()
	}
	if cfg.Session.PruneTTL == "" {
		cfg.Session.PruneTTL = defaultSessionPruneTTL.String()
	}
	if cfg.Session.Queue.MaxDepth == nil {
		defaultDepth := defaultQueueMaxDepth
		cfg.Session.Queue.MaxDepth = &defaultDepth
	}
	if cfg.Session.Queue.CollectDelay == "" {
		cfg.Session.Queue.CollectDelay = defaultQueueCollectDelay.String()
	}
	if cfg.Session.Queue.Mode == "" {
		cfg.Session.Queue.Mode = defaultQueueMode
	}
	// Reconcile cwd / deprecated workspace on the operator's raw input — the
	// default must NOT be pre-filled first or a pure-default deployment would
	// trip the deprecation warning.
	if cfg.Session.CWD != "" {
		if cfg.Session.Workspace != "" && cfg.Session.Workspace != cfg.Session.CWD {
			cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
				Layer: "config-deprecated", Key: "session.workspace", Action: "ignored",
				Reason: "both 'session.cwd' and deprecated 'session.workspace' configured; using 'cwd'",
			}})
		}
		cfg.Session.Workspace = cfg.Session.CWD
	} else if cfg.Session.Workspace != "" {
		cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
			Layer: "config-deprecated", Key: "session.workspace", Action: "rewritten",
			Reason: "'session.workspace' is deprecated, please rename to 'session.cwd'",
		}})
		cfg.Session.CWD = cfg.Session.Workspace
	} else {
		// Mirror the default into the alias so readers of either field work.
		cfg.Session.CWD = defaultSessionCWD
		cfg.Session.Workspace = defaultSessionCWD
	}

	if cfg.Session.AutoChain.Enabled != nil || cfg.Session.AutoChain.WindowHours != 0 || cfg.Session.AutoChain.Cap != 0 {
		cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
			Layer: "config-deprecated", Key: "session.auto_chain", Action: "ignored",
			Reason: "'session.auto_chain' is deprecated and has no effect; remove this block from config",
		}})
	}
}
