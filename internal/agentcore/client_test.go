package agentcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

// fakeAPI implements runtimeAPI with a canned response body.
type fakeAPI struct {
	invokeBody   io.ReadCloser
	invokeErr    error
	gotInvoke    *bedrockagentcore.InvokeAgentRuntimeInput
	stopErr      error
	gotStopRunID string
}

func (f *fakeAPI) InvokeAgentRuntime(_ context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	f.gotInvoke = params
	if f.invokeErr != nil {
		return nil, f.invokeErr
	}
	return &bedrockagentcore.InvokeAgentRuntimeOutput{
		ContentType:      aws.String("text/event-stream"),
		Response:         f.invokeBody,
		RuntimeSessionId: params.RuntimeSessionId,
		StatusCode:       aws.Int32(200),
	}, nil
}

func (f *fakeAPI) StopRuntimeSession(_ context.Context, params *bedrockagentcore.StopRuntimeSessionInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.StopRuntimeSessionOutput, error) {
	f.gotStopRunID = aws.ToString(params.RuntimeSessionId)
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return &bedrockagentcore.StopRuntimeSessionOutput{}, nil
}

// sse builds a bootstrap-shaped SSE body from envelope JSON fragments.
func sse(frames ...string) io.ReadCloser {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return io.NopCloser(strings.NewReader(b.String()))
}

func testRunID() string { return NewRunID(time.Now()) }

func newTestClient(api runtimeAPI) *Client {
	return newWithAPI(api, Config{
		RuntimeARN: "arn:aws:bedrock-agentcore:us-west-2:111122223333:runtime/test-x",
		Region:     "us-west-2",
	})
}

func TestRun_SuccessStream(t *testing.T) {
	api := &fakeAPI{invokeBody: sse(
		`{"kind":"boot","msg":"materialized in 1ms","ts":"t"}`,
		`{"kind":"cli","line":{"type":"system","subtype":"init"},"ts":"t"}`,
		`{"kind":"keepalive","ts":"t"}`,
		`{"kind":"cli","line":{"type":"result","is_error":false},"ts":"t"}`,
		`{"kind":"exit","code":0,"msg":"cli-exited","ts":"t"}`,
	)}
	c := newTestClient(api)

	var got []EnvelopeKind
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "hi"},
		func(env *Envelope) error {
			got = append(got, env.Kind)
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != Success {
		t.Fatalf("state = %q, want success", res.State)
	}
	// Keepalives must be filtered before the sink (F6 liveness frames are
	// transport noise, not job events).
	for _, k := range got {
		if k == KindKeepalive {
			t.Fatal("keepalive leaked into sink")
		}
	}
	if len(got) != 4 {
		t.Fatalf("sink saw %d envelopes, want 4 (boot+cli+cli+exit)", len(got))
	}
	// Wire contract: accept/content-type headers select the SSE path.
	if aws.ToString(api.gotInvoke.Accept) != "text/event-stream" {
		t.Fatalf("Accept = %q", aws.ToString(api.gotInvoke.Accept))
	}
}

func TestRun_TransportBreakMidStream(t *testing.T) {
	// Body that yields one frame then a read error.
	r := io.MultiReader(
		strings.NewReader("data: {\"kind\":\"cli\",\"line\":{\"type\":\"system\"},\"ts\":\"t\"}\n\n"),
		iotest{err: errors.New("connection reset by peer")},
	)
	api := &fakeAPI{invokeBody: io.NopCloser(r)}
	c := newTestClient(api)

	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "hi"}, nil)
	if err != nil {
		t.Fatalf("Run returned API error for stream break: %v", err)
	}
	if res.State != FailedTransport {
		t.Fatalf("state = %q, want failed-transport", res.State)
	}
	if res.Err == nil {
		t.Fatal("transport failure must carry the stream error")
	}
}

// iotest is a Reader that always fails.
type iotest struct{ err error }

func (r iotest) Read([]byte) (int, error) { return 0, r.err }

func TestRun_CleanEOFWithoutTerminal_IsTransport(t *testing.T) {
	// V8 first-attempt shape: platform burned the microVM mid-job; stream
	// ends cleanly but no result/exit ever arrived.
	api := &fakeAPI{invokeBody: sse(
		`{"kind":"cli","line":{"type":"system","subtype":"init"},"ts":"t"}`,
		`{"kind":"cli","line":{"type":"assistant"},"ts":"t"}`,
	)}
	c := newTestClient(api)
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != FailedTransport {
		t.Fatalf("state = %q, want failed-transport (idle-burn shape)", res.State)
	}
	// RunResult invariant: FailedTransport always carries a non-nil Err so
	// `if res.Err != nil` and `if res.State == FailedTransport` agree.
	if !errors.Is(res.Err, ErrNoTerminalAttestation) {
		t.Fatalf("res.Err = %v, want ErrNoTerminalAttestation", res.Err)
	}
}

func TestRun_CRLFFraming(t *testing.T) {
	// A middlebox may rewrite SSE line endings to CRLF; frames must still
	// decode (Scanner splits on \n leaving a trailing \r).
	body := "data: {\"kind\":\"cli\",\"line\":{\"type\":\"result\",\"is_error\":false},\"ts\":\"t\"}\r\n\r\n" +
		"data: {\"kind\":\"exit\",\"code\":0,\"ts\":\"t\"}\r\n\r\n"
	api := &fakeAPI{invokeBody: io.NopCloser(strings.NewReader(body))}
	c := newTestClient(api)
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != Success {
		t.Fatalf("state = %q, want success (CRLF frames must decode)", res.State)
	}
}

func TestRun_SubtypeOnlyErrorIsFailedClean(t *testing.T) {
	// Defence against CLI builds that signal failure via subtype without
	// is_error: error_* subtype must not classify as Success.
	api := &fakeAPI{invokeBody: sse(
		`{"kind":"cli","line":{"type":"result","subtype":"error_max_turns"},"ts":"t"}`,
		`{"kind":"exit","code":1,"ts":"t"}`,
	)}
	c := newTestClient(api)
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != FailedClean {
		t.Fatalf("state = %q, want failed-clean for error_* subtype", res.State)
	}
}

func TestRun_FailedClean_CLIError(t *testing.T) {
	api := &fakeAPI{invokeBody: sse(
		`{"kind":"cli","line":{"type":"result","is_error":true},"ts":"t"}`,
		`{"kind":"exit","code":1,"msg":"cli-exited","ts":"t"}`,
	)}
	c := newTestClient(api)
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != FailedClean {
		t.Fatalf("state = %q, want failed-clean", res.State)
	}
	if res.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", res.ExitCode)
	}
}

func TestRun_SinkErrorAborts(t *testing.T) {
	api := &fakeAPI{invokeBody: sse(
		`{"kind":"cli","line":{"type":"system"},"ts":"t"}`,
		`{"kind":"cli","line":{"type":"result","is_error":false},"ts":"t"}`,
		`{"kind":"exit","code":0,"ts":"t"}`,
	)}
	c := newTestClient(api)
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"},
		func(*Envelope) error { return fmt.Errorf("disk full") })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A sink that can't persist events means the run record lies; the run
	// must classify unsafe even though the stream itself was healthy.
	if res.State != FailedTransport {
		t.Fatalf("state = %q, want failed-transport on sink failure", res.State)
	}
}

func TestRun_InvokeError(t *testing.T) {
	api := &fakeAPI{invokeErr: errors.New("throttled")}
	c := newTestClient(api)
	res, err := c.Run(context.Background(), testRunID(), &Payload{Prompt: "x"}, nil)
	// Return contract: an attempted job never reports failure via the
	// function error — `if err != nil { return }` must not skip the §6.2
	// gate. The invoke failure lands in res.State/res.Err.
	if err != nil {
		t.Fatalf("Run returned function error for attempted job: %v", err)
	}
	if res.State != FailedTransport {
		t.Fatalf("res.State = %q, want failed-transport", res.State)
	}
	if res.Err == nil {
		t.Fatal("res.Err must carry the invoke failure")
	}
}

func TestRun_ShortRunIDRejected(t *testing.T) {
	c := newTestClient(&fakeAPI{})
	// F3: API minimum 33 chars; reject locally with a readable error.
	if _, err := c.Run(context.Background(), "run-too-short", &Payload{Prompt: "x"}, nil); err == nil {
		t.Fatal("want error for short runID")
	}
}

func TestRun_EmptyPromptRejected(t *testing.T) {
	c := newTestClient(&fakeAPI{})
	if _, err := c.Run(context.Background(), testRunID(), &Payload{}, nil); err == nil {
		t.Fatal("want error for empty prompt")
	}
}

func TestStop_PassesRunID(t *testing.T) {
	api := &fakeAPI{}
	c := newTestClient(api)
	id := testRunID()
	if err := c.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if api.gotStopRunID != id {
		t.Fatalf("stop saw runID %q, want %q", api.gotStopRunID, id)
	}
}

func TestStop_ErrorWrapped(t *testing.T) {
	api := &fakeAPI{stopErr: errors.New("denied")}
	c := newTestClient(api)
	if err := c.Stop(context.Background(), testRunID()); err == nil {
		t.Fatal("want wrapped stop error")
	}
}

func TestNewRunID_Constraints(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewRunID(time.Now())
		if len(id) < 33 {
			t.Fatalf("NewRunID %q shorter than API minimum 33", id)
		}
		if seen[id] {
			t.Fatalf("NewRunID collision: %q", id)
		}
		seen[id] = true
	}
}

func TestNew_ConfigValidation(t *testing.T) {
	if _, err := New(context.Background(), Config{Region: "us-west-2"}); err == nil {
		t.Fatal("want error for missing RuntimeARN")
	}
	if _, err := New(context.Background(), Config{RuntimeARN: "arn:x"}); err == nil {
		t.Fatal("want error for missing Region")
	}
}

func TestPayload_MarshalSizeCeiling(t *testing.T) {
	p := &Payload{Prompt: strings.Repeat("a", maxPayloadBytes)}
	if _, err := p.Marshal(); err == nil {
		t.Fatal("want oversize error")
	}
}
