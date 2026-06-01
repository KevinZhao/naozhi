package sysession

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
)

type fakeRoots struct{ roots []string }

func (f fakeRoots) KnownWorkspaceRoots() []string { return f.roots }

// seedOldAttachment drops a past-upload-TTL, unreferenced attachment
// (payload + .meta) into <ws>/.naozhi/attachments/<10d-ago>/.
func seedOldAttachment(t *testing.T, ws, stem string, now time.Time) string {
	t.Helper()
	day := now.AddDate(0, 0, -10).Format("2006-01-02")
	dir := filepath.Join(ws, attachment.Dir, day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, stem+".png")
	if err := os.WriteFile(payload, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := attachment.Meta{UploadedAt: now.AddDate(0, 0, -10)}
	buf, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, stem+".meta"), buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return payload
}

func newTestGC(roots WorkspaceRootLister, now time.Time) *attachmentGC {
	d, _ := newAttachmentGC(DaemonDeps{WorkspaceRoots: roots})
	gc := d.(*attachmentGC)
	gc.nowFn = func() time.Time { return now }
	// Fixtures write .meta at real wall-clock now, so the default 5m
	// meta-grace would skip every freshly-seeded attachment. Disable it
	// for tests that aren't specifically exercising the grace window.
	gc.metaGrace = 0
	return gc
}

// TestAttachmentGC_NilRootsNoOp: a daemon with no lister wired logs and
// returns an empty report rather than panicking.
func TestAttachmentGC_NilRootsNoOp(t *testing.T) {
	gc := newTestGC(nil, time.Now())
	rep, err := gc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rep.Examined != 0 || rep.Acted != 0 {
		t.Errorf("expected empty report, got %+v", rep)
	}
}

// TestAttachmentGC_ReapsAcrossRoots: two distinct workspace roots each
// with an old attachment — both get reaped, Acted == 2.
func TestAttachmentGC_ReapsAcrossRoots(t *testing.T) {
	now := time.Now().UTC()
	ws1, ws2 := t.TempDir(), t.TempDir()
	p1 := seedOldAttachment(t, ws1, "a", now)
	p2 := seedOldAttachment(t, ws2, "b", now)

	gc := newTestGC(fakeRoots{[]string{ws1, ws2}}, now)
	rep, err := gc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rep.Examined != 2 {
		t.Errorf("Examined=%d, want 2", rep.Examined)
	}
	if rep.Acted != 2 {
		t.Errorf("Acted=%d, want 2", rep.Acted)
	}
	for _, p := range []string{p1, p2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("payload %s not removed: %v", p, err)
		}
	}
}

// TestAttachmentGC_DryRunDeletesNothing: dry-run reports work but leaves
// files on disk.
func TestAttachmentGC_DryRunDeletesNothing(t *testing.T) {
	now := time.Now().UTC()
	ws := t.TempDir()
	p := seedOldAttachment(t, ws, "a", now)

	gc := newTestGC(fakeRoots{[]string{ws}}, now)
	gc.dryRun = true
	rep, err := gc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rep.Acted != 0 {
		t.Errorf("Acted=%d in dry-run, want 0", rep.Acted)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("dry-run deleted payload: %v", err)
	}
}

// TestAttachmentGC_PerRootCapAndCursor: a root with 3 reapable files and
// per_root_cap=1 deletes 1 per tick; the round-robin cursor advances so
// repeated ticks drain it and also visit the second root.
func TestAttachmentGC_PerRootCapAndCursor(t *testing.T) {
	now := time.Now().UTC()
	ws1, ws2 := t.TempDir(), t.TempDir()
	seedOldAttachment(t, ws1, "a", now)
	seedOldAttachment(t, ws1, "b", now)
	seedOldAttachment(t, ws2, "c", now)

	gc := newTestGC(fakeRoots{[]string{ws1, ws2}}, now)
	gc.perRootCap = 1

	total := 0
	for i := 0; i < 5; i++ {
		rep, err := gc.Tick(context.Background())
		if err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
		total += rep.Acted
	}
	if total != 3 {
		t.Errorf("drained %d files over 5 ticks, want 3", total)
	}
}

// TestAttachmentGC_BadRootSkipped: a relative / empty root is skipped
// and bucketed, not fatal.
func TestAttachmentGC_BadRootSkipped(t *testing.T) {
	now := time.Now().UTC()
	ws := t.TempDir()
	seedOldAttachment(t, ws, "a", now)

	gc := newTestGC(fakeRoots{[]string{"", "relative/path", ws}}, now)
	rep, err := gc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rep.Acted != 1 {
		t.Errorf("Acted=%d, want 1 (only the abs root)", rep.Acted)
	}
	if rep.Skipped["bad_root"] != 2 {
		t.Errorf("bad_root=%d, want 2", rep.Skipped["bad_root"])
	}
}

// TestAttachmentGC_ConfigureValidatesTTL: ref_ttl < upload_ttl errors.
func TestAttachmentGC_ConfigureValidatesTTL(t *testing.T) {
	d, _ := newAttachmentGC(DaemonDeps{})
	err := d.(Configurable).Configure(DaemonConfig{
		"upload_ttl": 30 * 24 * time.Hour,
		"ref_ttl":    7 * 24 * time.Hour,
	})
	if err == nil {
		t.Fatal("expected error when ref_ttl < upload_ttl")
	}
}

// TestAttachmentGC_ConfigureAppliesKnobs: valid knobs land on the daemon.
func TestAttachmentGC_ConfigureAppliesKnobs(t *testing.T) {
	d, _ := newAttachmentGC(DaemonDeps{})
	gc := d.(*attachmentGC)
	if err := d.(Configurable).Configure(DaemonConfig{
		"upload_ttl":   3 * 24 * time.Hour,
		"ref_ttl":      60 * 24 * time.Hour,
		"per_root_cap": 42,
		"meta_grace":   2 * time.Minute,
		"dry_run":      true,
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if gc.uploadTTL != 3*24*time.Hour || gc.refTTL != 60*24*time.Hour ||
		gc.perRootCap != 42 || gc.metaGrace != 2*time.Minute || !gc.dryRun {
		t.Errorf("knobs not applied: %+v", gc)
	}
}

// TestAttachmentGC_CtxCancelStops: a cancelled context halts the sweep
// promptly without touching files.
func TestAttachmentGC_CtxCancelStops(t *testing.T) {
	now := time.Now().UTC()
	ws := t.TempDir()
	p := seedOldAttachment(t, ws, "a", now)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	gc := newTestGC(fakeRoots{[]string{ws}}, now)
	_, err := gc.Tick(ctx)
	if err == nil {
		t.Errorf("expected ctx error")
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("payload deleted despite cancelled ctx: %v", err)
	}
}
