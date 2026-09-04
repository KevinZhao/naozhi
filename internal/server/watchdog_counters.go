package server

import "sync/atomic"

// watchdogCounters groups the no-output and total watchdog-kill counters
// observed by the /health handler, /api/sessions handler and the dispatch
// watchdog (#838). They are exposed as *atomic.Int64 (noOutPtr/totalPtr) for
// by-pointer DI.
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

// watchdogSnapshot is the read-side view of the kill counters. The two
// fields are loaded independently, so it is eventually-consistent rather
// than a single linearization point.
type watchdogSnapshot struct {
	NoOutputKills int64
	TotalKills    int64
}

// Snapshot returns the current kill counts in one call.
func (w *watchdogCounters) Snapshot() watchdogSnapshot {
	return watchdogSnapshot{
		NoOutputKills: w.noOutput.Load(),
		TotalKills:    w.total.Load(),
	}
}
