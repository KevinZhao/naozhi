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
// ScratchPool, AllowedRoot) in eo. Shared ones (Router, Resolver, Scheduler)
// come from opts; HubOptions.AllowedRoot is the tailer's projects root, a
// different root from the engine's workspace one, as in buildWSStack. Like buildServer it always wires the Orchestrator, over a
// collect-mode queue, so sends take production's turn path.
func newHubForTest(t testing.TB, opts HubOptions, eo sendEngineOpts) *Hub {
	t.Helper()
	if opts.Engine != nil || opts.Broadcaster != nil {
		panic("newHubForTest: the port builds Engine and Broadcaster; leave them unset in opts")
	}
	if eo.Router != nil || eo.Resolver != nil || eo.Ctx != nil || eo.Notify != nil || eo.Turns != nil {
		panic("newHubForTest: shared and Hub-derived dependencies come from opts; leave them unset in eo")
	}
	// Like buildWSStack, one concrete router feeds the Hub, turnSender and
	// the engine; a fake HubRouter would reach only the Hub.
	concrete, ok := opts.Router.(*session.Router)
	if opts.Router != nil && !ok {
		panic("newHubForTest: opts.Router must be a *session.Router")
	}
	bcast := newWSBroadcaster(newSubscriberRegistry())
	var router turnRouter
	if concrete != nil {
		router = concrete
	}
	var prompts cronPromptSaver
	if opts.Scheduler != nil {
		prompts = opts.Scheduler
	}
	eo.Turns = turn.New(turn.QueueOptions{MaxDepth: 5}, turnSender{router: router, notify: bcast, prompts: prompts})
	eo.Router = concrete
	eo.Resolver = opts.Resolver
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
