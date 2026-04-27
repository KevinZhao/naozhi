package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestDetectVersionCtx_CancelledCtxAbortsPromptly locks R55-QUAL-004's
// ctx plumbing: when the caller's ctx is already cancelled, the probe
// must return immediately instead of waiting out the inner 5s timeout.
// detectVersion (the Background-derived legacy helper) would block here
// regardless of caller shutdown signals; the Ctx variant must not.
//
// We simulate a slow --version binary with a shell script that sleeps
// 10s. Without ctx wiring, the probe would need ≥5s (inner timeout) or
// ≥10s (script). With ctx wiring, an already-cancelled parent returns
// well under 1s.
func TestDetectVersionCtx_CancelledCtxAbortsPromptly(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	dir := t.TempDir()
	slow := filepath.Join(dir, "slow-cli")
	script := "#!/bin/sh\nsleep 10\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate cancel — mimics SIGTERM during startup

	start := time.Now()
	version := detectVersionCtx(ctx, slow)
	elapsed := time.Since(start)

	// With a cancelled parent ctx, exec.CommandContext should kill the
	// child almost immediately; allow generous slack for slow CI, but
	// assert well under the inner 5s cap and the script's 10s sleep.
	if elapsed > 2*time.Second {
		t.Errorf("cancelled ctx probe took %v; want <2s (ctx not wired)", elapsed)
	}
	// The binary never printed a version string, so we expect empty.
	if version != "" {
		t.Errorf("version = %q; want empty on cancelled probe", version)
	}
}

// TestDetectBackendsCtx_CancelledCtxStillReturnsSlice verifies that even
// when the parent ctx is cancelled, DetectBackendsCtx returns a well-
// formed slice (one entry per knownBackends) with Available=false for
// any that needed a --version call — the function must not panic or
// return nil under a pre-cancelled parent, because main.go's fail-fast
// logic relies on a deterministic response shape.
func TestDetectBackendsCtx_CancelledCtxStillReturnsSlice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := DetectBackendsCtx(ctx)
	if len(got) != len(knownBackends) {
		t.Errorf("DetectBackendsCtx returned %d entries, want %d", len(got), len(knownBackends))
	}
	// Detection still probes disk (os.Stat), so we cannot assert Available
	// is universally false — the test just validates the shape contract.
	for i, info := range got {
		if info.ID == "" {
			t.Errorf("entry[%d] has empty ID", i)
		}
	}
}
