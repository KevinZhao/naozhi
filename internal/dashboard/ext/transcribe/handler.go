package transcribe

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
	transcribepkg "github.com/naozhi/naozhi/internal/transcribe"
)

// TranscribeSemCap is the maximum number of concurrent ffmpeg transcriptions.
// Exceeded requests receive 503 immediately to prevent CPU/memory DoS.
const TranscribeSemCap = 3

// transcribeWallClockCap bounds a single Transcribe call: ffmpeg decode relies
// on ctx-cancel (no `-t` flag), so a crafted looping stream could otherwise pin
// a TranscribeSemCap slot until the client disconnects (#499).
const transcribeWallClockCap = 10 * time.Minute

// Handler handles the audio transcription API endpoint.
type Handler struct {
	transcriber       transcribepkg.Service
	transcribeLimiter IPLimiter     // per-IP transcribe rate limiter (5/min)
	sem               chan struct{} // concurrency limiter (capacity TranscribeSemCap)
}

// HandleTranscribe accepts an audio file upload and returns transcribed text.
// POST /api/transcribe  (multipart/form-data, field "audio")
func (h *Handler) HandleTranscribe(w http.ResponseWriter, r *http.Request) {
	if h.transcribeLimiter != nil && !h.transcribeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "transcribe rate limit exceeded"})
		return
	}
	if h.transcriber == nil {
		http.Error(w, "transcription not configured", http.StatusNotImplemented)
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-r.Context().Done():
		httputil.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "transcribe busy"})
		return
	default:
		httputil.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "transcribe busy"})
		return
	}

	const maxAudioSize = 10 << 20 // 10 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioSize+4096)
	parseErr := r.ParseMultipartForm(maxAudioSize)
	// ParseMultipartForm may have written tmp files even on error.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if parseErr != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	if httputil.RejectIfTooManyFields(w, r) {
		return
	}

	files := r.MultipartForm.File["audio"]
	if len(files) == 0 {
		http.Error(w, "missing audio field", http.StatusBadRequest)
		return
	}
	fh := files[0]

	f, err := fh.Open()
	if err != nil {
		http.Error(w, "failed to read audio", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	// The envelope cap does not bound a single part; LimitReader (+1 sentinel)
	// keeps io.ReadAll bounded.
	data, err := io.ReadAll(io.LimitReader(f, maxAudioSize+1))
	if err != nil {
		http.Error(w, "failed to read audio", http.StatusInternalServerError)
		return
	}
	if len(data) > maxAudioSize {
		http.Error(w, "audio too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Step 1: allowlist the declared Content-Type before sniffing.
	declaredMIME := fh.Header.Get("Content-Type")
	switch declaredMIME {
	case "audio/ogg", "audio/mpeg", "audio/wav", "audio/flac", "audio/mp4",
		"audio/amr", "audio/webm", "audio/aac", "audio/x-m4a",
		"video/mp4", "video/webm": // some browsers tag voice memos as video
	default:
		// 415 maps to the dashboard's "unsupported audio format" message; 400
		// is reserved for malformed requests (#2433).
		http.Error(w, "unsupported audio format", http.StatusUnsupportedMediaType)
		return
	}
	// Step 2: magic bytes. DetectContentType reports "application/ogg" for
	// legitimate OGG (Feishu voice); transcribe runs a stricter DetectFormat.
	detected := http.DetectContentType(data)
	if !strings.HasPrefix(detected, "audio/") &&
		!strings.HasPrefix(detected, "video/") &&
		detected != "application/ogg" {
		http.Error(w, "file content is not audio", http.StatusUnsupportedMediaType)
		return
	}
	// Pass the sniffed MIME, not the client header, so a caller cannot coerce
	// ffmpeg into a mismatched format; application/ogg → audio/ogg skips ffmpeg.
	mimeType := detected
	if mimeType == "application/ogg" {
		mimeType = "audio/ogg"
	}
	// Wall-clock cap so crafted audio cannot pin a slot indefinitely (#499).
	tctx, tcancel := context.WithTimeout(r.Context(), transcribeWallClockCap)
	defer tcancel()
	text, err := h.transcriber.Transcribe(tctx, data, mimeType)
	if err != nil {
		slog.Warn("transcribe failed", "err", err, "mime", mimeType, "declared", declaredMIME, "size", len(data))
		http.Error(w, "transcription failed", http.StatusInternalServerError)
		return
	}

	// Cap the response so a misbehaving upstream cannot flood the browser.
	const maxTranscribeRespBytes = 1 << 20 // 1 MiB
	if len(text) > maxTranscribeRespBytes {
		slog.Warn("transcribe text truncated", "orig_len", len(text), "cap", maxTranscribeRespBytes)
		text = text[:textutil.TruncateAtRuneBoundary(text, maxTranscribeRespBytes)]
	}

	// Last-hop sanitiser before IM dispatch / dashboard wire: a future
	// transcriber (or a regression upstream) must not land bidi / C1 / LS-PS
	// runes in user-facing text. Mirrors the cron sanitiseRunResult policy (#516).
	text = osutil.SanitizeForLog(text, maxTranscribeRespBytes)

	slog.Info("transcribe ok", "text_len", len(text), "mime", mimeType, "size", len(data))

	httputil.WriteJSON(w, map[string]string{"text": text})
}

// Deps bundles all wiring for New.
type Deps struct {
	Transcriber transcribepkg.Service
	Limiter     IPLimiter
	SemCap      int
}

// denyAllLimiter is substituted when New is wired without a Limiter so a
// misconfiguration fails closed rather than silently disabling per-IP rate
// limiting (#2235). Production always injects a real limiter.
type denyAllLimiter struct{}

func (denyAllLimiter) Allow(string) bool               { return false }
func (denyAllLimiter) AllowRequest(*http.Request) bool { return false }

// New constructs a Handler from injected deps.
func New(d Deps) *Handler {
	var sem chan struct{}
	if d.SemCap > 0 {
		sem = make(chan struct{}, d.SemCap)
	}
	limiter := d.Limiter
	if limiter == nil {
		limiter = denyAllLimiter{}
	}
	return &Handler{
		transcriber:       d.Transcriber,
		transcribeLimiter: limiter,
		sem:               sem,
	}
}
