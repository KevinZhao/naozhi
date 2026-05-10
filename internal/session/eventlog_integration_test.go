package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
)

// waitFor polls fn up to timeout at 10ms intervals, returning true
// as soon as fn returns true. Used instead of time.Sleep so the
// goroutine-driven persister has deterministic testable semantics.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// newEventLogRouter is a test-only helper that constructs a Router
// with the event-log persister enabled against a fresh tmpdir.
// Returns the router and the events directory so tests can inspect
// on-disk state.
func newEventLogRouter(t *testing.T, devMode bool) (*Router, string) {
	t.Helper()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")
	eventLogDir := filepath.Join(tmp, "events")
	r := NewRouter(RouterConfig{
		MaxProcs:        4,
		TTL:             time.Hour,
		StorePath:       storePath,
		EventLogDir:     eventLogDir,
		EventLogDevMode: devMode,
	})
	t.Cleanup(r.Shutdown)
	return r, eventLogDir
}

// TestEventLogIntegration_PersisterStartsWhenDirSet is the smoke
// test: NewRouter with EventLogDir non-empty must produce a live
// persister. Without this we'd lose visibility into a future
// regression where NewPersister silently errors out.
func TestEventLogIntegration_PersisterStartsWhenDirSet(t *testing.T) {
	r, dir := newEventLogRouter(t, false)
	if r.eventLogPersister == nil {
		t.Fatal("eventLogPersister is nil despite EventLogDir set")
	}
	// Directory must exist.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("events dir not created: %v", err)
	}
}

// TestEventLogIntegration_DirectSinkWorks cuts through Router and
// tests the persister + naozhilog source directly. This anchors the
// "EventEntry → disk → EventEntry" round-trip, which router-level
// tests build on.
func TestEventLogIntegration_DirectSinkWorks(t *testing.T) {
	r, dir := newEventLogRouter(t, false)
	sinkBuilder := r.eventLogPersister.SinkFor("k")
	// Tests construct the sink with a nil tracker and empty keyhash
	// to exercise the persist path in isolation. Integration through
	// the Router (see spawnSession/installPersistSink) supplies real
	// values.
	sink := newEventLogSink(sinkBuilder, nil, "")

	// Replay path: sinkReady=true as we pass replayPhase=true.
	sink([]cli.EventEntry{{UUID: "aa", Time: 100, Type: "user", Summary: "replay"}}, true)

	// Live path.
	sink([]cli.EventEntry{{UUID: "bb", Time: 200, Type: "user", Summary: "live"}}, false)

	// Wait for the persister to drain + fsync.
	if !waitFor(t, time.Second, func() bool {
		return r.eventLogPersister.Stats().Written >= 1
	}) {
		t.Fatalf("persister never wrote: stats=%+v", r.eventLogPersister.Stats())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = r.eventLogPersister.Flush(ctx)

	// Read back via naozhilog.Source.
	src := naozhilog.New(dir, "k")
	got, err := src.LoadLatest(context.Background(), 100)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries (want 1 live; replay must have been dropped)", len(got))
	}
	if got[0].Summary != "live" {
		t.Errorf("entry is %q, want 'live'", got[0].Summary)
	}
	if r.eventLogPersister.Stats().ReplayLeak != 1 {
		t.Errorf("ReplayLeak=%d, want 1", r.eventLogPersister.Stats().ReplayLeak)
	}
}

// TestEventLogIntegration_RouterDropKeyRemovesFiles: Remove a
// session → the persister files for that key disappear. This is the
// production path Dashboard × button hits.
func TestEventLogIntegration_RouterDropKeyRemovesFiles(t *testing.T) {
	r, dir := newEventLogRouter(t, false)
	key := "dashboard:direct:alice:general"

	// Direct sink so we don't need a full cli.Process — the Router's
	// Remove path exercises DropKey independently of spawnSession.
	sink := newEventLogSink(r.eventLogPersister.SinkFor(key), nil, "")
	sink([]cli.EventEntry{{UUID: "aa", Time: 1, Type: "user"}}, false)

	// Flush so the file definitely exists on disk before we remove.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = r.eventLogPersister.Flush(ctx)

	logPath := persist.LogPath(dir, key)
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file missing before remove: %v", err)
	}

	// Simulate "user clicked × on session card". The session map
	// doesn't contain `key` because we didn't spawn, so Remove
	// returns false — but the event log drop is still worth
	// validating in isolation.
	r.dropEventLogForKey(key)

	// File should be gone.
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("log file still exists after dropEventLogForKey: err=%v", err)
	}
}

// TestEventLogIntegration_RestartLoadsLatest: write entries, stop
// the router (simulating graceful shutdown), start a fresh router
// against the SAME events dir + sessions.json, then read the new
// session's naozhilog-backed history.
//
// Validates the full "restart and see your image back" user journey.
func TestEventLogIntegration_RestartLoadsLatest(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")
	eventLogDir := filepath.Join(tmp, "events")

	// Run 1: write 3 events for one key, then Shutdown.
	r1 := NewRouter(RouterConfig{
		MaxProcs:    4,
		TTL:         time.Hour,
		StorePath:   storePath,
		EventLogDir: eventLogDir,
	})
	key := "dashboard:direct:alice:general"
	sink := newEventLogSink(r1.eventLogPersister.SinkFor(key), nil, "")
	sink([]cli.EventEntry{
		{UUID: "aaa", Time: 100, Type: "user", Summary: "hi", Images: []string{"data:image/jpeg;base64,XYZ="}},
		{UUID: "bbb", Time: 200, Type: "text", Summary: "hello"},
		{UUID: "ccc", Time: 300, Type: "user", Summary: "again"},
	}, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = r1.eventLogPersister.Flush(ctx)
	cancel()
	r1.Shutdown()

	// Run 2: fresh router, same dirs. Load the persisted entries via
	// the naozhilog.Source it wires into attachHistorySource.
	r2 := NewRouter(RouterConfig{
		MaxProcs:    4,
		TTL:         time.Hour,
		StorePath:   storePath,
		EventLogDir: eventLogDir,
	})
	t.Cleanup(r2.Shutdown)

	src := naozhilog.New(eventLogDir, key)
	got, err := src.LoadLatest(context.Background(), 100)
	if err != nil {
		t.Fatalf("LoadLatest run2: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("run2 got %d entries, want 3 (persisted from run1)", len(got))
	}
	// The first entry (with an image) must survive verbatim — this
	// is the whole point of the feature.
	if len(got[0].Images) != 1 {
		t.Errorf("Images dropped across restart: %+v", got[0])
	}
	if got[0].Summary != "hi" {
		t.Errorf("got[0].Summary=%q, want 'hi'", got[0].Summary)
	}
}

// TestEventLogIntegration_ReplayLeakPanics_DevMode ensures the
// DevMode panic guard is effective when a caller violates the
// "SetPersistSink after InjectHistory" ordering. Production (DevMode=false)
// is tested separately via TestEventLogIntegration_DirectSinkWorks.
func TestEventLogIntegration_ReplayLeakPanics_DevMode(t *testing.T) {
	r, _ := newEventLogRouter(t, true)
	sink := newEventLogSink(r.eventLogPersister.SinkFor("k"), nil, "")

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("DevMode replay did not panic")
		} else {
			msg := ""
			switch v := r.(type) {
			case string:
				msg = v
			case error:
				msg = v.Error()
			}
			if !strings.Contains(msg, "replay") {
				t.Errorf("panic message lacked 'replay': %v", r)
			}
		}
	}()
	sink([]cli.EventEntry{{UUID: "aa", Time: 1, Type: "user"}}, true)
}

// TestEventLogIntegration_DisabledByEmptyDir ensures the opt-out
// path: EventLogDir="" means no persister, attachHistorySource
// uses the single-source legacy behaviour, no files anywhere.
func TestEventLogIntegration_DisabledByEmptyDir(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:  2,
		TTL:       time.Hour,
		StorePath: filepath.Join(tmp, "sessions.json"),
		// EventLogDir intentionally empty.
	})
	t.Cleanup(r.Shutdown)
	if r.eventLogPersister != nil {
		t.Errorf("persister created despite empty EventLogDir")
	}
	// Recycle the helper so we also confirm DropKey is a no-op.
	r.dropEventLogForKey("any")
}
