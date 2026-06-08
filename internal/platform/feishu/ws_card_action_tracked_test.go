package feishu

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestDispatchCardActionTracked is the regression guard for
// R20260608-133914-LB-4 (#1964): the WS OnP2CardActionTrigger path must run
// the card dispatch under the same wg + semaphore discipline as the WS
// message branches and the webhook card branch, instead of calling the
// handler synchronously with no tracking or back-pressure. The cases pin:
//   - the dispatch actually fires the handler (happy path),
//   - f.wg tracks it so Stop()'s wg.Wait() can drain it,
//   - a full semaphore drops the click (best-effort) without leaking a wg
//     count and without firing the handler.
func TestDispatchCardActionTracked(t *testing.T) {
	t.Parallel()

	payload := cardActionPayload{
		Kind:      "ask_answer",
		ToolUseID: "toolu_xyz",
		Header:    "Error style",
		Label:     "Return an error",
	}

	tests := []struct {
		name        string
		semCap      int
		preFill     int // slots occupied before dispatch (to force a full sem)
		wantCalled  int32
		wantDropped bool
	}{
		{"dispatched_with_capacity", 20, 0, 1, false},
		{"dropped_when_sem_full", 1, 1, 0, true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &Feishu{}
			sem := make(chan struct{}, tt.semCap)
			for i := 0; i < tt.preFill; i++ {
				sem <- struct{}{}
			}

			var called atomic.Int32
			var got platform.IncomingMessage
			handler := func(_ context.Context, m platform.IncomingMessage) {
				called.Add(1)
				got = m
			}

			// messageID empty so the cosmetic EditMessage goroutine is skipped
			// (it requires a configured client); we only assert tracking + sem.
			f.dispatchCardActionTracked(context.Background(), sem, payload,
				"oc_123", "", "group", "ou_user", handler)

			if called.Load() != tt.wantCalled {
				t.Fatalf("handler called %d times, want %d", called.Load(), tt.wantCalled)
			}
			if !tt.wantDropped {
				if got.Text != "Error style: Return an error." {
					t.Errorf("message text = %q", got.Text)
				}
				if !got.MentionMe {
					t.Error("card click should force MentionMe=true")
				}
			}

			// In both cases f.wg must be balanced (count back to 0): the
			// dispatched path Done()s in its defer; the dropped path Done()s
			// before returning. wg.Wait() returning promptly proves no leak.
			waitDone := make(chan struct{})
			go func() {
				f.wg.Wait()
				close(waitDone)
			}()
			select {
			case <-waitDone:
			case <-time.After(2 * time.Second):
				t.Fatal("f.wg.Wait() did not return: card dispatch leaked a wg count")
			}

			// The semaphore slot used by a successful dispatch must be released
			// (released in defer); the pre-filled slot in the drop case stays.
			if len(sem) != tt.preFill {
				t.Errorf("sem occupancy = %d, want %d (slot not released)", len(sem), tt.preFill)
			}
		})
	}
}

// TestDispatchCardActionTracked_StopDrainsInFlight pins that f.wg.Wait()
// (the body of Feishu.Stop()) actually blocks until an in-flight card
// dispatch finishes — the graceful-shutdown drain the pre-fix synchronous
// call could not provide.
func TestDispatchCardActionTracked_StopDrainsInFlight(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	sem := make(chan struct{}, 20)

	release := make(chan struct{})
	started := make(chan struct{})
	var finished atomic.Bool
	handler := func(_ context.Context, _ platform.IncomingMessage) {
		close(started)
		<-release // hold the dispatch in-flight
		finished.Store(true)
	}

	var dispatchWG sync.WaitGroup
	dispatchWG.Add(1)
	go func() {
		defer dispatchWG.Done()
		f.dispatchCardActionTracked(context.Background(), sem,
			cardActionPayload{Kind: "ask_answer", Label: "L"},
			"oc_1", "", "group", "ou_1", handler)
	}()

	<-started // dispatch is in-flight, holding a wg count

	drained := make(chan struct{})
	go func() {
		f.wg.Wait() // mirrors Feishu.Stop()'s wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatal("wg.Wait() returned before in-flight card dispatch finished")
	case <-time.After(100 * time.Millisecond):
		// Expected: still draining.
	}

	close(release) // let the dispatch complete
	select {
	case <-drained:
		if !finished.Load() {
			t.Error("wg.Wait() returned but handler did not finish")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wg.Wait() did not return after dispatch completed")
	}
	dispatchWG.Wait()
}
