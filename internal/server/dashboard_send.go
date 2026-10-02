package server

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// Upload size ceilings. The PDF cap honours Anthropic's 32 MB document-block
// limit so a file accepted here is not later rejected by the API; both values
// match the byte counts announced to the user.
const (
	maxImageBytes = 10 << 20 // 10 MB
	maxPDFBytes   = 32 << 20 // 32 MB (Anthropic API limit)

	// uploadBodyBytes bounds the multipart envelope for /api/sessions/upload:
	// maxPDFBytes + ~2 MB multipart overhead.
	uploadBodyBytes = maxPDFBytes + (2 << 20)

	// maxMultipartFields caps non-file form fields per multipart request:
	// net/http allows ~1000 Value entries, so a padded body could inflate the
	// in-memory Value map without exceeding our byte cap.
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

// SendHandler serves the HTTP send API, delegating to sendEngine for local
// sends and for every router / workspace / notify need it has.
//
// engine replaced a `hub *Hub` field in #2551. The handler used to branch on
// `h.hub != nil` in five places — the HTTP layer asking whether the WebSocket
// layer exists — and now depends only on the send pipeline it actually uses.
// It is NOT optional: build it with newSendEngine (production wiring passes
// buildWSStack's w.engine), never as a zero value. See sendEngine's godoc for
// what each zero field breaks.
//
// There is deliberately no router field (#2632). Until then the handler held
// a SendRouter view AND reached engine.router for writes — two handles on one
// *session.Router, which is the exact "two views of one state" shape #2551
// filed against the old hub+router pair. Reads and writes both go through
// engine methods now; the handler touches no engine field directly, and the
// send_engine_ownership lint rule keeps it that way.
type SendHandler struct {
	nodeAccess    NodeAccessor
	engine        *sendEngine
	uploadStore   *uploadStore
	uploadLimiter *ipLimiter     // per-IP upload rate limiter (10/min)
	sendLimiter   *ipLimiter     // per-IP send rate limiter (30/min)
	auth          *auth.Handlers // for isSecure(r) when minting the nz_anon cookie in no-token mode
	trustedProxy  bool           // whether to trust X-Forwarded-For for client IP
	orient        *orientConfig  // image auto-orientation; nil = feature off
}

func (h *SendHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	if h.sendLimiter != nil && !h.sendLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": sendRateLimitedMsg})
		return
	}

	var key, text, node, workspace, resumeID, backend, accessProfile string
	var images []clievent.Attachment
	var fileIDs []string

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		// Inline multipart uploads bypass the uploadStore per-owner quota;
		// gate them behind the dedicated uploadLimiter so a burst of
		// multipart sends can't slip past at the (looser) sendLimiter rate.
		// Without this, 30 req/min × 5 files × 10 MB = 1.5 GB/min of inline
		// file bytes would be funneled into CLI stdin.
		if h.uploadLimiter != nil && !h.uploadLimiter.AllowRequest(r) {
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": uploadRateLimitedMsg})
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
		accessProfile = r.FormValue("access_profile")
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
			Key           string   `json:"key"`
			Text          string   `json:"text"`
			Node          string   `json:"node"`
			Workspace     string   `json:"workspace"`
			ResumeID      string   `json:"resume_id"`
			Backend       string   `json:"backend"`
			AccessProfile string   `json:"access_profile"`
			FileIDs       []string `json:"file_ids"`
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
		accessProfile = req.AccessProfile
		fileIDs = req.FileIDs
	}

	if len(fileIDs) > maxFilesPerSend {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": errTooManyFiles})
		return
	}

	// Pre-uploaded file IDs are ownership-checked (cross-user theft). Never
	// echo a client-supplied fid in the error (SetEscapeHTML(false) would
	// render it unescaped). TakeAll is atomic: if any fid is missing / expired
	// / foreign-owned nothing is consumed, so a batch retry works.
	owner, ok := uploadOwnerOrFail(w, r, h.auth, h.trustedProxy)
	if !ok {
		return
	}

	// Pure-input validation (key + text cap) runs BEFORE TakeAll consumes the
	// pre-uploaded entries, so a 400 here leaves the batch intact for retry;
	// rejections after TakeAll go through writeSendError with filesConsumed
	// so the client drops its stale chips (#2014).
	if key == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
		return
	}
	// Validate key at the HTTP boundary so the raw attacker-controlled string
	// (C1 / bidi / non-UTF-8) never reaches slog attrs before sessionSend's
	// own validation.
	if err := session.ValidateSessionKey(key); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
		return
	}
	// Same per-field text cap as the WS path (maxWSSendTextBytes): the body
	// MaxBytesReader alone would let one multi-MB text reach CoalesceMessages
	// and CLI stdin.
	if len(text) > maxWSSendTextBytes {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "text too long"})
		return
	}

	filesConsumed := len(fileIDs) > 0 // true ⇒ TakeAll below deletes the store entries
	if filesConsumed {
		taken, err := h.uploadStore.TakeAll(fileIDs, owner)
		if err != nil {
			slog.Debug("send: one or more file_ids not found or expired", "count", len(fileIDs))
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "file not found or expired"})
			return
		}
		images = append(images, taken...)
	}

	if text == "" && len(images) == 0 {
		writeSendError(w, http.StatusBadRequest, "text or files required", filesConsumed)
		return
	}

	// Remote-node sends don't carry attachments (the node has no way to
	// host the workspace file locally). Reject BEFORE persisting so we
	// don't leave files on disk that will never be read. The deeper
	// remote-node branch below repeats this check for defence in depth.
	if node != "" && node != "local" && len(images) > 0 {
		writeSendError(w, http.StatusBadRequest, "files not supported for remote nodes", filesConsumed)
		return
	}

	// Persist file_ref attachments (PDFs) here, not in sessionSend: this is
	// the last HTTP-layer point with owner + workspace + key in scope, a
	// persist failure must surface synchronously as 4xx/5xx, and the
	// remote-node branch below takes no attachments. rollback runs on EVERY
	// failure path below and is cleared once sessionSend accepts.
	var rollback func()
	if hasPersistableAttachment(images) {
		// Validate workspace against allowedRoot BEFORE writing anything —
		// `workspace` is attacker-influenced and sessionSend's own check runs
		// only after we would already have persisted bytes.
		// resolveAttachmentWorkspace falls back to the router's saved
		// workspace because the dashboard does not re-send it every message.
		validatedWS, err := h.engine.resolveAttachmentWorkspace(key, workspace)
		if err != nil {
			slog.Warn("attachment workspace validation failed",
				"key", session.SanitizeLogAttr(key), "err", err)
			writeSendError(w, http.StatusBadRequest, "invalid workspace", filesConsumed)
			return
		}
		resolved, rb, perr := persistFileRefs(validatedWS, images, key, owner)
		if perr != nil {
			writeSendError(w, perr.status, perr.msg, filesConsumed)
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
			writeSendError(w, http.StatusBadRequest, "files not supported for remote nodes", filesConsumed)
			return
		}
		// Syntactic workspace gate (same as WS handleRemoteSend): the remote
		// node's own EvalSymlinks check may pass any absolute path when its
		// defaultWorkspace is unconfigured.
		if err := validateRemoteWorkspace(workspace); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace"})
			return
		}
		// Refuse remote dispatch for a non-default access-profile session —
		// the env overlay is host-local and never crosses the wire (RFC
		// project-access-profile P1-a). Unconditional since #2551; this is not
		// a tightening — gateRemoteAccessProfile returns nil for a nil
		// resolver by design, which is exactly what the old `h.hub != nil`
		// guard produced for test harnesses.
		// selectNodeForBackend below is the single node + cap authority (one
		// GetNode; no TOCTOU between lookup and cap check).
		if err := h.engine.gateRemoteAccess(node, key); err != nil {
			cleanup()
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		nc, err := selectNodeForBackend(h.nodeAccess, node, backend)
		if err != nil {
			cleanup()
			// 400 across the board (TestHandleAPISend_UnknownNode pins this);
			// the message is one of the structured sentinels (ErrUnknownBackend
			// / ErrNodeNotConnected / ErrNodeMissingCap).
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if nc == nil {
			// Unreachable given the node gate above; kept to match
			// selectNodeForBackend's documented nil-return contract.
			return
		}
		// remoteSend tracks the goroutine so drain waits for the in-flight
		// RPC before closing node connections (else it could write to a
		// closed nc.conn); a late arrival during shutdown gets 503 instead of
		// a goroutine. Error fan-out to the key's subscribers is the
		// engine's job — there is no ack channel on HTTP.
		if !h.engine.remoteSend(nc, node, key, text, workspace) {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "server shutting down"})
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": key})
		return
	}

	reset, status, err := h.engine.sessionSend(sendParams{
		Key: key, Text: text, Images: images,
		Workspace: workspace, ResumeID: resumeID, Backend: backend,
		AccessProfile: accessProfile,
	}, h.engine.sendErrorCallback(key))
	if err != nil {
		cleanup()
		// Forward only the localised label: the raw error may embed workspace
		// paths or internal session keys.
		slog.Warn("dashboard sessionSend rejected", "key", session.SanitizeLogAttr(key), "err", err)
		writeSendError(w, http.StatusForbidden, asyncErrorMessage(err), filesConsumed)
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

// handleBind eagerly persists the per-chat workspace override for a freshly
// created dashboard session, WITHOUT spawning a CLI turn. POST /api/sessions/bind.
// Binding at creation time means a reload or a second browser before the first
// send still spawns in the chosen cwd instead of the workspace root.
//
// Security parity with handleSend: same send rate limiter (so /bind is not a
// cheaper override-flood vector), session key validation, allowedRoot
// validation before persisting, and an empty chat-key prefix (":agent") is
// refused. Remote-node sessions are a no-op.
func (h *SendHandler) handleBind(w http.ResponseWriter, r *http.Request) {
	if h.sendLimiter != nil && !h.sendLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": sendRateLimitedMsg})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Key       string `json:"key"`
		Node      string `json:"node"`
		Workspace string `json:"workspace"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		slog.Debug("dashboard bind: invalid JSON", "err", err)
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := session.ValidateSessionKey(req.Key); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
		return
	}
	// Remote sessions resolve their workspace on their own node — never write a
	// local-router override for them. Ack so the fire-and-forget client is happy.
	if req.Node != "" && req.Node != "local" {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.Workspace == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "workspace required"})
		return
	}
	// Persist under the 3-segment chat-key prefix (strip the trailing
	// ":agentID"), matching the SetWorkspace contract used by handleSend. A
	// key whose only colon is at index 0 (":agent") would persist the empty
	// chat key — refuse it.
	idx := strings.LastIndexByte(req.Key, ':')
	if idx <= 0 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
		return
	}
	if err := h.engine.bindWorkspace(req.Key[:idx], req.Workspace); err != nil {
		// Mirror handleSend: scrub the attacker-influenced path before logging
		// and never echo the resolved filesystem path back to the client.
		slog.Warn("bind: workspace validation failed", "err", err, "workspace", osutil.SanitizeForLog(req.Workspace, 200))
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace"})
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
