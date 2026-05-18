package metrics

import (
	"encoding/json"
	"expvar"
	"strings"
	"sync"
	"testing"
)

// TestCLISpawnTotal_RecordsLabeledAndLegacy pins the central double-write
// invariant: a single RecordCLISpawn call MUST bump both the legacy
// unlabeled counter and the per-backend vector by the same amount. A
// regression that drops one half (e.g. a refactor that "centralizes" the
// labeled side and forgets the legacy mirror) silently breaks the dashboards
// and alert rules built on the legacy name during the 4-week migration.
func TestCLISpawnTotal_RecordsLabeledAndLegacy(t *testing.T) {
	// Capture starting values — the package-level counters are process-wide
	// singletons so other tests in the binary may already have moved them.
	startLegacy := CLISpawnTotal.Value()
	startLabeled := CLISpawnTotalByBackend.Get("claude")
	startSum := CLISpawnTotalByBackend.Sum()

	RecordCLISpawn("claude")

	if got := CLISpawnTotal.Value() - startLegacy; got != 1 {
		t.Errorf("legacy CLISpawnTotal delta = %d, want 1", got)
	}
	if got := CLISpawnTotalByBackend.Get("claude") - startLabeled; got != 1 {
		t.Errorf("labeled[claude] delta = %d, want 1", got)
	}
	if got := CLISpawnTotalByBackend.Sum() - startSum; got != 1 {
		t.Errorf("Sum delta = %d, want 1", got)
	}
}

// TestRecordCLISpawn_TableDriven covers backend ∈ {claude, kiro, ""} so
// the LabelEmpty fallback branch is exercised. The legacy counter increments
// on every call regardless of backend value — that's the migration property
// dashboards depend on.
func TestRecordCLISpawn_TableDriven(t *testing.T) {
	cases := []struct {
		name     string
		backend  string
		labelKey string
	}{
		{"claude", "claude", "claude"},
		{"kiro", "kiro", "kiro"},
		{"empty (legacy)", "", LabelEmpty},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			startLegacy := CLISpawnTotal.Value()
			startLabel := CLISpawnTotalByBackend.Get(tc.labelKey)

			RecordCLISpawn(tc.backend)

			if got := CLISpawnTotal.Value() - startLegacy; got != 1 {
				t.Errorf("legacy delta = %d, want 1", got)
			}
			if got := CLISpawnTotalByBackend.Get(tc.labelKey) - startLabel; got != 1 {
				t.Errorf("labeled[%s] delta = %d, want 1", tc.labelKey, got)
			}
		})
	}
}

// TestProtocolRPCErrorTotal_LabelsRoundTrip confirms that distinct
// (backend, method, code) tuples land in distinct map entries — i.e. they
// don't collide via labelKey collisions. Three orthogonal tuples sharing
// any one dimension exercise the join logic on every position.
func TestProtocolRPCErrorTotal_LabelsRoundTrip(t *testing.T) {
	cases := []struct {
		backend string
		method  string
		code    string
	}{
		{"kiro", "session/prompt", "-32601"},
		{"kiro", "session/prompt", "-32602"},   // same backend+method, diff code
		{"kiro", "initialize", "-32601"},       // same backend+code, diff method
		{"gemini", "session/prompt", "-32601"}, // same method+code, diff backend
	}
	starts := make([]int64, len(cases))
	for i, c := range cases {
		starts[i] = ProtocolRPCErrorTotalByBackend.Get(c.backend, c.method, c.code)
	}
	for _, c := range cases {
		RecordProtocolRPCError(c.backend, c.method, c.code)
	}
	for i, c := range cases {
		got := ProtocolRPCErrorTotalByBackend.Get(c.backend, c.method, c.code) - starts[i]
		if got != 1 {
			t.Errorf("tuple (%q,%q,%q) delta = %d, want 1", c.backend, c.method, c.code, got)
		}
	}
}

// TestACPCancelTotal_BackendLabel pins the per-backend cancel counter.
// Even though only kiro emits cancels today, the metric is multi-backend
// from day one so a future Gemini ACP backend would naturally share the
// metric without a schema break.
func TestACPCancelTotal_BackendLabel(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		key     string
	}{
		{"kiro", "kiro", "kiro"},
		{"future-gemini", "gemini", "gemini"},
		{"empty", "", LabelEmpty},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			start := ACPCancelTotalByBackend.Get(tc.key)
			RecordACPCancel(tc.backend)
			if got := ACPCancelTotalByBackend.Get(tc.key) - start; got != 1 {
				t.Errorf("delta = %d, want 1", got)
			}
		})
	}
}

// TestRecordSessionActive_GaugeBookkeeping verifies +1/-1 deltas track
// correctly per backend AND aggregate into the unlabeled mirror.
func TestRecordSessionActive_GaugeBookkeeping(t *testing.T) {
	startMirror := SessionActive.Value()
	startClaude := SessionActiveByBackend.Get("claude")
	startKiro := SessionActiveByBackend.Get("kiro")

	RecordSessionActive("claude", 1)
	RecordSessionActive("claude", 1)
	RecordSessionActive("kiro", 1)
	RecordSessionActive("claude", -1)

	if got := SessionActive.Value() - startMirror; got != 2 {
		t.Errorf("mirror delta = %d, want 2 (3 inc - 1 dec)", got)
	}
	if got := SessionActiveByBackend.Get("claude") - startClaude; got != 1 {
		t.Errorf("labeled[claude] delta = %d, want 1", got)
	}
	if got := SessionActiveByBackend.Get("kiro") - startKiro; got != 1 {
		t.Errorf("labeled[kiro] delta = %d, want 1", got)
	}
}

// TestRecordSessionActive_ZeroDeltaNoOp pins the early-return so a
// 0-delta call cannot accidentally spam the mirror with no-ops. Cheap
// invariant; protects future refactors that might inline the helper.
func TestRecordSessionActive_ZeroDeltaNoOp(t *testing.T) {
	startMirror := SessionActive.Value()
	startClaude := SessionActiveByBackend.Get("claude")
	RecordSessionActive("claude", 0)
	if got := SessionActive.Value() - startMirror; got != 0 {
		t.Errorf("mirror moved on 0-delta: %d", got)
	}
	if got := SessionActiveByBackend.Get("claude") - startClaude; got != 0 {
		t.Errorf("labeled moved on 0-delta: %d", got)
	}
}

// TestLabelKey_OverlongCollapsesToOverflow ensures that an attacker-controlled
// agent that emits a 10KB method name doesn't blow up cardinality. The bucket
// converges into LabelOverflow and overflowCount tallies the event so an
// operator alert can fire.
func TestLabelKey_OverlongCollapsesToOverflow(t *testing.T) {
	// Build a tuple whose joined length exceeds maxLabelKeyLen even after
	// each segment is clipped to maxLabelSegmentLen. With 5 segments of
	// 64 bytes + 4 separators = 324 > 256.
	long := strings.Repeat("a", 80) // > maxLabelSegmentLen, clipped to 64
	startOverflow := overflowCount.Value()
	startBucket := ProtocolRPCErrorTotalByBackend.Get(LabelOverflow)
	// Use 5 long segments to definitely exceed even after per-segment clip.
	// RecordProtocolRPCError takes 3, so call the underlying counter directly
	// with a 5-segment tuple to drive the maxLabelKeyLen path.
	ProtocolRPCErrorTotalByBackend.Add(1, long, long, long, long, long)
	if got := overflowCount.Value() - startOverflow; got != 1 {
		t.Errorf("overflowCount delta = %d, want 1", got)
	}
	if got := ProtocolRPCErrorTotalByBackend.Get(LabelOverflow) - startBucket; got != 1 {
		t.Errorf("overflow bucket delta = %d, want 1", got)
	}
}

// TestLabelKey_EmptyCollapsesToEmpty confirms the LabelEmpty sentinel —
// distinct from LabelOverflow so dashboards can tell "missing data" from
// "label too long".
func TestLabelKey_EmptyCollapsesToEmpty(t *testing.T) {
	if got := labelKey([]string{}); got != LabelEmpty {
		t.Errorf("zero-length labels = %q, want %q", got, LabelEmpty)
	}
	if got := labelKey([]string{""}); got != LabelEmpty {
		t.Errorf("single-empty labels = %q, want %q", got, LabelEmpty)
	}
	if got := labelKey([]string{"a", "", "c"}); got != "a|"+LabelEmpty+"|c" {
		t.Errorf("mixed empty middle = %q", got)
	}
}

// TestLabeledCounter_JSONShape ensures the expvar.Map encoding stays
// scrape-friendly: the top-level value must be a JSON object whose values
// are JSON numbers, mirroring the legacy expvar.Int contract.
func TestLabeledCounter_JSONShape(t *testing.T) {
	counter := NewLabeledCounter("test_metric_jsonshape_" + t.Name())
	counter.Add(1, "claude")
	counter.Add(2, "kiro")

	v := expvar.Get("test_metric_jsonshape_" + t.Name())
	if v == nil {
		t.Fatal("metric not registered with expvar")
	}
	raw := v.String()
	var obj map[string]json.Number
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("JSON shape not map[string]number: %v\nraw=%s", err, raw)
	}
	if obj["claude"].String() != "1" {
		t.Errorf("claude=%v, want 1", obj["claude"])
	}
	if obj["kiro"].String() != "2" {
		t.Errorf("kiro=%v, want 2", obj["kiro"])
	}
}

// TestLabeledCounter_ConcurrentAdd exercises the underlying expvar.Map's
// thread safety. labelKey itself uses a sync.Pool of strings.Builder, so a
// race here would surface either as a wrong total or as -race detector
// fire. Two goroutines × 1000 iterations is enough to flush the pool.
func TestLabeledCounter_ConcurrentAdd(t *testing.T) {
	counter := NewLabeledCounter("test_metric_concurrent_" + t.Name())
	const N = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			counter.Add(1, "claude")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			counter.Add(1, "kiro")
		}
	}()
	wg.Wait()
	if got := counter.Get("claude"); got != N {
		t.Errorf("claude = %d, want %d", got, N)
	}
	if got := counter.Get("kiro"); got != N {
		t.Errorf("kiro = %d, want %d", got, N)
	}
	if got := counter.Sum(); got != 2*N {
		t.Errorf("Sum = %d, want %d", got, 2*N)
	}
}

// TestLabeledGauge_Add pins the single-call atomic delta path used by
// router reconciliation. Verifies positive / negative / zero deltas all
// land in one expvar map operation rather than N Inc/Dec iterations,
// which was the racy pattern flagged in PR #113 review.
func TestLabeledGauge_Add(t *testing.T) {
	gauge := NewLabeledGauge("test_gauge_add_" + t.Name())
	gauge.Add(5, "claude")
	if got := gauge.Get("claude"); got != 5 {
		t.Errorf("Add(5)=%d, want 5", got)
	}
	gauge.Add(-3, "claude")
	if got := gauge.Get("claude"); got != 2 {
		t.Errorf("Add(-3)=%d, want 2", got)
	}
	gauge.Add(0, "claude") // no-op fast path
	if got := gauge.Get("claude"); got != 2 {
		t.Errorf("Add(0)=%d, want 2 (unchanged)", got)
	}
	// Negative going below zero is allowed (matches Dec contract).
	gauge.Add(-10, "claude")
	if got := gauge.Get("claude"); got != -8 {
		t.Errorf("Add(-10)=%d, want -8 (allowed)", got)
	}
}

// TestLabeledGauge_SumAndForEachKey covers the read-side helpers used by
// the session router's reconciliation path. Sum() is the cluster-wide
// rollup; ForEachKey enumerates buckets so reconcile can drive a vanished
// backend's value to zero (without it, the bucket would linger at its
// last non-zero value indefinitely).
func TestLabeledGauge_SumAndForEachKey(t *testing.T) {
	gauge := NewLabeledGauge("test_gauge_sum_foreach_" + t.Name())
	gauge.Inc("claude")
	gauge.Inc("claude")
	gauge.Inc("kiro")
	if got := gauge.Sum(); got != 3 {
		t.Errorf("Sum = %d, want 3", got)
	}
	seen := map[string]bool{}
	gauge.ForEachKey(func(k string) { seen[k] = true })
	if !seen["claude"] || !seen["kiro"] {
		t.Errorf("ForEachKey missed keys: %v", seen)
	}
}

// TestLabeledCounter_ForEachKey covers the symmetric helper on the
// counter side; rarely needed at runtime but exercised in tests.
func TestLabeledCounter_ForEachKey(t *testing.T) {
	counter := NewLabeledCounter("test_counter_foreach_" + t.Name())
	counter.Add(1, "claude")
	counter.Add(1, "kiro")
	seen := map[string]bool{}
	counter.ForEachKey(func(k string) { seen[k] = true })
	if !seen["claude"] || !seen["kiro"] {
		t.Errorf("ForEachKey missed keys: %v", seen)
	}
}

// TestRegisteredMetricNames pins the canonical expvar names introduced
// by Sprint 6a — the contract docs/ops/pprof.md operators rely on.
// Renaming any of these is an externally visible change.
func TestRegisteredMetricNames(t *testing.T) {
	want := []string{
		"naozhi_cli_spawn_total_by_backend",
		"naozhi_session_active",
		"naozhi_session_active_by_backend",
		"naozhi_protocol_rpc_error_total",
		"naozhi_acp_cancel_total",
		"naozhi_metrics_label_overflow_total",
	}
	for _, name := range want {
		name := name
		t.Run(name, func(t *testing.T) {
			if v := expvar.Get(name); v == nil {
				t.Fatalf("metric %q not registered", name)
			}
		})
	}
}
