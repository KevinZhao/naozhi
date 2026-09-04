// Package attachment persists user-uploaded files into the session workspace
// so Claude can reach them via its native Read tool (formats whose base64
// size exceeds the stdin line cap, cli.maxStdinLineBytes).
//
// Layout: <workspace>/.naozhi/attachments/<yyyy-mm-dd>/<uuid>.<ext> plus a
// <uuid>.meta sidecar. UUID filenames keep the original (possibly sensitive)
// filename out of paths the model sees; the .meta carries it for UI display.
package attachment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// Dir is the subtree under the session workspace where attachments live.
// A var (not const) so tests can shrink it; external code treats it as read-only.
var Dir = filepath.Join(".naozhi", "attachments")

// Errors surfaced to HTTP callers; messages stay generic (workspace paths
// are operator-only and must not be echoed to clients).
var (
	ErrWorkspaceRequired = errors.New("attachment: workspace is required")
	ErrEmptyData         = errors.New("attachment: data is empty")
)

// Meta is the sidecar stored alongside each attachment. Fields are stable
// JSON; unknown fields on read are ignored.
type Meta struct {
	OrigName   string    `json:"orig_name"`
	MimeType   string    `json:"mime_type"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
	// SessionKey is audit-only. The GC does not key on it: attachments are
	// tied to a workspace, not a session, because multiple sessions can share
	// a workspace.
	SessionKey string `json:"session_key,omitempty"`
	// Owner is the dashboard-auth-derived identifier from uploadOwner().
	// Internal logs only — do not surface to other users.
	Owner string `json:"owner,omitempty"`

	// ReferencingKeyHashes is the sorted set of session key-hashes
	// (persist.KeyHash) whose event log references this attachment, kept by
	// the tracker. Empty means the upload-time TTL alone decides GC;
	// non-empty keeps the file while refTTL (from LastReferencedAt) holds.
	ReferencingKeyHashes []string `json:"referencing_keyhashes,omitempty"`

	// LastReferencedAt is the latest unix-ms moment any session's event log
	// was observed referencing this attachment. Zero: never observed.
	LastReferencedAt int64 `json:"last_referenced_at,omitempty"`
}

// AddReference inserts keyhash into ReferencingKeyHashes keeping the slice
// sorted + deduplicated. Returns true if the slice actually changed, which
// the tracker uses to skip unnecessary .meta rewrites.
func (m *Meta) AddReference(keyhash string) bool {
	if keyhash == "" {
		return false
	}
	lo, hi := 0, len(m.ReferencingKeyHashes)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case m.ReferencingKeyHashes[mid] < keyhash:
			lo = mid + 1
		case m.ReferencingKeyHashes[mid] > keyhash:
			hi = mid
		default:
			return false
		}
	}
	m.ReferencingKeyHashes = append(m.ReferencingKeyHashes, "")
	copy(m.ReferencingKeyHashes[lo+1:], m.ReferencingKeyHashes[lo:])
	m.ReferencingKeyHashes[lo] = keyhash
	return true
}

// RemoveReference drops keyhash from ReferencingKeyHashes (if present).
// Returns true when the slice shrank.
func (m *Meta) RemoveReference(keyhash string) bool {
	if keyhash == "" {
		return false
	}
	lo, hi := 0, len(m.ReferencingKeyHashes)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case m.ReferencingKeyHashes[mid] < keyhash:
			lo = mid + 1
		case m.ReferencingKeyHashes[mid] > keyhash:
			hi = mid
		default:
			m.ReferencingKeyHashes = append(
				m.ReferencingKeyHashes[:mid],
				m.ReferencingKeyHashes[mid+1:]...,
			)
			return true
		}
	}
	return false
}

// HasReference reports membership without mutating.
func (m *Meta) HasReference(keyhash string) bool {
	lo, hi := 0, len(m.ReferencingKeyHashes)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case m.ReferencingKeyHashes[mid] < keyhash:
			lo = mid + 1
		case m.ReferencingKeyHashes[mid] > keyhash:
			hi = mid
		default:
			return true
		}
	}
	return false
}

// Persisted is what Persist returns: enough to build a cli.Attachment with
// Kind=KindFileRef without the caller having to re-stat the file.
type Persisted struct {
	// RelPath is workspace-relative with forward slashes (for the CLI Read tool).
	RelPath string
	// AbsPath is for the HTTP handler's rollback on downstream failure; not for the model.
	AbsPath string
	// Size is the byte count written.
	Size int64
}

// Persist writes data to a fresh UUID-named file under the workspace
// attachment directory, together with its .meta sidecar. workspace MUST be
// an absolute path that already exists (not created here, so a misconfigured
// workspace is not masked). ext must include the leading dot and is clamped
// to a tiny allowlist so ".."/"/"/NUL from user input cannot slip in.
func Persist(workspace string, data []byte, ext string, meta Meta) (Persisted, error) {
	if workspace == "" {
		return Persisted{}, ErrWorkspaceRequired
	}
	if !filepath.IsAbs(workspace) {
		return Persisted{}, fmt.Errorf("attachment: workspace must be absolute, got %q", workspace)
	}
	if len(data) == 0 {
		return Persisted{}, ErrEmptyData
	}
	cleanExt, err := sanitizeExt(ext)
	if err != nil {
		return Persisted{}, err
	}

	// UTC date dir: one timezone for GC and operators, no DST day-boundary race.
	dateDir := time.Now().UTC().Format("2006-01-02")
	absDir := filepath.Join(workspace, Dir, dateDir)
	// 0o700: co-resident users on a multi-tenant host must not be able to
	// walk the attachments subtree.
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return Persisted{}, fmt.Errorf("mkdir %s: %w", absDir, err)
	}

	id, err := newID()
	if err != nil {
		return Persisted{}, err
	}
	baseName := id + cleanExt
	absPath := filepath.Join(absDir, baseName)
	metaPath := filepath.Join(absDir, id+".meta")

	// Payload first, atomically; if meta fails afterwards the payload is
	// rolled back so the caller never sees a half-committed attachment.
	// 0o600 pairs with the 0o700 dir mode above.
	if err := osutil.WriteFileAtomic(absPath, data, 0o600); err != nil {
		return Persisted{}, err
	}

	if meta.Size == 0 {
		meta.Size = int64(len(data))
	}
	if meta.UploadedAt.IsZero() {
		meta.UploadedAt = time.Now().UTC()
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		_ = os.Remove(absPath)
		return Persisted{}, fmt.Errorf("marshal meta: %w", err)
	}
	// Meta carries Owner / SessionKey — owner-only like the payload.
	if err := osutil.WriteFileAtomic(metaPath, metaBytes, 0o600); err != nil {
		_ = os.Remove(absPath)
		return Persisted{}, err
	}

	// Forward slashes regardless of host OS: backslashes would be rejected
	// or misresolved by the CLI Read tool.
	rel := path.Join(Dir, dateDir, baseName)
	rel = strings.ReplaceAll(rel, `\`, "/")

	return Persisted{
		RelPath: rel,
		AbsPath: absPath,
		Size:    int64(len(data)),
	}, nil
}

// Remove deletes the attachment file and its meta sidecar. Intended for the
// rollback path when the downstream send fails after Persist succeeded.
// Missing files are not an error.
func Remove(absPath string) {
	if absPath == "" {
		return
	}
	_ = os.Remove(absPath)
	base := filepath.Base(absPath)
	if idx := strings.LastIndex(base, "."); idx > 0 {
		metaPath := filepath.Join(filepath.Dir(absPath), base[:idx]+".meta")
		_ = os.Remove(metaPath)
	}
}

// DefaultRefTTL is the second time-bound applied by GCWithRefs: files
// referenced by at least one session's event log survive this long past
// their last observed reference even if UploadedAt is older than uploadTTL.
// Operators tighten it via the attachment-gc daemon config.
const DefaultRefTTL = 30 * 24 * time.Hour

// ReapReason classifies why a payload was reaped (or would be, in dry-run)
// so operators can tell "safe to delete legacy files" apart from "tracker
// has not bumped this yet" before flipping dry_run off.
type ReapReason string

const (
	// ReasonLegacyNoMeta: no .meta sidecar; date-directory heuristic only.
	ReasonLegacyNoMeta ReapReason = "legacy_no_meta"
	// ReasonMetaNoRefs: .meta exists but no refs — genuinely unreferenced OR
	// not yet bumped by the tracker; the high-risk bucket.
	ReasonMetaNoRefs ReapReason = "meta_no_refs"
	// ReasonRefsExpired: referenced once but LastReferencedAt is past refTTL.
	ReasonRefsExpired ReapReason = "refs_expired"
)

// GCOptions controls a single GCWithRefs sweep over one workspace.
type GCOptions struct {
	// UploadTTL: files younger than this (UploadedAt, or date-dir for legacy) are kept.
	UploadTTL time.Duration
	// RefTTL: referenced files survive this long past LastReferencedAt.
	RefTTL time.Duration
	// Now is the reference clock (injected for deterministic tests).
	Now time.Time
	// MaxRemove caps payloads removed per sweep; 0 = unlimited. When hit the
	// sweep returns early and the daemon's cursor services other roots next tick.
	MaxRemove int
	// MetaGrace: skip payloads whose .meta was modified within this window
	// (a bump may have landed after GC read a pre-bump snapshot). 0 disables.
	MetaGrace time.Duration
	// DryRun: decide and bucket-count would-removes without touching disk.
	DryRun bool
}

// GCResult reports the outcome of one GCWithRefs sweep.
type GCResult struct {
	// Removed is the number of payloads deleted (0 in dry-run).
	Removed int
	// WouldRemove counts would-be deletions by reason (dry-run and live).
	WouldRemove map[ReapReason]int
	// Stopped is true when the sweep returned early because MaxRemove was hit.
	Stopped bool
}

func (r *GCResult) bump(reason ReapReason) {
	if r.WouldRemove == nil {
		r.WouldRemove = make(map[ReapReason]int, 3)
	}
	r.WouldRemove[reason]++
}

// GCWithRefs is the refcount-aware reaper. For every payload under
// <workspace>/.naozhi/attachments/<date>/ it reads the sibling .meta and
// keeps the file when
//
//	( now - UploadedAt < uploadTTL ) OR
//	( len(ReferencingKeyHashes) > 0 AND now - UnixMilli(LastReferencedAt) < refTTL )
//
// Files without a .meta use the date-directory NAME (not mtime) for the
// upload-TTL check and count as unreferenced. ctx is honoured per day-dir
// and per file; on cancellation the partial result is returned with ctx.Err().
func GCWithRefs(ctx context.Context, workspace string, opts GCOptions) (GCResult, error) {
	var res GCResult
	if workspace == "" {
		return res, ErrWorkspaceRequired
	}
	// A non-positive UploadTTL would make the keep-branch never fire and reap
	// every unreferenced upload, including ones seconds old. Treat it as
	// "GC disabled" rather than "delete everything". (#2226)
	if opts.UploadTTL <= 0 {
		return res, nil
	}
	root := filepath.Join(workspace, Dir)
	dayEntries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("read %s: %w", root, err)
	}

	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	now := opts.Now
	uploadCutoff := now.UTC().Add(-opts.UploadTTL)
	refCutoffMS := now.Add(-opts.RefTTL).UnixMilli()

	for _, de := range dayEntries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !de.IsDir() {
			continue
		}
		// Refuse to follow a symlinked date directory: os.Remove on a
		// symlinked dir / TOCTOU swap could reach outside the attachment root.
		dayPath := filepath.Join(root, de.Name())
		li, lerr := os.Lstat(dayPath)
		if lerr != nil || li.Mode()&os.ModeSymlink != 0 || !li.IsDir() {
			if lerr == nil {
				slog.Warn("attachment GC: refusing to traverse non-directory",
					"dir", dayPath, "mode", li.Mode().String())
			}
			continue
		}
		dayTime, parseErr := time.Parse("2006-01-02", de.Name())
		if parseErr != nil {
			// Unknown directory name — operator footprint; leave alone.
			continue
		}

		fileEntries, err := os.ReadDir(dayPath)
		if err != nil {
			slog.Warn("attachment GC: read day dir failed",
				"dir", dayPath, "err", err)
			continue
		}
		kept := 0
		for _, fe := range fileEntries {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if fe.IsDir() {
				continue
			}
			name := fe.Name()
			// .meta sidecars follow the payload's decision, not their own.
			if strings.HasSuffix(name, ".meta") {
				continue
			}
			abs := filepath.Join(dayPath, name)
			metaPath := MetaPathFor(abs)

			keep, reason, err := shouldKeepAttachment(metaPath, dayTime, uploadCutoff, refCutoffMS)
			if err != nil {
				slog.Warn("attachment GC: keep-decision failed",
					"path", abs, "err", err)
				// Err on the side of retaining data; the next sweep revisits.
				kept++
				continue
			}
			if keep {
				kept++
				continue
			}

			// A bump may have landed after we read a pre-bump snapshot;
			// retain recently touched .meta this round.
			if opts.MetaGrace > 0 {
				if mi, merr := os.Stat(metaPath); merr == nil &&
					now.Sub(mi.ModTime()) < opts.MetaGrace {
					kept++
					continue
				}
			}

			res.bump(reason)

			if opts.DryRun {
				slog.Info("attachment GC: would remove",
					"path", abs, "reason", string(reason),
					"day", de.Name())
				// dry-run does not count toward the MaxRemove budget.
				continue
			}

			// Remove .meta FIRST, then payload: if the tracker races an
			// UpdateMetaFile, loadMetaFile returns (nil,nil) and the m==nil
			// branch refuses to recreate the sidecar, so no orphan meta. A
			// leftover payload is reaped next sweep via the legacy path.
			if err := os.Remove(metaPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("attachment GC: remove meta failed",
					"path", metaPath, "err", err)
			}
			if err := os.Remove(abs); err != nil {
				slog.Warn("attachment GC: remove payload failed",
					"path", abs, "err", err)
				continue
			}
			slog.Info("attachment GC: removed",
				"path", abs, "reason", string(reason))
			res.Removed++

			if opts.MaxRemove > 0 && res.Removed >= opts.MaxRemove {
				res.Stopped = true
				return res, nil
			}
		}

		// Prune empty day dirs only when older than uploadTTL. INVARIANT:
		// this never touches today's Persist target dir — do not relax.
		if kept == 0 && !opts.DryRun && dayTime.Add(24*time.Hour).Before(uploadCutoff) {
			if err := os.Remove(dayPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Debug("attachment GC: empty day dir remove failed",
					"dir", dayPath, "err", err)
			}
		}
	}
	return res, nil
}

// shouldKeepAttachment applies the double-TTL rule (see GCWithRefs).
// Returns (keep, reapReason, err); reapReason is meaningful only when
// keep==false. Missing .meta falls back to the date-directory time for
// upload-age and assumes no references.
func shouldKeepAttachment(metaPath string, dayTime time.Time, uploadCutoff time.Time, refCutoffMS int64) (bool, ReapReason, error) {
	meta, err := loadMetaFile(metaPath)
	if err != nil {
		return false, "", err
	}

	uploadOld := false
	switch {
	case meta == nil:
		// Legacy: dayTime + 24h is the liberal "uploaded on this day" proxy.
		uploadOld = dayTime.Add(24 * time.Hour).Before(uploadCutoff)
	default:
		uploadTime := meta.UploadedAt
		if uploadTime.IsZero() {
			// Defensive: Persist always populates UploadedAt.
			uploadTime = dayTime.Add(24 * time.Hour)
		}
		uploadOld = uploadTime.Before(uploadCutoff)
	}

	hasRefs := meta != nil && len(meta.ReferencingKeyHashes) > 0
	refRecent := meta != nil && meta.LastReferencedAt > 0 &&
		meta.LastReferencedAt > refCutoffMS

	if !uploadOld {
		return true, "", nil
	}
	if hasRefs && refRecent {
		return true, "", nil
	}

	switch {
	case meta == nil:
		return false, ReasonLegacyNoMeta, nil
	case hasRefs:
		return false, ReasonRefsExpired, nil
	default:
		return false, ReasonMetaNoRefs, nil
	}
}

// loadMetaFile reads + parses a single .meta sidecar. Missing files return
// (nil, nil) — legacy attachments. Corrupt JSON returns an error so the
// caller retains the file.
func loadMetaFile(path string) (*Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read meta %s: %w", path, err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse meta %s: %w", path, err)
	}
	return &m, nil
}

// MetaPathFor returns the sibling .meta path for an attachment payload file,
// matching the layout Persist creates. Exported so the tracker shares one
// implementation and the .meta namespace cannot split between GC and tracker.
func MetaPathFor(absPayload string) string {
	base := filepath.Base(absPayload)
	if idx := strings.LastIndex(base, "."); idx > 0 {
		return filepath.Join(filepath.Dir(absPayload), base[:idx]+".meta")
	}
	// No extension: Persist never produces this, but stay well-formed.
	return absPayload + ".meta"
}

// UpdateMetaFile reads metaPath, applies mutate, and writes it back
// atomically; changed=false skips the write. Caller owns serialization (in
// production only the tracker's single writer goroutine calls this).
func UpdateMetaFile(metaPath string, mutate func(*Meta) bool) (bool, error) {
	m, err := loadMetaFile(metaPath)
	if err != nil {
		return false, err
	}
	if m == nil {
		// Legacy attachment with no meta: refuse rather than invent upload
		// metadata. Path kept out of the error (workspace paths are
		// operator-only); the caller logs metaPath itself.
		return false, errors.New("meta sidecar missing")
	}
	if !mutate(m) {
		return false, nil
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return false, fmt.Errorf("marshal meta %s: %w", metaPath, err)
	}
	if err := osutil.WriteFileAtomic(metaPath, buf, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// newID returns a 128-bit random hex string. crypto/rand only: a predictable
// id in a shared workspace could be probed by a co-tenant.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// sanitizeExt rejects anything outside a tiny allowlist so a new format
// needs a compile/review touchpoint before it can slip through.
func sanitizeExt(ext string) (string, error) {
	switch strings.ToLower(ext) {
	case ".pdf":
		return ".pdf", nil
	case ".jpg":
		return ".jpg", nil
	case ".jpeg":
		return ".jpg", nil
	case ".png":
		return ".png", nil
	case ".gif":
		return ".gif", nil
	case ".webp":
		return ".webp", nil
	default:
		return "", fmt.Errorf("attachment: unsupported extension %q", ext)
	}
}
