// File eventlog_persist.go: the PersistSink contract — SetPersistSink(Pair), invoke fan-out,
// and the replay-phase guard atomics.
// Split from eventlog.go per docs/rfc/eventlog-split.md (ARCH-EVENTLOG-SPLIT);
// the EventLog struct and constructor live in eventlog.go.

package cli

// PersistSinkOne is the single-entry counterpart to PersistSink. When
// installed alongside (or in lieu of) the slice-shaped PersistSink via
// SetPersistSinkPair, it is preferred by Append's hot path so the
// per-call `[]EventEntry{e}` literal allocation disappears (#410).
//
// Semantics match PersistSink exactly: the entry is the same value that
// would have landed at index 0 of the slice variant; replayPhase is
// derived from sinkReady identically. AppendBatch always uses the slice
// path — collapsing N>1 entries into N single-entry calls would lose the
// per-batch atomic write-order the persister relies on (see persister.go
// SinkFor batching contract).
//
// Implementations MUST be non-blocking, identical to PersistSink. Callers
// that retain the EventEntry past return must copy any reference fields
// they care about (Images / ImagePaths / AskQuestion / ToolCall) — the
// EventEntry struct itself is passed by value, but its slice/pointer
// fields share backing memory with the ring buffer slot.
type PersistSinkOne func(entry EventEntry, replayPhase bool)

// PersistSink is the event log's persistence hook contract.
// cli.EventLog calls the stored sink (when set) after every Append
// and AppendBatch, passing:
//
//   - entries: a defensive copy of the appended EventEntry values
//     (sink implementations may retain the slice — EventLog will
//     not modify it after the call returns).
//   - replayPhase: true while sinkReady is false (i.e. this Append
//     came before SetPersistSink was called). The Persister drops
//     and counter-metrics these batches so a broken call path
//     (InjectHistory after SetPersistSink) cannot create a
//     log-replay-amplification loop.
//
// Implementations MUST be non-blocking. Persister.SinkFor satisfies
// this contract by using a non-blocking channel send + drop policy;
// a custom sink may choose a different policy (synchronous disk
// commit in a test, metrics-only accounting) but must NEVER hold
// up the Append caller — EventLog takes pains to release l.mu
// before invoking the sink specifically so slow sinks can't stall
// the ring buffer.
//
// # Relationship with persist.PersistSink (R222-ARCH-4 anchor)
//
// internal/eventlog/persist also defines a `PersistSink` symbol —
// the on-disk Persister's accept-from-bridge hook. The two are
// deliberately distinct types today:
//
//   - cli.PersistSink (this type) takes []EventEntry, the cli-domain
//     wire shape. Lives next to EventLog because EventLog is the
//     producer.
//   - persist.PersistSink takes persist/schema entries, the on-disk
//     wire shape (uuid, replay flag, framing fields).
//
// session/eventlog_bridge.go is the only place that translates
// between them — capturing the cli-side slice, building the
// persist-side schema records, and forwarding to the Persister.
// R222-ARCH-4 / R227-ARCH-15 propose collapsing the two types onto
// a single internal/eventlog/schema struct so the bridge's marshal
// step disappears, but that requires moving EventEntry out of
// internal/cli (today multiple consumers in cli rely on the
// in-package type for sub-agent linkage). Until that lands, treat
// the two PersistSink names as a documented refactor seam, not a
// drift.
type PersistSink func(entries []EventEntry, replayPhase bool)

// SetPersistSink installs the on-disk persistence hook. See the
// PersistSink contract + the sinkReady field godoc for the full
// ordering rules.
//
// This method is the only public way to flip sinkReady to true.
// Calling it twice replaces the sink (last-writer-wins); calling
// it with nil "clears" the sink AND flips sinkReady back to false
// so that any subsequent SetPersistSink(real) re-enters the
// pre-attach phase cleanly. R20260526-GO-010: without this reset,
// a "pause persist → re-install sink → InjectHistory" sequence
// would tag the replay batch replayPhase=false (live) and the
// Persister would commit the duplicate history to disk.
//
// R224-GO-5 (closes TODO): the original review flagged a "race
// window between sink Store and sinkReady Store where one entry
// can be wrongly tagged replay=true". The current ordering
// (sink-first, sinkReady-second) is intentional and the asymmetry
// is on the safe side:
//
//   - Inverted order (sinkReady=true first) opens a window where
//     invokePersistSink loads a nil sink AFTER Append observed
//     sinkReady=true → the event is dropped on the floor with no
//     telemetry path to recover it.
//   - Current order (sink first, then sinkReady) opens a window
//     where Append observes sinkReady=false but the sink IS set;
//     the event reaches the Persister tagged replayPhase=true,
//     which is the SAME tag history-replay paths use. Persister
//     drops + counters those entries instead of committing them
//     to disk. The window is bounded by two atomic Stores
//     (sub-ns) and only fires for live events landing in that
//     gap on a freshly-attached sink — by definition the sink
//     just attached and there is no data to lose; the next event
//     after the second Store lands cleanly as live.
//
// Reversing the stores would be strictly worse: silent event loss
// vs the current "occasional belt-and-suspenders replay drop".
// Atomic.Pointer cannot carry both fields without a pointer
// allocation per Store, which would dominate the cost of a path
// that runs once per session lifetime. Keep the asymmetry.
func (l *EventLog) SetPersistSink(fn PersistSink) {
	if fn == nil {
		// Order: clear sinkReady FIRST so any concurrent Append racing
		// the uninstall observes the pre-attach phase before the sink
		// pointer goes nil. Storing the pointer first would open a
		// window where invokePersistSink loads a non-nil pointer but
		// reads sinkReady=true, then by the time it dispatches the
		// pointer is nil — same shape as the inverted-order race
		// documented in the install path. R20260526-GO-010.
		l.sinkReady.Store(false)
		l.persistSinkPtr.Store(nil)
		// Clear any previously paired single-entry sink — leaving it
		// installed would cause Append to fire the single-entry
		// closure while AppendBatch silently no-ops (slice ptr nil),
		// breaking the "consistent dispatch" invariant the two paths
		// share. SetPersistSinkPair is the only entrypoint that
		// installs a single sink; SetPersistSink-with-nil clears both
		// for symmetry.
		l.persistSinkOnePtr.Store(nil)
		return
	}
	// Store the sink pointer FIRST so any concurrent Append that
	// reads sinkReady=true will also see a valid sink. Without this
	// ordering there's a window where Append sees sinkReady=true
	// but Load returns nil, losing the event. See R224-GO-5 anchor
	// in the godoc above for the ordering proof.
	p := fn
	l.persistSinkPtr.Store(&p)
	// Installing a slice-only sink retracts any previously paired
	// single-entry sink: callers who switch back from the pair API to
	// the legacy slice API must not silently keep the old single sink
	// firing — the two slices may correspond to entirely different
	// downstream destinations.
	l.persistSinkOnePtr.Store(nil)
	l.sinkReady.Store(true)
}

// SetPersistSinkPair installs both the slice-shaped batch sink and a
// single-entry fast-path sink in one call. The two sinks MUST drain to
// the same downstream destination — Append uses `single`, AppendBatch
// uses `batch`, and the per-call decision is invisible to operators.
// When `single` is nil, behaviour collapses back to SetPersistSink(batch).
//
// Ordering matches SetPersistSink's documented R224-GO-5 contract: the
// sink pointers are stored before sinkReady flips to true so a concurrent
// Append observing sinkReady=true is guaranteed to see a non-nil sink
// for at least one dispatch path. The single-entry pointer is stored
// before the slice pointer so Append's "prefer single" dispatch never
// regresses to a slice-literal alloc once the pair has been installed.
//
// #410: the single-entry path lets Append skip the `[]EventEntry{e}`
// literal that would otherwise escape through the slice sink's retention
// contract, removing one heap alloc per live event on the hot path.
func (l *EventLog) SetPersistSinkPair(batch PersistSink, single PersistSinkOne) {
	if batch == nil {
		// Treat a nil batch as "uninstall everything" so callers do not
		// have to remember a separate clear sequence; mirrors
		// SetPersistSink(nil) semantics — including the sinkReady
		// reset that lets a subsequent re-install enter the pre-attach
		// phase cleanly. R20260526-GO-010.
		l.sinkReady.Store(false)
		l.persistSinkOnePtr.Store(nil)
		l.persistSinkPtr.Store(nil)
		return
	}
	bp := batch
	if single != nil {
		sp := single
		l.persistSinkOnePtr.Store(&sp)
	} else {
		l.persistSinkOnePtr.Store(nil)
	}
	l.persistSinkPtr.Store(&bp)
	l.sinkReady.Store(true)
}

// invokePersistSink is the Append / AppendBatch helper that fires
// the sink (when set) after the ring-buffer mutations are committed
// and l.mu has been released.
//
// replayPhase is derived from sinkReady at the time of the call —
// entries appended before SetPersistSink ran are replay-tagged,
// entries after are live.
//
// `entries` must be a slice that is safe for the sink to retain —
// callers pass a freshly-copied slice (not a view into the ring
// buffer) because the ring can wrap and overwrite slots shortly
// after.
func (l *EventLog) invokePersistSink(entries []EventEntry) {
	p := l.persistSinkPtr.Load()
	if p == nil {
		return
	}
	// When sinkReady is false the batch must be tagged replayPhase=true
	// — this is the runtime blocker-1 guard from RFC §3.2.3.
	replay := !l.sinkReady.Load()
	if replay {
		// R242-ARCH-20: count replay-phase invocations so /health (or
		// equivalent diagnostic endpoint) can surface a non-zero value
		// as a contract-violation signal. Steady-state production should
		// see this counter freeze at the InjectHistory replay total and
		// never grow once SetPersistSink has run.
		l.replayInvokeTotal.Add(1)
	}
	(*p)(entries, replay)
}

// invokePersistSinkOne is the single-entry counterpart to invokePersistSink,
// fired only by Append (not AppendBatch). Returns true when the single sink
// was attached and dispatched; false when the caller must fall back to the
// slice-shaped invokePersistSink path. Sharing the same replayPhase
// derivation + replayInvokeTotal counter as invokePersistSink keeps the
// telemetry surface unified: a sink-pair caller and a slice-only caller
// observe identical counter behaviour. (#410)
func (l *EventLog) invokePersistSinkOne(entry EventEntry) bool {
	p := l.persistSinkOnePtr.Load()
	if p == nil {
		return false
	}
	replay := !l.sinkReady.Load()
	if replay {
		l.replayInvokeTotal.Add(1)
	}
	(*p)(entry, replay)
	return true
}

// ReplayInvokeTotal returns the number of invokePersistSink calls that
// observed sinkReady=false (replayPhase=true). This is a diagnostic
// counter only: production code does not gate behaviour on it. Tests
// use it to assert that the SetPersistSink-after-InjectHistory ordering
// held; dashboards / /health endpoints can expose it to detect a
// pre-attach burst that would otherwise be silently absorbed by the
// Persister's replay-drop logic.
//
// R242-ARCH-20 (closed): the review asked for a `replayDropTotal
// atomic.Int64` exposed on /health to detect the SetPersistSink double-
// store ordering window misfiring in production. The counter pair is
// already in place across the cli ↔ persist boundary:
//
//   - cli side: ReplayInvokeTotal() above counts invokePersistSink calls
//     that fired with replayPhase=true (the cli's local view of "this
//     entry was tagged replay because sinkReady was false").
//   - persist side: persist.Stats().ReplayLeak (persister.replayLeakCnt)
//     counts entries the Persister received with replayPhase=true and
//     dropped on the floor, plus persist.Observer.OnReplayLeak fires per
//     batch for push-based monitoring.
//
// Operators wiring /health surface both values: cli's count > 0 with
// persist's count == 0 means the sink had not yet attached when the
// race fired (the harmless case the SetPersistSink godoc above
// documents); both > 0 means the InjectHistory replay batches were
// genuinely absorbed by the persister's replay-drop guard. The
// counters together fully cover the contract-violation surface the
// original review wanted observable; no additional `replayDropTotal`
// is needed because the boundary is two-sided and each side keeps its
// own honest count.
//
// Safe to call from any goroutine; returns the cumulative count from
// the EventLog's construction.
func (l *EventLog) ReplayInvokeTotal() int64 {
	return l.replayInvokeTotal.Load()
}

// SinkReady reports whether SetPersistSink has wired a persistence hook
// and toggled `sinkReady` to true. Designed for /health surfacing — pair
// with ReplayInvokeTotal() so operators can distinguish "the sink simply
// hasn't attached yet" (SinkReady=false, ReplayInvokeTotal frozen at the
// InjectHistory replay total) from "the SetPersistSink-after-Append
// ordering window opened in production" (SinkReady=true,
// ReplayInvokeTotal still climbing — should be statistically impossible
// under correct caller ordering).
//
// R242-ARCH-20 (closes the diagnostic surface the original review asked
// for). The counter pair already covers the leak side; this accessor
// closes the "is the sink up?" half so /health doesn't have to peek at
// internal atomics.
//
// Safe to call from any goroutine. Returns false on a nil receiver so
// /health request paths that observe a torn-down EventLog (rare, but
// possible during shutdown) report "not ready" rather than panic.
func (l *EventLog) SinkReady() bool {
	if l == nil {
		return false
	}
	return l.sinkReady.Load()
}
