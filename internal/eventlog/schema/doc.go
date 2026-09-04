// Package schema defines the on-disk wire format for naozhi's per-session
// event log persistence (see docs/rfc/event-log-persistence.md):
//
//   - Record: the envelope every line in <keyhash>.log carries
//   - FileHeader: the metadata record that is always line #0
//   - IdxEntry: the fixed-width record format of the .idx sidecar
//
// schema must NOT import cli (cli depends on schema). Record.Entry is
// json.RawMessage so schema owns the envelope bytes and callers (persist /
// naozhilog) own EventEntry (de)serialization. Serialization is UTF-8 JSON
// with one trailing newline, length-prefixed by internal/eventlog/persist
// (framing.go); schema does no file I/O.
//
// WireVersion bumps are breaking: a reader seeing a newer WireVersion must
// refuse the file and fall back to Claude CLI JSONL. Additive fields need
// no bump as long as older readers can ignore them.
package schema
