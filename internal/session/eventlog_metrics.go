package session

import (
	"github.com/naozhi/naozhi/internal/metrics"
)

// eventLogMetricsObserver forwards persist.Observer callbacks to the
// process-wide expvar counters in internal/metrics. Kept here (not in persist)
// so the persist package stays independent of the metrics library.
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
