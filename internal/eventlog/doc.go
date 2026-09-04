// Package eventlog is a documentation-only aggregator for the on-disk
// event-log subsystem; it exports no symbols. The code lives in two leaf
// subpackages with a one-way dependency (persist → schema):
//
//	internal/eventlog/schema   — wire format: Record envelope, WireVersion /
//	                             MinReadVersion read window, (un)marshal
//	                             helpers, MaxRecordBytes. Record.Entry stays
//	                             json.RawMessage so schema never imports cli.
//
//	internal/eventlog/persist  — write path: async Persister, per-key sinks,
//	                             sparse idx, rotation, startup recovery.
//
// There is no shared EventEntry type to hoist here: it is defined in
// internal/cli, persist carries only persist.Entry (JSON bytes + TimeMS),
// and a parent-level type would either create an import cycle or duplicate
// cli's (#1186). Readers/encoders import schema; the write path imports
// persist.
package eventlog
