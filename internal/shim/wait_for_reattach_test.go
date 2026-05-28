package shim

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestWaitForReattach_DoneCloses verifies the helper exits promptly when
// s.done is closed before any reconnect arrives. Mirrors the original
// "exiting: done after <reason>" branch. R237-CR-5 (#707).
func TestWaitForReattach_DoneCloses(t *testing.T) {
	s := &shimServer{done: make(chan struct{})}
	acceptCh := make(chan net.Conn, 1)
	var spawnCalls atomic.Int32
	spawn := func(net.Conn) { spawnCalls.Add(1) }

	close(s.done)

	doneCh := make(chan struct{})
	go func() {
		s.waitForReattach(acceptCh, spawn, "test")
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("waitForReattach did not return after s.done close")
	}
	if got := spawnCalls.Load(); got != 0 {
		t.Fatalf("spawnClient called %d times; want 0 (no accept fired)", got)
	}
}

// TestWaitForReattach_ReconnectThenDone verifies the second-stage timer
// path: a client connects, spawnClient fires once, then s.done closes
// and the reconnectTimer branch returns immediately. Mirrors the
// "exiting: done after <reason> + reconnect" branch. R237-CR-5.
func TestWaitForReattach_ReconnectThenDone(t *testing.T) {
	s := &shimServer{done: make(chan struct{})}
	acceptCh := make(chan net.Conn, 1)
	var spawnCalls atomic.Int32
	spawn := func(net.Conn) { spawnCalls.Add(1) }

	// Pre-load a fake conn so the first select-arm picks it up.
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	acceptCh <- left

	doneCh := make(chan struct{})
	go func() {
		s.waitForReattach(acceptCh, spawn, "test")
		close(doneCh)
	}()

	// TEST1 (#395): wait for spawn to fire before closing s.done so the
	// test exercises the "reconnect THEN done" ordering deterministically.
	// Eventually replaces the previous time.Sleep(20ms) so slow CI runs
	// produce a clear diagnostic instead of a silent ordering race.
	testhelper.Eventually(t, func() bool {
		return spawnCalls.Load() == 1
	}, 2*time.Second, "spawn never fired after acceptCh delivery")
	close(s.done)

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("waitForReattach did not return after reconnect + s.done close")
	}
	if got := spawnCalls.Load(); got != 1 {
		t.Fatalf("spawnClient called %d times; want 1 (single accept)", got)
	}
}
