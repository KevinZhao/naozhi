package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
)

// recordingRouter captures the cwd HandleTakeover passes to Takeover so the
// test can assert it is the same cleaned path WaitAndCleanup operates on.
type recordingRouter struct {
	mu  sync.Mutex
	cwd string
}

func (r *recordingRouter) Takeover(_ context.Context, _, _, cwd string, _ session.AgentOpts) error {
	r.mu.Lock()
	r.cwd = cwd
	r.mu.Unlock()
	return nil
}

func (r *recordingRouter) takeoverCWD() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cwd
}

// TestHandleTakeover_CleanupUsesCleanedCWD verifies the #1786 fix: the
// background WaitAndCleanup goroutine derives its lock-dir slug from the same
// filepath.Clean'd cwd that router.Takeover spawns the session under — not the
// raw req.CWD. Before the fix, a trailing-slash cwd produced a different
// projDirName slug ("...-foo-" vs "...-foo") so the lock dir was never removed.
//
// The test seeds the lock dir at the CLEANED-slug path and asserts the drained
// goroutine removed it. With the old raw-reqCWD code the cleaned-slug dir would
// survive, failing the assertion.
func TestHandleTakeover_CleanupUsesCleanedCWD(t *testing.T) {
	const deadPID = 2147480001
	const sessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Unique cwd under a per-test temp dir so the derived slug cannot collide
	// with any real claude lock dir on this host.
	base := t.TempDir()
	cleanCWD := filepath.Join(base, "proj")
	if err := os.MkdirAll(cleanCWD, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	// Request sends the cwd WITH a trailing slash — the case the bug missed.
	reqCWD := cleanCWD + "/"

	// Pre-create the lock dir that WaitAndCleanup must remove, keyed by the
	// CLEANED cwd's slug (the path the session actually runs under).
	tmpBase := os.TempDir()
	cleanedSlug := discovery.ClaudeProjectSlug(filepath.Clean(reqCWD))
	rawSlug := discovery.ClaudeProjectSlug(reqCWD)
	if cleanedSlug == rawSlug {
		t.Fatalf("test setup invalid: cleaned and raw slugs match (%q); trailing slash should differ", cleanedSlug)
	}
	uidDir := filepath.Join(tmpBase, fmt.Sprintf("claude-%d", os.Getuid()))
	cleanedLockDir := filepath.Join(uidDir, cleanedSlug, sessionID)
	if err := os.MkdirAll(cleanedLockDir, 0o755); err != nil {
		t.Fatalf("mkdir cleaned lock dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(filepath.Join(uidDir, cleanedSlug))
		_ = os.RemoveAll(filepath.Join(uidDir, rawSlug))
	})

	router := &recordingRouter{}
	fc := &fakeCache{snapshot: []discovery.DiscoveredSession{
		{PID: deadPID, SessionID: sessionID, CWD: cleanCWD, ProcStartTime: 1},
	}}
	h := New(Deps{
		Cache:      fc,
		NodeAccess: fakeNodeAccess{},
		ClaudeDir:  t.TempDir(),
		Router:     router,
		// PID is not alive, so the identity check/SIGTERM short-circuit and
		// the goroutine proceeds straight to cleanup + takeover.
		ProcStartTime: func(int) (uint64, error) { return 1, nil },
	})
	h.SetAppContext(context.Background())

	body, _ := json.Marshal(map[string]any{
		"pid":             deadPID,
		"session_id":      sessionID,
		"cwd":             reqCWD,
		"proc_start_time": 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleTakeover(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("HandleTakeover status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	// Drain the background goroutine (cleanup + Takeover).
	done := make(chan struct{})
	go func() { h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait() did not drain background takeover goroutine")
	}

	// The cleaned-slug lock dir must be gone — proving WaitAndCleanup used the
	// cleaned cwd, matching the session key path.
	if _, err := os.Stat(cleanedLockDir); !os.IsNotExist(err) {
		t.Fatalf("cleaned-slug lock dir still present after cleanup (stat err=%v); WaitAndCleanup used the wrong cwd", err)
	}

	// And the cwd handed to router.Takeover must equal the cleaned cwd, i.e.
	// the same path basis the cleanup used (single source of truth).
	wantClean := filepath.Clean(reqCWD)
	if got := router.takeoverCWD(); got != wantClean {
		t.Fatalf("router.Takeover cwd = %q, want cleaned %q", got, wantClean)
	}
}
