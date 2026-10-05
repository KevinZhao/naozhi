package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestHandleRequest_Takeover_RefusalKeepsCLIAlive pins #3315 on the
// reverse-connected path: a takeover the node refuses, by its router or by
// the cwd check, is answered before SIGTERM, so the external CLI survives.
func TestHandleRequest_Takeover_RefusalKeepsCLIAlive(t *testing.T) {
	cases := []struct {
		name    string
		cwd     string
		stop    bool
		wantErr error
		wantMsg string
	}{
		{name: "router stopped", stop: true, wantErr: session.ErrRouterStopped},
		{name: "invalid cwd", cwd: "relative/dir", wantMsg: "takeover cwd invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Skipf("cannot start child: %v", err)
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})
			start, err := discovery.ProcStartTime(cmd.Process.Pid)
			if err != nil || start == 0 {
				t.Skipf("no process start time on this platform: %v", err)
			}

			r := makeRouter()
			if tc.stop {
				r.Shutdown()
			}
			c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(r), nil, nil, Discovery{}, nil)
			params, _ := json.Marshal(map[string]any{
				"pid": cmd.Process.Pid, "session_id": "12345678-1234-1234-1234-123456789012",
				"cwd": tc.cwd, "proc_start_time": start,
			})
			var wg sync.WaitGroup
			_, err = c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "takeover", Params: params}, &wg)
			wg.Wait()
			if err == nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("takeover err = %v, want %v / %q", err, tc.wantErr, tc.wantMsg)
			}

			// A zombie still passes kill(pid, 0), so read how the child died:
			// by this SIGKILL, not by a SIGTERM from the connector.
			_ = cmd.Process.Kill()
			st, err := cmd.Process.Wait()
			if err != nil {
				t.Fatalf("reap child: %v", err)
			}
			if ws, ok := st.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("refused takeover still killed the external CLI: %v", st)
			}
		})
	}
}
