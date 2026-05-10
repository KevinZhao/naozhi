package persist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// fixture writes a header + N entries into a <key>.log / .idx pair
// using the same framing / idx codec the Persister will use. The
// helper returns the final log/idx paths so tests can mutate them to
// simulate crash states.
func fixture(t *testing.T, dir string, nEntries int) (logPath, idxPath string) {
	t.Helper()
	key := "fixture:" + t.Name()
	stem := KeyHash(key)
	logPath = filepath.Join(dir, stem+".log")
	idxPath = filepath.Join(dir, stem+".idx")

	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	iw, err := NewIdxWriter(idxPath, 0o600)
	if err != nil {
		lf.Close()
		t.Fatalf("open idx: %v", err)
	}

	// Header at seq=0.
	hdr := schema.NewHeader(key, 1700000000000, "test")
	hdrBody, _ := schema.MarshalRecord(hdr)
	n, err := WriteRecordRaw(lf, hdrBody)
	if err != nil {
		t.Fatalf("write header: %v", err)
	}
	iw.Append(schema.IdxEntry{
		Seq: 0, ByteOff: 0, Len: int32(n), TimeMS: hdr.Header.CreatedAt,
	})
	offset := n

	// N entry records.
	for i := 1; i <= nEntries; i++ {
		payload := []byte(`{"time":` + itoa(int64(1700000000000+i)) +
			`,"uuid":"uuid-` + itoa(int64(i)) + `","type":"user","summary":"m"}`)
		rec := schema.NewEntry(uint64(i), payload)
		body, _ := schema.MarshalRecord(rec)
		nWrote, err := WriteRecordRaw(lf, body)
		if err != nil {
			t.Fatalf("write entry %d: %v", i, err)
		}
		iw.Append(schema.IdxEntry{
			Seq: uint64(i), ByteOff: offset, Len: int32(nWrote),
			TimeMS: int64(1700000000000 + i),
		})
		offset += nWrote
	}

	if err := lf.Sync(); err != nil {
		t.Fatalf("sync log: %v", err)
	}
	lf.Close()
	iw.Sync()
	iw.Close()
	return logPath, idxPath
}

// TestRecover_FreshInstall returns NextSeq=1 when neither file exists.
func TestRecover_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	res, err := Recover(
		filepath.Join(dir, "ghost.log"),
		filepath.Join(dir, "ghost.idx"),
	)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.NextSeq != 1 {
		t.Errorf("NextSeq=%d, want 1", res.NextSeq)
	}
	if res.Repaired {
		t.Errorf("Repaired=true on fresh install")
	}
}

// TestRecover_CleanPair: a log+idx in perfect alignment returns the
// next seq without repair.
func TestRecover_CleanPair(t *testing.T) {
	dir := t.TempDir()
	logPath, idxPath := fixture(t, dir, 3)

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Repaired {
		t.Errorf("Repaired=true on clean pair")
	}
	// 3 entries + 1 header → next seq is 4.
	if res.NextSeq != 4 {
		t.Errorf("NextSeq=%d, want 4", res.NextSeq)
	}
	if !res.HeaderValid {
		t.Errorf("HeaderValid=false on committed file")
	}
}

// TestRecover_IdxTailTorn: last idx entry was written half-way (bytes
// not a multiple of 28). The partial bytes must be dropped, reducing
// effective idx length.
func TestRecover_IdxTailTorn(t *testing.T) {
	dir := t.TempDir()
	logPath, idxPath := fixture(t, dir, 3)

	// Append 5 garbage bytes to idx tail.
	f, _ := os.OpenFile(idxPath, os.O_APPEND|os.O_WRONLY, 0o600)
	f.Write([]byte{1, 2, 3, 4, 5})
	f.Close()

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !res.Repaired {
		t.Errorf("Repaired=false despite torn idx tail")
	}
	// Aligned back down — idx effectively describes 3 entries + header.
	if res.NextSeq != 4 {
		t.Errorf("NextSeq=%d, want 4", res.NextSeq)
	}
	// Post-recovery idx must be an exact multiple of 28.
	fi, _ := os.Stat(idxPath)
	if fi.Size()%schema.IdxEntrySize != 0 {
		t.Errorf("idx size %d not aligned to %d", fi.Size(), schema.IdxEntrySize)
	}
}

// TestRecover_LogTailUnbacked: log has bytes past the idx's last
// edge (writer's log.Sync completed but idx.Sync didn't). Recovery
// must ftruncate the log down to the idx edge.
func TestRecover_LogTailUnbacked(t *testing.T) {
	dir := t.TempDir()
	logPath, idxPath := fixture(t, dir, 3)

	// Append 100 garbage bytes to log.
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	garbage := make([]byte, 100)
	f.Write(garbage)
	f.Close()

	resultBefore, _ := os.Stat(logPath)
	originalSize := resultBefore.Size()

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !res.Repaired {
		t.Errorf("Repaired=false despite unbacked log tail")
	}
	if res.NextSeq != 4 {
		t.Errorf("NextSeq=%d, want 4", res.NextSeq)
	}
	fi, _ := os.Stat(logPath)
	if fi.Size() >= originalSize {
		t.Errorf("log still %d bytes, expected truncation below %d",
			fi.Size(), originalSize)
	}
}

// TestRecover_IdxAheadOfLog: idx entries point past log end (the
// pathological "write-order broke" case that RFC §3.2.4 is designed
// to prevent). Recovery must back off idx to the first entry that
// fits.
func TestRecover_IdxAheadOfLog(t *testing.T) {
	dir := t.TempDir()
	logPath, idxPath := fixture(t, dir, 5)

	// Truncate log to cut off the last 2 entries. Idx still references
	// them → idx-ahead.
	logSizeBefore, _ := os.Stat(logPath)
	// Find the ByteOff of seq=4 by reading idx.
	idx, _ := ReadAllIdx(idxPath)
	if len(idx) != 6 { // header + 5 entries
		t.Fatalf("fixture created %d idx entries, want 6", len(idx))
	}
	cutEdge := idx[4].ByteOff + int64(idx[4].Len) // keep seq 0..3, cut 4..5
	_ = logSizeBefore
	truncateFile(logPath, cutEdge)

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !res.Repaired {
		t.Errorf("Repaired=false despite idx-ahead")
	}
	// After reconcile, idx should have 5 entries (header + 1-3 + 4),
	// next seq = 5.
	if res.NextSeq != 5 {
		t.Errorf("NextSeq=%d, want 5", res.NextSeq)
	}
	newIdx, _ := ReadAllIdx(idxPath)
	if len(newIdx) != 5 {
		t.Errorf("post-recovery idx has %d entries, want 5", len(newIdx))
	}
	// Log size must equal the idx's new last entry edge.
	last := newIdx[len(newIdx)-1]
	logFI, _ := os.Stat(logPath)
	if logFI.Size() != last.ByteOff+int64(last.Len) {
		t.Errorf("log size=%d, idx edge=%d", logFI.Size(),
			last.ByteOff+int64(last.Len))
	}
}

// TestRecover_IdxExistsButLogMissing: operator rm'd the log. Idx
// alone is meaningless; recovery clears idx so next startup is clean.
func TestRecover_IdxExistsButLogMissing(t *testing.T) {
	dir := t.TempDir()
	logPath, idxPath := fixture(t, dir, 2)
	os.Remove(logPath)

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !res.Repaired {
		t.Errorf("Repaired=false despite missing log")
	}
	if res.NextSeq != 1 {
		t.Errorf("NextSeq=%d, want 1 (reset)", res.NextSeq)
	}
	if _, err := os.Stat(idxPath); !os.IsNotExist(err) {
		t.Errorf("idx should have been cleared, err=%v", err)
	}
}

// TestRecover_LogExistsButIdxMissing: the first record's write
// window — log has bytes but idx never received its first write.
// Log must be truncated to 0 (no safe edge to anchor).
func TestRecover_LogExistsButIdxMissing(t *testing.T) {
	dir := t.TempDir()
	logPath, idxPath := fixture(t, dir, 2)
	os.Remove(idxPath)

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !res.Repaired {
		t.Errorf("Repaired=false despite missing idx")
	}
	if res.NextSeq != 1 {
		t.Errorf("NextSeq=%d, want 1", res.NextSeq)
	}
	fi, _ := os.Stat(logPath)
	if fi.Size() != 0 {
		t.Errorf("log not truncated: size=%d", fi.Size())
	}
}

// TestRecover_IdxAheadOfLog_AllEntriesDiscarded: extreme case where
// every idx entry points beyond the log (log truncated to 0). We
// must wipe both files rather than try to salvage.
func TestRecover_IdxAheadOfLog_AllEntriesDiscarded(t *testing.T) {
	dir := t.TempDir()
	logPath, idxPath := fixture(t, dir, 3)

	// Truncate log to 0 but leave idx intact.
	truncateFile(logPath, 0)

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !res.Repaired {
		t.Errorf("Repaired=false despite total discard scenario")
	}
	if res.NextSeq != 1 {
		t.Errorf("NextSeq=%d, want 1", res.NextSeq)
	}
	idx, _ := ReadAllIdx(idxPath)
	if len(idx) != 0 {
		t.Errorf("idx should be empty, got %d entries", len(idx))
	}
}

// TestSweepOrphans removes .tmp.* files from the events dir.
func TestSweepOrphans(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, KeyHash("live")+".log")
	os.WriteFile(keep, []byte("committed"), 0o600)

	orphans := []string{
		KeyHash("rot1") + ".tmp.1700000000.log",
		KeyHash("rot2") + ".tmp.1700000001.idx",
	}
	for _, o := range orphans {
		os.WriteFile(filepath.Join(dir, o), []byte("stage"), 0o600)
	}
	// And a non-naozhi file that must NOT be touched.
	readme := filepath.Join(dir, "README.md")
	os.WriteFile(readme, []byte("hi"), 0o600)

	removed, err := SweepOrphans(dir)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("committed file was removed: %v", err)
	}
	if _, err := os.Stat(readme); err != nil {
		t.Errorf("README.md was removed: %v", err)
	}
	for _, o := range orphans {
		if _, err := os.Stat(filepath.Join(dir, o)); !os.IsNotExist(err) {
			t.Errorf("%s not removed (err=%v)", o, err)
		}
	}
	_ = strings.TrimSpace
}

// TestSweepOrphans_MissingDir is a no-op (fresh install).
func TestSweepOrphans_MissingDir(t *testing.T) {
	n, err := SweepOrphans(filepath.Join(t.TempDir(), "ghost"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if n != 0 {
		t.Errorf("removed=%d from nonexistent dir", n)
	}
}
