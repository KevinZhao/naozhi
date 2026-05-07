package attachment

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersist_WritesPayloadAndMeta(t *testing.T) {
	ws := t.TempDir()
	data := []byte("%PDF-1.4\nfake pdf bytes\n")
	meta := Meta{
		OrigName:   "report.pdf",
		MimeType:   "application/pdf",
		SessionKey: "dash:direct:alice:general",
		Owner:      "alice",
	}

	got, err := Persist(ws, data, ".pdf", meta)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}

	if got.Size != int64(len(data)) {
		t.Errorf("Size=%d want=%d", got.Size, len(data))
	}
	if !strings.HasPrefix(got.RelPath, ".naozhi/attachments/") {
		t.Errorf("RelPath=%q does not start with attachment dir", got.RelPath)
	}
	if !strings.HasSuffix(got.RelPath, ".pdf") {
		t.Errorf("RelPath=%q missing .pdf suffix", got.RelPath)
	}
	if strings.ContainsRune(got.RelPath, '\\') {
		t.Errorf("RelPath=%q contains backslash — must be forward-slash-only", got.RelPath)
	}

	// Payload verifiable on disk
	readBack, err := os.ReadFile(got.AbsPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(readBack) != string(data) {
		t.Errorf("payload mismatch")
	}

	// Meta sidecar present with expected fields
	base := filepath.Base(got.AbsPath)
	id := strings.TrimSuffix(base, ".pdf")
	metaPath := filepath.Join(filepath.Dir(got.AbsPath), id+".meta")
	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var gotMeta Meta
	if err := json.Unmarshal(metaRaw, &gotMeta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if gotMeta.OrigName != meta.OrigName {
		t.Errorf("meta.OrigName=%q want=%q", gotMeta.OrigName, meta.OrigName)
	}
	if gotMeta.Size != int64(len(data)) {
		t.Errorf("meta.Size=%d want=%d", gotMeta.Size, len(data))
	}
	if gotMeta.UploadedAt.IsZero() {
		t.Error("meta.UploadedAt should have been filled in")
	}
}

func TestPersist_RejectsEmptyWorkspace(t *testing.T) {
	_, err := Persist("", []byte("x"), ".pdf", Meta{})
	if !errors.Is(err, ErrWorkspaceRequired) {
		t.Errorf("err=%v want ErrWorkspaceRequired", err)
	}
}

func TestPersist_RejectsRelativeWorkspace(t *testing.T) {
	_, err := Persist("relative/path", []byte("x"), ".pdf", Meta{})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected absolute-path error, got %v", err)
	}
}

func TestPersist_RejectsEmptyData(t *testing.T) {
	_, err := Persist(t.TempDir(), nil, ".pdf", Meta{})
	if !errors.Is(err, ErrEmptyData) {
		t.Errorf("err=%v want ErrEmptyData", err)
	}
}

func TestPersist_RejectsUnknownExt(t *testing.T) {
	_, err := Persist(t.TempDir(), []byte("x"), ".exe", Meta{})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("expected unsupported-ext error, got %v", err)
	}
}

func TestPersist_GeneratesUniquePaths(t *testing.T) {
	ws := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got, err := Persist(ws, []byte("pdf"), ".pdf", Meta{})
		if err != nil {
			t.Fatalf("Persist #%d: %v", i, err)
		}
		if seen[got.RelPath] {
			t.Fatalf("collision on iteration %d: %q", i, got.RelPath)
		}
		seen[got.RelPath] = true
	}
}

func TestRemove_DeletesPayloadAndMeta(t *testing.T) {
	ws := t.TempDir()
	got, err := Persist(ws, []byte("pdf"), ".pdf", Meta{})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	Remove(got.AbsPath)

	if _, err := os.Stat(got.AbsPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("payload should be gone, stat err=%v", err)
	}
	base := filepath.Base(got.AbsPath)
	id := strings.TrimSuffix(base, ".pdf")
	metaPath := filepath.Join(filepath.Dir(got.AbsPath), id+".meta")
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("meta should be gone, stat err=%v", err)
	}
}

func TestRemove_SilentOnMissing(t *testing.T) {
	// Must not panic.
	Remove("")
	Remove("/nonexistent/path/that/was/never/there.pdf")
}

func TestGC_DropsOldDatesKeepsRecent(t *testing.T) {
	ws := t.TempDir()
	// Seed an old + a recent date dir.
	root := filepath.Join(ws, Dir)
	oldDir := filepath.Join(root, "2026-04-01")
	newDir := filepath.Join(root, "2026-05-06")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop marker files so we can verify the sweep.
	if err := os.WriteFile(filepath.Join(oldDir, "x.pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "y.pdf"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cutoff: anything older than 7 days from 2026-05-06 should die.
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	n, err := GC(ws, 7*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 1 {
		t.Errorf("removed=%d want=1", n)
	}
	if _, err := os.Stat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old dir should be gone")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("recent dir should remain, got %v", err)
	}
}

func TestGC_NoAttachmentDirIsNotAnError(t *testing.T) {
	ws := t.TempDir()
	n, err := GC(ws, time.Hour, time.Now())
	if err != nil {
		t.Errorf("GC on empty workspace returned err=%v", err)
	}
	if n != 0 {
		t.Errorf("removed=%d want=0", n)
	}
}

func TestGC_IgnoresNonDateDirectories(t *testing.T) {
	ws := t.TempDir()
	root := filepath.Join(ws, Dir)
	// An operator dropped a stray directory. GC must not touch it.
	strayDir := filepath.Join(root, "notes")
	if err := os.MkdirAll(strayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := GC(ws, time.Hour, time.Now().Add(1000*time.Hour))
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 0 {
		t.Errorf("removed=%d want=0", n)
	}
	if _, err := os.Stat(strayDir); err != nil {
		t.Errorf("stray dir should remain, got %v", err)
	}
}
