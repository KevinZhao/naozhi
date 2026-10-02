package server

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/project"
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
	// One case per refused field: five engine-only fields in opts, six shared
	// or Hub-derived fields in eo. Dropping any one check from the port fails
	// exactly one case.
	cases := map[string]struct {
		opts HubOptions
		eo   sendEngineOpts
	}{
		"guard in opts":       {opts: HubOptions{Guard: session.NewGuard()}},
		"queue in opts":       {opts: HubOptions{Queue: &dispatch.MessageQueue{}}},
		"agents in opts":      {opts: HubOptions{Agents: map[string]session.AgentOpts{}}},
		"projectMgr in opts":  {opts: HubOptions{ProjectMgr: &project.Manager{}}},
		"scratchPool in opts": {opts: HubOptions{ScratchPool: &session.ScratchPool{}}},
		"router in eo":        {eo: sendEngineOpts{Router: &session.Router{}}},
		"resolver in eo":      {eo: sendEngineOpts{Resolver: &session.KeyResolver{}}},
		"scheduler in eo":     {eo: sendEngineOpts{Scheduler: fakeCronSessions{}}},
		"allowedRoot in eo":   {eo: sendEngineOpts{AllowedRoot: "/tmp/nz-root"}},
		"ctx in eo":           {eo: sendEngineOpts{Ctx: context.Background()}},
		"notify in eo":        {eo: sendEngineOpts{Notify: nopNotifier{}}},
	}
	for name, c := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: newHubForTest accepted it", name)
				}
			}()
			newHubForTest(c.opts, c.eo)
		}()
	}
}
