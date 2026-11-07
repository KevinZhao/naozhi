// Package meetings hosts the meeting recording endpoints over internal/meeting
// (docs/rfc/meeting-notes.md §6, §7).
//
//	GET    /api/meetings                  the recorder's status and every meeting
//	GET    /api/meetings/{id}             one meeting with its summary and transcript
//	GET    /api/meetings/{id}/audio/{track}  the mic / remote audio (m4a, else wav)
//	POST   /api/meetings/stop             stop the recording now
//	POST   /api/meetings/authorize        bring up the system prompts (answers at once;
//	                                      the status follows on the meeting_state frame)
//	POST   /api/meetings/{id}/keep        {"keep":bool}: keep the audio past 30 days
//	POST   /api/meetings/{id}/retry       process a failed meeting again
//	DELETE /api/meetings/{id}/audio       delete the audio now
//
// All sit behind the /api/* auth middleware.
package meetings

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/meeting"
)

// Handler serves the meeting endpoints. A nil manager lists nothing and refuses changes.
type Handler struct {
	m *meeting.Manager
	// bg is the context authorisation prompts run under (the server's lifetime).
	bg context.Context
}

// New returns a Handler over m.
func New(m *meeting.Manager, bg context.Context) *Handler {
	if bg == nil {
		bg = context.Background()
	}
	return &Handler{m: m, bg: bg}
}

type listResp struct {
	Status   meeting.Status    `json:"status"`
	Meetings []meeting.Meeting `json:"meetings"`
}

// HandleList serves GET /api/meetings.
func (h *Handler) HandleList(w http.ResponseWriter, _ *http.Request) {
	resp := listResp{Meetings: []meeting.Meeting{}}
	if h.m != nil {
		resp.Status = h.m.Status()
		if ms := h.m.Store().List(); ms != nil {
			resp.Meetings = ms
		}
	}
	httputil.WriteJSON(w, resp)
}

type detailResp struct {
	Meeting    meeting.Meeting `json:"meeting"`
	Summary    string          `json:"summary"`
	Transcript []meeting.Line  `json:"transcript"`
	Audio      []string        `json:"audio"`
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) (meeting.Meeting, bool) {
	if h.m == nil {
		http.Error(w, "meeting recording not available", http.StatusServiceUnavailable)
		return meeting.Meeting{}, false
	}
	mt, err := h.m.Store().Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, "no such meeting", http.StatusNotFound)
		return meeting.Meeting{}, false
	}
	return mt, true
}

// HandleGet serves GET /api/meetings/{id}.
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	mt, ok := h.get(w, r)
	if !ok {
		return
	}
	dir := h.m.Store().Dir(mt.ID)
	resp := detailResp{Meeting: mt, Transcript: []meeting.Line{}, Audio: []string{}}
	if b, err := os.ReadFile(filepath.Join(dir, "summary.md")); err == nil {
		resp.Summary = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "transcript.json")); err == nil {
		var t struct {
			Lines []meeting.Line `json:"lines"`
		}
		if json.Unmarshal(b, &t) == nil && t.Lines != nil {
			resp.Transcript = t.Lines
		}
	}
	for _, track := range []string{"mic", "remote"} {
		if audioPath(dir, track) != "" {
			resp.Audio = append(resp.Audio, track)
		}
	}
	httputil.WriteJSON(w, resp)
}

func audioPath(dir, track string) string {
	for _, ext := range []string{".m4a", ".wav"} {
		p := filepath.Join(dir, "audio", track+ext)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// HandleAudio serves GET /api/meetings/{id}/audio/{track}; ServeFile answers Range requests,
// so the page can play from a transcript line's timestamp.
func (h *Handler) HandleAudio(w http.ResponseWriter, r *http.Request) {
	mt, ok := h.get(w, r)
	if !ok {
		return
	}
	track := r.PathValue("track")
	if track != "mic" && track != "remote" {
		http.Error(w, "track is mic or remote", http.StatusBadRequest)
		return
	}
	p := audioPath(h.m.Store().Dir(mt.ID), track)
	if p == "" {
		http.Error(w, "no audio", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeFile(w, r, p)
}

// HandleStop serves POST /api/meetings/stop.
func (h *Handler) HandleStop(w http.ResponseWriter, _ *http.Request) {
	if h.m == nil {
		http.Error(w, "meeting recording not available", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, map[string]any{"stopped": h.m.StopRecording("user")})
}

// HandleAuthorize serves POST /api/meetings/authorize. The prompts wait for the user at the
// Mac, so the request does not.
func (h *Handler) HandleAuthorize(w http.ResponseWriter, _ *http.Request) {
	if h.m == nil {
		http.Error(w, "meeting recording not available", http.StatusServiceUnavailable)
		return
	}
	go func() {
		if _, err := h.m.Authorize(h.bg); err != nil {
			slog.Warn("meeting: authorize", "err", err)
		}
	}()
	httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]any{"started": true})
}

// HandleKeep serves POST /api/meetings/{id}/keep.
func (h *Handler) HandleKeep(w http.ResponseWriter, r *http.Request) {
	mt, ok := h.get(w, r)
	if !ok {
		return
	}
	var req struct {
		Keep bool `json:"keep"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	mt, err := h.m.KeepAudio(mt.ID, req.Keep)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httputil.WriteJSON(w, mt)
}

// HandleRetry serves POST /api/meetings/{id}/retry.
func (h *Handler) HandleRetry(w http.ResponseWriter, r *http.Request) {
	mt, ok := h.get(w, r)
	if !ok {
		return
	}
	if err := h.m.Retry(mt.ID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	httputil.WriteOK(w)
}

// HandleDeleteAudio serves DELETE /api/meetings/{id}/audio.
func (h *Handler) HandleDeleteAudio(w http.ResponseWriter, r *http.Request) {
	mt, ok := h.get(w, r)
	if !ok {
		return
	}
	if err := h.m.DeleteAudio(mt.ID); err != nil {
		code := http.StatusInternalServerError
		if !errors.Is(err, os.ErrNotExist) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	httputil.WriteOK(w)
}
