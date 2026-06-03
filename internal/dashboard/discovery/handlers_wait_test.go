package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
)

// fakeCache is a minimal CacheView for exercising HandleClose's background
// goroutine lifecycle without a real discovery scanner.
type fakeCache struct {
	snapshot []discovery.DiscoveredSession
	evicted  int32
}

func (f *fakeCache) Snapshot() []discovery.DiscoveredSession { return f.snapshot }
func (f *fakeCache) EvictPID(int)                            { atomic.AddInt32(&f.evicted, 1) }

// fakeNodeAccess reports no nodes so HandleClose stays on the local path.
type fakeNodeAccess struct{}

func (fakeNodeAccess) HasNodes() bool { return false }
func (fakeNodeAccess) LookupNode(http.ResponseWriter, string) (node.Conn, bool) {
	panic("not used")
}

// TestWaitIdleReturnsImmediately verifies Wait() does not block when no
// background goroutine has been launched.
func TestWaitIdleReturnsImmediately(t *testing.T) {
	h := New(Deps{})
	done := make(chan struct{})
	go func() { h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait() blocked while idle")
	}
}

// TestHandleCloseDrainsViaWait verifies the close handler tracks its
// background WaitAndCleanup goroutine in the WaitGroup so Wait() blocks
// until it exits (graceful-shutdown drain guarantee, #1662).
func TestHandleCloseDrainsViaWait(t *testing.T) {
	// Use a PID that is essentially never alive so WaitAndCleanup's
	// FindProcess/exit wait returns quickly. PidAlive is false -> the
	// handler skips identity check and signalling, just spawns cleanup.
	const deadPID = 2147480000
	fc := &fakeCache{snapshot: []discovery.DiscoveredSession{
		{PID: deadPID, SessionID: "s1", CWD: t.TempDir(), ProcStartTime: 1},
	}}
	h := New(Deps{
		Cache:      fc,
		NodeAccess: fakeNodeAccess{},
		ClaudeDir:  t.TempDir(),
	})
	h.SetAppContext(context.Background())

	body, _ := json.Marshal(map[string]any{
		"pid":             deadPID,
		"session_id":      "s1",
		"cwd":             "/tmp",
		"proc_start_time": 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/close", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleClose(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HandleClose status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Wait() must return once the spawned goroutine finishes.
	done := make(chan struct{})
	go func() { h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait() did not drain background close goroutine")
	}
}
