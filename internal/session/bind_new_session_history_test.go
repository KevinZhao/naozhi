package session

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestBindNewSessionHistory_NoPersisterSafe pins R242-ARCH-11 (#733): the
// bindNewSessionHistory helper sequences loadResumeHistoryOnSpawn before
// installPersistSink. With no event-log persister configured the sink-install
// step is a no-op, so the helper must run cleanly even when handed a nil
// process — exercising the ordering wrapper without a real CLI subprocess.
func TestBindNewSessionHistory_NoPersisterSafe(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 2, TTL: time.Hour})
	t.Cleanup(r.Shutdown)
	if r.eventLogPersister != nil {
		t.Fatal("test expects no persister (EventLogDir empty)")
	}

	key := "dashboard:direct:alice:general"
	s := &ManagedSession{key: key}
	s.initCreatedAtIfUnset()

	// Pre-seed persistedHistory so loadResumeHistoryOnSpawn / InjectHistory
	// have content to carry; the absence of a prevSessionID chain keeps the
	// resume walk a no-op while still driving the helper end-to-end.
	s.InjectHistory([]cli.EventEntry{
		{UUID: "aaa", Time: 100, Type: "user", Summary: "hi"},
	})

	// nil proc is safe here: installPersistSink early-returns on a nil
	// persister before ever touching proc. The call must not panic.
	r.bindNewSessionHistory(context.Background(), s, nil, key, "", "", nil, nil)

	if !s.hasInjectedHistory() {
		t.Fatal("seeded history lost after bindNewSessionHistory")
	}
}
