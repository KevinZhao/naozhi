package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/osutil"
)

// TestHandleClose_PidReuseReturns409 verifies the close handler routes the
// SIGTERM through the atomic SendTermVerified path and returns 409 Conflict
// (not 200) when the injected ProcStartTime reports an identity mismatch —
// i.e. the PID was recycled since discovery. The innocent bystander process
// must survive. (#1670)
func TestHandleClose_PidReuseReturns409(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	fc := &fakeCache{snapshot: []discovery.DiscoveredSession{
		{PID: pid, SessionID: "s1", CWD: t.TempDir(), ProcStartTime: 100},
	}}
	h := New(Deps{
		Cache:      fc,
		NodeAccess: fakeNodeAccess{},
		ClaudeDir:  t.TempDir(),
		// Report a start_time that differs from the request's expectation
		// -> simulates PID reuse -> SendTermVerified must refuse + 409.
		ProcStartTime: func(int) (uint64, error) { return 999, nil },
	})
	h.SetAppContext(context.Background())

	body, _ := json.Marshal(map[string]any{
		"pid":             pid,
		"session_id":      "s1",
		"proc_start_time": 100, // != ProcStartTime's 999 -> mismatch
	})
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/close", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleClose(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("HandleClose on PID reuse = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !osutil.PidAlive(pid) {
		t.Fatal("PID-reuse close killed the bystander process; TOCTOU guard failed")
	}
}

// TestHandleClose_MatchingIdentitySucceeds verifies the happy path: when the
// injected ProcStartTime matches the request expectation, the handler signals
// the process and returns 200. (#1670)
func TestHandleClose_MatchingIdentitySucceeds(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	fc := &fakeCache{snapshot: []discovery.DiscoveredSession{
		{PID: pid, SessionID: "s1", CWD: t.TempDir(), ProcStartTime: 100},
	}}
	h := New(Deps{
		Cache:         fc,
		NodeAccess:    fakeNodeAccess{},
		ClaudeDir:     t.TempDir(),
		ProcStartTime: func(int) (uint64, error) { return 100, nil }, // matches
	})
	h.SetAppContext(context.Background())

	body, _ := json.Marshal(map[string]any{
		"pid":             pid,
		"session_id":      "s1",
		"proc_start_time": 100,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/close", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleClose(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HandleClose matching identity = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	h.Wait() // drain background cleanup goroutine
}
