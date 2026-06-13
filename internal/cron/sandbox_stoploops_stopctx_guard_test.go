package cron

import (
	"os"
	"regexp"
	"testing"
)

// TestStopSandboxRunsForJob_HasStopCtxGuard is a source-anchor test
// asserting that the stopSandboxRunsForJob scan loop contains a
// s.stopCtx.Err() guard before performing any per-entry work, mirroring
// the symmetrical guard in reconcileSandboxPending (line 105).
// Without this guard, N×30s StopSession calls during shutdown can exhaust
// gcWaitBudget. [R20260613-GO-002]
func TestStopSandboxRunsForJob_HasStopCtxGuard(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("sandbox_pending.go")
	if err != nil {
		t.Fatalf("could not read sandbox_pending.go: %v", err)
	}

	// Locate the stopSandboxRunsForJob function body.
	fnStart := regexp.MustCompile(`func \(s \*Scheduler\) stopSandboxRunsForJob\(`).FindIndex(body)
	if fnStart == nil {
		t.Fatal("stopSandboxRunsForJob not found in sandbox_pending.go")
	}
	// Grab the text from the function start to the end of the file (the next
	// top-level func or end-of-file terminates naturally for our match purposes).
	fnBody := body[fnStart[0]:]

	// The guard must appear INSIDE the for-range loop over entries,
	// and BEFORE any os.ReadFile or StopSession call.
	guardRe := regexp.MustCompile(`for\s*,\s*e\s*:=\s*range\s+entries\s*\{[^}]*s\.stopCtx\.Err\(\)\s*!=\s*nil`)
	// Use a more permissive approach: find for..range entries, then check guard precedes ReadFile.
	forRangeIdx := regexp.MustCompile(`for\s+_,\s+e\s*:=\s*range\s+entries\s*\{`).FindIndex(fnBody)
	if forRangeIdx == nil {
		t.Fatal("stopSandboxRunsForJob: for _, e := range entries loop not found")
	}
	loopBody := fnBody[forRangeIdx[0]:]

	stopCtxIdx := regexp.MustCompile(`s\.stopCtx\.Err\(\)`).FindIndex(loopBody)
	if stopCtxIdx == nil {
		t.Error("stopSandboxRunsForJob scan loop is missing s.stopCtx.Err() guard [R20260613-GO-002]")
	}
	readFileIdx := regexp.MustCompile(`os\.ReadFile\(`).FindIndex(loopBody)
	if readFileIdx == nil {
		t.Fatal("stopSandboxRunsForJob: os.ReadFile not found in loop body")
	}
	if stopCtxIdx != nil && stopCtxIdx[0] > readFileIdx[0] {
		t.Error("stopSandboxRunsForJob: stopCtx guard appears AFTER os.ReadFile — guard must be first in loop body [R20260613-GO-002]")
	}

	_ = guardRe // declared for documentation clarity
}
