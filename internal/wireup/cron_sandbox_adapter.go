// cron_sandbox_adapter.go adapts internal/agentcore's Client to cron's
// SandboxRunner seam, mirroring cron_router_adapter.go's role for the
// session router: cron stays compile-time independent of the AWS SDK, and
// the dependency arrow points main → wireup → {cron, agentcore}.
package wireup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/agentcore"
	"github.com/naozhi/naozhi/internal/cron"
)

// stopConfirmTimeout bounds the post-transport-failure StopRuntimeSession
// call. Validation V4 measured Stop taking effect within seconds; 30s
// covers API retry slack without wedging the cron worker.
const stopConfirmTimeout = 30 * time.Second

// agentcoreSandboxRunner implements cron.SandboxRunner over agentcore.Client.
type agentcoreSandboxRunner struct {
	client *agentcore.Client
	// settings is the tenant config layer injected into every microVM
	// (~/.claude/settings.json — RFC §2.1 runtime-injection column).
	// Built once at construction: CC inside the microVM talks to Bedrock
	// via the Runtime IAM execution role, so the only required keys are
	// the Bedrock switch + region. No credentials ever ride here.
	settings json.RawMessage
}

// sandboxSettings renders the injected settings.json for the microVM CLI.
func sandboxSettings(region string) (json.RawMessage, error) {
	b, err := json.Marshal(map[string]any{
		"env": map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_REGION":              region,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("wireup: render sandbox settings: %w", err)
	}
	return b, nil
}

// newAgentcoreSandboxRunner builds the production SandboxRunner, or
// (nil, nil) when BOTH fields are empty (feature off). A half-filled
// config (one field set) is an operator mistake, not "off" — it returns
// an error so WireSchedulers WARNs instead of silently disabling.
func newAgentcoreSandboxRunner(ctx context.Context, runtimeARN, region string) (cron.SandboxRunner, error) {
	if runtimeARN == "" && region == "" {
		return nil, nil // sandbox placement not configured — feature off
	}
	client, err := agentcore.New(ctx, agentcore.Config{RuntimeARN: runtimeARN, Region: region})
	if err != nil {
		return nil, fmt.Errorf("wireup: agentcore sandbox client: %w", err)
	}
	settings, err := sandboxSettings(region)
	if err != nil {
		return nil, err
	}
	return &agentcoreSandboxRunner{client: client, settings: settings}, nil
}

// RunJob executes one run-once job: invoke, hold the stream (fanning raw
// envelope lines to eventSink), classify, and — on transport failure —
// attempt the §6.2 rule-1 StopRuntimeSession containment before returning.
func (r *agentcoreSandboxRunner) RunJob(ctx context.Context, job cron.SandboxJob, eventSink func(line []byte) error) (cron.SandboxOutcome, error) {
	payload := &agentcore.Payload{
		Settings: r.settings,
		Prompt:   job.Prompt,
		Model:    job.Model,
	}

	// runtimeSessionId: cron run IDs are 16-hex (generateRunID) but the
	// AgentCore API requires ≥33 chars (validation F3). Derive a compliant
	// id that EMBEDS the cron runID so logs/CloudTrail correlate back to
	// the run record: "run-<cronRunID>-<unixnano>" ≈ 40 chars, unique per
	// invocation (RFC §4.1 — never reuse across runs).
	runtimeID := fmt.Sprintf("run-%s-%d", job.RunID, time.Now().UnixNano())

	var resultText string
	sink := func(env *agentcore.Envelope) error {
		// Track the latest result-bearing CLI line's text for the cron
		// run record. Cheap probe: full parsing stays with the dashboard
		// (Phase 2 run-detail view reads the persisted event log).
		if env.Kind == agentcore.KindCLI {
			if txt, ok := agentcore.ResultText(env.Line); ok {
				resultText = txt
			}
		}
		if eventSink == nil {
			return nil
		}
		raw, err := envelopeLine(env)
		if err != nil {
			return err
		}
		return eventSink(raw)
	}

	res, err := r.client.Run(ctx, runtimeID, payload, sink)
	if err != nil {
		// Never-attempted contract (PR-2a): invalid payload, nothing
		// reached the platform.
		return cron.SandboxOutcome{}, err
	}

	switch res.State {
	case agentcore.Success:
		return cron.SandboxOutcome{State: cron.SandboxStateSuccess, ResultText: resultText}, nil
	case agentcore.FailedClean:
		return cron.SandboxOutcome{
			State:      cron.SandboxStateFailedClean,
			ResultText: resultText,
			ErrMsg:     fmt.Sprintf("sandbox job failed (exit %d)", res.ExitCode),
		}, nil
	default: // agentcore.FailedTransport
		out := cron.SandboxOutcome{
			State:      cron.SandboxStateFailedTransport,
			ResultText: resultText,
		}
		if res.Err != nil {
			out.ErrMsg = res.Err.Error()
		}
		// §6.2 rule 1: the microVM may still be running; Stop before this
		// run can ever be replayed. Use a fresh short-lived ctx — the run
		// ctx may already be cancelled (that cancellation may be WHY we
		// are here), and the containment call must not inherit it.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopConfirmTimeout)
		defer cancel()
		if stopErr := r.client.Stop(stopCtx, runtimeID); stopErr != nil {
			slog.Error("cron sandbox: StopRuntimeSession failed after transport break; microVM fate unknown",
				"run_id", job.RunID, "runtime_session_id", runtimeID, "err", stopErr)
		} else {
			out.StopConfirmed = true
		}
		return out, nil
	}
}

// envelopeLine re-encodes one envelope as a single NDJSON line for cron's
// schema-agnostic event log.
func envelopeLine(env *agentcore.Envelope) ([]byte, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("wireup: encode sandbox envelope: %w", err)
	}
	return b, nil
}
