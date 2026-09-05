//go:build !release

// Package session test utilities for cross-package consumers.
//
// This file is named testutil.go (not *_test.go) on purpose: Go only compiles
// *_test.go files for the enclosing package's own tests, and TestProcess +
// Router.InjectSession are consumed by internal/server and internal/upstream
// tests. The `//go:build !release` constraint keeps it out of a binary built
// with `-tags release`; a release build fails at link time if anything
// reachable from cmd/naozhi ever references these symbols. A subpackage
// carve-out is blocked on Router.InjectSession touching unexported state.
package session

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestProcess is a mock processIface for use in tests outside the session package.
type TestProcess struct {
	EventLog       *cli.EventLog
	StateVal       cli.ProcessState
	AliveVal       bool
	DeathReasonVal string
	// ModelVal lets snapshot tests drive proc.Model() without subclassing.
	ModelVal string
	// LiveVersionVal lets snapshot tests drive proc.LiveVersion() — the
	// CLI binary version self-reported in system/init.
	LiveVersionVal string
	// EffortVal lets snapshot tests drive proc.Effort() — the backend-reported
	// thinking-effort tier.
	EffortVal string
	// MeteringVal lets cost tests drive proc.MeteringUsage() (the process-level
	// running sum kiro/codex report).
	MeteringVal []cli.MeteringEntry
	// ShadowVal is returned once by TakeShadowUsage (partial-turn accounting).
	ShadowVal cli.ShadowUsage
	SendFunc  func(ctx context.Context, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error)
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

func (p *TestProcess) Send(ctx context.Context, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error) {
	if p.SendFunc != nil {
		return p.SendFunc(ctx, text, images, onEvent)
	}
	return &cli.SendResult{Text: "mock response"}, nil
}

// SendPassthrough mirrors Send for tests that don't care about passthrough
// semantics. Ignores priority; returns the same mock result as Send.
func (p *TestProcess) SendPassthrough(ctx context.Context, text string, images []cli.Attachment, onEvent cli.EventCallback, priority string) (*cli.SendResult, error) {
	return p.Send(ctx, text, images, onEvent)
}

// DiscardPassthroughPending is a no-op on the test stub — there are no real
// pending slots to flush.
func (p *TestProcess) DiscardPassthroughPending(reason error) {}

// PassthroughDepth always reports 0 on the test stub.
func (p *TestProcess) PassthroughDepth() int { return 0 }

// SupportsPassthrough defaults to false so tests that don't opt in use the
// legacy Send path; wrap TestProcess or use a real *cli.Process to exercise it.
func (p *TestProcess) SupportsPassthrough() bool { return false }

func (p *TestProcess) SessionID() string                      { return "" }
func (p *TestProcess) State() cli.ProcessState                { return p.StateVal }
func (p *TestProcess) DeathReason() string                    { return p.DeathReasonVal }
func (p *TestProcess) TotalCost() float64                     { return 0 }
func (p *TestProcess) EventEntries() []clievent.EventEntry    { return p.EventLog.Entries() }
func (p *TestProcess) EventLastN(n int) []clievent.EventEntry { return p.EventLog.LastN(n) }
func (p *TestProcess) EventLastNVisible(visibleTarget, maxTotal int) []clievent.EventEntry {
	return p.EventLog.LastNVisible(visibleTarget, maxTotal)
}
func (p *TestProcess) EventEntriesSince(afterMS int64) []clievent.EventEntry {
	return p.EventLog.EntriesSince(afterMS)
}
func (p *TestProcess) EventEntriesSinceAppend(dst []clievent.EventEntry, afterMS int64) []clievent.EventEntry {
	return p.EventLog.EntriesSinceAppend(dst, afterMS)
}
func (p *TestProcess) EventEntriesBefore(beforeMS int64, limit int) []clievent.EventEntry {
	return p.EventLog.EntriesBefore(beforeMS, limit)
}
func (p *TestProcess) LastActivitySummary() string                { return p.EventLog.LastActivitySummary() }
func (p *TestProcess) LastResponseSummary() string                { return p.EventLog.LastResponseSummary() }
func (p *TestProcess) LastEventAt() time.Time                     { return p.EventLog.LastEventAt() }
func (p *TestProcess) UserTurnCount() int64                       { return p.EventLog.UserTurnCount() }
func (p *TestProcess) ProtocolName() string                       { return "test" }
func (p *TestProcess) SubscribeEvents() (<-chan struct{}, func()) { return p.EventLog.Subscribe() }
func (p *TestProcess) PID() int                                   { return 0 }
func (p *TestProcess) InjectHistory(entries []clievent.EventEntry) {
	for _, e := range entries {
		p.EventLog.Append(e)
	}
}
func (p *TestProcess) TurnAgents() []cli.SubagentInfo { return p.EventLog.TurnAgents() }

// Normalize-layer stubs (docs/rfc/multi-backend.md §8.8); zero values keep
// SessionSnapshot assertions stable.
func (p *TestProcess) ContextUsagePercent() float64       { return 0 }
func (p *TestProcess) TurnDurationMs() int64              { return 0 }
func (p *TestProcess) MeteringUsage() []cli.MeteringEntry { return p.MeteringVal }
func (p *TestProcess) TakeShadowUsage() cli.ShadowUsage {
	u := p.ShadowVal
	p.ShadowVal = cli.ShadowUsage{}
	return u
}
func (p *TestProcess) MeteringGen() uint64 { return 0 }
func (p *TestProcess) Model() string       { return p.ModelVal }
func (p *TestProcess) LiveVersion() string { return p.LiveVersionVal }
func (p *TestProcess) Effort() string      { return p.EffortVal }

// InjectSession inserts a session with the given TestProcess into the router.
// For use in tests that need sessions without spawning real CLI processes.
// A nil proc yields a detached (no-process / stub) session.
func (r *Router) InjectSession(key string, proc *TestProcess) *ManagedSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &ManagedSession{
		key:      key,
		runStore: r.sessionRuns, // mirror production wiring so Send records runs
	}
	if proc != nil { // typed-nil *TestProcess must not become a non-nil iface
		s.storeProcess(proc)
	}
	s.touchLastActive()
	s.initCreatedAtIfUnset()
	r.attachHistorySource(s)
	r.ss.sessions[key] = s
	r.ss.activeCount.Add(1)
	return s
}

// SetWorkspaceForTest stamps the session-level workspace on an injected
// session; production sets it through the spawn / takeover paths.
func (s *ManagedSession) SetWorkspaceForTest(ws string) { s.setWorkspace(ws) }
