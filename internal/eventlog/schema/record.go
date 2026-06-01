package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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

// MinReadVersion is the oldest WireVersion that the current reader will
// still accept. R230B-ARCH-21: when WireVersion bumps to 2, callers
// upgrading from a v1-only build need to either drop their v1 files or
// keep this value at 1 for one release cycle so the dashboard can still
// read pre-bump history. Bumping MinReadVersion to N is the explicit
// "we no longer guarantee back-compat with versions < N" signal.
//
// Today MinReadVersion == WireVersion == 1; Validate / UnmarshalRecord
// reject anything below MinReadVersion AND anything above WireVersion.
// The two boundaries are kept distinct so a future bump can advance
// WireVersion (writes new format) while leaving MinReadVersion behind
// (reads continue to accept the old format) for the migration window.
const MinReadVersion = 1

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

// marshalRecordBufPool reuses bytes.Buffer instances so MarshalRecord
// avoids the per-call backing-array alloc that json.Marshal performs
// internally. Persister.handleBatch is the hot caller (~50 sess × 50
// evt/s ≈ 2500 marshal/s in 500-session deployments). R245-PERF-12.
var marshalRecordBufPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 4*1024))
	},
}

// marshalRecordPoolMaxCap drops oversized buffers from the pool so a
// one-off MaxRecordBytes record does not pin memory across the
// process lifetime.
const marshalRecordPoolMaxCap = 64 * 1024

// MarshalRecord serializes r to JSON and validates invariants before
// writing. Callers (the persist package) must pair this with the
// length-prefix framing in persist.
//
// Returns ErrRecordTooLarge when the encoded bytes exceed MaxRecordBytes
// — the persist layer will drop the batch and counter the loss rather
// than block.
//
// The encoded JSON is built via a pooled bytes.Buffer + json.Encoder
// (R245-PERF-12) to avoid the per-call backing-array alloc that
// json.Marshal performs internally. The returned []byte is a fresh
// copy (independent of the pooled buffer) so the caller may retain
// it past the end of MarshalRecord — the buffer is reset and re-filed
// before return.
//
// R249-PERF-22 (#996) verify-stale: the trailing `out := make + copy`
// at the end is a hard correctness invariant of the pooled-buffer
// design, not a missed optimisation. Returning `body` directly would
// hand the caller a slice whose backing array gets re-filed into the
// pool by the defer below; the next pool consumer would Reset() that
// array and start writing into the byte range a previous caller still
// holds. Hot-path callers (Persister.handleBatch) avoid the copy by
// using MarshalRecordInto with their own scratch buffer; the surviving
// MarshalRecord callers (initial header write, tests, recovery) run
// at most once per file lifetime so the per-call alloc is in the
// noise. Do not "optimise" by removing the copy — see
// MarshalRecordInto immediately below for the API that lets callers
// own the buffer.
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

// MarshalRecordInto encodes r as JSON and appends the bytes to dst,
// returning the slice of dst that holds the encoded record. dst MUST
// be empty (or have its content treated as already-flushed) because
// callers walk the returned slice as a self-contained record body.
//
// Mirrors MarshalRecord's validation and ErrRecordTooLarge contract,
// but lets the caller pool the destination buffer to avoid the
// per-call alloc that json.Marshal performs for its own scratch
// space (encodeState in encoding/json).
//
// R245-PERF-12 [REFACTOR R242-PERF-13]: persister.handleBatch is the
// hot path (≥5 events/s × N sessions) and was the heaviest remaining
// reflection alloc in the persist tier; this helper plus a
// sync.Pool-backed bytes.Buffer in persister gets handleBatch off
// the per-event encodeState alloc, mirroring bridgeEncPool in
// internal/session/eventlog_bridge.go.
//
// json.Encoder always appends a trailing '\n' after the JSON object;
// we strip it here so the returned slice is byte-identical to
// MarshalRecord's output (the framing layer adds its own newline).
func MarshalRecordInto(buf *bytes.Buffer, r *Record) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if buf == nil {
		return nil, fmt.Errorf("marshal record: nil buffer")
	}
	startLen := buf.Len()
	// R20260531070014-PERF-3 (#1537): json.NewEncoder allocates an
	// encodeState (reflection scratch) on every call. handleBatch drives
	// this per entry on the persist hot path, so we borrow a pooled
	// encoder (bound to its own scratch buffer) instead of building a
	// fresh one each time. The encoder cannot be retargeted to `buf`
	// (encoding/json.Encoder has no Reset), so it writes into its own
	// pooled buffer and we copy the finished body into the caller's
	// `buf` — that copy is the JSON-body copy the persist tier needs
	// anyway (see #1524), so the only cost amortised away here is the
	// per-call encodeState alloc.
	enc := recordEncPool.Get().(*recordEnc)
	enc.buf.Reset()
	// Escape behaviour is preserved from the pre-pool implementation
	// (json.NewEncoder default = HTML escaping ON). The on-disk format is
	// consumed only via UnmarshalRecord, so escaped (<) and raw (<)
	// bytes decode identically; #1537 only swaps the encoder allocation
	// strategy and must not change existing entry-record output.
	if err := enc.enc.Encode(r); err != nil {
		putRecordEnc(enc)
		return nil, fmt.Errorf("marshal record: %w", err)
	}
	// Encode appended bytes plus a trailing '\n'. Strip the newline so
	// the returned slice matches MarshalRecord exactly — the framing
	// layer adds its own '\n' separator.
	enc.buf.Truncate(enc.buf.Len() - 1) // Encode always appends one '\n'
	if enc.buf.Len() > MaxRecordBytes {
		size := enc.buf.Len()
		putRecordEnc(enc)
		return nil, fmt.Errorf("record seq=%d size=%d: %w",
			r.Seq, size, ErrRecordTooLarge)
	}
	// Copy the encoded body into the caller's buffer so the returned
	// slice survives the pooled encoder going back into recordEncPool.
	buf.Write(enc.buf.Bytes())
	putRecordEnc(enc)
	return buf.Bytes()[startLen:], nil
}

// recordEnc pairs a bytes.Buffer with a json.Encoder bound to it so the
// encodeState reflection scratch is reused across MarshalRecordInto
// calls. Mirrors the bridgeEncPool idiom in
// internal/session/eventlog_bridge.go. R20260531070014-PERF-3 (#1537).
type recordEnc struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var recordEncPool = sync.Pool{
	New: func() any {
		buf := bytes.NewBuffer(make([]byte, 0, 4*1024))
		// Default escape behaviour (HTML escaping ON) — preserves the
		// pre-pool MarshalRecordInto output. See its body comment.
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
	if r.V <= 0 {
		return nil, fmt.Errorf("record v=%d: %w", r.V, ErrInvalidVersion)
	}
	if r.V > WireVersion {
		return nil, fmt.Errorf("record v=%d: %w", r.V, ErrUnsupportedVersion)
	}
	// R230B-ARCH-21: forward-compat negotiation. v < MinReadVersion is
	// flagged the same way as v > WireVersion — readers refuse rather
	// than silently fudge through a known-broken format. Today
	// MinReadVersion == 1 so this branch is unreachable, but pinning
	// the contract now means a later bump (e.g. WireVersion=2,
	// MinReadVersion=2 after migration) only requires changing two
	// constants, not adding a new check. Order matters: r.V <= 0 is
	// checked first so malformed records (negative / zero) keep
	// surfacing ErrInvalidVersion rather than the unsupported alias.
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
//
// Ownership: entryJSON is assumed freshly allocated by the caller (e.g.
// json.Marshal output in invokePersistSink) and is taken over by the
// returned Record. Callers must not retain or mutate entryJSON after this
// call. Skipping the defensive copy halves per-entry alloc on the persist
// hot path.
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
