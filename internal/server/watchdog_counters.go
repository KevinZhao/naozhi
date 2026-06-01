package server

import "sync/atomic"

// watchdogCounters groups the no-output and total watchdog-kill counters into
// a single cohesive observability unit instead of two free-floating
// atomic.Int64 fields scattered on Server. R243-ARCH-7 / #838 — the first
// concrete step toward centralizing watchdog/metrics DI: a named home for the
// counters that the /health handler, /api/sessions handler, and the dispatch
// watchdog all observe.
//
// The two counters keep exposing *atomic.Int64 (via noOutPtr/totalPtr) so the
// existing by-pointer DI into HealthHandler / dispatch / SessionHandlers stays
// byte-for-byte compatible; only the field grouping changes.
type watchdogCounters struct {
	// noOutput counts sessions killed for producing no output within the
	// no-output timeout window.
	noOutput atomic.Int64
	// total counts all watchdog-initiated kills (no-output + total-timeout).
	total atomic.Int64
}

// noOutPtr returns the shared no-output-kill counter for by-pointer DI.
func (w *watchdogCounters) noOutPtr() *atomic.Int64 { return &w.noOutput }

// totalPtr returns the shared total-kill counter for by-pointer DI.
func (w *watchdogCounters) totalPtr() *atomic.Int64 { return &w.total }

// watchdogSnapshot is the consistent read-side view of the kill counters,
// the unified observability surface #838 asks for: a single atomic read pair
// instead of two open-coded .Load() calls scattered across the /health and
// /api/sessions handlers. Both fields are loaded independently (the counters
// are independent), so the snapshot is eventually-consistent, not a single
// linearization point — which matches the existing handler semantics.
type watchdogSnapshot struct {
	NoOutputKills int64
	TotalKills    int64
}

// Snapshot returns the current kill counts in one call. Read sites that today
// do `c.noOutPtr().Load()` + `c.totalPtr().Load()` can migrate to this so the
// counter layout stays an implementation detail of watchdogCounters.
func (w *watchdogCounters) Snapshot() watchdogSnapshot {
	return watchdogSnapshot{
		NoOutputKills: w.noOutput.Load(),
		TotalKills:    w.total.Load(),
	}
}
