package persist

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestRotate_InterruptedBetweenRenames: rotate renames tmp.log, then tmp.idx.
// When only one of the two lands (a crash between the syscalls, or a power
// loss that keeps one), the next start must still see a log every record of
// which decodes, with seqs continuing past the newest one, rather than a
// mixed old/new pair cut mid-frame.
func TestRotate_InterruptedBetweenRenames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		landLog bool // which rename landed
	}{
		{"log renamed, idx not", true},
		{"idx renamed, log not", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const total = DefaultKeepRecords + 400
			writeRecords(t, dir, "k", total)

			logPath := LogPath(dir, "k")
			stem := KeyHash("k")
			idxPath := filepath.Join(dir, stem+idxExt)

			// Stage exactly as rotate does.
			idxEntries, err := ReadAllIdx(idxPath)
			if err != nil {
				t.Fatal(err)
			}
			cutIdx := chooseCutIndex(idxEntries, DefaultKeepRecords, 4)
			if cutIdx <= 0 {
				t.Fatalf("nothing to cut: %d idx entries", len(idxEntries))
			}
			tmpLog := tmpLogPath(dir, stem, 1)
			tmpIdx := tmpIdxPath(dir, stem, 1)
			if _, newIdx, err := spliceLog(logPath, tmpLog, idxEntries, cutIdx); err != nil {
				t.Fatal(err)
			} else if err := writeIdxFile(tmpIdx, newIdx); err != nil {
				t.Fatal(err)
			}
			if tc.landLog {
				err = os.Rename(tmpLog, logPath)
			} else {
				err = os.Rename(tmpIdx, idxPath)
			}
			if err != nil {
				t.Fatal(err)
			}

			// Restart: the sweep runs at NewPersister, Recover when the
			// key's writer opens.
			if _, err := SweepOrphans(dir); err != nil {
				t.Fatal(err)
			}
			res, err := Recover(logPath, idxPath)
			if err != nil {
				t.Fatal(err)
			}
			if res.NextSeq != total+1 {
				t.Errorf("NextSeq = %d, want %d (seqs reused or newest records dropped)", res.NextSeq, total+1)
			}
			firstSeq, lastSeq, n := scanAllFrames(t, logPath)
			if lastSeq != total {
				t.Errorf("last decodable seq = %d, want %d", lastSeq, total)
			}
			if n == 0 || uint64(n) != lastSeq-firstSeq+1 {
				t.Errorf("%d records decode for seqs %d..%d, want a contiguous run", n, firstSeq, lastSeq)
			}
			if fi, err := os.Stat(logPath); err != nil || fi.Size() != res.LogSize {
				t.Errorf("log size %v (err %v), Recover reported %d", fi.Size(), err, res.LogSize)
			}
		})
	}
}

// writeRecords persists n records for key under dir and stops the Persister,
// leaving a consistent log/idx pair with IdxStride 4.
func writeRecords(t *testing.T, dir, key string, n int) {
	t.Helper()
	p, err := NewPersister(Options{
		Dir:            dir,
		MaxFileBytes:   64 * 1024 * 1024,
		IdxStride:      4,
		FlushInterval:  20 * time.Millisecond,
		IdleCloseAfter: time.Minute,
		ChannelBuffer:  128,
		Generator:      "naozhi-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := p.SinkFor(key)
	const batchSize = 50
	for start := 0; start < n; start += batchSize {
		batch := make([]Entry, 0, batchSize)
		for i := start; i < start+batchSize && i < n; i++ {
			batch = append(batch, entry(t, int64(1700000000000+i), "u"+itoa(int64(i))))
		}
		sink(batch, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if st := p.Stats(); st.Written != int64(n) {
		t.Fatalf("written %d, want %d", st.Written, n)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// scanAllFrames decodes every frame of the log to EOF, failing on a torn or
// undecodable one, and returns the first and last record seq and the record
// count (header excluded).
func scanAllFrames(t *testing.T, path string) (uint64, uint64, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var first, last uint64
	n := -1
	for {
		body, _, err := ReadFramedBody(br)
		if errors.Is(err, io.EOF) {
			return first, last, n
		}
		if err != nil {
			t.Fatalf("frame %d: %v (log cut mid-record)", n+1, err)
		}
		rec, err := schema.UnmarshalRecord(body)
		if err != nil {
			t.Fatalf("frame %d: decode: %v", n+1, err)
		}
		if n == 0 {
			first = rec.Seq
		}
		last = rec.Seq
		n++
	}
}

// TestSweepOrphans_LoneHalfOfFailedRotateIsDeleted: a failed rotate's cleanup
// can stop after removing one staging file while the live pair is still old
// and consistent; the other half must be deleted, not renamed over it.
func TestSweepOrphans_LoneHalfOfFailedRotateIsDeleted(t *testing.T) {
	dir := t.TempDir()
	const total = DefaultKeepRecords + 400
	writeRecords(t, dir, "k", total)
	logPath := LogPath(dir, "k")
	stem := KeyHash("k")
	idxPath := filepath.Join(dir, stem+idxExt)
	idxEntries, err := ReadAllIdx(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	tmpLog := tmpLogPath(dir, stem, 1)
	tmpIdx := tmpIdxPath(dir, stem, 1)
	_, newIdx, err := spliceLog(logPath, tmpLog, idxEntries, chooseCutIndex(idxEntries, DefaultKeepRecords, 4))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIdxFile(tmpIdx, newIdx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tmpLog); err != nil {
		t.Fatal(err)
	}
	if n, err := SweepOrphans(dir); err != nil || n != 1 {
		t.Fatalf("SweepOrphans = %d, %v; want 1 removed", n, err)
	}
	if _, err := os.Stat(tmpIdx); !os.IsNotExist(err) {
		t.Errorf("staging idx left behind: %v", err)
	}
	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.Repaired || res.NextSeq != total+1 {
		t.Errorf("Recover = %+v, want the untouched old pair (NextSeq %d, no repair)", res, total+1)
	}
}
