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

// transcribeWallClockCap bounds the wall-clock lifetime of a single Transcribe
// call (R247-SEC-6, #499). The underlying ffmpeg decode stage relies on
// ctx-cancel propagation (no `-t` argv flag), so a crafted audio stream
// that loops indefinitely inside libavformat could otherwise occupy a
// TranscribeSemCap slot until the outer HTTP request context cancels —
// which for a long client connection may be effectively never. 10 minutes
// matches the proposal in #499 and is well above the 10 MB upload cap ×
// realistic decode throughput, so it cannot fire on legitimate audio.
const transcribeWallClockCap = 10 * time.Minute

// Handler handles the audio transcription API endpoint.
type Handler struct {
	transcriber       transcribepkg.Service
	transcribeLimiter IPLimiter    // per-IP transcribe rate limiter (5/min)
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

	// Acquire concurrency slot; reject immediately if all slots are busy.
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
	// Register cleanup before any return path. ParseMultipartForm may have
	// partially populated r.MultipartForm (and written tmp files) even on
	// error; attempting to RemoveAll on a nil form is safe to guard against.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if parseErr != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	if rejectIfTooManyFields(w, r) {
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

	// MaxBytesReader on r.Body bounds the outer multipart envelope, but
	// ParseMultipartForm may stream a single large part to a tmp file
	// without re-checking the per-part length. Wrap the reader with an
	// explicit LimitReader (+1 sentinel) so a runaway part that slipped
	// past the envelope cap (e.g. via base64-padded length-of-a-length
	// confusion) cannot exhaust memory in io.ReadAll.
	data, err := io.ReadAll(io.LimitReader(f, maxAudioSize+1))
	if err != nil {
		http.Error(w, "failed to read audio", http.StatusInternalServerError)
		return
	}
	if len(data) > maxAudioSize {
		http.Error(w, "audio too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Step 1: allowlist the client-supplied Content-Type so obviously wrong
	// uploads are rejected cheaply before we run DetectContentType.
	declaredMIME := fh.Header.Get("Content-Type")
	switch declaredMIME {
	case "audio/ogg", "audio/mpeg", "audio/wav", "audio/flac", "audio/mp4",
		"audio/amr", "audio/webm", "audio/aac", "audio/x-m4a",
		"video/mp4", "video/webm": // some browsers tag voice memos as video
	default:
		http.Error(w, "unsupported audio format", http.StatusBadRequest)
		return
	}
	// Step 2: magic-byte validation. http.DetectContentType returns
	// "application/ogg" for legitimate OGG streams (Feishu voice); accept that
	// too. The transcribe package runs a stricter DetectFormat before dispatch
	// so ffmpeg never sees content that lacks the right magic.
	detected := http.DetectContentType(data)
	if !strings.HasPrefix(detected, "audio/") &&
		!strings.HasPrefix(detected, "video/") &&
		detected != "application/ogg" {
		http.Error(w, "file content is not audio", http.StatusBadRequest)
		return
	}
	// Use the sniffed MIME (not the client-supplied header) as the hint handed
	// to the transcriber. This prevents a caller from mislabelling content to
	// coerce ffmpeg dispatch into a format that doesn't match the actual bytes.
	// Normalize application/ogg → audio/ogg so transcribe's streaming path
	// can pick up OGG uploads without spawning ffmpeg unnecessarily.
	mimeType := detected
	if mimeType == "application/ogg" {
		mimeType = "audio/ogg"
	}
	// R247-SEC-6 (#499): bound the decode stage with a wall-clock cap so
	// a crafted audio cannot pin a TranscribeSemCap slot indefinitely.
	tctx, tcancel := context.WithTimeout(r.Context(), transcribeWallClockCap)
	defer tcancel()
	text, err := h.transcriber.Transcribe(tctx, data, mimeType)
	if err != nil {
		slog.Warn("transcribe failed", "err", err, "mime", mimeType, "declared", declaredMIME, "size", len(data))
		http.Error(w, "transcription failed", http.StatusInternalServerError)
		return
	}

	// Defence-in-depth: cap the response payload so a misbehaving upstream
	// (e.g. AWS Transcribe returning a multi-megabyte transcript for a long
	// audio) cannot push an unbounded JSON body to the browser.
	const maxTranscribeRespBytes = 1 << 20 // 1 MiB
	if len(text) > maxTranscribeRespBytes {
		slog.Warn("transcribe text truncated", "orig_len", len(text), "cap", maxTranscribeRespBytes)
		text = text[:textutil.TruncateAtRuneBoundary(text, maxTranscribeRespBytes)]
	}

	// R247-SEC-18 (#516): defence-in-depth sanitiser at the dashboard
	// boundary. The upstream AWS transcriber already runs joined results
	// through osutil.SanitizeForLog, but this server-side file is the last
	// hop before the bytes hit IM dispatch / dashboard wire and a future
	// transcriber implementation (or a regression in the AWS path) must
	// not be able to land bidi / C1 / LS-PS runes — which terminal log
	// tail-ers and some browsers still interpret — into the user-facing
	// reply text. Mirrors the cron sanitiseRunResult policy so the same
	// scrub policy covers both run-result text and transcript text.
	text = osutil.SanitizeForLog(text, maxTranscribeRespBytes)

	slog.Info("transcribe ok", "text_len", len(text), "mime", mimeType, "size", len(data))

	httputil.WriteJSON(w, map[string]string{"text": text})
}

// Deps bundles all wiring for New. Phase 3d.
type Deps struct {
	Transcriber transcribepkg.Service
	Limiter     IPLimiter
	SemCap      int
}

// New constructs a Handler from injected deps.
func New(d Deps) *Handler {
	var sem chan struct{}
	if d.SemCap > 0 {
		sem = make(chan struct{}, d.SemCap)
	}
	return &Handler{
		transcriber:       d.Transcriber,
		transcribeLimiter: d.Limiter,
		sem:               sem,
	}
}
