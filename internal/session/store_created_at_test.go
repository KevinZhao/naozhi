package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSaveLoadStore_CreatedAtRoundTrip pins the persistence contract: a
// session's createdAt nano timestamp survives save → load → restore through
// the storeEntry JSON encoding, so sidebar order does not reshuffle on
// naozhi restart.
func TestSaveLoadStore_CreatedAtRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	stamp := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	s := newSessionWithID("feishu:direct:alice:general", "sess-111")
	s.createdAt.Store(stamp)
	s.lastActive.Store(stamp + int64(time.Hour))

	if err := saveStore(path, map[string]*ManagedSession{s.key: s}); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	restored := loadStore(path)
	if got := restored[s.key].CreatedAt; got != stamp {
		t.Errorf("CreatedAt round-trip: got %d want %d", got, stamp)
	}
}

// TestLoadStore_LegacyKeepsCreatedAtZero locks the upgrade-boot fallback:
// a sessions.json written by an older naozhi (no created_at field) loads
// successfully and the storeEntry's CreatedAt stays zero. The router's
// restore path then falls back to LastActive.
func TestLoadStore_LegacyKeepsCreatedAtZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	legacy := []map[string]any{{
		"key":         "feishu:direct:bob:general",
		"session_id":  "sess-legacy",
		"last_active": int64(1234567890000000000),
	}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy store: %v", err)
	}
	restored := loadStore(path)
	entry := restored["feishu:direct:bob:general"]
	if entry == nil {
		t.Fatalf("legacy session missing from restored store")
	}
	if entry.CreatedAt != 0 {
		t.Errorf("legacy CreatedAt = %d, want 0 (fallback applied at router restore)", entry.CreatedAt)
	}
	if entry.LastActive != 1234567890000000000 {
		t.Errorf("legacy LastActive = %d, lost during load", entry.LastActive)
	}
}

// TestSnapshot_IncludesCreatedAtMillis pins the wire-format contract: the
// dashboard payload (SessionSnapshot.CreatedAt) is in unix ms, mirroring
// LastActive's unit so the JS sort comparator can mix them in the legacy
// fallback branch.
func TestSnapshot_IncludesCreatedAtMillis(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := &ManagedSession{key: "feishu:direct:alice:general"}
	s.createdAt.Store(stamp.UnixNano())
	snap := s.Snapshot()
	if got := snap.CreatedAt; got != stamp.UnixMilli() {
		t.Errorf("snapshot CreatedAt = %d, want %d (unix ms)", got, stamp.UnixMilli())
	}
}

// TestInitCreatedAtIfUnset_Idempotent locks the helper's contract: a non-zero
// existing value is left alone so spawn / Rename paths that pre-populate
// createdAt do not get clobbered.
func TestInitCreatedAtIfUnset_Idempotent(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.createdAt.Store(42)
	s.initCreatedAtIfUnset()
	if got := s.createdAt.Load(); got != 42 {
		t.Errorf("init clobbered preset value: got %d want 42", got)
	}

	s2 := &ManagedSession{key: "k2"}
	before := time.Now().UnixNano()
	s2.initCreatedAtIfUnset()
	got := s2.createdAt.Load()
	if got < before {
		t.Errorf("init stamped value too small: got %d, time.Now nano was >= %d", got, before)
	}
}
