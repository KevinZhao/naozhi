package session

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// A passthrough send leaves the process Ready until the CLI starts the turn,
// while the dashboard was already pushed "running": the session reads running
// from the send's start, and ready again once it returns.
func TestState_SendUnderwayReadsRunning(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "dashboard:direct:alice:general"}
	proc := NewTestProcess()
	entered, release := make(chan struct{}), make(chan struct{})
	proc.SendPassthroughFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback, string) (*clievent.SendResult, error) {
		close(entered)
		<-release
		return &clievent.SendResult{Text: "ok"}, nil
	}
	s.storeProcess(proc)

	if got := s.State(); got != "ready" {
		t.Fatalf("idle: State = %q, want ready", got)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.SendPassthrough(context.Background(), "hi", nil, nil, "")
	}()
	<-entered
	if proc.State() != cli.StateReady {
		t.Fatalf("test process state = %v, want Ready (the CLI has not started the turn)", proc.State())
	}
	if got := s.State(); got != "running" {
		t.Errorf("send underway: State = %q, want running", got)
	}
	if got := s.Snapshot().State; got != "running" {
		t.Errorf("send underway: Snapshot().State = %q, want running", got)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send did not return")
	}
	if got := s.Snapshot().State; got != "ready" {
		t.Errorf("send returned: Snapshot().State = %q, want ready", got)
	}

	// A dead process stays dead whatever is waiting on it.
	s.turnWaiters.Add(1)
	defer s.turnWaiters.Add(-1)
	proc.SetState(cli.StateDead)
	if got := s.State(); got != "dead" {
		t.Errorf("dead with a waiter: State = %q, want dead", got)
	}
}
