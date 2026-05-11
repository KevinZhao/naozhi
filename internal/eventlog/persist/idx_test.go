package persist

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestIdxWriter_AppendAndReadBack is the round-trip for the per-entry
// API the Persister uses during normal operation.
func TestIdxWriter_AppendAndReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.idx")

	w, err := NewIdxWriter(path, 0o600)
	if err != nil {
		t.Fatalf("NewIdxWriter: %v", err)
	}
	entries := []schema.IdxEntry{
		{Seq: 0, ByteOff: 0, Len: 50, TimeMS: 1},
		{Seq: 1, ByteOff: 50, Len: 120, TimeMS: 2},
		{Seq: 2, ByteOff: 170, Len: 80, TimeMS: 3},
	}
	for _, e := range entries {
		if err := w.Append(e); err != nil {
			t.Fatalf("Append %+v: %v", e, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := ReadAllIdx(path)
	if err != nil {
		t.Fatalf("ReadAllIdx: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("got %d entries, want %d", len(got), len(entries))
	}
	for i, e := range entries {
		if got[i] != e {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], e)
		}
	}
}

// TestIdxWriter_AppendBatch exercises the rotate-path bulk writer
// (999 extra syscalls saved during reindex).
func TestIdxWriter_AppendBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.idx")

	w, err := NewIdxWriter(path, 0o600)
	if err != nil {
		t.Fatalf("NewIdxWriter: %v", err)
	}
	batch := make([]schema.IdxEntry, 10)
	for i := range batch {
		batch[i] = schema.IdxEntry{
			Seq:     uint64(i),
			ByteOff: int64(i * 100),
			Len:     100,
			TimeMS:  int64(1700000000000 + i),
		}
	}
	if err := w.AppendBatch(batch); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	w.Sync()
	w.Close()

	got, err := ReadAllIdx(path)
	if err != nil {
		t.Fatalf("ReadAllIdx: %v", err)
	}
	if len(got) != len(batch) {
		t.Fatalf("got %d entries, want %d", len(got), len(batch))
	}
	for i, e := range batch {
		if got[i] != e {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], e)
		}
	}
}

// TestReadAllIdx_MissingFile returns empty (not error) so recovery
// can treat "never persisted" and "persisted empty" symmetrically
// without a special case branch.
func TestReadAllIdx_MissingFile(t *testing.T) {
	got, err := ReadAllIdx(filepath.Join(t.TempDir(), "ghost.idx"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries for missing file, want 0", len(got))
	}
}

// TestReadAllIdx_PartialTrailingEntry covers the torn-write tail: a
// writer crashed mid-Append, leaving N*28 + k bytes (0 < k < 28). The
// reader MUST drop the k bogus bytes, not attempt to decode them.
func TestReadAllIdx_PartialTrailingEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "torn.idx")

	// Write 2 good entries, then 5 garbage bytes.
	w, err := NewIdxWriter(path, 0o600)
	if err != nil {
		t.Fatalf("NewIdxWriter: %v", err)
	}
	w.Append(schema.IdxEntry{Seq: 0, ByteOff: 0, Len: 50, TimeMS: 1})
	w.Append(schema.IdxEntry{Seq: 1, ByteOff: 50, Len: 80, TimeMS: 2})
	w.Sync()
	w.Close()

	// Manually append torn bytes.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	f.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	f.Close()

	got, err := ReadAllIdx(path)
	if err != nil {
		t.Fatalf("ReadAllIdx should tolerate torn tail: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d entries, want 2 (torn tail dropped)", len(got))
	}
}

// TestLastIdxEntry_Empty returns (zero, false, nil) for a missing or
// empty file. Used by recovery's "nothing to align against" branch.
func TestLastIdxEntry_Empty(t *testing.T) {
	_, ok, err := LastIdxEntry(filepath.Join(t.TempDir(), "ghost.idx"))
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if ok {
		t.Errorf("ok=true for missing file")
	}

	// Empty file also returns false.
	empty := filepath.Join(t.TempDir(), "empty.idx")
	os.WriteFile(empty, nil, 0o600)
	_, ok, err = LastIdxEntry(empty)
	if err != nil {
		t.Fatalf("empty file: %v", err)
	}
	if ok {
		t.Errorf("ok=true for empty file")
	}
}

// TestLastIdxEntry_HappyPath avoids a full ReadAllIdx — the recovery
// path calls this on every startup so it matters that it touches only
// the last 28 bytes.
func TestLastIdxEntry_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.idx")
	w, _ := NewIdxWriter(path, 0o600)
	for i := 0; i < 5; i++ {
		w.Append(schema.IdxEntry{
			Seq: uint64(i), ByteOff: int64(i * 100),
			Len: 100, TimeMS: int64(1700000000000 + i),
		})
	}
	w.Sync()
	w.Close()

	last, ok, err := LastIdxEntry(path)
	if err != nil {
		t.Fatalf("LastIdxEntry: %v", err)
	}
	if !ok {
		t.Fatalf("ok=false on populated file")
	}
	if last.Seq != 4 {
		t.Errorf("last.Seq=%d, want 4", last.Seq)
	}
}

// TestLastIdxEntry_TornTail confirms the partial-entry at tail is
// ignored — LastIdxEntry must return the LAST COMPLETE entry, not
// a half-decoded tail.
func TestLastIdxEntry_TornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.idx")
	w, _ := NewIdxWriter(path, 0o600)
	w.Append(schema.IdxEntry{Seq: 0, ByteOff: 0, Len: 50, TimeMS: 1})
	w.Append(schema.IdxEntry{Seq: 1, ByteOff: 50, Len: 80, TimeMS: 2})
	w.Sync()
	w.Close()

	// Append partial bytes for a would-be seq=2 entry.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.Write(make([]byte, 10))
	f.Close()

	last, ok, err := LastIdxEntry(path)
	if err != nil {
		t.Fatalf("LastIdxEntry: %v", err)
	}
	if !ok {
		t.Fatalf("ok=false despite 2 complete entries")
	}
	if last.Seq != 1 {
		t.Errorf("last.Seq=%d, want 1 (torn tail ignored)", last.Seq)
	}
}

// TestIdxWriter_Truncate exercises the recovery path where idx has
// entries pointing past log end and must be cut.
func TestIdxWriter_Truncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.idx")
	w, _ := NewIdxWriter(path, 0o600)
	for i := 0; i < 5; i++ {
		w.Append(schema.IdxEntry{Seq: uint64(i), ByteOff: int64(i * 100), Len: 100, TimeMS: int64(i + 1)})
	}
	w.Sync()

	// Cut to just 2 entries (56 bytes).
	if err := w.Truncate(2 * schema.IdxEntrySize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	// Append should resume right at EOF, not at a stale offset.
	if err := w.Append(schema.IdxEntry{Seq: 99, ByteOff: 9999, Len: 42, TimeMS: 42}); err != nil {
		t.Fatalf("Append post-truncate: %v", err)
	}
	w.Sync()
	w.Close()

	got, _ := ReadAllIdx(path)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (2 kept + 1 appended)", len(got))
	}
	if got[0].Seq != 0 || got[1].Seq != 1 || got[2].Seq != 99 {
		t.Errorf("unexpected sequence: %v", got)
	}
}

// TestIdxWriter_Size tracks file size accurately across appends —
// Persister uses this to decide idx stride alignment after rotate.
func TestIdxWriter_Size(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.idx")
	w, _ := NewIdxWriter(path, 0o600)
	defer w.Close()

	for i := 0; i < 3; i++ {
		w.Append(schema.IdxEntry{Seq: uint64(i)})
	}
	w.Sync()

	got, err := w.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if got != 3*schema.IdxEntrySize {
		t.Errorf("Size=%d, want %d", got, 3*schema.IdxEntrySize)
	}
}
