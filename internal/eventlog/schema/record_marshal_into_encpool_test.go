package schema

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// TestMarshalRecordInto_ParsesBackIdentically pins R20260531070014-PERF-3
// (#1537): swapping the per-call json.NewEncoder for a pooled encoder MUST
// NOT change the decoded record. We assert the encoded body round-trips to
// an equal Record (the on-disk format is consumed only via UnmarshalRecord,
// so HTML-escape choices are functionally transparent). Plain and
// HTML-escapable payloads are both covered.
func TestMarshalRecordInto_ParsesBackIdentically(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"time":1700000001000,"uuid":"aa00","type":"user","summary":"plain"}`),
		// HTML-escapable chars: < > & must round-trip.
		[]byte(`{"time":1700000002000,"uuid":"bb00","type":"assistant","summary":"a<b>c&d"}`),
	}
	for i, p := range payloads {
		var buf bytes.Buffer
		got, err := MarshalRecordInto(&buf, NewEntry(uint64(i+1), p))
		if err != nil {
			t.Fatalf("payload %d MarshalRecordInto: %v", i, err)
		}
		rec, err := UnmarshalRecord(got)
		if err != nil {
			t.Fatalf("payload %d UnmarshalRecord: %v", i, err)
		}
		if rec.Seq != uint64(i+1) {
			t.Errorf("payload %d seq = %d, want %d", i, rec.Seq, i+1)
		}
		// The entry sub-message is JSON-encoded (HTML escaping may apply
		// to <,>,&), so compare decoded values rather than raw bytes.
		var gotEntry, wantEntry map[string]any
		if err := json.Unmarshal(rec.Entry, &gotEntry); err != nil {
			t.Fatalf("payload %d decode rec.Entry: %v", i, err)
		}
		if err := json.Unmarshal(p, &wantEntry); err != nil {
			t.Fatalf("payload %d decode want entry: %v", i, err)
		}
		if !reflect.DeepEqual(gotEntry, wantEntry) {
			t.Errorf("payload %d entry round-trip mismatch:\n got=%v\nwant=%v", i, gotEntry, wantEntry)
		}
		// Returned slice must alias the caller's buf (caller owns lifecycle).
		if &got[0] != &buf.Bytes()[0] {
			t.Errorf("payload %d: returned slice does not alias caller buf", i)
		}
	}
}

// TestMarshalRecordInto_StableOutput pins that repeated marshals of the
// same record produce byte-identical output — the pooled encoder must not
// drift (e.g. carry escape-mode or buffer state) between calls.
func TestMarshalRecordInto_StableOutput(t *testing.T) {
	p := []byte(`{"time":1700000002000,"uuid":"bb00","type":"assistant","summary":"a<b>c&d"}`)
	var first bytes.Buffer
	want, err := MarshalRecordInto(&first, NewEntry(5, p))
	if err != nil {
		t.Fatalf("first MarshalRecordInto: %v", err)
	}
	wantCopy := append([]byte(nil), want...)
	for i := 0; i < 8; i++ {
		var b bytes.Buffer
		got, err := MarshalRecordInto(&b, NewEntry(5, p))
		if err != nil {
			t.Fatalf("iter %d MarshalRecordInto: %v", i, err)
		}
		if !bytes.Equal(got, wantCopy) {
			t.Fatalf("iter %d output drifted:\n got=%q\nwant=%q", i, got, wantCopy)
		}
	}
}

// TestMarshalRecordInto_PooledEncoderReuse drives many calls through the
// shared recordEncPool to prove the pooled encoder's scratch buffer is
// reset between records — a missed Reset would prepend a prior record's
// bytes to the next one.
func TestMarshalRecordInto_PooledEncoderReuse(t *testing.T) {
	const payload = `{"time":1700000000000,"uuid":"cc00","type":"user","summary":"reuse"}`
	for i := 0; i < 64; i++ {
		var buf bytes.Buffer
		got, err := MarshalRecordInto(&buf, NewEntry(uint64(i+1), []byte(payload)))
		if err != nil {
			t.Fatalf("iter %d MarshalRecordInto: %v", i, err)
		}
		rec, err := UnmarshalRecord(got)
		if err != nil {
			t.Fatalf("iter %d UnmarshalRecord: %v", i, err)
		}
		// payload has no HTML-escapable chars, so raw bytes match verbatim.
		if rec.Seq != uint64(i+1) || string(rec.Entry) != payload {
			t.Fatalf("iter %d pooled-encoder reuse corrupted output: seq=%d entry=%q",
				i, rec.Seq, rec.Entry)
		}
	}
}

// TestMarshalRecordInto_AppendsToCallerBuf verifies the body is appended
// after any pre-existing content (startLen semantics) and the returned
// slice is exactly the appended region.
func TestMarshalRecordInto_AppendsToCallerBuf(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("PREFIX")
	r := NewEntry(7, []byte(`{"time":1700000003000,"uuid":"dd00","type":"user","summary":"x"}`))
	body, err := MarshalRecordInto(&buf, r)
	if err != nil {
		t.Fatalf("MarshalRecordInto: %v", err)
	}
	if got := buf.String(); got[:6] != "PREFIX" {
		t.Errorf("prefix clobbered: %q", got)
	}
	if !bytes.Equal(body, buf.Bytes()[6:]) {
		t.Errorf("returned slice is not the appended region")
	}
	// And the appended region must be a valid record.
	if _, err := UnmarshalRecord(body); err != nil {
		t.Errorf("appended body not a valid record: %v", err)
	}
}
