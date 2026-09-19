package persist

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestGapRecord_DroppedBatchLeavesDurableMark is #2664's core claim: after a
// channel-full drop, the NEXT batch that gets through carries a gap record in
// front, so a reader of events/<key> can tell "no messages in this span" from
// "a batch was dropped here". Before this the only evidence was a process-
// memory counter (reset on restart) and one Warn line (log retention).
func TestGapRecord_DroppedBatchLeavesDurableMark(t *testing.T) {
	t.Parallel()
	p, dir := newTestPersister(t)
	const key = "feishu:p2p:gap-test"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}

	// Simulate the drop bookkeeping the channel-full path performs (driving a
	// real overflow would race the drain loop; the pendingGap contract is what
	// the durable mark depends on and is what this pins).
	s.pendingGap.Add(37)

	s.accept([]Entry{
		{TimeMS: 1700000000000, JSON: []byte(`{"time":1700000000000,"type":"text","summary":"after the gap"}`)},
	}, false)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	recs := readAllRecords(t, LogPath(dir, key))
	var gapIdx, textIdx = -1, -1
	var gapBody []byte
	for i, r := range recs {
		if len(r.Entry) == 0 {
			continue
		}
		if bytes.Contains(r.Entry, []byte(gapEntryType)) {
			gapIdx, gapBody = i, r.Entry
		}
		if bytes.Contains(r.Entry, []byte("after the gap")) {
			textIdx = i
		}
	}
	if gapIdx < 0 {
		t.Fatal("no gap record on disk after a drop; the reader cannot see the loss")
	}
	if textIdx < 0 || gapIdx > textIdx {
		t.Fatalf("gap record at %d must precede the batch that carried it (text at %d)", gapIdx, textIdx)
	}
	var gap clievent.EventEntry
	if err := json.Unmarshal(gapBody, &gap); err != nil {
		t.Fatalf("gap record does not parse as an EventEntry: %v\n%s", err, gapBody)
	}
	if gap.Type != gapEntryType {
		t.Errorf("Type = %q", gap.Type)
	}
	if !strings.Contains(gap.Detail, "dropped=37") {
		t.Errorf("Detail = %q, want the machine-readable count", gap.Detail)
	}
	if gap.Time != 1700000000000 {
		t.Errorf("Time = %d, want the carrying batch's first timestamp — the gap spans up to that instant", gap.Time)
	}
	// And the counter is consumed: a second batch must NOT repeat the gap.
	s.accept([]Entry{
		{TimeMS: 1700000001000, JSON: []byte(`{"time":1700000001000,"type":"text","summary":"later"}`)},
	}, false)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("Flush 2: %v", err)
	}
	count := 0
	for _, r := range readAllRecords(t, LogPath(dir, key)) {
		if len(r.Entry) > 0 && bytes.Contains(r.Entry, []byte(gapEntryType)) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("gap records = %d, want exactly 1 — the mark must not repeat once written", count)
	}
}

// TestGapRecord_DropDuringCarryRestoresCount: if the batch carrying the gap is
// itself dropped, the swapped-out count must ride back (plus the new batch) —
// otherwise the tally vanishes and the eventual gap record under-reports.
func TestGapRecord_DropDuringCarryRestoresCount(t *testing.T) {
	t.Parallel()
	// ChannelBuffer 0 is not allowed; use 1 and pre-fill it so accept's
	// non-blocking send loses.
	p, _ := newTestPersister(t, func(o *Options) { o.ChannelBuffer = 1 })
	const key = "feishu:p2p:gap-refill"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}

	// Stop the persister's drain by stuffing the channel from outside.
	// (Racing the run loop is unreliable; owning the channel state is not.)
	blocker := batchJob{Key: "other", Stem: KeyHash("other")}
	select {
	case p.in <- blocker:
	default:
		t.Skip("drain won the race before the blocker landed; channel-state ownership not available")
	}

	s.pendingGap.Add(5)
	s.accept([]Entry{{TimeMS: 1, JSON: []byte(`{"time":1,"type":"text"}`)}}, false)

	if got := s.pendingGap.Load(); got != 6 {
		t.Fatalf("pendingGap after drop-during-carry = %d, want 6 (5 restored + 1 new)", got)
	}
}

// TestGapEntryShape_MatchesEventEntry pins the hand-built JSON to the real
// EventEntry tags: persist deliberately does not import clievent in
// production code, so the linkage lives here, in the test, where the import
// is free — a renamed tag on either side fails this before it silently
// produces gap records nobody can parse.
func TestGapEntryShape_MatchesEventEntry(t *testing.T) {
	t.Parallel()
	src := gapEntryJSON{Time: 42, Type: gapEntryType, Summary: "s", Detail: "d"}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var round clievent.EventEntry
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.Time != 42 || round.Type != gapEntryType || round.Summary != "s" || round.Detail != "d" {
		t.Errorf("round-trip lost fields: %+v", round)
	}
}
