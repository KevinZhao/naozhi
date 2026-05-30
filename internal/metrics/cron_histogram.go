// Cron execution-latency histogram (OBS1 / #392).
//
// Why a hand-rolled expvar histogram (not Prometheus client_golang):
// the metrics package commits to "zero deps, stdlib-stable" (see the
// metrics.go top docstring) so naozhi stays a single self-hostable
// binary. #392 explicitly weighs the lightweight-stdlib option against
// pulling in Prometheus / OpenTelemetry and lands on the stdlib path for
// the typical single-binary deployment. This file implements a fixed-
// bucket cumulative histogram on top of expvar.Map so /debug/vars
// scrapers (and the Grafana single-stat panels operators already wire to
// the existing naozhi_cron_execution_slow_total counter) gain a real
// latency distribution without a new scrape protocol.
//
// Bucket layout: cumulative "less-than-or-equal" buckets keyed by their
// upper bound in milliseconds, plus a terminal "+Inf" bucket. A single
// observation increments every bucket whose bound it falls under, which
// is the Prometheus histogram convention — a downstream PromQL-style
// query (or jq) can recover per-band counts by subtracting adjacent
// buckets. The "+Inf" bucket therefore always equals the total
// observation count, and `naozhi_cron_execution_duration_ms_sum` carries
// the running millisecond total so operators can derive a mean.
//
// Cardinality is bounded by construction: the bucket boundaries are a
// compile-time constant slice, so this map can never grow past
// len(cronLatencyBucketsMs)+1 keys regardless of input.

package metrics

import (
	"expvar"
	"strconv"
)

// cronLatencyBucketsMs are the cumulative upper bounds (in milliseconds)
// for the cron execution-latency histogram. Chosen to straddle the
// defaultCronSlowThreshold (30s = 30000ms) used by the existing slow
// counter so the histogram and that counter tell a consistent story:
// fast jobs cluster in the sub-second / few-second buckets, and anything
// past 30s lands in the same tail the slow counter already alerts on.
var cronLatencyBucketsMs = []int64{
	100, 500, 1000, 5000, 15000, 30000, 60000, 120000, 300000,
}

// cronLatencyBucketKeys is the precomputed string key for each bucket
// bound plus the terminal "+Inf" bucket, built once at init so Observe
// allocates nothing on the hot path. Index i (0..len(buckets)-1) maps to
// the bound at cronLatencyBucketsMs[i]; the final index is "+Inf".
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
	// success-path execution latency. Keys are the upper bound in ms
	// (and "+Inf"); each value is the count of observations <= that
	// bound. Naming deliberately omits the `_total` suffix so the
	// docs/ops/pprof.md doc-sync contract (which scans only `naozhi_*_total`
	// counter names) does not require a per-bucket row — the histogram is
	// documented as a single unit in the runbook prose instead.
	CronExecutionDurationBucket = expvar.NewMap("naozhi_cron_execution_duration_ms_bucket")

	// CronExecutionDurationSum accumulates the total observed milliseconds.
	// Pair with the "+Inf" bucket (= total count) to compute a mean. No
	// `_total` suffix for the same doc-sync reason as the bucket map.
	CronExecutionDurationSum = expvar.NewInt("naozhi_cron_execution_duration_ms_sum")
)

// ObserveCronExecutionDuration records a single cron success-path
// execution latency into the histogram. Negative inputs (clock skew /
// monotonic anomalies) are clamped to 0 so a bogus sample cannot corrupt
// the running sum or skip every bucket. Safe for concurrent use:
// expvar.Map.Add and expvar.Int.Add are each atomic.
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
	// Every observation falls in the terminal +Inf bucket, which therefore
	// tracks the total observation count.
	CronExecutionDurationBucket.Add(cronLatencyBucketKeys[len(cronLatencyBucketsMs)], 1)
}
