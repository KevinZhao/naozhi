package sandboxstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/textutil"
)

// RunSnapshot is the content-addressed record of a sandbox run's INPUT
// — everything needed to replay it into a fresh microVM:
//
//	<root>/runsnapshots/<jobID>/<runID>.json   ← this manifest
//	<root>/runsnapshots/blobs/<sha256>          ← deduped content blobs
//
// SECRETS RED LINE: the manifest stores secret REFERENCE NAMES only
// (SecretRefs), never values; replay re-resolves by reference at inject time.
// TestSnapshot_NeverPersistsSecretValues pins this.
type RunSnapshot struct {
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

// snapshotDir resolves the snapshot tree root ("" when persistence is disabled
// — store-less test fixtures skip snapshots entirely).
func (st Store) snapshotDir() string {
	return st.Subtree("runsnapshots")
}

// WriteSnapshot persists one run's input manifest + prompt blob BEFORE
// the invoke so a replay can re-inject the exact input. Best-effort: a write
// failure logs and does not fail the run. Content-addressed: N runs sharing a
// prompt store one blob. Callers MUST NOT pass secret values in secretRefs —
// the manifest is plaintext on disk.
func (st Store) WriteSnapshot(jobID, runID, prompt, model, imageVersion string, secretRefs []string, lg *slog.Logger) {
	root := st.snapshotDir()
	if root == "" {
		return
	}
	// Path-traversal guard mirroring the readers: the test seam and any future
	// caller must not escape the snapshot root.
	if !validID(jobID) || !validID(runID) {
		lg.Warn("cron sandbox: snapshot write rejected non-hex id", "job_id", jobID, "run_id", runID)
		return
	}
	// Shared hold from the blob write to the manifest write: the blob GC's
	// sweep takes blobMu exclusively, so it cannot remove the blob between
	// this run's touch and the manifest that references it.
	blobMu.RLock()
	defer blobMu.RUnlock()
	promptHash, err := st.writeBlob(root, prompt)
	if err != nil {
		lg.Warn("cron sandbox: snapshot blob write failed; replay unavailable for this run", "err", err)
		return
	}
	man := RunSnapshot{
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
	if err := st.MkdirSubtree(dir); err != nil {
		lg.Warn("cron sandbox: snapshot dir create failed; replay unavailable", "err", err)
		return
	}
	// Atomic write to match writeBlob's tmp+rename: a truncated manifest
	// would dangle a hash to an unparseable blob.
	if err := osutil.WriteFileAtomic(filepath.Join(dir, runID+".json"), b, 0o600); err != nil {
		lg.Warn("cron sandbox: snapshot manifest write failed; replay unavailable", "err", err)
	}
}

// writeBlob writes content to the content-addressed blob store and
// returns its SHA-256 hex hash. Idempotent: an existing blob (same hash) is
// kept (dedup, §5.2) but its mtime is refreshed, because the blob GC spares
// only young unreferenced blobs and this run's manifest has not landed yet.
// Anything else at the path (missing, a symlink, untouchable) is rewritten;
// the rename replaces a symlink rather than writing through it. Empty content
// returns "" with no write. Callers hold blobMu shared.
func (st Store) writeBlob(root, content string) (string, error) {
	if content == "" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	blobDir := filepath.Join(root, "blobs")
	if err := st.MkdirSubtree(blobDir); err != nil {
		return "", fmt.Errorf("mkdir blob dir: %w", err)
	}
	path := filepath.Join(blobDir, hash)
	if isRegularFile(path) {
		if now := time.Now(); os.Chtimes(path, now, now) == nil {
			return hash, nil // dedup: blob already present, now young again
		}
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
		// Chtimes and Rename — if the target now exists, our content is already
		// there (same hash), so treat it as success and drop our tmp.
		if isRegularFile(path) {
			_ = os.Remove(tmp)
			return hash, nil
		}
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename blob: %w", err)
	}
	return hash, nil
}

// SnapshotManifest reads one run's input manifest for the §7.3
// snapshot panel / replay. Returns (nil, false, nil) when absent (a local
// run, snapshots-disabled deploy, or a run that predates snapshots) so the
// caller renders "snapshot unavailable" rather than an error. IDs are
// shape-validated (path-traversal guard).
func (st Store) SnapshotManifest(jobID, runID string) (*RunSnapshot, bool, error) {
	if st.Root == "" {
		return nil, false, nil
	}
	if !validID(jobID) || !validID(runID) {
		return nil, false, fmt.Errorf("cron sandbox: invalid jobID/runID")
	}
	return readManifest(filepath.Join(st.snapshotDir(), jobID, runID+".json"))
}

// readManifest loads one manifest by path; (nil, false, nil)
// when absent. Shared by the reader API above and the blob GC's mark phase so
// the two cannot disagree about what counts as a live manifest.
func readManifest(path string) (*RunSnapshot, bool, error) {
	// jsonfile bounds the read, refuses a symlink, and moves an unparseable
	// manifest aside as .corrupt.<ts> instead of leaving it to be re-read and
	// re-failed on every reader call and every GC pass (#2709). A corrupt
	// manifest marks no blobs either way — it cannot name one — so the blob GC's
	// view is unchanged.
	man, outcome, err := jsonfile.Load[RunSnapshot](path, jsonfile.Options{
		MaxBytes: maxSnapshotManifestBytes,
		Label:    "cron sandbox snapshot manifest",
	})
	if err != nil {
		return nil, false, fmt.Errorf("cron sandbox: read snapshot manifest: %w", err)
	}
	switch outcome {
	case jsonfile.Absent:
		return nil, false, nil
	case jsonfile.Parsed:
		return &man, true, nil
	default:
		// The read that finds it reports "unreadable" rather than "no
		// snapshot". The file has been moved aside, so later reads see no
		// manifest; that is safe because replay refuses without one, and the
		// .corrupt sibling keeps the evidence. The blob GC's mark phase skips
		// errors already, so a corrupt manifest marks no blobs either way.
		return nil, false, fmt.Errorf("cron sandbox: unreadable snapshot manifest")
	}
}

// maxSnapshotManifestBytes caps one manifest. It carries a model name, an image
// tag, a prompt hash and a handful of secret REF names — never the prompt
// itself, which lives in the content-addressed blob — so 64 KiB is orders above
// any legitimate payload and bounds what a tampered file can allocate.
const maxSnapshotManifestBytes = 64 << 10

// SnapshotPrompt reads the prompt blob a manifest references. The
// hash is content-addressed, so this is the exact prompt the run used — even
// if the job's current prompt has since been edited (§5.2). Returns ""
// when the manifest has no prompt hash. blobHash is shape-validated (hex).
func (st Store) SnapshotPrompt(blobHash string) (string, error) {
	if st.Root == "" || blobHash == "" {
		return "", nil
	}
	if !isSHA256Hex(blobHash) {
		return "", fmt.Errorf("cron sandbox: invalid blob hash")
	}
	path := filepath.Join(st.snapshotDir(), "blobs", blobHash)
	b, err := readRegularBounded(path, textutil.MaxCronPromptBytes)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("cron sandbox: snapshot manifest references a missing blob; replay unavailable", "blob", blobHash)
			return "", nil
		}
		return "", fmt.Errorf("cron sandbox: read snapshot blob: %w", err)
	}
	// Content-addressed: bytes that do not hash to their name are not the
	// prompt the run used.
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != blobHash {
		return "", fmt.Errorf("cron sandbox: snapshot blob does not match its hash")
	}
	return string(b), nil
}

// isRegularFile reports whether path itself (not a symlink target) is a
// regular file — the only shape readRegularBounded accepts.
func isRegularFile(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// errNotRegular reports a path that is a symlink or not a regular file.
var errNotRegular = errors.New("not a regular file")

// readRegularBounded reads path when it is a regular file (not a symlink) of
// at most max bytes.
func readRegularBounded(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("over the %d-byte cap", max)
	}
	return b, nil
}

// DeleteJobSnapshots removes a deleted job's snapshot manifest subtree
// (runsnapshots/<jobID>/). Best-effort: a missing tree is fine. Content-
// addressed blobs are deliberately NOT touched (shared across jobs); GCBlobs
// collects the ones no manifest references any more.
func (st Store) DeleteJobSnapshots(jobID string) {
	root := st.snapshotDir()
	if root == "" || !validID(jobID) {
		return
	}
	dir := filepath.Join(root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron sandbox: snapshot subtree delete failed", "job_id", jobID, "err", err)
	}
}

// isSHA256Hex reports whether s is exactly 64 lowercase hex chars — the
// shape writeBlob produces. Guards the blob path against traversal.
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
