package attachment

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// seedReapable drops `n` past-upload-TTL, unreferenced attachments
// (each with a .meta) into a single old day directory. Returns the day
// dir absolute path.
func seedReapable(t *testing.T, ws string, n int, now time.Time) string {
	t.Helper()
	day := now.AddDate(0, 0, -10).Format("2006-01-02")
	meta := Meta{UploadedAt: now.AddDate(0, 0, -10)}
	for i := 0; i < n; i++ {
		fixturePersisted(t, ws, day, stemf(i), meta)
	}
	return filepath.Join(ws, Dir, day)
}

func stemf(i int) string { return "reap-" + string(rune('a'+i%26)) + string(rune('0'+i/26)) }

// TestGCWithRefs_MaxRemoveCaps: with MaxRemove=2 over 5 reapable files,
// exactly 2 are deleted and Stopped is reported (RFC §4.6-2).
func TestGCWithRefs_MaxRemoveCaps(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	seedReapable(t, ws, 5, now)

	res, err := GCWithRefs(context.Background(), ws, GCOptions{
		UploadTTL: 7 * 24 * time.Hour,
		RefTTL:    DefaultRefTTL,
		Now:       now,
		MaxRemove: 2,
	})
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if res.Removed != 2 {
		t.Errorf("Removed=%d, want 2 (capped)", res.Removed)
	}
	if !res.Stopped {
		t.Errorf("Stopped=false, want true when cap hit")
	}
}

// TestGCWithRefs_DryRunBuckets: dry-run deletes nothing but reports
// WouldRemove bucketed by reason (RFC §6 E4).
func TestGCWithRefs_DryRunBuckets(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	day10 := now.AddDate(0, 0, -10).Format("2006-01-02")

	// 1) legacy: no .meta sidecar.
	dayDir := filepath.Join(ws, Dir, day10)
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dayDir, "legacy.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 2) meta, no refs.
	fixturePersisted(t, ws, day10, "norefs", Meta{UploadedAt: now.AddDate(0, 0, -10)})
	// 3) refs expired.
	fixturePersisted(t, ws, day10, "expired", Meta{
		UploadedAt:           now.AddDate(0, 0, -40),
		ReferencingKeyHashes: []string{"s"},
		LastReferencedAt:     now.AddDate(0, 0, -35).UnixMilli(),
	})

	res, err := GCWithRefs(context.Background(), ws, GCOptions{
		UploadTTL: 7 * 24 * time.Hour,
		RefTTL:    DefaultRefTTL,
		Now:       now,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d in dry-run, want 0", res.Removed)
	}
	for reason, want := range map[ReapReason]int{
		ReasonLegacyNoMeta: 1,
		ReasonMetaNoRefs:   1,
		ReasonRefsExpired:  1,
	} {
		if got := res.WouldRemove[reason]; got != want {
			t.Errorf("WouldRemove[%s]=%d, want %d", reason, got, want)
		}
	}
	// Files must still be on disk.
	if _, err := os.Stat(filepath.Join(dayDir, "legacy.png")); err != nil {
		t.Errorf("dry-run deleted legacy.png: %v", err)
	}
}

// TestGCWithRefs_CtxCancel: a cancelled context returns promptly with
// the context error and no further deletions (RFC §4.6-1, §10 F4).
func TestGCWithRefs_CtxCancel(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	seedReapable(t, ws, 5, now)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	res, err := GCWithRefs(ctx, ws, GCOptions{
		UploadTTL: 7 * 24 * time.Hour,
		RefTTL:    DefaultRefTTL,
		Now:       now,
	})
	if err == nil {
		t.Errorf("expected context error, got nil")
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d after immediate cancel, want 0", res.Removed)
	}
}

// TestGCWithRefs_MetaGraceSkips: a reapable file whose .meta was just
// modified is retained this sweep (F2 — closes the bump-race window).
func TestGCWithRefs_MetaGraceSkips(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	day := now.AddDate(0, 0, -10).Format("2006-01-02")
	payload, metaPath := fixturePersisted(t, ws, day, "fresh-meta",
		Meta{UploadedAt: now.AddDate(0, 0, -10)})

	// Touch the .meta to "now" so it falls inside the grace window.
	if err := os.Chtimes(metaPath, now, now); err != nil {
		t.Fatal(err)
	}

	res, err := GCWithRefs(context.Background(), ws, GCOptions{
		UploadTTL: 7 * 24 * time.Hour,
		RefTTL:    DefaultRefTTL,
		Now:       now,
		MetaGrace: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d, want 0 (within meta grace)", res.Removed)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Errorf("payload deleted despite meta grace: %v", err)
	}
}

// TestGCWithRefs_DeletesMetaFirst: verify deletion order leaves no
// orphan .meta. After a live reap, neither payload nor meta remains
// (F1). The ordering guarantee itself is covered structurally; here we
// assert the post-condition.
func TestGCWithRefs_DeletesMetaFirst(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	day := now.AddDate(0, 0, -10).Format("2006-01-02")
	payload, metaPath := fixturePersisted(t, ws, day, "ordered",
		Meta{UploadedAt: now.AddDate(0, 0, -10)})

	if _, err := GCWithRefs(context.Background(), ws, GCOptions{
		UploadTTL: 7 * 24 * time.Hour,
		RefTTL:    DefaultRefTTL,
		Now:       now,
	}); err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Errorf("orphan .meta remains: %v", err)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Errorf("payload remains: %v", err)
	}
}

// TestGCWithRefs_RefusesSymlinkedDayDir: a date-named symlink pointing
// outside the attachment root must not be traversed or deleted
// (security-critical, store.go Lstat guard).
func TestGCWithRefs_RefusesSymlinkedDayDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	ws := t.TempDir()
	now := time.Now().UTC()

	// A real directory full of "victim" data outside the attachments root.
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "important.txt")
	if err := os.WriteFile(victimFile, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(ws, Dir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Date-named symlink (old date so it would be reaped if followed).
	link := filepath.Join(root, now.AddDate(0, 0, -30).Format("2006-01-02"))
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	res, err := GCWithRefs(context.Background(), ws, GCOptions{
		UploadTTL: 7 * 24 * time.Hour,
		RefTTL:    DefaultRefTTL,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d, want 0 (symlink must be skipped)", res.Removed)
	}
	if _, err := os.Stat(victimFile); err != nil {
		t.Errorf("victim data deleted through symlink: %v", err)
	}
}

// TestGCWithRefs_CorruptMetaRetains: a corrupt .meta makes the
// keep-decision error out, and the file is retained (err-on-keep).
func TestGCWithRefs_CorruptMetaRetains(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	day := now.AddDate(0, 0, -10).Format("2006-01-02")
	dayDir := filepath.Join(ws, Dir, day)
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dayDir, "corrupt.png")
	if err := os.WriteFile(payload, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Corrupt JSON sidecar.
	if err := os.WriteFile(filepath.Join(dayDir, "corrupt.meta"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := GCWithRefs(context.Background(), ws, GCOptions{
		UploadTTL: 7 * 24 * time.Hour,
		RefTTL:    DefaultRefTTL,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("GCWithRefs: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed=%d, want 0 (corrupt meta → retain)", res.Removed)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Errorf("payload deleted despite corrupt meta: %v", err)
	}
}
