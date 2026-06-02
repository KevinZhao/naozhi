package cli

import (
	"context"
	"testing"
)

// TestRegisterHistoryFactory_LastWriteWins pins the documented
// re-registration contract on RegisterHistoryFactory: re-registering an
// already-known backend ID overwrites the previous factory and the last
// registration wins. The doc comment on RegisterHistoryFactory states
// "Re-registering a backend ID overwrites the previous factory; the last
// registration wins. Tests rely on this to inject failing factories." —
// but no test asserted it directly until now.
//
// This invariant is load-bearing for #1033 (R240-ARCH-17): the proposed
// refactor moves HistoryFactoryFn registration out of internal/cli into a
// dedicated internal/history package. Whatever shape the registry takes
// after that move, last-write-wins must survive or every test that injects
// a replacement factory (and the live re-wireup path) breaks silently.
func TestRegisterHistoryFactory_LastWriteWins(t *testing.T) {
	// Not Parallel: mutates the shared registry under a unique key, but the
	// final lookup must observe THIS test's last write, so keep it serial
	// against itself. The unique key isolates it from other tests.
	const id = "cli-test-lastwrite-x4k2"

	first := &recordingHistorySource{tag: "first"}
	second := &recordingHistorySource{tag: "second"}

	RegisterHistoryFactory(id, func(HistorySessionView, HistoryWiring) HistorySource {
		return first
	})
	RegisterHistoryFactory(id, func(HistorySessionView, HistoryWiring) HistorySource {
		return second
	})

	got := pickHistoryFactory(id)
	if got == nil {
		t.Fatal("pickHistoryFactory returned nil after registration")
	}
	src := got(&fakeHistorySession{}, HistoryWiring{})
	rec, ok := src.(*recordingHistorySource)
	if !ok {
		t.Fatalf("factory returned %T; want *recordingHistorySource", src)
	}
	if rec.tag != "second" {
		t.Fatalf("last-write-wins violated: got factory %q, want %q", rec.tag, "second")
	}
}

// recordingHistorySource is a HistorySource stub whose only purpose is to
// carry a tag so the test can tell which factory produced it.
type recordingHistorySource struct {
	tag string
}

func (r *recordingHistorySource) LoadBefore(context.Context, int64, int) ([]EventEntry, error) {
	return nil, nil
}
