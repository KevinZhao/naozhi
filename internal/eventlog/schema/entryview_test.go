package schema

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestDecodeEntryView_RoundTrip is the contract guard for EventEntry
// field evolution. A fixture that MIMICS the cli.EventEntry shape is
// marshalled, wrapped in a Record, then decoded through DecodeEntryView.
// Every EntryView accessor must return the fixture value.
//
// This test is deliberately decoupled from the cli package (schema
// cannot import cli). The fixture uses field names chosen to match
// cli.EventEntry exactly. A drift between this fixture and the real
// type is caught by a parallel test in internal/cli that asserts
// EventEntry satisfies schema.EntryView on a round-trip.
func TestDecodeEntryView_RoundTrip(t *testing.T) {
	fixture := map[string]any{
		"time":    int64(1700000000000),
		"uuid":    "a1b2c3d4e5f60708aabbccddeeff0011",
		"type":    "user",
		"summary": "hello world",
		"detail":  "hello world with extra context",
		"images":  []string{"data:image/jpeg;base64,/9j/AAA="},
		// Extra forward-compat field; EntryView must ignore it
		// without erroring (this is what lets additive EventEntry
		// fields roll out without bumping WireVersion).
		"future_field": "ignored",
	}
	payload, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	r := NewEntry(1, payload)
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	buf, err := MarshalRecord(r)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	got, err := UnmarshalRecord(buf)
	if err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}

	view, err := DecodeEntryView(got.Entry)
	if err != nil {
		t.Fatalf("decode view: %v", err)
	}

	if view.Time() != 1700000000000 {
		t.Errorf("Time()=%d, want 1700000000000", view.Time())
	}
	if view.UUID() != "a1b2c3d4e5f60708aabbccddeeff0011" {
		t.Errorf("UUID()=%q, mismatch", view.UUID())
	}
	if view.Kind() != "user" {
		t.Errorf("Kind()=%q, want %q", view.Kind(), "user")
	}
	if !view.HasImages() {
		t.Errorf("HasImages()=false, fixture had one image")
	}
}

// TestDecodeEntryView_EmptyImages covers the HasImages=false path. An
// entry without an "images" field should return false, not panic and
// not return true due to a nil-vs-empty slice confusion.
func TestDecodeEntryView_EmptyImages(t *testing.T) {
	payload := []byte(`{"time":1,"uuid":"aa","type":"text"}`)
	view, err := DecodeEntryView(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.HasImages() {
		t.Errorf("HasImages()=true on fixture without images field")
	}
}

// TestDecodeEntryView_MissingUUID returns "" without error — UUID
// absence is legal during the migration window where historical
// EventEntries (parsed from Claude JSONL) may lack the field.
// MergedSource is responsible for deriving a stable UUID in that case;
// schema must NOT reject the record.
func TestDecodeEntryView_MissingUUID(t *testing.T) {
	payload := []byte(`{"time":1,"type":"user","summary":"hi"}`)
	view, err := DecodeEntryView(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.UUID() != "" {
		t.Errorf("UUID()=%q, want empty", view.UUID())
	}
	if view.Time() != 1 {
		t.Errorf("Time()=%d, want 1", view.Time())
	}
}

// TestDecodeEntryView_EmptyPayload rejects zero bytes — that's a
// caller bug (ValidateRecord already caught it at write time) rather
// than a data condition the reader tries to interpret.
func TestDecodeEntryView_EmptyPayload(t *testing.T) {
	_, err := DecodeEntryView(nil)
	if !errors.Is(err, ErrEntryMissingPayload) {
		t.Errorf("err=%v, want ErrEntryMissingPayload", err)
	}
	_, err = DecodeEntryView(json.RawMessage{})
	if !errors.Is(err, ErrEntryMissingPayload) {
		t.Errorf("err=%v, want ErrEntryMissingPayload", err)
	}
}

// TestDecodeEntryView_MalformedJSON returns a non-sentinel error so
// callers can distinguish "schema-level invariant broken" (sentinels)
// from "this line is corrupt, skip it". The persist package relies on
// this distinction to decide whether to abort reading the file.
func TestDecodeEntryView_MalformedJSON(t *testing.T) {
	_, err := DecodeEntryView([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if errors.Is(err, ErrEntryMissingPayload) {
		t.Errorf("malformed JSON wrongly mapped to ErrEntryMissingPayload")
	}
}
