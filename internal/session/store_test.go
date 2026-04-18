package session

import (
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
