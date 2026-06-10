package agentcore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

// runtimeAPI is the slice of the AgentCore data-plane SDK the client uses.
// Interface seam so tests inject fakes (mirrors internal/transcribe's
// transcribeAPI pattern).
type runtimeAPI interface {
	InvokeAgentRuntime(ctx context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
	StopRuntimeSession(ctx context.Context, params *bedrockagentcore.StopRuntimeSessionInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.StopRuntimeSessionOutput, error)
}

// Config selects the runtime a sandbox job is sent to.
type Config struct {
	// RuntimeARN is the AgentCore Runtime to invoke (required).
	RuntimeARN string
	// Region for the AWS client (required; the runtime is regional).
	Region string
}

// Client invokes run-once jobs on an AgentCore Runtime and holds their
// event streams (decision A1-a). Safe for concurrent use.
type Client struct {
	api runtimeAPI
	cfg Config
}

// New builds a Client using the default AWS credential chain (env → IAM
// role → profile), matching internal/transcribe's loading pattern.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.RuntimeARN == "" {
		return nil, fmt.Errorf("agentcore: RuntimeARN is required")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("agentcore: Region is required")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("agentcore: load aws config: %w", err)
	}
	return &Client{api: bedrockagentcore.NewFromConfig(awsCfg), cfg: cfg}, nil
}

// newWithAPI is the test constructor.
func newWithAPI(api runtimeAPI, cfg Config) *Client {
	return &Client{api: api, cfg: cfg}
}

// RunResult is the outcome of one held run.
type RunResult struct {
	RunID string
	// State is the §6.1 three-way classification.
	State TerminalState
	// ExitCode is the CLI exit code when an exit frame arrived (else 0).
	ExitCode int
	// Err is the underlying failure. Invariant: non-nil if and only if
	// State == FailedTransport (a clean-EOF cut with no terminal
	// attestation carries ErrNoTerminalAttestation). Callers may branch on
	// either State or Err interchangeably.
	Err error
}

// ErrNoTerminalAttestation marks a stream that ended cleanly at the HTTP
// layer without the job ever attesting a terminal state (no result event,
// no exit frame) — the platform idle-burn shape observed in validation V8.
// The microVM's fate is unknown; §6.2 containment applies.
var ErrNoTerminalAttestation = errors.New("agentcore: stream ended without result or exit attestation")

// EventSink receives decoded envelopes during a run, in stream order, from
// the goroutine that owns the stream. Keepalive frames are filtered out
// before the sink. Classification observes each envelope BEFORE the sink
// sees it — sinks are read-only consumers; mutating the envelope has no
// effect on the terminal state. Sink errors abort the run (classified
// FailedTransport): losing events is worse than losing the run — the run
// record would lie.
type EventSink func(env *Envelope) error

// Run invokes one run-once job and holds the event stream until terminal
// (RFC §4.3 A1-a). It blocks for the job's whole lifetime; run it from a
// goroutine the caller owns.
//
// Return contract (deliberately strict so the §6.2 gate cannot be skipped
// by an `if err != nil { return }` idiom):
//
//	err != nil ⟺ res == nil — the job was never attempted (invalid
//	    payload / runID); nothing reached the platform, no containment due.
//	err == nil ⟺ res != nil — the job was attempted; res.State is the
//	    only truth about its fate. Invoke-call failures, stream breaks,
//	    and sink failures all land in res.State == FailedTransport with
//	    res.Err set. There is no path where an attempted job's failure is
//	    reported via the function error.
//
// ctx cancellation breaks the hold and classifies FailedTransport — the
// §6.2 containment (Stop-then-confirm before replay) applies. maxLifetime
// clamping is the runtime's job (configured ≤60min per §6.2 rule 2); Run
// adds no extra timeout of its own.
func (c *Client) Run(ctx context.Context, runID string, payload *Payload, sink EventSink) (*RunResult, error) {
	if len(runID) < 33 {
		// Validation F3: the API rejects shorter ids with an opaque 4xx.
		return nil, fmt.Errorf("agentcore: runID %q shorter than 33 chars (API minimum)", runID)
	}
	body, err := payload.Marshal()
	if err != nil {
		return nil, err
	}

	out, err := c.api.InvokeAgentRuntime(ctx, &bedrockagentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn:  aws.String(c.cfg.RuntimeARN),
		RuntimeSessionId: aws.String(runID),
		ContentType:      aws.String("application/json"),
		Accept:           aws.String("text/event-stream"),
		Payload:          body,
	})
	if err != nil {
		// The invoke call failed, but the request may have reached the
		// platform — conservatively an attempted job: FailedTransport, so
		// retry paths go through the §6.2 Stop-then-confirm gate.
		return &RunResult{
			RunID: runID,
			State: FailedTransport,
			Err:   fmt.Errorf("agentcore: invoke: %w", err),
		}, nil
	}
	defer out.Response.Close()

	return holdStream(ctx, runID, out.Response, sink), nil
}

// holdStream decodes SSE frames off the response body until it ends, fans
// envelopes out to sink, and classifies the terminal state. Free function:
// it depends only on the stream, never on AWS state. Streaming decode
// line-by-line — never buffer the whole body (validation F1 is the whole
// reason this package exists).
func holdStream(ctx context.Context, runID string, body io.Reader, sink EventSink) *RunResult {
	var cls classifier
	res := &RunResult{RunID: runID}

	sc := bufio.NewScanner(body)
	// Single stream-json lines can carry large tool results. The bootstrap
	// caps its stdout lines at 16MB and wraps them in the SSE envelope —
	// allow envelope overhead on top so a max-size CLI line still fits.
	sc.Buffer(make([]byte, 64*1024), (16<<20)+(64<<10))

	for sc.Scan() {
		raw := sc.Bytes()
		// SSE framing: "data: {...}" lines separated by blank lines. Strip
		// a trailing \r in case a middlebox rewrote LF to CRLF (Scanner
		// only splits on \n).
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		data, ok := bytes.CutPrefix(raw, []byte("data: "))
		if !ok {
			continue // blank separators, comments
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			slog.Warn("agentcore: undecodable SSE frame, skipped",
				"run_id", runID, "len", len(data))
			continue
		}
		cls.observe(&env)
		if env.Kind == KindExit {
			res.ExitCode = env.Code
		}
		if env.Kind == KindKeepalive {
			continue // liveness only — never reaches the sink
		}
		if sink != nil {
			if err := sink(&env); err != nil {
				res.State = FailedTransport
				res.Err = fmt.Errorf("agentcore: event sink: %w", err)
				return res
			}
		}
	}

	streamErr := sc.Err()
	if streamErr == nil && ctx.Err() != nil {
		// Scanner can surface a cancelled read as a clean EOF depending on
		// the transport layer; ctx is the truth.
		streamErr = ctx.Err()
	}
	res.State = cls.terminal(streamErr)
	if res.State == FailedTransport {
		res.Err = streamErr
		if res.Err == nil {
			res.Err = ErrNoTerminalAttestation
		}
	}
	return res
}

// Stop tears down a runtime session. This is the §6.2 rule-1 termination
// primitive: after FailedTransport, Stop MUST succeed before the run is
// eligible for replay. Validation V4: takes effect within seconds.
func (c *Client) Stop(ctx context.Context, runID string) error {
	_, err := c.api.StopRuntimeSession(ctx, &bedrockagentcore.StopRuntimeSessionInput{
		AgentRuntimeArn:  aws.String(c.cfg.RuntimeARN),
		RuntimeSessionId: aws.String(runID),
	})
	if err != nil {
		return fmt.Errorf("agentcore: stop session %s: %w", runID, err)
	}
	return nil
}
