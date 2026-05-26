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
//     the reverse. Record.Entry stays json.RawMessage so schema owns the
//     envelope bytes and the callers (persist / naozhilog / cli) own the
//     higher-level (de)serialization.
//
//   - The previous EntryView interface (DEADCODE-14, #1207) was removed:
//     it was abstraction-without-consumers. The same "EventEntry JSON
//     shape doesn't drift" guarantee is preserved by the cli package's
//     own round-trip tests and by Record's MarshalRecord/UnmarshalRecord
//     contract tests in record_test.go.
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
