// Package eventlog is a documentation-only aggregator for the on-disk
// event-log subsystem. It deliberately exports no symbols: the real code
// lives in two leaf subpackages with a one-way dependency
// (persist → schema).
//
// Layout and why the parent is intentionally empty (R250-ARCH-25 / #1186):
//
//	internal/eventlog/schema   — wire format: the Record envelope,
//	                             WireVersion / MinReadVersion read-window
//	                             handshake, and (un)marshal helpers. Owns
//	                             framing-adjacent constants (MaxRecordBytes)
//	                             but NOT EventEntry semantics: Record.Entry
//	                             stays json.RawMessage on purpose so schema
//	                             never has to import cli.
//
//	internal/eventlog/persist  — write path: the async Persister, per-key
//	                             sinks, idx sparse index, rotation and
//	                             startup recovery. Imports schema (down),
//	                             never the reverse.
//
// There is NO shared "EventEntry" type to hoist into this parent. The
// EventEntry shape is defined in internal/cli; persist deliberately
// carries only persist.Entry (already-serialised JSON bytes + TimeMS) so
// it stays free of a cli import, and schema treats the entry payload as
// opaque json.RawMessage. Promoting a parent-level types.go would either
// create an import cycle (a parent both leaves depend on, importing back
// down) or duplicate cli's type — so the parent stays doc-only. New
// consumers pick the leaf that matches their direction: readers/encoders
// import schema; the write path imports persist (which re-exposes the
// schema surface it needs).
package eventlog
