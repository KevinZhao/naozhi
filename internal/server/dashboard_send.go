package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// anonCookieName labels a per-browser random bucket used ONLY in no-token
// (opt-in) mode to disambiguate uploadOwner between co-NAT users. NOT an
// auth credential — just a 16-byte random label hashed into the owner key
// so User A's upload cannot be claimed by User B via TakeAll.
const anonCookieName = "nz_anon"

// mintAnonCookie writes a freshly-random nz_anon cookie and returns its value.
// HttpOnly, SameSite=Strict (matches the auth cookie; nz_anon is only read by
// same-origin XHR so Lax offered no value and left a cross-site-GET window
// open for any future GET handler that reads it), Secure gated by
// auth.isSecure(r), 30-day MaxAge.
func mintAnonCookie(w http.ResponseWriter, r *http.Request, auth *AuthHandlers) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	val := hex.EncodeToString(buf[:])
	secure := auth != nil && auth.isSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name: anonCookieName, Value: val, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: secure, MaxAge: 30 * 24 * 3600,
	})
	return val, nil
}

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
	uploadLimiter *ipLimiter    // per-IP upload rate limiter (10/min)
	sendLimiter   *ipLimiter    // per-IP send rate limiter (30/min)
	auth          *AuthHandlers // for isSecure(r) when minting the nz_anon cookie in no-token mode
	trustedProxy  bool          // whether to trust X-Forwarded-For for client IP
}

// ownerKeyFromCookie returns a stable owner key derived from an HMAC
// auth-cookie value. The cookie is itself an HMAC hex string so hashing it
// ensures the owner key does not leak raw MAC material (the old code used a
// raw 16-char cookie prefix which exposed half of the MAC).
//
// R247-SEC-16: sha256[:8] gave 64-bit owner-key entropy — collision-find /
// preimage feasible at scale (~2^32 keys for 50% collision). Bump to
// sha256[:16] (128-bit) so the per-owner upload bucket cannot be steered
// onto another tenant's quota by a chosen-cookie collision attack. The
// owner key is opaque: only equality-tested against ownerCounts/ownerBytes
// map keys. Existing in-memory entries from prior process incarnations are
// invalidated on restart anyway (uploadStore is RAM-only), so widening
// the key has no migration cost. Mirrors R246-SEC-5 / R247-SEC-24
// (resume key) and R67-SEC-1 (WS bearer hash) which all carry ≥128-bit
// material elsewhere in the codebase.
func ownerKeyFromCookie(cookieValue string) string {
	if cookieValue == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cookieValue))
	return hex.EncodeToString(sum[:16])
}

// uploadOwner derives a stable owner key from auth cookie, Bearer token, or
// (in no-token mode) a per-browser nz_anon cookie. RNEW-SEC-005: previously
// no-token mode fell to clientIP(), so co-NAT User B could claim User A's
// upload via TakeAll. Minting nz_anon gives each browser a distinct owner;
// IP remains a last-resort fallback only when crypto/rand fails.
func uploadOwner(w http.ResponseWriter, r *http.Request, auth *AuthHandlers, trustedProxy bool) string {
	if c, err := r.Cookie(authCookieName); err == nil && c.Value != "" {
		return ownerKeyFromCookie(c.Value)
	}
	if bearer := r.Header.Get("Authorization"); strings.HasPrefix(bearer, "Bearer ") {
		if token := strings.TrimPrefix(bearer, "Bearer "); token != "" {
			// R247-SEC-16: 128-bit (matches ownerKeyFromCookie); see godoc above.
			sum := sha256.Sum256([]byte(token))
			return hex.EncodeToString(sum[:16])
		}
	}
	if c, err := r.Cookie(anonCookieName); err == nil && c.Value != "" {
		return ownerKeyFromCookie(c.Value)
	}
	if w != nil {
		val, err := mintAnonCookie(w, r, auth)
		if err == nil {
			return ownerKeyFromCookie(val)
		}
		slog.Warn("uploadOwner: mintAnonCookie failed, falling back to client IP", "err", err)
	}
	// R246-SEC-8: route the IP fallback through ownerKeyFromCookie so the
	// resulting key is the same SHA-256 hex shape the auth-cookie / Bearer /
	// nz_anon paths produce. Previously a mintAnonCookie failure (crypto/rand
	// exhausted) emitted a raw IPv4/IPv6 string that polluted ownerCounts
	// alongside SHA-256 hex keys, breaking per-owner accounting and TakeAll
	// scoping for the unlucky few requests that hit the fallback. Hashing also
	// keeps raw IP literals out of the in-memory key set.
	ip := clientIP(r, trustedProxy)
	if ip == "" {
		// Empty owner keys collide across all unauthenticated callers; mirror
		// ip_limiter's _unknown_ sentinel so we still produce a stable
		// (hashed) bucket rather than the empty string.
		ip = "_unknown_"
	}
	return ownerKeyFromCookie(ip)
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
	// declared (Content-Type header) is client-controlled and used here only
	// to pick the size gate (PDF gets a higher cap than images). A spoofed
	// `application/pdf` Content-Type therefore lets a non-PDF up to maxPDFBytes
	// past the metadata gate — this is intentional defense-in-depth: the
	// http.DetectContentType sniff below (R218B-SEC-1) re-validates against
	// magic bytes and rejects spoofed PDFs before we persist anything, and
	// images that lie about being PDFs would still fail the image-prefix path
	// since `declared` would not start with "image/". The size gate is "best
	// effort early reject", not the final authority.
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
	// fh.Size comes from the Content-Disposition header (client-controlled).
	// The size gate above rejects oversize uploads based on that header, but
	// a lying client could understate size to bypass the gate. Wrap the
	// reader in a LimitReader as a defence-in-depth byte cap, with a +1
	// margin so we can detect overflow.
	var sizeLimit int64
	if isPDF {
		sizeLimit = maxPDFBytes
	} else {
		sizeLimit = maxImageBytes
	}
	data, err := io.ReadAll(io.LimitReader(f, sizeLimit+1))
	if err != nil {
		slog.Debug("upload: read multipart file failed", "err", err)
		return cli.Attachment{}, errors.New("failed to read uploaded file")
	}
	if int64(len(data)) > sizeLimit {
		return cli.Attachment{}, fmt.Errorf("file too large (max %d MB)", sizeLimit>>20)
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

	// Image path — gate purely on the byte-sniffed `detected` MIME (R244-SEC-P2-1
	// #886). Trusting `declared` (the client-supplied Content-Type) lets a caller
	// either smuggle a non-image body past the image branch by claiming
	// "image/png", or have a legitimate PNG rejected because the client sent
	// "application/octet-stream". `http.DetectContentType` is the authoritative
	// classifier; allowlist the raster formats Claude accepts so a future
	// sniffer change cannot silently let SVG (text/xml) or any other oddity
	// through. The `declared` value is still used above to pick the size gate
	// (PDF gets a higher cap) — that is intentional defense-in-depth, not the
	// final authority.
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		// ok
	default:
		return cli.Attachment{}, fmt.Errorf("only image/* or application/pdf files are accepted")
	}
	// R232-SEC-7 (#1002): defence-in-depth secondary magic scan. http.DetectContentType
	// only inspects the leading bytes; a JFIF (or any image) header followed by an
	// embedded "%PDF-" body slips past the first sniff and would otherwise be
	// persisted as KindImageInline carrying a PDF payload. Scan the first 4 KB
	// for the PDF magic and reject — the offset window is bounded so the cost
	// is constant on legitimate uploads, while a nested PDF container is
	// always shorter than 4 KB into the file (real JFIF headers are ~20-30
	// bytes; %PDF-` would land well before 4 KB to be processed by any
	// downstream PDF reader).
	if pdfNestedInImage(data) {
		return cli.Attachment{}, fmt.Errorf("file appears to be a PDF disguised as an image")
	}
	return cli.Attachment{
		Kind:     cli.KindImageInline,
		Data:     data,
		MimeType: detected,
	}, nil
}

// pdfNestedInImage reports whether the payload — already classified as an
// image by http.DetectContentType — contains a "%PDF-" magic sequence in
// the first 4 KB. R232-SEC-7 (#1002): blocks JFIF+PDF nested-container
// bypass where the JFIF header passes the leading-byte sniff but the body
// is actually a PDF.
func pdfNestedInImage(data []byte) bool {
	const scanWindow = 4 * 1024
	end := len(data)
	if end > scanWindow {
		end = scanWindow
	}
	return bytes.Contains(data[:end], pdfMagicSignature)
}

// pdfMagicSignature is the 5-byte signature every conforming PDF starts
// with. Kept at package scope so pdfNestedInImage's bounded-window scan
// does not re-allocate the literal on each upload.
var pdfMagicSignature = []byte("%PDF-")

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
			// drop C0 control chars
		case osutil.IsLogInjectionRune(r):
			// drop C1 controls and bidi-override runes so they do not
			// reach the .meta sidecar, Content-Disposition header, or
			// the text hint Claude receives.
		case r == '/' || r == '\\':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Byte short-circuit: a string ≤ maxClientFilenameRunes bytes cannot
	// exceed maxClientFilenameRunes runes (one byte per ASCII rune is the
	// best case; multi-byte UTF-8 only increases byte count).
	if len(out) > maxClientFilenameRunes && utf8.RuneCountInString(out) > maxClientFilenameRunes {
		runes := []rune(out)
		out = string(runes[:maxClientFilenameRunes])
	}
	return out
}

// maxClientFilenameRunes caps sanitizeClientFilename output length. The
// value reaches three sinks (the .meta sidecar, the prepended text Claude
// receives, and dashboard UI previews); a 4 KB filename would bloat the
// prompt and the sidecar without any legitimate use case. 120 runes is
// plenty for real filenames; longer values are almost certainly adversarial.
// R222-CR-6.
const maxClientFilenameRunes = 120

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
	owner := uploadOwner(w, r, h.auth, h.trustedProxy)
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
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": errTooManyFiles})
			return
		}
		for _, fh := range files {
			img, err := parseAttachmentFile(fh, false)
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
		if err := decodeJSONBody(r, &req); err != nil {
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
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": errTooManyFiles})
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
	owner := uploadOwner(w, r, h.auth, h.trustedProxy)
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
		// Sprint 6b: single-pass node + cap lookup. Earlier this path
		// called LookupNode AND selectNodeForBackend in sequence, opening
		// a TOCTOU window where the node could disconnect between the two
		// GetNode calls and emit inconsistent error formats (plain text
		// vs JSON). selectNodeForBackend now is the single authority:
		// returns nc on success, sentinel error on missing-node /
		// unknown-backend / missing-cap. For claude / unset backend,
		// RequiredNodeCaps is nil so the cap loop is a no-op.
		// PR #119 review fix.
		nc, err := selectNodeForBackend(h.nodeAccess, node, backend)
		if err != nil {
			cleanup()
			// 400 across the board to keep the legacy LookupNode contract
			// (TestHandleAPISend_UnknownNode pins this). The error
			// message itself is structured (ErrUnknownBackend /
			// ErrNodeNotConnected / ErrNodeMissingCap) so dashboards
			// can errors.Is on the JSON body if richer routing is needed.
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if nc == nil {
			// nodeID was empty / "local" — fall through to local dispatch
			// without a remote send. This branch should not be reachable
			// here because the caller already gated on `node != "" &&
			// node != "local"`, but the fallback keeps the contract
			// matching selectNodeForBackend's documented semantics.
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
				slog.Error("remote send",
					"node", osutil.SanitizeForLog(node, 128),
					"key", capturedKey, "err", err)
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
		// Forward only the localised user-facing label; the raw error may
		// embed workspace paths or internal session keys that an authenticated
		// dashboard user (or a stolen cookie) should not learn from a 403.
		// Operators retain full diagnostics via the slog at sessionSend's
		// own callsite. R218-SEC-P1.
		slog.Warn("dashboard sessionSend rejected", "key", key, "err", err)
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": asyncErrorMessage(err)})
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
//
// Cross-platform contract — R241-SEC-8 (#468):
//
//   - The trailing `/` is REQUIRED. handleAttachment compares against this
//     literal via strings.HasPrefix on the POSIX-cleaned wire path, after
//     rejecting any input containing a backslash or NUL byte
//     (see handleAttachment's pre-clean above). The slash here is a wire-
//     format separator (forward slash always, on every platform), NOT a
//     filesystem separator — never substitute filepath.Separator here.
//
//   - Down-stream Joins must convert via filepath.FromSlash before passing
//     the prefix-trimmed remainder to filepath.Join (see the attachRootAbs
//     line that calls strings.TrimSuffix(attachmentDirPrefix, "/") inside
//     filepath.Join). On Windows / on a hypothetical FS with a non-`/`
//     separator, the FromSlash hop is what makes this prefix portable.
//
//   - Adding a backslash variant or making this prefix platform-conditional
//     would BREAK the wire contract: every existing EventEntry.ImagePaths
//     value on disk uses forward slashes regardless of host OS, and any
//     mismatch with the HasPrefix gate either rejects legitimate paths
//     (denial-of-service) or admits non-attachment paths (escape).
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
	// path.Clean (POSIX, forward-slash only) is correct for the wire
	// shape we accept (we already rejected backslash + IsAbs above), and
	// matches the forward-slash attachmentDirPrefix HasPrefix check
	// below. EvalSymlinks + attachRootAbs HasPrefix below provides the
	// authoritative containment check after Join.
	cleaned := path.Clean(relRaw)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	// R215-SEC-P2-3: defence-in-depth on non-Linux. On Linux path.Clean and
	// filepath.Clean are identical so this guard is a no-op. On macOS /
	// Windows the OS path semantics diverge from POSIX (case-insensitive
	// FS, alternate separators, drive letters); even though the pre-clean
	// rejects backslash + IsAbs, an OS-aware re-clean catches anything the
	// POSIX cleaner missed. We require the two cleaners to agree on the
	// same forward-slash representation — any divergence is rejected
	// rather than silently accepted, since by definition it means the
	// wire shape was not stable across the path/filepath boundary.
	if osCleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(cleaned))); osCleaned != cleaned {
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

	// Lstat (not Stat) closes the symlink-swap TOCTOU window: between
	// EvalSymlinks resolving the path and the subsequent os.Open, an
	// attacker with write access to attachRootAbs could swap a file for a
	// symlink pointing outside the workspace. Lstat reports the symlink
	// itself, letting us reject and refuse to follow.
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

	// R222-SEC-5: defence-in-depth magic-byte check. If a future code path
	// outside attachment.sanitizeExt drops a non-image into the attachment
	// subtree, the extension-only MIME pin above would still serve it as
	// e.g. image/jpeg — a browser would refuse to render it but XSS / drive-
	// by-download channels remain. http.DetectContentType inspects the first
	// 512 bytes; if the sniff disagrees with the extension's MIME we degrade
	// to application/octet-stream + Content-Disposition: attachment so the
	// payload cannot be rendered inline (e.g. as a crafted SVG). The fast-
	// path (bytes start with the expected magic) keeps inline rendering for
	// all legitimate uploads, since http.DetectContentType for a complete
	// PNG/JPEG/GIF/WebP header returns the matching image MIME.
	disposition := "inline"
	disableInlineRender := false
	var sniffBuf [512]byte
	if n, _ := io.ReadFull(f, sniffBuf[:]); n > 0 {
		sniffed := http.DetectContentType(sniffBuf[:n])
		// R226-SEC-10: defence-in-depth against future ext-allowlist drift —
		// even when the sniffed MIME matches the ext-derived mime exactly,
		// SVG / XHTML / XML payloads MUST never go inline. The current ext
		// allowlist already excludes .svg / .xml / .xhtml, but a future
		// reviewer who relaxes that without remembering the sniff guard would
		// otherwise let a `<svg onload=...>` payload render as an embedded
		// frame. http.DetectContentType returns these MIMEs with a `; charset=...`
		// suffix in some Go versions, so match on the prefix.
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

	// RNEW-SEC-004: ETag is derived from sha256(size||mtime) and then
	// truncated to 16 hex chars. The prior "%d-%d" form leaked both the
	// exact size in bytes AND a nanosecond-precision mtime through the
	// response header on any authenticated GET — enough to passively
	// track when a user's workspace attachments change. The hash
	// preserves cacheability (same inputs → same ETag) and collision
	// resistance is ample for a per-object validator.
	// R224-PERF-4: build etagSeed via strconv.AppendInt into a stack buffer
	// instead of fmt.Sprintf — this header is set on every authenticated
	// attachment GET, and fmt.Sprintf's reflection-driven formatter shows up
	// in CPU profiles for download-heavy workspaces. The two int64s + 1-byte
	// separator fit easily in 48 bytes; the SHA-256 input is byte-identical
	// to the prior "%d|%d" form so the resulting ETag is unchanged.
	var etagBuf [48]byte
	etagSeed := strconv.AppendInt(etagBuf[:0], info.Size(), 10)
	etagSeed = append(etagSeed, '|')
	etagSeed = strconv.AppendInt(etagSeed, info.ModTime().UnixNano(), 10)
	etagSum := sha256.Sum256(etagSeed)
	// R246-SEC-13: widen the ETag from 8 (64-bit) to 12 bytes (96-bit) of the
	// hash. The header is opportunistically cacheable per object, but a 64-bit
	// truncation puts birthday-bound ETag forgery within ~2^32 attempts for an
	// attacker who can passively observe many ETags. 96 bits restores
	// "cryptographically irrelevant collision risk" without bloating the
	// header beyond 24 hex chars.
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
