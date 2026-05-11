package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
)

// countingObs records every Observer callback count so tests can
// assert on metrics without reaching into the Tracker's private
// counters. -race safe.
type countingObs struct {
	bump, clear, drop, errs atomic.Int64
}

func (c *countingObs) OnReferenceBump(n int)          { c.bump.Add(int64(n)) }
func (c *countingObs) OnReferenceClear(n int)         { c.clear.Add(int64(n)) }
func (c *countingObs) OnMetaWriteError(string, error) { c.errs.Add(1) }
func (c *countingObs) OnDrop(n int)                   { c.drop.Add(int64(n)) }

// writeAttachment drops a (payload, meta) pair for a workspace + key.
// The resulting path is what EventEntry.ImagePaths would record:
// ".naozhi/attachments/<date>/<stem>.png".
func writeAttachment(t *testing.T, workspace, date, stem string, meta attachment.Meta) (relPath, metaPath string) {
	t.Helper()
	day := filepath.Join(workspace, attachment.Dir, date)
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(day, stem+".png")
	if err := os.WriteFile(payload, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath = filepath.Join(day, stem+".meta")
	buf, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	relPath = filepath.ToSlash(
		filepath.Join(attachment.Dir, date, stem+".png"),
	)
	return
}

// newTracker spins up a Tracker against a single workspace for use
// in tests. Tight coalesce + small buffer so the worker exercises
// the real debounce path without blowing up test duration.
func newTracker(t *testing.T, workspace string, obs Observer) *Tracker {
	t.Helper()
	tr, err := NewTracker(Options{
		Workspaces: func(string) string { return workspace },
		// Tiny window so assertions can land sub-second.
		CoalesceWindow: 100 * time.Millisecond,
		ChannelBuffer:  64,
		Observer:       obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tr.Stop(ctx)
	})
	return tr
}

// TestNewTracker_RequiresWorkspaces: a nil resolver is a caller bug
// — constructor must refuse.
func TestNewTracker_RequiresWorkspaces(t *testing.T) {
	_, err := NewTracker(Options{})
	if err == nil {
		t.Fatal("expected error for nil WorkspaceResolver")
	}
}

// TestOnPersistedEntry_BumpsMeta exercises the primary hot path.
// One bump → after coalesce window elapses → .meta has the keyhash
// and LastReferencedAt.
func TestOnPersistedEntry_BumpsMeta(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, metaPath := writeAttachment(t, ws, date, "a1",
		attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	tr.OnPersistedEntry("sess-hash-1", []string{rel}, 1700000000000)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var m attachment.Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !m.HasReference("sess-hash-1") {
		t.Errorf("reference not persisted: %+v", m.ReferencingKeyHashes)
	}
	if m.LastReferencedAt != 1700000000000 {
		t.Errorf("LastReferencedAt=%d, want 1700000000000", m.LastReferencedAt)
	}
	if obs.bump.Load() != 1 {
		t.Errorf("OnReferenceBump=%d, want 1", obs.bump.Load())
	}
}

// TestOnPersistedEntry_CoalescesDuplicates: 10 rapid bumps to the
// same (keyhash, path) produce exactly one .meta write (via
// OnReferenceBump count).
func TestOnPersistedEntry_CoalescesDuplicates(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, metaPath := writeAttachment(t, ws, date, "a2",
		attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	for i := 0; i < 10; i++ {
		tr.OnPersistedEntry("sess", []string{rel}, int64(1700000000000+i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	if obs.bump.Load() != 1 {
		t.Errorf("OnReferenceBump=%d, want 1 (coalesced)", obs.bump.Load())
	}
	// LastReferencedAt must reflect the HIGHEST observed timeMS.
	raw, _ := os.ReadFile(metaPath)
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if m.LastReferencedAt != 1700000000009 {
		t.Errorf("LastReferencedAt=%d, want last timeMS 1700000000009",
			m.LastReferencedAt)
	}
}

// TestOnPersistedEntry_MultipleKeysDistinctPaths: two sessions
// referencing different attachments → two .meta writes, both keyhash
// strings land exactly once each.
func TestOnPersistedEntry_MultipleKeysDistinctPaths(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel1, _ := writeAttachment(t, ws, date, "x1", attachment.Meta{UploadedAt: time.Now()})
	rel2, _ := writeAttachment(t, ws, date, "x2", attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	tr.OnPersistedEntry("sA", []string{rel1}, 10)
	tr.OnPersistedEntry("sB", []string{rel2}, 20)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	if obs.bump.Load() != 2 {
		t.Errorf("bumps=%d, want 2", obs.bump.Load())
	}
}

// TestOnPersistedEntry_SharedAttachment: two sessions bumping the
// SAME attachment results in a single .meta with both keyhashes
// present in sorted order.
func TestOnPersistedEntry_SharedAttachment(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, metaPath := writeAttachment(t, ws, date, "shared",
		attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	tr.OnPersistedEntry("sB", []string{rel}, 10)
	tr.OnPersistedEntry("sA", []string{rel}, 20)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	raw, _ := os.ReadFile(metaPath)
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if len(m.ReferencingKeyHashes) != 2 {
		t.Fatalf("refs=%v", m.ReferencingKeyHashes)
	}
	if m.ReferencingKeyHashes[0] != "sA" || m.ReferencingKeyHashes[1] != "sB" {
		t.Errorf("unsorted: %v", m.ReferencingKeyHashes)
	}
}

// TestOnPersistedEntry_EscapedPathRejected: an absolute path that
// doesn't live under workspace/attachments must never be touched.
// Guards against a compromised EventEntry exploiting the tracker to
// rewrite operator files.
func TestOnPersistedEntry_EscapedPathRejected(t *testing.T) {
	ws := t.TempDir()
	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	// Absolute path outside the attachment subtree.
	tr.OnPersistedEntry("s", []string{"/etc/passwd"}, 1)
	// ".." traversal rejected too.
	tr.OnPersistedEntry("s", []string{"../../../escape.png"}, 2)
	// Path outside the attachment subtree (rooted at ws but not in
	// .naozhi/attachments/).
	tr.OnPersistedEntry("s", []string{"other.png"}, 3)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	if obs.bump.Load() != 0 {
		t.Errorf("bumps=%d, expected 0 (all escapes rejected)", obs.bump.Load())
	}
	if obs.errs.Load() != 0 {
		t.Errorf("errs=%d (should silently drop, not error)", obs.errs.Load())
	}
}

// TestOnPersistedEntry_UnknownKeyhash: the WorkspaceResolver returns
// "" → the tracker drops the bump silently (no .meta churn on
// deleted-but-still-scheduled sessions).
func TestOnPersistedEntry_UnknownKeyhash(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, metaPath := writeAttachment(t, ws, date, "u",
		attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr, err := NewTracker(Options{
		Workspaces:     func(k string) string { return "" },
		CoalesceWindow: 100 * time.Millisecond,
		ChannelBuffer:  64,
		Observer:       obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Stop(context.Background()) })
	tr.OnPersistedEntry("unknown-sess", []string{rel}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	raw, _ := os.ReadFile(metaPath)
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if len(m.ReferencingKeyHashes) != 0 {
		t.Errorf("refs=%v, expected no bump", m.ReferencingKeyHashes)
	}
}

// TestOnSessionRemoved_ClearsRefs: after bumping two attachments
// then invoking OnSessionRemoved, both .meta files drop the keyhash.
func TestOnSessionRemoved_ClearsRefs(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel1, meta1 := writeAttachment(t, ws, date, "r1", attachment.Meta{UploadedAt: time.Now()})
	rel2, meta2 := writeAttachment(t, ws, date, "r2", attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	tr.OnPersistedEntry("sessX", []string{rel1, rel2}, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	// Pre-check: both metas have the reference.
	for _, mp := range []string{meta1, meta2} {
		raw, _ := os.ReadFile(mp)
		var m attachment.Meta
		_ = json.Unmarshal(raw, &m)
		if !m.HasReference("sessX") {
			t.Fatalf("reference not set in %s: %v", mp, m)
		}
	}

	if err := tr.OnSessionRemoved(ctx, "sessX", ws); err != nil {
		t.Fatalf("OnSessionRemoved: %v", err)
	}
	// Post-condition: keyhash removed from both.
	for _, mp := range []string{meta1, meta2} {
		raw, _ := os.ReadFile(mp)
		var m attachment.Meta
		_ = json.Unmarshal(raw, &m)
		if m.HasReference("sessX") {
			t.Errorf("reference still present in %s", mp)
		}
	}
	if obs.clear.Load() != 2 {
		t.Errorf("OnReferenceClear=%d, want 2", obs.clear.Load())
	}
}

// TestOnSessionRemoved_UnknownSession is a no-op — no meta has the
// keyhash, so nothing is rewritten.
func TestOnSessionRemoved_UnknownSession(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	writeAttachment(t, ws, date, "u", attachment.Meta{
		UploadedAt:           time.Now(),
		ReferencingKeyHashes: []string{"otherSess"},
	})

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.OnSessionRemoved(ctx, "ghostSess", ws); err != nil {
		t.Fatalf("OnSessionRemoved: %v", err)
	}
	if obs.clear.Load() != 0 {
		t.Errorf("OnReferenceClear=%d, want 0", obs.clear.Load())
	}
}

// TestChannelFull_Drops exercises the non-blocking drop path. With
// a tiny buffer + synchronous worker blocked on a sleeping resolver,
// the caller's dispatch should enter the drop branch.
func TestChannelFull_Drops(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, _ := writeAttachment(t, ws, date, "q",
		attachment.Meta{UploadedAt: time.Now()})
	obs := &countingObs{}
	// Workspace resolver that sleeps — forces handleBump to stall
	// on the very first job, backing up the in channel.
	tr, err := NewTracker(Options{
		Workspaces: func(string) string {
			time.Sleep(200 * time.Millisecond)
			return ws
		},
		ChannelBuffer:  1,
		CoalesceWindow: 500 * time.Millisecond,
		Observer:       obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Stop(context.Background()) })

	// Blast many bumps.
	for i := 0; i < 50; i++ {
		tr.OnPersistedEntry("s", []string{rel}, int64(i))
	}
	// At least one should have been dropped.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if obs.drop.Load() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("no drops recorded under channel pressure (drop=%d)", obs.drop.Load())
}

// TestStop_DrainsPendingBumps: pending bumps not yet past coalesce
// window must still land on disk during Stop.
func TestStop_DrainsPendingBumps(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, metaPath := writeAttachment(t, ws, date, "drain",
		attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr, err := NewTracker(Options{
		Workspaces:     func(string) string { return ws },
		ChannelBuffer:  8,
		CoalesceWindow: 1 * time.Hour, // never auto-flush
		Observer:       obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.OnPersistedEntry("sess", []string{rel}, 42)
	time.Sleep(50 * time.Millisecond) // let the worker pick it up
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tr.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	raw, _ := os.ReadFile(metaPath)
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if !m.HasReference("sess") {
		t.Errorf("pending bump not flushed on Stop: %+v", m)
	}
}

// TestStop_Idempotent: calling Stop twice is fine.
func TestStop_Idempotent(t *testing.T) {
	tr, err := NewTracker(Options{
		Workspaces: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.Stop(ctx); err != nil {
		t.Fatalf("Stop 1: %v", err)
	}
	if err := tr.Stop(ctx); err != nil {
		t.Fatalf("Stop 2: %v", err)
	}
}

// TestOnPersistedEntry_AfterStop_NoOp: hot-path calls post-Stop
// must not panic or write.
func TestOnPersistedEntry_AfterStop_NoOp(t *testing.T) {
	ws := t.TempDir()
	tr := newTracker(t, ws, nil /* no obs — check no write */)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Stop(ctx)
	// Should not panic. If a .meta is missing, a stale bump would
	// hit err-write; with no bump nothing happens.
	tr.OnPersistedEntry("s", []string{"x"}, 1)
}

// TestWriterAlive_Lifecycle: idle tracker is alive (empty channel);
// still alive after draining a bump; false after Stop. "Never drained"
// is NOT unhealthy — quiet idle is a legitimate steady state.
func TestWriterAlive_Lifecycle(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, _ := writeAttachment(t, ws, date, "alive",
		attachment.Meta{UploadedAt: time.Now()})
	tr := newTracker(t, ws, nil)
	// Idle (never drained, channel empty) must be alive — a fresh
	// tracker with no image events is still healthy.
	if !tr.WriterAlive() {
		t.Errorf("WriterAlive=false when idle (cold start)")
	}
	tr.OnPersistedEntry("s", []string{rel}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)
	if !tr.WriterAlive() {
		t.Errorf("WriterAlive=false after successful drain")
	}
	_ = tr.Stop(ctx)
	if tr.WriterAlive() {
		t.Errorf("WriterAlive=true after Stop")
	}
}

// TestResolve_AbsolutePathUnderWorkspace: absolute path that DOES
// sit under workspace/attachments is accepted.
func TestResolve_AbsolutePathUnderWorkspace(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, metaPath := writeAttachment(t, ws, date, "abs",
		attachment.Meta{UploadedAt: time.Now()})
	absPath := filepath.Join(ws, filepath.FromSlash(rel))

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	tr.OnPersistedEntry("s", []string{absPath}, 99)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	raw, _ := os.ReadFile(metaPath)
	var m attachment.Meta
	_ = json.Unmarshal(raw, &m)
	if !m.HasReference("s") {
		t.Errorf("absolute path under workspace not accepted: %+v", m)
	}
}

// TestConcurrent_Bumps: many goroutines hammering the tracker with
// the same (keyhash, path) → exactly one .meta write + bump counter
// = 1. -race flags any shared-state mutation regression.
func TestConcurrent_Bumps(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, _ := writeAttachment(t, ws, date, "conc",
		attachment.Meta{UploadedAt: time.Now()})

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				tr.OnPersistedEntry("s", []string{rel}, int64(i*100+j))
			}
		}(i)
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	if obs.bump.Load() != 1 {
		t.Errorf("bumps=%d, want 1 (coalesce across goroutines)", obs.bump.Load())
	}
}

// TestErrWriteFailure: a bump on an attachment whose .meta was
// deleted → UpdateMetaFile returns error → Observer OnMetaWriteError
// called exactly once.
func TestErrWriteFailure(t *testing.T) {
	ws := t.TempDir()
	date := time.Now().Format("2006-01-02")
	rel, metaPath := writeAttachment(t, ws, date, "err",
		attachment.Meta{UploadedAt: time.Now()})
	// Remove the meta sidecar — bump will fail with
	// "meta sidecar missing" from UpdateMetaFile.
	os.Remove(metaPath)

	obs := &countingObs{}
	tr := newTracker(t, ws, obs)
	tr.OnPersistedEntry("s", []string{rel}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tr.Flush(ctx)

	if obs.errs.Load() != 1 {
		t.Errorf("errs=%d, want 1 (missing sidecar)", obs.errs.Load())
	}
	if obs.bump.Load() != 0 {
		t.Errorf("bump counter incremented despite error: %d", obs.bump.Load())
	}
}

// TestErrTrackerClosed_Import ensures the exported error doesn't
// drift in rename; callers outside the package rely on `errors.Is`.
func TestErrTrackerClosed_Import(t *testing.T) {
	if !errors.Is(ErrTrackerClosed, ErrTrackerClosed) {
		t.Fatal("sentinel not match-able with errors.Is")
	}
}
