package server

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// newHubForTest is the one place tests call NewHub, and it builds the stack
// the way buildWSStack does: a broadcaster over a fresh registry, a
// turn.Orchestrator whose turnSender notifies it, an engine that submits to
// that Orchestrator, and a Hub over the engine and the broadcaster. Hub
// dependencies go in opts and the engine-only ones (Agents, ProjectMgr,
// ScratchPool) in eo. Shared ones (Router, Resolver, Scheduler, AllowedRoot)
// come from opts. Like buildServer it always wires the Orchestrator, over a
// collect-mode queue, so sends take production's turn path.
func newHubForTest(t testing.TB, opts HubOptions, eo sendEngineOpts) *Hub {
	t.Helper()
	if opts.Engine != nil || opts.Broadcaster != nil {
		panic("newHubForTest: the port builds Engine and Broadcaster; leave them unset in opts")
	}
	if eo.Router != nil || eo.Resolver != nil || eo.AllowedRoot != "" || eo.Ctx != nil || eo.Notify != nil || eo.Turns != nil {
		panic("newHubForTest: shared and Hub-derived dependencies come from opts; leave them unset in eo")
	}
	bcast := newWSBroadcaster(newSubscriberRegistry())
	var router turnRouter
	if opts.Router != nil {
		router = opts.Router
	}
	var prompts cronPromptSaver
	if opts.Scheduler != nil {
		prompts = opts.Scheduler
	}
	eo.Turns = turn.New(turn.QueueOptions{MaxDepth: 5}, turnSender{router: router, notify: bcast, prompts: prompts})
	eo.Router = opts.Router
	eo.Resolver = opts.Resolver
	eo.AllowedRoot = opts.AllowedRoot
	eo.Ctx = opts.ParentCtx
	eo.Notify = bcast
	opts.Engine = newSendEngine(eo)
	opts.Broadcaster = bcast
	return NewHub(opts)
}

// busyForTest makes key look mid-turn to hub's send path: it takes the owner
// slot on the hub's queue with a turn that never runs, so the next send for
// key queues behind it. The slot is released when the test ends.
func busyForTest(t testing.TB, hub *Hub, key string) {
	t.Helper()
	if ack := hub.engine.turns.Submit(context.Background(), turn.Request{Key: key, Text: "busy"}, neverRunAdmission{}); ack != turn.AckOwner {
		t.Fatalf("busyForTest: %q already has an owner (ack %d)", key, ack)
	}
	t.Cleanup(func() { hub.engine.turns.Retire(context.Background(), key) })
}

// neverRunAdmission admits a run and never starts it.
type neverRunAdmission struct{}

func (neverRunAdmission) Admit(turn.RunKind) (func(fn func(ctx context.Context)), bool) {
	return func(func(ctx context.Context)) {}, true
}

// TestNewHubForTest_RejectsMisplacedDeps pins the port's split: a dependency
// on the wrong side would reach only one of the Hub and the engine, or be
// overwritten here, so the port refuses it instead.
func TestNewHubForTest_RejectsMisplacedDeps(t *testing.T) {
	t.Parallel()
	// One case per refused field: the two siblings the port builds itself in
	// opts (the engine-only fields no longer exist on HubOptions, so the
	// compiler refuses those), six shared or port-built fields in eo.
	// Dropping any one check from the port fails exactly one case.
	cases := map[string]struct {
		opts HubOptions
		eo   sendEngineOpts
	}{
		"engine in opts":      {opts: HubOptions{Engine: newSendEngine(sendEngineOpts{})}},
		"broadcaster in opts": {opts: HubOptions{Broadcaster: newWSBroadcaster(newSubscriberRegistry())}},
		"router in eo":        {eo: sendEngineOpts{Router: &session.Router{}}},
		"resolver in eo":      {eo: sendEngineOpts{Resolver: &session.KeyResolver{}}},
		"allowedRoot in eo":   {eo: sendEngineOpts{AllowedRoot: "/tmp/nz-root"}},
		"ctx in eo":           {eo: sendEngineOpts{Ctx: context.Background()}},
		"notify in eo":        {eo: sendEngineOpts{Notify: nopNotifier{}}},
		"turns in eo":         {eo: sendEngineOpts{Turns: turn.New(turn.QueueOptions{MaxDepth: 1}, turnSender{})}},
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
