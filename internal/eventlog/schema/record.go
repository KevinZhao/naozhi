package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// WireVersion is the schema version of Record envelopes and <keyhash>.log
// files. Bump policy:
//   - Additive EventEntry fields with omitempty → no bump.
//   - New Record.Type values or a changed field JSON shape → bump.
//
// Readers MUST refuse a file whose header declares a WireVersion greater
// than this constant and fall back to the Claude CLI JSONL source.
const WireVersion = 1

// MinReadVersion is the oldest WireVersion the reader still accepts.
// Validate / UnmarshalRecord reject v < MinReadVersion and v > WireVersion.
// Kept distinct from WireVersion so a future bump can advance the write
// format while still reading the old one for a migration window.
const MinReadVersion = 1

// Record types. Exactly one of Header / Entry is populated per record,
// selected by Type.
const (
	TypeHeader = "header"
	TypeEntry  = "entry"
)

// MaxRecordBytes caps a single serialized Record, enforced by the framing
// layer. 4 MiB fits a large multi-image user message while bounding reader
// memory; an oversize record is a caller bug, rejected at write time.
const MaxRecordBytes = 4 * 1024 * 1024

// Record is the envelope every persisted line carries.
//
// Invariants (enforced by Validate):
//   - V equals WireVersion
//   - Seq is strictly monotonic within a file; the header is always Seq=0
//   - Exactly one of Header (TypeHeader) / Entry (TypeEntry) is set
//   - Entry stays json.RawMessage: schema owns framing, not EventEntry
//     semantics
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

// marshalRecordBufPool reuses bytes.Buffer instances so MarshalRecord avoids
// json.Marshal's per-call backing-array alloc.
var marshalRecordBufPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 4*1024))
	},
}

// marshalRecordPoolMaxCap drops oversized buffers from the pool so a
// one-off MaxRecordBytes record does not pin memory across the
// process lifetime.
const marshalRecordPoolMaxCap = 64 * 1024

// MarshalRecord validates r and serializes it to JSON; the persist layer
// pairs the bytes with length-prefix framing. Returns ErrRecordTooLarge
// when the encoding exceeds MaxRecordBytes.
//
// The returned []byte is a fresh copy: the pooled buffer is re-filed by the
// defer, so returning its bytes directly would let the next pool user
// overwrite a slice a caller still holds (#996). Hot-path callers avoid the
// copy via MarshalRecordInto.
func MarshalRecord(r *Record) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	buf := marshalRecordBufPool.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() > marshalRecordPoolMaxCap {
			return
		}
		buf.Reset()
		marshalRecordBufPool.Put(buf)
	}()
	buf.Reset()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("marshal record: %w", err)
	}
	body := buf.Bytes()
	if n := len(body); n > 0 && body[n-1] == '\n' {
		body = body[:n-1]
	}
	if len(body) > MaxRecordBytes {
		return nil, fmt.Errorf("record seq=%d size=%d: %w",
			r.Seq, len(body), ErrRecordTooLarge)
	}
	out := make([]byte, len(body))
	copy(out, body)
	return out, nil
}

// MarshalRecordInto encodes r and appends it to buf, returning the slice of
// buf holding the record. buf MUST be empty or already flushed since
// callers treat the returned slice as a self-contained body. Same
// validation and ErrRecordTooLarge contract as MarshalRecord, minus the
// per-call alloc — Persister.handleBatch pools the destination buffer.
func MarshalRecordInto(buf *bytes.Buffer, r *Record) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if buf == nil {
		return nil, fmt.Errorf("marshal record: nil buffer")
	}
	startLen := buf.Len()
	// json.NewEncoder allocates an encodeState per call, so borrow a pooled
	// encoder bound to its own buffer (Encoder has no Reset) and copy the
	// finished body into buf (#1537).
	enc := recordEncPool.Get().(*recordEnc)
	enc.buf.Reset()
	// Default HTML escaping stays ON: on-disk output must remain
	// byte-identical to earlier entry records (#1537).
	if err := enc.enc.Encode(r); err != nil {
		putRecordEnc(enc)
		return nil, fmt.Errorf("marshal record: %w", err)
	}
	enc.buf.Truncate(enc.buf.Len() - 1) // Encode always appends one '\n'
	if enc.buf.Len() > MaxRecordBytes {
		size := enc.buf.Len()
		putRecordEnc(enc)
		return nil, fmt.Errorf("record seq=%d size=%d: %w",
			r.Seq, size, ErrRecordTooLarge)
	}
	// Copy into buf so the returned slice outlives the pooled encoder.
	buf.Write(enc.buf.Bytes())
	putRecordEnc(enc)
	return buf.Bytes()[startLen:], nil
}

// recordEnc pairs a bytes.Buffer with a json.Encoder bound to it so the
// encodeState scratch is reused across MarshalRecordInto calls.
type recordEnc struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var recordEncPool = sync.Pool{
	New: func() any {
		buf := bytes.NewBuffer(make([]byte, 0, 4*1024))
		// HTML escaping stays ON; see MarshalRecordInto.
		return &recordEnc{buf: buf, enc: json.NewEncoder(buf)}
	},
}

// recordEncMaxCap caps buffer reuse so a one-off oversize record does not
// permanently pin a large heap allocation in the pool.
const recordEncMaxCap = 64 * 1024

func putRecordEnc(e *recordEnc) {
	if e == nil {
		return
	}
	if e.buf.Cap() > recordEncMaxCap {
		return
	}
	e.buf.Reset()
	recordEncPool.Put(e)
}

// UnmarshalRecord parses one JSON record. Returns ErrUnsupportedVersion for
// a version outside [MinReadVersion, WireVersion]; callers should stop
// reading the file (subsequent bytes are undefined). Does NOT check
// Header / Entry exclusivity so readers can accept forward-compatible
// record types; call Validate for strict checking.
func UnmarshalRecord(data []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("unmarshal record: %w", err)
	}
	if r.V <= 0 {
		return nil, fmt.Errorf("record v=%d: %w", r.V, ErrInvalidVersion)
	}
	if r.V > WireVersion {
		return nil, fmt.Errorf("record v=%d: %w", r.V, ErrUnsupportedVersion)
	}
	// Checked after r.V <= 0 so malformed (zero/negative) versions keep
	// surfacing ErrInvalidVersion rather than ErrUnsupportedVersion.
	if r.V < MinReadVersion {
		return nil, fmt.Errorf("record v=%d: %w", r.V, ErrUnsupportedVersion)
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

// NewHeader constructs a valid TypeHeader Record (Header.Version mirrors V,
// Seq=0).
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

// NewEntry constructs a TypeEntry Record from an already-serialized
// payload. seq must be > 0 (0 is the header slot). entryJSON is taken over
// by the returned Record without copying: callers must not retain or
// mutate it afterwards.
func NewEntry(seq uint64, entryJSON []byte) *Record {
	return &Record{
		V:     WireVersion,
		Seq:   seq,
		Type:  TypeEntry,
		Entry: json.RawMessage(entryJSON),
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
