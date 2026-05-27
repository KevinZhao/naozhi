package cli

import (
	"context"
	"testing"
	"time"
)

// TestLifecycleContextNoChannelsNotCanceled pins R20260527-GO-13 (#1289):
// when both done and killCh are nil (legacy test fixture path), the
// lifecycleContext returns a never-canceled context (matches the
// pre-existing godoc contract) and does NOT leak a cancelCtx that would
// trip `go vet` lostcancel.
func TestLifecycleContextNoChannelsNotCanceled(t *testing.T) {
	t.Parallel()
	p := &Process{}
	ctx := p.lifecycleContext()
	if ctx == nil {
		t.Fatal("lifecycleContext returned nil")
	}
	// Re-call: must return the same value (sync.Once).
	if got := p.lifecycleContext(); got != ctx {
		t.Fatalf("second call returned different ctx: %p vs %p", got, ctx)
	}
	// Must not be cancelable on its own.
	select {
	case <-ctx.Done():
		t.Fatal("ctx canceled with no lifetime channels — should be Background")
	case <-time.After(20 * time.Millisecond):
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err non-nil: %v", err)
	}
	// p.lifecycleCtxCancel must be a callable no-op so callers (or test
	// teardown) cannot panic if they ever invoke it.
	if p.lifecycleCtxCancel == nil {
		t.Fatal("lifecycleCtxCancel must be a callable no-op, not nil")
	}
	p.lifecycleCtxCancel()
	// After the no-op cancel, the returned ctx must still be NOT canceled
	// (Background is unaffected).
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err after no-op cancel: %v", err)
	}
}

// TestLifecycleContextCancelsOnDone verifies the live-signal path: when
// p.done is set, closing it cancels the lifecycle ctx.
func TestLifecycleContextCancelsOnDone(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	p := &Process{done: done}
	ctx := p.lifecycleContext()
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err pre-close: %v", err)
	}
	close(done)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ctx not canceled after p.done closed")
	}
	if !errorsIsCanceled(ctx.Err()) {
		t.Fatalf("ctx.Err = %v, want context.Canceled", ctx.Err())
	}
}

// TestLifecycleContextCancelsOnKill verifies the kill-signal path.
func TestLifecycleContextCancelsOnKill(t *testing.T) {
	t.Parallel()
	killCh := make(chan struct{})
	p := &Process{killCh: killCh}
	ctx := p.lifecycleContext()
	close(killCh)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ctx not canceled after killCh closed")
	}
}

func errorsIsCanceled(err error) bool {
	return err == context.Canceled
}
