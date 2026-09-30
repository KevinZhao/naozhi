package runtelemetry

import (
	"strings"
	"testing"
)

type recordingBroadcaster struct{ started, ended []string }

func (r *recordingBroadcaster) BroadcastRunStarted(ev RunStartedEvent) {
	r.started = append(r.started, ev.RunID)
}
func (r *recordingBroadcaster) BroadcastRunEnded(ev RunEndedEvent) {
	r.ended = append(r.ended, ev.RunID)
}

// Events before the bind are dropped; after it they are forwarded; a second
// bind panics.
func TestRelay(t *testing.T) {
	var r Relay
	r.BroadcastRunStarted(RunStartedEvent{RunID: "early"})
	r.BroadcastRunEnded(RunEndedEvent{RunID: "early"})

	rec := &recordingBroadcaster{}
	r.Bind(rec)
	r.BroadcastRunStarted(RunStartedEvent{RunID: "a"})
	r.BroadcastRunEnded(RunEndedEvent{RunID: "b"})
	if strings.Join(rec.started, ",") != "a" || strings.Join(rec.ended, ",") != "b" {
		t.Errorf("forwarded started=%v ended=%v, want [a] [b]", rec.started, rec.ended)
	}

	defer func() {
		if p, _ := recover().(string); !strings.Contains(p, "bound twice") {
			t.Errorf("second Bind: panic %q", p)
		}
	}()
	r.Bind(&recordingBroadcaster{})
}
