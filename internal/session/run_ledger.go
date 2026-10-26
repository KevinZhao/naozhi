package session

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"

	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// RunLedger is the run-history and cost-ledger facet: the per-session
// run-timing store and the shared cost-accounting sink Router hands to every
// ManagedSession. Reached through Router.Runs().
type RunLedger struct {
	// runs persists per-run wall-clock timing. Constructed in NewRouter from
	// the store's datadir.Layout, injected into every ManagedSession; nil
	// when StorePath is empty. Closed in Shutdown to flush the async write
	// worker.
	runs *runhistory.Store
	// cost is the shared ledger sink handed to every ManagedSession.
	cost *costAccounting
}

// Runs returns Router's run-ledger facet, or nil when the Router is nil.
func (r *Router) Runs() *RunLedger {
	if r == nil {
		return nil
	}
	return &r.runs
}

// List returns the newest-first run-history records for a session key,
// optionally paginated: only runs started strictly before `before` are
// returned (zero `before` = no upper bound), capped at limit. Returns nil
// when run-history persistence is disabled. Read path for GET
// /api/sessions/runs — shares the same store instance the Send path writes to.
func (r *RunLedger) List(key string, limit int, before time.Time) []runhistory.SessionRun {
	return r.runs.List(key, limit, before)
}

// Stats returns the aggregate timing stats over a session's recent runs.
// Zero value when persistence is disabled or the session has no runs.
func (r *RunLedger) Stats(key string) runhistory.SessionRunStats {
	return r.runs.Stats(key)
}

// CostLedgerConfig is the router-side view of config.cost.
type CostLedgerConfig struct {
	Disabled      bool
	RetentionDays int
}

// CostLedger exposes the shared ledger for the dashboard cost API; nil when
// the router has no persistence. Read-only consumers only.
func (r *RunLedger) CostLedger() *costledger.Store {
	if r == nil || r.cost == nil {
		return nil
	}
	return r.cost.ledger
}

// Invalidate frees the resident run-history ring for key (on-disk records
// stay); the per-session ring map stays bounded. No-op when run-history
// persistence is disabled.
func (r *RunLedger) Invalidate(key string) {
	r.runs.Invalidate(key)
}

// Close flushes the run-history write worker and the cost ledger's day-file
// writer, blocking on the bounded queues draining; process-end bookings still
// running get endBookingsDrain to land first. Shutdown's teardown step; a nil
// run-history store or disabled cost ledger make their half a no-op.
func (r *RunLedger) Close() {
	r.runs.Close()
	if r.cost != nil {
		if !r.cost.waitEnds(endBookingsDrain) {
			slog.Warn("shutdown: process-end cost bookings still running; their partial entries are lost")
		}
		r.cost.ledger.Close()
	}
}
