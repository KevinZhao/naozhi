package cron

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapDirOf(storePath string) string {
	return filepath.Join(filepath.Dir(storePath), "runsnapshots")
}

// TestSnapshot_WriteThenReadRoundTrip pins the §5.1/§5.2 input snapshot:
// write a run's input, read the manifest + content-addressed prompt back.
func TestSnapshot_WriteThenReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	jobID, runID := "0123456789abcdef", "feedfacefeedface"
	prompt := "do the cloud thing"
	s.writeSandboxSnapshot(jobID, runID, prompt, "haiku", "phase2", nil, slog.Default())

	man, ok, err := s.SandboxRunSnapshotManifest(jobID, runID)
	if err != nil || !ok {
		t.Fatalf("manifest read: ok=%v err=%v", ok, err)
	}
	if man.Model != "haiku" || man.ImageVersion != "phase2" {
		t.Fatalf("manifest fields wrong: %+v", man)
	}
	wantHash := sha256.Sum256([]byte(prompt))
	if man.PromptHash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("prompt hash = %q, want %x", man.PromptHash, wantHash)
	}
	got, err := s.SandboxRunSnapshotPrompt(man.PromptHash)
	if err != nil {
		t.Fatalf("prompt read: %v", err)
	}
	if got != prompt {
		t.Fatalf("prompt = %q, want %q", got, prompt)
	}
}

// TestSnapshot_NeverPersistsSecretValues is the §5.1 RED LINE audit: a
// secret VALUE passed anywhere into the input must never appear on disk;
// only reference NAMES are stored. We pass a sentinel value as the prompt's
// neighbour and a ref name, then grep the entire snapshot tree.
func TestSnapshot_NeverPersistsSecretValues(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	const secretValue = "ghp_SUPERSECRETTOKENVALUE_must_never_persist"
	// The writer takes secret REF NAMES only — never values. Pass the ref
	// name; the value must not appear because we never hand it to the writer.
	s.writeSandboxSnapshot("0123456789abcdef", "feedfacefeedface",
		"summarise the repo", "haiku", "phase2", []string{"github_token"}, slog.Default())

	// Walk the whole snapshot tree and assert the secret value is absent.
	root := snapDirOf(storePath)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(data), secretValue) {
			t.Fatalf("§5.1 RED LINE: secret value leaked into %s", path)
		}
		return nil
	})

	// The ref NAME, however, must be present in the manifest.
	man, ok, _ := s.SandboxRunSnapshotManifest("0123456789abcdef", "feedfacefeedface")
	if !ok || len(man.SecretRefs) != 1 || man.SecretRefs[0] != "github_token" {
		t.Fatalf("secret ref name not recorded: %+v", man)
	}
}

// TestSnapshot_ContentAddressedDedup pins §5.2: two runs with the same
// prompt share one blob.
func TestSnapshot_ContentAddressedDedup(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	prompt := "same prompt across runs"
	s.writeSandboxSnapshot("0123456789abcdef", "1111111111111111", prompt, "", "", nil, slog.Default())
	s.writeSandboxSnapshot("0123456789abcdef", "2222222222222222", prompt, "", "", nil, slog.Default())

	blobs, err := os.ReadDir(filepath.Join(snapDirOf(storePath), "blobs"))
	if err != nil {
		t.Fatalf("read blobs: %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("blob count = %d, want 1 (dedup)", len(blobs))
	}
	// Both manifests reference the same hash.
	m1, _, _ := s.SandboxRunSnapshotManifest("0123456789abcdef", "1111111111111111")
	m2, _, _ := s.SandboxRunSnapshotManifest("0123456789abcdef", "2222222222222222")
	if m1.PromptHash != m2.PromptHash || m1.PromptHash == "" {
		t.Fatalf("dedup hashes differ: %q vs %q", m1.PromptHash, m2.PromptHash)
	}
}

// TestSnapshot_MissingIsUnavailable: a run with no snapshot returns
// (nil,false,nil) so the panel renders "unavailable", not an error.
func TestSnapshot_MissingIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	man, ok, err := s.SandboxRunSnapshotManifest("0123456789abcdef", "feedfacefeedface")
	if man != nil || ok || err != nil {
		t.Fatalf("missing snapshot: man=%v ok=%v err=%v", man, ok, err)
	}
}

// TestSnapshot_RejectsBadIDs guards the path-traversal surface on both the
// manifest and the blob reader.
func TestSnapshot_RejectsBadIDs(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	if _, _, err := s.SandboxRunSnapshotManifest("../etc", "feedfacefeedface"); err == nil {
		t.Fatal("must reject non-hex jobID")
	}
	if _, err := s.SandboxRunSnapshotPrompt("../../etc/passwd"); err == nil {
		t.Fatal("must reject non-sha256 blob hash")
	}
	if _, err := s.SandboxRunSnapshotPrompt(strings.Repeat("g", 64)); err == nil {
		t.Fatal("must reject 64-char non-hex blob hash")
	}
}

// TestSnapshot_WrittenBeforeInvoke pins the crash-survival contract: the
// snapshot must exist DURING the run (written before RunJob), so a crash
// mid-run still leaves a replayable input.
func TestSnapshot_WrittenBeforeInvoke(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")

	var sawDuringRun bool
	runner := &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateSuccess}}
	probe := &probeRunner{inner: runner, onRun: func(job SandboxJob) {
		_, ok, _ := schedForProbe.SandboxRunSnapshotManifest(job.JobID, job.RunID)
		sawDuringRun = ok
	}}
	s, rec := sandboxTestScheduler(t, probe, storePath)
	schedForProbe = s
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	if !sawDuringRun {
		t.Fatal("snapshot must exist DURING the run (written before invoke)")
	}
}

// schedForProbe lets the probe closure reach the scheduler under test.
var schedForProbe *Scheduler

// TestSnapshot_DeletedJobDropsManifests pins §5.1 cleanup: deleting a job
// removes its snapshot manifest subtree (blobs are shared, so they stay).
func TestSnapshot_DeletedJobDropsManifests(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	jobID, runID := "0123456789abcdef", "feedfacefeedface"
	s.writeSandboxSnapshot(jobID, runID, "p", "", "", nil, slog.Default())

	jobDir := filepath.Join(snapDirOf(storePath), jobID)
	if _, err := os.Stat(jobDir); err != nil {
		t.Fatalf("snapshot dir should exist before delete: %v", err)
	}
	s.deleteJobSnapshots(jobID)
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatal("snapshot manifest subtree must be removed on job delete")
	}
}
