package session

import (
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestTestProcess_ConcurrentStateAccess drives the stub the way a router
// does: Close/Kill/SetState/SetAlive from one side, State/Alive/IsRunning
// from stream and sweep goroutines; the race half needs -race. The closing
// checks pin what the setters and Close leave behind.
func TestTestProcess_ConcurrentStateAccess(t *testing.T) {
	t.Parallel()
	p := NewTestProcess()
	start := make(chan struct{})
	var wg sync.WaitGroup
	const rounds = 200
	writers := []func(){
		p.Close,
		p.Kill,
		func() { p.SetState(cli.StateRunning) },
		func() { p.SetAlive(true) },
	}
	readers := []func(){
		func() { _ = p.State() },
		func() { _ = p.Alive() },
		func() { _ = p.IsRunning() },
	}
	for _, fn := range append(writers, readers...) {
		wg.Add(1)
		go func(fn func()) {
			defer wg.Done()
			<-start
			for range rounds {
				fn()
			}
		}(fn)
	}
	close(start)
	wg.Wait()

	p.SetState(cli.StateRunning)
	p.SetAlive(true)
	if !p.IsRunning() || !p.Alive() {
		t.Fatalf("after SetState/SetAlive: IsRunning=%v Alive=%v, want true/true", p.IsRunning(), p.Alive())
	}
	p.Close()
	if p.State() != cli.StateDead || p.Alive() {
		t.Fatalf("after Close: State=%v Alive=%v, want dead/false", p.State(), p.Alive())
	}
}
