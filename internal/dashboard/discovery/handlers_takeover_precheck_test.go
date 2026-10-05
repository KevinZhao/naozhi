package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
)

// precheckRouter refuses every takeover with err and records what reached it.
type precheckRouter struct {
	err       error
	mu        sync.Mutex
	checked   []string
	takeovers int
}

func (r *precheckRouter) TakeoverPrecheck(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked = append(r.checked, key)
	return r.err
}

func (r *precheckRouter) Takeover(context.Context, string, string, string, session.AgentOpts) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.takeovers++
	return nil
}

// TestHandleTakeover_PrecheckRefusalKeepsCLIAlive pins #3315: a takeover the
// router refuses up front is answered before SIGTERM, so the external CLI,
// its discovered card and the router are all left alone.
func TestHandleTakeover_PrecheckRefusalKeepsCLIAlive(t *testing.T) {
	const sessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"max procs", fmt.Errorf("%w (3), all busy", session.ErrMaxProcs), http.StatusServiceUnavailable, "takeover refused: max concurrent processes reached"},
		{"spawn in flight", session.ErrSpawnInFlight, http.StatusConflict, "takeover already in progress"},
		{"router stopped", session.ErrRouterStopped, http.StatusServiceUnavailable, "takeover refused: router is shutting down"},
		{"unknown", errors.New("boom"), http.StatusServiceUnavailable, "takeover unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Skipf("cannot start child: %v", err)
			}
			pid := cmd.Process.Pid
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})

			cwd := t.TempDir()
			router := &precheckRouter{err: tc.err}
			fc := &fakeCache{snapshot: []discovery.DiscoveredSession{
				{PID: pid, SessionID: sessionID, CWD: cwd, ProcStartTime: 100},
			}}
			var startTimeReads atomic.Int32
			h := New(Deps{
				Cache:      fc,
				NodeAccess: fakeNodeAccess{},
				ClaudeDir:  t.TempDir(),
				Router:     router,
				// Matches the request, so only the precheck stands between
				// this request and a real SIGTERM of the child.
				ProcStartTime: func(int) (uint64, error) { startTimeReads.Add(1); return 100, nil },
				AppCtx:        context.Background(),
			})

			body, _ := json.Marshal(map[string]any{
				"pid": pid, "session_id": sessionID, "cwd": cwd, "proc_start_time": 100,
			})
			rec := httptest.NewRecorder()
			h.HandleTakeover(rec, httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", bytes.NewReader(body)))
			h.Wait()

			if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("HandleTakeover = %d %q, want %d containing %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
			// A zombie still passes kill(pid, 0), so read how the child died:
			// by this SIGKILL, not by a SIGTERM from the handler.
			_ = cmd.Process.Kill()
			st, err := cmd.Process.Wait()
			if err != nil {
				t.Fatalf("reap child: %v", err)
			}
			if ws, ok := st.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("refused takeover still killed the external CLI: %v", st)
			}
			if n := startTimeReads.Load(); n != 0 {
				t.Fatalf("identity read %d times: the SIGTERM path ran", n)
			}
			if n := atomic.LoadInt32(&fc.evicted); n != 0 {
				t.Fatalf("EvictPID called %d times; the card must stay", n)
			}
			wantKey := session.TakeoverKey(session.SanitizeCWDKey(cwd))
			router.mu.Lock()
			defer router.mu.Unlock()
			if len(router.checked) != 1 || router.checked[0] != wantKey {
				t.Fatalf("precheck keys = %q, want [%q]", router.checked, wantKey)
			}
			if router.takeovers != 0 {
				t.Fatalf("Takeover ran %d times after a refused precheck", router.takeovers)
			}
		})
	}
}
