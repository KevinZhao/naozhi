package shim

import (
	"errors"
	"net"
	"sync"
	"testing"
)

// TestShimHandle_SendMsg_ClosedGuard verifies that SendMsg becomes a
// deterministic net.ErrClosed no-op once the handle has been Close()d
// (R20260608133928-ARCH-1, #1969). Before the fix, SendMsg after Close
// would Flush onto an already-closed connection, racing Conn.Close().
func TestShimHandle_SendMsg_ClosedGuard(t *testing.T) {
	tests := []struct {
		name     string
		closeFn  func(h *ShimHandle)
		wantNoOp bool
	}{
		{
			name:     "after Close",
			closeFn:  func(h *ShimHandle) { h.Close() },
			wantNoOp: true,
		},
		{
			name:     "after Detach (Close inside)",
			closeFn:  func(h *ShimHandle) { h.Detach() },
			wantNoOp: true,
		},
		{
			name:     "after Shutdown (Close inside)",
			closeFn:  func(h *ShimHandle) { h.Shutdown() },
			wantNoOp: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handle, server := newTestHandlePair(t)
			defer handle.Conn.Close()
			defer server.Close()

			// Detach/Shutdown call SendMsg internally before Close; drain
			// the server side so that initial write does not block on the
			// synchronous net.Pipe.
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				var msg ClientMsg
				_ = readLineMsgConn(server, &msg) // best-effort drain
			}()

			tt.closeFn(handle)
			<-drained

			// Post-close SendMsg must be a deterministic no-op, never a
			// racy write onto the closed fd.
			err := handle.SendMsg(ClientMsg{Type: "write", Line: "x"})
			if tt.wantNoOp {
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("SendMsg after close = %v, want net.ErrClosed", err)
				}
			}
		})
	}
}

// TestShimHandle_SendMsg_ConcurrentCloseNoRace exercises the WriteMu /
// ClientDone interaction under concurrent SendMsg and Close so that
// `go test -race` flags any write-after-close on the connection.
func TestShimHandle_SendMsg_ConcurrentCloseNoRace(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	// Continuously drain the server side so non-closed writes can complete.
	go func() {
		for {
			var msg ClientMsg
			if err := readLineMsgConn(server, &msg); err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			// Either succeeds or returns net.ErrClosed; must never panic
			// or write onto a closed fd.
			_ = handle.SendMsg(ClientMsg{Type: "write", Line: "x"})
		}
	}()
	go func() {
		defer wg.Done()
		handle.Close()
	}()
	wg.Wait()

	// Once closed, SendMsg is a stable no-op.
	if err := handle.SendMsg(ClientMsg{Type: "write", Line: "x"}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("final SendMsg = %v, want net.ErrClosed", err)
	}
}
