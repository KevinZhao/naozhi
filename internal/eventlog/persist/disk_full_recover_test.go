package persist

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// A *bufio.Writer latches its first write error, so a writer whose log
// buffer hit ENOSPC once used to stay dead after the disk freed up: every
// later event of the session was dropped and the flush warning repeated on
// each tick, for as long as the process lived. These tests fill the "disk",
// free it again, and require the session's events to land.

// fullDisk is an io.Writer over a log fd that behaves like a full disk while
// full is set: it writes half of each chunk (leaving a torn record on disk,
// as a real short write does) and returns ENOSPC.
type fullDisk struct {
	f    *os.File
	full *atomic.Bool
}

func (d fullDisk) Write(b []byte) (int, error) {
	if !d.full.Load() {
		return d.f.Write(b)
	}
	n, _ := d.f.Write(b[:len(b)/2])
	return n, &os.PathError{Op: "write", Path: d.f.Name(), Err: syscall.ENOSPC}
}

// installFullDisk routes every log fd opened under dir through a fullDisk.
// Tests using it must not call t.Parallel: the hook is package-global.
func installFullDisk(t *testing.T, dir string) *atomic.Bool {
	t.Helper()
	full := new(atomic.Bool)
	logFileWriterHook = func(f *os.File) io.Writer {
		if !strings.HasPrefix(f.Name(), dir+string(filepath.Separator)) {
			return f
		}
		return fullDisk{f: f, full: full}
	}
	t.Cleanup(func() { logFileWriterHook = nil })
	return full
}

// countWarns installs a slog default that counts Warn messages; the
// returned func snapshots the counts. Not for t.Parallel tests.
func countWarns(t *testing.T) func() map[string]int {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(&captureWarnHandler{onWarn: func(msg string) {
		mu.Lock()
		counts[msg]++
		mu.Unlock()
	}}))
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(counts))
		for k, v := range counts {
			out[k] = v
		}
		return out
	}
}

func flushOrFail(t *testing.T, p *Persister) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	return p.Flush(ctx)
}

// assertRecoveredLog stops p and checks the on-disk pair is clean (Recover
// has nothing to repair) and holds exactly wantUUIDs after the header, with
// strictly increasing seqs.
func assertRecoveredLog(t *testing.T, p *Persister, dir, key string, wantUUIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	logPath := LogPath(dir, key)
	idxPath := filepath.Join(dir, KeyHash(key)+idxExt)
	rec, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rec.Repaired {
		t.Errorf("Recover repaired the pair after a clean Stop; the reopened writer left it inconsistent")
	}
	recs := readAllRecords(t, logPath)
	if len(recs) == 0 || recs[0].Seq != 0 || recs[0].Header == nil {
		t.Fatalf("log does not start with a header: %d records", len(recs))
	}
	var got []string
	for i, r := range recs[1:] {
		if r.Seq <= recs[i].Seq {
			t.Errorf("seq %d follows seq %d; seqs must strictly increase", r.Seq, recs[i].Seq)
		}
		got = append(got, string(r.Entry))
	}
	if len(got) != len(wantUUIDs) {
		t.Fatalf("log holds %d entries, want %d (%v)", len(got), len(wantUUIDs), wantUUIDs)
	}
	for i, u := range wantUUIDs {
		if !strings.Contains(got[i], `"`+u+`"`) {
			t.Errorf("entry %d = %s, want uuid %s", i, got[i], u)
		}
	}
}

// TestPersister_ResumesAfterDiskFull_Flush: the disk fills between two
// flushes and then frees up. The flush that hit ENOSPC fails; the next one
// must succeed and the log must hold the events before and after the outage.
func TestPersister_ResumesAfterDiskFull_Flush(t *testing.T) {
	// A debounce window far beyond the test keeps the ticker out of the way,
	// so every flush below is the explicit one.
	p, dir := newTestPersister(t, func(o *Options) { o.FlushInterval = time.Hour })
	full := installFullDisk(t, dir)
	const key = "dashboard:direct:diskfull:general"
	sink := p.SinkFor(key)

	sink([]Entry{entry(t, 1700000001000, "before")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush before the outage: %v", err)
	}

	full.Store(true)
	sink([]Entry{entry(t, 1700000002000, "during")}, false)
	if err := flushOrFail(t, p); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Flush on a full disk: err=%v, want ENOSPC", err)
	}

	full.Store(false)
	sink([]Entry{entry(t, 1700000003000, "after")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush after the disk freed up: %v (the latched bufio error was retried instead of the writer being reopened)", err)
	}
	if got := p.Stats().Dropped; got < 1 {
		t.Errorf("Stats().Dropped = %d, want >= 1 for the event lost to the outage", got)
	}
	assertRecoveredLog(t, p, dir, key, "before", "after")
}

// TestPersister_ResumesAfterDiskFull_MidBatch: a batch bigger than the log
// buffer spills to the fd inside handleBatch, so the ENOSPC surfaces on the
// ingest path rather than at flush time.
func TestPersister_ResumesAfterDiskFull_MidBatch(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) { o.FlushInterval = time.Hour })
	full := installFullDisk(t, dir)
	const key = "dashboard:direct:diskfull-batch:general"
	sink := p.SinkFor(key)

	sink([]Entry{entry(t, 1700000001000, "before")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush before the outage: %v", err)
	}

	warns := countWarns(t)
	full.Store(true)
	big := strings.Repeat("x", logWriteBufSize/2)
	var batch []Entry
	for i := 0; i < 4; i++ {
		batch = append(batch, Entry{
			JSON:   []byte(`{"uuid":"during","summary":"` + big + `"}`),
			TimeMS: 1700000002000 + int64(i),
		})
	}
	sink(batch, false)
	// The ingest path retires the writer at the first failed write, so the
	// rest of the batch is not written into the latched buffer one warning
	// at a time and this flush has nothing left to fail on.
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush after the failed batch: %v, want nil (the writer should already be retired)", err)
	}
	if got := warns(); got["event log persist: write path failed"] != 1 || len(got) != 1 {
		t.Fatalf("warnings during the outage = %v, want exactly one \"write path failed\"", got)
	}

	full.Store(false)
	sink([]Entry{entry(t, 1700000003000, "after")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush after the disk freed up: %v", err)
	}
	if got := p.Stats().Dropped; got < int64(len(batch)) {
		t.Errorf("Stats().Dropped = %d, want >= %d for the batch lost to the outage", got, len(batch))
	}
	assertRecoveredLog(t, p, dir, key, "before", "after")
}

// TestPersister_ResumesAfterDiskFull_Tick: the production path — nobody
// calls Flush, the debounce ticker does — must recover on its own.
func TestPersister_ResumesAfterDiskFull_Tick(t *testing.T) {
	p, dir := newTestPersister(t)
	full := installFullDisk(t, dir)
	const key = "dashboard:direct:diskfull-tick:general"
	sink := p.SinkFor(key)

	sink([]Entry{entry(t, 1700000001000, "before")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush before the outage: %v", err)
	}

	full.Store(true)
	sink([]Entry{entry(t, 1700000002000, "during")}, false)
	testhelper.Eventually(t, func() bool { return p.Stats().Dropped > 0 }, opGuard,
		"the debounce tick never settled the failed flush")

	full.Store(false)
	sink([]Entry{entry(t, 1700000003000, "after")}, false)
	assertRecoveredLog(t, p, dir, key, "before", "after")
}

// TestNoteFailure_Throttles: a disk that stays full must not log every
// failure; one warning per failureWarnEvery carries the suppressed count,
// and a success resets the run.
func TestNoteFailure_Throttles(t *testing.T) {
	now := time.Unix(1700000000, 0)
	p := &Persister{
		opts:    Options{Clock: func() time.Time { return now }},
		failing: make(map[string]*failureState),
	}
	const key = "k"
	err := errors.New("no space left on device")

	p.noteFailure(key, "flush", err)
	for i := 0; i < 9; i++ {
		now = now.Add(100 * time.Millisecond)
		p.noteFailure(key, "flush", err)
	}
	st := p.failing[key]
	if st.failures != 10 || st.suppressed != 9 {
		t.Fatalf("after 10 failures inside one window: failures=%d suppressed=%d, want 10 and 9",
			st.failures, st.suppressed)
	}

	now = now.Add(failureWarnEvery)
	p.noteFailure(key, "flush", err)
	if st.suppressed != 0 || !st.lastWarn.Equal(now) {
		t.Fatalf("a failure past the window must warn and reset the suppressed count: suppressed=%d lastWarn=%v",
			st.suppressed, st.lastWarn)
	}

	p.noteRecovered(key)
	if _, ok := p.failing[key]; ok {
		t.Fatalf("noteRecovered left the key in p.failing")
	}
}

// TestFlush_IdxSyncFailure_Poisons: after an idx fsync failure the entries
// are already appended, so retrying would append them a second time and a
// later Recover could cut the log back to a stale duplicate. The writer must
// be poisoned instead, and Recover over what it left must keep every record
// with an idx whose seqs strictly increase.
func TestFlush_IdxSyncFailure_Poisons(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) { o.IdxStride = 1 })
	const key = "fk-idx-sync"
	sink := p.SinkFor(key)
	sink([]Entry{entry(t, 1700000001000, "before")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The run goroutine is gone, so the test owns a hand-built writer.
	logPath := LogPath(dir, key)
	idxPath := filepath.Join(dir, KeyHash(key)+idxExt)
	rec, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	idxW, err := NewIdxWriter(idxPath, 0o600)
	if err != nil {
		t.Fatalf("open idx: %v", err)
	}
	w := &perKeyWriter{
		key: key, stem: KeyHash(key),
		logFile: logFile, logBuf: acquireLogBuf(logFile), idxWriter: idxW,
		logPath: logPath, idxPath: idxPath,
		nextSeq: rec.NextSeq, bytes: rec.LogSize,
	}
	for i := 0; i < 2; i++ {
		n, err := WriteRecordRaw(w.logBuf, []byte(`{}`))
		if err != nil {
			t.Fatalf("write record: %v", err)
		}
		w.pendingIdx = append(w.pendingIdx, schema.IdxEntry{Seq: w.nextSeq, ByteOff: w.bytes, Len: int32(n)})
		w.bytes += n
		w.nextSeq++
	}
	w.dirty = true

	idxW.syncFailHook = func() error { return errors.New("injected idx fsync EIO") }
	if err := w.flush(p); err == nil {
		t.Fatalf("flush: expected the injected idx fsync error")
	}
	if !w.poisoned {
		t.Fatalf("an idx fsync failure left the writer unpoisoned; a retry would append its entries twice")
	}
	_ = w.close()

	rec, err = Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover after the failure: %v", err)
	}
	if rec.NextSeq != w.nextSeq {
		t.Errorf("Recover NextSeq = %d, want %d: records whose idx entries reached the file were cut", rec.NextSeq, w.nextSeq)
	}
	idx, err := ReadAllIdx(idxPath)
	if err != nil {
		t.Fatalf("ReadAllIdx: %v", err)
	}
	for i := 1; i < len(idx); i++ {
		if idx[i].Seq <= idx[i-1].Seq || idx[i].ByteOff <= idx[i-1].ByteOff {
			t.Fatalf("idx entry %d (seq %d off %d) does not follow entry %d (seq %d off %d)",
				i, idx[i].Seq, idx[i].ByteOff, i-1, idx[i-1].Seq, idx[i-1].ByteOff)
		}
	}
}
