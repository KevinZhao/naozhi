// File: metrics_observer.go
//
// R237-ARCH-5 (#582): break the leaf→caller reverse dependency where the
// server/wshub files reached straight into the internal/metrics package's
// concrete expvar globals (metrics.PanicRecoveredTotal.Add(1) etc.).
//
// Instead of every wshub call site importing internal/metrics directly, the
// package now talks to a small serverMetricsObserver interface. The default
// observer (expvarServerMetrics) forwards to the real metrics globals, so
// production behaviour is byte-for-byte unchanged. Tests can swap the
// package-level `serverMetrics` for a counting fake via withServerMetrics(...)
// to assert that an auth-fail / panic-recovery path actually bumped the
// counter, without scraping /api/debug/vars.
//
// This is the per-package Observer seam the issue's proposal calls for; it is
// intentionally scoped to the four counters the server package touches rather
// than mirroring the entire metrics surface — YAGNI keeps the seam honest and
// the diff minimal.
package server

import "github.com/naozhi/naozhi/internal/metrics"

// serverMetricsObserver is the narrow set of process counters the server /
// wshub layer increments. Implementations must be safe for concurrent use —
// every method is called from request / goroutine paths without external
// locking. The default expvar implementation satisfies this because
// expvar.Int.Add is atomic.
type serverMetricsObserver interface {
	// PanicRecovered records that a recover() boundary in the server layer
	// (wsclient readPump/writePump, wshub send / event-push fan-out) absorbed
	// a panic.
	PanicRecovered()
	// WSAuthFail records a WebSocket auth_fail reply (aggregate counter).
	WSAuthFail()
	// WSAuthFailRateLimited records the rate-limiter-triggered subset of
	// WSAuthFail.
	WSAuthFailRateLimited()
	// WSAuthFailInvalidToken records the invalid-token subset of WSAuthFail.
	WSAuthFailInvalidToken()
}

// expvarServerMetrics is the production observer: it forwards each event to the
// corresponding internal/metrics expvar global. Stateless, so a single shared
// value is reused as the package default.
type expvarServerMetrics struct{}

func (expvarServerMetrics) PanicRecovered()        { metrics.PanicRecoveredTotal.Add(1) }
func (expvarServerMetrics) WSAuthFail()            { metrics.WSAuthFailTotal.Add(1) }
func (expvarServerMetrics) WSAuthFailRateLimited() { metrics.WSAuthFailRateLimitedTotal.Add(1) }
func (expvarServerMetrics) WSAuthFailInvalidToken() {
	metrics.WSAuthFailInvalidTokenTotal.Add(1)
}

// serverMetrics is the package-level observer the server layer increments
// through. main.go does not need to inject anything — the expvar-backed
// default preserves the prior direct-global behaviour. Tests reassign it via
// withServerMetrics and restore on cleanup.
var serverMetrics serverMetricsObserver = expvarServerMetrics{}
