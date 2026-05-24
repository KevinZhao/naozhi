package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetiredStore_MarkAndGet(t *testing.T) {
	rs, err := NewRetiredStore("")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	now := time.UnixMilli(1700000000000)
	rs.MarkRetired("sid-1", now)
	if got := rs.Get("sid-1"); got != now.UnixMilli() {
		t.Fatalf("get: want %d, got %d", now.UnixMilli(), got)
	}
	if got := rs.Get("missing"); got != 0 {
		t.Fatalf("missing: want 0, got %d", got)
	}
	if got := rs.Get(""); got != 0 {
		t.Fatalf("empty id: want 0, got %d", got)
	}
}

func TestRetiredStore_MarkRetiredEmptyIDIgnored(t *testing.T) {
	rs, _ := NewRetiredStore("")
	rs.MarkRetired("", time.Now())
	if rs.Len() != 0 {
		t.Fatalf("empty id should not be stored, got len=%d", rs.Len())
	}
}

func TestRetiredStore_MonotonicTimestamp(t *testing.T) {
	rs, _ := NewRetiredStore("")
	t1 := time.UnixMilli(1700000000000)
	t2 := time.UnixMilli(1700000010000)
	rs.MarkRetired("sid-1", t2)
	rs.MarkRetired("sid-1", t1) // older — should not overwrite
	if got := rs.Get("sid-1"); got != t2.UnixMilli() {
		t.Fatalf("monotonic: want %d, got %d", t2.UnixMilli(), got)
	}
	rs.MarkRetired("sid-1", t2.Add(time.Second))
	if got := rs.Get("sid-1"); got != t2.Add(time.Second).UnixMilli() {
		t.Fatalf("newer should overwrite, got %d", got)
	}
}

func TestRetiredStore_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retired.json")

	rs1, err := NewRetiredStore(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	rs1.MarkRetired("sid-a", time.UnixMilli(1700000001000))
	rs1.MarkRetired("sid-b", time.UnixMilli(1700000002000))
	if err := rs1.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File should exist on disk.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat saved file: %v", err)
	}
	// Re-open and assert state survived.
	rs2, err := NewRetiredStore(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if got := rs2.Get("sid-a"); got != 1700000001000 {
		t.Fatalf("a: got %d", got)
	}
	if got := rs2.Get("sid-b"); got != 1700000002000 {
		t.Fatalf("b: got %d", got)
	}
}

func TestRetiredStore_SaveNoOpWhenClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retired.json")
	rs, _ := NewRetiredStore(path)
	if err := rs.Save(); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("clean save should not create file")
	}
}

func TestRetiredStore_LoadCorruptIsTolerated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retired.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	rs, err := NewRetiredStore(path)
	if err == nil {
		t.Fatalf("expected parse error to be returned")
	}
	if rs == nil {
		t.Fatalf("store should still be usable after load error")
	}
	rs.MarkRetired("fresh", time.UnixMilli(1700000000000))
	if rs.Get("fresh") == 0 {
		t.Fatalf("store should accept new marks after load failure")
	}
}

func TestRetiredStore_Prune_OldEntries(t *testing.T) {
	rs, _ := NewRetiredStore("")
	rs.MarkRetired("old", time.UnixMilli(1000))
	rs.MarkRetired("recent", time.UnixMilli(5000))
	removed := rs.Prune(3000)
	if removed != 1 {
		t.Fatalf("prune removed %d, want 1", removed)
	}
	if rs.Get("old") != 0 || rs.Get("recent") == 0 {
		t.Fatalf("wrong entries pruned")
	}
}

func TestRetiredStore_Prune_CapTrim(t *testing.T) {
	rs, err := NewRetiredStoreWithCap("", 3)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	rs.MarkRetired("a", time.UnixMilli(1000))
	rs.MarkRetired("b", time.UnixMilli(2000))
	rs.MarkRetired("c", time.UnixMilli(3000))
	rs.MarkRetired("d", time.UnixMilli(4000))
	rs.MarkRetired("e", time.UnixMilli(5000))
	removed := rs.Prune(0) // no time cutoff; only cap trim
	if removed != 2 {
		t.Fatalf("cap trim removed %d, want 2", removed)
	}
	if rs.Len() != 3 {
		t.Fatalf("len after trim = %d, want 3", rs.Len())
	}
	if rs.Get("a") != 0 || rs.Get("b") != 0 {
		t.Fatalf("oldest entries should be trimmed")
	}
	if rs.Get("c") == 0 || rs.Get("d") == 0 || rs.Get("e") == 0 {
		t.Fatalf("newest 3 should survive")
	}
}

func TestRetiredStore_FileSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retired.json")
	rs, _ := NewRetiredStore(path)
	rs.MarkRetired("sid-1", time.UnixMilli(1700000000000))
	if err := rs.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var file retiredStoreFileV1
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if file.Version != retiredStoreVersion {
		t.Fatalf("version: got %d, want %d", file.Version, retiredStoreVersion)
	}
	if file.Entries["sid-1"] != 1700000000000 {
		t.Fatalf("entry round-trip: %v", file.Entries)
	}
}

func TestRetiredStore_Snapshot(t *testing.T) {
	rs, _ := NewRetiredStore("")
	rs.MarkRetired("a", time.UnixMilli(1))
	rs.MarkRetired("b", time.UnixMilli(2))
	snap := rs.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len: %d", len(snap))
	}
	// Mutating the snapshot must not affect the store.
	snap["a"] = 999
	if rs.Get("a") != 1 {
		t.Fatalf("snapshot should be a copy")
	}
}
