package system

import (
	"net/http"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/apiwatch"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// HandleAPIWatch serves GET /api/system/apiwatch?hours=N[&key=K]: the model
// API retries, failed turns, watchdog timeouts and latency of the last N
// hours (1–168, default 24), optionally of one session. With no store wired
// the report is empty.
func (h *Handlers) HandleAPIWatch(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 168 {
			http.Error(w, "hours must be 1-168", http.StatusBadRequest)
			return
		}
		hours = n
	}
	httputil.WriteJSON(w, h.apiWatch.Report(apiwatch.Query{
		Window: time.Duration(hours) * time.Hour,
		Key:    r.URL.Query().Get("key"),
	}))
}
