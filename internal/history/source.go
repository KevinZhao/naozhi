// Package history defines a backend-agnostic interface for loading historical
// EventEntry pages from persistent storage outside of the in-memory ring.
//
// The dashboard "load earlier" pagination walks the in-memory history first
// (the 500-entry ring maintained by cli.EventLog / ManagedSession's
// persistedHistory). When that ring is exhausted, the session falls back
// through a Source to reach storage that long outlives the process —
// for claude-code this is the ~/.claude/projects/**/{session-id}.jsonl
// files; for other CLIs (kiro, gemini-acp, future backends) it may be a
// different format or no durable source at all.
//
// Keeping this behind a small interface lets the session layer stay
// backend-agnostic: it just asks "give me up to N entries strictly older
// than T" and doesn't care whether the answer came from JSONL, a remote
// API, or is empty.
package history

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
)

// Source exposes a read-only view of a session's historical events backed
// by persistent storage. Implementations must be safe for concurrent use —
// the dashboard can fire pagination requests while the session is actively
// appending new events on the write path.
//
// LoadBefore returns up to `limit` entries whose Time is strictly less than
// `beforeMS`, in chronological order (oldest → newest). The contract mirrors
// cli.EventLog.EntriesBefore so memory-tier and disk-tier results can be
// concatenated without an ordering adapter.
//
// Semantics:
//   - beforeMS <= 0 is treated as "no upper bound" (newest-`limit` tail).
//   - limit <= 0 returns nil.
//   - An empty result ([]entry, nil) means "no more history available" and
//     must be distinguished from a transient error by the (nil, error)
//     contract — errors are informational; the caller should log and treat
//     them as end-of-history to avoid infinite retry loops.
//   - ctx cancellation propagates into file I/O; implementations should
//     return promptly on Done.
type Source interface {
	LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error)
}

// Noop is a Source that always returns nil. Backends without a durable
// history store (kiro today, any future CLI whose transcript isn't yet
// introspectable) use this as a placeholder so the session layer can
// treat Source as never-nil and skip defensive null checks at call sites.
type Noop struct{}

// LoadBefore always returns (nil, nil) — no history, no error. Callers
// interpret this as "end of history reached" on the very first call.
func (Noop) LoadBefore(context.Context, int64, int) ([]cli.EventEntry, error) {
	return nil, nil
}
