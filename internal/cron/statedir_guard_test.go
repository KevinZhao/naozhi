//go:build !windows

package cron

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #2166: every sandbox-state writer routes its MkdirAll through
// mkdirStateSubtree, which refuses a subtree that resolved to a symlink. A
// planted `<stateDir>/<subtree> → /elsewhere` must NOT redirect writes into the
// attacker-chosen target. These tests pre-create each subtree as a symlink to a
// separate temp dir, drive the writer, and assert (a) the write was refused
// and (b) no file landed at the symlink target.

// newStoreDir returns a scheduler whose store directory is a fresh temp dir,
// plus that directory path. The store file itself need not exist for the
// state-subtree writers (they only derive siblings of its dir).
func guardScheduler(t *testing.T) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	return s, dir
}

// plantSymlink replaces <storeDir>/<name> with a symlink to a fresh target
// dir, returning the target. Skips the test on platforms without symlink
// support (mirrors ensurejobdir_root_fsync_test.go).
func plantSymlink(t *testing.T, storeDir, name string) string {
	t.Helper()
	target := t.TempDir()
	link := filepath.Join(storeDir, name)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	return target
}

// assertTargetEmpty fails if any entry landed under the symlink target.
func assertTargetEmpty(t *testing.T, target, what string) {
	t.Helper()
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s: %d file(s) landed at symlink target; write should have been refused", what, len(entries))
	}
}

func TestStateDirGuard_PendingRefusesSymlink(t *testing.T) {
	s, dir := guardScheduler(t)
	target := plantSymlink(t, dir, "sandboxpending")

	path := s.writeSandboxPending(sandboxPending{
		JobID:            "0123456789abcdef",
		RunID:            "feedfacefeedface",
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	}, slog.Default())

	if path != "" {
		t.Fatalf("writeSandboxPending returned %q; want '' (symlink subtree must be refused)", path)
	}
	assertTargetEmpty(t, target, "sandboxpending")
}

func TestStateDirGuard_AttentionRefusesSymlink(t *testing.T) {
	s, dir := guardScheduler(t)
	target := plantSymlink(t, dir, "sandboxattention")

	s.writeSandboxAttention(sandboxAttention{
		JobID:       "0123456789abcdef",
		RunID:       "feedfacefeedface",
		Reason:      attentionReasonTransport,
		CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())

	assertTargetEmpty(t, target, "sandboxattention")
}

func TestStateDirGuard_SnapshotRefusesSymlink(t *testing.T) {
	s, dir := guardScheduler(t)
	// runsnapshots is the snapshot root; both the manifest subdir and the
	// blobs/ subdir live under it. A symlinked root must refuse the blob write
	// (which happens first) and never reach the manifest write.
	target := plantSymlink(t, dir, "runsnapshots")

	s.writeSandboxSnapshot("0123456789abcdef", "feedfacefeedface", "the prompt", "", "", nil, slog.Default())

	assertTargetEmpty(t, target, "runsnapshots")
}

func TestStateDirGuard_BlobRefusesSymlink(t *testing.T) {
	s, dir := guardScheduler(t)
	// Pre-create runsnapshots as a real dir, then symlink only its blobs/
	// subdir. writeSnapshotBlob's mkdirStateSubtree(blobs) must refuse.
	root := filepath.Join(dir, "runsnapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "blobs")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	s.writeSandboxSnapshot("0123456789abcdef", "feedfacefeedface", "the prompt", "", "", nil, slog.Default())

	assertTargetEmpty(t, target, "runsnapshots/blobs")
	// The manifest must not have been written either (blob write failed first).
	if _, err := os.Stat(filepath.Join(root, "0123456789abcdef", "feedfacefeedface.json")); err == nil {
		t.Fatal("snapshot manifest written despite blob-dir symlink refusal")
	}
}

func TestStateDirGuard_EventSinkRefusesSymlink(t *testing.T) {
	s, dir := guardScheduler(t)
	// sandboxevents/<jobID> is created per run; symlink the sandboxevents
	// parent so the per-job MkdirAll resolves through it.
	target := plantSymlink(t, dir, "sandboxevents")

	sink, closeSink := s.sandboxEventSink("0123456789abcdef", "feedfacefeedface", slog.Default())
	// Degraded no-op sink: write must not panic and must not land a file.
	if err := sink([]byte(`{"k":"v"}`)); err != nil {
		t.Fatalf("degraded sink returned error: %v", err)
	}
	closeSink()

	assertTargetEmpty(t, target, "sandboxevents")
}

// TestStateDirGuard_HappyPath: with a normal (non-symlink) store dir, every
// writer succeeds, creates its subtree 0700, and lands its file.
func TestStateDirGuard_HappyPath(t *testing.T) {
	s, dir := guardScheduler(t)

	// Pending.
	path := s.writeSandboxPending(sandboxPending{
		JobID:            "0123456789abcdef",
		RunID:            "feedfacefeedface",
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	}, slog.Default())
	if path == "" {
		t.Fatal("writeSandboxPending returned '' on a normal dir; want a path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending file not written: %v", err)
	}
	assertMode0700(t, filepath.Join(dir, "sandboxpending"))

	// Attention.
	s.writeSandboxAttention(sandboxAttention{
		JobID:       "0123456789abcdef",
		RunID:       "feedfacefeedface",
		Reason:      attentionReasonTransport,
		CreatedAtMS: time.Now().UnixMilli(),
	}, slog.Default())
	if s.SandboxAttentionCount() != 1 {
		t.Fatalf("attention count = %d, want 1 on a normal dir", s.SandboxAttentionCount())
	}
	assertMode0700(t, filepath.Join(dir, "sandboxattention"))

	// Snapshot + blob.
	s.writeSandboxSnapshot("0123456789abcdef", "feedfacefeedface", "the prompt", "", "", nil, slog.Default())
	man, ok, err := s.SandboxRunSnapshotManifest("0123456789abcdef", "feedfacefeedface")
	if err != nil || !ok || man == nil {
		t.Fatalf("snapshot manifest not readable on a normal dir: ok=%v err=%v", ok, err)
	}
	assertMode0700(t, filepath.Join(dir, "runsnapshots"))
	assertMode0700(t, filepath.Join(dir, "runsnapshots", "blobs"))

	// Event sink.
	sink, closeSink := s.sandboxEventSink("0123456789abcdef", "abcabcabcabcabc1", slog.Default())
	if err := sink([]byte(`{"k":"v"}`)); err != nil {
		t.Fatalf("event sink write error on normal dir: %v", err)
	}
	closeSink()
	assertMode0700(t, filepath.Join(dir, "sandboxevents", "0123456789abcdef"))
	if _, err := os.Stat(filepath.Join(dir, "sandboxevents", "0123456789abcdef", "abcabcabcabcabc1.ndjson")); err != nil {
		t.Fatalf("event log file not written: %v", err)
	}
}

func assertMode0700(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s mode = %v, want 0700", dir, fi.Mode().Perm())
	}
}
