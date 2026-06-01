package session

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSaveIfDirty_PersistsAndClearsFlag pins the R20260531070014-PERF-9 (#1535)
// contract: saveIfDirty must snapshot the session map under the shared read
// lock (rather than the exclusive write lock) and still produce a correct
// on-disk store plus clear the storeDirty flag when no concurrent mutation
// raced the save.
func TestSaveIfDirty_PersistsAndClearsFlag(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		sessions:  make(map[string]*ManagedSession),
		maxProcs:  3,
		ttl:       30 * time.Minute,
		pruneTTL:  72 * time.Hour,
		storePath: storePath,
	}
	r.sessions["feishu:direct:user1:general"] = newSessionWithID("feishu:direct:user1:general", "sess-abc")
	r.mu.Lock()
	r.storeDirty = true
	r.mu.Unlock()
	r.storeGen.Add(1)

	r.saveIfDirty()

	loaded := loadStore(storePath)
	if loaded == nil {
		t.Fatal("saveIfDirty should have written the store")
	}
	if got := loaded["feishu:direct:user1:general"]; got == nil || got.SessionID != "sess-abc" {
		t.Fatalf("session not persisted correctly: %v", loaded)
	}

	r.mu.RLock()
	dirty := r.storeDirty
	r.mu.RUnlock()
	if dirty {
		t.Error("storeDirty should be cleared after a successful saveIfDirty with no concurrent mutation")
	}
}

// TestSaveIfDirty_NoopWhenClean verifies the early-exit path: with all dirty
// flags false the call must not touch disk.
func TestSaveIfDirty_NoopWhenClean(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		sessions:  make(map[string]*ManagedSession),
		maxProcs:  3,
		ttl:       30 * time.Minute,
		pruneTTL:  72 * time.Hour,
		storePath: storePath,
	}
	r.sessions["feishu:direct:user1:general"] = newSessionWithID("feishu:direct:user1:general", "sess-abc")

	r.saveIfDirty()

	if loaded := loadStore(storePath); loaded != nil {
		t.Errorf("saveIfDirty must not write when nothing is dirty; got %v", loaded)
	}
}

// TestSaveIfDirty_KnownIDsThrottleCommit verifies the known-IDs save path still
// commits knownIDsSavedAt (now under the short write-locked section) and
// persists the IDs when the throttle interval has elapsed.
func TestSaveIfDirty_KnownIDsThrottleCommit(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		sessions:  make(map[string]*ManagedSession),
		maxProcs:  3,
		ttl:       30 * time.Minute,
		pruneTTL:  72 * time.Hour,
		storePath: storePath,
		knownIDs:  map[string]bool{"sess-1": true, "sess-2": true},
	}
	r.mu.Lock()
	r.knownIDsDirty = true
	r.knownIDsSavedAt = time.Now().Add(-2 * knownIDsSaveInterval)
	before := r.knownIDsSavedAt
	r.mu.Unlock()

	r.saveIfDirty()

	r.mu.RLock()
	savedAt := r.knownIDsSavedAt
	dirty := r.knownIDsDirty
	r.mu.RUnlock()
	if !savedAt.After(before) {
		t.Error("knownIDsSavedAt must be advanced after a due known-IDs save")
	}
	if dirty {
		t.Error("knownIDsDirty should be cleared after a successful save")
	}

	loaded := loadKnownIDs(storePath)
	if len(loaded) != 2 || !loaded["sess-1"] || !loaded["sess-2"] {
		t.Errorf("known IDs not persisted: %v", loaded)
	}
}
