// Package persist implements naozhi's per-session event log persistence
// layer (see docs/rfc/event-log-persistence.md).
//
// Responsibilities:
//
//   - Write cli.EventEntry batches to durable per-session <keyhash>.log
//     files with strict log→idx→fsync ordering.
//   - Maintain a sparse <keyhash>.idx sidecar that drives O(1) rotate
//     tail-cut and startup recovery.
//   - Rotate oversized log files by keeping only the newest N records.
//   - Provide a non-blocking PersistSink that cli.EventLog hooks so
//     Append/AppendBatch never stall on disk I/O.
//
// Non-responsibilities (deliberately out of this package):
//
//   - Reading event history back: that lives in
//     internal/history/naozhilog, which builds a history.Source on top
//     of the same files persist writes.
//   - cli.EventEntry semantics: this package treats entries as opaque
//     JSON bytes wrapped in the schema.Record envelope. If EventEntry
//     evolves, schema.EntryView catches it in CI long before persist
//     sees the drift.
//   - Merging local + Claude JSONL sources: that is MergedSource's job.
//
// Concurrency model:
//
//   - All file writes happen on a single writer goroutine owned by
//     Persister. Producers (cli.EventLog callers) only enqueue
//     PersistJobs via the Sink closure; they never touch files.
//   - Readers (naozhilog.Source) open their own read-only file
//     descriptors and are NOT synchronized with the writer. Partial
//     tail records (writer crashed mid-fsync) are tolerated by the
//     framing decoder.
//
// Data durability model:
//
//   - Every batch is written as: log.Write N × → log.Sync → idx.Write
//     N × → idx.Sync. This makes the idx sidecar strictly lag the
//     log file by at most one fsync window, so idx entries always
//     point at bytes already persisted in the log.
//   - Crash recovery leans on this invariant: startup truncates the
//     log to the idx's last safe edge, discarding any trailing bytes
//     that reached the log but whose idx entry never fsynced.
//
// See recovery.go for the exact truncation algorithm and persister.go
// for the debounce + drop-on-full policy.
package persist
