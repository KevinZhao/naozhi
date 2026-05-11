package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestRecord_HeaderRoundTrip exercises the "header line" path:
// NewHeader → Validate → MarshalRecord → UnmarshalRecord → field equality.
func TestRecord_HeaderRoundTrip(t *testing.T) {
	h := NewHeader("dashboard:direct:alice:general", 1700000000000, "naozhi-test")
	if err := h.Validate(); err != nil {
		t.Fatalf("fresh header did not validate: %v", err)
	}
	buf, err := MarshalRecord(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Readable on-disk form — operators should be able to `less` the log
	// and recognize lines without a tool. A regression that breaks JSON
	// compactness would force us to re-justify the "human readable"
	// claim in the RFC.
	if !json.Valid(buf) {
		t.Fatalf("marshalled bytes are not valid JSON: %q", buf)
	}
	if !bytes.Contains(buf, []byte(`"type":"header"`)) {
		t.Errorf("marshalled header missing type discriminator: %q", buf)
	}

	got, err := UnmarshalRecord(buf)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.V != WireVersion {
		t.Errorf("V=%d, want %d", got.V, WireVersion)
	}
	if got.Seq != 0 {
		t.Errorf("Seq=%d, want 0", got.Seq)
	}
	if got.Type != TypeHeader {
		t.Errorf("Type=%q, want %q", got.Type, TypeHeader)
	}
	if got.Header == nil {
		t.Fatalf("Header missing")
	}
	if got.Header.Key != h.Header.Key {
		t.Errorf("Key=%q, want %q", got.Header.Key, h.Header.Key)
	}
	if got.Header.CreatedAt != h.Header.CreatedAt {
		t.Errorf("CreatedAt=%d, want %d", got.Header.CreatedAt, h.Header.CreatedAt)
	}
	if got.Header.Generator != h.Header.Generator {
		t.Errorf("Generator=%q, want %q", got.Header.Generator, h.Header.Generator)
	}
	if len(got.Entry) != 0 {
		t.Errorf("Header record carries Entry payload (len=%d)", len(got.Entry))
	}
}

// TestRecord_EntryRoundTrip exercises the "entry line" path. The Entry
// payload is kept as raw JSON so downstream callers own EventEntry shape.
func TestRecord_EntryRoundTrip(t *testing.T) {
	payload := []byte(`{"time":1700000001000,"uuid":"aa00","type":"user","summary":"hi"}`)
	r := NewEntry(1, payload)
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	buf, err := MarshalRecord(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnmarshalRecord(buf)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Seq != 1 {
		t.Errorf("Seq=%d, want 1", got.Seq)
	}
	if got.Type != TypeEntry {
		t.Errorf("Type=%q, want %q", got.Type, TypeEntry)
	}
	// Entry payload survives round-trip byte-for-byte so downstream
	// consumers get the same bytes they wrote.
	if !bytes.Equal(got.Entry, payload) {
		t.Errorf("Entry payload not preserved\n got:  %q\n want: %q",
			got.Entry, payload)
	}
}

// TestRecord_Validate_RejectsMalformed covers the invariants documented
// on the Record struct. Table-driven so adding a new invariant is a
// single row rather than another full test.
func TestRecord_Validate_RejectsMalformed(t *testing.T) {
	tests := []struct {
		name    string
		build   func() *Record
		wantErr error
	}{
		{
			name:    "nil record",
			build:   func() *Record { return nil },
			wantErr: ErrNilRecord,
		},
		{
			name: "v=0",
			build: func() *Record {
				r := NewHeader("k", 1, "")
				r.V = 0
				return r
			},
			wantErr: ErrInvalidVersion,
		},
		{
			name: "v mismatch between record and header",
			build: func() *Record {
				r := NewHeader("k", 1, "")
				r.Header.Version = 999
				return r
			},
			wantErr: ErrInvalidVersion,
		},
		{
			name: "header seq != 0",
			build: func() *Record {
				r := NewHeader("k", 1, "")
				r.Seq = 42
				return r
			},
			wantErr: ErrHeaderBadSeq,
		},
		{
			name: "header missing key",
			build: func() *Record {
				r := NewHeader("", 1, "")
				return r
			},
			wantErr: ErrHeaderMissingKey,
		},
		{
			name: "header missing created_at",
			build: func() *Record {
				r := NewHeader("k", 0, "")
				return r
			},
			wantErr: ErrHeaderMissingCreatedAt,
		},
		{
			name: "header carries entry payload",
			build: func() *Record {
				r := NewHeader("k", 1, "")
				r.Entry = json.RawMessage(`{}`)
				return r
			},
			wantErr: ErrHeaderHasEntry,
		},
		{
			name: "entry missing payload",
			build: func() *Record {
				return &Record{V: WireVersion, Seq: 1, Type: TypeEntry}
			},
			wantErr: ErrEntryMissingPayload,
		},
		{
			name: "entry carries header",
			build: func() *Record {
				r := NewEntry(1, []byte(`{}`))
				r.Header = &FileHeader{Version: WireVersion, Key: "k", CreatedAt: 1}
				return r
			},
			wantErr: ErrEntryHasHeader,
		},
		{
			name: "unknown type",
			build: func() *Record {
				return &Record{V: WireVersion, Seq: 1, Type: "footer"}
			},
			wantErr: ErrUnknownType,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.build()
			err := r.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Validate = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}

// TestRecord_MarshalRecord_RejectsOversize guarantees the 4 MiB cap is
// enforced at the marshal boundary. A producer emitting a bigger record
// is a caller bug (upstream must filter) — schema refuses to write it
// rather than let the persist layer discover the limit mid-write.
func TestRecord_MarshalRecord_RejectsOversize(t *testing.T) {
	// Build an entry whose payload alone exceeds the cap. Using Repeat on
	// a JSON-safe byte means the encode step cannot magically shrink it.
	payload := []byte(`{"time":1,"uuid":"aa","type":"user","detail":"` +
		strings.Repeat("a", MaxRecordBytes+1) + `"}`)
	r := NewEntry(1, payload)
	_, err := MarshalRecord(r)
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("MarshalRecord oversize = %v, want ErrRecordTooLarge", err)
	}
}

// TestRecord_UnmarshalRecord_RejectsNewerVersion documents the
// forward-compat policy: readers must refuse newer files so the caller
// can fall back to Claude CLI JSONL rather than parse a subset.
func TestRecord_UnmarshalRecord_RejectsNewerVersion(t *testing.T) {
	// Construct a record with V in the future. We can't use MarshalRecord
	// (Validate would reject), so craft the JSON by hand — this is exactly
	// what a newer naozhi binary would produce for its own WireVersion.
	future := []byte(`{"v":999,"seq":0,"type":"header","header":{"v":999,"key":"k","created_at":1}}`)
	_, err := UnmarshalRecord(future)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("unmarshal future version = %v, want ErrUnsupportedVersion", err)
	}
}

// TestRecord_UnmarshalRecord_RejectsInvalidVersion covers V<=0 which
// should never appear in a legitimate file; matches the writer-side
// Validate() check.
func TestRecord_UnmarshalRecord_RejectsInvalidVersion(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"v=0", `{"v":0,"seq":1,"type":"entry","entry":{}}`},
		{"v=-1", `{"v":-1,"seq":1,"type":"entry","entry":{}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UnmarshalRecord([]byte(tc.data))
			if !errors.Is(err, ErrInvalidVersion) {
				t.Errorf("err=%v, want ErrInvalidVersion", err)
			}
		})
	}
}

// TestRecord_UnmarshalRecord_RejectsGarbage ensures callers see a
// distinct error vs a semantic one so they can tell "file drifted" from
// "line torn at fsync".
func TestRecord_UnmarshalRecord_RejectsGarbage(t *testing.T) {
	_, err := UnmarshalRecord([]byte("not json at all"))
	if err == nil {
		t.Fatalf("expected decode error, got nil")
	}
	// We don't promise which specific error flavour; only that it's not
	// a "version" error, so callers handling ErrUnsupportedVersion don't
	// fall into the wrong branch.
	if errors.Is(err, ErrUnsupportedVersion) || errors.Is(err, ErrInvalidVersion) {
		t.Errorf("garbage bytes should not map to a version error, got %v", err)
	}
}

// TestRecord_UnmarshalRecord_ReservedFields confirms that forward-compat
// additive fields on Record itself don't break today's reader. (This
// is the guarantee the RFC relies on for additive EventEntry fields.)
func TestRecord_UnmarshalRecord_ReservedFields(t *testing.T) {
	data := []byte(`{"v":1,"seq":2,"type":"entry","entry":{"time":1},"future_field":"ignored"}`)
	got, err := UnmarshalRecord(data)
	if err != nil {
		t.Fatalf("unmarshal additive field: %v", err)
	}
	if got.Seq != 2 {
		t.Errorf("Seq=%d, want 2", got.Seq)
	}
}
