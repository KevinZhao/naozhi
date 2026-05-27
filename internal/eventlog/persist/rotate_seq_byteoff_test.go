package persist

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestSpliceLog_PreservesSeqViaByteOffCorrelation pins R217-GO-4 (#603):
// spliceLog correlates source records to idx entries via ByteOff (no
// per-record schema.UnmarshalRecord just to read Seq). The test exercises
// rotate end-to-end and verifies that, after rotate completes, every idx
// entry's Seq matches the Seq embedded inside the framed record at the
// indexed ByteOff. If a future refactor reverts to body-decode-for-seq with
// a bug, or if the ByteOff correlation drifts (off-by-one frame), the
// indexed Seq would no longer line up with the framed body and this test
// would fail.
func TestSpliceLog_PreservesSeqViaByteOffCorrelation(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) {
		// Tight cap → at least one rotate; small stride → dense idx
		// → many idx-entry/record correlations to verify.
		o.MaxFileBytes = 8 * 1024
		o.IdxStride = 2
	})
	sink := p.SinkFor("k")

	mkEntry := func(i int) Entry {
		payload := map[string]any{
			"time":   int64(1700000000000 + i),
			"uuid":   fmt.Sprintf("u%d", i),
			"type":   "user",
			"detail": fmt.Sprintf("payload %d: %s", i, strings.Repeat("x", 512)),
		}
		buf, _ := json.Marshal(payload)
		return Entry{JSON: buf, TimeMS: int64(1700000000000 + i)}
	}

	for i := 0; i < DefaultKeepRecords+50; i++ {
		sink([]Entry{mkEntry(i)}, false)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	logPath := LogPath(dir, "k")
	idxPath := filepath.Join(dir, KeyHash("k")+".idx")

	// Read idx entries from disk.
	idxBytes, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read idx file: %v", err)
	}
	if len(idxBytes)%schema.IdxEntrySize != 0 {
		t.Fatalf("idx file size %d not a multiple of IdxEntrySize=%d",
			len(idxBytes), schema.IdxEntrySize)
	}
	n := len(idxBytes) / schema.IdxEntrySize
	idxEntries := make([]schema.IdxEntry, 0, n)
	for i := 0; i < n; i++ {
		e, err := schema.UnmarshalIdxEntry(idxBytes[i*schema.IdxEntrySize:])
		if err != nil {
			t.Fatalf("decode idx entry %d: %v", i, err)
		}
		idxEntries = append(idxEntries, e)
	}
	if len(idxEntries) < 2 {
		t.Fatalf("idx has %d entries, expected at least 2 (header + tail records)",
			len(idxEntries))
	}

	// Header invariants.
	if idxEntries[0].ByteOff != 0 || idxEntries[0].Seq != 0 {
		t.Errorf("idx[0] should be header (ByteOff=0, Seq=0), got ByteOff=%d Seq=%d",
			idxEntries[0].ByteOff, idxEntries[0].Seq)
	}

	// Build a ByteOff → Seq map by scanning the on-disk log frame-by-frame.
	body2Seq := byteOffToSeq(t, logPath)

	// For every non-header idx entry, the indexed ByteOff must point at a
	// framed body whose decoded Seq matches the idx entry's Seq.
	var prevSeq uint64
	for i, e := range idxEntries[1:] {
		gotSeq, ok := body2Seq[e.ByteOff]
		if !ok {
			t.Errorf("idx[%d] ByteOff=%d does not match any record-frame start",
				i+1, e.ByteOff)
			continue
		}
		if gotSeq != e.Seq {
			t.Errorf("idx[%d] Seq=%d but body at ByteOff=%d has Seq=%d (correlation broken)",
				i+1, e.Seq, e.ByteOff, gotSeq)
		}
		if i > 0 && e.Seq <= prevSeq {
			t.Errorf("idx[%d] Seq=%d not strictly greater than prev=%d (sparse-but-ordered violated)",
				i+1, e.Seq, prevSeq)
		}
		prevSeq = e.Seq
	}
}

// byteOffToSeq scans the on-disk log and returns the byte offset of each
// framed record (the start of its length prefix) → its decoded Seq.
// Header records have Seq=0.
func byteOffToSeq(t *testing.T, path string) map[int64]uint64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	out := make(map[int64]uint64)
	var off int64
	for off < int64(len(data)) {
		// Length prefix is decimal digits ending in '\n'.
		nl := indexByte(data[off:], '\n')
		if nl < 0 {
			break
		}
		digits := data[off : off+int64(nl)]
		var n int64
		for _, c := range digits {
			if c < '0' || c > '9' {
				t.Fatalf("non-digit in length prefix at offset %d", off)
			}
			n = n*10 + int64(c-'0')
		}
		bodyStart := off + int64(nl) + 1
		bodyEnd := bodyStart + n
		if bodyEnd+1 > int64(len(data)) {
			break
		}
		body := data[bodyStart:bodyEnd]
		rec, err := schema.UnmarshalRecord(body)
		if err != nil {
			t.Fatalf("decode body at offset %d: %v", off, err)
		}
		out[off] = rec.Seq
		// Advance: total frame = digits + '\n' + body + '\n'.
		off = bodyEnd + 1
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
