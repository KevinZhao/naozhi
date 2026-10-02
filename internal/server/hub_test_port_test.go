package server

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// newHubForTest is the one place tests call NewHub, and it builds the stack
// the way buildWSStack does: a broadcaster over a fresh registry, an engine
// that notifies it, and a Hub over both. Hub dependencies go in opts and the
// engine-only ones (Guard, Queue, Agents, ProjectMgr, ScratchPool) in eo.
// Router, Resolver, Scheduler and AllowedRoot are shared and come from opts;
// Ctx and Notify are derived here, and so are opts.Engine and opts.Broadcaster.
func newHubForTest(opts HubOptions, eo sendEngineOpts) *Hub {
	if opts.Engine != nil || opts.Broadcaster != nil {
		panic("newHubForTest: the port builds Engine and Broadcaster; leave them unset in opts")
	}
	if eo.Router != nil || eo.Resolver != nil || eo.Scheduler != nil || eo.AllowedRoot != "" || eo.Ctx != nil || eo.Notify != nil {
		panic("newHubForTest: shared and Hub-derived dependencies come from opts; leave them unset in eo")
	}
	bcast := newWSBroadcaster(newSubscriberRegistry())
	eo.Router = opts.Router
	eo.Resolver = opts.Resolver
	eo.Scheduler = opts.Scheduler
	eo.AllowedRoot = opts.AllowedRoot
	eo.Ctx = opts.ParentCtx
	eo.Notify = bcast
	opts.Engine = newSendEngine(eo)
	opts.Broadcaster = bcast
	return NewHub(opts)
}

// TestNewHubForTest_RejectsMisplacedDeps pins the port's split: a dependency
// on the wrong side would reach only one of the Hub and the engine, or be
// overwritten here, so the port refuses it instead.
func TestNewHubForTest_RejectsMisplacedDeps(t *testing.T) {
	t.Parallel()
	// One case per refused field: the two siblings the port builds itself in
	// opts (the engine-only fields no longer exist on HubOptions, so the
	// compiler refuses those), six shared or Hub-derived fields in eo.
	// Dropping any one check from the port fails exactly one case.
	cases := map[string]struct {
		opts HubOptions
		eo   sendEngineOpts
	}{
		"engine in opts":      {opts: HubOptions{Engine: newSendEngine(sendEngineOpts{})}},
		"broadcaster in opts": {opts: HubOptions{Broadcaster: newWSBroadcaster(newSubscriberRegistry())}},
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
