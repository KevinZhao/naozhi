package session

import (
	"path/filepath"
	"testing"
	"time"
)

// TestShutdown_UsesSliceStoreContract used to live here, scanning
// router_cleanup.go for `[]*ManagedSession` and `saveStoreSlice(` to pin
// R20260603-PERF-1 (a value slice instead of a map copy, avoiding hashmap bucket
// allocation). It is gone (Epic I #2547).
//
// The functional half — that shutdown still persists every session through the
// slice path — is the round-trip test below, which a revert to the map form would
// not fail but which is what actually matters. The perf half is one map allocation
// per SHUTDOWN, and Shutdown runs once per process (pinned by
// TestShutdown_RunsItsBodyOnce). Guarding one allocation per process lifetime with
// a source-scanning test that every rename breaks is the shape #2497 is about.

// TestShutdown_SliceStore_MultiSessionRoundTrip verifies that after the
// PERF-1 change shutdown still persists multiple sessions correctly via the
// slice path (R20260603-PERF-1).
func TestShutdown_SliceStore_MultiSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		ss:        newSessionTable(),
		maxProcs:  3,
		ttl:       30 * time.Minute,
		storePath: storePath,
	}
	r.ss.Put("feishu:direct:alice:general", newSessionWithID("feishu:direct:alice:general", "sess-alice"))
	r.ss.Put("feishu:direct:bob:general", newSessionWithID("feishu:direct:bob:general", "sess-bob"))

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
