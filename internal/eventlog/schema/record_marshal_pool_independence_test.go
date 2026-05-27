package schema

import (
	"bytes"
	"testing"
)

// TestMarshalRecord_ResultIsPoolIndependent pins R249-PERF-22 (#996)
// verify-stale: the trailing `out := make + copy` at the end of
// MarshalRecord is NOT a missed optimisation — it is the contract that
// keeps the returned bytes safe to retain past the call. The pooled
// bytes.Buffer's backing array gets re-filed via marshalRecordBufPool
// on the defer; if MarshalRecord returned the buf-owned slice directly,
// the next caller's Reset() + Encode would scribble into the byte range
// a prior caller still holds, corrupting whatever it had written to
// disk or wire.
//
// Future "optimisations" that try to skip the copy must pair with
// either (a) eliminating the pool entirely or (b) handing the caller
// the buffer pointer so they own the lifecycle (the existing
// MarshalRecordInto API does the latter). This test makes such a
// regression fail loudly: it allocates a record, marshals it, then
// forces several more MarshalRecord calls to drive pool churn. If we
// truly returned fresh bytes, the first slice keeps its original
// content; if some future edit re-aliases pool bytes into the return
// value, the first slice will see the second marshal's payload.
func TestMarshalRecord_ResultIsPoolIndependent(t *testing.T) {
	first := []byte(`{"time":1700000001000,"uuid":"aa00","type":"user","summary":"first"}`)
	r1 := NewEntry(1, first)
	body1, err := MarshalRecord(r1)
	if err != nil {
		t.Fatalf("first MarshalRecord: %v", err)
	}
	want := append([]byte(nil), body1...)

	// Force several more marshals so the pool is exercised. Each one
	// would Reset() + re-Encode into the same backing array if the
	// pool rotated buffers and we had returned the alias.
	for i := 0; i < 5; i++ {
		payload := []byte(`{"time":1700000002000,"uuid":"bb00","type":"assistant","summary":"second-payload-please-overwrite-the-leaked-array"}`)
		r2 := NewEntry(uint64(i+2), payload)
		if _, err := MarshalRecord(r2); err != nil {
			t.Fatalf("repeat MarshalRecord %d: %v", i, err)
		}
	}

	// body1 must still hold its original bytes verbatim — pool churn
	// must not have corrupted it.
	if !bytes.Equal(body1, want) {
		t.Fatalf("body1 was corrupted by subsequent MarshalRecord calls — pool isolation broke. got=%q want=%q",
			body1, want)
	}
}
