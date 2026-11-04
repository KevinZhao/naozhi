package session

import (
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// NoteSource lists a session's side notes (planner/note.Notes).
type NoteSource interface {
	List(key string, now time.Time) []note.Note
}

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
