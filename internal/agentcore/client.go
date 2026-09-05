package agentcore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/naozhi/naozhi/internal/costledger"
	"io"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// runtimeAPI is the slice of the AgentCore SDK the client uses (test seam).
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
// event streams. Safe for concurrent use.
type Client struct {
	api runtimeAPI
	cfg Config
}

// New builds a Client using the default AWS credential chain.
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
	// RuntimeARN must belong to the operator's own account: a config-injected
	// ARN for an attacker-controlled runtime would exfiltrate the job prompt.
	ident, err := sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("agentcore: resolve caller account (sts): %w", err)
	}
	if err := verifyARNAccount(cfg.RuntimeARN, aws.ToString(ident.Account)); err != nil {
		return nil, err
	}
	return &Client{api: bedrockagentcore.NewFromConfig(awsCfg), cfg: cfg}, nil
}

// verifyARNAccount checks that runtimeARN parses, names a regional
// bedrock-agentcore runtime, and matches the caller's account. Pure (no AWS).
func verifyARNAccount(runtimeARN, accountID string) error {
	if accountID == "" {
		return fmt.Errorf("agentcore: empty caller account id from STS")
	}
	parsed, err := arn.Parse(runtimeARN)
	if err != nil {
		return fmt.Errorf("agentcore: invalid RuntimeARN %q: %w", runtimeARN, err)
	}
	// A same-account ARN for another service would pass the account check alone.
	if parsed.Service != "bedrock-agentcore" {
		return fmt.Errorf("agentcore: RuntimeARN service %q is not bedrock-agentcore (config-injection guard)", parsed.Service)
	}
	if parsed.Region == "" {
		return fmt.Errorf("agentcore: RuntimeARN %q has empty region (runtime is regional; region is required)", runtimeARN)
	}
	if parsed.AccountID != accountID {
		return fmt.Errorf("agentcore: RuntimeARN account %q does not match caller account %q (config-injection guard)", parsed.AccountID, accountID)
	}
	return nil
}

// newWithAPI is the test constructor.
func newWithAPI(api runtimeAPI, cfg Config) *Client {
	return &Client{api: api, cfg: cfg}
}

// RunResult is the outcome of one held run.
type RunResult struct {
	RunID string
	// State is the three-way terminal classification.
	State TerminalState
	// ExitCode is the CLI exit code when an exit frame arrived (else 0).
	ExitCode int
	// CostUSD is total_cost_usd from the CLI result event; 0 when none arrived.
	CostUSD float64
	// Models / Basis are the result event's per-model drill-down and worst
	// price basis; nil / "" when none arrived.
	Models []costledger.ModelDelta
	Basis  costledger.Basis
	// DurationMS is duration_ms from the CLI result event; 0 when none arrived.
	DurationMS int64
	// ImageVersion / MemoryPeakBytes come from the bootstrap kind=meta frame.
	ImageVersion    string
	MemoryPeakBytes int64
	// Err is non-nil iff State == FailedTransport (a clean-EOF cut without
	// attestation carries ErrNoTerminalAttestation).
	Err error
}

// ErrNoTerminalAttestation marks a stream that ended cleanly at the HTTP
// layer without result or exit frame (platform idle-burn); microVM fate unknown.
var ErrNoTerminalAttestation = errors.New("agentcore: stream ended without result or exit attestation")

// EventSink receives decoded envelopes in stream order (keepalives filtered).
// Classification observes each envelope BEFORE the sink; sinks are read-only.
// A sink error aborts the run as FailedTransport — a lossy run record would lie.
type EventSink func(env *Envelope) error

// Run invokes one run-once job and blocks until the event stream is terminal.
//
// Return contract (strict so the containment gate cannot be skipped):
//
//	err != nil ⟺ res == nil — never attempted (invalid payload / runID).
//	err == nil ⟺ res != nil — attempted; res.State is the only truth. Invoke
//	    failures, stream breaks and sink failures all land in FailedTransport.
//
// ctx cancellation classifies FailedTransport (Stop-then-confirm before
// replay). Run adds no timeout; maxLifetime clamping is the runtime's job.
func (c *Client) Run(ctx context.Context, runID string, payload *Payload, sink EventSink) (*RunResult, error) {
	if len(runID) < 33 {
		// The API rejects shorter ids with an opaque 4xx.
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
		// The request may have reached the platform: conservatively attempted.
		return &RunResult{
			RunID: runID,
			State: FailedTransport,
			Err:   fmt.Errorf("agentcore: invoke: %w", err),
		}, nil
	}
	defer out.Response.Close()

	return holdStream(ctx, runID, out.Response, sink), nil
}

// holdStream decodes SSE frames line-by-line (never buffering the whole
// body), fans envelopes out to sink, and classifies the terminal state.
func holdStream(ctx context.Context, runID string, body io.Reader, sink EventSink) *RunResult {
	var cls classifier
	res := &RunResult{RunID: runID}

	sc := bufio.NewScanner(body)
	// Shared wire ceiling: any line accepted here is readable back (#2083).
	sc.Buffer(make([]byte, 64*1024), MaxEnvelopeLineBytes)

	for sc.Scan() {
		raw := sc.Bytes()
		// SSE "data: {...}" frames; strip \r in case a middlebox rewrote LF to CRLF.
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		data, ok := bytes.CutPrefix(raw, []byte("data: "))
		if !ok {
			continue
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			slog.Warn("agentcore: undecodable SSE frame, skipped",
				"run_id", runID, "len", len(data))
			continue
		}
		cls.observe(&env)
		switch env.Kind {
		case KindExit:
			res.ExitCode = env.Code
		case KindMeta:
			if env.ImageVersion != "" {
				res.ImageVersion = env.ImageVersion
			}
			if env.MemoryPeakBytes > 0 {
				res.MemoryPeakBytes = env.MemoryPeakBytes
			}
		case KindCLI:
			if m, ok := ResultMetaOf(env.Line); ok {
				res.CostUSD = m.CostUSD
				res.DurationMS = m.DurationMS
				res.Models, res.Basis = m.Models, m.Basis
			}
		}
		if env.Kind == KindKeepalive {
			continue
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
		// A cancelled read can surface as a clean EOF; ctx is the truth.
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

// RuntimeARN returns the configured runtime ARN.
func (c *Client) RuntimeARN() string { return c.cfg.RuntimeARN }

// Stop tears down a runtime session. After FailedTransport, Stop MUST
// succeed before the run is eligible for replay.
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
