package runtelemetry

import "testing"

type countB struct{ started, ended int }

func (c *countB) BroadcastRunStarted(RunStartedEvent) { c.started++ }
func (c *countB) BroadcastRunEnded(RunEndedEvent)     { c.ended++ }

func TestTee(t *testing.T) {
	t.Parallel()
	if Tee(nil, nil) != nil {
		t.Fatal("two nils must stay nil")
	}
	a, b := &countB{}, &countB{}
	if Tee(a, nil) != Broadcaster(a) || Tee(nil, b) != Broadcaster(b) {
		t.Fatal("a nil side must return the other unchanged")
	}
	both := Tee(a, b)
	both.BroadcastRunStarted(RunStartedEvent{})
	both.BroadcastRunEnded(RunEndedEvent{})
	both.BroadcastRunEnded(RunEndedEvent{})
	if a.started != 1 || b.started != 1 || a.ended != 2 || b.ended != 2 {
		t.Fatalf("a=%+v b=%+v", *a, *b)
	}
}
