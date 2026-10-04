package persist

import (
	"encoding/json"
	"fmt"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// gap.go — the persist_gap record. Events dropped on the way to disk are
// tallied per key until a gap record in front of the key's next stored batch
// marks the span, so a reader of events/<key> can tell "no messages here" from
// "events were lost here" and history's GapFill knows where to look.
//
// Drops in accept (channel full) are counted on the sink and handed over with
// the next batch that gets through (batchJob.gapN). Drops on the run goroutine
// go into Persister.runGap. A written gap record moves its tally onto the
// writer until a flush makes it durable; retireWriter hands it back.

// gapEntryJSON is the persisted gap record's body: EventEntry-shaped JSON built
// by hand because persist deliberately does not import clievent. Field names
// must match clievent.EventEntry's json tags — the linkage is pinned by
// TestGapEntryShape_MatchesEventEntry.
//
// It has no uuid on purpose: the record shares Time with the batch it fronts,
// and an empty UUID sorts first under merged's (Time, UUID) order, keeping the
// record ahead of its batch and local sorted for mergeDedup's fast path.
// Readers exempt it from missing-UUID checks by Type.
type gapEntryJSON struct {
	Time    int64  `json:"time"`
	Type    string `json:"type"`
	Summary string `json:"summary,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// gapEntryType is the EventEntry.Type of a persistence-gap record, owned by
// schema so readers share it. Additive: consumers that do not know it render
// the summary text through their default branch (event_render.js eventHtml).
const gapEntryType = schema.GapEntryType

// gapCause is why the events in a tally were lost; one tally may mix both.
type gapCause uint8

const (
	// gapChannelFull: a queue was full — the ingest channel, or the FIFO of
	// a stem whose files are being removed.
	gapChannelFull gapCause = 1 << iota
	// gapWriteFailed: opening, writing or flushing the key's files failed.
	gapWriteFailed
)

// gapTally counts lost events not yet marked on disk.
type gapTally struct {
	n     int64
	cause gapCause
}

func (t *gapTally) add(o gapTally) {
	if o.n <= 0 {
		return
	}
	t.n += o.n
	t.cause |= o.cause
}

// holdGap adds t to key's tally for its next gap record. Run-goroutine only.
func (p *Persister) holdGap(key string, t gapTally) {
	if t.n <= 0 {
		return
	}
	cur := p.runGap[key]
	cur.add(t)
	p.runGap[key] = cur
}

// noteRunDrop counts n events the run goroutine dropped for key and holds
// them for key's next gap record.
func (p *Persister) noteRunDrop(key string, n int, cause gapCause) {
	if n <= 0 {
		return
	}
	p.droppedCnt.Add(int64(n))
	p.opts.Observer.OnDrop(n)
	p.holdGap(key, gapTally{n: int64(n), cause: cause})
}

// gapRecordJSON builds the gap record for t, stamped timeMS: the time of the
// first entry it fronts, so the gap reads as "between the previous stored
// record and this one".
func gapRecordJSON(t gapTally, timeMS int64) ([]byte, error) {
	reason, what := "persist_channel_full", "持久化过载"
	switch t.cause {
	case gapWriteFailed:
		reason, what = "persist_write_failed", "写盘失败"
	case gapChannelFull | gapWriteFailed:
		reason, what = "persist_channel_full+persist_write_failed", "持久化过载与写盘失败"
	}
	return json.Marshal(gapEntryJSON{
		Time:    timeMS,
		Type:    gapEntryType,
		Summary: fmt.Sprintf("事件缺口：%s，此前丢弃 %d 条事件", what, t.n),
		Detail:  fmt.Sprintf("dropped=%d reason=%s", t.n, reason),
	})
}
