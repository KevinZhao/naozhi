package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
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
