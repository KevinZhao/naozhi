package persist

// Entry is the producer-side unit the Persister consumes. It carries
// the already-serialised JSON bytes of a cli.EventEntry plus the
// two fields persist needs to address it (TimeMS for idx sparse
// sampling / startup recovery, and the implicit ordering given by
// the surrounding slice).
//
// Why not pass cli.EventEntry directly: persist must not import cli
// (cli imports schema; cli and persist are peers, both downstream of
// schema). The session / router layer owns the adapter that marshals
// cli.EventEntry into Entry and calls PersistSink; the adapter lives
// in internal/session/eventlog_bridge.go.
//
// Lifecycle of Entry.JSON (R20260531A-PERF-3, #1524):
//   - The producer owns the bytes only up to the PersistSink call. The
//     bytes are BORROWED: the producer MAY reuse Entry.JSON's backing
//     array the moment the sink returns, and need NOT allocate a fresh
//     []byte per Entry.
//   - Persister.SinkFor's sink (sessionSink.accept) takes ownership by
//     copying every Entry.JSON into a pooled per-batch arena before it
//     queues the batch on the async channel. The Persister goroutine
//     then reads the arena-owned bytes once to write them to disk and
//     never mutates them; the arena is returned to the pool after the
//     batch is written.
//   - This contract lets the session bridge pass its pooled-encoder
//     output directly instead of doing a redundant make([]byte)+copy
//     per event — accept's single pooled copy replaces it. A custom
//     sink that retains entries past its own return MUST copy the bytes
//     itself (SinkFor already does).
type Entry struct {
	// JSON is the full schema-compliant EventEntry JSON. The
	// Persister wraps it in a schema.Record before writing.
	JSON []byte
	// TimeMS mirrors EventEntry.Time; persist stores it on the
	// IdxEntry so readers can binary-search idx by timestamp without
	// decoding log bodies.
	TimeMS int64
}

// PersistSink is the callback cli.EventLog invokes after Append /
// AppendBatch. See RFC §3.2.1 for the ordering contract around
// replayPhase and the rationale for a batch signature.
//
// Implementation notes:
//   - The sink MUST be non-blocking. On full channel it drops the
//     batch and increments droppedCnt. This is the core "never stall
//     Append" guarantee.
//   - The sink MUST tolerate nil / zero-length entries — it is
//     convenient for adapter tests to pass empty batches through
//     without a no-op branch.
//   - replayPhase=true indicates the batch is a replay from historical
//     storage (InjectHistory, shim reconnect). The sink discards such
//     batches to avoid the self-amplification loop described in
//     RFC §3.3 and signals the contract violation via slog.Error +
//     Observer.OnReplayLeak + replayLeakCnt. In DevMode the log is
//     tagged `dev_mode=true` for grep visibility (R242-GO-11).
type PersistSink func(entries []Entry, replayPhase bool)
