package schema

import (
	"encoding/json"
	"errors"
	"fmt"
)

// WireVersion is the current schema version for Record envelopes and
// <keyhash>.log file formats. Bump this constant when the JSON shape
// changes in a way older readers cannot safely ignore.
//
// Policy:
//   - Additive EventEntry fields with `omitempty` → no bump (old readers
//     simply drop the unknown field).
//   - New Record.Type values → bump (older readers would treat them as
//     malformed and skip the entire line, losing events).
//   - Changing a field's JSON shape (int → string, etc.) → bump.
//
// Readers MUST refuse to load a file whose header declares a WireVersion
// greater than this constant, falling back to the Claude CLI JSONL source.
// This failure mode is intentional — silently parsing a newer-format file
// with a best-effort subset would mask real compatibility breakage.
const WireVersion = 1

// Record types. Exactly one of Header / Entry is populated per record,
// selected by Type.
const (
	TypeHeader = "header"
	TypeEntry  = "entry"
)

// MaxRecordBytes caps the size of a single serialized Record (header +
// length-prefix line content together), enforced by the framing layer.
// 4 MiB is enough for a large multi-image user message (several 80 KiB
// thumbnails + Detail text) while bounding peak write amplification and
// memory use on the reader side. Records over this limit are rejected at
// write time; rejections are a bug in the caller, not a data condition
// the reader should try to recover from.
const MaxRecordBytes = 4 * 1024 * 1024

// Record is the envelope every persisted line carries.
//
// Invariants (enforced by Validate):
//
//   - V must match WireVersion (readers check; writers never emit other)
//   - Seq must be strictly monotonic within a file (0, 1, 2, …) — the
//     header is always Seq=0
//   - Exactly one of Header (when Type == TypeHeader) / Entry (when
//     Type == TypeEntry) is non-zero
//   - Entry is kept as json.RawMessage so schema owns framing but not
//     EventEntry semantics
type Record struct {
	V      int             `json:"v"`
	Seq    uint64          `json:"seq"`
	Type   string          `json:"type"`
	Entry  json.RawMessage `json:"entry,omitempty"`
	Header *FileHeader     `json:"header,omitempty"`
}

// FileHeader is the payload of the first record (Seq=0) in every log
// file. It is self-describing so a file can be identified without any
// external index.
type FileHeader struct {
	Version   int    `json:"v"`          // echoes Record.V at write time; readers compare both
	Key       string `json:"key"`        // original session key (not hashed) — source of truth for keyhash reverse lookup
	CreatedAt int64  `json:"created_at"` // unix ms when the file was first created
	Generator string `json:"gen,omitempty"`
}

// MarshalRecord serializes r to JSON and validates invariants before
// writing. Callers (the persist package) must pair this with the
// length-prefix framing in persist.
//
// Returns ErrRecordTooLarge when the encoded bytes exceed MaxRecordBytes
// — the persist layer will drop the batch and counter the loss rather
// than block.
func MarshalRecord(r *Record) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	buf, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal record: %w", err)
	}
	if len(buf) > MaxRecordBytes {
		return nil, fmt.Errorf("record seq=%d size=%d: %w",
			r.Seq, len(buf), ErrRecordTooLarge)
	}
	return buf, nil
}

// UnmarshalRecord parses a single JSON-encoded record. Returns
// ErrUnsupportedVersion when the record declares a WireVersion newer
// than we can read; callers should stop reading the file on this error
// (subsequent bytes are undefined).
//
// Does NOT validate Header / Entry exclusivity — a reader may want to
// accept forward-compatible record types it doesn't fully understand
// (see readers_accept_unknown_record_types in
// internal/eventlog/persist). Use Validate() explicitly when strict
// checking is required.
func UnmarshalRecord(data []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("unmarshal record: %w", err)
	}
	if r.V > WireVersion {
		return nil, fmt.Errorf("record v=%d: %w", r.V, ErrUnsupportedVersion)
	}
	if r.V <= 0 {
		return nil, fmt.Errorf("record v=%d: %w", r.V, ErrInvalidVersion)
	}
	return &r, nil
}

// Validate checks the invariants documented on Record.
func (r *Record) Validate() error {
	if r == nil {
		return ErrNilRecord
	}
	if r.V != WireVersion {
		return fmt.Errorf("record v=%d (want %d): %w",
			r.V, WireVersion, ErrInvalidVersion)
	}
	switch r.Type {
	case TypeHeader:
		if r.Header == nil {
			return ErrHeaderMissingPayload
		}
		if len(r.Entry) != 0 {
			return ErrHeaderHasEntry
		}
		if r.Seq != 0 {
			return fmt.Errorf("header seq=%d (want 0): %w",
				r.Seq, ErrHeaderBadSeq)
		}
		if r.Header.Version != r.V {
			return fmt.Errorf("header version mismatch: record v=%d header v=%d: %w",
				r.V, r.Header.Version, ErrInvalidVersion)
		}
		if r.Header.Key == "" {
			return ErrHeaderMissingKey
		}
		if r.Header.CreatedAt <= 0 {
			return ErrHeaderMissingCreatedAt
		}
	case TypeEntry:
		if r.Header != nil {
			return ErrEntryHasHeader
		}
		if len(r.Entry) == 0 {
			return ErrEntryMissingPayload
		}
	default:
		return fmt.Errorf("type=%q: %w", r.Type, ErrUnknownType)
	}
	return nil
}

// NewHeader constructs a valid TypeHeader Record from the given metadata.
// A convenience wrapper so callers don't have to remember the Version-V
// mirror rule or Seq=0 constraint.
func NewHeader(key string, createdAtMS int64, generator string) *Record {
	return &Record{
		V:    WireVersion,
		Seq:  0,
		Type: TypeHeader,
		Header: &FileHeader{
			Version:   WireVersion,
			Key:       key,
			CreatedAt: createdAtMS,
			Generator: generator,
		},
	}
}

// NewEntry constructs a valid TypeEntry Record from an already-serialized
// payload. `seq` must be > 0 (seq=0 is the header slot). `entryJSON` is
// the raw JSON of an EventEntry (or compatible payload); schema does not
// validate its shape beyond "non-empty".
func NewEntry(seq uint64, entryJSON []byte) *Record {
	return &Record{
		V:     WireVersion,
		Seq:   seq,
		Type:  TypeEntry,
		Entry: append(json.RawMessage(nil), entryJSON...),
	}
}

// Errors that users of this package may want to match with errors.Is.
var (
	ErrNilRecord              = errors.New("schema: nil record")
	ErrInvalidVersion         = errors.New("schema: invalid version")
	ErrUnsupportedVersion     = errors.New("schema: unsupported (newer) version")
	ErrUnknownType            = errors.New("schema: unknown record type")
	ErrHeaderMissingPayload   = errors.New("schema: type=header without header payload")
	ErrHeaderHasEntry         = errors.New("schema: type=header with entry payload")
	ErrHeaderBadSeq           = errors.New("schema: header must have seq=0")
	ErrHeaderMissingKey       = errors.New("schema: header missing key")
	ErrHeaderMissingCreatedAt = errors.New("schema: header missing created_at")
	ErrEntryHasHeader         = errors.New("schema: type=entry with header payload")
	ErrEntryMissingPayload    = errors.New("schema: type=entry without entry payload")
	ErrRecordTooLarge         = errors.New("schema: record exceeds MaxRecordBytes")
)
