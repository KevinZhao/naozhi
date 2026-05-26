package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCliAvailable_TTLRefresh locks R250-SEC-3: the cliAvailable cache must
// re-stat the binary path after the TTL window elapses. Without a TTL a
// removed/redeployed binary stays "available" until process restart, which
// silently masks deploy errors on /health. The test exercises the refresh
// path via the cliAvailableAt seam (synthetic clock) so the production
// 60s TTL doesn't block the test for a real minute.
func TestCliAvailable_TTLRefresh(t *testing.T) {
	// The package-level cache is shared across tests; use a unique path
	// per subtest so we don't race a sibling test's cache entry. No
	// t.Parallel — we mutate the package-level cliAvailCacheTTL to make
	// the assertions readable, and parallel tests would observe each
	// others' TTL changes.

	dir := t.TempDir()
	binPath := filepath.Join(dir, "naozhi-cli-fake")

	// Phase 1: file exists → cache should report true and persist.
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}
	t0 := time.Now()
	if !cliAvailableAt(binPath, t0) {
		t.Fatal("expected available=true on first call (file present)")
	}

	// Phase 2: remove the file but stay inside the TTL window. The cache
	// must keep returning true — that is the whole point of the cache.
	if err := os.Remove(binPath); err != nil {
		t.Fatalf("remove binary: %v", err)
	}
	if !cliAvailableAt(binPath, t0.Add(cliAvailCacheTTL/2)) {
		t.Error("expected stale-true read inside TTL window — TTL is too short or refresh is too eager")
	}

	// Phase 3: advance the clock past the TTL. The next call must re-stat
	// and surface the removal as available=false. This is the regression
	// guard for R250-SEC-3.
	if cliAvailableAt(binPath, t0.Add(cliAvailCacheTTL+time.Second)) {
		t.Error("expected available=false after TTL expiry — cache is not refreshing")
	}

	// Phase 4: re-create the binary and advance again; cache must come
	// back to true. Confirms the TTL path is bidirectional, not just a
	// one-shot invalidation.
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("recreate binary: %v", err)
	}
	if !cliAvailableAt(binPath, t0.Add(2*cliAvailCacheTTL+time.Second)) {
		t.Error("expected available=true after re-create + TTL expiry")
	}
}

// TestCliAvailable_KeyedByPath pins that cache entries do not bleed across
// different paths — a precondition for the TTL test above and a defence
// against a future refactor accidentally collapsing the key.
func TestCliAvailable_KeyedByPath(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	bad := filepath.Join(dir, "bad-does-not-exist")
	if err := os.WriteFile(good, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	now := time.Now()
	if !cliAvailableAt(good, now) {
		t.Error("good path should be available")
	}
	if cliAvailableAt(bad, now) {
		t.Error("bad path should not be available — cache must not share answers across keys")
	}
}
