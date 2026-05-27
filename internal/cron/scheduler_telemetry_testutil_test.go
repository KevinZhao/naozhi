package cron

import (
	"sync"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// recordingBroadcaster is a minimal runtelemetry.Broadcaster that
// captures every RunStarted / RunEnded event for assertion. Replaces
// the per-test ad-hoc closures that the pre-Phase-D
// SetOnRunStarted / SetOnRunEnded setters fed.
//
// Tests that previously did:
//
//	s.SetOnRunEnded(func(ev RunEndedEvent) { got = ev })
//
// now do:
//
//	rec := &recordingBroadcaster{}
//	s := NewScheduler(SchedulerConfig{Telemetry: rec, ...})
//	... drive the run ...
//	got := rec.endedAtCron(0) // single ended event captured
//
// recordingBroadcaster also exposes the converted cron-local view via
// endedAtCron / endedAllCron so tests keep asserting on RunEndedEvent
// shape (not the runtelemetry shape).
type recordingBroadcaster struct {
	mu      sync.Mutex
	started []runtelemetry.RunStartedEvent
	ended   []runtelemetry.RunEndedEvent
}

func (r *recordingBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {
	r.mu.Lock()
	r.started = append(r.started, ev)
	r.mu.Unlock()
}

func (r *recordingBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	r.mu.Lock()
	r.ended = append(r.ended, ev)
	r.mu.Unlock()
}

// endedCount returns the captured RunEnded count.
func (r *recordingBroadcaster) endedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ended)
}

// endedAtCron returns the i-th captured RunEnded translated back to the
// cron-local RunEndedEvent shape. Panics if i is out of range so tests
// fail loudly on capture-count mismatch instead of silently asserting a
// zero value.
func (r *recordingBroadcaster) endedAtCron(i int) RunEndedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := r.ended[i]
	return RunEndedEvent{
		JobID:      ev.OwnerID,
		RunID:      ev.RunID,
		State:      RunState(ev.State),
		StartedAt:  ev.StartedAt,
		EndedAt:    ev.EndedAt,
		DurationMS: ev.DurationMS,
		SessionID:  ev.SessionID,
		ErrorClass: ErrorClass(ev.ErrorClass),
		ErrorMsg:   ev.ErrorMsg,
		Trigger:    TriggerKind(ev.Trigger),
	}
}

// endedAllCron returns every captured RunEnded translated back to
// cron-local shape, for tests that loop / multi-event assertions.
func (r *recordingBroadcaster) endedAllCron() []RunEndedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RunEndedEvent, len(r.ended))
	for i, ev := range r.ended {
		out[i] = RunEndedEvent{
			JobID:      ev.OwnerID,
			RunID:      ev.RunID,
			State:      RunState(ev.State),
			StartedAt:  ev.StartedAt,
			EndedAt:    ev.EndedAt,
			DurationMS: ev.DurationMS,
			SessionID:  ev.SessionID,
			ErrorClass: ErrorClass(ev.ErrorClass),
			ErrorMsg:   ev.ErrorMsg,
			Trigger:    TriggerKind(ev.Trigger),
		}
	}
	return out
}
