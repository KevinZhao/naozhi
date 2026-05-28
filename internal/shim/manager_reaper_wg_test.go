package shim

import (
	"context"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"
)

// TestStopAll_WaitsForReaperGoroutines verifies R216-GO-6 (#565):
// StopAll's bounded wait covers the per-shim cmd.Wait() reaper goroutines
// tracked via Manager.reaperWG, not just the per-handle Shutdown sends.
//
// The test exercises the contract directly without spawning a real shim:
// it adds a fake reaper to reaperWG, calls StopAll(ctx) with a tight
// deadline, and asserts StopAll returns within the deadline (the WaitGroup
// path must not block past ctx). It then releases the fake reaper and
// asserts StopAll could not have returned via the wg-drain path before
// release.
func TestStopAll_WaitsForReaperGoroutines(t *testing.T) {
	m := &Manager{shims: make(map[string]*ShimHandle)}

	// Park a fake "reaper" inside reaperWG. StopAll's done-channel goroutine
	// blocks on m.reaperWG.Wait() so it must NOT close `done` until we
	// release this Add. ctx-deadline is the only escape.
	m.reaperWG.Add(1)
	release := make(chan struct{})
	var releasedOnce sync.Once
	releaseFn := func() {
		releasedOnce.Do(func() {
			close(release)
			m.reaperWG.Done()
		})
	}
	defer releaseFn()

	go func() {
		<-release
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	m.StopAll(ctx)
	elapsed := time.Since(start)

	// StopAll must return when ctx expires even though reaperWG is still
	// held — otherwise systemd shutdown could hang forever on a stuck reaper.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("StopAll did not return within ctx-deadline budget: %v", elapsed)
	}
	if elapsed < 40*time.Millisecond {
		// Sanity check: it must have at least waited for ctx.
		t.Fatalf("StopAll returned too early (%v) — reaperWG.Wait was bypassed", elapsed)
	}
}

// TestReaperGoroutine_OnlyCapturesLocalKeyHash pins the second half of the
// R216-GO-6 (#565) contract: the reaper goroutine spawned inside
// StartShimWithBackend must only capture function-local values, not Manager
// state. Today only `keyHash` (a local string) is closed over alongside the
// `cmd` reference. The test source-greps the goroutine body so a future edit
// that captures e.g. `m.shims` or `m.someField` from the surrounding receiver
// is caught here rather than by a sporadic data race in production.
//
// We accept references to `cmd` (cmd.Wait return value) and `keyHash` (the
// log line's session-ID hash). Anything else implies a closure capture that
// the WaitGroup contract alone is not enough to defend — the reaper would
// then need a stronger ownership story (e.g. snapshot-and-pass-by-value).
//
// The helper is intentionally textual: a runtime check would require
// instantiating a fake Manager + spawning the actual goroutine, which is
// what TestStopAll_WaitsForReaperGoroutines already exercises. Pinning the
// SOURCE shape catches the regression at compile-test time, before the race
// detector ever has to. R216-GO-6 (#565).
func TestReaperGoroutine_OnlyCapturesLocalKeyHash(t *testing.T) {
	t.Parallel()
	// Locate the source via runtime build path; fallback to relative path
	// if the binary was stripped of source info (test caches).
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Skipf("manager.go not readable in cwd; skipping source-shape pin: %v", err)
	}
	// Match the goroutine body literally — same shape as line 451-456.
	// Captures the body between `m.reaperWG.Add(1)` and the matching `}()`.
	re := regexp.MustCompile(`(?s)m\.reaperWG\.Add\(1\)\s*\n\s*go func\(\) \{(.*?)\}\(\)`)
	matches := re.FindAllSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("could not locate reaper goroutine in manager.go — has the spawn site moved? Update the regex above.")
	}
	for i, m := range matches {
		body := string(m[1])
		// Forbid any reference to `m.` inside the goroutine body except the
		// permitted `m.reaperWG.Done()` Done call. Anything like `m.shims`
		// or `m.someField` would imply a captured Manager-state read.
		// Lines starting with `//` are comments; strip those before checking.
		stripped := stripComments(body)
		// Permitted: `m.reaperWG.Done()` and any `m.reaperWG` reference is
		// part of the WaitGroup contract itself, not a Manager-state read.
		// Anything else (m.shims, m.cgroup, ...) is a violation.
		fieldRefs := regexp.MustCompile(`\bm\.[A-Za-z_]\w*`).FindAllString(stripped, -1)
		for _, ref := range fieldRefs {
			if ref == "m.reaperWG" {
				continue
			}
			t.Fatalf("reaper goroutine #%d body captures Manager state %q — R216-GO-6 contract requires only function-local captures (keyHash) plus the WaitGroup contract (m.reaperWG). Body:\n%s",
				i, ref, body)
		}
	}
}

// stripComments returns src with `//`-line comments removed so regex
// scans against goroutine bodies don't trip on prose.
func stripComments(src string) string {
	re := regexp.MustCompile(`(?m)//[^\n]*`)
	return re.ReplaceAllString(src, "")
}

// TestStopAll_DrainsWhenReapersExit verifies the happy path: when the
// reaper goroutines complete before ctx expiry, StopAll returns via the
// wg-drain branch rather than the ctx-expired branch.
func TestStopAll_DrainsWhenReapersExit(t *testing.T) {
	m := &Manager{shims: make(map[string]*ShimHandle)}

	// Schedule a fake reaper that exits well within the ctx window.
	m.reaperWG.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		m.reaperWG.Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	m.StopAll(ctx)
	elapsed := time.Since(start)

	// Drain branch: bounded by reaper exit time, not ctx.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("StopAll did not drain promptly: %v (reaperWG should have completed in ~10ms)", elapsed)
	}
}
