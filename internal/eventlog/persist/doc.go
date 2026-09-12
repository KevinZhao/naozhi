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
//   - Provide a non-blocking PersistSink so ring.EventLog Append/AppendBatch
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
//   - ring.EventLog (internal/cli/eventlog.go) — in-memory ring buffer and
//     producer of every event.
//   - internal/eventlog/persist (this package) — on-disk writer fed by
//     ring.EventLog through the PersistSink closure.
//   - internal/eventlog/schema — wire format shared by persist and replay
//     readers; strictly upstream of cli.
//   - internal/history/naozhilog — replay reader for the files persist wrote.
//
// persist.PersistSink (entry.go) takes persist.Entry (post-marshal);
// ring.PersistSink takes []clievent.EventEntry (pre-marshal). Only
// internal/session/eventlog_bridge.go translates between them.
//
// Files in this package (persister.go was 1,509 lines until J9 of #2548):
//
//   - persister.go — Options, the Persister struct, construction, and the
//     public surface (FS/Pressure/Accept/SinkFor/DropKey/Flush/Stop/Stats).
//   - sink.go      — the ingest path: sessionSink.accept hands a batch to the
//     run goroutine, handleBatch turns it into records on disk.
//   - loop.go      — the run goroutine: its select loop, the op protocol it
//     serves, and shutdown. Everything here is single-goroutine, which is what
//     lets flushScratch be reused without synchronisation.
//   - flush.go     — when writers are flushed and closed, plus flushScratch.
//   - writer.go    — the per-key writer: open, flush, close, remove its files.
//   - pools.go     — the buffer/arena pools the write path reuses.
//   - rotate.go / recovery.go / idx.go / framing.go / keyhash.go / entry.go /
//     fstype*.go — already separate before J9.
//
// Persister implements none of the internal/eventlog/api interfaces
// (EventStore = Appender + Reader + Subscriber): it is driven by the per-key
// PersistSink (SinkFor) and read back via Recover. The adapter is deferred
// to #1570.
package persist
