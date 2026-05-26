// Package schema defines the on-disk wire format for naozhi's per-session
// event log persistence (see docs/rfc/event-log-persistence.md).
//
// The package owns:
//
//   - Record: the envelope every line in <keyhash>.log carries
//   - FileHeader: the metadata record that is always line #0
//   - IdxEntry: the fixed-width record format of the .idx sidecar
//
// Design constraints:
//
//   - schema must NOT import the cli package. cli depends on schema, not
//     the reverse. Record.Entry is json.RawMessage so schema owns the
//     envelope bytes and downstream callers (persist / naozhilog) own the
//     EventEntry-shape (de)serialization on top.
//
//   - Record.Entry is deliberately json.RawMessage so schema owns the
//     envelope bytes and the callers (persist / naozhilog) own the
//     higher-level (de)serialization.
//
//   - All serialization is UTF-8 JSON with a single trailing newline,
//     length-prefixed at the framing layer in internal/eventlog/persist
//     (see framing.go). schema itself does not touch file I/O.
//
// WireVersion bumps are breaking: a reader that sees a newer WireVersion
// than it knows must refuse the file and fall back to Claude CLI JSONL.
// Additive fields on Record / FileHeader / the EventEntry payload can be
// rolled forward without a bump as long as older readers can ignore them.
package schema
