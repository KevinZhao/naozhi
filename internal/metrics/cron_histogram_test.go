package metrics

import (
	"expvar"
	"testing"
)

// bucketCount reads the current count for a cumulative bucket key.
func bucketCount(t *testing.T, key string) int64 {
	t.Helper()
	v := CronExecutionDurationBucket.Get(key)
	if v == nil {
		return 0
	}
	iv, ok := v.(*expvar.Int)
	if !ok {
		t.Fatalf("bucket %q is %T, want *expvar.Int", key, v)
	}
	return iv.Value()
}

// TestObserveCronExecutionDuration_CumulativeBuckets verifies the OBS1
// (#392) histogram is cumulative: an observation increments every bucket
// whose bound it falls under, the +Inf bucket tracks the total count, and
// the sum accumulates observed milliseconds. The histogram is a process-
// global singleton, so this test reads deltas rather than absolute values
// to stay robust if other tests in the package also observe.
func TestObserveCronExecutionDuration_CumulativeBuckets(t *testing.T) {
	// Snapshot the buckets we assert on.
	before100 := bucketCount(t, "100")
	before500 := bucketCount(t, "500")
	before30000 := bucketCount(t, "30000")
	beforeInf := bucketCount(t, "+Inf")
	beforeSum := CronExecutionDurationSum.Value()

	// A 250ms observation: <=500, <=1000, ... <=+Inf but NOT <=100.
	ObserveCronExecutionDuration(250)

	if got := bucketCount(t, "100") - before100; got != 0 {
		t.Errorf("250ms must NOT fall in the <=100 bucket; delta=%d", got)
	}
	if got := bucketCount(t, "500") - before500; got != 1 {
		t.Errorf("250ms must fall in the <=500 bucket; delta=%d want 1", got)
	}
	if got := bucketCount(t, "30000") - before30000; got != 1 {
		t.Errorf("250ms must fall in the <=30000 bucket (cumulative); delta=%d want 1", got)
	}
	if got := bucketCount(t, "+Inf") - beforeInf; got != 1 {
		t.Errorf("+Inf bucket must count every observation; delta=%d want 1", got)
	}
	if got := CronExecutionDurationSum.Value() - beforeSum; got != 250 {
		t.Errorf("sum delta=%d want 250", got)
	}
}

// TestObserveCronExecutionDuration_TailAndClamp verifies a past-300s
// observation only lands in +Inf (none of the finite buckets) and that a
// negative input is clamped to 0 (lands in every bucket, adds 0 to sum).
func TestObserveCronExecutionDuration_TailAndClamp(t *testing.T) {
	beforeInf := bucketCount(t, "+Inf")
	before300k := bucketCount(t, "300000")
	beforeSum := CronExecutionDurationSum.Value()

	ObserveCronExecutionDuration(500000) // > 300000ms, tail only
	if got := bucketCount(t, "300000") - before300k; got != 0 {
		t.Errorf("500000ms must NOT fall in the <=300000 bucket; delta=%d", got)
	}
	if got := bucketCount(t, "+Inf") - beforeInf; got != 1 {
		t.Errorf("tail observation must still hit +Inf; delta=%d want 1", got)
	}
	if got := CronExecutionDurationSum.Value() - beforeSum; got != 500000 {
		t.Errorf("sum delta=%d want 500000", got)
	}

	// Negative clamp: lands in the smallest bucket, adds 0 to the sum.
	before100 := bucketCount(t, "100")
	sumPre := CronExecutionDurationSum.Value()
	ObserveCronExecutionDuration(-5)
	if got := bucketCount(t, "100") - before100; got != 1 {
		t.Errorf("clamped-to-0 observation must land in the <=100 bucket; delta=%d want 1", got)
	}
	if got := CronExecutionDurationSum.Value() - sumPre; got != 0 {
		t.Errorf("negative input must add 0 to sum; delta=%d want 0", got)
	}
}

// TestCronLatencyBucketKeys_MatchBounds is a structural guard: the
// precomputed key slice must align 1:1 with the bound slice plus a final
// "+Inf" sentinel, or Observe would index the wrong key.
func TestCronLatencyBucketKeys_MatchBounds(t *testing.T) {
	if len(cronLatencyBucketKeys) != len(cronLatencyBucketsMs)+1 {
		t.Fatalf("key slice len=%d want bounds+1=%d", len(cronLatencyBucketKeys), len(cronLatencyBucketsMs)+1)
	}
	if cronLatencyBucketKeys[len(cronLatencyBucketKeys)-1] != "+Inf" {
		t.Errorf("terminal key=%q want +Inf", cronLatencyBucketKeys[len(cronLatencyBucketKeys)-1])
	}
}
