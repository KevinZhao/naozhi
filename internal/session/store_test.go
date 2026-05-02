package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	sessions := map[string]*ManagedSession{
		"feishu:direct:alice:general": newSessionWithID("feishu:direct:alice:general", "sess-111"),
		"feishu:group:xxx:general":    newSessionWithID("feishu:group:xxx:general", "sess-222"),
		"feishu:direct:bob:general":   {key: "feishu:direct:bob:general"}, // empty session ID should be skipped
	}

	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}

	restored := loadStore(path)
	if len(restored) != 2 {
		t.Fatalf("loadStore() returned %d entries, want 2", len(restored))
	}
	if restored["feishu:direct:alice:general"].SessionID != "sess-111" {
		t.Errorf("alice session = %q, want %q", restored["feishu:direct:alice:general"].SessionID, "sess-111")
	}
	if restored["feishu:group:xxx:general"].SessionID != "sess-222" {
		t.Errorf("group session = %q, want %q", restored["feishu:group:xxx:general"].SessionID, "sess-222")
	}
}

func TestSaveAndLoadUserLabel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	labeled := newSessionWithID("feishu:direct:alice:general", "sess-111")
	labeled.SetUserLabel("重构会话")
	unlabeled := newSessionWithID("feishu:group:xxx:general", "sess-222")

	sessions := map[string]*ManagedSession{
		labeled.key:   labeled,
		unlabeled.key: unlabeled,
	}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}

	restored := loadStore(path)
	if got := restored[labeled.key].UserLabel; got != "重构会话" {
		t.Errorf("labeled UserLabel = %q, want %q", got, "重构会话")
	}
	if got := restored[unlabeled.key].UserLabel; got != "" {
		t.Errorf("unlabeled UserLabel = %q, want empty", got)
	}
}

func TestLoadStoreNotExist(t *testing.T) {
	restored := loadStore("/tmp/does-not-exist-naozhi-test.json")
	if restored != nil {
		t.Errorf("loadStore(nonexistent) = %v, want nil", restored)
	}
}

func TestSaveStoreEmptyPath(t *testing.T) {
	if err := saveStore("", nil); err != nil {
		t.Errorf("saveStore(\"\") error: %v", err)
	}
}

func TestLoadStoreEmptyPath(t *testing.T) {
	if restored := loadStore(""); restored != nil {
		t.Errorf("loadStore(\"\") = %v, want nil", restored)
	}
}

func TestSaveStoreCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "sessions.json")

	sessions := map[string]*ManagedSession{
		"test:key": newSessionWithID("test:key", "sess-1"),
	}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestLoadStoreInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("not json"), 0644)

	restored := loadStore(path)
	if restored != nil {
		t.Errorf("loadStore(bad json) = %v, want nil", restored)
	}
}

func TestSaveAndLoadPrevSessionIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	s := newSessionWithID("test:key", "sess-3")
	s.prevSessionIDs = []string{"sess-1", "sess-2"}

	sessions := map[string]*ManagedSession{"test:key": s}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}

	restored := loadStore(path)
	if restored == nil {
		t.Fatal("loadStore() returned nil")
	}
	entry := restored["test:key"]
	if entry == nil {
		t.Fatal("entry not found")
	}
	if len(entry.PrevSessionIDs) != 2 || entry.PrevSessionIDs[0] != "sess-1" || entry.PrevSessionIDs[1] != "sess-2" {
		t.Errorf("PrevSessionIDs = %v, want [sess-1 sess-2]", entry.PrevSessionIDs)
	}
}

// TestStoreMetaPath pins the sidecar path derivation — sessions.json →
// sessions.meta.json in the same directory. Locking this keeps storeMetaPath
// separate from any ad-hoc callers that might otherwise drift.
func TestStoreMetaPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/var/naozhi/sessions.json", "/var/naozhi/sessions.meta.json"},
		// Non-.json suffix: callers only ever pass *.json, but the derivation
		// stays sensible if it's e.g. *.store (future-proofing).
		{"/tmp/custom.store", "/tmp/custom.meta.store"},
		{"bare.json", "bare.meta.json"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := storeMetaPath(tc.in)
			if got != tc.want {
				t.Errorf("storeMetaPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSaveStore_WritesMetaSidecar pins that a successful saveStore also
// writes sessions.meta.json with the current version. Operators can
// inspect the meta for drift detection, and a future naozhi upgrade can
// key migration decisions on the stored version.
func TestSaveStore_WritesMetaSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	sessions := map[string]*ManagedSession{
		"k": newSessionWithID("k", "sess-1"),
	}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	metaPath := filepath.Join(dir, "sessions.meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("meta file not written: %v", err)
	}
	var m storeMeta
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if m.Version != storeFormatVersion {
		t.Errorf("meta.Version = %d, want %d", m.Version, storeFormatVersion)
	}
	if m.WrittenAt <= 0 {
		t.Errorf("meta.WrittenAt = %d, want > 0", m.WrittenAt)
	}
}

// TestLoadStore_WithoutMetaSidecar pins the back-compat path: an on-disk
// sessions.json from a pre-sidecar naozhi (no .meta.json next to it) must
// still load without warning-level noise preventing startup. This is the
// primary reason the sidecar was picked over embedding version in the
// array file itself — zero risk of breaking every existing deployment.
func TestLoadStore_WithoutMetaSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	// Hand-write a legacy-shape sessions.json without any meta file.
	legacy := `[{"key":"legacy:k","session_id":"sess-old"}]`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatalf("write legacy store: %v", err)
	}
	// Confirm meta is NOT present.
	if _, err := os.Stat(filepath.Join(dir, "sessions.meta.json")); !os.IsNotExist(err) {
		t.Fatalf("meta file unexpectedly present before test: err=%v", err)
	}

	restored := loadStore(path)
	if restored == nil {
		t.Fatal("loadStore returned nil on legacy-shape file")
	}
	if restored["legacy:k"] == nil || restored["legacy:k"].SessionID != "sess-old" {
		t.Errorf("legacy entry not restored; got %+v", restored["legacy:k"])
	}
}

// TestLoadStore_HonoursMetaVersionSignal covers the "future downgrade"
// scenario: operator downgrades naozhi to a binary with an older
// storeFormatVersion; loadStore still returns the entries (the array is
// parsed on its own), but the warning log line gives the operator a
// heads-up that the on-disk format was written by a newer version. We
// can't assert the log line directly without plumbing a logger sink, but
// the test at minimum verifies no panic / no parse abort on this branch.
func TestLoadStore_HonoursMetaVersionSignal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	metaPath := filepath.Join(dir, "sessions.meta.json")

	// Future-shape sessions.json (stored as legacy array so our current
	// parser still handles it).
	body := `[{"key":"k","session_id":"sess-x"}]`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	// Claim version 99 — far above storeFormatVersion.
	future, _ := json.Marshal(storeMeta{Version: 99, WrittenAt: 123456789})
	if err := os.WriteFile(metaPath, future, 0600); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	restored := loadStore(path)
	if restored == nil {
		t.Fatal("loadStore should still return entries on future-version meta")
	}
	if restored["k"] == nil || restored["k"].SessionID != "sess-x" {
		t.Errorf("entry = %+v, want session_id=sess-x", restored["k"])
	}
}

// TestReadStoreMeta_MalformedMetaIsTolerated pins that a corrupt
// sessions.meta.json does not tank load — we treat it as "missing", same
// as the legacy path. This matters because the meta file is written
// atomically but a bad sysadmin could hand-edit it; failing hard would
// lose all sessions on the next startup.
func TestReadStoreMeta_MalformedMetaIsTolerated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	metaPath := filepath.Join(dir, "sessions.meta.json")

	if err := os.WriteFile(path, []byte(`[]`), 0600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	if err := os.WriteFile(metaPath, []byte("not-json"), 0600); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	_, ok := readStoreMeta(path)
	if ok {
		t.Error("readStoreMeta should report ok=false on malformed meta")
	}
	// Load should still succeed (empty but non-nil map — actually nil for
	// empty array, but the important thing is no panic).
	restored := loadStore(path)
	// restored may be an empty map — just ensure no panic occurred.
	_ = restored
}
