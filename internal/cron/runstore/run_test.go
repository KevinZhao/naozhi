package runstore

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// TestSandbox_MetaExcludedFromSummary pins the list-endpoint payload guard:
// recent_runs loads 50 jobs × 5 summaries — the full receipt would bloat
// it. The summary() projection must drop the nested SandboxMeta and every
// heavy field; it carries ONLY the single cost_usd float (the §7.5 data
// source — per-run小字 + monthly aggregate), not runtime_arn/image/etc.
func TestSandbox_MetaExcludedFromSummary(t *testing.T) {
	r := &CronRun{
		RunID: "a", JobID: "b", State: runtelemetry.RunStateSucceeded,
		SandboxMeta: &SandboxRunMeta{
			CostUSD: 1.23, ImageVersion: "phase2",
			RuntimeARN: "arn:x", MemoryPeakBytes: 1 << 20,
		},
	}
	data, err := json.Marshal(r.summary())
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	got := string(data)
	// The nested receipt and its heavy fields must NOT be in the summary.
	for _, forbidden := range []string{"sandbox_meta", "phase2", "image_version", "runtime_arn", "memory_peak"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("CronRunSummary leaked heavy meta field %q: %s", forbidden, got)
		}
	}
	// cost_usd IS expected (the §7.5 lightweight cost source).
	if !strings.Contains(got, `"cost_usd":1.23`) {
		t.Fatalf("CronRunSummary must carry cost_usd for §7.5: %s", got)
	}
}

// TestSandboxRunMeta_WireTags freezes the JSON tags — the dashboard run
// detail (§7.3) keys off these literals, so a rename must fail here first.
func TestSandboxRunMeta_WireTags(t *testing.T) {
	m := SandboxRunMeta{
		RuntimeARN:      "arn",
		ImageVersion:    "v1",
		ExitStatus:      2,
		CostUSD:         0.5,
		DurationMS:      10,
		MemoryPeakBytes: 99,
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"runtime_arn":`, `"image_version":`, `"exit_status":`,
		`"cost_usd":`, `"duration_ms":`, `"memory_peak_bytes":`,
	} {
		if !strings.Contains(string(data), key) {
			t.Errorf("SandboxRunMeta JSON missing wire key %s; got %s", key, data)
		}
	}
}

// TestCronRun_ReplayOfWireTagAndSummary pins replay_of on both CronRun and
// the summary projection (the list view renders the chain badge too).
func TestCronRun_ReplayOfWireTagAndSummary(t *testing.T) {
	r := &CronRun{RunID: "b", JobID: "j", State: runtelemetry.RunStateSucceeded, ReplayOf: "a"}

	data, _ := json.Marshal(r)
	if !strings.Contains(string(data), `"replay_of":"a"`) {
		t.Fatalf("CronRun must carry replay_of: %s", data)
	}
	// Original runs (empty ReplayOf) omit the key.
	data, _ = json.Marshal(&CronRun{RunID: "a", JobID: "j", State: runtelemetry.RunStateSucceeded})
	if strings.Contains(string(data), "replay_of") {
		t.Fatalf("original run must omit replay_of: %s", data)
	}
	// Summary carries it (UI list draws the chain off the summary).
	sdata, _ := json.Marshal(r.summary())
	if !strings.Contains(string(sdata), `"replay_of":"a"`) {
		t.Fatalf("CronRunSummary must carry replay_of: %s", sdata)
	}
}

// goldenCronRun sets every CronRun field, nested receipt included, to a value
// that survives a JSON round trip.
func goldenCronRun() *CronRun {
	start := time.Date(2026, 10, 6, 12, 0, 0, 123000000, time.UTC)
	return &CronRun{
		RunID:       "0123456789abcdef",
		JobID:       "fedcba9876543210",
		State:       runtelemetry.RunStateFailed,
		Trigger:     runtelemetry.TriggerManual,
		StartedAt:   start,
		EndedAt:     start.Add(1500 * time.Millisecond),
		DurationMS:  1500,
		SessionID:   "sess-<1>&",
		Prompt:      "say \"hi\" <b>",
		WorkDir:     "/srv/wd",
		Fresh:       true,
		Result:      "partial…[truncated]",
		ResultBytes: 22,
		ErrorClass:  runtelemetry.ErrClassCronSendError,
		ErrorMsg:    "send: timeout",
		ReplayOf:    "aaaaaaaaaaaaaaaa",
		SandboxMeta: &SandboxRunMeta{
			RuntimeARN:      "arn:aws:bedrock-agentcore:us-east-1:1:runtime/x",
			ImageVersion:    "v7",
			ExitStatus:      0,
			CostUSD:         0.25,
			DurationMS:      1400,
			MemoryPeakBytes: 1 << 20,
			Models: []costledger.ModelDelta{{
				Model: "opus", RawModel: "claude-opus", Provider: "bedrock", Basis: costledger.BasisList,
				CostUSD: 0.25, Tokens: costledger.Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40},
			}},
			Basis: costledger.BasisList,
		},
		CostUSD: 0.5,
	}
}

// Run files on disk outlive any build: cmd/naozhi's cost reconcile and the
// dashboard read records an older binary wrote. These goldens were captured
// from the record types before they moved into this package, so a tag, field
// order or encoder change that alters the bytes fails here.
const (
	goldenRecordJSON  = `{"run_id":"0123456789abcdef","job_id":"fedcba9876543210","state":"failed","trigger":"manual","started_at":"2026-10-06T12:00:00.123Z","ended_at":"2026-10-06T12:00:01.623Z","duration_ms":1500,"session_id":"sess-\u003c1\u003e\u0026","prompt":"say \"hi\" \u003cb\u003e","work_dir":"/srv/wd","fresh":true,"result":"partial…[truncated]","result_bytes":22,"error_class":"send_error","error_msg":"send: timeout","replay_of":"aaaaaaaaaaaaaaaa","sandbox_meta":{"runtime_arn":"arn:aws:bedrock-agentcore:us-east-1:1:runtime/x","image_version":"v7","exit_status":0,"cost_usd":0.25,"duration_ms":1400,"memory_peak_bytes":1048576,"models":[{"model":"opus","raw_model":"claude-opus","provider":"bedrock","basis":"list","cost_usd":0.25,"input":10,"output":20,"cache_read":30,"cache_write":40}],"basis":"list"},"cost_usd":0.5}`
	goldenSummaryJSON = `{"run_id":"0123456789abcdef","job_id":"fedcba9876543210","state":"failed","trigger":"manual","started_at":"2026-10-06T12:00:00.123Z","ended_at":"2026-10-06T12:00:01.623Z","duration_ms":1500,"session_id":"sess-\u003c1\u003e\u0026","error_class":"send_error","replay_of":"aaaaaaaaaaaaaaaa","cost_usd":0.25}`
)

func TestCronRun_WireFormatGolden(t *testing.T) {
	t.Parallel()
	got, err := marshalRunPooled(goldenCronRun())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenRecordJSON {
		t.Errorf("record bytes drifted:\n got %s\nwant %s", got, goldenRecordJSON)
	}
	back, err := decodeRunBytes([]byte(goldenRecordJSON), MaxRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, goldenCronRun()) {
		t.Errorf("golden record decodes to %+v, want %+v", back, goldenCronRun())
	}
	sum, err := json.Marshal(goldenCronRun().summary())
	if err != nil {
		t.Fatal(err)
	}
	if string(sum) != goldenSummaryJSON {
		t.Errorf("summary bytes drifted:\n got %s\nwant %s", sum, goldenSummaryJSON)
	}
}
