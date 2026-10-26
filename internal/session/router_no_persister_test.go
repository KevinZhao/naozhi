package session

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRouter_NoPersisterWhenNeitherSet locks in that omitting
// EventLogDir keeps event-log persistence disabled — the path that legacy callers without an event-log dir
// rely on.
func TestRouter_NoPersisterWhenNeitherSet(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:  4,
		TTL:       time.Hour,
		StorePath: filepath.Join(tmp, "sessions.json"),
	})
	t.Cleanup(r.Shutdown)
	if r.hist.persister != nil {
		t.Error("eventLogPersister should be nil when no event log dir is set")
	}
}
