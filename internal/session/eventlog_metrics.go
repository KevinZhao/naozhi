package session

import (
	"github.com/naozhi/naozhi/internal/metrics"
)

// eventLogMetricsObserver is the production Observer that forwards
// persist.Observer callbacks to the process-wide expvar counters in
// internal/metrics. Kept in the session package (not persist) so the
// persist package stays independent of the metrics library —
// per-test Persisters can still run with a nil / custom observer.
type eventLogMetricsObserver struct{}

func (eventLogMetricsObserver) OnWrite(n int) {
	if n <= 0 {
		return
	}
	metrics.EventLogPersistWrittenTotal.Add(int64(n))
}

func (eventLogMetricsObserver) OnDrop(n int) {
	if n <= 0 {
		return
	}
	metrics.EventLogPersistDroppedTotal.Add(int64(n))
}

func (eventLogMetricsObserver) OnFsync() {
	metrics.EventLogPersistFsyncTotal.Add(1)
}

func (eventLogMetricsObserver) OnMalformed() {
	metrics.EventLogPersistMalformedLinesTotal.Add(1)
}

func (eventLogMetricsObserver) OnReplayLeak(n int) {
	if n <= 0 {
		return
	}
	metrics.EventLogPersistReplayLeakTotal.Add(int64(n))
}
