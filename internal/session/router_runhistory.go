package session

import (
	"time"

	"github.com/naozhi/naozhi/internal/costledger"

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

// SessionRunsHealth is the session run-history store's loss counters, for
// /health. Records are written off the conversation goroutine and a failure
// cannot fail the user's turn, so these counters are the only signal that
// history is being lost (#2792).
type SessionRunsHealth struct {
	// Enabled is false when run history is not persisted; /health then omits
	// the section.
	Enabled bool
	// WriteFailedDiskFull / WriteFailedOther split record-write failures so
	// ENOSPC is distinguishable from EACCES / I/O errors.
	WriteFailedDiskFull int64
	WriteFailedOther    int64
	// AsyncDropped counts records dropped because the async write queue was
	// full when the turn finished.
	AsyncDropped int64
}

// SessionRunsHealth snapshots the run-history store's loss counters.
func (r *Router) SessionRunsHealth() SessionRunsHealth {
	if r == nil || !r.sessionRuns.Enabled() {
		return SessionRunsHealth{}
	}
	full, other := r.sessionRuns.WriteFailedTotals()
	return SessionRunsHealth{
		Enabled:             true,
		WriteFailedDiskFull: full,
		WriteFailedOther:    other,
		AsyncDropped:        r.sessionRuns.DropTotal(),
	}
}

// CostLedgerConfig is the router-side view of config.cost.
type CostLedgerConfig struct {
	Disabled      bool
	RetentionDays int
	RollupDays    int
}

// CostLedger exposes the shared ledger for the dashboard cost API; nil when
// the router has no persistence. Read-only consumers only.
func (r *Router) CostLedger() *costledger.Store {
	if r.costAcct == nil {
		return nil
	}
	return r.costAcct.ledger
}

// SetCostRunOwnership installs the gate that tells accountTurnCost a turn is
// owned by a cron run (which writes the ledger itself). nil disables the gate.
func (r *Router) SetCostRunOwnership(fn func(key string) bool) {
	r.costAcct.setRunOwnership(fn)
}
