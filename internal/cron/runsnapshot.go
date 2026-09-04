package cron

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
)

// SandboxRunSnapshot is the content-addressed record of a sandbox run's INPUT
// — everything needed to replay it into a fresh microVM:
//
//	<store-dir>/runsnapshots/<jobID>/<runID>.json   ← this manifest
//	<store-dir>/runsnapshots/blobs/<sha256>          ← deduped content blobs
//
// SECRETS RED LINE: the manifest stores secret REFERENCE NAMES only
// (SecretRefs), never values; replay re-resolves by reference at inject time.
// TestSnapshot_NeverPersistsSecretValues pins this.
type SandboxRunSnapshot struct {
	RunID string `json:"run_id"`
	JobID string `json:"job_id"`
	// PromptHash is the SHA-256 of the (agent-command-stripped) prompt; the
	// text itself lives deduped in the blob store.
	PromptHash string `json:"prompt_hash,omitempty"`
	// Model pins the CLI model the run requested ("" = image default).
	Model string `json:"model,omitempty"`
	// ImageVersion records the base image the run targeted so a replay can pin
	// it. Empty when unknown.
	ImageVersion string `json:"image_version,omitempty"`
	// SecretRefs are the NAMES of injected secrets — never the values.
	SecretRefs []string `json:"secret_refs,omitempty"`
	// SchemaV guards forward migrations of the manifest shape.
	SchemaV int `json:"schema_v"`
}

const sandboxSnapshotSchemaV = 1

// sandboxSnapshotDir resolves the snapshot tree root ("" when persistence
// is disabled — store-less test fixtures skip snapshots entirely).
func (s *Scheduler) sandboxSnapshotDir() string {
	return s.stateSubtree("runsnapshots")
}

// writeSandboxSnapshot persists one run's input manifest + prompt blob BEFORE
// the invoke so a replay can re-inject the exact input. Best-effort: a write
// failure logs and does not fail the run. Content-addressed: N runs sharing a
// prompt store one blob. Callers MUST NOT pass secret values in secretRefs —
// the manifest is plaintext on disk.
func (s *Scheduler) writeSandboxSnapshot(jobID, runID, prompt, model, imageVersion string, secretRefs []string, lg *slog.Logger) {
	root := s.sandboxSnapshotDir()
	if root == "" {
		return
	}
	// Path-traversal guard mirroring the readers: the test seam and any future
	// caller must not escape the snapshot root.
	if !IsValidID(jobID) || !IsValidID(runID) {
		lg.Warn("cron sandbox: snapshot write rejected non-hex id", "job_id", jobID, "run_id", runID)
		return
	}
	promptHash, err := s.writeSnapshotBlob(root, prompt)
	if err != nil {
		lg.Warn("cron sandbox: snapshot blob write failed; replay unavailable for this run", "err", err)
		return
	}
	man := SandboxRunSnapshot{
		RunID:        runID,
		JobID:        jobID,
		PromptHash:   promptHash,
		Model:        model,
		ImageVersion: imageVersion,
		SecretRefs:   secretRefs,
		SchemaV:      sandboxSnapshotSchemaV,
	}
	b, err := json.Marshal(man)
	if err != nil {
		lg.Warn("cron sandbox: snapshot manifest marshal failed", "err", err)
		return
	}
	dir := filepath.Join(root, jobID)
	if err := s.mkdirStateSubtree(dir); err != nil {
		lg.Warn("cron sandbox: snapshot dir create failed; replay unavailable", "err", err)
		return
	}
	// Atomic write to match writeSnapshotBlob's tmp+rename: a truncated manifest
	// would dangle a hash to an unparseable blob.
	if err := osutil.WriteFileAtomic(filepath.Join(dir, runID+".json"), b, 0o600); err != nil {
		lg.Warn("cron sandbox: snapshot manifest write failed; replay unavailable", "err", err)
	}
}

// writeSnapshotBlob writes content to the content-addressed blob store and
// returns its SHA-256 hex hash. Idempotent: an existing blob (same hash) is
// left untouched (dedup, §5.2). Empty content returns "" with no write.
func (s *Scheduler) writeSnapshotBlob(root, content string) (string, error) {
	if content == "" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	blobDir := filepath.Join(root, "blobs")
	if err := s.mkdirStateSubtree(blobDir); err != nil {
		return "", fmt.Errorf("mkdir blob dir: %w", err)
	}
	path := filepath.Join(blobDir, hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil // dedup: blob already present
	}
	// Unique temp file + rename: a reader never sees a half-written blob, and two
	// writers racing the SAME hash cannot collide on one tmp path (a shared tmp
	// let the loser's Remove delete the winner's blob). The rename is idempotent
	// because same hash ⇒ same bytes.
	f, err := os.CreateTemp(blobDir, hash+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create blob tmp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write blob tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("sync blob tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close blob tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Another writer may have committed the identical blob between our
		// Stat and Rename — if the target now exists, our content is already
		// there (same hash), so treat it as success and drop our tmp.
		if _, statErr := os.Stat(path); statErr == nil {
			_ = os.Remove(tmp)
			return hash, nil
		}
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename blob: %w", err)
	}
	return hash, nil
}

// SandboxRunSnapshotManifest reads one run's input manifest for the §7.3
// snapshot panel / replay. Returns (nil, false, nil) when absent (a local
// run, snapshots-disabled deploy, or a run that predates snapshots) so the
// caller renders "snapshot unavailable" rather than an error. IDs are
// shape-validated (path-traversal guard).
func (s *Scheduler) SandboxRunSnapshotManifest(jobID, runID string) (*SandboxRunSnapshot, bool, error) {
	if s == nil || s.storePath == "" {
		return nil, false, nil
	}
	if !IsValidID(jobID) || !IsValidID(runID) {
		return nil, false, fmt.Errorf("cron sandbox: invalid jobID/runID")
	}
	path := filepath.Join(s.sandboxSnapshotDir(), jobID, runID+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cron sandbox: read snapshot manifest: %w", err)
	}
	var man SandboxRunSnapshot
	if err := json.Unmarshal(b, &man); err != nil {
		return nil, false, fmt.Errorf("cron sandbox: parse snapshot manifest: %w", err)
	}
	return &man, true, nil
}

// SandboxRunSnapshotPrompt reads the prompt blob a manifest references. The
// hash is content-addressed, so this is the exact prompt the run used — even
// if the job's current prompt has since been edited (§5.2). Returns ""
// when the manifest has no prompt hash. blobHash is shape-validated (hex).
func (s *Scheduler) SandboxRunSnapshotPrompt(blobHash string) (string, error) {
	if s == nil || s.storePath == "" || blobHash == "" {
		return "", nil
	}
	if !isSHA256Hex(blobHash) {
		return "", fmt.Errorf("cron sandbox: invalid blob hash")
	}
	path := filepath.Join(s.sandboxSnapshotDir(), "blobs", blobHash)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // blob GC'd or never written
		}
		return "", fmt.Errorf("cron sandbox: read snapshot blob: %w", err)
	}
	return string(b), nil
}

// deleteJobSnapshots removes a deleted job's snapshot manifest subtree
// (runsnapshots/<jobID>/). Best-effort: a missing tree is fine. Content-
// addressed blobs are deliberately NOT touched (shared across jobs); see
// the deleteJobRuns TODO for the blob GC follow-up.
func (s *Scheduler) deleteJobSnapshots(jobID string) {
	root := s.sandboxSnapshotDir()
	if root == "" || !IsValidID(jobID) {
		return
	}
	dir := filepath.Join(root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron sandbox: snapshot subtree delete failed", "job_id", jobID, "err", err)
	}
}

// WriteSandboxSnapshotForTest is an exported seam so consumer-package tests
// (dashboard handlers) can stage a snapshot without driving a full run.
// Production code uses the unexported writeSandboxSnapshot. NOT for runtime
// use — mirrors agentcore.NewWithAPIForTest.
func (s *Scheduler) WriteSandboxSnapshotForTest(jobID, runID, prompt, model, imageVersion string, secretRefs []string) {
	s.writeSandboxSnapshot(jobID, runID, prompt, model, imageVersion, secretRefs, slog.Default())
}

// isSHA256Hex reports whether s is exactly 64 lowercase hex chars — the
// shape writeSnapshotBlob produces. Guards the blob path against traversal.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
