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
// vision-capable side runner. Defined here (consumer side) so the handler
// can be unit-tested with a stub and so the server package doesn't take a
// hard dependency on sysession's concrete type. Implemented by
// sysession.VisionRunner, passed in via ServerOptions.ImageOrientRunner.
type VisionOrienter interface {
	RunVision(ctx context.Context, stdinLine []byte, model string) ([]byte, error)
}

// orientConfig carries the resolved runtime knobs for the feature. A nil
// *orientConfig (or runner) means the feature is off — the handler then
// returns a benign "not enabled" so the client simply skips rotation.
type orientConfig struct {
	enabled bool
	model   string
	runner  VisionOrienter
	// timeout caps the whole vision call. Haiku measured ~12s; 45s leaves
	// headroom for a cold CLI start without hanging the handler forever.
	timeout time.Duration
}

// orientTimeoutDefault is used when orientConfig.timeout is zero.
const orientTimeoutDefault = 45 * time.Second

// buildOrientConfig resolves the auto-orient feature from ServerOptions.
// Returns nil when the feature is disabled or no runner was wired — the
// handler treats a nil *orientConfig as a benign no-op. Keeping the nil
// shape (rather than a struct with enabled=false) means the handler's
// single nil-check covers "off", "no runner", and "not configured".
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
// Request JSON: {"id":"<upload-id>"}. The id must reference a live upload
// owned by the caller (Peek enforces ownership). The handler:
//  1. Peeks the image bytes (does NOT consume — the user still sends it).
//  2. Asks the vision model which edge holds the top of the text.
//  3. On an actionable verdict, rotates the bytes and Replaces the stored
//     entry in place (preserving id/owner/TTL).
//  4. Returns {"rotated":bool,"degrees":int}. rotated=false covers every
//     fail-safe path (feature off, unclear verdict, decode/rotate failure,
//     store race) — the client keeps the original image.
//
// This endpoint is best-effort: it never errors the upload. A 200 with
// rotated=false is the normal "nothing to do" response.
func (h *SendHandler) handleOrient(w http.ResponseWriter, r *http.Request) {
	// Reuse the upload limiter: orient is triggered once per uploaded image,
	// so the same 10/min budget bounds abuse without a new limiter.
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

	owner, ok := uploadOwnerOrFail(w, r, h.auth, h.trustedProxy)
	if !ok {
		return
	}

	// Feature disabled (or not wired) → benign no-op. Done AFTER owner
	// resolution so a probing client can't use this endpoint to discover
	// whether the feature exists without a valid session.
	if h.orient == nil || !h.orient.enabled || h.orient.runner == nil {
		writeJSON(w, map[string]any{"rotated": false, "degrees": 0})
		return
	}

	img := h.uploadStore.Peek(req.ID, owner)
	if img == nil {
		// Not found / expired / wrong owner — opaque, same as Take.
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
	// Echo the corrected image inline (data URL) so the client can refresh
	// its preview thumbnail without a second round-trip — there is no
	// endpoint that serves a still-pending upload-store entry by id (the
	// attachment endpoint only serves persisted workspace files), so the
	// bytes must ride back on this response. The payload is the same
	// downscaled JPEG the client already holds, re-rotated, so it's bounded.
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(rotatedBytes)
	writeJSON(w, map[string]any{"rotated": true, "degrees": verdict, "image": dataURL})
}

// orientImage runs the vision call + rotation + store replace. Returns the
// applied clockwise degrees and the rotated JPEG bytes on success; on every
// failure/no-op path it returns (0, nil) and the original stored bytes stay
// put.
func (h *SendHandler) orientImage(parent context.Context, id, owner string, img cli.ImageData) (int, []byte) {
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
	rotImg := cli.ImageData{Kind: cli.KindImageInline, Data: out, MimeType: "image/jpeg"}
	if !h.uploadStore.Replace(id, owner, rotImg) {
		// The entry expired or was consumed between Peek and Replace, or the
		// rotated payload would exceed quota. Keep the original (already
		// stored) — the user can still send it unrotated.
		slog.Info("orient: store replace rejected, keeping original", "id_present", id != "")
		return 0, nil
	}
	return v.DegreesCW, out
}
