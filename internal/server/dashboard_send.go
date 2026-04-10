package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// SendHandler serves the HTTP send API, delegating to Hub for local sends.
type SendHandler struct {
	nodeAccess NodeAccessor
	hub        *Hub
}

func (h *SendHandler) handleSend(w http.ResponseWriter, r *http.Request) {
	var key, text, node, workspace, resumeID string
	var images []cli.ImageData

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

		files := r.MultipartForm.File["files"]
		if len(files) > 10 {
			http.Error(w, "too many files (max 10)", http.StatusBadRequest)
			return
		}
		for _, fh := range files {
			if fh.Size > 10<<20 {
				http.Error(w, "file too large (max 10MB)", http.StatusBadRequest)
				return
			}
			f, err := fh.Open()
			if err != nil {
				http.Error(w, "open file: "+err.Error(), http.StatusBadRequest)
				return
			}
			data, readErr := io.ReadAll(f)
			f.Close()
			if readErr != nil {
				http.Error(w, "read file: "+readErr.Error(), http.StatusBadRequest)
				return
			}
			mime := fh.Header.Get("Content-Type")
			if !strings.HasPrefix(mime, "image/") {
				http.Error(w, "only image/* files are accepted", http.StatusBadRequest)
				return
			}
			// Verify MIME type with magic-byte detection to prevent spoofed Content-Type
			detected := http.DetectContentType(data)
			if !strings.HasPrefix(detected, "image/") {
				http.Error(w, "file content does not match an image format", http.StatusBadRequest)
				return
			}
			images = append(images, cli.ImageData{Data: data, MimeType: mime})
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
		var req struct {
			Key       string `json:"key"`
			Text      string `json:"text"`
			Node      string `json:"node"`
			Workspace string `json:"workspace"`
			ResumeID  string `json:"resume_id"`
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
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"status": "accepted", "key": key})
		return
	}

	reset, err := h.hub.sessionSend(sendParams{
		Key: key, Text: text, Images: images,
		Workspace: workspace, ResumeID: resumeID,
	}, nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if reset {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]string{"key": key, "status": "reset"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "accepted", "key": key})
}
