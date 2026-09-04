// Cron execution-latency histogram (#392): a fixed-bucket cumulative
// histogram on expvar.Map, keeping the package's zero-dependency commitment.
// Buckets are cumulative "<= bound" keyed by upper bound in ms plus a
// terminal "+Inf" bucket (Prometheus convention), so "+Inf" equals the total
// observation count and per-band counts are adjacent-bucket differences.
// Cardinality is bounded by the constant bucket slice.

package metrics

import (
	"expvar"
	"strconv"
)

// cronLatencyBucketsMs are the cumulative upper bounds in ms, straddling the
// 30s defaultCronSlowThreshold so the histogram and slow counter agree.
var cronLatencyBucketsMs = []int64{
	100, 500, 1000, 5000, 15000, 30000, 60000, 120000, 300000,
}

// cronLatencyBucketKeys holds the precomputed key per bucket bound plus the
// terminal "+Inf", so Observe allocates nothing on the hot path.
var cronLatencyBucketKeys = buildCronLatencyBucketKeys()

func buildCronLatencyBucketKeys() []string {
	keys := make([]string, len(cronLatencyBucketsMs)+1)
	for i, b := range cronLatencyBucketsMs {
		keys[i] = strconv.FormatInt(b, 10)
	}
	keys[len(cronLatencyBucketsMs)] = "+Inf"
	return keys
}

var (
	// CronExecutionDurationBucket is the cumulative bucket map for cron
	// success-path latency. No `_total` suffix so the docs/ops/pprof.md
	// doc-sync contract does not demand a per-bucket row.
	CronExecutionDurationBucket = expvar.NewMap("naozhi_cron_execution_duration_ms_bucket")

	// CronExecutionDurationSum accumulates observed milliseconds; divide by
	// the "+Inf" bucket for a mean.
	CronExecutionDurationSum = expvar.NewInt("naozhi_cron_execution_duration_ms_sum")
)

// ObserveCronExecutionDuration records one cron success-path latency.
// Negative inputs (clock skew) clamp to 0 so a bogus sample cannot corrupt
// the sum or skip every bucket. Concurrency-safe: expvar Add is atomic.
func ObserveCronExecutionDuration(ms int64) {
	if ms < 0 {
		ms = 0
	}
	CronExecutionDurationSum.Add(ms)
	for i, bound := range cronLatencyBucketsMs {
		if ms <= bound {
			CronExecutionDurationBucket.Add(cronLatencyBucketKeys[i], 1)
		}
	}
	// +Inf always increments, so it tracks the total observation count.
	CronExecutionDurationBucket.Add(cronLatencyBucketKeys[len(cronLatencyBucketsMs)], 1)
}
