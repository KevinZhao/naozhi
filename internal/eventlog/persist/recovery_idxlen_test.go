package persist

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// writeRawIdx writes entries verbatim, bypassing the writer, so a test
// can plant the bit-flipped Len / ByteOff values that a hardware fault
// or journal-replay surprise would leave on disk.
func writeRawIdx(t *testing.T, path string, entries []schema.IdxEntry) {
	t.Helper()
	buf := make([]byte, 0, len(entries)*schema.IdxEntrySize)
	var one [schema.IdxEntrySize]byte
	for _, e := range entries {
		buf = append(buf, schema.MarshalIdxEntry(one[:], e)...)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write idx: %v", err)
	}
}

// planLog writes a log file of the given size and an idx describing a
// header record plus one data record, then corrupts the LAST entry's
// Len to the supplied value.
func planCorruptPair(t *testing.T, logSize int64, corruptLen int32) (logPath, idxPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "test.log")
	idxPath = filepath.Join(dir, "test.idx")
	if err := os.WriteFile(logPath, make([]byte, logSize), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	writeRawIdx(t, idxPath, []schema.IdxEntry{
		{Seq: 0, ByteOff: 0, Len: 100, TimeMS: 1},
		{Seq: 1, ByteOff: 100, Len: corruptLen, TimeMS: 2},
	})
	return logPath, idxPath
}

// TestRecover_NegativeIdxLen_DoesNotTruncateBelowByteOff guards against
// silent data loss from a bit-flipped idx Len.
//
// schema.UnmarshalIdxEntry decodes Len with a bare uint32->int32 cast,
// so a high-bit flip yields a negative Len. Recovery derives the log
// truncation edge from ByteOff+Len, and a Len of -1 puts that edge one
// byte BELOW ByteOff — a plausible-looking offset in the middle of the
// previous record. Before the idxEntrySane guard, Recover truncated the
// log there and returned success, silently slicing a committed record
// in half.
//
// Correct behaviour: back off to the newest SANE idx entry (the header
// at ByteOff=0, Len=100) and truncate to that edge, so no committed
// record is left half-written.
func TestRecover_NegativeIdxLen_DoesNotTruncateBelowByteOff(t *testing.T) {
	logPath, idxPath := planCorruptPair(t, 300, -1)

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The corrupt entry must not have been used as the edge. ByteOff-1
	// (=99) is the value the unguarded code produced.
	if res.LogSize == 99 {
		t.Fatalf("Recover used corrupt entry as edge: LogSize=99 (ByteOff-1); "+
			"expected fallback to the last sane entry, got %+v", res)
	}
	// Header entry is the newest sane one: edge = 0 + 100.
	if res.LogSize != 100 {
		t.Errorf("LogSize = %d; want 100 (header edge)", res.LogSize)
	}
	if res.NextSeq != 1 {
		t.Errorf("NextSeq = %d; want 1 (after header seq=0)", res.NextSeq)
	}
	if !res.Repaired {
		t.Error("Repaired = false; want true so alerting surfaces the corruption")
	}

	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if fi.Size() != res.LogSize {
		t.Errorf("on-disk log size = %d; want %d (must match reported LogSize)",
			fi.Size(), res.LogSize)
	}
}

// TestRecover_MinInt32IdxLen_NoEINVAL guards the second failure mode:
// Len=math.MinInt32 makes ByteOff+Len negative, so ftruncate(2) returns
// EINVAL and Recover previously failed outright with "truncate log:
// invalid argument" — a message that never names the real cause, and
// which prevents the Persister for that session from starting at all.
func TestRecover_MinInt32IdxLen_NoEINVAL(t *testing.T) {
	logPath, idxPath := planCorruptPair(t, 300, math.MinInt32)

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover failed on corrupt idx Len (regression: negative edge "+
			"reached ftruncate): %v", err)
	}
	if res.LogSize != 100 {
		t.Errorf("LogSize = %d; want 100 (header edge)", res.LogSize)
	}
	if !res.Repaired {
		t.Error("Repaired = false; want true")
	}
}

// TestRecover_AllIdxEntriesCorrupt_WipesBoth covers the case where no
// entry is trustworthy. Recovery must fail safe by wiping to a clean
// slate rather than truncating to a garbage offset or erroring out.
func TestRecover_AllIdxEntriesCorrupt_WipesBoth(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	idxPath := filepath.Join(dir, "test.idx")
	if err := os.WriteFile(logPath, make([]byte, 300), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	writeRawIdx(t, idxPath, []schema.IdxEntry{
		{Seq: 0, ByteOff: -8, Len: 100, TimeMS: 1},
		{Seq: 1, ByteOff: 100, Len: -1, TimeMS: 2},
	})

	res, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.NextSeq != 1 || res.LogSize != 0 {
		t.Errorf("got NextSeq=%d LogSize=%d; want NextSeq=1 LogSize=0 (clean slate)",
			res.NextSeq, res.LogSize)
	}
	if !res.Repaired {
		t.Error("Repaired = false; want true")
	}
}

// TestIdxEntrySane_Bounds pins the guard's accept/reject boundaries so a
// future change cannot silently widen them.
func TestIdxEntrySane_Bounds(t *testing.T) {
	tests := []struct {
		name string
		e    schema.IdxEntry
		want bool
	}{
		{"header", schema.IdxEntry{Seq: 0, ByteOff: 0, Len: 120}, true},
		{"normal", schema.IdxEntry{Seq: 7, ByteOff: 4096, Len: 512}, true},
		{"zero len", schema.IdxEntry{Seq: 1, ByteOff: 10, Len: 0}, true},
		{"max framed len", schema.IdxEntry{Seq: 1, ByteOff: 0, Len: int32(maxIdxEntryLen)}, true},
		{"len one past max", schema.IdxEntry{Seq: 1, ByteOff: 0, Len: int32(maxIdxEntryLen) + 1}, false},
		{"negative len", schema.IdxEntry{Seq: 1, ByteOff: 100, Len: -1}, false},
		{"min int32 len", schema.IdxEntry{Seq: 1, ByteOff: 100, Len: math.MinInt32}, false},
		{"negative byteoff", schema.IdxEntry{Seq: 1, ByteOff: -1, Len: 10}, false},
		{"byteoff overflows edge", schema.IdxEntry{Seq: 1, ByteOff: math.MaxInt64, Len: 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := idxEntrySane(tc.e); got != tc.want {
				t.Errorf("idxEntrySane(%+v) = %v; want %v", tc.e, got, tc.want)
			}
		})
	}
}
