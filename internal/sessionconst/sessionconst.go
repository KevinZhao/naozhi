// Package sessionconst exposes session-tuning constants that other low-level
// packages (notably internal/config) need at default-application time. The
// constants live here, not in internal/session, so internal/config does not
// have to reverse-import internal/session for a single literal.
//
// Anchor: R222-ARCH-3. internal/session re-exports these names so the broader
// codebase can keep referring to session.DefaultMaxProcs and friends; new
// callers should prefer sessionconst directly to keep the dependency direction
// pointing the right way (low-level → low-level, never config → session).
package sessionconst

import "time"

// DefaultMaxProcs is the concurrent-process cap applied when
// RouterConfig.MaxProcs is not set. internal/config.applyDefaults reads this
// value when the user leaves session.max_procs unset in config.yaml.
const DefaultMaxProcs = 3

// DefaultTTL is the idle-session eviction threshold applied when
// RouterConfig.TTL is not set.
const DefaultTTL = 30 * time.Minute

// DefaultPruneTTL is the "keep metadata for long-idle session" threshold
// applied when RouterConfig.PruneTTL is not set. Entries older than this are
// pruned from the store even when exempt.
const DefaultPruneTTL = 72 * time.Hour
