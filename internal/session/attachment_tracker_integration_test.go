package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// routerWithWorkspace spins up a Router with a session whose
// Workspace() resolves to ws so the tracker's resolver closure
// returns the expected directory. The fake ManagedSession is
// registered via r.mu write-lock to keep concurrency semantics
// correct.
func routerWithWorkspace(t *testing.T, ws string, key string) (*Router, string) {
	t.Helper()
	tmp := t.TempDir()
	eventLogDir := filepath.Join(tmp, "events")
	r := NewRouter(RouterConfig{
		MaxProcs:    4,
		TTL:         time.Hour,
		StorePath:   filepath.Join(tmp, "sessions.json"),
		EventLogDir: eventLogDir,
	})
	t.Cleanup(r.Shutdown)
	// Register a bare ManagedSession so the tracker's resolver
	// closure can locate it.
	s := &ManagedSession{key: key}
	s.setWorkspace(ws)
	r.mu.Lock()
	r.sessions[key] = s
	r.mu.Unlock()
	return r, eventLogDir
}

// writeAttachmentPair drops a (payload, meta) pair on disk so the
// tracker has something to bump.
func writeAttachmentPair(t *testing.T, ws, date, stem string, uploaded time.Time) (relPath, metaPath string) {
	t.Helper()
	day := filepath.Join(ws, attachment.Dir, date)
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(day, stem+".png")
	if err := os.WriteFile(payload, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath = filepath.Join(day, stem+".meta")
	meta := attachment.Meta{OrigName: stem + ".png", UploadedAt: uploaded}
	buf, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	relPath = filepath.ToSlash(
		filepath.Join(attachment.Dir, date, stem+".png"),
	)
	return
}

// TestTrackerIntegration_BumpsMetaThroughSink is the end-to-end
// happy-path exercise: emit an EventEntry with ImagePaths via the
// event-log sink bridge → tracker bump lands → .meta contains the
// keyhash. Mirrors the production path from
// cli.EventLog.Append → bridge → tracker.
func TestTrackerIntegration_BumpsMetaThroughSink(t *testing.T) {
	ws := t.TempDir()
	key := "dashboard:direct:alice:general"
	r, _ := routerWithWorkspace(t, ws, key)

	now := time.Now().UTC()
	rel, metaPath := writeAttachmentPair(t, ws, now.Format("2006-01-02"), "att1", now)

	// Build the same sink installPersistSink would install.
	sink := newEventLogSink(
		r.eventLogPersister.SinkFor(key),
		r.attachmentTracker,
		persist.KeyHash(key),
	)
	// Fire a live event carrying the attachment path.
	sink([]cli.EventEntry{{
		UUID:       "abc",
		Time:       1700000000000,
		Type:       "user",
		Summary:    "with image",
		Images:     []string{"data:image/png;base64,AAA="},
		ImagePaths: []string{rel},
	}}, false)

	// Flush both the persister and the tracker so assertions are
	// deterministic.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.eventLogPersister.Flush(ctx)
	if err := r.attachmentTracker.Flush(ctx); err != nil {
		t.Fatalf("tracker.Flush: %v", err)
	}

	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if !m.HasReference(persist.KeyHash(key)) {
		t.Errorf("meta missing keyhash reference: %+v", m)
	}
	if m.LastReferencedAt != 1700000000000 {
		t.Errorf("LastReferencedAt=%d", m.LastReferencedAt)
	}
}

// TestTrackerIntegration_ReplayPhaseSkipsBump: a replay-phase batch
// must NOT update the tracker (would inflate LastReferencedAt with
// a fresh time and defeat the refTTL expiry).
func TestTrackerIntegration_ReplayPhaseSkipsBump(t *testing.T) {
	ws := t.TempDir()
	key := "k"
	r, _ := routerWithWorkspace(t, ws, key)

	now := time.Now().UTC()
	rel, metaPath := writeAttachmentPair(t, ws, now.Format("2006-01-02"), "r1", now)

	sink := newEventLogSink(
		r.eventLogPersister.SinkFor(key),
		r.attachmentTracker,
		persist.KeyHash(key),
	)
	sink([]cli.EventEntry{{
		UUID: "rrr", Time: 1, Type: "user",
		ImagePaths: []string{rel},
	}}, true /* replayPhase */)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.eventLogPersister.Flush(ctx)
	_ = r.attachmentTracker.Flush(ctx)

	raw, _ := os.ReadFile(metaPath)
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if len(m.ReferencingKeyHashes) != 0 {
		t.Errorf("replay phase polluted meta: %v", m.ReferencingKeyHashes)
	}
	if m.LastReferencedAt != 0 {
		t.Errorf("LastReferencedAt=%d on replay, want 0", m.LastReferencedAt)
	}
}

// TestTrackerIntegration_RemoveClearsRefs: Router.Remove triggers
// OnSessionRemoved which walks the workspace and drops the keyhash
// from every .meta file.
func TestTrackerIntegration_RemoveClearsRefs(t *testing.T) {
	ws := t.TempDir()
	key := "remove-me"
	r, _ := routerWithWorkspace(t, ws, key)

	now := time.Now().UTC()
	rel, metaPath := writeAttachmentPair(t, ws, now.Format("2006-01-02"), "rm", now)

	sink := newEventLogSink(
		r.eventLogPersister.SinkFor(key),
		r.attachmentTracker,
		persist.KeyHash(key),
	)
	sink([]cli.EventEntry{{UUID: "u", Time: 1, Type: "user", ImagePaths: []string{rel}}}, false)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.eventLogPersister.Flush(ctx)
	_ = r.attachmentTracker.Flush(ctx)

	// Pre-condition: keyhash present.
	raw, _ := os.ReadFile(metaPath)
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if !m.HasReference(persist.KeyHash(key)) {
		t.Fatalf("keyhash not bumped pre-remove")
	}
	// Guard: workspace must be non-empty on the registered session
	// — if setWorkspace didn't stick we'd silently skip clear.
	r.mu.RLock()
	ws2 := r.sessions[key].Workspace()
	r.mu.RUnlock()
	if ws2 != ws {
		t.Fatalf("workspace drift: got %q, want %q", ws2, ws)
	}

	// Remove the session → OnSessionRemoved walks ws and clears.
	if r.attachmentTracker == nil {
		t.Fatalf("attachment tracker not initialized")
	}
	beforeStats := r.attachmentTracker.Stats()
	if !r.Remove(key) {
		t.Fatal("Remove returned false")
	}
	afterStats := r.attachmentTracker.Stats()
	t.Logf("tracker stats before=%+v after=%+v", beforeStats, afterStats)

	// Re-read into a fresh Meta. json.Unmarshal does NOT zero
	// existing slice fields when the JSON omits them (which happens
	// after the remove → len==0 triggers omitempty). Reusing `m`
	// here would mask the clear.
	raw, _ = os.ReadFile(metaPath)
	var after attachment.Meta
	_ = json.Unmarshal(raw, &after)
	if after.HasReference(persist.KeyHash(key)) {
		t.Errorf("keyhash still present after Remove: %v", after.ReferencingKeyHashes)
	}
}

// TestTrackerIntegration_DisabledConfig: no EventLogDir → no
// tracker → sink bridge tolerates nil tracker without panicking.
func TestTrackerIntegration_DisabledConfig(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:  2,
		TTL:       time.Hour,
		StorePath: filepath.Join(tmp, "sessions.json"),
		// EventLogDir empty → tracker disabled.
	})
	t.Cleanup(r.Shutdown)
	if r.attachmentTracker != nil {
		t.Fatal("tracker created despite disabled event log")
	}

	// Building a sink with a nil tracker must be safe (future
	// refactor may flow through the same path for disabled mode).
	sink := newEventLogSink(
		func(entries []persist.Entry, replayPhase bool) {},
		nil, "",
	)
	// Should not panic.
	sink([]cli.EventEntry{{Type: "user", Time: 1, ImagePaths: []string{".naozhi/attachments/x/y.png"}}}, false)
}
