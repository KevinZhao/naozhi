// Package attachment persists user-uploaded files into the session workspace
// so Claude can reach them via its native Read tool. Images still travel
// inline as content blocks; this package exists for formats (PDF, future
// docx/xlsx) whose base64 size exceeds the 12 MB stdin line cap documented
// on cli.maxStdinLineBytes.
//
// On-disk layout (rooted at the session workspace):
//
//	<workspace>/.naozhi/attachments/<yyyy-mm-dd>/<uuid>.<ext>
//	<workspace>/.naozhi/attachments/<yyyy-mm-dd>/<uuid>.meta
//
// The date directory lets the GC drop an entire day in one call instead of
// statting every file. UUID filenames prevent collisions and keep the
// original (possibly sensitive) filename out of paths the model sees; the
// original name is preserved in the .meta sidecar for UI display.
//
// This package never reads PDF bytes — it only writes them. Parsing is the
// Anthropic API's job, which the CLI reaches through its Read tool.
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
// Kept as a package-level var (not const) so tests running against a bare
// tmpdir can shrink it if they need to simulate "pre-existing workspace
// content at Dir". External code should treat it as read-only.
var Dir = filepath.Join(".naozhi", "attachments")

// Errors surfaced to HTTP callers. Keep messages generic — the workspace
// path is operator-only information and must not be echoed to clients.
var (
	ErrWorkspaceRequired = errors.New("attachment: workspace is required")
	ErrEmptyData         = errors.New("attachment: data is empty")
)

// Meta is the sidecar stored alongside each attachment. Fields are stable
// JSON so the GC / future UI can read them without pulling in a newer
// naozhi version. Unknown fields on read are ignored.
type Meta struct {
	OrigName   string    `json:"orig_name"`
	MimeType   string    `json:"mime_type"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
	// SessionKey is recorded for audit / debugging only. The GC does not
	// key on it; attachments are tied to a workspace, not a session, because
	// multiple sessions can share a workspace and we don't want a /new to
	// orphan files a follow-up conversation might still reference.
	SessionKey string `json:"session_key,omitempty"`
	// Owner is the dashboard-auth-derived identifier from uploadOwner().
	// Used only for internal logs — do not surface to other users.
	Owner string `json:"owner,omitempty"`

	// ReferencingKeyHashes is the sorted set of session key-hashes
	// (persist.KeyHash(key)) whose on-disk event log has persisted an
	// entry carrying this attachment's relative path. Maintained by
	// the attachment tracker running inside session.Router.
	//
	// Semantics for GC (RFC: attachment-refcount §3.3):
	//
	//   - A nil / missing slice means this attachment predates the
	//     refcount RFC. The legacy upload-time TTL alone drives
	//     cleanup for such files.
	//
	//   - A non-empty slice means at least one session's event log
	//     references this attachment. The file is retained as long
	//     as the second time-bound (refTTL, anchored on
	//     LastReferencedAt) has NOT elapsed; an empty slice after
	//     the tracker has removed every session is treated exactly
	//     like "no references" and falls back to the legacy upload
	//     TTL decision.
	//
	// Stored as a sorted []string so an operator inspecting the
	// .meta file with `cat` or `jq` can spot which sessions touched
	// the attachment without needing an external index.
	ReferencingKeyHashes []string `json:"referencing_keyhashes,omitempty"`

	// LastReferencedAt records the most recent unix-ms wall-clock
	// moment at which any session's event log was observed referencing
	// this attachment. The GC keeps the attachment as long as
	//
	//	now.UnixMilli() - LastReferencedAt < refTTL
	//
	// even if UploadedAt is well past the primary TTL. A zero / missing
	// value signals the tracker has never observed the attachment
	// (legacy or the Meta was written before the refcount RFC landed).
	LastReferencedAt int64 `json:"last_referenced_at,omitempty"`
}

// AddReference inserts keyhash into ReferencingKeyHashes keeping the
// slice sorted + deduplicated. Idempotent: the same keyhash can be
// bumped repeatedly without growing the list. Returns true if the
// slice actually changed, which the tracker uses to skip unnecessary
// .meta rewrites.
func (m *Meta) AddReference(keyhash string) bool {
	if keyhash == "" {
		return false
	}
	// Binary search to keep the common "already present" path
	// alloc-free.
	lo, hi := 0, len(m.ReferencingKeyHashes)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case m.ReferencingKeyHashes[mid] < keyhash:
			lo = mid + 1
		case m.ReferencingKeyHashes[mid] > keyhash:
			hi = mid
		default:
			return false // already present
		}
	}
	m.ReferencingKeyHashes = append(m.ReferencingKeyHashes, "")
	copy(m.ReferencingKeyHashes[lo+1:], m.ReferencingKeyHashes[lo:])
	m.ReferencingKeyHashes[lo] = keyhash
	return true
}

// RemoveReference drops keyhash from ReferencingKeyHashes (if
// present). Returns true when the slice shrank. Paired with
// AddReference for session-deletion propagation — the tracker walks
// every referenced attachment and calls this when a session is
// removed.
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

// HasReference is a tiny helper for readers that need to test for
// membership without mutating.
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
	// RelPath is the workspace-relative path with forward slashes, suitable
	// for pasting into the CLI Read tool and showing to the user. Example:
	//   ".naozhi/attachments/2026-05-06/a1b2c3d4....pdf"
	RelPath string
	// AbsPath is the filesystem path — used by the HTTP handler to clean up
	// on downstream failure (e.g. the send itself fails after the file
	// landed on disk). Not intended for the model.
	AbsPath string
	// Size is the byte count written.
	Size int64
}

// Persist writes data to a fresh UUID-named file under the workspace
// attachment directory, together with its .meta sidecar. The returned
// RelPath uses forward slashes regardless of host OS so the same string
// works in Claude's Read tool input and in the dashboard UI.
//
// workspace MUST be an absolute path that already exists. Callers typically
// pass the session's resolved working directory; Persist does not create
// the workspace itself because doing so would mask configuration errors
// (workspace misconfigured → attachments silently written to wrong place).
//
// ext should include the leading dot ("." + "pdf" → ".pdf"). It is clamped
// to a tiny allowlist to prevent any ".."/"/"/null byte from slipping past
// a future caller that reads ext from user input. The current production
// caller hardcodes ".pdf".
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

	// Date subdir in UTC: GC logic and operator spot-checks both benefit
	// from a single timezone. Local time would risk a day-boundary race
	// on DST edges.
	dateDir := time.Now().UTC().Format("2006-01-02")
	absDir := filepath.Join(workspace, Dir, dateDir)
	// Restrict to owner-only (0o700). Multi-tenant hosts would otherwise
	// let co-resident users walk the attachments subtree and read uploaded
	// content directly off disk. Single-user deployments see no behaviour
	// change; shared deployments gain a meaningful barrier.
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

	// Write the payload atomically first. If meta fails after the payload
	// landed, we rollback the payload — ensures "no half-committed
	// attachment" from the caller's point of view.
	//
	// 0o600 keeps payload readable only by the naozhi user. Pairs with the
	// 0o700 date-dir mode above; without it a group-readable directory
	// cap is defeated by world-readable files inside.
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
	// Meta carries Owner / SessionKey — restrict to owner-only for the
	// same reasons as the payload itself.
	if err := osutil.WriteFileAtomic(metaPath, metaBytes, 0o600); err != nil {
		_ = os.Remove(absPath)
		return Persisted{}, err
	}

	// Forward-slash relative path regardless of host OS. Windows callers
	// would otherwise feed backslashes into the Read tool, which the CLI
	// would either reject or silently misresolve.
	rel := path.Join(Dir, dateDir, baseName)
	// On Windows, filepath.Join-built Dir uses "\" — normalize.
	rel = strings.ReplaceAll(rel, `\`, "/")

	return Persisted{
		RelPath: rel,
		AbsPath: absPath,
		Size:    int64(len(data)),
	}, nil
}

// Remove deletes the attachment file and its meta sidecar. Intended for the
// rollback path when the downstream send fails after Persist succeeded.
// Missing files are not an error — Remove can be called unconditionally
// after any failure without an exists check.
func Remove(absPath string) {
	if absPath == "" {
		return
	}
	_ = os.Remove(absPath)
	// The meta file lives next to the payload with the same basename minus
	// the payload extension: "<uuid>.pdf" → "<uuid>.meta".
	base := filepath.Base(absPath)
	if idx := strings.LastIndex(base, "."); idx > 0 {
		metaPath := filepath.Join(filepath.Dir(absPath), base[:idx]+".meta")
		_ = os.Remove(metaPath)
	}
}

// DefaultRefTTL is the second time-bound applied by GCWithRefs:
// files referenced by at least one session's event log survive this
// long past their last observed reference even if UploadedAt is
// older than uploadTTL. The 30-day default is conservative enough
// that a user returning to a session after a long gap still sees
// their images; operators can tighten it via the attachment-gc
// daemon config (see docs/rfc/attachment-gc-daemon.md).
const DefaultRefTTL = 30 * 24 * time.Hour

// ReapReason classifies why a payload was reaped (or would be, in
// dry-run). The attachment-gc daemon buckets dry-run counts by reason
// so operators can tell "safe to delete legacy files" apart from
// "tracker has not bumped this yet" before flipping dry_run off.
// See docs/rfc/attachment-gc-daemon.md §6 (E4).
type ReapReason string

const (
	// ReasonLegacyNoMeta: no .meta sidecar; decided purely by the
	// date-directory upload-TTL heuristic. Generally safe to delete.
	ReasonLegacyNoMeta ReapReason = "legacy_no_meta"
	// ReasonMetaNoRefs: .meta exists but no ReferencingKeyHashes.
	// Could be genuinely unreferenced OR the tracker simply has not
	// bumped it yet — the high-risk bucket operators must weigh.
	ReasonMetaNoRefs ReapReason = "meta_no_refs"
	// ReasonRefsExpired: referenced once but LastReferencedAt is past
	// refTTL. Long-unreferenced; safe to delete.
	ReasonRefsExpired ReapReason = "refs_expired"
)

// GCOptions controls a single GCWithRefs sweep over one workspace.
type GCOptions struct {
	// UploadTTL: files younger than this (by UploadedAt, or date-dir
	// for legacy) are always kept.
	UploadTTL time.Duration
	// RefTTL: referenced files survive this long past LastReferencedAt
	// even when older than UploadTTL.
	RefTTL time.Duration
	// Now is the reference clock (injected for deterministic tests).
	Now time.Time
	// MaxRemove caps payloads removed in this sweep (per-root budget,
	// RFC §4.6-2). 0 means unlimited. When the cap is hit the sweep
	// returns early; the daemon's round-robin cursor ensures other
	// roots get serviced next tick.
	MaxRemove int
	// MetaGrace: skip any payload whose .meta sidecar was modified
	// within this window (RFC §10 F2). Closes the race where a bump
	// just landed but GC read a pre-bump snapshot. 0 disables.
	MetaGrace time.Duration
	// DryRun: decide keep/delete and bucket-count would-removes, but
	// do NOT touch disk. RFC §6.
	DryRun bool
}

// GCResult reports the outcome of one GCWithRefs sweep.
type GCResult struct {
	// Removed is the number of payloads deleted (0 in dry-run).
	Removed int
	// WouldRemove counts payloads that WOULD be deleted, bucketed by
	// reason. Populated in both dry-run and live mode (in live mode it
	// mirrors the reap reasons for observability).
	WouldRemove map[ReapReason]int
	// Stopped is true when the sweep returned early because MaxRemove
	// was hit (more work remains for the next tick).
	Stopped bool
}

func (r *GCResult) bump(reason ReapReason) {
	if r.WouldRemove == nil {
		r.WouldRemove = make(map[ReapReason]int, 3)
	}
	r.WouldRemove[reason]++
}

// GCWithRefs is the refcount-aware reaper (see RFC §3.3 +
// docs/rfc/attachment-gc-daemon.md). For every image / PDF file under
// <workspace>/.naozhi/attachments/<date>/ it reads the sibling .meta
// sidecar and keeps the file when:
//
//	( now - UploadedAt        <  uploadTTL )
//	OR
//	( len(ReferencingKeyHashes) > 0 AND
//	  now - UnixMilli(LastReferencedAt) < refTTL )
//
// Files without a .meta sidecar fall back to the date-directory
// NAME (parsed via time.Parse, NOT filesystem mtime) for the
// upload-TTL check and are treated as unreferenced — the upload-TTL
// branch alone decides them. That mirrors the removed legacy GC
// behaviour for migration.
//
// Empty date directories are pruned after the per-file sweep so the
// top-level directory listing stays tidy.
//
// ctx is honoured at both the day-directory and per-file granularity
// so a large backlog cannot wedge the caller (the attachment-gc
// daemon's Tick context / shutdown). On cancellation it returns the
// partial result with ctx.Err().
//
// Callers: the attachment-gc daemon (internal/sysession). The tracker
// (internal/attachment/tracker) runs separately — it only WRITES
// refcount data, it never deletes.
func GCWithRefs(ctx context.Context, workspace string, opts GCOptions) (GCResult, error) {
	var res GCResult
	if workspace == "" {
		return res, ErrWorkspaceRequired
	}
	root := filepath.Join(workspace, Dir)
	dayEntries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("read %s: %w", root, err)
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
		// symlinked dir / TOCTOU swap could otherwise reach outside the
		// attachment root.
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
		// Per-file keep/delete pass.
		kept := 0
		for _, fe := range fileEntries {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if fe.IsDir() {
				continue
			}
			name := fe.Name()
			// Skip .meta sidecars — they follow the payload file's
			// decision, not their own.
			if strings.HasSuffix(name, ".meta") {
				continue
			}
			abs := filepath.Join(dayPath, name)
			metaPath := metaPathFor(abs)

			keep, reason, err := shouldKeepAttachment(metaPath, dayTime, uploadCutoff, refCutoffMS)
			if err != nil {
				slog.Warn("attachment GC: keep-decision failed",
					"path", abs, "err", err)
				// Err on the side of retaining data; the next sweep
				// revisits it.
				kept++
				continue
			}
			if keep {
				kept++
				continue
			}

			// F2: skip payloads whose .meta was just touched by the
			// tracker — a bump may have landed after we'd otherwise
			// have read a pre-bump snapshot. Retain this round.
			if opts.MetaGrace > 0 {
				if mi, merr := os.Stat(metaPath); merr == nil &&
					now.Sub(mi.ModTime()) < opts.MetaGrace {
					kept++
					continue
				}
			}

			// Count the would-remove for observability / dry-run.
			res.bump(reason)

			if opts.DryRun {
				slog.Info("attachment GC: would remove",
					"path", abs, "reason", string(reason),
					"day", de.Name())
				// dry-run does not delete and does not count toward the
				// real MaxRemove budget — it reports the full picture.
				continue
			}

			// Live delete. F1: remove .meta FIRST, then payload. If the
			// tracker races an UpdateMetaFile in the window, loadMetaFile
			// returns (nil,nil) → UpdateMetaFile's m==nil branch refuses
			// to recreate the sidecar, so we cannot leave an orphan meta.
			// A leftover payload (if we die between the two removes) is
			// reaped next sweep via the legacy-fallback path.
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

		// Prune empty day directories opportunistically. Only when the
		// day is older than uploadTTL — a freshly uploaded day that
		// happens to end up empty (unusual) stays on disk for incoming
		// uploads. INVARIANT (RFC §10 F3): this only ever touches dirs
		// >= uploadTTL old, which by construction never equals today's
		// Persist target dir — do not relax this condition.
		if kept == 0 && !opts.DryRun && dayTime.Add(24*time.Hour).Before(uploadCutoff) {
			if err := os.Remove(dayPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Debug("attachment GC: empty day dir remove failed",
					"dir", dayPath, "err", err)
			}
		}
	}
	return res, nil
}

// shouldKeepAttachment applies the double-TTL rule. See GCWithRefs
// godoc for the precise formula. Returns (keep, reapReason, err);
// reapReason is meaningful only when keep==false.
//
// Missing .meta: the attachment predates the refcount RFC. We fall
// back to the date-directory parse time for upload-age (the actual
// upload time is unrecoverable without the sidecar) and assume no
// references.
func shouldKeepAttachment(metaPath string, dayTime time.Time, uploadCutoff time.Time, refCutoffMS int64) (bool, ReapReason, error) {
	meta, err := loadMetaFile(metaPath)
	if err != nil {
		return false, "", err
	}

	// Upload-age decision.
	uploadOld := false
	switch {
	case meta == nil:
		// Legacy attachment — use dayTime + 24h as the liberal
		// "uploaded on this day or later" proxy.
		uploadOld = dayTime.Add(24 * time.Hour).Before(uploadCutoff)
	default:
		uploadTime := meta.UploadedAt
		if uploadTime.IsZero() {
			// Meta exists but UploadedAt was never populated (should
			// not happen post-Persist; defensive). Fall back to the
			// day-parse conservative rule.
			uploadTime = dayTime.Add(24 * time.Hour)
		}
		uploadOld = uploadTime.Before(uploadCutoff)
	}

	// Refcount decision.
	hasRefs := meta != nil && len(meta.ReferencingKeyHashes) > 0
	refRecent := meta != nil && meta.LastReferencedAt > 0 &&
		meta.LastReferencedAt > refCutoffMS

	// Keep when either bound still holds.
	if !uploadOld {
		return true, "", nil
	}
	if hasRefs && refRecent {
		return true, "", nil
	}

	// Reaping — classify why for the dry-run buckets (RFC §6 E4).
	switch {
	case meta == nil:
		return false, ReasonLegacyNoMeta, nil
	case hasRefs: // refs exist but expired (refRecent was false)
		return false, ReasonRefsExpired, nil
	default:
		return false, ReasonMetaNoRefs, nil
	}
}

// loadMetaFile reads + parses a single .meta sidecar. Missing files
// return (nil, nil) — the caller treats them as legacy attachments.
// Corrupt JSON returns an error so the caller retains the file
// (err-on-the-side-of-keep semantics).
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

// metaPathFor returns the sibling .meta path for an attachment
// payload file. Matches the layout Persist creates: strips the
// payload extension and appends ".meta".
func metaPathFor(absPayload string) string {
	base := filepath.Base(absPayload)
	if idx := strings.LastIndex(base, "."); idx > 0 {
		return filepath.Join(filepath.Dir(absPayload), base[:idx]+".meta")
	}
	// Edge case: no extension. Append directly — Persist never
	// produces this, but the helper should still return a
	// well-formed path rather than panic.
	return absPayload + ".meta"
}

// UpdateMetaFile reads <path>.meta, applies mutate, and writes it
// back atomically. Used by the tracker to bump LastReferencedAt and
// the ReferencingKeyHashes set without duplicating the
// read-modify-write boilerplate.
//
// Concurrency: caller owns serialization. In production the tracker's
// single writer goroutine is the only UpdateMetaFile caller, so no
// locking is needed here. Tests that exercise concurrent updates
// must serialise externally.
//
// Returns (changed, err) — `changed=false` when mutate reported no
// change, in which case we skip the write entirely (cheap idempotence
// guard for the common "already present" case).
func UpdateMetaFile(metaPath string, mutate func(*Meta) bool) (bool, error) {
	m, err := loadMetaFile(metaPath)
	if err != nil {
		return false, err
	}
	if m == nil {
		// Legacy attachment with no meta; we cannot append references
		// without inventing upload metadata, so we refuse rather than
		// write a partial sidecar.
		return false, fmt.Errorf("meta sidecar missing: %s", metaPath)
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

// newID returns a 128-bit random hex string. crypto/rand is the only
// acceptable source: a predictable id in the workspace could be probed by
// a co-tenant (dashboard deployments are typically single-user but ops
// teams share workspaces occasionally).
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// sanitizeExt rejects anything outside a tiny allowlist. ".pdf" was the
// original entry (see docs/rfc/pdf-attachment.md); images joined when the
// dashboard lightbox grew a "view original" affordance and needed a
// durable URL instead of the 600 px thumbnail data URI. Keeping this
// narrow forces a compile/review touchpoint before a new format can
// slip through.
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
