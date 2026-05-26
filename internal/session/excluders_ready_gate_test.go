// Tests for R242-ARCH-16 (#760) excluders-ready gate.
//
// Two contracts:
//
//  1. Default RouterConfig (PendingExcluders=false) leaves the gate
//     closed, so legacy embedders and tests see no behavioural change
//     — waitExcludersReady returns true without blocking.
//
//  2. With PendingExcluders=true the gate blocks until
//     MarkExcludersReady is called; if the bounded timeout fires
//     first the auto-chain reader gives up and the metrics counter
//     increments.
package session

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestExcludersReadyGate_DefaultClosed verifies that a Router built
// without RouterConfig.PendingExcluders has a pre-closed gate, so
// waitExcludersReady returns true immediately. This pins the
// no-regression contract for embedders that never register any
// excluders (single-owner topologies, tests).
func TestExcludersReadyGate_DefaultClosed(t *testing.T) {
	r := NewRouter(RouterConfig{})

	start := time.Now()
	ok := r.waitExcludersReady(context.Background())
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("default gate must be pre-closed; waitExcludersReady returned false")
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("default gate must not block; waitExcludersReady took %v", elapsed)
	}
}

// TestExcludersReadyGate_PendingBlocksUntilMark verifies that with
// PendingExcluders=true the gate blocks until MarkExcludersReady is
// called from another goroutine, and then returns true.
func TestExcludersReadyGate_PendingBlocksUntilMark(t *testing.T) {
	r := NewRouter(RouterConfig{PendingExcluders: true})

	// Sanity: the channel exists but is open.
	select {
	case <-r.excludersReadyCh:
		t.Fatal("PendingExcluders=true must leave excludersReadyCh open at NewRouter")
	default:
	}

	done := make(chan bool, 1)
	go func() {
		done <- r.waitExcludersReady(context.Background())
	}()

	// Mark ready after a brief delay; the waiter must observe the
	// close and return true.
	time.Sleep(20 * time.Millisecond)
	r.MarkExcludersReady()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitExcludersReady returned false after MarkExcludersReady")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitExcludersReady did not return within 2s of MarkExcludersReady")
	}
}

// TestExcludersReadyGate_CtxCancelExits verifies that a ctx cancel
// while the gate is still open causes waitExcludersReady to return
// false and the timeout counter to increment. Without this the
// auto-chain readers could block indefinitely on a wireup that
// never calls MarkExcludersReady.
func TestExcludersReadyGate_CtxCancelExits(t *testing.T) {
	r := NewRouter(RouterConfig{PendingExcluders: true})

	before := metrics.AutoChainExcludersWaitTimeout.Value()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- r.waitExcludersReady(ctx)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("waitExcludersReady should return false on ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitExcludersReady did not return within 2s of ctx cancel")
	}

	if got := metrics.AutoChainExcludersWaitTimeout.Value(); got <= before {
		t.Fatalf("AutoChainExcludersWaitTimeout did not increment on timeout: before=%d after=%d",
			before, got)
	}
}

// TestExcludersReadyGate_MarkIdempotent verifies that calling
// MarkExcludersReady multiple times does not panic on the second
// close — sync.Once guards the close call.
func TestExcludersReadyGate_MarkIdempotent(t *testing.T) {
	r := NewRouter(RouterConfig{PendingExcluders: true})
	r.MarkExcludersReady()
	r.MarkExcludersReady() // must not panic on double close.
	r.MarkExcludersReady()

	if !r.waitExcludersReady(context.Background()) {
		t.Fatal("gate must be closed after MarkExcludersReady")
	}
}
