package persist

// Entry is the producer-side unit the Persister consumes: the serialised
// JSON of a clievent.EventEntry plus TimeMS (idx sparse sampling /
// recovery). persist does not import cli, so the adapter in
// internal/session/eventlog_bridge.go performs the conversion.
//
// Entry.JSON is BORROWED (#1524): the producer may reuse its backing array
// as soon as the PersistSink call returns. Persister.SinkFor's sink copies
// every Entry.JSON into a pooled per-batch arena before queuing; a custom
// sink that retains entries past its own return MUST copy them itself.
type Entry struct {
	// JSON is the EventEntry JSON; the Persister wraps it in a schema.Record.
	JSON []byte
	// TimeMS mirrors EventEntry.Time; stored on the IdxEntry so readers can
	// binary-search idx by timestamp without decoding log bodies.
	TimeMS int64
}

// PersistSink is the callback cli.EventLog invokes after Append /
// AppendBatch (RFC §3.2.1). Contract:
//   - MUST be non-blocking: on a full channel it drops the batch and
//     increments droppedCnt ("never stall Append").
//   - MUST tolerate nil / empty entries.
//   - replayPhase=true marks a replay from historical storage
//     (InjectHistory, shim reconnect); the sink discards it to avoid the
//     RFC §3.3 self-amplification loop and reports the violation via
//     slog.Error + Observer.OnReplayLeak + replayLeakCnt.
type PersistSink func(entries []Entry, replayPhase bool)
