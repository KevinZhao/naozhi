package session

import (
	"net/http"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// maxRunsPageLimit caps the run-history page size (matches the store's
// keepCount ceiling — asking for more than the ring holds is pointless).
const maxRunsPageLimit = runhistory.DefaultKeepCount

// runSummaryView is the wire shape for one run row. Timestamps are unix-ms
// for parity with the cron timeline (cron_view.js consumes ms), avoiding a
// second time format on the client.
type runSummaryView struct {
	RunID       string  `json:"run_id"`
	Outcome     string  `json:"outcome"`
	StartedAt   int64   `json:"started_at"`
	EndedAt     int64   `json:"ended_at,omitempty"`
	DurationMS  int64   `json:"duration_ms"`
	FirstByteMS int64   `json:"first_byte_ms,omitempty"`
	CostUSD     float64 `json:"cost_usd,omitempty"`
	ErrorClass  string  `json:"error_class,omitempty"`
}

type runsListResp struct {
	Runs  []runSummaryView           `json:"runs"`
	Stats runhistory.SessionRunStats `json:"stats"`
}

func toRunView(r runhistory.SessionRun) runSummaryView {
	v := runSummaryView{
		RunID:       r.RunID,
		Outcome:     string(r.Outcome),
		StartedAt:   r.StartedAt.UnixMilli(),
		DurationMS:  r.DurationMS,
		FirstByteMS: r.FirstByteMS,
		CostUSD:     r.CostUSD,
		ErrorClass:  string(r.ErrorClass),
	}
	if !r.EndedAt.IsZero() {
		v.EndedAt = r.EndedAt.UnixMilli()
	}
	return v
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
	// Same key validation the events endpoint enforces (R172-SEC-L2): caps
	// length and rejects control bytes before the key reaches slog / the store.
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

	runs := h.router.SessionRuns(key, limit, before)
	views := make([]runSummaryView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toRunView(run))
	}
	// Stats always reflect the full recent window (not the paginated slice),
	// so the summary bar is stable across "load earlier" paging.
	httputil.WriteJSON(w, runsListResp{Runs: views, Stats: h.router.SessionRunStats(key)})
}
