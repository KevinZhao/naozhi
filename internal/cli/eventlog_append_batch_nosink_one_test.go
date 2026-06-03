package cli

// R20260603-PERF-4 regression-lock: AppendBatch !captureForSink len==1 fast
// path uses a stack scalar (preparedOne) instead of make([]EventEntry,1).
//
// Tests verify that the scalar path is behaviourally identical to the
// multi-entry slice path: UUID stamping, default-Time application, image
// sanitize, ring-buffer write, and caller-slice immutability contract all hold.

import (
	"reflect"
	"testing"
)

// TestAppendBatch_NoSink_Len1_DefaultTimeApplied verifies that a zero-Time
// single entry gets a non-zero default on the preparedOne scalar path.
func TestAppendBatch_NoSink_Len1_DefaultTimeApplied(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{{Type: "user", Summary: "single"}})

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].Time == 0 {
		t.Errorf("entries[0].Time = 0, want a non-zero defaulted value on the len==1 no-sink path")
	}
}

// TestAppendBatch_NoSink_Len1_UUIDStamped verifies stampUUID still writes
// through &entries[i] (in-place on caller slice) on the scalar path.
func TestAppendBatch_NoSink_Len1_UUIDStamped(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	in := []EventEntry{{Type: "user", Summary: "stamp"}}
	l.AppendBatch(in)

	if in[0].UUID == "" {
		t.Errorf("caller slice UUID not stamped on len==1 no-sink path")
	}
	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].UUID != in[0].UUID {
		t.Errorf("ring UUID %q != caller UUID %q", entries[0].UUID, in[0].UUID)
	}
}

// TestAppendBatch_NoSink_Len1_ImagesSanitized verifies that invalid data URIs
// are stripped on the preparedOne scalar path.
func TestAppendBatch_NoSink_Len1_ImagesSanitized(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{{
		Type: "user",
		Images: []string{
			"data:image/png;base64,A",
			"javascript:evil()",
			"data:image/webp;base64,B",
		},
	}})

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	want := []string{"data:image/png;base64,A", "data:image/webp;base64,B"}
	if !reflect.DeepEqual(entries[0].Images, want) {
		t.Errorf("Images = %v, want %v", entries[0].Images, want)
	}
}

// TestAppendBatch_NoSink_Len1_CallerSliceNotMutated verifies that neither Time
// nor Images are written back into the caller's slice on the scalar path.
// Only UUID stamping (in-place, historical contract) is allowed.
func TestAppendBatch_NoSink_Len1_CallerSliceNotMutated(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	in := []EventEntry{{
		Type: "user",
		Images: []string{
			"data:image/png;base64,A",
			"javascript:evil()",
		},
	}}
	l.AppendBatch(in)

	if in[0].Time != 0 {
		t.Errorf("caller slice Time mutated to %d on len==1 no-sink path, want 0", in[0].Time)
	}
	wantImgs := []string{"data:image/png;base64,A", "javascript:evil()"}
	if !reflect.DeepEqual(in[0].Images, wantImgs) {
		t.Errorf("caller slice Images mutated to %v, want %v", in[0].Images, wantImgs)
	}
	// UUID must be stamped in place.
	if in[0].UUID == "" {
		t.Errorf("caller slice UUID not stamped; stampUUID must still run on the len==1 no-sink path")
	}
}

// TestAppendBatch_NoSink_Len1_EntryInRingBuffer confirms the entry is written
// to the ring buffer even when the scalar fast path is active.
func TestAppendBatch_NoSink_Len1_EntryInRingBuffer(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{{Type: "user", Summary: "ring-scalar"}})

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("ring buffer has %d entries, want 1", len(entries))
	}
	if entries[0].Summary != "ring-scalar" {
		t.Errorf("ring entry Summary = %q, want %q", entries[0].Summary, "ring-scalar")
	}
}

// TestAppendBatch_NoSink_Len1_ExplicitTimePreserved verifies that an entry
// with a non-zero Time is not overwritten by the default on the scalar path.
func TestAppendBatch_NoSink_Len1_ExplicitTimePreserved(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{{Type: "user", Summary: "ts", Time: 99999}})

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].Time != 99999 {
		t.Errorf("Time = %d, want 99999 (explicit Time must be preserved)", entries[0].Time)
	}
}

// TestAppendBatch_NoSink_Len1_VsLen2_Parity verifies that the ring output of a
// single-entry AppendBatch (scalar path) matches the output of a two-entry call
// that exercises the slice path, for the same entry content.
func TestAppendBatch_NoSink_Len1_VsLen2_Parity(t *testing.T) {
	t.Parallel()

	l1 := NewEventLog(8)
	l1.AppendBatch([]EventEntry{{Type: "user", Summary: "parity", Time: 12345}})
	e1 := l1.Entries()

	l2 := NewEventLog(8)
	l2.AppendBatch([]EventEntry{
		{Type: "user", Summary: "parity", Time: 12345},
		{Type: "user", Summary: "extra", Time: 12346},
	})
	e2 := l2.Entries()

	if len(e1) != 1 {
		t.Fatalf("scalar path: %d entries, want 1", len(e1))
	}
	if len(e2) != 2 {
		t.Fatalf("slice path: %d entries, want 2", len(e2))
	}
	// First entry must be identical in both ring buffers (excluding UUID which
	// is independently stamped).
	if e1[0].Summary != e2[0].Summary {
		t.Errorf("Summary mismatch: scalar=%q slice=%q", e1[0].Summary, e2[0].Summary)
	}
	if e1[0].Time != e2[0].Time {
		t.Errorf("Time mismatch: scalar=%d slice=%d", e1[0].Time, e2[0].Time)
	}
}
