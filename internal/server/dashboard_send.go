package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// Upload size ceilings. Images stay at the long-standing 10 MB; PDFs get
// their own cap derived from Anthropic's 32 MB document-block limit — we
// honour the upstream ceiling so a file accepted here won't later be
// rejected by the API. Both match the byte counts announced to the user
// so frontend and backend error messages agree.
const (
	maxImageBytes = 10 << 20 // 10 MB
	maxPDFBytes   = 32 << 20 // 32 MB (Anthropic API limit)

	// uploadBodyBytes bounds the multipart envelope for /api/sessions/upload.
	// Max payload is maxPDFBytes + ~2 MB for multipart overhead (boundary,
	// Content-Disposition headers, form-field metadata). Lifted from 11 MB
	// when PDFs joined the upload path.
	uploadBodyBytes = maxPDFBytes + (2 << 20)

	// RNEW-SEC-001: cap the number of non-file form fields we accept in
	// any multipart request. Go's http package has a soft default of 1000
	// Value entries; for naozhi no legitimate request needs more than a
	// handful (key, text, node, workspace, resume_id, backend, file_ids
	// repeated up to maxFilesPerSend). A padded-body attacker could
	// otherwise inflate the in-memory Value map without exceeding our
	// byte cap. 32 leaves generous headroom for legitimate clients.
	maxMultipartFields = 32
)

// rejectIfTooManyFields returns true (and writes a 400) when the
// multipart form carries more than maxMultipartFields non-file entries.
// Callers must invoke this immediately after ParseMultipartForm and bail
// out on a true return. File uploads are counted separately by the
// caller-specific "files"/"file" slice length checks.
func rejectIfTooManyFields(w http.ResponseWriter, r *http.Request) bool {
	if r.MultipartForm == nil {
		return false
	}
	total := 0
	for _, vs := range r.MultipartForm.Value {
		total += len(vs)
		if total > maxMultipartFields {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many form fields"})
			return true
		}
	}
	return false
}

// SendHandler serves the HTTP send API, delegating to Hub for local sends.
type SendHandler struct {
	nodeAccess    NodeAccessor
	hub           *Hub
	uploadStore   *uploadStore
	uploadLimiter *ipLimiter // per-IP upload rate limiter (10/min)
	sendLimiter   *ipLimiter // per-IP send rate limiter (30/min)
	trustedProxy  bool       // whether to trust X-Forwarded-For for client IP
}

// ownerKeyFromCookie returns a stable owner key derived from an HMAC
// auth-cookie value. The cookie is itself an HMAC hex string so hashing it
// ensures the owner key does not leak raw MAC material (the old code used a
// raw 16-char cookie prefix which exposed half of the MAC).
func ownerKeyFromCookie(cookieValue string) string {
	if cookieValue == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cookieValue))
	return hex.EncodeToString(sum[:8])
}

// uploadOwner derives a stable owner key from the request's auth cookie,
// Bearer token, or (as a fallback) client IP. Cookie and Bearer paths both
// end up as hex-encoded SHA-256 prefixes so HTTP and WebSocket owner keys
// are comparable when both sides hold the same cookie.
func uploadOwner(r *http.Request, trustedProxy bool) string {
	if c, err := r.Cookie(authCookieName); err == nil && c.Value != "" {
		return ownerKeyFromCookie(c.Value)
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		if token != "" {
			sum := sha256.Sum256([]byte(token))
			return hex.EncodeToString(sum[:8])
		}
	}
	// Unauthenticated (no-token mode): use real client IP as owner.
	return clientIP(r, trustedProxy)
}

// parseAttachmentFile reads and validates a single multipart file. Images
// become KindImageInline with raw bytes; PDFs become KindFileRef with the
// bytes still in Data for the caller to later persist to the session
// workspace (the HTTP layer doesn't know which workspace yet when the
// upload-only endpoint is hit). Anything else is rejected.
//
// allowPDF is false for the legacy inline-multipart path of /api/sessions/send:
// that path caps body size at ~22 MB for images only, so letting a 32 MB
// PDF through would trigger a confusing "bad multipart form" error from
// MaxBytesReader instead of a clean "too large" message. The upload-only
// endpoint sets allowPDF=true.
func parseAttachmentFile(fh *multipart.FileHeader, allowPDF bool) (cli.Attachment, error) {
	declared := fh.Header.Get("Content-Type")
	isPDF := declared == "application/pdf"
	if isPDF && !allowPDF {
		return cli.Attachment{}, fmt.Errorf("PDF attachments must be sent via /api/sessions/upload")
	}

	// Size gates before read: refuse oversize on metadata alone so we don't
	// pull a 50 MB file into memory just to reject it.
	switch {
	case isPDF:
		if fh.Size > maxPDFBytes {
			return cli.Attachment{}, fmt.Errorf("PDF too large (max %d MB)", maxPDFBytes>>20)
		}
	default:
		if fh.Size > maxImageBytes {
			return cli.Attachment{}, fmt.Errorf("file too large (max %d MB)", maxImageBytes>>20)
		}
	}

	f, err := fh.Open()
	if err != nil {
		// Wrapped os.PathError can surface the temp-file path; keep that for
		// operator logs, return a generic message to the client.
		slog.Debug("upload: open multipart file failed", "err", err)
		return cli.Attachment{}, errors.New("failed to read uploaded file")
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		slog.Debug("upload: read multipart file failed", "err", err)
		return cli.Attachment{}, errors.New("failed to read uploaded file")
	}

	// RNEW-SEC-002: defence-in-depth against a PDF-shaped gzip bomb. Our
	// downstream doesn't decompress attachments today, but the two-byte
	// gzip magic (0x1F 0x8B) would not trigger DetectContentType's PDF
	// branch — so this check catches it before anything else can. Still
	// we reject explicitly to stop any future component from unwittingly
	// accepting a compressed container for text/pdf.
	if len(data) >= 2 && data[0] == 0x1F && data[1] == 0x8B {
		return cli.Attachment{}, fmt.Errorf("compressed files are not accepted")
	}
	detected := http.DetectContentType(data)
	if isPDF {
		// PDF magic is "%PDF-" in the first 5 bytes; http.DetectContentType
		// returns "application/pdf" for that signature. A caller claiming
		// PDF but sniffing as anything else is either spoofing or corrupt —
		// reject before we persist bytes we can't trust.
		if detected != "application/pdf" {
			return cli.Attachment{}, fmt.Errorf("file does not look like a PDF")
		}
		return cli.Attachment{
			Kind:     cli.KindFileRef,
			Data:     data,
			MimeType: "application/pdf",
			OrigName: sanitizeClientFilename(fh.Filename),
			Size:     int64(len(data)),
		}, nil
	}

	// Image path — preserve the pre-PDF allowlist and prefix guard. SVG is
	// deliberately rejected: even though DetectContentType returns text/xml
	// for SVG (which would already fall through the image/* prefix check),
	// we allowlist the raster formats Claude actually accepts so a future
	// sniffer change cannot silently let SVG + script payloads through.
	if !strings.HasPrefix(declared, "image/") {
		return cli.Attachment{}, fmt.Errorf("only image/* or application/pdf files are accepted")
	}
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		// ok
	default:
		return cli.Attachment{}, fmt.Errorf("unsupported image format (jpeg/png/gif/webp only)")
	}
	return cli.Attachment{
		Kind:     cli.KindImageInline,
		Data:     data,
		MimeType: detected,
	}, nil
}

// parseImageFile is retained as a thin shim so test helpers and any external
// callers that imported the old name keep compiling during the migration.
// New code should call parseAttachmentFile directly with allowPDF explicitly.
func parseImageFile(fh *multipart.FileHeader) (cli.ImageData, error) {
	return parseAttachmentFile(fh, false)
}

// hasFileRef reports whether any attachment in atts needs to be persisted
// to the session workspace (PDF today; other formats in the future). O(n)
// on a list already bounded at maxFilesPerSend (20), so cost is trivial.
func hasFileRef(atts []cli.Attachment) bool {
	for _, a := range atts {
		if a.Kind == cli.KindFileRef {
			return true
		}
	}
	return false
}

// hasPersistableAttachment reports whether any attachment needs to hit
// persistFileRefs. file_ref must land on disk or the Read-tool hint has
// nothing to point at; inline images are persisted on a best-effort basis
// so the dashboard lightbox can load the original instead of the 600 px
// thumbnail. If neither applies (e.g. no attachments at all), the caller
// can skip the workspace resolution + persist round trip entirely.
func hasPersistableAttachment(atts []cli.Attachment) bool {
	for _, a := range atts {
		if a.Kind == cli.KindFileRef {
			return true
		}
		if imageExtForMime(a.MimeType) != "" && len(a.Data) > 0 {
			return true
		}
	}
	return false
}

// imageExtForMime maps a recognised image MIME type to its canonical file
// extension (including the leading dot). Returns "" for anything not in
// the allowlist — matches attachment.sanitizeExt so a MIME that slips past
// here also cannot slip past Persist.
func imageExtForMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

// resolveAttachmentWorkspace picks the validated absolute path to write
// file_ref attachments under for the given session key. Resolution order:
//
//  1. If the caller's request carries an explicit `reqWorkspace`, use it
//     (matches the existing "dashboard can pick a CWD per send" semantics).
//  2. Otherwise consult the router's saved workspace for the chat:
//     - live ManagedSession.Workspace() if a session is already spawned
//     - router.GetWorkspace(chatKey) from the persisted workspaceOverrides
//     or the default workspace, as a fallback for discovered/paused sessions
//
// This plugs the bug where sending to an already-running session from the
// dashboard WS path carried msg.Workspace="" (the frontend has no reason
// to re-announce the workspace on every send) and attachment persistence
// failed with "workspace is not a valid directory".
//
// Returns the validated absolute path or an error that mirrors
// validateWorkspace's generic client-facing message. The key/hub arguments
// are required because the fallback crosses into router state.
func resolveAttachmentWorkspace(hub *Hub, sessionKey, reqWorkspace string) (string, error) {
	// Hot path: client announced a workspace — trust but validate it.
	if reqWorkspace != "" {
		return validateWorkspace(reqWorkspace, hub.allowedRoot)
	}
	// Fallback: pull from the session / router. Prefer the live session's
	// Workspace() because that is the cwd the CLI process is actually
	// running under; if it's absent (paused / discovered / fresh key), fall
	// back to the chat-prefix override lookup used across dispatch /
	// takeover paths. The router lookup takes the chat-key prefix — the
	// trailing ":general" / ":<agent>" suffix is a per-agent discriminator,
	// not part of the workspace override key.
	var ws string
	if sess := hub.router.GetSession(sessionKey); sess != nil {
		ws = sess.Workspace()
	}
	if ws == "" {
		chatKey := sessionKey
		if idx := strings.LastIndexByte(sessionKey, ':'); idx > 0 {
			chatKey = sessionKey[:idx]
		}
		ws = hub.router.GetWorkspace(chatKey)
	}
	if ws == "" {
		return "", fmt.Errorf("workspace is not a valid directory")
	}
	// Revalidate against allowedRoot — the saved workspace was validated at
	// SetWorkspace time but config changes (allowedRoot tightened since the
	// last SetWorkspace) could leave a stale entry that would otherwise
	// slip past the path-traversal gate.
	return validateWorkspace(ws, hub.allowedRoot)
}

// persistErr is the (status, msg) pair returned from persistFileRefs. The
// struct lets the handler forward both pieces without building a new error
// type hierarchy just for this one call site.
type persistErr struct {
	status int
	msg    string
}

// persistFileRefs walks atts and, for every Kind==KindFileRef entry,
// writes its Data to the session workspace via internal/attachment.Persist.
// It returns a new []cli.Attachment slice where file_ref entries now carry
// WorkspacePath (and have Data cleared to release memory) and image_inline
// entries are passed through unchanged.
//
// Image inline entries are ALSO persisted to disk as a side effect: the
// inline Data still rides along to the CLI (unchanged), but a copy is
// written to the workspace attachment directory and the relative path is
// stashed in WorkspacePath. buildUserEntry pulls it onto EventEntry so the
// dashboard lightbox can load the original via /api/sessions/attachment
// instead of a downsampled data URI. If the persist fails, we log and
// continue — the inline bytes still reach the CLI so the model call
// succeeds; only the "view original" affordance degrades to the thumbnail.
//
// rollback, when non-nil, removes every file that Persist just wrote.
// Callers use it on any failure path between this call and the point
// where sessionSend accepts the request — without rollback, a validation
// failure after persist would leak disk until the GC sweep.
//
// Workspace requirement: file_ref attachments need a real absolute path.
// If workspace is empty or relative, we refuse here rather than silently
// writing somewhere unexpected. The same guard fires upstream in
// session.router (workspace resolution), but surfacing it at the HTTP
// layer gives the user a readable 400 instead of a generic forbidden.
func persistFileRefs(workspace string, atts []cli.Attachment, sessionKey, owner string) ([]cli.Attachment, func(), *persistErr) {
	if workspace == "" {
		return nil, nil, &persistErr{status: http.StatusBadRequest, msg: "workspace is required for file attachments"}
	}

	out := make([]cli.Attachment, len(atts))
	// written tracks absPaths across the batch so rollback can remove
	// every file if a later element fails. Capacity matches the realistic
	// upper bound (batch size) to avoid a growth reallocation in the
	// common happy path.
	written := make([]string, 0, len(atts))
	rollback := func() {
		for _, p := range written {
			attachment.Remove(p)
		}
	}

	for i, a := range atts {
		if a.Kind != cli.KindFileRef {
			out[i] = a
			// Inline images: best-effort persist a copy to disk so the
			// dashboard lightbox can fetch the original. Failure is
			// non-fatal — the inline Data still rides along to the CLI.
			// out[i].Data is DELIBERATELY retained (unlike file_ref
			// below which clears Data post-persist) because the inline
			// image path ships bytes as a content block; the on-disk
			// copy is purely for the dashboard's "view original" URL.
			// Lifetime is request-scoped — the duplicate bytes are
			// freed when the send completes, not cached indefinitely.
			if ext := imageExtForMime(a.MimeType); ext != "" && len(a.Data) > 0 {
				meta := attachment.Meta{
					OrigName:   a.OrigName,
					MimeType:   a.MimeType,
					Size:       int64(len(a.Data)),
					SessionKey: sessionKey,
					Owner:      owner,
				}
				if p, err := attachment.Persist(workspace, a.Data, ext, meta); err == nil {
					written = append(written, p.AbsPath)
					out[i].WorkspacePath = p.RelPath
				} else {
					slog.Debug("inline image persist failed",
						"key", sessionKey, "err", err)
				}
			}
			continue
		}
		// Map MimeType to an extension allowlist entry. Only PDF is live
		// today — future formats add a case.
		var ext string
		switch a.MimeType {
		case "application/pdf":
			ext = ".pdf"
		default:
			rollback()
			return nil, nil, &persistErr{
				status: http.StatusBadRequest,
				msg:    "unsupported attachment type",
			}
		}
		meta := attachment.Meta{
			OrigName:   a.OrigName,
			MimeType:   a.MimeType,
			Size:       int64(len(a.Data)),
			SessionKey: sessionKey,
			Owner:      owner,
		}
		p, err := attachment.Persist(workspace, a.Data, ext, meta)
		if err != nil {
			rollback()
			// A disk-full / permission error is operator-visible via slog but
			// collapsed to a generic message for the client; exposing the path
			// would leak workspace layout.
			slog.Warn("attachment persist failed",
				"key", sessionKey, "owner", owner, "err", err)
			return nil, nil, &persistErr{
				status: http.StatusInternalServerError,
				msg:    "failed to save attachment",
			}
		}
		written = append(written, p.AbsPath)
		out[i] = cli.Attachment{
			Kind:          cli.KindFileRef,
			MimeType:      a.MimeType,
			WorkspacePath: p.RelPath,
			OrigName:      a.OrigName,
			Size:          p.Size,
			// Data intentionally nil: coalesce/dispatch will copy this slice
			// multiple times and we don't want a 32 MB PDF riding along in
			// memory for a trip that only needs the path string.
		}
	}
	return out, rollback, nil
}

// sanitizeClientFilename reduces the attacker-controlled filename down to a
// display-safe string. The value flows into:
//
//  1. the .meta sidecar (JSON-encoded, so raw control bytes are escaped)
//  2. the prepended text that Claude receives
//  3. dashboard UI previews
//
// A filename containing C0 controls or path separators would confuse the
// dashboard renderer (case 3) or be mistakenly trusted as a path fragment
// if ever reused (cases that would be bugs, but cheap to preempt). We
// strip control bytes, collapse path separators to underscores, and
// truncate to a sane length. We do NOT base this on filepath.Base —
// Windows separators ("\\") on a Linux server wouldn't be stripped by
// filepath.Base.
func sanitizeClientFilename(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control chars
		case r == '/' || r == '\\':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Cap length so a 4 KB filename cannot bloat the prompt or the
	// meta sidecar. 120 runes is plenty for real filenames; longer values
	// are almost certainly adversarial.
	const maxFilenameLen = 120
	if len([]rune(out)) > maxFilenameLen {
		runes := []rune(out)
		out = string(runes[:maxFilenameLen])
	}
	return out
}

// handleUpload accepts a single image OR PDF file and stores it for later
// reference by file_ids. PDFs are held in memory until the matching send
// call, at which point they are persisted into the session workspace so
// Claude can read them via its native Read tool (images remain inline).
// POST /api/sessions/upload  (multipart/form-data, field "file")
// Response: {"id": "<hex>", "kind": "image_inline"|"file_ref", "size": <bytes>, "name": "..."}
func (h *SendHandler) handleUpload(w http.ResponseWriter, r *http.Request) {
	if h.uploadLimiter != nil && !h.uploadLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "upload rate limit exceeded"})
		return
	}
	// PDF cap dominates the body size. MaxBytesReader gives a clean 413
	// rather than the opaque "bad multipart form" that ParseMultipartForm
	// returns when the body exceeds its own limit.
	r.Body = http.MaxBytesReader(w, r.Body, int64(uploadBodyBytes))
	if err := r.ParseMultipartForm(int64(uploadBodyBytes)); err != nil {
		// Don't echo stdlib internals (boundary details, file-system paths)
		// back to the client; log internally for operator triage.
		slog.Warn("upload: multipart parse failed", "err", err)
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad multipart form"})
		return
	}
	if rejectIfTooManyFields(w, r) {
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "exactly one file required"})
		return
	}
	att, err := parseAttachmentFile(files[0], true)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	owner := uploadOwner(r, h.trustedProxy)
	id, err := h.uploadStore.Put(owner, att)
	if err != nil {
		// Distinguish per-owner quota from global exhaustion so the client
		// can show "你上传的文件过多" vs a generic "服务繁忙" prompt.
		msg := "too many pending uploads"
		if errors.Is(err, errUploadPerOwner) {
			msg = "upload quota exceeded for this user"
		}
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": msg})
		return
	}
	// Echo attachment kind + size + name so the frontend can render a PDF
	// chip differently from an image thumbnail without needing a second
	// round-trip.
	writeJSON(w, map[string]any{
		"id":   id,
		"kind": att.Kind,
		"size": att.Size,
		"name": att.OrigName,
		"mime": att.MimeType,
	})
}

func (h *SendHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	if h.sendLimiter != nil && !h.sendLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "send rate limit exceeded"})
		return
	}

	var key, text, node, workspace, resumeID, backend string
	var images []cli.ImageData
	var fileIDs []string

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		// Inline multipart uploads bypass the uploadStore per-owner quota;
		// gate them behind the dedicated uploadLimiter so a burst of
		// multipart sends can't slip past at the (looser) sendLimiter rate.
		// Without this, 30 req/min × 5 files × 10 MB = 1.5 GB/min of inline
		// file bytes would be funneled into CLI stdin.
		if h.uploadLimiter != nil && !h.uploadLimiter.AllowRequest(r) {
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "upload rate limit exceeded"})
			return
		}
		// Shrink body cap to 22 MB (2× max inline file 10 MB + form overhead)
		// and drop inline fan-out from 5→2 so authenticated users uploading
		// many attachments per turn must route through /api/sessions/upload
		// which enforces maxUploadPerOwner.
		r.Body = http.MaxBytesReader(w, r.Body, 22<<20)
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			slog.Warn("send: multipart parse failed", "err", err)
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad multipart form"})
			return
		}
		if rejectIfTooManyFields(w, r) {
			return
		}
		key = r.FormValue("key")
		text = r.FormValue("text")
		node = r.FormValue("node")
		workspace = r.FormValue("workspace")
		resumeID = r.FormValue("resume_id")
		backend = r.FormValue("backend")
		fileIDs = r.MultipartForm.Value["file_ids"]

		files := r.MultipartForm.File["files"]
		if len(files) > 2 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many inline files (max 2); use /api/sessions/upload for more"})
			return
		}
		if len(files)+len(fileIDs) > maxFilesPerSend {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("too many files (max %d)", maxFilesPerSend)})
			return
		}
		for _, fh := range files {
			img, err := parseImageFile(fh)
			if err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			images = append(images, img)
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20) // 2 MB — leaves headroom over the 1 MB text field cap
		var req struct {
			Key       string   `json:"key"`
			Text      string   `json:"text"`
			Node      string   `json:"node"`
			Workspace string   `json:"workspace"`
			ResumeID  string   `json:"resume_id"`
			Backend   string   `json:"backend"`
			FileIDs   []string `json:"file_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Debug("dashboard send: invalid JSON", "err", err)
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		key = req.Key
		text = req.Text
		node = req.Node
		workspace = req.Workspace
		resumeID = req.ResumeID
		backend = req.Backend
		fileIDs = req.FileIDs
	}

	if len(fileIDs) > maxFilesPerSend {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("too many files (max %d)", maxFilesPerSend)})
		return
	}

	// Resolve pre-uploaded file IDs — ownership-checked to prevent cross-user theft.
	// Do not echo the client-supplied fid in the error response; the id is
	// user-controlled and echoing it back with SetEscapeHTML(false) would
	// allow HTML payloads to appear unescaped in any future text/html
	// degraded path. Log the offending id internally for operator triage.
	//
	// Atomic TakeAll: if any fid is missing, expired, or foreign-owned,
	// nothing is consumed — the user can retry the whole batch after
	// re-uploading instead of losing the earlier valid images silently.
	// R37-CONCUR4.
	owner := uploadOwner(r, h.trustedProxy)
	if len(fileIDs) > 0 {
		taken, err := h.uploadStore.TakeAll(fileIDs, owner)
		if err != nil {
			slog.Debug("send: one or more file_ids not found or expired", "count", len(fileIDs))
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "file not found or expired"})
			return
		}
		images = append(images, taken...)
	}

	if key == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
		return
	}
	// Pre-validate key at the HTTP boundary so the raw attacker-controlled
	// string cannot flow into slog attrs (e.g. the "workspace validation
	// failed" Warn at send.go:166) before sessionSend's own validation
	// rejects it. Mirrors the R60-GO-H1 sanitize-before-log pattern on the
	// IM path. R60-SEC-8 / R175-SEC-P1: promoted to the full
	// session.ValidateSessionKey contract (C1 / bidi / non-UTF-8 also
	// rejected).
	if err := session.ValidateSessionKey(key); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
		return
	}
	// Enforce the same per-field text cap on the HTTP JSON/multipart path as
	// the WS path enforces (see wshub.go handleSend). Without this, the WS
	// cap is trivially bypassed by any authenticated client: the body-level
	// MaxBytesReader bounds the whole body, but a single max-sized text
	// payload would reach CoalesceMessages and drive a multi-MB CLI stdin
	// write. Inner cap matches maxWSSendTextBytes. R60-SEC-2.
	if len(text) > maxWSSendTextBytes {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "text too long"})
		return
	}
	if text == "" && len(images) == 0 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "text or files required"})
		return
	}

	// Remote-node sends don't carry attachments (the node has no way to
	// host the workspace file locally). Reject BEFORE persisting so we
	// don't leave files on disk that will never be read. The deeper
	// remote-node branch below repeats this check for defence in depth.
	if node != "" && node != "local" && len(images) > 0 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "files not supported for remote nodes"})
		return
	}

	// Persist file_ref attachments (PDFs) into the session workspace so
	// Claude's Read tool can reach them. Done here rather than in
	// sessionSend because:
	//   1. We need the authenticated owner + workspace + key all together,
	//      and this is the last HTTP-layer point where the request cookie /
	//      bearer is still in scope.
	//   2. A failure to persist must be surfaced synchronously as 4xx/5xx
	//      so the user can retry; moving it into sessionSend would require
	//      a new error sentinel and a round-trip through the queue path.
	//   3. Remote node proxying (below) doesn't take attachments, so we
	//      guard before that branch.
	//
	// Rollback semantics: `rollback` runs on EVERY failure path below
	// (400/403/5xx) but is set to nil once sessionSend reports an accepted
	// status so the files stay on disk for the session's Read tool.
	var rollback func()
	if hasPersistableAttachment(images) {
		// R61-SEC: validate workspace against allowedRoot BEFORE writing
		// anything. `workspace` is attacker-influenced (dashboard form
		// field); without this check a client could direct writes to any
		// absolute path the naozhi user can touch (e.g. /tmp). sessionSend
		// runs the same validation further down, but by then we would have
		// already persisted bytes.
		//
		// resolveAttachmentWorkspace adds a fallback to the router's saved
		// workspace when the request omits it — the dashboard does not
		// re-send the workspace on every message for an established session,
		// and without this the second PDF upload to a running session would
		// fail with "workspace is not a valid directory".
		validatedWS, err := resolveAttachmentWorkspace(h.hub, key, workspace)
		if err != nil {
			slog.Warn("attachment workspace validation failed",
				"key", key, "err", err)
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace"})
			return
		}
		resolved, rb, perr := persistFileRefs(validatedWS, images, key, owner)
		if perr != nil {
			writeJSONStatus(w, perr.status, map[string]string{"error": perr.msg})
			return
		}
		images = resolved
		rollback = rb
	}
	// Named helper so every early-return path below deletes the just-written
	// files. Safe to call when rollback is nil.
	cleanup := func() {
		if rollback != nil {
			rollback()
		}
	}

	// Remote node proxy
	if node != "" && node != "local" {
		if len(images) > 0 {
			cleanup()
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "files not supported for remote nodes"})
			return
		}
		// Syntactic workspace gate — same rationale as the WS path in
		// handleRemoteSend. The remote node's own EvalSymlinks check may
		// pass any absolute path when its defaultWorkspace is unconfigured.
		// R61-SEC-2.
		if err := validateRemoteWorkspace(workspace); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace"})
			return
		}
		nc, ok := h.nodeAccess.LookupNode(w, node)
		if !ok {
			return
		}
		capturedKey, capturedText, capturedWorkspace := key, text, workspace
		// Track via sendWG (when hub is available) so Shutdown waits for the
		// in-flight RPC before closing node connections — without this the
		// goroutine could write to a closed nc.conn after sendWG.Wait returned.
		// Use TrackSend (gated by sendTrackMu) so a late Add cannot escape
		// Shutdown's Wait — when shuttingDown fires we skip the goroutine
		// entirely and return 503 so the client can retry after restart.
		var release func()
		if h.hub != nil {
			r, shuttingDown := h.hub.TrackSend()
			if shuttingDown {
				writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "server shutting down"})
				return
			}
			release = r
		}
		go func() {
			if release != nil {
				defer release()
			}
			// Prefer hub's lifecycle ctx so shutdown cancels in-flight
			// remote sends. Fallback (test / bootstrap paths where hub is
			// nil) uses a bounded timeout rather than Background so the
			// goroutine cannot outlive the handler by more than the RPC.
			var ctx context.Context
			var cancel context.CancelFunc
			if h.hub != nil {
				ctx = h.hub.ctx
				cancel = func() {}
			} else {
				ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			}
			defer cancel()
			if err := nc.Send(ctx, capturedKey, capturedText, capturedWorkspace); err != nil {
				slog.Error("remote send", "node", node, "key", capturedKey, "err", err)
			} else {
				nc.RefreshSubscription(capturedKey)
			}
			if h.hub != nil {
				h.hub.BroadcastSessionsUpdate()
			}
		}()
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": key})
		return
	}

	reset, status, err := h.hub.sessionSend(sendParams{
		Key: key, Text: text, Images: images,
		Workspace: workspace, ResumeID: resumeID, Backend: backend,
	}, nil)
	if err != nil {
		cleanup()
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	// From this point on the attachments have entered the dispatch pipeline
	// and must remain on disk until the GC ages them out — clear rollback.
	rollback = nil
	if reset {
		writeJSON(w, map[string]string{"key": key, "status": "reset"})
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": string(status), "key": key})
}

// attachmentDirPrefix is the workspace-relative prefix every path served
// via /api/sessions/attachment must start with. Matches attachment.Dir
// expressed with forward slashes (the sole form seen in EventEntry.ImagePaths).
// Kept separate from attachment.Dir so the HTTP layer's guard does not silently
// loosen if attachment.Dir grows a platform-dependent separator someday.
const attachmentDirPrefix = ".naozhi/attachments/"

// maxAttachmentBytes caps the per-response size. Images from the dashboard
// are already downscaled to <=1600 px long edge / q0.8 so sit well under
// this; the cap exists to neutralise a crafted session that attached a
// 10 MB image before this endpoint existed — we refuse to stream it
// inline and the client falls back to the thumbnail. 16 MB leaves headroom
// for future raw-mode uploads while staying below the 50 MB project file
// cap that serveRaw uses.
const maxAttachmentBytes = 16 << 20

// handleAttachment streams an on-disk inline image from the session
// workspace attachment directory. Supersedes the data-URI thumbnail for
// the dashboard lightbox "view original" affordance — the thumbnail
// remains embedded in EventEntry.Images for backward compatibility and
// as a fallback when ImagePaths is empty.
//
// Request: GET /api/sessions/attachment?key=<session>&path=<ws-rel>
// Response: image/jpeg | image/png | image/gif | image/webp
//
// Authentication is the standard auth middleware (session cookie / Bearer).
// Authorization reuses the "session exists" boundary: anyone who can reach
// /api/sessions/events for a key can also reach its attachments. Path is
// constrained to attachmentDirPrefix under the session's current workspace,
// so a compromised key field cannot exfiltrate arbitrary workspace files.
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
	// absolute paths, no traversal, no NUL. Mirrors resolveProjectFile's
	// guard so a crafted `path` field cannot escape the attachment dir
	// even if the workspace resolution below returns a path the user
	// does not own.
	if len(relRaw) > 1024 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "path too long"})
		return
	}
	if strings.ContainsRune(relRaw, 0) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	if strings.ContainsRune(relRaw, '\\') || filepath.IsAbs(relRaw) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	cleaned := path.Clean(relRaw)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	// Pin to attachment subtree — refuses /etc/passwd, /workspace/secret.env,
	// and any other workspace path that happens to be an image. The only
	// authoritative producer of these paths is persistFileRefs writing
	// under .naozhi/attachments/<date>/<uuid>.<ext>, so a legitimate URL
	// always starts with the prefix.
	if !strings.HasPrefix(cleaned, attachmentDirPrefix) {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Resolve the session workspace. Session may be live (GetSession) or
	// paused/discovered (router.GetWorkspace fallback), matching the
	// resolveAttachmentWorkspace contract. We do NOT accept a `workspace`
	// query parameter: the path is pinned to whatever workspace is
	// associated with the key, so a crafted workspace in the query would
	// just be ignored.
	var ws string
	if sess := h.hub.router.GetSession(key); sess != nil {
		ws = sess.Workspace()
	}
	if ws == "" {
		chatKey := key
		if idx := strings.LastIndexByte(key, ':'); idx > 0 {
			chatKey = key[:idx]
		}
		ws = h.hub.router.GetWorkspace(chatKey)
	}
	if ws == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	validatedWS, err := validateWorkspace(ws, h.hub.allowedRoot)
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

	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if info.Size() > maxAttachmentBytes {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large"})
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "open failed"})
		return
	}
	defer f.Close()

	// Pin MIME from the file extension (we own the producer) rather than
	// sniffing content. attachment.sanitizeExt is the only path that can
	// create these files, so ext is always one of our allowlist entries.
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
		// Unknown extension inside our own subtree — refuse rather than
		// guess. This path is only reachable if an operator manually
		// dropped a non-image file into the attachment dir.
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// RNEW-SEC-004: ETag is derived from sha256(size||mtime) and then
	// truncated to 16 hex chars. The prior "%d-%d" form leaked both the
	// exact size in bytes AND a nanosecond-precision mtime through the
	// response header on any authenticated GET — enough to passively
	// track when a user's workspace attachments change. The hash
	// preserves cacheability (same inputs → same ETag) and collision
	// resistance is ample for a per-object validator.
	etagSeed := fmt.Sprintf("%d|%d", info.Size(), info.ModTime().UnixNano())
	etagSum := sha256.Sum256([]byte(etagSeed))
	etag := `"` + hex.EncodeToString(etagSum[:8]) + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	// Tight CSP: attachments are image-only, no inline scripts, no third-
	// party resources. sandbox closes top-level-navigation XSS channels for
	// formats (e.g. a future .svg) that slip past the ext check.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; img-src 'self' data:")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}
