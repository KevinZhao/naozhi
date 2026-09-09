package merged

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// The property this file pins: the local tier can have an INTERIOR HOLE, and a
// turn that exists only in fallback inside that hole must survive the merge.
//
// This is not hypothetical. internal/eventlog/persist drops a whole batch when
// its ingest channel is full (persister.go, `select { case p.in <- job: default:
// … droppedCnt.Add …}`, DefaultChannelBuffer = 4096) — an instantaneous burst
// overrun or a wedged writer. The drop is counted in memory and logged; NOTHING
// is written to the log file. So a reader of events/<key>/ cannot distinguish
// "no messages in this window" from "a batch was dropped here", and the hole
// sits between local records on both sides rather than before the earliest one.
//
// Today's design is safe against this by construction: dedup is decided PER
// FALLBACK ENTRY (a fallback row is dropped only when it finds a specific
// partner), so an unpaired row is always kept. Epic F #2544's proposed
// water-mark split — "once local has any record for a key, trust only local from
// minLocalTime onward" — is decided PER RANGE, which turns conservative
// retention into optimistic deletion and would hide exactly these messages.
// That is the #2406 failure mode ("fixed the display, lost messages") that took
// four review rounds.
//
// The 40 existing tests cover pairing cardinality, skew direction and order
// independence; none constructs a positional gap. See
// docs/rfc/history-merge-watermark.md.

// TestMerged_LocalInteriorGap_FallbackOnlyTurnSurvives is the guard: local has
// turns at t=1000 and t=3000 but dropped the one at t=2000, which fallback still
// carries. All three must come back.
func TestMerged_LocalInteriorGap_FallbackOnlyTurnSurvives(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "local-1", Time: 1000, Type: "user", Detail: "first question"},
			// t=2000 dropped by the persister — no record, no marker.
			{UUID: "local-3", Time: 3000, Type: "user", Detail: "third question"},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "cli-1", Time: 1005, Type: "user", Detail: "first question"},
			{UUID: "cli-2", Time: 2005, Type: "user", Detail: "second question"},
			{UUID: "cli-3", Time: 3005, Type: "user", Detail: "third question"},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	details := map[string]int{}
	for _, e := range got {
		details[e.Detail]++
	}
	for _, want := range []string{"first question", "second question", "third question"} {
		if details[want] == 0 {
			t.Errorf("%q lost: a turn present only in fallback, inside a local gap, must survive\ngot %d entries: %+v", want, len(got), got)
		}
		if details[want] > 1 {
			t.Errorf("%q duplicated %dx: the twins should have deduped", want, details[want])
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d entries, want exactly 3 (one per turn)\n%+v", len(got), got)
	}
}

// TestMerged_LocalGapAtHead_FallbackFillsIt covers the gap being the OLDEST
// window rather than an interior one: local's earliest record is t=3000, so a
// water-mark scheme keyed on minLocalTime would keep these — the case the epic's
// proposal does handle. Here so the two situations are distinguishable when this
// file is read as the specification.
func TestMerged_LocalGapAtHead_FallbackFillsIt(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "local-3", Time: 3000, Type: "user", Detail: "third question"},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "cli-1", Time: 1005, Type: "user", Detail: "first question"},
			{UUID: "cli-2", Time: 2005, Type: "user", Detail: "second question"},
			{UUID: "cli-3", Time: 3005, Type: "user", Detail: "third question"},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
}

// TestMerged_LocalInteriorGap_MultipleDroppedTurns is the burst shape the
// persister actually produces: it drops a whole BATCH, not one entry, so a real
// gap is several consecutive turns wide.
func TestMerged_LocalInteriorGap_MultipleDroppedTurns(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "local-1", Time: 1000, Type: "user", Detail: "q1"},
			// t=2000..4000 — one dropped batch.
			{UUID: "local-5", Time: 5000, Type: "user", Detail: "q5"},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "cli-1", Time: 1005, Type: "user", Detail: "q1"},
			{UUID: "cli-2", Time: 2005, Type: "user", Detail: "q2"},
			{UUID: "cli-3", Time: 3005, Type: "user", Detail: "q3"},
			{UUID: "cli-4", Time: 4005, Type: "user", Detail: "q4"},
			{UUID: "cli-5", Time: 5005, Type: "user", Detail: "q5"},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d entries, want 5 — the dropped batch must be recovered from fallback: %+v", len(got), got)
	}
}
