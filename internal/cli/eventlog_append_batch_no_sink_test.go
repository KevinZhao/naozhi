package cli

// R103901-PERF-7 regression-lock: the !captureForSink AppendBatch path
// (no persist sink wired — test harnesses, headless tools, the InjectHistory
// replay phase before the persister attaches) now preprocesses each entry
// (default Time + image sanitize) in the pre-lock loop instead of inside
// l.mu. These tests pin the observable behaviour so the move stays
// behaviour-equivalent: default Time application, UUID stamping, image
// sanitize, insertion order, and the caller-slice mutation contract.

import (
	"reflect"
	"testing"
)

// TestAppendBatch_NoSink_DefaultTimeApplied verifies that zero-Time entries
// get the single pre-lock wall-clock default while explicit-Time entries are
// left untouched — on the !captureForSink path (no sink wired).
func TestAppendBatch_NoSink_DefaultTimeApplied(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{
		{Type: "user", Summary: "no-time"},         // Time==0 → defaulted
		{Type: "text", Time: 12345, Summary: "ts"}, // explicit Time preserved
		{Type: "user", Summary: "no-time-2"},       // Time==0 → defaulted
	})

	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if entries[0].Time == 0 {
		t.Errorf("entries[0].Time = 0, want a defaulted non-zero wall-clock value")
	}
	if entries[1].Time != 12345 {
		t.Errorf("entries[1].Time = %d, want 12345 (explicit Time must be preserved)", entries[1].Time)
	}
	if entries[2].Time == 0 {
		t.Errorf("entries[2].Time = 0, want a defaulted non-zero wall-clock value")
	}
	// Both zero-Time entries share the single pre-lock wall-clock read.
	if entries[0].Time != entries[2].Time {
		t.Errorf("defaulted Times differ: %d vs %d — both should use the single pre-lock now()", entries[0].Time, entries[2].Time)
	}
	// Insertion order preserved.
	if entries[0].Summary != "no-time" || entries[1].Summary != "ts" || entries[2].Summary != "no-time-2" {
		t.Errorf("insertion order broken: %q, %q, %q", entries[0].Summary, entries[1].Summary, entries[2].Summary)
	}
}

// TestAppendBatch_NoSink_UUIDStamped verifies UUID stamping on the
// !captureForSink path: empty UUIDs get a fresh value, caller-set UUIDs
// (history replay determinism) are preserved.
func TestAppendBatch_NoSink_UUIDStamped(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{
		{Type: "user", Summary: "a"},                     // empty UUID → stamped
		{Type: "user", UUID: "fixed-uuid", Summary: "b"}, // preserved
	})
	entries := l.Entries()
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].UUID == "" {
		t.Errorf("entries[0].UUID is empty, want a freshly stamped value")
	}
	if entries[1].UUID != "fixed-uuid" {
		t.Errorf("entries[1].UUID = %q, want %q (caller-set UUID must be preserved)", entries[1].UUID, "fixed-uuid")
	}
}

// TestAppendBatch_NoSink_ImagesSanitized mirrors the image-sanitize
// enforcement specifically for the !captureForSink path: invalid data URIs
// are stripped before the entry lands in the ring buffer.
func TestAppendBatch_NoSink_ImagesSanitized(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.AppendBatch([]EventEntry{
		{
			Type: "user",
			Images: []string{
				"data:image/png;base64,A",
				"javascript:alert(1)",
				"data:image/webp;base64,B",
			},
		},
	})
	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	want := []string{"data:image/png;base64,A", "data:image/webp;base64,B"}
	if !reflect.DeepEqual(entries[0].Images, want) {
		t.Errorf("Images = %v, want %v", entries[0].Images, want)
	}
}

// TestAppendBatch_NoSink_CallerSliceNotMutated pins the contract that the
// pre-lock preprocessing buffer (rather than in-place mutation of the
// caller's slice) leaves the caller's Time/Images fields untouched. Only the
// UUID is stamped in place, matching the historical behaviour of the
// stamp-inside-lock implementation (stampUUID always wrote through &entries[i]).
func TestAppendBatch_NoSink_CallerSliceNotMutated(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	in := []EventEntry{
		{
			Type: "user",
			Images: []string{
				"data:image/png;base64,A",
				"javascript:alert(1)",
			},
		},
	}
	l.AppendBatch(in)

	// Time was 0 on input; the default must NOT have been written back into
	// the caller's slice.
	if in[0].Time != 0 {
		t.Errorf("caller slice Time mutated to %d, want 0 (default must only land in the ring copy)", in[0].Time)
	}
	// Images sanitize must NOT have been written back into the caller's slice.
	wantImgs := []string{"data:image/png;base64,A", "javascript:alert(1)"}
	if !reflect.DeepEqual(in[0].Images, wantImgs) {
		t.Errorf("caller slice Images mutated to %v, want %v (sanitize must only affect the ring copy)", in[0].Images, wantImgs)
	}
	// UUID stamping IS done in place (unchanged behaviour).
	if in[0].UUID == "" {
		t.Errorf("caller slice UUID not stamped; stampUUID must still write through &entries[i]")
	}
}
