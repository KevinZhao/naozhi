// Package eventlog is the aggregator/landing package for naozhi's
// per-session event-log subsystem. It holds no code of its own — its job
// is to be the single documented entry point that routes a new consumer to
// the right subpackage instead of leaving them to guess between the
// lower-level packages and inherit whichever one's symbols.
//
// R250-ARCH-25 (#1186): before this file the internal/eventlog directory
// was empty (not an importable package), so the "parent package holds
// shared types" expectation the directory layout implies had nowhere to
// land. The fix is deliberately a doc/aggregator package, NOT a type move:
// the event-log subsystem does not actually have a type that both
// subpackages co-own and duplicate. The shared wire vocabulary already
// lives in exactly one place (schema, the dependency-graph leaf), and the
// pre-marshal in-memory EventEntry shape lives in internal/cli — moving
// either "up" here would only force cli / cron / session / history (all
// current importers) to re-import for no structural gain while widening the
// blast radius into packages outside the eventlog tree. So the navigability
// symptom is fixed here (one documented entry point); the symbols stay
// where the import graph already wants them.
//
// # Subpackage map
//
// The token "eventlog" spans several positions in the data flow; package
// boundaries make the split explicit (see also internal/eventlog/persist
// doc.go and R237-ARCH-13):
//
//   - internal/eventlog/schema — wire-format types (Record envelope,
//     WireVersion / MinReadVersion, MaxRecordBytes, framing constants).
//     The dependency-graph leaf: imported by persist and by readers, imports
//     nothing else in the eventlog tree. Import this when you need the
//     on-disk record shape or version constants.
//   - internal/eventlog/persist — the ON-DISK writer. Consumes batches via
//     a non-blocking PersistSink, writes <keyhash>.log + sparse
//     <keyhash>.idx with strict log→idx→fsync ordering, and handles rotate
//   - crash recovery. Imports schema (downward). Import this when you are
//     wiring the write path.
//
// Two packages outside this tree complete the picture and are intentionally
// NOT re-exported here:
//
//   - internal/cli (cli.EventLog) — the IN-MEMORY ring buffer that produces
//     every event and owns the pre-marshal cli.EventEntry shape + its own
//     cli.PersistSink contract. cli is upstream of persist via the bridge,
//     not a child of this package.
//   - internal/history/naozhilog — the REPLAY reader that re-hydrates
//     history from the files persist wrote.
//
// The cli↔persist translation (cli.EventEntry → persist.Entry) lives solely
// in internal/session/eventlog_bridge.go. The longer-term unification of the
// three append/read/subscribe surfaces behind a single EventStore interface
// is tracked under R243-ARCH-12 (#1369) and is out of scope for this
// aggregator: it requires an evals pass to avoid regressing each tier's
// perf hot path.
package eventlog
