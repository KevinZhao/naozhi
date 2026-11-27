package session

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// NoteSource lists a session's side notes and takes the user's "没用" on one
// the Planner wrote (planner/note.Notes).
type NoteSource interface {
	List(key string, now time.Time) []note.Note
	Useless(key, id string) (note.Note, error)
}

// plannerNoteID is the ID of a note the Planner wrote (note.Notes.Write).
var plannerNoteID = regexp.MustCompile(`^p[0-9a-f]{12}$`)

// HandleNotes serves GET /api/sessions/notes?key=: the gateway's side notes on
// a local session, oldest first (planner RFC §15.3 ③). The session view
// places them in its event stream by time; they are not session events.
func (h *Handlers) HandleNotes(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if err := sessionkey.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	notes := []note.Note{}
	if h.deps.Notes != nil {
		if got := h.deps.Notes.List(key, time.Now()); got != nil {
			notes = got
		}
	}
	httputil.WriteJSON(w, map[string]any{"notes": notes})
}

// HandleNoteUseless serves POST /api/sessions/notes/useless: the user marks
// note id, which the Planner wrote on session key, useless (planner RFC
// §15.22 ②). The mark is journaled and the Planner told. Only this node's
// sessions: the note is looked up in this node's journal, so one written on
// another node is not found. The route is deliberately not among a node's
// api routes (node.IsAPIRoute): the primary's page shows a remote session's
// notes without 没用, which is clicked on that node's own page.
func (h *Handlers) HandleNoteUseless(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
		ID  string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "key and id are required", http.StatusBadRequest)
		return
	}
	if err := sessionkey.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	if !plannerNoteID.MatchString(req.ID) {
		http.Error(w, "invalid id parameter", http.StatusBadRequest)
		return
	}
	if h.deps.Notes == nil {
		http.Error(w, "side notes are not available", http.StatusServiceUnavailable)
		return
	}
	nt, err := h.deps.Notes.Useless(req.Key, req.ID)
	switch {
	case errors.Is(err, note.ErrNoNote):
		http.Error(w, "note not found", http.StatusNotFound)
		return
	case err != nil:
		slog.Warn("session notes: useless mark not kept", "key", sessionkey.SanitizeLogAttr(req.Key), "err", err)
		http.Error(w, "the mark was not kept", http.StatusInternalServerError)
		return
	}
	httputil.WriteJSON(w, map[string]any{"note": nt})
}
