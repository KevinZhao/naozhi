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
// cli.EventEntry into Entry and calls PersistSink; see
// internal/session/eventlog_bridge.go when Phase 2 lands.
//
// Lifecycle of Entry.JSON:
//   - Owned by the producer before the PersistSink call.
//   - The Persister goroutine reads the bytes once (to write them to
//     disk) and never mutates them.
//   - After PersistSink returns, the producer may reuse the backing
//     array — but the Persister will have already queued its own
//     reference. Adapters should therefore allocate a fresh []byte
//     per Entry (the session adapter does this: one json.Marshal per
//     EventEntry produces a fresh buffer each time).
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
//     RFC §3.3. In DevMode this path panics so tests catch any new
//     caller site that forgot the SetPersistSink ordering contract.
type PersistSink func(entries []Entry, replayPhase bool)
