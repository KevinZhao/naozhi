package session

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// TestShutdown_UsesSliceStoreContract pins R20260603-PERF-1: shutdown() must
// allocate a []*ManagedSession slice (not a map[string]*ManagedSession) and
// call saveStoreSlice, not saveStore. This avoids hashmap bucket allocation on
// every shutdown. A source-level check catches a silent revert that would
// still compile but waste allocations.
func TestShutdown_UsesSliceStoreContract(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("router_cleanup.go")
	if err != nil {
		t.Fatalf("read router_cleanup.go: %v", err)
	}

	// The slice allocation pattern must be present inside shutdown().
	if !regexp.MustCompile(`\[\]\*ManagedSession`).Match(src) {
		t.Error("shutdown(): []*ManagedSession slice allocation not found. " +
			"R20260603-PERF-1 requires a value slice instead of a map copy to " +
			"avoid hashmap bucket allocation on every shutdown.")
	}
	// saveStoreSlice must be called (not the map-based saveStore).
	if !regexp.MustCompile(`saveStoreSlice\(`).Match(src) {
		t.Error("shutdown(): saveStoreSlice call not found. " +
			"R20260603-PERF-1 replaced saveStore(map) with saveStoreSlice(slice).")
	}
}

// TestShutdown_SliceStore_MultiSessionRoundTrip verifies that after the
// PERF-1 change shutdown still persists multiple sessions correctly via the
// slice path (R20260603-PERF-1).
func TestShutdown_SliceStore_MultiSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		ss:        sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:  3,
		ttl:       30 * time.Minute,
		storePath: storePath,
	}
	r.ss.sessions["feishu:direct:alice:general"] = newSessionWithID("feishu:direct:alice:general", "sess-alice")
	r.ss.sessions["feishu:direct:bob:general"] = newSessionWithID("feishu:direct:bob:general", "sess-bob")

	r.Shutdown()

	loaded := loadStore(storePath)
	if len(loaded) != 2 {
		t.Fatalf("loaded %d sessions, want 2", len(loaded))
	}
	if loaded["feishu:direct:alice:general"].SessionID != "sess-alice" {
		t.Errorf("alice session missing or wrong ID: %v", loaded)
	}
	if loaded["feishu:direct:bob:general"].SessionID != "sess-bob" {
		t.Errorf("bob session missing or wrong ID: %v", loaded)
	}
}
