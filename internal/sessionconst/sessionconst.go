// Package sessionconst exposes session-tuning constants that low-level
// packages (notably internal/config) need without importing internal/session.
// internal/session re-exports these names; new callers should prefer
// sessionconst directly.
package sessionconst

import "time"

// DefaultMaxProcs is the concurrent-process cap applied when
// RouterConfig.MaxProcs (session.max_procs) is not set.
const DefaultMaxProcs = 3

// DefaultTTL is the idle-session eviction threshold applied when
// RouterConfig.TTL is not set.
const DefaultTTL = 30 * time.Minute

// DefaultPruneTTL is the "keep metadata for long-idle session" threshold
// applied when RouterConfig.PruneTTL is not set. Entries older than this are
// pruned from the store even when exempt.
const DefaultPruneTTL = 72 * time.Hour
