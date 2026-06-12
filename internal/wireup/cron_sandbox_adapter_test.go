package wireup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/naozhi/naozhi/internal/agentcore"
	"github.com/naozhi/naozhi/internal/cron"
)

// fakeAgentcoreAPI lets the adapter tests drive agentcore.Client through
// its exported test seam (NewWithAPI is unexported; we go through the SSE
// body instead — the adapter only sees agentcore.Client's public surface).
type fakeAgentcoreAPI struct {
	body    string
	stopErr error
	stopped []string
}

func (f *fakeAgentcoreAPI) InvokeAgentRuntime(_ context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	return &bedrockagentcore.InvokeAgentRuntimeOutput{
		ContentType:      aws.String("text/event-stream"),
		Response:         io.NopCloser(strings.NewReader(f.body)),
		RuntimeSessionId: params.RuntimeSessionId,
		StatusCode:       aws.Int32(200),
	}, nil
}

func (f *fakeAgentcoreAPI) StopRuntimeSession(_ context.Context, params *bedrockagentcore.StopRuntimeSessionInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.StopRuntimeSessionOutput, error) {
	f.stopped = append(f.stopped, aws.ToString(params.RuntimeSessionId))
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return &bedrockagentcore.StopRuntimeSessionOutput{}, nil
}

func sseBody(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return b.String()
}

func adapterJob() cron.SandboxJob {
	// cron-shaped 16-hex run id: the adapter must derive an API-compliant
	// runtime session id from it (≥33 chars, validation F3).
	return cron.SandboxJob{JobID: "j1", RunID: "0123456789abcdef", Prompt: "hi"}
}

func TestAdapter_SuccessExtractsResultText(t *testing.T) {
	api := &fakeAgentcoreAPI{body: sseBody(
		`{"kind":"cli","line":{"type":"system","subtype":"init"},"ts":"t"}`,
		`{"kind":"cli","line":{"type":"result","is_error":false,"result":"最终答案"},"ts":"t"}`,
		`{"kind":"exit","code":0,"ts":"t"}`,
	)}
	r := &agentcoreSandboxRunner{client: agentcore.NewWithAPIForTest(api,
		agentcore.Config{RuntimeARN: "arn:x", Region: "us-west-2"})}

	var lines [][]byte
	out, err := r.RunJob(context.Background(), adapterJob(), func(l []byte) error {
		cp := make([]byte, len(l))
		copy(cp, l)
		lines = append(lines, cp)
		return nil
	})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if out.State != cron.SandboxStateSuccess {
		t.Fatalf("state = %q, want success", out.State)
	}
	if out.ResultText != "最终答案" {
		t.Fatalf("result text = %q", out.ResultText)
	}
	if len(lines) != 3 {
		t.Fatalf("sink saw %d lines, want 3", len(lines))
	}
	if len(api.stopped) != 0 {
		t.Fatal("success must not call Stop")
	}
}

func TestAdapter_TransportTriggersStopConfirm(t *testing.T) {
	// Clean EOF with no terminal attestation = failed-transport (idle-burn
	// shape); adapter must attempt Stop and report confirmation.
	api := &fakeAgentcoreAPI{body: sseBody(
		`{"kind":"cli","line":{"type":"system"},"ts":"t"}`,
	)}
	r := &agentcoreSandboxRunner{client: agentcore.NewWithAPIForTest(api,
		agentcore.Config{RuntimeARN: "arn:x", Region: "us-west-2"})}

	job := adapterJob()
	out, err := r.RunJob(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if out.State != cron.SandboxStateFailedTransport {
		t.Fatalf("state = %q, want failed-transport", out.State)
	}
	if !out.StopConfirmed {
		t.Fatal("Stop succeeded; StopConfirmed must be true")
	}
	// The adapter derives the runtime session id ("run-<cronRunID>-<nano>")
	// to satisfy the ≥33-char API minimum while embedding the cron run id
	// for correlation; Stop must target that derived id.
	if len(api.stopped) != 1 || !strings.HasPrefix(api.stopped[0], "run-"+job.RunID+"-") {
		t.Fatalf("stop calls = %v, want one with prefix run-%s-", api.stopped, job.RunID)
	}
}

func TestAdapter_TransportStopFailureLeavesUnconfirmed(t *testing.T) {
	api := &fakeAgentcoreAPI{
		body:    sseBody(`{"kind":"cli","line":{"type":"system"},"ts":"t"}`),
		stopErr: errors.New("denied"),
	}
	r := &agentcoreSandboxRunner{client: agentcore.NewWithAPIForTest(api,
		agentcore.Config{RuntimeARN: "arn:x", Region: "us-west-2"})}

	out, err := r.RunJob(context.Background(), adapterJob(), nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if out.StopConfirmed {
		t.Fatal("Stop failed; StopConfirmed must be false (§6.2: fate unknown)")
	}
}

func TestNewAgentcoreSandboxRunner_DisabledConfig(t *testing.T) {
	r, err := newAgentcoreSandboxRunner(context.Background(), "", "")
	if err != nil || r != nil {
		t.Fatalf("disabled config: r=%v err=%v, want nil/nil", r, err)
	}
}

// TestAdapter_MapsMetaFromRunResult pins PR-1: the adapter must thread the
// agentcore execution receipt (cost/duration/image/memory/exit) plus the
// configured runtime ARN into cron.SandboxOutcome.Meta.
func TestAdapter_MapsMetaFromRunResult(t *testing.T) {
	api := &fakeAgentcoreAPI{body: sseBody(
		`{"kind":"cli","line":{"type":"result","is_error":false,"result":"ok","total_cost_usd":0.0044,"duration_ms":1888},"ts":"t"}`,
		`{"kind":"meta","image_version":"phase2","memory_peak_bytes":268435456,"ts":"t"}`,
		`{"kind":"exit","code":0,"ts":"t"}`,
	)}
	r := &agentcoreSandboxRunner{client: agentcore.NewWithAPIForTest(api,
		agentcore.Config{RuntimeARN: "arn:aws:bedrock-agentcore:us-west-2:1:runtime/x", Region: "us-west-2"})}

	out, err := r.RunJob(context.Background(), adapterJob(), nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if out.State != cron.SandboxStateSuccess {
		t.Fatalf("state = %q, want success", out.State)
	}
	want := cron.SandboxRunMeta{
		RuntimeARN:      "arn:aws:bedrock-agentcore:us-west-2:1:runtime/x",
		ImageVersion:    "phase2",
		ExitStatus:      0,
		CostUSD:         0.0044,
		DurationMS:      1888,
		MemoryPeakBytes: 268435456,
	}
	if out.Meta != want {
		t.Fatalf("Meta = %+v, want %+v", out.Meta, want)
	}
}
