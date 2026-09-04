// File eventlog_persist.go: the PersistSink contract — SetPersistSink(Pair),
// invoke fan-out, and the replay-phase guard atomics.

package cli

import "github.com/naozhi/naozhi/internal/cli/clievent"

// PersistSinkOne is the single-entry counterpart to PersistSink, installed via
// SetPersistSinkPair and preferred by Append so the per-call `[]EventEntry{e}`
// literal disappears (#410). Semantics match PersistSink: same value that
// would be at index 0, same replayPhase derivation. AppendBatch always uses
// the slice path so the persister keeps per-batch write order.
//
// Implementations MUST be non-blocking. The entry is passed by value but its
// slice/pointer fields (Images / ImagePaths / AskQuestion / ToolCall) share
// backing memory with the ring slot — copy them if retained past return.
type PersistSinkOne func(entry clievent.EventEntry, replayPhase bool)

// PersistSink is the event log's persistence hook, called after every Append
// and AppendBatch with a defensive copy of the appended entries (the sink may
// retain the slice) and replayPhase=true while sinkReady is false, i.e. the
// Append preceded SetPersistSink; the Persister drops and counts such batches
// so a mis-ordered InjectHistory cannot create a replay-amplification loop.
// Implementations MUST be non-blocking: EventLog releases l.mu before invoking
// the sink precisely so a slow sink cannot stall the ring.
// internal/eventlog/persist defines its own PersistSink over on-disk schema
// entries; the two types are a deliberate seam and session/eventlog_bridge.go
// is the only translator between them.
type PersistSink func(entries []clievent.EventEntry, replayPhase bool)

// SetPersistSink installs the on-disk persistence hook and is the only public
// way to flip sinkReady to true. Calling it again replaces the sink; nil
// clears the sink AND resets sinkReady so a later re-install re-enters the
// pre-attach phase (otherwise "pause → re-install → InjectHistory" would
// persist duplicate history).
//
// Store order is sink first, sinkReady second, on purpose: the inverse would
// let Append see sinkReady=true then load a nil sink and drop the event. The
// chosen order can only tag a live event replayPhase=true in a sub-ns window
// on a just-attached sink, which the Persister drops and counts. Keep it.
func (l *EventLog) SetPersistSink(fn PersistSink) {
	if fn == nil {
		// Clear sinkReady FIRST so an Append racing the uninstall sees the
		// pre-attach phase before the pointer goes nil (mirror image of the
		// install-order argument above).
		l.sinkReady.Store(false)
		l.persistSinkPtr.Store(nil)
		// Also drop a paired single-entry sink, otherwise Append would keep
		// firing it while AppendBatch silently no-ops.
		l.persistSinkOnePtr.Store(nil)
		return
	}
	// Pointer before sinkReady: see the godoc ordering argument.
	p := fn
	l.persistSinkPtr.Store(&p)
	// A slice-only install retracts any paired single sink; the two may point
	// at different downstream destinations.
	l.persistSinkOnePtr.Store(nil)
	l.sinkReady.Store(true)
}

// SetPersistSinkPair installs the batch sink and a single-entry fast-path
// sink together. Both MUST drain to the same destination — Append uses
// `single`, AppendBatch uses `batch`. nil `single` collapses to
// SetPersistSink(batch).
//
// Same ordering contract as SetPersistSink: pointers before sinkReady. The
// single pointer is stored before the slice pointer so Append's
// "prefer single" dispatch never regresses to a slice-literal alloc once the
// pair is installed (#410).
func (l *EventLog) SetPersistSinkPair(batch PersistSink, single PersistSinkOne) {
	if batch == nil {
		// nil batch = uninstall everything, including the sinkReady reset
		// (mirrors SetPersistSink(nil)).
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

// invokePersistSink fires the slice sink (when set) after the ring mutations
// are committed and l.mu is released. replayPhase is derived from sinkReady at
// call time. `entries` must be safe for the sink to retain — callers pass a
// fresh copy, never a view into the ring, because the ring can wrap shortly
// after.
func (l *EventLog) invokePersistSink(entries []clievent.EventEntry) {
	p := l.persistSinkPtr.Load()
	if p == nil {
		return
	}
	replay := !l.sinkReady.Load()
	if replay {
		// Diagnostic counter for /health: should freeze at the InjectHistory
		// replay total once SetPersistSink has run.
		l.replayInvokeTotal.Add(1)
	}
	(*p)(entries, replay)
}

// invokePersistSinkOne is the single-entry counterpart to invokePersistSink,
// used only by Append. Returns false when no single sink is attached and the
// caller must fall back to the slice path. Shares the replayPhase derivation
// and replayInvokeTotal counter so both sink shapes report identically (#410).
func (l *EventLog) invokePersistSinkOne(entry clievent.EventEntry) bool {
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

// ReplayInvokeTotal returns how many sink invocations observed sinkReady=false
// (replayPhase=true). Diagnostic only: tests assert the
// SetPersistSink-after-InjectHistory ordering with it and /health can expose
// it. Pair with persist.Stats().ReplayLeak: cli>0 && persist==0 means the sink
// had not attached yet (harmless); both >0 means the persister absorbed replay
// batches. Safe from any goroutine.
func (l *EventLog) ReplayInvokeTotal() int64 {
	return l.replayInvokeTotal.Load()
}

// SinkReady reports whether SetPersistSink has wired a persistence hook.
// Paired with ReplayInvokeTotal on /health it distinguishes "sink not attached
// yet" from "ordering window opened in production" (SinkReady=true with the
// counter still climbing). Returns false on a nil receiver so a torn-down
// EventLog during shutdown reports "not ready" instead of panicking.
func (l *EventLog) SinkReady() bool {
	if l == nil {
		return false
	}
	return l.sinkReady.Load()
}
