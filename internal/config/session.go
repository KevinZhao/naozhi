package config

import (
	"fmt"
	"time"

	"github.com/naozhi/naozhi/internal/sessionconst"
)

type SessionConfig struct {
	MaxProcs         int                        `yaml:"max_procs"`
	TTL              string                     `yaml:"ttl"`
	PruneTTL         string                     `yaml:"prune_ttl"` // how long dead/suspended sessions stay in the list before removal
	Watchdog         WatchdogConfig             `yaml:"watchdog"`
	Queue            QueueConfig                `yaml:"queue"`
	StorePath        string                     `yaml:"store_path"`
	CWD              string                     `yaml:"cwd"` // default working directory for CLI processes
	Shim             ShimConfig                 `yaml:"shim"`
	ProjectStableKey ProjectStableKeyYAMLConfig `yaml:"project_stable_key,omitempty"`
	// GroupScope is what one session of an IM group chat covers: a "thread"
	// (default; a message outside any thread uses the chat's), the whole
	// "chat", or one "user".
	GroupScope string `yaml:"group_scope,omitempty"`
}

// Group-chat session scopes (SessionConfig.GroupScope).
const (
	GroupScopeThread = "thread"
	GroupScopeChat   = "chat"
	GroupScopeUser   = "user"
)

// validateGroupScope rejects a session.group_scope outside the three scopes
// (empty is the default, thread).
func validateGroupScope(cfg *Config) error {
	switch cfg.Session.GroupScope {
	case "", GroupScopeThread, GroupScopeChat, GroupScopeUser:
		return nil
	}
	return fmt.Errorf("session.group_scope must be %q, %q or %q, got %q",
		GroupScopeThread, GroupScopeChat, GroupScopeUser, cfg.Session.GroupScope)
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

// ShimIdleTimeout is how long a shim lives without a client (default 4h).
func (c *Config) ShimIdleTimeout() time.Duration {
	return orDefault(c.cachedShimIdleTimeout, defaultShimIdleTimeout)
}

// ShimWatchdogTimeout is the shim's disconnect watchdog (default 30m).
func (c *Config) ShimWatchdogTimeout() time.Duration {
	return orDefault(c.cachedShimWatchdogTimeout, defaultShimWatchdogTimeout)
}

// ShimMaxBufferBytes is the shim ring buffer's byte cap (default 50MB).
func (c *Config) ShimMaxBufferBytes() int64 {
	return orDefault(c.cachedShimMaxBufferBytes, defaultShimMaxBufferBytes)
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
// turn.ParseMode (a string here avoids a config → turn cycle).
func (c *Config) QueueMode() string {
	return c.Session.Queue.Mode
}

// applySessionDefaults fills the session limits, queue knobs and cwd. Split
// out of applyDefaults (#2710 J11).
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
	if cfg.Session.CWD == "" {
		cfg.Session.CWD = defaultSessionCWD
	}
	if cfg.Session.GroupScope == "" {
		cfg.Session.GroupScope = GroupScopeThread
	}
}
