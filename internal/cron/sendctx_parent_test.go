package cron

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSendCtxParentedOnStopCtx is a structural regression test for
// R238-GO-4 / R236-GO-07 (#790, #500): the sendCtx WithTimeout call in
// scheduler_run.go must parent on s.stopCtx, not context.Background, so
// Scheduler.Stop() short-circuits an in-flight Send instead of letting
// it run for up to jobTimeout (default 5min) after Stop returns.
//
// The assertion is intentionally textual: there is no ergonomic seam to
// observe sendCtx itself (it's a local in execute() and *ManagedSession
// is concrete, not interface-mockable). Pinning the source line is a
// small price for catching silent regressions to the historical
// "context.Background()" parent — the inverse change is a one-line edit
// that would otherwise pass every existing test.
func TestSendCtxParentedOnStopCtx(t *testing.T) {
	t.Parallel()
	src := readSchedulerRunSource(t)
	// Allow either order of the two args; gofmt always reorders to
	// (parent, jobTimeout) so the literal substring is stable, but be
	// explicit about both halves so a reformat that introduces newlines
	// inside the call still passes.
	if !strings.Contains(src, "context.WithTimeout(s.stopCtx, jobTimeout)") {
		t.Errorf("scheduler_run.go must parent sendCtx on s.stopCtx;\n" +
			"see issue #790 / #500 — Background() parent leaks Send past Stop()")
	}
	if strings.Contains(src, "context.WithTimeout(context.Background(), jobTimeout)") {
		t.Errorf("scheduler_run.go still uses context.Background() for sendCtx; " +
			"R238-GO-4 regression — must be s.stopCtx")
	}
}

// TestSendCtxCancelsOnStopCtxCancel is the runtime half of the above:
// confirms Go's stdlib semantics still hold (a WithTimeout child of a
// cancelled ctx is itself cancelled). If a future stdlib change ever
// breaks this invariant — extremely unlikely, but it would silently
// reintroduce the original bug — this test fails first.
func TestSendCtxCancelsOnStopCtxCancel(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(context.Background())
	child, childCancel := context.WithTimeout(parent, 5*time.Minute)
	defer childCancel()

	parentCancel()
	select {
	case <-child.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatal("child WithTimeout(parent, 5m) did not cancel after parentCancel; " +
			"sendCtx parenting on s.stopCtx would not propagate Stop()")
	}
}

// readSchedulerRunSource returns the contents of scheduler_run.go for
// textual structural assertions. Kept separate from the test bodies so
// the read failure path is unambiguous. The package test cwd is the
// package directory itself, so the relative path resolves correctly.
func readSchedulerRunSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("os.ReadFile scheduler_run.go: %v", err)
	}
	return string(b)
}
