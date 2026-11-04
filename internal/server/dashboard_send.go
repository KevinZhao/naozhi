package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Upload size ceilings. The PDF cap honours Anthropic's 32 MB document-block
// limit so a file accepted here is not later rejected by the API; both values
// match the byte counts announced to the user.
const (
	maxImageBytes = 10 << 20                      // 10 MB
	maxPDFBytes   = limits.MaxFileAttachmentBytes // 32 MB (Anthropic API limit)

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
// engine methods now; the handler touches no engine field directly.
type SendHandler struct {
	nodeAccess   NodeAccessor
	engine       *sendEngine
	uploadStore  *uploadStore
	auth         *auth.Handlers // for isSecure(r) when minting the nz_anon cookie in no-token mode
	trustedProxy bool           // whether to trust X-Forwarded-For for client IP
	orient       *orientConfig  // image auto-orientation; nil = feature off
	stateDir     string         // root of tool-image ImagePaths; "" serves none
}

func (h *SendHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	var key, text, targetNode, workspace, resumeID, backend, accessProfile, sendID string
	var images []clievent.Attachment
	var fileIDs []string

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
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
		targetNode = r.FormValue("node")
		workspace = r.FormValue("workspace")
		resumeID = r.FormValue("resume_id")
		backend = r.FormValue("backend")
		accessProfile = r.FormValue("access_profile")
		sendID = r.FormValue("send_id")
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
			SendID        string   `json:"send_id"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			slog.Debug("dashboard send: invalid JSON", "err", err)
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		key = req.Key
		text = req.Text
		targetNode = req.Node
		workspace = req.Workspace
		resumeID = req.ResumeID
		backend = req.Backend
		accessProfile = req.AccessProfile
		fileIDs = req.FileIDs
		sendID = req.SendID
	}
	// The dashboard's send_id is the turn ID of a send this host forwards to
	// a node; a primary forwarding a send here names it in the header
	// (node.IdempotencyKeyHeader), and only that makes a local send run once
	// per ID. A browser's send to a local session is unchanged.
	idemKey := r.Header.Get(node.IdempotencyKeyHeader)
	if (sendID != "" && !node.ValidTurnID(sendID)) || (idemKey != "" && !node.ValidTurnID(idemKey)) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid send_id"})
		return
	}
	if idemKey != "" {
		// The echo tells the primary this node runs the ID once.
		w.Header().Set(node.IdempotencyKeyHeader, idemKey)
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
	if err := sessionkey.ValidateSessionKey(key); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
		return
	}
	// Same per-field text cap as the WS path (maxWSSendTextBytes): the body
	// MaxBytesReader alone would let one multi-MB text reach turn.Coalesce
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
	if targetNode != "" && targetNode != "local" && len(images) > 0 {
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
				"key", sessionkey.SanitizeLogAttr(key), "err", err)
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
	if targetNode != "" && targetNode != "local" {
		if len(images) > 0 {
			cleanup()
			writeSendError(w, http.StatusBadRequest, errRemoteFiles, filesConsumed)
			return
		}
		// An access profile is a host-local overlay no link carries; refuse
		// the pick rather than drop it.
		if accessProfile != "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": errRemoteAccessProfile})
			return
		}
		// Syntactic workspace gate (same as WS handleRemoteSend): the remote
		// node's own EvalSymlinks check may pass any absolute path when its
		// defaultWorkspace is unconfigured.
		if err := validateRemoteWorkspace(workspace); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace"})
			return
		}
		// Refuse remote dispatch for a scratch: or non-default access-profile
		// session — the env overlay is host-local and never crosses the wire
		// (RFC project-access-profile P1-a). Unconditional since #2551; not
		// a tightening — the profile check is a no-op for a nil resolver
		// by design, which is exactly what the old `h.hub != nil`
		// guard produced for test harnesses.
		if err := h.engine.gateRemoteAccess(targetNode, key); err != nil {
			cleanup()
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		nc, err := selectNode(h.nodeAccess, targetNode)
		if err != nil {
			cleanup()
			// 400 across the board (TestHandleAPISend_UnknownNode pins this);
			// the message is the ErrNodeNotConnected sentinel.
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if nc == nil {
			// Unreachable given the node gate above; kept to match
			// selectNode's documented nil-return contract.
			return
		}
		// The backend pick is checked here, before the 202, so a node that
		// does not run it is this request's 400 rather than a later toast.
		if !isValidBackendID(backend) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid backend identifier"})
			return
		}
		bctx, bcancel := context.WithTimeout(r.Context(), remoteNodeProxyTimeout)
		err = remoteBackend(bctx, nc, backend)
		bcancel()
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": remoteSendErrorText(err, "remote send failed")})
			return
		}
		// remoteSend tracks the goroutine so drain waits for the in-flight
		// RPC before closing node connections (else it could write to a
		// closed nc.conn); a late arrival during shutdown gets 503 instead of
		// a goroutine. Error fan-out to the key's subscribers is the
		// engine's job — there is no ack channel on HTTP.
		if !h.engine.remoteSend(nc, targetNode, node.SendRequest{
			Key: key, Text: text, Workspace: workspace,
			Backend: backend, ResumeID: resumeID, TurnID: sendID,
		}) {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "server shutting down"})
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": key})
		return
	}

	p := sendParams{
		Key: key, Text: text, Images: images,
		Workspace: workspace, ResumeID: resumeID, Backend: backend,
		AccessProfile: accessProfile,
		TraceID:       osutil.SanitizeForLog(ctxutil.TraceID(r.Context()), 128),
	}
	var reset, dup bool
	var status sendAckStatus
	var err error
	if idemKey == "" {
		reset, status, err = h.engine.sessionSend(p, h.engine.httpOrigin(key))
	} else {
		p.TraceID = idemKey
		var res node.SendResult
		res, err = h.engine.dedup.do(r.Context(), key, idemKey, func() (string, error) {
			reset, st, err := h.engine.sessionSend(p, h.engine.httpOrigin(key))
			if reset {
				return string(sendAckReset), err
			}
			return string(st), err
		})
		status, dup = sendAckStatus(res.Status), res.Dup
		reset = status == sendAckReset
	}
	if dup {
		// The first send of this ID took its own copy of any attachment.
		cleanup()
		code := http.StatusAccepted
		if reset {
			code = http.StatusOK
		}
		writeJSONStatus(w, code, map[string]any{"status": string(status), "key": key, "dup": true})
		return
	}
	if err != nil {
		cleanup()
		// Forward only the localised label: the raw error may embed workspace
		// paths or internal session keys.
		slog.Warn("dashboard sessionSend rejected", "key", sessionkey.SanitizeLogAttr(key), "err", err)
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
// Security parity with handleSend: session key validation, allowedRoot
// validation before persisting, and an empty chat-key prefix (":agent") is
// refused. Remote-node sessions are a no-op.
func (h *SendHandler) handleBind(w http.ResponseWriter, r *http.Request) {
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
	if err := sessionkey.ValidateSessionKey(req.Key); err != nil {
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
