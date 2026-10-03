package shim

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

// promptly bounds a ctx-aborted handshake. The fixed limits it must beat are
// connect's 10s hello deadline and DrainReplay's 20s drainReplayTimeout.
const promptly = 2 * time.Second

var ctxToken = []byte("ctx-token-32-bytes-padded!!!!!!!")

// helloLine is a hello frame connect accepts.
func helloLine() []byte {
	return fmt.Appendf(nil, "{\"type\":\"hello\",\"protocol_version\":%d}\n", ProtocolVersion)
}

// serveAttach accepts one connection, reads the attach line, runs then, and
// holds the conn open until the client closes it.
func (f *fakeShimServer) serveAttach(then func(conn net.Conn, rd *bufio.Reader)) {
	f.handleOnce(func(conn net.Conn) {
		rd := bufio.NewReader(conn)
		if _, err := rd.ReadBytes('\n'); err != nil {
			return
		}
		then(conn, rd)
		io.Copy(io.Discard, rd) //nolint:errcheck // hold until the client hangs up
	})
}

func TestConnect_CancelDuringHelloAbortsPromptly(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancelled once the attach has arrived, i.e. while connect waits for hello.
	srv.serveAttach(func(net.Conn, *bufio.Reader) { cancel() })

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	start := time.Now()
	_, err := m.connect(ctx, srv.path, ctxToken, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("connect err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > promptly {
		t.Errorf("connect returned %v after cancel, want under %v", took, promptly)
	}
}

func TestConnect_CtxDeadlineShortensTheHelloWait(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()
	srv.serveAttach(func(net.Conn, *bufio.Reader) {}) // never says hello

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	start := time.Now()
	_, err := m.connect(ctx, srv.path, ctxToken, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connect err = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > promptly {
		t.Errorf("connect returned %v, want the 200ms ctx deadline to cut the 10s hello wait", took)
	}
}

// TestConnect_CancelledCtxNeverDials covers the later shims of a reconnect
// pass that SIGTERM has already cancelled: none of them may be dialled.
func TestConnect_CancelledCtxNeverDials(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	if _, err := m.connect(ctx, srv.path, ctxToken, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("connect err = %v, want context.Canceled", err)
	}
	// A completed dial sits in the listen backlog even after the client has
	// closed it, so a short Accept still returns it.
	if err := srv.ln.(*net.UnixListener).SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := srv.ln.Accept(); err == nil {
		conn.Close()
		t.Error("connect dialled the shim with an already-cancelled ctx")
	}
}

// roundTrip writes a ping on h and reads the server's echo back.
func roundTrip(h *ShimHandle) error {
	if _, err := h.Conn.Write([]byte("ping\n")); err != nil {
		return err
	}
	line, err := h.Reader.ReadString('\n')
	if err != nil {
		return err
	}
	if line != "pong\n" {
		return fmt.Errorf("echo = %q, want pong", line)
	}
	return nil
}

// servePong answers one ping line with pong.
func servePong(conn net.Conn, rd *bufio.Reader) {
	if _, err := rd.ReadString('\n'); err != nil {
		return
	}
	conn.Write([]byte("pong\n")) //nolint:errcheck
}

func TestConnect_CancelAfterHandshakeLeavesTheConnOpen(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()
	srv.serveAttach(func(conn net.Conn, rd *bufio.Reader) {
		conn.Write(helloLine()) //nolint:errcheck
		servePong(conn, rd)
	})

	ctx, cancel := context.WithCancel(context.Background())
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	h, err := m.connect(ctx, srv.path, ctxToken, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer h.Close()
	cancel()
	if err := roundTrip(h); err != nil {
		t.Errorf("round trip after cancelling the handshake ctx: %v, want the conn left to its Process", err)
	}
}

// TestConnect_CancelRacingTheHelloNeverReturnsAClosedConn cancels after a
// varying number of yields once the hello is written, so the cancel lands on
// either side of the handshake's end. Whichever wins, connect returns a ctx
// error or a usable conn, never a handle the cancel callback has closed.
func TestConnect_CancelRacingTheHelloNeverReturnsAClosedConn(t *testing.T) {
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	var won, lost int
	for i := range 300 {
		srv := newFakeShimServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		srv.serveAttach(func(conn net.Conn, rd *bufio.Reader) {
			conn.Write(helloLine()) //nolint:errcheck
			go func() {
				for range i % 100 {
					runtime.Gosched()
				}
				cancel()
			}()
			servePong(conn, rd)
		})
		h, err := m.connect(ctx, srv.path, ctxToken, 0)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("connect err = %v, want context.Canceled", err)
			}
			lost++
		} else {
			won++
			if err := roundTrip(h); err != nil {
				t.Fatalf("connect returned a handle whose conn the cancel closed: %v", err)
			}
			h.Close()
		}
		cancel()
		srv.cleanup()
	}
	t.Logf("handshake beat the cancel %d times, lost %d", won, lost)
}

func TestShimHandle_DrainReplay_CancelAbortsPromptly(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// net.Pipe hands the line over synchronously, so the drain is
		// running when cancel fires; replay_done never comes.
		writeLine(t, server, ServerMsg{Type: "replay", Seq: 1, Line: "a"})
		cancel()
	}()

	start := time.Now()
	_, err := handle.DrainReplay(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DrainReplay err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > promptly {
		t.Errorf("DrainReplay returned %v after cancel, want under %v", took, promptly)
	}
}

func TestShimHandle_DrainReplay_CtxDeadlineShortensTheWait(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := handle.DrainReplay(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DrainReplay err = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > promptly {
		t.Errorf("DrainReplay returned %v, want the 200ms ctx deadline to cut the 20s wait", took)
	}
}

func TestShimHandle_DrainReplay_CancelAfterDrainLeavesTheConnOpen(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	go func() {
		writeLine(t, server, ServerMsg{Type: "replay_done"})
		servePong(server, bufio.NewReader(server))
	}()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := handle.DrainReplay(ctx); err != nil {
		t.Fatalf("DrainReplay: %v", err)
	}
	cancel()
	if err := roundTrip(handle); err != nil {
		t.Errorf("round trip after cancelling the drain ctx: %v, want the conn left to its Process", err)
	}
}
