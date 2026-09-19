package session

import (
	"net/http"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/dashboard/runview"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// maxRunsPageLimit caps the run-history page size (matches the store's
// keepCount ceiling — asking for more than the ring holds is pointless).
const maxRunsPageLimit = runhistory.DefaultKeepCount

// The run rows speak runview.Summary — the same shape every dashboard run
// list serves (#2540). The outcome vocabulary this endpoint used to expose
// (completed/error/timeout/canceled — four words nothing else spoke) maps
// into runtelemetry.RunState inside runview.FromSessionRun; the on-disk
// records keep their word, the wire stops here.
type runsListResp struct {
	Runs  []runview.Summary          `json:"runs"`
	Stats runhistory.SessionRunStats `json:"stats"`
}

// HandleRuns serves GET /api/sessions/runs?key=&limit=&before= — the session
// run-history timeline + aggregate stats. Read-only; shares the same store
// instance the Send path writes to (via Router).
func (h *Handlers) HandleRuns(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
	// Same key validation as the events endpoint: caps length and rejects
	// control bytes before the key reaches slog / the store.
	if err := sessionpkg.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	var (
		limit  int
		before time.Time
	)
	if s := q.Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			http.Error(w, "invalid limit parameter", http.StatusBadRequest)
			return
		}
		if v > maxRunsPageLimit {
			v = maxRunsPageLimit
		}
		limit = v
	}
	if s := q.Get("before"); s != "" {
		ms, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			http.Error(w, "invalid before parameter", http.StatusBadRequest)
			return
		}
		before = time.UnixMilli(ms)
	}

	runs := h.deps.Router.SessionRuns(key, limit, before)
	views := make([]runview.Summary, 0, len(runs))
	for _, run := range runs {
		views = append(views, runview.FromSessionRun(run))
	}
	// Stats always reflect the full recent window (not the paginated slice),
	// so the summary bar is stable across "load earlier" paging.
	httputil.WriteJSON(w, runsListResp{Runs: views, Stats: h.deps.Router.SessionRunStats(key)})
}
