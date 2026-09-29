package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/session"
)

// attachmentDirPrefix is the workspace-relative prefix every path served
// via /api/sessions/attachment must start with (attachment.Dir in wire form).
// Kept separate from attachment.Dir so the HTTP guard cannot loosen silently.
//
// Wire contract (#468): forward slash on every platform, trailing `/`
// REQUIRED. Never substitute filepath.Separator or make it
// platform-conditional — every EventEntry.ImagePaths value uses `/`, and a
// mismatch with the HasPrefix gate either rejects legitimate paths or admits
// non-attachment ones. Downstream Joins convert via filepath.FromSlash.
const attachmentDirPrefix = ".naozhi/attachments/"

// maxAttachmentBytes caps the per-response size; oversize files are refused
// and the client falls back to the thumbnail. Stays below serveRaw's project
// file cap.
const maxAttachmentBytes = 16 << 20

// cleanAttachmentRelPath validates the workspace-relative attachment path
// the dashboard sends in ?path=. Returns (cleaned, "") on accept and
// ("", errMsg) on reject; errMsg is the client-facing JSON error string.
// Separate from handleAttachment so the path.Clean / filepath.Clean
// divergence guard is unit-testable (#536).
func cleanAttachmentRelPath(relRaw string) (string, string) {
	if len(relRaw) > 1024 {
		return "", "path too long"
	}
	if strings.ContainsRune(relRaw, 0) {
		return "", "invalid path"
	}
	if strings.ContainsRune(relRaw, '\\') || filepath.IsAbs(relRaw) {
		return "", "invalid path"
	}
	// path.Clean (POSIX) matches the forward-slash wire shape (backslash +
	// IsAbs already rejected); EvalSymlinks + attachRootAbs HasPrefix in the
	// handler is the authoritative containment check.
	cleaned := path.Clean(relRaw)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", "invalid path"
	}
	// Defence-in-depth on non-Linux: path.Clean and filepath.Clean must
	// agree on the forward-slash form; any divergence means the wire shape
	// is not stable across the path/filepath boundary (#536).
	if osCleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(cleaned))); osCleaned != cleaned {
		return "", "invalid path"
	}
	return cleaned, ""
}

// handleAttachment streams an on-disk inline image from the session
// workspace attachment directory for the dashboard lightbox "view original"
// (the data-URI thumbnail in EventEntry.Images remains the fallback).
//
// Request: GET /api/sessions/attachment?key=<session>&path=<ws-rel>
// Response: image/jpeg | image/png | image/gif | image/webp
//
// Authorization is the "session exists" boundary shared with
// /api/sessions/events. The path is pinned to attachmentDirPrefix under the
// session's workspace so a crafted key cannot exfiltrate other files.
func (h *SendHandler) handleAttachment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	relRaw := q.Get("path")
	if key == "" || relRaw == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "key and path are required"})
		return
	}
	if err := session.ValidateSessionKey(key); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
		return
	}

	// Strict path shape: workspace-relative, forward slashes only, no
	// absolute paths, no traversal, no NUL (mirrors resolveProjectFile).
	cleaned, errMsg := cleanAttachmentRelPath(relRaw)
	if errMsg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}
	// Pin to the attachment subtree: persistFileRefs is the only producer
	// of these paths, so a legitimate URL always starts with the prefix.
	if !strings.HasPrefix(cleaned, attachmentDirPrefix) {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Resolve the workspace from the key only (live session, else router
	// fallback — the same rule resolveAttachmentWorkspace applies on the
	// write side). A `workspace` query parameter is deliberately NOT
	// accepted.
	ws := h.engine.sessionWorkspace(key)
	if ws == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	validatedWS, err := h.engine.validateWorkspace(ws)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	abs := filepath.Join(validatedWS, filepath.FromSlash(cleaned))
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		slog.Debug("attachment: eval symlinks failed", "err", err)
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	// Symlink-escape defence: resolved MUST still live under validatedWS/attachmentDir.
	attachRootAbs := filepath.Join(validatedWS, filepath.FromSlash(strings.TrimSuffix(attachmentDirPrefix, "/")))
	if resolved != attachRootAbs &&
		!strings.HasPrefix(resolved, attachRootAbs+string(filepath.Separator)) {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Lstat (not Stat): a file swapped for a symlink after EvalSymlinks must
	// be rejected, not followed.
	info, err := os.Lstat(resolved)
	if err != nil || info.IsDir() {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if info.Size() > maxAttachmentBytes {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large"})
		return
	}

	// O_NOFOLLOW open (dashproject.OpenWorkspaceFile) closes the Lstat→Open
	// symlink-swap TOCTOU: a final-component symlink fails atomically with
	// ELOOP (#917).
	f, err := dashproject.OpenWorkspaceFile(resolved)
	if err != nil {
		// Symlink trap → same 404 as missing: escape attempts and absent
		// files must look identical.
		if errors.Is(err, syscall.ELOOP) {
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "open failed"})
		return
	}
	defer f.Close()

	// MIME is pinned from the extension: attachment.sanitizeExt is the only
	// producer of these files.
	ext := strings.ToLower(filepath.Ext(resolved))
	var mime string
	switch ext {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	default:
		// Unknown extension inside our own subtree — refuse rather than guess.
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Defence-in-depth magic-byte check: if the sniffed MIME disagrees with
	// the extension pin, degrade to application/octet-stream +
	// Content-Disposition: attachment so a non-image that lands in our
	// subtree (e.g. a crafted SVG) can never render inline.
	disposition := "inline"
	disableInlineRender := false
	var sniffBuf [512]byte
	if n, _ := io.ReadFull(f, sniffBuf[:]); n > 0 {
		sniffed := http.DetectContentType(sniffBuf[:n])
		// SVG / XHTML / XML MUST never go inline even when sniff matches the
		// ext MIME (a future ext-allowlist relaxation must not enable
		// `<svg onload>`). Prefix match: DetectContentType may append
		// `; charset=...`.
		isXMLLike := strings.HasPrefix(sniffed, "image/svg+xml") ||
			strings.HasPrefix(sniffed, "application/xhtml+xml") ||
			strings.HasPrefix(sniffed, "text/xml") ||
			strings.HasPrefix(sniffed, "application/xml")
		if isXMLLike {
			slog.Warn("attachment: SVG/XML-like content detected, forcing attachment download",
				"ext", ext, "ext_mime", mime, "sniffed", sniffed,
				"path", filepath.Base(resolved))
			mime = "application/octet-stream"
			disposition = "attachment"
			disableInlineRender = true
		} else if !strings.EqualFold(sniffed, mime) {
			slog.Warn("attachment: magic-byte mismatch, degrading to octet-stream",
				"ext", ext, "ext_mime", mime, "sniffed", sniffed,
				"path", filepath.Base(resolved))
			mime = "application/octet-stream"
			disposition = "attachment"
			disableInlineRender = true
		}
	}
	// Rewind so http.ServeContent below streams the full payload, not just
	// the bytes after the sniff cursor.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}

	// ETag = truncated sha256(size|mtime|salt): a plain "<size>-<mtime>" form
	// would leak exact size and mtime to any authenticated GET. Built via
	// strconv.AppendInt into a stack buffer — this runs on every attachment
	// GET.
	var etagBuf [48]byte
	etagSeed := buildAttachmentETagSeed(etagBuf[:0], info.Size(), info.ModTime())
	etagSum := sha256.Sum256(etagSeed)
	// 12 bytes (96-bit): a 64-bit truncation would put birthday-bound ETag
	// forgery within ~2^32 observed ETags.
	etag := `"` + hex.EncodeToString(etagSum[:12]) + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if disableInlineRender {
		// Belt-and-braces: when degraded, blank out frame embedding so the
		// browser cannot side-load the byte stream into a context that
		// ignores Content-Disposition (e.g. <img>, <iframe>).
		w.Header().Set("X-Frame-Options", "DENY")
	}
	// Tight CSP: attachments are image-only, no inline scripts, no third-
	// party resources. sandbox closes top-level-navigation XSS channels for
	// formats (e.g. a future .svg) that slip past the ext check.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; img-src 'self' data:")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

// buildAttachmentETagSeed builds the SHA-256 input for the attachment ETag:
// "<size>|<mtime-millis>|<process-salt>" appended into dst (caller passes a
// stack buffer so the hot path stays allocation-free). Millisecond mtime, not
// nanos, denies an attacker ~30 bits of entropy to brute-force via
// If-None-Match probes; dashproject.FileETagSalt (per-process secret) defeats
// offline pre-imaging of (size, mtime). ETags rotate once per deploy.
func buildAttachmentETagSeed(dst []byte, size int64, mtime time.Time) []byte {
	dst = strconv.AppendInt(dst, size, 10)
	dst = append(dst, '|')
	dst = strconv.AppendInt(dst, mtime.UnixMilli(), 10)
	dst = append(dst, '|')
	dst = append(dst, dashproject.FileETagSalt...)
	return dst
}
