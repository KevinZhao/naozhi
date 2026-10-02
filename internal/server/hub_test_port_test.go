package server

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// newHubForTest is the one place tests call NewHub, and it builds the stack
// the way buildWSStack does: a broadcaster over a fresh registry, an engine
// that notifies it, and a Hub over both. Hub dependencies go in opts and the
// engine-only ones (Guard, Queue, Agents, ProjectMgr, ScratchPool) in eo.
// Shared ones (Router, Resolver, Scheduler, AllowedRoot) come from opts. Like
// buildServer it always wires a queue and a guard (an unset eo.Queue gets a
// collect-mode queue), so sends take production's owner-loop path; the
// cleanup fails the test if one fell through to sessionSendLegacy anyway.
func newHubForTest(t testing.TB, opts HubOptions, eo sendEngineOpts) *Hub {
	t.Helper()
	if opts.Engine != nil || opts.Broadcaster != nil {
		panic("newHubForTest: the port builds Engine and Broadcaster; leave them unset in opts")
	}
	if eo.Router != nil || eo.Resolver != nil || eo.Scheduler != nil || eo.AllowedRoot != "" || eo.Ctx != nil || eo.Notify != nil {
		panic("newHubForTest: shared and Hub-derived dependencies come from opts; leave them unset in eo")
	}
	if eo.Queue == nil {
		eo.Queue = turn.NewQueueWithMode(5, 0, turn.ModeCollect)
	}
	if eo.Guard == nil {
		eo.Guard = session.NewGuard()
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
	hub := NewHub(opts)
	t.Cleanup(func() {
		if n := hub.engine.LegacySendInvokes(); n != 0 {
			t.Errorf("newHubForTest: %d send(s) took sessionSendLegacy; the port wires a queue, so something swapped it out", n)
		}
	})
	return hub
}

// busyForTest makes key look mid-turn to hub's send path: it takes the owner
// slot on the hub's queue, so the next send for key queues behind a turn that
// never runs. The slot is released when the test ends.
func busyForTest(t testing.TB, hub *Hub, key string) {
	t.Helper()
	if isOwner, _, _, _, _ := hub.engine.queue.Enqueue(key, turn.Msg{Text: "busy"}); !isOwner {
		t.Fatalf("busyForTest: %q already has an owner", key)
	}
	t.Cleanup(func() { hub.engine.queue.Discard(key) })
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
			newHubForTest(t, c.opts, c.eo)
		}()
	}
}
