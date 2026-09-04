package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// VisionOrienter is the minimal capability the orient handler needs from a
// vision-capable side runner. Implemented by sysession.VisionRunner, passed
// in via ServerOptions.ImageOrientRunner.
type VisionOrienter interface {
	RunVision(ctx context.Context, stdinLine []byte, model string) ([]byte, error)
}

// orientConfig carries the resolved runtime knobs for the feature. A nil
// *orientConfig (or runner) means the feature is off.
type orientConfig struct {
	enabled bool
	model   string
	runner  VisionOrienter
	// timeout caps the whole vision call (~12s typical; headroom for cold CLI start).
	timeout time.Duration
}

// orientTimeoutDefault is used when orientConfig.timeout is zero.
const orientTimeoutDefault = 45 * time.Second

// buildOrientConfig resolves the auto-orient feature from ServerOptions.
// Returns nil when disabled or no runner was wired, so the handler's single
// nil-check covers every "off" case.
func buildOrientConfig(opts ServerOptions) *orientConfig {
	if !opts.ImageOrientEnabled || opts.ImageOrientRunner == nil {
		return nil
	}
	return &orientConfig{
		enabled: true,
		model:   opts.ImageOrientModel,
		runner:  opts.ImageOrientRunner,
		timeout: orientTimeoutDefault,
	}
}

// handleOrient implements POST /api/sessions/orient.
//
// Request {"id":"<upload-id>"} must reference a live upload owned by the
// caller. Peeks (does not consume) the image, asks the vision model for the
// text orientation, and on an actionable verdict Replaces the stored entry
// in place. Best-effort: returns {"rotated":bool,"degrees":int}, with
// rotated=false on every fail-safe path so the client keeps the original.
func (h *SendHandler) handleOrient(w http.ResponseWriter, r *http.Request) {
	// Orient fires once per uploaded image, so the upload limiter's budget fits.
	if h.uploadLimiter != nil && !h.uploadLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.ID == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}
	// Validate ID shape (32 lowercase hex) before it reaches the store.
	if !isUploadID(req.ID) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	owner, ok := uploadOwnerOrFail(w, r, h.auth, h.trustedProxy)
	if !ok {
		return
	}

	// Checked AFTER owner resolution so an unauthenticated probe cannot
	// discover whether the feature exists.
	if h.orient == nil || !h.orient.enabled || h.orient.runner == nil {
		writeJSON(w, map[string]any{"rotated": false, "degrees": 0})
		return
	}

	img := h.uploadStore.Peek(req.ID, owner)
	if img == nil {
		// Not found / expired / wrong owner — deliberately opaque.
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "not found or expired"})
		return
	}
	// Only inline images are orientable. PDFs / file refs are never rotated.
	if img.Kind == cli.KindFileRef || len(img.Data) == 0 {
		writeJSON(w, map[string]any{"rotated": false, "degrees": 0})
		return
	}

	verdict, rotatedBytes := h.orientImage(r.Context(), req.ID, owner, *img)
	if rotatedBytes == nil {
		writeJSON(w, map[string]any{"rotated": false, "degrees": 0})
		return
	}
	// Echo the corrected image inline: no endpoint serves a still-pending
	// upload-store entry by id, so the preview refresh must ride this response.
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(rotatedBytes)
	writeJSON(w, map[string]any{"rotated": true, "degrees": verdict, "image": dataURL})
}

// orientImage runs the vision call + rotation + store replace. Returns the
// applied clockwise degrees and the rotated JPEG bytes on success; on every
// failure/no-op path it returns (0, nil) and the original stored bytes stay
// put.
func (h *SendHandler) orientImage(parent context.Context, id, owner string, img cli.Attachment) (int, []byte) {
	line, err := cli.BuildOrientMessage(img.Data, img.MimeType)
	if err != nil {
		slog.Warn("orient: build message failed", "err", err)
		return 0, nil
	}

	timeout := h.orient.timeout
	if timeout <= 0 {
		timeout = orientTimeoutDefault
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	stdout, err := h.orient.runner.RunVision(ctx, line, h.orient.model)
	if err != nil {
		// Timeout, CLI missing, exec failure — all best-effort skips.
		slog.Info("orient: vision call failed, leaving image as-is", "err", err)
		return 0, nil
	}

	v, actionable := cli.ParseOrientStreamJSON(stdout)
	if !actionable {
		return 0, nil
	}

	out, ok := cli.RotateJPEG(img.Data, v.DegreesCW)
	if !ok {
		slog.Warn("orient: rotate failed despite actionable verdict", "deg", v.DegreesCW)
		return 0, nil
	}

	// Re-encode always produces JPEG; reflect that in the stored mime so a
	// PNG-in/JPEG-out doesn't desync the content type sent to Claude.
	rotImg := cli.Attachment{Kind: cli.KindImageInline, Data: out, MimeType: "image/jpeg"}
	if !h.uploadStore.Replace(id, owner, rotImg) {
		// Expired/consumed between Peek and Replace, or over quota; the
		// original stays stored and sendable.
		slog.Info("orient: store replace rejected, keeping original", "id_present", id != "")
		return 0, nil
	}
	return v.DegreesCW, out
}

// isUploadID reports whether s matches the upload ID shape produced by
// uploadStore.Put: exactly 32 lowercase hex characters
// (hex.EncodeToString of 16 crypto/rand bytes).
func isUploadID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
