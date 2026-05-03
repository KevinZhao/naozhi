package feishu

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCleanupNoncesTick_NormalSweep — sanity-check that a single tick sweep
// deletes expired entries and clamps the counter. No panic path is exercised
// by this test; it just covers the happy path.
func TestCleanupNoncesTick_NormalSweep(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	// Store two expired entries (ts < now).
	f.seenNonces.Store("k1", int64(1))
	f.seenNonces.Store("k2", int64(2))
	f.seenNoncesCount.Store(2)

	f.cleanupNoncesTick()

	if _, ok := f.seenNonces.Load("k1"); ok {
		t.Error("expected k1 deleted by sweep")
	}
	if _, ok := f.seenNonces.Load("k2"); ok {
		t.Error("expected k2 deleted by sweep")
	}
	if n := f.seenNoncesCount.Load(); n != 0 {
		t.Errorf("seenNoncesCount = %d; want 0", n)
	}
}

// TestCleanupNoncesTick_ClampsNegativeCounter — the defensive comment in
// production says: if Delete bypasses the counted insert path, the counter
// could go negative. Exercise that path with one pre-seeded expired entry
// but counter=0, and verify the tick clamps back to 0 instead of landing at
// -1.
func TestCleanupNoncesTick_ClampsNegativeCounter(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	f.seenNonces.Store("k1", int64(0)) // expired (< now)
	f.seenNoncesCount.Store(0)

	f.cleanupNoncesTick()

	if n := f.seenNoncesCount.Load(); n != 0 {
		t.Errorf("counter should be clamped to 0 after negative Add, got %d", n)
	}
}

// TestCleanupNoncesTick_DropsMalformedEntry — the defensive type assertion
// must evict entries whose value is not int64 (e.g. a future refactor that
// accidentally stores a different type). Counter is intentionally not
// incremented for such entries in production inserts, so a delete here
// drives the counter toward negative which the clamp corrects.
func TestCleanupNoncesTick_DropsMalformedEntry(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	// Store a string where int64 is expected.
	f.seenNonces.Store("bad", "not-an-int64")
	f.seenNoncesCount.Store(0)

	f.cleanupNoncesTick()

	if _, ok := f.seenNonces.Load("bad"); ok {
		t.Error("malformed entry should have been dropped")
	}
}

// TestCleanupNonces_RecoverAtTickLevel is an R175-P1 source-level regression
// gate. The earlier Round 174 shape placed `defer recover()` at the function
// scope, which would log a panic but then unwind the goroutine — killing
// replay protection for the process lifetime. The fix must locate the
// recover frame inside a per-tick helper (cleanupNoncesTick) so the `for`
// loop survives and the next tick retries. A future revert that inlines
// the recover back into cleanupNonces proper will fail this test.
func TestCleanupNonces_RecoverAtTickLevel(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("feishu.go")
	if err != nil {
		t.Fatalf("read feishu.go: %v", err)
	}
	src := string(data)

	// 1) cleanupNonces itself must NOT have a function-scope defer recover.
	idx := strings.Index(src, "func (f *Feishu) cleanupNonces(ctx context.Context) {")
	if idx < 0 {
		t.Fatal("cleanupNonces func not found")
	}
	end := strings.Index(src[idx:], "\n}\n")
	if end < 0 {
		t.Fatal("cleanupNonces body not terminated")
	}
	body := src[idx : idx+end]
	if strings.Contains(body, "defer func() {\n\t\tif r := recover()") {
		t.Error("cleanupNonces must not contain a function-scope recover; " +
			"recover must live in cleanupNoncesTick so a panic does NOT " +
			"exit the for-loop and kill replay protection")
	}

	// 2) The tick helper must exist and must contain the recover frame.
	tickIdx := strings.Index(src, "func (f *Feishu) cleanupNoncesTick()")
	if tickIdx < 0 {
		t.Fatal("cleanupNoncesTick helper missing — recover must be isolated per-tick")
	}
	tickEnd := strings.Index(src[tickIdx:], "\n}\n")
	if tickEnd < 0 {
		t.Fatal("cleanupNoncesTick body not terminated")
	}
	tickBody := src[tickIdx : tickIdx+tickEnd]
	if !strings.Contains(tickBody, "recover()") {
		t.Error("cleanupNoncesTick must contain recover() so a panic does not propagate to the for-loop")
	}

	// 3) cleanupNonces must actually call the tick helper.
	if !strings.Contains(body, "f.cleanupNoncesTick()") {
		t.Error("cleanupNonces must invoke cleanupNoncesTick so recover is engaged on each sweep")
	}
}

// Compile-time sanity: cleanupNoncesTick exists and has the right shape —
// if someone renames or removes it the other tests fail to compile, but we
// also want an explicit reference so the package doc is reachable.
var _ = (*Feishu)(nil).cleanupNoncesTick

// silence unused import if atomic goes away in future refactors
var _ atomic.Int64
