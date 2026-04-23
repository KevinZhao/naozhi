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
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

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

// parseImageFile reads and validates a single multipart file as an image.
func parseImageFile(fh *multipart.FileHeader) (cli.ImageData, error) {
	if fh.Size > 10<<20 {
		return cli.ImageData{}, fmt.Errorf("file too large (max 10MB)")
	}
	f, err := fh.Open()
	if err != nil {
		// Wrapped os.PathError can surface the temp-file path; keep that for
		// operator logs, return a generic message to the client.
		slog.Debug("upload: open multipart file failed", "err", err)
		return cli.ImageData{}, errors.New("failed to read uploaded file")
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		slog.Debug("upload: read multipart file failed", "err", err)
		return cli.ImageData{}, errors.New("failed to read uploaded file")
	}
	mime := fh.Header.Get("Content-Type")
	if !strings.HasPrefix(mime, "image/") {
		return cli.ImageData{}, fmt.Errorf("only image/* files are accepted")
	}
	detected := http.DetectContentType(data)
	// Allowlist the raster formats Claude actually accepts. In particular
	// reject SVG: even though DetectContentType returns text/xml for svg
	// (so the prefix check below would already block it), we want a
	// defence-in-depth check against a future sniffer that labels SVG as
	// image/svg+xml — SVG can embed <script> and is unsafe to forward to
	// any consumer that renders it.
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		// ok
	default:
		return cli.ImageData{}, fmt.Errorf("unsupported image format (jpeg/png/gif/webp only)")
	}
	return cli.ImageData{Data: data, MimeType: detected}, nil
}

// handleUpload accepts a single image file and stores it for later reference by file_ids.
// POST /api/sessions/upload  (multipart/form-data, field "file")
// Response: {"id": "<hex>"}
func (h *SendHandler) handleUpload(w http.ResponseWriter, r *http.Request) {
	if h.uploadLimiter != nil && !h.uploadLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "upload rate limit exceeded"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 11<<20) // 10MB + form overhead
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		// Don't echo stdlib internals (boundary details, file-system paths)
		// back to the client; log internally for operator triage.
		slog.Warn("upload: multipart parse failed", "err", err)
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad multipart form"})
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "exactly one file required"})
		return
	}
	img, err := parseImageFile(files[0])
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	owner := uploadOwner(r, h.trustedProxy)
	id, err := h.uploadStore.Put(owner, img)
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
	writeJSON(w, map[string]string{"id": id})
}

func (h *SendHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	if h.sendLimiter != nil && !h.sendLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "send rate limit exceeded"})
		return
	}

	var key, text, node, workspace, resumeID string
	var images []cli.ImageData
	var fileIDs []string

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		// Inline multipart uploads bypass the uploadStore per-owner quota, so
		// cap the request body conservatively. Clients uploading more than 5
		// files per turn should use /api/sessions/upload + file_ids which
		// enforces maxUploadPerOwner. 5×10MB = 50MB body + form overhead.
		r.Body = http.MaxBytesReader(w, r.Body, 55<<20)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			slog.Warn("send: multipart parse failed", "err", err)
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad multipart form"})
			return
		}
		key = r.FormValue("key")
		text = r.FormValue("text")
		node = r.FormValue("node")
		workspace = r.FormValue("workspace")
		resumeID = r.FormValue("resume_id")
		fileIDs = r.MultipartForm.Value["file_ids"]

		files := r.MultipartForm.File["files"]
		if len(files) > 5 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many inline files (max 5); use /api/sessions/upload for more"})
			return
		}
		if len(files)+len(fileIDs) > 10 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many files (max 10)"})
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
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
		var req struct {
			Key       string   `json:"key"`
			Text      string   `json:"text"`
			Node      string   `json:"node"`
			Workspace string   `json:"workspace"`
			ResumeID  string   `json:"resume_id"`
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
		fileIDs = req.FileIDs
	}

	if len(fileIDs) > 10 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many files (max 10)"})
		return
	}

	// Resolve pre-uploaded file IDs — ownership-checked to prevent cross-user theft.
	// Do not echo the client-supplied fid in the error response; the id is
	// user-controlled and echoing it back with SetEscapeHTML(false) would
	// allow HTML payloads to appear unescaped in any future text/html
	// degraded path. Log the offending id internally for operator triage.
	owner := uploadOwner(r, h.trustedProxy)
	for _, fid := range fileIDs {
		img := h.uploadStore.Take(fid, owner)
		if img == nil {
			slog.Debug("send: file_id not found or expired", "fid", fid)
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "file not found or expired"})
			return
		}
		images = append(images, *img)
	}

	if key == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
		return
	}
	if text == "" && len(images) == 0 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "text or files required"})
		return
	}

	// Remote node proxy
	if node != "" && node != "local" {
		if len(images) > 0 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "files not supported for remote nodes"})
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
		Workspace: workspace, ResumeID: resumeID,
	}, nil)
	if err != nil {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if reset {
		writeJSON(w, map[string]string{"key": key, "status": "reset"})
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": string(status), "key": key})
}
