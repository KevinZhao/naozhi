package session

import (
	"context"

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
func (p *TestProcess) LastEntryOfType(typ string) cli.EventEntry {
	return p.EventLog.LastEntryOfType(typ)
}
func (p *TestProcess) LastActivitySummary() string                { return p.EventLog.LastActivitySummary() }
func (p *TestProcess) ProtocolName() string                       { return "test" }
func (p *TestProcess) SubscribeEvents() (<-chan struct{}, func()) { return p.EventLog.Subscribe() }
func (p *TestProcess) PID() int                                   { return 0 }
func (p *TestProcess) InjectHistory(entries []cli.EventEntry) {
	for _, e := range entries {
		p.EventLog.Append(e)
	}
}
func (p *TestProcess) TurnAgents() []cli.SubagentInfo { return p.EventLog.TurnAgents() }

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
	r.sessions[key] = s
	r.activeCount.Add(1)
	return s
}
