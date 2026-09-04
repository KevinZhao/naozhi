// Package history defines a backend-agnostic interface for loading older
// EventEntry pages from storage that outlives the in-memory ring (claude-code:
// ~/.claude/projects/**/{session-id}.jsonl; other CLIs: their own format or
// nothing). The session layer only asks "up to N entries strictly older than T".
package history

import "github.com/naozhi/naozhi/internal/cli"

// Source exposes a read-only view of a session's past events. It is an
// alias for cli.HistorySource so the two definitions cannot drift; cli owns
// the canonical interface and does not import this package (#761).
//
// Implementations must be safe for concurrent use. LoadBefore returns up to
// `limit` entries with Time strictly less than `beforeMS`, oldest → newest,
// mirroring cli.EventLog.EntriesBefore. beforeMS <= 0 means no upper bound;
// limit <= 0 returns nil. Errors are informational: callers log and treat
// them as end-of-history. ctx cancellation must propagate into file I/O.
type Source = cli.HistorySource

// Noop is a Source that always returns nil; backends without a durable history
// store use it so the session layer can treat Source as never-nil (#761).
type Noop = cli.NoopHistorySource
