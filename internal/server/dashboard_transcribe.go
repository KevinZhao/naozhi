package server

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// handleAPITranscribe accepts an audio file upload and returns transcribed text.
// POST /api/transcribe  (multipart/form-data, field "audio")
func (s *Server) handleAPITranscribe(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.transcriber == nil {
		http.Error(w, "transcription not configured", http.StatusNotImplemented)
		return
	}

	const maxAudioSize = 10 << 20 // 10 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioSize+4096)
	if err := r.ParseMultipartForm(maxAudioSize); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()

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

	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "failed to read audio", http.StatusInternalServerError)
		return
	}

	mimeType := fh.Header.Get("Content-Type")
	switch mimeType {
	case "audio/ogg", "audio/mpeg", "audio/wav", "audio/flac", "audio/mp4",
		"audio/amr", "audio/webm", "audio/aac", "audio/x-m4a",
		"video/mp4", "video/webm": // some browsers tag voice memos as video
	default:
		http.Error(w, "unsupported audio format", http.StatusBadRequest)
		return
	}
	// Magic byte validation: reject files whose actual content doesn't match audio/video.
	detected := http.DetectContentType(data)
	if !strings.HasPrefix(detected, "audio/") && !strings.HasPrefix(detected, "video/") && detected != "application/octet-stream" {
		http.Error(w, "file content is not audio", http.StatusBadRequest)
		return
	}
	text, err := s.transcriber.Transcribe(r.Context(), data, mimeType)
	if err != nil {
		slog.Warn("transcribe failed", "err", err, "mime", mimeType, "size", len(data))
		http.Error(w, "transcription failed", http.StatusInternalServerError)
		return
	}

	slog.Info("transcribe ok", "text_len", len(text), "mime", mimeType, "size", len(data))

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{"text": text})
}
