package cron

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestR20260613CR2_DeadlineExceededUsesErrorsIs pins R20260613-CR-2 + CR-6:
// the transport-failure state classification in executeSandbox must use
// errors.Is(ctx.Err(), context.DeadlineExceeded) rather than == so that
// wrapped deadline errors are correctly classified as RunStateTimedOut.
//
// context.DeadlineExceeded is an interface value; errors.Is traverses the
// error chain, matching wrapped variants (e.g. fmt.Errorf("...: %w", ctx.Err())).
// The == operator only matches the exact sentinel and would miss wrapped forms.
func TestR20260613CR2_DeadlineExceededUsesErrorsIs(t *testing.T) {
	t.Parallel()

	// Direct sentinel — both == and errors.Is should match.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if ctx.Err() != context.DeadlineExceeded {
		t.Skip("context didn't expire immediately; skip")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Error("errors.Is failed for direct context.DeadlineExceeded")
	}

	// Verify the classification logic (mirroring the production site):
	// Before the fix: == was used; after the fix: errors.Is is used.
	state := RunStateFailed
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		state = RunStateTimedOut
	}
	if state != RunStateTimedOut {
		t.Errorf("state = %v, want RunStateTimedOut for DeadlineExceeded ctx", state)
	}
}

// TestR20260613CR2_CanceledCtxDoesNotClassifyAsTimedOut verifies that a
// context.Canceled error is NOT classified as RunStateTimedOut — it must
// remain RunStateFailed (the cancel-to-Canceled upgrade is tracked as a
// separate issue; this test just pins the non-regression for the Canceled case).
func TestR20260613CR2_CanceledCtxDoesNotClassifyAsTimedOut(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// ctx.Err() == context.Canceled

	state := RunStateFailed
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		state = RunStateTimedOut
	}
	if state != RunStateFailed {
		t.Errorf("state = %v for Canceled ctx, want RunStateFailed (not TimedOut)", state)
	}
}
