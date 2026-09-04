// Package persist implements naozhi's per-session event log persistence
// layer (see docs/rfc/event-log-persistence.md).
//
// Responsibilities:
//
//   - Write clievent.EventEntry batches to durable per-session <keyhash>.log
//     files with strict log→idx→fsync ordering.
//   - Maintain a sparse <keyhash>.idx sidecar that drives O(1) rotate
//     tail-cut and startup recovery.
//   - Rotate oversized log files by keeping only the newest N records.
//   - Provide a non-blocking PersistSink so cli.EventLog Append/AppendBatch
//     never stall on disk I/O.
//
// Out of scope: reading history back (internal/history/naozhilog),
// clievent.EventEntry semantics (entries are opaque JSON inside the
// schema.Record envelope), and merging with Claude JSONL (MergedSource).
//
// Concurrency: all file writes happen on the single writer goroutine owned
// by Persister; producers only enqueue via the Sink closure. Readers open
// their own read-only descriptors, unsynchronised with the writer; the
// framing decoder tolerates a partial tail record.
//
// Durability: every batch is written as log.Write × N → log.Sync →
// idx.Write × N → idx.Sync, so idx entries always point at bytes already
// persisted in the log. Startup recovery truncates the log to the idx's
// last safe edge (see recovery.go); persister.go holds the debounce and
// drop-on-full policy.
//
// # Three "eventlog" packages
//
//   - cli.EventLog (internal/cli/eventlog.go) — in-memory ring buffer and
//     producer of every event.
//   - internal/eventlog/persist (this package) — on-disk writer fed by
//     cli.EventLog through the PersistSink closure.
//   - internal/eventlog/schema — wire format shared by persist and replay
//     readers; strictly upstream of cli.
//   - internal/history/naozhilog — replay reader for the files persist wrote.
//
// persist.PersistSink (entry.go) takes persist.Entry (post-marshal);
// cli.PersistSink takes []clievent.EventEntry (pre-marshal). Only
// internal/session/eventlog_bridge.go translates between them.
//
// Persister implements none of the internal/eventlog/api interfaces
// (EventStore = Appender + Reader + Subscriber): it is driven by the per-key
// PersistSink (SinkFor) and read back via Recover. The adapter is deferred
// to #1570.
package persist
