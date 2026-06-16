package session

import (
	"time"

	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// SessionRuns returns the newest-first run-history records for a session key,
// optionally paginated: only runs started strictly before `before` are
// returned (zero `before` = no upper bound), capped at limit. Returns nil
// when run-history persistence is disabled. Read path for GET
// /api/sessions/runs — shares the same store instance the Send path writes to.
func (r *Router) SessionRuns(key string, limit int, before time.Time) []runhistory.SessionRun {
	return r.sessionRuns.List(key, limit, before)
}

// SessionRunStats returns the aggregate timing stats over a session's recent
// runs. Zero value when persistence is disabled or the session has no runs.
func (r *Router) SessionRunStats(key string) runhistory.SessionRunStats {
	return r.sessionRuns.Stats(key)
}
