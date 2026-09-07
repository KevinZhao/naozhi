package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	pool := session.NewScratchPool(router, session.DefaultScratchMax, session.DefaultScratchTTL)
	var sched CronView = fakeCronSessions{}

	hub := NewHub(HubOptions{
		Router:      router,
		Guard:       guard,
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

// TestHub_NoPostConstructionSetters is a source-level guardrail that the
// SetScheduler / SetScratchPool / SetUploadStore setters stay deleted so a
// future change cannot silently reintroduce the call-order-vs-Start race they
// caused (#431).
//
// SetUploadStore used to be an intentional exception here: the store's cleanup
// loop had to bind to an app-lifecycle ctx that only existed AFTER the Hub, so
// the store could not be passed to NewHub. #2552 moved appCtx into buildServer,
// which removed the reason for the exception — the store is now built one line
// before the Hub and passed through HubOptions.UploadStore. So the exception
// became a plain #431 violation and the setter is gone.
func TestHub_NoPostConstructionSetters(t *testing.T) {
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
		"func (h *Hub) SetUploadStore(",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("wshub.go reintroduced %q — these deps must be wired via HubOptions, not a post-construction setter (#431)", banned)
		}
	}

	// The store must arrive through HubOptions instead.
	if !strings.Contains(body, "UploadStore *uploadStore") {
		t.Error("wshub.go: HubOptions must carry UploadStore — without it the only way to wire the store " +
			"is a post-construction setter, i.e. the #431 race this test bans")
	}
}
