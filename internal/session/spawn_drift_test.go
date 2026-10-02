package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/shim"
)

// driftArgsFor builds the drift check reconnectShims uses, from r's facets.
func driftArgsFor(r *Router) driftArgs {
	return driftArgs{backends: &r.backends, spawn: &r.spawn}
}

// TestReconnectShims_DriftCheckUsesTheRoutersFacets runs the drift check
// ReconnectShimsCtx builds itself, not driftArgsFor's copy. A surviving shim
// whose argv carries the router's --model, --mcp-config and --settings is reconnected,
// not shut down as drifted; one whose argv differs is marked as a drift
// shutdown. The shim is this test process with a socket file nothing listens
// on, so the reconnect and the shutdown both fail harmlessly after the verdict.
func TestReconnectShims_DriftCheckUsesTheRoutersFacets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness")
	}
	for _, tc := range []struct {
		name  string
		extra []string
		drift bool
	}{
		{"argv recorded under the router's spawn config", nil, false},
		{"argv that differs from it", []string{"--extra-flag"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
			w.ShimManager = mgr
			r := NewRouter(RouterConfig{
				Wrapper:            w,
				Model:              "claude-opus-5",
				MCPConfigFile:      filepath.Join(dir, "mcp.json"),
				NaozhiSettingsFile: filepath.Join(dir, "settings.json"),
			})
			t.Cleanup(r.Shutdown)

			key := "feishu:direct:alice:general"
			sess := injectSession(r, key, nil)
			stored := driftArgsFor(r).driftCompareArgs(w, "claude", key, sess, &shim.SpawnOverlay{})
			for _, flag := range []string{"--model", "--mcp-config", "--settings"} {
				if !slices.Contains(stored, flag) {
					t.Fatalf("recorded argv %q lacks %s: the router's backends and spawn config are not both in play", stored, flag)
				}
			}
			// The socket path Reconnect expects, as a plain file nothing listens on.
			// It must exist: ShimPID below is this test process, and Discover
			// SIGTERMs the PID of a shim whose socket is missing.
			t.Setenv("XDG_RUNTIME_DIR", dir)
			socket := shim.SocketPath(shim.KeyHash(key))
			if err := os.WriteFile(socket, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			state := shim.State{
				ShimPID: os.Getpid(), Socket: socket, Key: key, Backend: "claude",
				CLIArgs: append(stored, tc.extra...), SpawnOverlay: &shim.SpawnOverlay{},
			}
			if err := shim.WriteStateFile(shim.StateFilePath(dir, shim.KeyHash(key)), state); err != nil {
				t.Fatal(err)
			}

			r.ReconnectShimsCtx(context.Background())

			if got := r.drift.has(key); got != tc.drift {
				t.Errorf("drift shutdown marked = %v, want %v", got, tc.drift)
			}
			if got := sess.overlayDrift.Load() != nil; got != tc.drift {
				t.Errorf("session overlay drift recorded = %v, want %v", got, tc.drift)
			}
		})
	}
}

// TestReconnectShims_ReconnectCarriesTheRoutersSpawnTimeouts pins the
// SpawnReconnect call in reconnectShims: the reattached process runs under the
// router's configured timeouts, not swapped or zeroed ones. A fake shim on the
// expected socket answers the attach with hello + replay_done and then stays
// silent, so the first turn must end on the 1ms no-output timeout (the watchdog
// polls at 1s); a zeroed or swapped no-output timeout waits minutes and hits
// the test's own deadline. The total timeout is read back directly.
func TestReconnectShims_ReconnectCarriesTheRoutersSpawnTimeouts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	// t.TempDir() on darwin overflows the 104-byte sun_path limit.
	dir, err := os.MkdirTemp("/tmp", "nz-reconn-")
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
	const noOutput, total = time.Millisecond, 97 * time.Minute
	r := NewRouter(RouterConfig{Wrapper: w, NoOutputTimeout: noOutput, TotalTimeout: total})
	t.Cleanup(r.Shutdown)

	key := "feishu:direct:alice:general"
	sess := injectSession(r, key, nil)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket := shim.SocketPath(shim.KeyHash(key))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	t.Cleanup(func() {
		ln.Close()
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
		rd := bufio.NewReader(conn)
		if _, err := rd.ReadBytes('\n'); err != nil { // attach
			return
		}
		fmt.Fprintf(conn, "{\"type\":\"hello\",\"protocol_version\":%d}\n{\"type\":\"replay_done\"}\n", shim.ProtocolVersion)
		io.Copy(io.Discard, rd) //nolint:errcheck // swallow the turn; never answer
	}()
	// ShimPID is this test process: it passes the liveness and binary-identity
	// gates. The socket must exist before ReconnectShimsCtx runs, or Discover
	// treats the shim as a zombie and SIGTERMs that PID, i.e. the test binary.
	state := shim.State{
		ShimPID: os.Getpid(), Socket: socket, Key: key, Backend: "claude",
		CLIArgs:      driftArgsFor(r).driftCompareArgs(w, "claude", key, sess, &shim.SpawnOverlay{}),
		SpawnOverlay: &shim.SpawnOverlay{},
	}
	if err := shim.WriteStateFile(shim.StateFilePath(dir, shim.KeyHash(key)), state); err != nil {
		t.Fatal(err)
	}

	r.ReconnectShimsCtx(context.Background())

	proc, ok := sess.loadProcess().(*cli.Process)
	if !ok || proc == nil {
		t.Fatalf("session process after reconnect = %T, want the reattached *cli.Process", sess.loadProcess())
	}
	if got := proc.TotalTimeout(); got != total {
		t.Errorf("reattached process TotalTimeout = %v, want the router's %v", got, total)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.Send(ctx, "hi", nil, nil); !errors.Is(err, clierr.ErrNoOutputTimeout) {
		t.Errorf("first turn on the silent shim: err = %v, want ErrNoOutputTimeout from the router's %v", err, noOutput)
	}
}
