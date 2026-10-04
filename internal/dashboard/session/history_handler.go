package session

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/discovery"
)

// historyListResp is the GET /api/sessions/history body. HistorySessions is
// never null; HistoryTag is the stats.history_tag that names this list.
type historyListResp struct {
	HistorySessions []discovery.RecentSession `json:"history_sessions"`
	HistoryTag      string                    `json:"history_tag,omitempty"`
}

// HandleHistory serves GET /api/sessions/history: the filesystem sessions the
// history popover lists. /api/sessions carries only their tag, so a poll pays
// for this list only when it moved. A non-empty list is served under the
// strong ETag "h<tag>", and an If-None-Match naming it gets a bodyless 304.
func (h *Handlers) HandleHistory(w http.ResponseWriter, r *http.Request) {
	list, tag := h.historyWithTag()
	if list == nil {
		list = []discovery.RecentSession{}
	}
	if tag != "" {
		etag := `"h` + tag + `"`
		w.Header().Set("ETag", etag)
		if etagListMatches(r.Header.Get("If-None-Match"), etag) {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	httputil.WriteJSON(w, historyListResp{HistorySessions: list, HistoryTag: tag})
}
