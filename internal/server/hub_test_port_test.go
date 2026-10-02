package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// newHubForTest is the one place tests call NewHub. Hub dependencies go in
// opts and the engine-only ones (Guard, Queue, Agents, ProjectMgr,
// ScratchPool) in eo. Router, Resolver, Scheduler and AllowedRoot are shared
// and come from opts; Ctx and Notify are derived from the Hub. Today the body
// copies eo into HubOptions and calls NewHub, so behaviour is unchanged; when
// the composition root builds the engine as a sibling, only this body moves.
func newHubForTest(opts HubOptions, eo sendEngineOpts) *Hub {
	if opts.Guard != nil || opts.Queue != nil || opts.Agents != nil || opts.ProjectMgr != nil || opts.ScratchPool != nil {
		panic("newHubForTest: engine-only dependencies belong in eo, not opts")
	}
	if eo.Router != nil || eo.Resolver != nil || eo.Scheduler != nil || eo.AllowedRoot != "" || eo.Ctx != nil || eo.Notify != nil {
		panic("newHubForTest: shared and Hub-derived dependencies come from opts; leave them unset in eo")
	}
	opts.Guard = eo.Guard
	opts.Queue = eo.Queue
	opts.Agents = eo.Agents
	opts.ProjectMgr = eo.ProjectMgr
	opts.ScratchPool = eo.ScratchPool
	return NewHub(opts)
}

// TestNewHubForTest_RejectsMisplacedDeps pins the port's split: a dependency
// on the wrong side would reach only one of the Hub and the engine, or be
// overwritten here, so the port refuses it instead.
func TestNewHubForTest_RejectsMisplacedDeps(t *testing.T) {
	t.Parallel()
	cases := map[string]func(){
		"guard in opts":     func() { newHubForTest(HubOptions{Guard: session.NewGuard()}, sendEngineOpts{}) },
		"allowedRoot in eo": func() { newHubForTest(HubOptions{}, sendEngineOpts{AllowedRoot: "/tmp/nz-root"}) },
		"notify in eo":      func() { newHubForTest(HubOptions{}, sendEngineOpts{Notify: nopNotifier{}}) },
	}
	for name, build := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: newHubForTest accepted it", name)
				}
			}()
			build()
		}()
	}
}
