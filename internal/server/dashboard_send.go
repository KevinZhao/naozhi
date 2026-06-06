package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
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

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// anonCookieName / anonCookieHexLen / anonCookieMaxAgeSeconds and the
// isValidAnonCookieValue / mintAnonCookie / ownerKeyFromCookie helpers
// moved to send_anon_cookie.go (Phase 5-prep, 2026-05-28).

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
//
// router is the consumer-side SendRouter view of *session.Router (see
// consumer.go). resolveAttachmentWorkspace used to reach the router via
// h.hub.router.* transits — R215-ARCH-P1-4 / #566 closes that Phase-2.5
// cleanup item by declaring the dependency on this struct directly.
// Wiring (dashboard.go) passes hub.router; tests can inject a stub
// satisfying SendRouter.
type SendHandler struct {
	nodeAccess    NodeAccessor
	hub           *Hub
	router        SendRouter
	uploadStore   *uploadStore
	uploadLimiter *ipLimiter     // per-IP upload rate limiter (10/min)
	sendLimiter   *ipLimiter     // per-IP send rate limiter (30/min)
	auth          *auth.Handlers // for isSecure(r) when minting the nz_anon cookie in no-token mode
	trustedProxy  bool           // whether to trust X-Forwarded-For for client IP
	orient        *orientConfig  // image auto-orientation; nil = feature off
}

// uploadOwner derives a stable owner key from auth cookie, Bearer token, or
// (in no-token mode) a per-browser nz_anon cookie. RNEW-SEC-005: previously
// no-token mode fell to clientIP(), so co-NAT User B could claim User A's
// upload via TakeAll. Minting nz_anon gives each browser a distinct owner.
//
// R247-SEC-8 (#501): the `ok=false` return signals "could not derive a
// per-browser owner key" — typically because every credential path was
// absent AND mintAnonCookie failed (crypto/rand exhausted on this kernel).
// Callers must surface 503 (retry) so the client retries instead of being
// silently bucketed alongside every other co-NAT browser via the legacy
// IP fallback. The IP fallback was hashed in R246-SEC-8 to keep the key
// shape uniform, but the underlying tenancy collision (User A's upload
// claimable by User B at the same NAT) remained — returning ok=false
// closes that hole at the API surface.
func uploadOwner(w http.ResponseWriter, r *http.Request, ah *auth.Handlers, trustedProxy bool) (string, bool) {
	// R040034-SEC-2 (#1399): the auth-cookie branch must verify the
	// cookie value matches the current cookieMAC before deriving an
	// owner key. Without the gate, a caller carrying both a Bearer
	// header (which authenticates them) and a stale-or-forged
	// `nz_auth=AAA` cookie would have their uploadOwner derived from
	// the cookie because the cookie branch runs first — letting one
	// authenticated identity bucket-shift across separate upload quota
	// namespaces by tweaking the cookie value. Constant-time compare
	// rejects forged values; we then fall through to the Bearer or
	// nz_anon branches so the caller still gets a stable owner key from
	// their actual credential. When auth is nil (test harness without
	// AuthHandlers wired) or cookieMAC() returns "" (no-token mode where
	// no auth cookie should ever be honoured) the cookie branch is
	// skipped entirely — same fail-closed posture as a forged value.
	if c, err := r.Cookie(auth.AuthCookieName); err == nil && c.Value != "" && ah != nil {
		if mac := ah.CookieMAC(); mac != "" &&
			subtle.ConstantTimeCompare([]byte(c.Value), []byte(mac)) == 1 {
			return ownerKeyFromCookie(c.Value), true
		}
	}
	if bearer := r.Header.Get("Authorization"); strings.HasPrefix(bearer, "Bearer ") {
		if token := strings.TrimPrefix(bearer, "Bearer "); token != "" {
			// R247-SEC-16: 128-bit (matches ownerKeyFromCookie); see godoc above.
			sum := sha256.Sum256([]byte(token))
			return hex.EncodeToString(sum[:16]), true
		}
	}
	// R236-SEC-06 (#485): only trust the cookie when it matches
	// mintAnonCookie's wire shape (32 lowercase-hex chars). Attacker-
	// supplied values fall through to mintAnonCookie below so the
	// uploadOwner bucket is always rooted in server-generated bytes;
	// the WS upgrade path applies the same gate in wsDeriveUploadOwner.
	if c, err := r.Cookie(anonCookieName); err == nil && isValidAnonCookieValue(c.Value) {
		return ownerKeyFromCookie(c.Value), true
	}
	if w != nil {
		val, err := mintAnonCookie(w, r, ah)
		if err == nil {
			return ownerKeyFromCookie(val), true
		}
		slog.Warn("uploadOwner: mintAnonCookie failed; refusing to fall back to IP-derived owner key", "err", err)
	}
	// R247-SEC-8 (#501): on rand failure (or no ResponseWriter to mint into)
	// we explicitly do NOT fall back to a clientIP-derived owner. Two
	// co-NAT browsers would otherwise share the same SHA-256-hashed bucket,
	// re-opening the TakeAll cross-tenant theft window that nz_anon was
	// designed to close. Empty owner + ok=false signals to callers that
	// they should respond 503 (retry) instead of pretending we minted a
	// real key.
	return "", false
}

// uploadOwnerOrFail wraps uploadOwner with the standard 503 response so
// individual handlers don't repeat the writeJSONStatus boilerplate. Returns
// owner + ok=true when a real per-browser key was minted/recovered; on
// ok=false the caller MUST stop processing — the response has already been
// written.
func uploadOwnerOrFail(w http.ResponseWriter, r *http.Request, ah *auth.Handlers, trustedProxy bool) (string, bool) {
	owner, ok := uploadOwner(w, r, ah, trustedProxy)
	if !ok {
		// Service Unavailable + Retry-After hints the client/dashboard to
		// retry on a fresh socket where /dev/urandom may have replenished.
		// 30s mirrors the existing rate-limiter Retry-After convention.
		w.Header().Set("Retry-After", "30")
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "could not derive upload owner; please retry"})
	}
	return owner, ok
}

// parseAttachmentFile / pdfNestedInImage / pdfMagicSignature /
// hasPersistableAttachment / imageExtForMime moved to
// send_attachment_validate.go (Phase 3f-prep, 2026-05-28).

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

// persistErr type + persistFileRefs moved to send_persist.go
// (Phase 3f-prep, 2026-05-28).

// sanitizeClientFilename + maxClientFilenameRunes moved to
// send_attachment_validate.go (Phase 3f-prep, 2026-05-28).

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
	owner, ok := uploadOwnerOrFail(w, r, h.auth, h.trustedProxy)
	if !ok {
		return
	}
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
	owner, ok := uploadOwnerOrFail(w, r, h.auth, h.trustedProxy)
	if !ok {
		return
	}
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
			//
			// R260528-SEC-5: even when the hub is alive we cap the per-RPC
			// deadline at 60s. Without this cap a hung remote node leaks
			// a goroutine + sendWG slot for the entire process lifetime,
			// since hub.ctx only cancels on shutdown. Inheriting hub.ctx
			// preserves the shutdown-cancel behaviour while bounding the
			// worst-case RPC duration.
			var ctx context.Context
			var cancel context.CancelFunc
			if h.hub != nil {
				ctx, cancel = context.WithTimeout(h.hub.ctx, 60*time.Second)
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

// cleanAttachmentRelPath validates the workspace-relative attachment path
// the dashboard sends in ?path=. Returns (cleaned, "") on accept and
// ("", errMsg) on reject — errMsg is the same operator-facing string the
// HTTP handler used to embed inline so JSON shape stays byte-for-byte
// stable. Carved out of handleAttachment (R215-SEC-P2-3 / #536) so a unit
// test can drive the path.Clean / filepath.Clean divergence guard
// directly; the inline cleaning logic is preserved verbatim below.
//
// The second cleaner check is a no-op on Linux (path.Clean and
// filepath.Clean agree) but rejects any input that round-trips
// differently through the OS-aware cleaner — mac/Windows paths with
// alternate separators or platform-specific dot handling. Documented as
// defense-in-depth: the pre-clean already rejects backslash + IsAbs.
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
	// path.Clean (POSIX, forward-slash only) is correct for the wire
	// shape we accept (we already rejected backslash + IsAbs above), and
	// matches the forward-slash attachmentDirPrefix HasPrefix check
	// below. EvalSymlinks + attachRootAbs HasPrefix below provides the
	// authoritative containment check after Join.
	cleaned := path.Clean(relRaw)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", "invalid path"
	}
	// R215-SEC-P2-3 (#536): defence-in-depth on non-Linux. On Linux
	// path.Clean and filepath.Clean are identical so this guard is a
	// no-op for legitimate input. On macOS / Windows the OS path
	// semantics diverge from POSIX (case-insensitive FS, alternate
	// separators, drive letters); even though the pre-clean rejects
	// backslash + IsAbs, an OS-aware re-clean catches anything the POSIX
	// cleaner missed. We require the two cleaners to agree on the same
	// forward-slash representation — any divergence is rejected rather
	// than silently accepted, since by definition it means the wire
	// shape was not stable across the path/filepath boundary.
	if osCleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(cleaned))); osCleaned != cleaned {
		return "", "invalid path"
	}
	return cleaned, ""
}

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
	// does not own. The shape checks live in cleanAttachmentRelPath so a
	// unit test can exercise the path.Clean / filepath.Clean divergence
	// guard (R215-SEC-P2-3 / #536) without standing up the HTTP handler.
	cleaned, errMsg := cleanAttachmentRelPath(relRaw)
	if errMsg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": errMsg})
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
	if sess := h.router.GetSession(key); sess != nil {
		ws = sess.Workspace()
	}
	if ws == "" {
		chatKey := key
		if idx := strings.LastIndexByte(key, ':'); idx > 0 {
			chatKey = key[:idx]
		}
		ws = h.router.GetWorkspace(chatKey)
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

	// R249-SEC-3 (#917): close the Lstat→Open TOCTOU symlink-swap window.
	// Between the Lstat above and an unconstrained os.Open, an attacker
	// with write access to attachRootAbs could replace `resolved` with a
	// symlink pointing outside the workspace. dashproject.OpenWorkspaceFile uses
	// O_NOFOLLOW on unix so a final-component symlink-swap fails atomically
	// at the kernel boundary (ELOOP); the windows shim falls back to
	// plain Open with the same residual posture as the rest of the codebase.
	// Mirrors the R219-SEC-2 close already shipped for handleFileGet.
	f, err := dashproject.OpenWorkspaceFile(resolved)
	if err != nil {
		// Map symlink-trap errors and any other open failure to the same
		// 404 the rest of the handler returns — "missing or escape attempt
		// look identical" matches the dashboard contract.
		if errors.Is(err, syscall.ELOOP) {
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
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
	etagSeed := buildAttachmentETagSeed(etagBuf[:0], info.Size(), info.ModTime())
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

// buildAttachmentETagSeed builds the SHA-256 input for the attachment ETag.
// Format: "<size>|<mtime-millis>|<process-salt>" appended into dst. Caller
// passes a stack buffer slice header so the hot path stays allocation-free
// (R224-PERF-4).
//
// R20260527122801-SEC-14: millisecond precision (was UnixNano). Nanoseconds
// gave an authenticated attacker an extra 30 bits of (size, mtime) entropy
// to brute-force per object via repeated If-None-Match probes. Milliseconds
// still distinguish every real attachment write — filesystem mtime updates
// land at human-message cadence, well above 1ms granularity — so cache
// effectiveness is unaffected.
//
// R040034-SEC-3: mix in dashproject.FileETagSalt (per-process 32-byte secret, shared with
// project_files ETags). Without the salt the only inputs were (size, mtime-
// millis) — an authenticated attacker can pre-image those for a target user's
// attachment in <2^96 work and use If-None-Match / 304-vs-200 distinguishers
// to confirm guesses. The salt rotates per process restart, defeating any
// off-line precomputation. ETags rotate once on deploy (one conditional
// revalidate per client) and resume normal caching.
func buildAttachmentETagSeed(dst []byte, size int64, mtime time.Time) []byte {
	dst = strconv.AppendInt(dst, size, 10)
	dst = append(dst, '|')
	dst = strconv.AppendInt(dst, mtime.UnixMilli(), 10)
	dst = append(dst, '|')
	dst = append(dst, dashproject.FileETagSalt...)
	return dst
}
