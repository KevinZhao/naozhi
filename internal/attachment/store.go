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
	if err := os.MkdirAll(absDir, 0o755); err != nil {
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
	if err := osutil.WriteFileAtomic(absPath, data, 0o644); err != nil {
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
	if err := osutil.WriteFileAtomic(metaPath, metaBytes, 0o644); err != nil {
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

// GC removes attachment date-directories older than ttl under workspace.
// Returns the number of directories removed. Errors on individual removes
// are logged but do not abort the sweep — a single permission-denied entry
// should not leave the rest of the backlog untouched.
//
// GC does NOT walk into the date directories. Since naozhi owns the entire
// Dir subtree, dropping the directory covers both .pdf payloads and .meta
// sidecars in one syscall. Any unexpected files an operator dropped in
// by hand will also be removed — that is the documented tradeoff of
// having naozhi own a subtree.
func GC(workspace string, ttl time.Duration, now time.Time) (int, error) {
	if workspace == "" {
		return 0, ErrWorkspaceRequired
	}
	root := filepath.Join(workspace, Dir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", root, err)
	}
	cutoff := now.UTC().Add(-ttl)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := time.Parse("2006-01-02", e.Name())
		if err != nil {
			// Non-date-formatted directory (operator footprint, partial
			// earlier naozhi version, etc.) — leave it alone.
			continue
		}
		// t is midnight of that day UTC; add 24h so we only delete a day
		// once it has fully elapsed past the cutoff. Prevents a 7-day TTL
		// from silently being 6 days at 00:00 UTC.
		if t.Add(24 * time.Hour).Before(cutoff) {
			p := filepath.Join(root, e.Name())
			if err := os.RemoveAll(p); err != nil {
				slog.Warn("attachment GC: remove failed", "dir", p, "err", err)
				continue
			}
			removed++
		}
	}
	return removed, nil
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

// sanitizeExt rejects anything outside a tiny allowlist. Today only ".pdf"
// is allowed; adding ".docx" would be a one-line change plus a matching
// entry in the upload handler. Keeping this narrow forces a compile/review
// touchpoint before a new format can slip through.
func sanitizeExt(ext string) (string, error) {
	switch strings.ToLower(ext) {
	case ".pdf":
		return ".pdf", nil
	default:
		return "", fmt.Errorf("attachment: unsupported extension %q", ext)
	}
}
