// anchor-keep: the stopCtx guard's position INSIDE the scan loop is the fact (bounds N x 30s StopSession at shutdown); observing it behaviourally means wall-clocking a budget exhaustion.
package cron

import (
	"os"
	"regexp"
	"testing"
)

// TestStopSandboxRunsForJob_HasStopCtxGuard asserts that the
// stopSandboxRunsForJob scan loop checks s.stopCtx.Err() before the per-entry
// Stop (stopOneSandboxPendingFile, up to sandboxStopTimeout each), mirroring
// the guard in reconcileSandboxPending. Without it, N×30s StopSession calls
// during shutdown can exhaust gcWaitBudget.
func TestStopSandboxRunsForJob_HasStopCtxGuard(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("sandbox_pending.go")
	if err != nil {
		t.Fatalf("could not read sandbox_pending.go: %v", err)
	}
	fnStart := regexp.MustCompile(`func \(s \*Scheduler\) stopSandboxRunsForJob\(`).FindIndex(body)
	if fnStart == nil {
		t.Fatal("stopSandboxRunsForJob not found in sandbox_pending.go")
	}
	fnBody := body[fnStart[0]:]
	forRangeIdx := regexp.MustCompile(`for\s+_,\s+e\s*:=\s*range\s+entries\s*\{`).FindIndex(fnBody)
	if forRangeIdx == nil {
		t.Fatal("stopSandboxRunsForJob: for _, e := range entries loop not found")
	}
	loopBody := fnBody[forRangeIdx[0]:]
	stopCtxIdx := regexp.MustCompile(`s\.stopCtx\.Err\(\)`).FindIndex(loopBody)
	if stopCtxIdx == nil {
		t.Error("stopSandboxRunsForJob scan loop is missing an s.stopCtx.Err() guard")
	}
	stopIdx := regexp.MustCompile(`s\.stopOneSandboxPendingFile\(`).FindIndex(loopBody)
	if stopIdx == nil {
		t.Fatal("stopSandboxRunsForJob: stopOneSandboxPendingFile not found in the loop body")
	}
	if stopCtxIdx != nil && stopCtxIdx[0] > stopIdx[0] {
		t.Error("stopSandboxRunsForJob: the stopCtx guard comes after the per-entry Stop; it must come first")
	}
}
