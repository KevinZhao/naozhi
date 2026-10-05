package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
)

// outcomeRouter fails or succeeds Takeover with err; a non-nil gate holds
// Takeover until it is closed.
type outcomeRouter struct {
	err  error
	gate chan struct{}
}

func (r *outcomeRouter) TakeoverPrecheck(string) error { return nil }

func (r *outcomeRouter) Takeover(context.Context, string, string, string, session.AgentOpts) error {
	if r.gate != nil {
		<-r.gate
	}
	return r.err
}

const statusTestPID = 2147480002 // not alive: SIGTERM and the exit wait short-circuit

func newStatusHandlers(t *testing.T, router SessionRouter, sessionIDs ...string) *Handlers {
	t.Helper()
	var snap []discovery.DiscoveredSession
	for _, id := range sessionIDs {
		snap = append(snap, discovery.DiscoveredSession{PID: statusTestPID, SessionID: id, ProcStartTime: 1})
	}
	return New(Deps{
		Cache:         &fakeCache{snapshot: snap},
		NodeAccess:    fakeNodeAccess{},
		ClaudeDir:     t.TempDir(),
		Router:        router,
		ProcStartTime: func(int) (uint64, error) { return 1, nil },
		AppCtx:        context.Background(),
	})
}

// takeoverID runs HandleTakeover and returns the takeover_id of its 202.
func takeoverID(h *Handlers, sessionID, cwd string) (string, error) {
	body, _ := json.Marshal(map[string]any{"pid": statusTestPID, "session_id": sessionID, "cwd": cwd, "proc_start_time": 1})
	rec := httptest.NewRecorder()
	h.HandleTakeover(rec, httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", bytes.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		return "", fmt.Errorf("HandleTakeover = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("decode 202 body: %v", err)
	}
	if id := resp["takeover_id"]; validTakeoverID(id) {
		return id, nil
	}
	return "", fmt.Errorf("takeover_id = %q, want 32 lowercase hex characters", resp["takeover_id"])
}

func postTakeover(t *testing.T, h *Handlers, sessionID, cwd string) string {
	t.Helper()
	id, err := takeoverID(h, sessionID, cwd)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// getStatus returns the status code and raw body of the status endpoint.
func getStatus(h *Handlers, id string) (int, string) {
	rec := httptest.NewRecorder()
	h.HandleTakeoverStatus(rec, httptest.NewRequest(http.MethodGet, "/api/discovered/takeover/status?id="+id, nil))
	return rec.Code, rec.Body.String()
}

func wantOutcome(t *testing.T, h *Handlers, id, state, class string) {
	t.Helper()
	code, body := getStatus(h, id)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode status body %q: %v", body, err)
	}
	if got["state"] != state || got["class"] != class {
		t.Fatalf("status = %s, want state=%q class=%q", body, state, class)
	}
}

func TestTakeoverStatus_BackgroundOutcome(t *testing.T) {
	const sessionID = "aaaaaaaa-bbbb-cccc-dddd-000000000001"
	for _, c := range []struct {
		name         string
		err          error
		state, class string
	}{
		{"max procs", fmt.Errorf("%w (3), all busy", session.ErrMaxProcs), takeoverFailed, "max_procs"},
		{"raced", fmt.Errorf("takeover %q: %w", "/secret/path", session.ErrTakeoverRaced), takeoverFailed, "in_progress"},
		{"spawn error", errors.New("exec /secret/path/claude: permission denied"), takeoverFailed, "spawn_failed"},
		{"success", nil, takeoverReady, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newStatusHandlers(t, &outcomeRouter{err: c.err}, sessionID)
			id := postTakeover(t, h, sessionID, t.TempDir())
			h.Wait()
			wantOutcome(t, h, id, c.state, c.class)
			if _, body := getStatus(h, id); strings.Contains(body, "secret") {
				t.Fatalf("status body leaks the error text: %s", body)
			}
		})
	}
}

func TestTakeoverStatus_PendingUntilTakeoverReturns(t *testing.T) {
	const sessionID = "aaaaaaaa-bbbb-cccc-dddd-000000000002"
	gate := make(chan struct{})
	h := newStatusHandlers(t, &outcomeRouter{err: session.ErrShimStuck, gate: gate}, sessionID)
	id := postTakeover(t, h, sessionID, t.TempDir())
	wantOutcome(t, h, id, takeoverPending, "")
	close(gate)
	h.Wait()
	wantOutcome(t, h, id, takeoverFailed, "shim_stuck")
}

func TestTakeoverStatus_UnknownAndMalformedIDs(t *testing.T) {
	h := newStatusHandlers(t, &outcomeRouter{})
	wantOutcome(t, h, strings.Repeat("ab", 16), takeoverUnknown, "")
	for _, id := range []string{"", "abc", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32), strings.Repeat("g", 32)} {
		if code, body := getStatus(h, id); code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want 400; body=%s", id, code, body)
		}
	}
}

func TestTakeoverTracker_TTLAndCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tr := newTakeoverTracker()
	tr.now = func() time.Time { return now }

	expired := tr.begin()
	tr.finish(expired, nil)
	now = now.Add(takeoverOutcomeTTL)
	if got := tr.lookup(expired).State; got != takeoverReady {
		t.Fatalf("at exactly the TTL: state = %q, want ready", got)
	}
	now = now.Add(time.Second)
	if got := tr.lookup(expired).State; got != takeoverUnknown {
		t.Fatalf("past the TTL: state = %q, want unknown", got)
	}
	tr.begin()
	if n := tr.size(); n != 1 {
		t.Fatalf("after a write past the TTL the tracker holds %d entries, want 1 (expired one pruned)", n)
	}

	ids := make([]string, takeoverOutcomeCap+1)
	for i := range ids {
		now = now.Add(time.Millisecond)
		ids[i] = tr.begin()
	}
	if got := tr.lookup(ids[0]).State; got != takeoverUnknown {
		t.Fatalf("oldest of %d entries: state = %q, want unknown (evicted)", len(ids), got)
	}
	for _, id := range ids[1:] {
		if got := tr.lookup(id).State; got != takeoverPending {
			t.Fatalf("entry %s: state = %q, want pending", id, got)
		}
	}
	if n := tr.size(); n != takeoverOutcomeCap {
		t.Fatalf("tracker holds %d entries, want %d (oldest evicted)", n, takeoverOutcomeCap)
	}
}

func (t *takeoverTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

func TestClassifyTakeoverErr(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w (3), all busy", session.ErrMaxProcs), "max_procs"},
		{session.ErrSpawnInFlight, "in_progress"},
		{fmt.Errorf("x: %w", session.ErrTakeoverRaced), "in_progress"},
		{session.ErrRouterStopped, "shutting_down"},
		{fmt.Errorf("spawn: %w", context.Canceled), "shutting_down"},
		{fmt.Errorf("spawn: %w", session.ErrCLIStartupFailed), "startup_failed"},
		{session.ErrShimStuck, "shim_stuck"},
		{errors.New("boom"), "spawn_failed"},
	} {
		if got := classifyTakeoverErr(c.err); got != c.want {
			t.Errorf("classifyTakeoverErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Parallel takeovers and status reads share the tracker; run under -race.
func TestTakeoverStatus_ConcurrentAttempts(t *testing.T) {
	const n = 16
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("aaaaaaaa-bbbb-cccc-dddd-%012d", i)
	}
	h := newStatusHandlers(t, &outcomeRouter{err: session.ErrMaxProcs}, ids...)
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := takeoverID(h, ids[i], t.TempDir())
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = id
			for range 5 {
				getStatus(h, got[i])
			}
		}()
	}
	wg.Wait()
	h.Wait()
	for _, id := range got {
		wantOutcome(t, h, id, takeoverFailed, "max_procs")
	}
}
