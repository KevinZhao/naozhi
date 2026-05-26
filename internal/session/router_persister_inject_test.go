package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// TestRouter_InjectedPersister_Wins verifies that when RouterConfig
// supplies an EventLogPersister the router does NOT construct its own
// from EventLogDir/EventLogGenerator. This is the R239-ARCH-N
// SessionStore/EventLog split contract: callers can own the Persister
// lifecycle independently of the SessionStore wiring.
func TestRouter_InjectedPersister_Wins(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")
	eventLogDir := filepath.Join(tmp, "events")

	p, err := persist.NewPersister(persist.Options{
		Dir:       eventLogDir,
		Generator: "test-injected",
		DevMode:   false,
		Observer:  eventLogMetricsObserver{},
	})
	if err != nil {
		t.Fatalf("NewPersister: %v", err)
	}

	r := NewRouter(RouterConfig{
		MaxProcs:          4,
		TTL:               time.Hour,
		StorePath:         storePath,
		EventLogDir:       eventLogDir,
		EventLogPersister: p,
	})
	t.Cleanup(r.Shutdown)

	if r.eventLogPersister == nil {
		t.Fatal("eventLogPersister is nil despite EventLogPersister injected")
	}
	if r.eventLogPersister != p {
		t.Errorf("router did not adopt injected persister: got %p want %p",
			r.eventLogPersister, p)
	}
}

// TestRouter_NoPersisterWhenNeitherSet locks in that omitting both
// EventLogDir and EventLogPersister keeps event-log persistence
// disabled — the path that legacy callers without an event-log dir
// rely on.
func TestRouter_NoPersisterWhenNeitherSet(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:  4,
		TTL:       time.Hour,
		StorePath: filepath.Join(tmp, "sessions.json"),
	})
	t.Cleanup(r.Shutdown)
	if r.eventLogPersister != nil {
		t.Error("eventLogPersister should be nil when neither dir nor injected persister set")
	}
}
