package dispatch

import "time"

// NewMessageQueue is a test-only convenience constructor that mints a
// MessageQueue in ModeCollect — the production default. Production code
// must construct via NewMessageQueueWithMode so the queue mode is explicit
// at every call site (Collect / Interrupt / Passthrough each have very
// different latency / cost profiles, and a wrapper that hides the mode
// makes mode-related regressions invisible at review time).
//
// Lives in a *_test.go file so the linker excludes it from the production
// binary; #1205 (DEADCODE-12) flagged 25+ test call sites all using
// "NewMessageQueue(5, 0)" — the wrapper reads better than the explicit
// 3-arg form for tests but had zero production callers.
//
// Cross-package test callers (internal/server) must use the explicit
// dispatch.NewMessageQueueWithMode form — Go disallows importing test
// helpers from another package's _test.go scope.
func NewMessageQueue(maxDepth int, collectDelay time.Duration) *MessageQueue {
	return NewMessageQueueWithMode(maxDepth, collectDelay, ModeCollect)
}
