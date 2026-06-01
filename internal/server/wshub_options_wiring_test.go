package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestNewHub_SchedulerAndScratchPoolFromOptions pins R176-ARCH-M3 (#431):
// Scheduler and ScratchPool must be wired into the Hub at construction via
// HubOptions, not through post-construction SetX setters whose call order
// relative to Hub.Start() was a hidden invariant and recurring race source.
func TestNewHub_SchedulerAndScratchPoolFromOptions(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	pool := session.NewScratchPool(router, session.DefaultScratchMax, session.DefaultScratchTTL)
	var sched CronView = fakeCronSessions{}

	hub := NewHub(HubOptions{
		Router:      router,
		Guard:       guard,
		NodesMu:     &nodesMu,
		Scheduler:   sched,
		ScratchPool: pool,
	})

	if hub.scheduler == nil {
		t.Fatal("hub.scheduler nil — HubOptions.Scheduler not wired at construction (#431)")
	}
	if hub.scratchPool != pool {
		t.Fatal("hub.scratchPool not set from HubOptions.ScratchPool (#431)")
	}
}

// TestHub_NoSchedulerOrScratchPoolSetters is a source-level guardrail that the
// SetScheduler / SetScratchPool setters stay deleted so a future change cannot
// silently reintroduce the call-order-vs-Start race they caused (#431). The
// upload-store setter is intentionally retained (its cleanup loop binds to the
// app-lifecycle ctx created after the Hub) and is asserted present so the test
// also documents why that one survives.
func TestHub_NoSchedulerOrScratchPoolSetters(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(self), "wshub.go")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}
	body := string(raw)

	for _, banned := range []string{
		"func (h *Hub) SetScheduler(",
		"func (h *Hub) SetScratchPool(",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("wshub.go reintroduced %q — these deps must be wired via HubOptions, not a post-construction setter (#431)", banned)
		}
	}

	if !strings.Contains(body, "func (h *Hub) SetUploadStore(") {
		t.Error("wshub.go: SetUploadStore must remain (its cleanup loop binds to the post-Hub app ctx, R215-ARCH-P2-3 #579)")
	}
}
