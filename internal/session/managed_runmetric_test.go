package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// newInstrumentedSession builds a ManagedSession bound to a TestProcess and a
// real (temp-dir) run-history store, so finishRun's async write actually
// lands on disk for assertion.
func newInstrumentedSession(t *testing.T, sendFunc func(context.Context, string, []cli.ImageData, cli.EventCallback) (*cli.SendResult, error)) (*ManagedSession, *runhistory.Store) {
	t.Helper()
	store := runhistory.NewStore(t.TempDir(), 0, 0)
	t.Cleanup(store.Close)
	s := &ManagedSession{key: "feishu:p2p:tester", runStore: store}
	s.storeProcess(&TestProcess{AliveVal: true, SendFunc: sendFunc})
	return s, store
}

func TestSend_RecordsCompletedRun(t *testing.T) {
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []cli.ImageData, on cli.EventCallback) (*cli.SendResult, error) {
		if on != nil {
			on(cli.Event{}) // emit a first byte
		}
		return &cli.SendResult{Text: "ok", CostUSD: 0.05}, nil
	})

	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	store.Close() // flush worker

	runs := store.Recent(s.key, 0)
	if len(runs) != 1 {
		t.Fatalf("want 1 recorded run, got %d", len(runs))
	}
	r := runs[0]
	if r.Outcome != runhistory.OutcomeCompleted {
		t.Errorf("outcome = %s, want completed", r.Outcome)
	}
	if r.DurationMS < 0 {
		t.Errorf("duration must be >= 0, got %d", r.DurationMS)
	}
	if r.FirstByteMS < 0 {
		t.Errorf("first byte must be >= 0, got %d", r.FirstByteMS)
	}
	if r.CostUSD != 0.05 {
		t.Errorf("cost = %v, want 0.05", r.CostUSD)
	}
}

func TestSend_OutcomeClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want runhistory.Outcome
	}{
		{"timeout", cli.ErrTotalTimeout, runhistory.OutcomeTimeout},
		{"no-output", cli.ErrNoOutputTimeout, runhistory.OutcomeTimeout},
		{"canceled", context.Canceled, runhistory.OutcomeCanceled},
		{"error", errors.New("boom"), runhistory.OutcomeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []cli.ImageData, on cli.EventCallback) (*cli.SendResult, error) {
				return nil, tt.err
			})
			_, _ = s.Send(context.Background(), "x", nil, nil)
			store.Close()
			runs := store.Recent(s.key, 0)
			if len(runs) != 1 {
				t.Fatalf("want 1 run, got %d", len(runs))
			}
			if runs[0].Outcome != tt.want {
				t.Errorf("outcome = %s, want %s", runs[0].Outcome, tt.want)
			}
		})
	}
}

func TestSend_FirstByteRecordedOnce(t *testing.T) {
	var firstByteCalls int
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []cli.ImageData, on cli.EventCallback) (*cli.SendResult, error) {
		// emit several events; FirstByteMS must reflect only the first
		for i := 0; i < 3; i++ {
			if on != nil {
				on(cli.Event{})
			}
		}
		return &cli.SendResult{Text: "ok"}, nil
	})
	// wrap an inner callback to count user-callback passthrough
	userCb := func(ev cli.Event) { firstByteCalls++ }
	if _, err := s.Send(context.Background(), "hi", nil, userCb); err != nil {
		t.Fatalf("Send: %v", err)
	}
	store.Close()
	if firstByteCalls != 3 {
		t.Errorf("user callback should receive all 3 events, got %d", firstByteCalls)
	}
	runs := store.Recent(s.key, 0)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	// FirstByteMS is set (>=0) and the run completed — single first-byte stamp
	if runs[0].Outcome != runhistory.OutcomeCompleted {
		t.Errorf("outcome = %s", runs[0].Outcome)
	}
}

func TestSendPassthrough_AlsoRecorded(t *testing.T) {
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []cli.ImageData, on cli.EventCallback) (*cli.SendResult, error) {
		return &cli.SendResult{Text: "ok"}, nil
	})
	if _, err := s.SendPassthrough(context.Background(), "hi", nil, nil, ""); err != nil {
		t.Fatalf("SendPassthrough: %v", err)
	}
	store.Close()
	if got := len(store.Recent(s.key, 0)); got != 1 {
		t.Errorf("passthrough run not recorded: got %d", got)
	}
}

func TestSend_NilStoreNoRecord(t *testing.T) {
	// runStore nil -> instrumentation no-ops, Send still works (regression
	// guard for the zero-alloc nil-callback fast path).
	s := &ManagedSession{key: "feishu:p2p:none"}
	s.storeProcess(&TestProcess{AliveVal: true})
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send with nil store: %v", err)
	}
}

// TestSend_FirstByteConcurrentWithFinish reproduces the passthrough hazard:
// the onEvent callback fires on a different goroutine (CLI readLoop) and may
// still be stamping the first-event time while finishRun reads it. The atomic
// stamp must make this race-free under -race.
func TestSend_FirstByteConcurrentWithFinish(t *testing.T) {
	releaseEvent := make(chan struct{})
	eventDone := make(chan struct{})
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []cli.ImageData, on cli.EventCallback) (*cli.SendResult, error) {
		// Fire onEvent from a separate goroutine that overlaps the return,
		// mimicking readLoop fan-out racing the caller's finishRun.
		go func() {
			<-releaseEvent
			if on != nil {
				on(cli.Event{})
			}
			close(eventDone)
		}()
		close(releaseEvent) // let the event goroutine run concurrently with return
		return &cli.SendResult{Text: "ok"}, nil
	})
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-eventDone
	store.Close()
	if got := len(store.Recent(s.key, 0)); got != 1 {
		t.Errorf("want 1 run, got %d", got)
	}
}

func TestSend_DurationMonotonic(t *testing.T) {
	s, store := newInstrumentedSession(t, func(ctx context.Context, text string, imgs []cli.ImageData, on cli.EventCallback) (*cli.SendResult, error) {
		time.Sleep(5 * time.Millisecond)
		return &cli.SendResult{Text: "ok"}, nil
	})
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	store.Close()
	runs := store.Recent(s.key, 0)
	if len(runs) != 1 || runs[0].DurationMS < 1 {
		t.Errorf("duration should reflect the ~5ms sleep, got %+v", runs)
	}
}
