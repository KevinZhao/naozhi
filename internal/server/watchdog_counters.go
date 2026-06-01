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
