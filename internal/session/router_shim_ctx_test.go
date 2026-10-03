package session

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestReconnectShimsCtx_CancelAbortsShimHandshake pins that parentCtx reaches
// the shim I/O of ReconnectShimsCtx. A fake shim on the expected socket takes
// the attach and goes silent, either cancelling the parent context before its
// hello or sending hello + one replay frame under a 300ms parent deadline. A
// pass needs parentCtx to cut the handshake short: without it the reconnect
// waits out the 10s hello deadline or the 15s spawn timeout. The orphan branch
// is left out: its SIGUSR2 fallback would signal ShimPID, the test binary.
func TestReconnectShimsCtx_CancelAbortsShimHandshake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	for _, tc := range []struct {
		name      string
		extra     []string // appended to the recorded argv; non-nil = drift branch
		midReplay bool     // hello + one replay under a parent deadline, instead of a cancel before hello
	}{
		{"reconnect, cancelled before hello", nil, false},
		{"reconnect, parent deadline mid replay", nil, true},
		{"drift shutdown, cancelled before hello", []string{"--extra-flag"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// t.TempDir() on darwin overflows the 104-byte sun_path limit.
			dir, err := os.MkdirTemp("/tmp", "nz-rctx-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
			w.ShimManager = mgr
			r := NewRouter(RouterConfig{Wrapper: w})
			t.Cleanup(r.Shutdown)

			key := "feishu:direct:alice:general"
			sess := injectSession(r, key, nil)
			t.Setenv("XDG_RUNTIME_DIR", dir)
			socket := shim.SocketPath(shim.KeyHash(key))
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })

			// Mid replay uses a deadline, not a cancel from the fake shim: the
			// shim cannot tell when connect has returned and the drain begun.
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.midReplay {
				ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				rd := bufio.NewReader(conn)
				if _, err := rd.ReadBytes('\n'); err != nil { // attach
					return
				}
				if tc.midReplay {
					fmt.Fprintf(conn, "{\"type\":\"hello\",\"protocol_version\":%d}\n{\"type\":\"replay\",\"seq\":1}\n", shim.ProtocolVersion)
				} else {
					cancel()
				}
				io.Copy(io.Discard, rd) //nolint:errcheck // silent until naozhi hangs up
			}()
			// ShimPID is this test process so the liveness and binary-identity
			// gates pass; the socket exists, so Discover does not SIGTERM it.
			state := shim.State{
				ShimPID: os.Getpid(), Socket: socket, Key: key, Backend: "claude",
				CLIArgs:      append(driftArgsFor(r).driftCompareArgs(w, "claude", key, sess, &shim.SpawnOverlay{}), tc.extra...),
				SpawnOverlay: &shim.SpawnOverlay{},
			}
			if err := shim.WriteStateFile(shim.StateFilePath(dir, shim.KeyHash(key)), state); err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			r.ReconnectShimsCtx(ctx)
			if took := time.Since(start); took > 3*time.Second {
				t.Errorf("ReconnectShimsCtx returned %v with its context done mid-handshake, want under 3s", took)
			}
			if tc.extra != nil {
				if !r.drift.has(key) {
					t.Error("drifted shim was not marked as a drift shutdown")
				}
			} else if p := sess.loadProcess(); p != nil {
				t.Errorf("session process after the aborted reconnect = %T, want none attached", p)
			}
		})
	}
}
