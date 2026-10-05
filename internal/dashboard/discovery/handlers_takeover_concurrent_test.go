package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// leaseRouter adapts a real *session.Router the way the server's adapter does.
type leaseRouter struct{ r *session.Router }

func (a leaseRouter) ReserveTakeover(key string, opts session.AgentOpts) (TakeoverLease, error) {
	lease, err := a.r.ReserveTakeover(key, opts)
	if err != nil {
		return nil, err
	}
	return routerLease{a.r, lease}, nil
}

type routerLease struct {
	r     *session.Router
	lease *session.TakeoverLease
}

func (l routerLease) Takeover(ctx context.Context, sessionID, cwd string) error {
	_, err := l.r.Takeover(ctx, l.lease, sessionID, cwd)
	return err
}

func (l routerLease) Release() { l.lease.Release() }

// startSleeper starts a child that only a signal ends; the returned func
// kills and reaps it and reports the signal it died of, the first one when
// it was dead already.
func startSleeper(t *testing.T) (*exec.Cmd, func() syscall.Signal) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	reaped := make(chan syscall.Signal, 1)
	go func() {
		st, _ := cmd.Process.Wait()
		ws, _ := st.Sys().(syscall.WaitStatus)
		reaped <- ws.Signal()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, func() syscall.Signal {
		_ = cmd.Process.Kill()
		select {
		case sig := <-reaped:
			return sig
		case <-time.After(10 * time.Second):
			t.Fatal("child not reaped")
			return 0
		}
	}
}

func postDiscoveredTakeover(h *Handlers, pid int, sessionID, cwd string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"pid": pid, "session_id": sessionID, "cwd": cwd, "proc_start_time": 100})
	rec := httptest.NewRecorder()
	h.HandleTakeover(rec, httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", bytes.NewReader(body)))
	return rec
}

// TestHandleTakeover_SecondTakeoverOfKeyKeepsItsCLI pins #3417: two external
// CLIs in one cwd map to one key, and the first takeover holds that key from
// before its SIGTERM until its spawn, so the second, arriving while the first
// CLI is still exiting, is refused and its CLI never signalled.
func TestHandleTakeover_SecondTakeoverOfKeyKeepsItsCLI(t *testing.T) {
	const sid1, sid2 = "aaaaaaaa-bbbb-cccc-dddd-000000000001", "aaaaaaaa-bbbb-cccc-dddd-000000000002"
	dir := t.TempDir()
	termed, trapped := filepath.Join(dir, "termed"), filepath.Join(dir, "trapped")
	// Survives SIGTERM, recording it, so the exit wait lasts until appCtx ends.
	cmd1 := exec.Command("sh", "-c", `trap 'echo > "$0"' TERM; echo > "$1"; while :; do sleep 0.05; done`, termed, trapped)
	if err := cmd1.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	exited1 := make(chan struct{})
	go func() { _ = cmd1.Wait(); close(exited1) }()
	t.Cleanup(func() { _ = cmd1.Process.Kill(); <-exited1 })
	testhelper.Eventually(t, func() bool {
		_, err := os.Stat(trapped)
		return err == nil
	}, 5*time.Second, "the child never installed its SIGTERM trap")
	cmd2, reap2 := startSleeper(t)
	cwd := t.TempDir()
	key := session.TakeoverKey(session.SanitizeCWDKey(cwd))

	router := session.NewRouter(session.RouterConfig{MaxProcs: 3})
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(Deps{
		Cache: &fakeCache{snapshot: []discovery.DiscoveredSession{
			{PID: cmd1.Process.Pid, SessionID: sid1, CWD: cwd, ProcStartTime: 100},
			{PID: cmd2.Process.Pid, SessionID: sid2, CWD: cwd, ProcStartTime: 100},
		}},
		NodeAccess:    fakeNodeAccess{},
		ClaudeDir:     t.TempDir(),
		Router:        leaseRouter{router},
		ProcStartTime: func(int) (uint64, error) { return 100, nil },
		AppCtx:        appCtx,
	})

	if rec := postDiscoveredTakeover(h, cmd1.Process.Pid, sid1, cwd); rec.Code != http.StatusAccepted {
		t.Fatalf("first takeover = %d %q, want 202", rec.Code, rec.Body.String())
	}
	testhelper.Eventually(t, func() bool {
		_, err := os.Stat(termed)
		return err == nil
	}, 5*time.Second, "the first takeover's CLI never got its SIGTERM")
	rec := postDiscoveredTakeover(h, cmd2.Process.Pid, sid2, cwd)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "takeover already in progress") {
		t.Fatalf("second takeover during the exit wait = %d %q, want 409 takeover already in progress", rec.Code, rec.Body.String())
	}
	if sig := reap2(); sig != syscall.SIGKILL {
		t.Fatalf("the refused takeover's CLI died of %v, not the test's SIGKILL", sig)
	}
	select {
	case <-exited1:
		t.Fatal("the first takeover's CLI exited before the exit wait ended")
	default:
	}

	// Ends the exit wait; the takeover then fails to spawn (no CLI here) and
	// gives the key back.
	cancel()
	h.Wait()
	lease, err := router.ReserveTakeover(key, session.AgentOpts{})
	if err != nil {
		t.Fatalf("ReserveTakeover after the takeover finished: %v", err)
	}
	lease.Release()
}

// TestHandleTakeover_PidReuseReleasesTheKey: a takeover refused at the SIGTERM
// gives its reservation back, or the key would stay refused until restart.
func TestHandleTakeover_PidReuseReleasesTheKey(t *testing.T) {
	const sid = "aaaaaaaa-bbbb-cccc-dddd-000000000003"
	cmd, reap := startSleeper(t)
	cwd := t.TempDir()
	router := session.NewRouter(session.RouterConfig{MaxProcs: 3})
	h := New(Deps{
		Cache: &fakeCache{snapshot: []discovery.DiscoveredSession{
			{PID: cmd.Process.Pid, SessionID: sid, CWD: cwd, ProcStartTime: 100},
		}},
		NodeAccess: fakeNodeAccess{},
		ClaudeDir:  t.TempDir(),
		Router:     leaseRouter{router},
		// Not the request's start time: the PID was reused.
		ProcStartTime: func(int) (uint64, error) { return 999, nil },
		AppCtx:        context.Background(),
	})

	if rec := postDiscoveredTakeover(h, cmd.Process.Pid, sid, cwd); rec.Code != http.StatusConflict {
		t.Fatalf("takeover of a reused PID = %d %q, want 409", rec.Code, rec.Body.String())
	}
	h.Wait()
	lease, err := router.ReserveTakeover(session.TakeoverKey(session.SanitizeCWDKey(cwd)), session.AgentOpts{})
	if errors.Is(err, session.ErrSpawnInFlight) {
		t.Fatal("the refused takeover kept its reservation of the key")
	}
	if err != nil {
		t.Fatalf("ReserveTakeover: %v", err)
	}
	lease.Release()
	if sig := reap(); sig != syscall.SIGKILL {
		t.Errorf("the reused PID's process died of %v, not the test's SIGKILL", sig)
	}
}

// TestHandleTakeover_TermFailureReleasesTheKey: a SIGTERM that fails for
// another reason than a reused PID (EPERM on a process naozhi may not signal)
// gives the reservation back too.
func TestHandleTakeover_TermFailureReleasesTheKey(t *testing.T) {
	const sid = "aaaaaaaa-bbbb-cccc-dddd-000000000004"
	// PID 1 is the target only where signalling it is refused, so the test
	// never delivers a signal.
	initProc, err := os.FindProcess(1)
	if err != nil || !errors.Is(initProc.Signal(syscall.Signal(0)), syscall.EPERM) {
		t.Skip("needs a PID 1 this user may not signal")
	}
	cwd := t.TempDir()
	router := session.NewRouter(session.RouterConfig{MaxProcs: 3})
	h := New(Deps{
		Cache: &fakeCache{snapshot: []discovery.DiscoveredSession{
			{PID: 1, SessionID: sid, CWD: cwd, ProcStartTime: 100},
		}},
		NodeAccess:    fakeNodeAccess{},
		ClaudeDir:     t.TempDir(),
		Router:        leaseRouter{router},
		ProcStartTime: func(int) (uint64, error) { return 100, nil },
		AppCtx:        context.Background(),
	})

	if rec := postDiscoveredTakeover(h, 1, sid, cwd); rec.Code != http.StatusInternalServerError {
		t.Fatalf("takeover with a refused SIGTERM = %d %q, want 500", rec.Code, rec.Body.String())
	}
	h.Wait()
	lease, err := router.ReserveTakeover(session.TakeoverKey(session.SanitizeCWDKey(cwd)), session.AgentOpts{})
	if err != nil {
		t.Fatalf("ReserveTakeover after the refused SIGTERM: %v", err)
	}
	lease.Release()
}
