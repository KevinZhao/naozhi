package sandboxstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/agentcore"
)

// writeEventLog stages <root>/sandboxevents/<jobID>/<runID>.ndjson.
func writeEventLog(t *testing.T, st Store, jobID, runID, body string) {
	t.Helper()
	dir := st.Subtree("sandboxevents", jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runID+".ndjson"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fillEventsSem takes every slot of the process-wide read gate and gives them
// back on cleanup, so the saturation does not leak into sibling tests.
func fillEventsSem(t *testing.T) {
	t.Helper()
	for i := 0; i < eventsSemCap; i++ {
		eventsSem <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < eventsSemCap; i++ {
			<-eventsSem
		}
	})
}

// TestRunEvents_BusyWhenSemSaturated verifies the concurrency gate
// [R20260613-SEC-5 / #2066]: when all eventsSemCap slots are held, a further
// read fails fast with ErrEventsBusy instead of allocating another scanner
// buffer.
func TestRunEvents_BusyWhenSemSaturated(t *testing.T) {
	st := newTestStore(t)
	jobID, runID := hexID(t), hexID(t)
	writeEventLog(t, st, jobID, runID, "{\"kind\":\"boot\"}\n")
	fillEventsSem(t)

	if _, _, err := st.RunEvents(jobID, runID, 10); !errors.Is(err, ErrEventsBusy) {
		t.Fatalf("saturated sem: err = %v, want ErrEventsBusy", err)
	}
}

// TestRunEvents_SemIsProcessWide: the gate bounds the host's memory, so it is
// one gate for the process, not one per Store. Two Stores over different roots
// — two schedulers — are both refused once the shared slots are taken.
func TestRunEvents_SemIsProcessWide(t *testing.T) {
	a, b := newTestStore(t), newTestStore(t)
	jobID, runID := hexID(t), hexID(t)
	writeEventLog(t, a, jobID, runID, "{\"kind\":\"boot\"}\n")
	writeEventLog(t, b, jobID, runID, "{\"kind\":\"boot\"}\n")
	fillEventsSem(t)

	for name, st := range map[string]Store{"a": a, "b": b} {
		if _, _, err := st.RunEvents(jobID, runID, 10); !errors.Is(err, ErrEventsBusy) {
			t.Errorf("store %s: err = %v, want ErrEventsBusy from the shared gate", name, err)
		}
	}
}

// TestRunEvents_ReleasesSemOnReturn verifies the semaphore slot is freed once a
// read completes, so back-to-back reads (the common case) all succeed rather
// than the gate latching after the first.
func TestRunEvents_ReleasesSemOnReturn(t *testing.T) {
	st := newTestStore(t)
	jobID, runID := hexID(t), hexID(t)
	writeEventLog(t, st, jobID, runID, "{\"kind\":\"boot\"}\n")

	for i := 0; i < eventsSemCap+2; i++ {
		if _, _, err := st.RunEvents(jobID, runID, 10); err != nil {
			t.Fatalf("read %d: unexpected err %v (slot not released?)", i, err)
		}
	}
}

// TestEventsCap_SharesAgentcoreCeiling pins the single-source-of-truth
// invariant (#2083): the reader's line cap must equal the agentcore SSE
// decoder's accept ceiling. If a future edit forks one end, this fails before
// the silent-drop bug can recur.
func TestEventsCap_SharesAgentcoreCeiling(t *testing.T) {
	if EventsMaxLineSize != agentcore.MaxEnvelopeLineBytes {
		t.Fatalf("reader cap %d != agentcore wire ceiling %d — the two ends have drifted (#2083)",
			EventsMaxLineSize, agentcore.MaxEnvelopeLineBytes)
	}
}
