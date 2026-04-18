package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// SendHandler serves the HTTP send API, delegating to Hub for local sends.
type SendHandler struct {
	nodeAccess    NodeAccessor
	hub           *Hub
	uploadStore   *uploadStore
	uploadLimiter *ipLimiter // per-IP upload rate limiter (10/min)
	trustedProxy  bool       // whether to trust X-Forwarded-For for client IP
}

// uploadOwner derives a stable owner key from the request's auth cookie.
// Using the raw cookie value (which is already HMAC-derived, not the raw token)
// ties each upload to the authenticated session without exposing the token.
// All dashboard sessions share the same cookie value, so this prevents
// cross-user theft rather than per-browser-tab isolation.
func uploadOwner(r *http.Request, trustedProxy bool) string {
	if c, err := r.Cookie(authCookieName); err == nil && c.Value != "" {
		// Cookie value is already an HMAC hex string — use first 16 bytes as owner key.
		if len(c.Value) >= 16 {
			return c.Value[:16]
		}
		return c.Value
	}
	// Bearer token path: hash the Authorization header value for the owner key.
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		if len(token) >= 16 {
			return hex.EncodeToString([]byte(token[:8]))
		}
		return hex.EncodeToString([]byte(token))
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
		return cli.ImageData{}, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return cli.ImageData{}, fmt.Errorf("read file: %w", err)
	}
	mime := fh.Header.Get("Content-Type")
	if !strings.HasPrefix(mime, "image/") {
		return cli.ImageData{}, fmt.Errorf("only image/* files are accepted")
	}
	detected := http.DetectContentType(data)
	if !strings.HasPrefix(detected, "image/") {
		return cli.ImageData{}, fmt.Errorf("file content does not match an image format")
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
		http.Error(w, "bad multipart form", http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		http.Error(w, "exactly one file required", http.StatusBadRequest)
		return
	}
	img, err := parseImageFile(files[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	owner := uploadOwner(r, h.trustedProxy)
	id, err := h.uploadStore.Put(owner, img)
	if err != nil {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "too many pending uploads"})
		return
	}
	writeJSON(w, map[string]string{"id": id})
}

func (h *SendHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	var key, text, node, workspace, resumeID string
	var images []cli.ImageData
	var fileIDs []string

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 105<<20) // 10 files × 10MB + form overhead
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart form", http.StatusBadRequest)
			return
		}
		key = r.FormValue("key")
		text = r.FormValue("text")
		node = r.FormValue("node")
		workspace = r.FormValue("workspace")
		resumeID = r.FormValue("resume_id")
		fileIDs = r.MultipartForm.Value["file_ids"]

		files := r.MultipartForm.File["files"]
		if len(files)+len(fileIDs) > 10 {
			http.Error(w, "too many files (max 10)", http.StatusBadRequest)
			return
		}
		for _, fh := range files {
			img, err := parseImageFile(fh)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
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
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		key = req.Key
		text = req.Text
		node = req.Node
		workspace = req.Workspace
		resumeID = req.ResumeID
		fileIDs = req.FileIDs
	}

	// Resolve pre-uploaded file IDs — ownership-checked to prevent cross-user theft.
	owner := uploadOwner(r, h.trustedProxy)
	for _, fid := range fileIDs {
		img := h.uploadStore.Take(fid, owner)
		if img == nil {
			http.Error(w, "file not found or expired: "+fid, http.StatusBadRequest)
			return
		}
		images = append(images, *img)
	}

	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if text == "" && len(images) == 0 {
		http.Error(w, "text or files required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if node != "" && node != "local" {
		if len(images) > 0 {
			http.Error(w, "files not supported for remote nodes", http.StatusBadRequest)
			return
		}
		nc, ok := h.nodeAccess.LookupNode(w, node)
		if !ok {
			return
		}
		capturedKey, capturedText, capturedWorkspace := key, text, workspace
		go func() {
			var ctx context.Context
			if h.hub != nil {
				ctx = h.hub.ctx
			} else {
				ctx = context.Background()
			}
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

	reset, err := h.hub.sessionSend(sendParams{
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
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": key})
}
