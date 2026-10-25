package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestNewHub_SchedulerAndScratchPoolFromOptions pins R176-ARCH-M3 (#431):
// Scheduler and ScratchPool must be wired at construction (HubOptions and
// sendEngineOpts), not through post-construction SetX setters whose call order
// relative to Hub.Start() was a hidden invariant and recurring race source.
func TestNewHub_SchedulerAndScratchPoolFromOptions(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	pool := session.NewScratchPool(router, session.DefaultScratchMax, session.DefaultScratchTTL)
	var sched CronView = fakeCronSessions{}

	hub := newHubForTest(t, HubOptions{
		Router:    router,
		Scheduler: sched,
	}, sendEngineOpts{ScratchPool: pool})

	if hub.scheduler == nil {
		t.Fatal("hub.scheduler nil — HubOptions.Scheduler not wired at construction (#431)")
	}
	if hub.engine.scratchPool != pool {
		t.Fatal("the send engine's scratchPool is not sendEngineOpts.ScratchPool (#431)")
	}
}
