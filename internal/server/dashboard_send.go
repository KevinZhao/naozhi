package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
)

func (s *Server) handleAPISend(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}

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

	// Handle /clear and /new — CLI built-in doesn't work in stream-json
	trimmed := strings.TrimSpace(text)
	if trimmed == "/clear" || trimmed == "/new" {
		s.router.Reset(key)
		if s.hub != nil {
			s.hub.BroadcastSessionsUpdate()
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]string{"key": key, "status": "reset"})
		return
	}

	// Remote node proxy
	if node != "" && node != "local" {
		nc, ok := s.lookupNode(w, node)
		if !ok {
			return
		}
		capturedKey, capturedText, capturedWorkspace := key, text, workspace
		go func() {
			var ctx context.Context
			if s.hub != nil {
				ctx = s.hub.ctx
			} else {
				ctx = context.Background()
			}
			if err := nc.Send(ctx, capturedKey, capturedText, capturedWorkspace); err != nil {
				slog.Error("remote send", "node", node, "key", capturedKey, "err", err)
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "key": key}); err != nil {
			slog.Error("encode accepted response", "err", err)
		}
		return
	}

	// Set workspace override for new dashboard sessions
	var validatedWorkspace string
	if workspace != "" {
		wsPath, err := validateWorkspace(workspace, s.allowedRoot)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		validatedWorkspace = wsPath
		if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
			chatKey := key[:idx]
			s.router.SetWorkspace(chatKey, wsPath)
		}
	}

	// Register for resume: pre-create a suspended entry so GetOrCreate
	// will --resume the specified session ID on the first message.
	if resumeID != "" && discovery.IsValidSessionID(resumeID) {
		ws := validatedWorkspace
		if ws == "" {
			ws = s.router.DefaultWorkspace()
		}
		s.router.RegisterForResume(key, resumeID, ws)
	}

	acquired := s.sessionGuard.TryAcquire(key)
	needInterrupt := !acquired

	if needInterrupt {
		// Session is running — interrupt; the goroutine below will wait for the guard
		s.router.InterruptSession(key)
		slog.Info("http send: interrupted running session for new message", "key", key)
	}

	capturedText := text
	capturedImages := images
	// Use hub context (if available) so in-flight sends are cancelled on shutdown.
	var sendCtx context.Context
	if s.hub != nil {
		sendCtx = s.hub.ctx
	} else {
		sendCtx = context.Background()
	}
	go func() {
		if needInterrupt {
			if !s.sessionGuard.AcquireTimeout(key, 5*time.Second) {
				slog.Error("http send: interrupt timed out", "key", key)
				return
			}
		}
		defer s.sessionGuard.Release(key)
		defer s.router.NotifyIdle() // wake Shutdown wait loop

		opts := buildSessionOpts(key, s.agents, s.projectMgr)
		sess, _, err := s.router.GetOrCreate(sendCtx, key, opts)
		if err != nil {
			slog.Error("dashboard send: get session", "key", key, "err", err)
			return
		}

		if _, err := s.sendWithBroadcast(sendCtx, key, sess, capturedText, capturedImages, nil); err != nil {
			slog.Error("dashboard send: send", "key", key, "err", err)
		} else {
			s.trySaveCronPrompt(key, capturedText)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "key": key}); err != nil {
		slog.Error("encode accepted response", "err", err)
	}
}
