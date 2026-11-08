package session

import (
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// DigestSource reads a session's digest (planner/digest.Writer).
type DigestSource interface {
	Read(key string) (updated time.Time, md []byte, ok bool)
}

// digestBody is the GET /api/sessions/digest body.
type digestBody struct {
	Key      string    `json:"key"`
	Updated  time.Time `json:"updated"`
	Markdown string    `json:"markdown"`
}

// HandleDigest serves GET /api/sessions/digest?key=: a local session's
// digest as the Planner reads it (planner RFC §15.12 ②), for the task card
// to expand; 404 while it has none. Its text is the session's own output,
// which the page renders as data.
func (h *Handlers) HandleDigest(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if err := sessionkey.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	if h.deps.Digests == nil {
		http.Error(w, "no digest", http.StatusNotFound)
		return
	}
	updated, md, ok := h.deps.Digests.Read(key)
	if !ok {
		http.Error(w, "no digest", http.StatusNotFound)
		return
	}
	httputil.WriteJSON(w, digestBody{Key: key, Updated: updated, Markdown: string(md)})
}
