package runstore

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// goldenCronRun sets every CronRun field to a value that survives a JSON
// round trip.
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
		CostUSD:     0.5,
	}
}

// Run files on disk outlive any build: the dashboard reads records an older
// binary wrote. These goldens were captured
// from the record types before they moved into this package, so a tag, field
// order or encoder change that alters the bytes fails here.
const (
	goldenRecordJSON  = `{"run_id":"0123456789abcdef","job_id":"fedcba9876543210","state":"failed","trigger":"manual","started_at":"2026-10-06T12:00:00.123Z","ended_at":"2026-10-06T12:00:01.623Z","duration_ms":1500,"session_id":"sess-\u003c1\u003e\u0026","prompt":"say \"hi\" \u003cb\u003e","work_dir":"/srv/wd","fresh":true,"result":"partial…[truncated]","result_bytes":22,"error_class":"send_error","error_msg":"send: timeout","cost_usd":0.5}`
	goldenSummaryJSON = `{"run_id":"0123456789abcdef","job_id":"fedcba9876543210","state":"failed","trigger":"manual","started_at":"2026-10-06T12:00:00.123Z","ended_at":"2026-10-06T12:00:01.623Z","duration_ms":1500,"session_id":"sess-\u003c1\u003e\u0026","error_class":"send_error","cost_usd":0.5}`
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

// TestCronRun_DecodesRemovedSandboxKeys: records written while the cloud
// sandbox existed carry replay_of and sandbox_meta. Both keys are ignored on
// read, so those records still load as the run they describe.
func TestCronRun_DecodesRemovedSandboxKeys(t *testing.T) {
	t.Parallel()
	const legacy = `{"run_id":"0123456789abcdef","job_id":"fedcba9876543210","state":"failed","trigger":"manual","started_at":"2026-10-06T12:00:00.123Z","ended_at":"2026-10-06T12:00:01.623Z","duration_ms":1500,"session_id":"sess-\u003c1\u003e\u0026","prompt":"say \"hi\" \u003cb\u003e","work_dir":"/srv/wd","fresh":true,"result":"partial…[truncated]","result_bytes":22,"error_class":"send_error","error_msg":"send: timeout","replay_of":"aaaaaaaaaaaaaaaa","sandbox_meta":{"runtime_arn":"arn:aws:bedrock-agentcore:us-east-1:1:runtime/x","image_version":"v7","exit_status":0,"cost_usd":0.25,"duration_ms":1400,"memory_peak_bytes":1048576,"models":[{"model":"opus","raw_model":"claude-opus","provider":"bedrock","basis":"list","cost_usd":0.25,"input":10,"output":20,"cache_read":30,"cache_write":40}],"basis":"list"},"cost_usd":0.5}`
	back, err := decodeRunBytes([]byte(legacy), MaxRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, goldenCronRun()) {
		t.Errorf("legacy record decodes to %+v, want %+v", back, goldenCronRun())
	}
}
