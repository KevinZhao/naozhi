// Package session test utilities for cross-package consumers.
//
// IMPORTANT (R226-CR-14 / R227-CR-2 / R230-CQ-5 / R232-ARCH-4): this file
// is named testutil.go (not testutil_test.go) on purpose — Go only
// compiles *_test.go files when the *enclosing* package is being tested,
// so types/functions defined there are NOT visible to other packages'
// tests. This package's TestProcess + Router.InjectSession are consumed
// by internal/server/*_test.go and internal/upstream/*_test.go, which
// forces the cross-package-visible "testutil.go" naming.
//
// Side effect: this file ships in the production binary. The TestProcess
// type and NewTestProcess constructor are dead code at runtime — nothing
// reachable from cmd/naozhi/main.go calls them — so the cost is binary
// size, not behaviour. Router.InjectSession is the one risky export: a
// crafted plugin that imports session and calls it could plant a fake
// process. naozhi has no plugin system, so the attack surface is empty
// in practice; if one is ever added, gate this file with a build tag
// and split the cross-package fake into internal/session/sessiontest.
//
// Migration note: a follow-up subpackage carve-out is the canonical fix.
// The blocker is `Router.InjectSession`, which touches r.mu / r.sessions
// / r.attachHistorySource (all unexported). A clean split would either
// export a narrow seam (e.g. `Router.injectForTest(processIface)` +
// matching helper in sessiontest) or move the InjectSession glue here
// and ship TestProcess from the subpackage. Defer until a build-tag
// boundary is needed for unrelated reasons.
package session

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestProcess is a mock processIface for use in tests outside the session package.
type TestProcess struct {
	EventLog       *cli.EventLog
	StateVal       cli.ProcessState
	AliveVal       bool
	DeathReasonVal string
	SendFunc       func(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
}

// NewTestProcess creates a TestProcess with an event log and ready state.
func NewTestProcess() *TestProcess {
	return &TestProcess{
		EventLog: cli.NewEventLog(0),
		StateVal: cli.StateReady,
		AliveVal: true,
	}
}

func (p *TestProcess) Alive() bool                { return p.AliveVal }
func (p *TestProcess) IsRunning() bool            { return p.StateVal == cli.StateRunning }
func (p *TestProcess) Close()                     { p.AliveVal = false; p.StateVal = cli.StateDead }
func (p *TestProcess) Kill()                      { p.AliveVal = false; p.StateVal = cli.StateDead }
func (p *TestProcess) Interrupt()                 {}
func (p *TestProcess) InterruptViaControl() error { return nil }

func (p *TestProcess) Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
	if p.SendFunc != nil {
		return p.SendFunc(ctx, text, images, onEvent)
	}
	return &cli.SendResult{Text: "mock response"}, nil
}

// SendPassthrough mirrors Send for tests that don't care about passthrough
// semantics. Ignores priority; returns the same mock result as Send.
func (p *TestProcess) SendPassthrough(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback, priority string) (*cli.SendResult, error) {
	return p.Send(ctx, text, images, onEvent)
}

// DiscardPassthroughPending is a no-op on the test stub — there are no real
// pending slots to flush.
func (p *TestProcess) DiscardPassthroughPending(reason error) {}

// PassthroughDepth always reports 0 on the test stub.
func (p *TestProcess) PassthroughDepth() int { return 0 }

// SupportsPassthrough defaults to false so tests that don't opt in use the
// legacy Send path. A test wanting to exercise passthrough can assign a
// TestProcess whose wrapper overrides this (or supply a real *cli.Process).
func (p *TestProcess) SupportsPassthrough() bool { return false }

func (p *TestProcess) GetSessionID() string              { return "" }
func (p *TestProcess) GetState() cli.ProcessState        { return p.StateVal }
func (p *TestProcess) DeathReason() string               { return p.DeathReasonVal }
func (p *TestProcess) TotalCost() float64                { return 0 }
func (p *TestProcess) EventEntries() []cli.EventEntry    { return p.EventLog.Entries() }
func (p *TestProcess) EventLastN(n int) []cli.EventEntry { return p.EventLog.LastN(n) }
func (p *TestProcess) EventEntriesSince(afterMS int64) []cli.EventEntry {
	return p.EventLog.EntriesSince(afterMS)
}
func (p *TestProcess) EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry {
	return p.EventLog.EntriesBefore(beforeMS, limit)
}
func (p *TestProcess) LastActivitySummary() string                { return p.EventLog.LastActivitySummary() }
func (p *TestProcess) LastEventAt() time.Time                     { return p.EventLog.LastEventAt() }
func (p *TestProcess) UserTurnCount() int64                       { return p.EventLog.UserTurnCount() }
func (p *TestProcess) ProtocolName() string                       { return "test" }
func (p *TestProcess) SubscribeEvents() (<-chan struct{}, func()) { return p.EventLog.Subscribe() }
func (p *TestProcess) PID() int                                   { return 0 }
func (p *TestProcess) InjectHistory(entries []cli.EventEntry) {
	for _, e := range entries {
		p.EventLog.Append(e)
	}
}
func (p *TestProcess) TurnAgents() []cli.SubagentInfo { return p.EventLog.TurnAgents() }

// Normalize-layer stubs (docs/rfc/multi-backend.md §8.8). Tests don't drive
// metadata through TestProcess; zero values match the pre-multi-backend
// SessionSnapshot output so existing assertions continue to pass.
func (p *TestProcess) ContextUsagePercent() float64       { return 0 }
func (p *TestProcess) TurnDurationMs() int64              { return 0 }
func (p *TestProcess) MeteringUsage() []cli.MeteringEntry { return nil }
func (p *TestProcess) Model() string                      { return "" }

// InjectSession inserts a session with the given TestProcess into the router.
// For use in tests that need sessions without spawning real CLI processes.
func (r *Router) InjectSession(key string, proc *TestProcess) *ManagedSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &ManagedSession{
		key: key,
	}
	s.storeProcess(proc)
	s.touchLastActive()
	r.attachHistorySource(s)
	r.sessions[key] = s
	r.activeCount.Add(1)
	return s
}
