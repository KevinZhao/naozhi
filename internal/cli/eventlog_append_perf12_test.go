package cli

import "testing"

// TestAppendBatch_CallerSliceContract_PERF12 locks the contract preserved by
// the single-copy pre-lock loop (R20260602190132-PERF-12): AppendBatch stamps
// the caller's slice in place with a UUID, but must NOT mutate any other field
// (default Time, image sanitization) on the caller's entries — those defaults
// apply only to the stored ring copy / persist-sink copy.
func TestAppendBatch_CallerSliceContract_PERF12(t *testing.T) {
	l := NewEventLog(16)

	caller := []EventEntry{
		{Type: "user", Summary: "a"}, // Time == 0 (no caller-set time)
		{Type: "text", Summary: "b", Time: 1234},
	}
	l.AppendBatch(caller)

	// Contract 1: UUID stamped in place on the caller slice.
	for i := range caller {
		if caller[i].UUID == "" {
			t.Errorf("caller[%d].UUID empty; stampUUID must stamp in place", i)
		}
	}
	// Contract 2: caller's zero Time stays zero — the default-Time fill
	// happens on the stored copy, not the caller slice.
	if caller[0].Time != 0 {
		t.Errorf("caller[0].Time mutated to %d; want untouched 0", caller[0].Time)
	}
	if caller[1].Time != 1234 {
		t.Errorf("caller[1].Time = %d; want preserved 1234", caller[1].Time)
	}

	// The stored ring entries get the default Time applied (non-zero) and
	// preserve the UUID.
	stored := l.Entries()
	if len(stored) != 2 {
		t.Fatalf("stored len=%d, want 2", len(stored))
	}
	if stored[0].Time == 0 {
		t.Errorf("stored[0].Time == 0; default Time should be applied to the copy")
	}
	if stored[0].UUID != caller[0].UUID {
		t.Errorf("stored[0].UUID=%q != caller[0].UUID=%q", stored[0].UUID, caller[0].UUID)
	}
	if stored[1].Time != 1234 {
		t.Errorf("stored[1].Time=%d; want preserved caller time 1234", stored[1].Time)
	}
}

// TestAppendBatch_SinkPath_CallerSliceContract_PERF12 repeats the contract
// check on the captureForSink branch (a persist sink attached + ready), which
// uses the separate sinkCopy buffer.
func TestAppendBatch_SinkPath_CallerSliceContract_PERF12(t *testing.T) {
	l := NewEventLog(16)
	batch := &captureSink{}
	l.SetPersistSink(batch.asSink())

	caller := []EventEntry{{Type: "user", Summary: "x"}}
	l.AppendBatch(caller)

	if caller[0].UUID == "" {
		t.Errorf("caller[0].UUID empty; stampUUID must run on sink path")
	}
	if caller[0].Time != 0 {
		t.Errorf("caller[0].Time mutated to %d on sink path; want 0", caller[0].Time)
	}

	got, _, ok := batch.lastBatch()
	if !ok || len(got) != 1 {
		t.Fatalf("sink batch missing or wrong len: ok=%v len=%d", ok, len(got))
	}
	if got[0].Time == 0 {
		t.Errorf("sink copy Time == 0; default Time should be applied to the sink copy")
	}
	if got[0].UUID != caller[0].UUID {
		t.Errorf("sink UUID=%q != caller UUID=%q", got[0].UUID, caller[0].UUID)
	}
}
