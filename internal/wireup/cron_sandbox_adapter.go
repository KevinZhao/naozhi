// cron_sandbox_adapter.go adapts internal/agentcore's Client to cron's
// SandboxRunner seam, mirroring cron_router_adapter.go's role for the
// session router: cron stays compile-time independent of the AWS SDK, and
// the dependency arrow points main → wireup → {cron, agentcore}.
package wireup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/naozhi/naozhi/internal/costledger"
	"log/slog"
	"time"

	bedrockagentcoretypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"

	"github.com/naozhi/naozhi/internal/agentcore"
	"github.com/naozhi/naozhi/internal/cron"
)

// stopConfirmTimeout bounds the post-transport-failure StopRuntimeSession
// call: Stop takes effect within seconds, 30s covers API retry slack.
const stopConfirmTimeout = 30 * time.Second

// agentcoreSandboxRunner implements cron.SandboxRunner over agentcore.Client.
type agentcoreSandboxRunner struct {
	client *agentcore.Client
	// settings is the settings.json injected into every microVM: only the
	// Bedrock switch + region — CC uses the Runtime IAM role, so no
	// credentials ever ride here.
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

// newAgentcoreSandboxRunner builds the SandboxRunner, or (nil, nil) when BOTH
// fields are empty. A half-filled config is an operator mistake and errors so
// WireSchedulers WARNs instead of silently disabling.
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

// RunJob executes one run-once job: invoke, fan raw envelope lines to
// eventSink, classify, and on transport failure attempt StopRuntimeSession
// containment (RFC §6.2 rule 1) before returning.
func (r *agentcoreSandboxRunner) RunJob(ctx context.Context, job cron.SandboxJob, eventSink func(line []byte) error) (cron.SandboxOutcome, error) {
	payload := &agentcore.Payload{
		Settings: r.settings,
		Prompt:   job.Prompt,
		Model:    job.Model,
	}

	// cron derives the id so the pending record holds it before the invoke.
	// Fail closed rather than synthesise one: a generated id would never match
	// the pending record and a later reconcile/Stop would orphan the microVM.
	runtimeID := job.RuntimeSessionID
	if runtimeID == "" {
		return cron.SandboxOutcome{}, fmt.Errorf("wireup: sandbox job %s has empty RuntimeSessionID", job.RunID)
	}

	var resultText string
	sink := func(env *agentcore.Envelope) error {
		// Latest result-bearing CLI line feeds the run record; full parsing
		// stays with the dashboard.
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
		// Never attempted: nothing reached the platform.
		return cron.SandboxOutcome{}, err
	}

	// Cloud-execution receipt in cron's SDK-free shape (RFC §7.3).
	meta := cron.SandboxRunMeta{
		RuntimeARN:      r.client.RuntimeARN(),
		ImageVersion:    res.ImageVersion,
		ExitStatus:      res.ExitCode,
		CostUSD:         res.CostUSD,
		DurationMS:      res.DurationMS,
		MemoryPeakBytes: res.MemoryPeakBytes,
		Models:          res.Models,
		Basis:           res.Basis,
	}
	// The run record has a 32 KiB cap; the ledger caps rows on Append, the
	// record needs the same bound here.
	if len(meta.Models) > costledger.MaxModels {
		meta.Models = meta.Models[:costledger.MaxModels]
	}

	switch res.State {
	case agentcore.Success:
		return cron.SandboxOutcome{State: cron.SandboxStateSuccess, ResultText: resultText, Meta: meta}, nil
	case agentcore.FailedClean:
		return cron.SandboxOutcome{
			State:      cron.SandboxStateFailedClean,
			ResultText: resultText,
			ErrMsg:     fmt.Sprintf("sandbox job failed (exit %d)", res.ExitCode),
			Meta:       meta,
		}, nil
	default: // agentcore.FailedTransport
		out := cron.SandboxOutcome{
			State:      cron.SandboxStateFailedTransport,
			ResultText: resultText,
			Meta:       meta,
		}
		if res.Err != nil {
			out.ErrMsg = res.Err.Error()
		}
		// The microVM may still be running; Stop before any replay. Fresh ctx:
		// the run ctx may be cancelled (possibly WHY we are here).
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

// StopSession terminates a runtime session by platform id (orphan cleanup
// after restart, RFC §6.5). ResourceNotFoundException maps to success: a
// pending record can predate the invoke ever reaching the platform, and an
// error would park that file in an every-boot retry loop.
func (r *agentcoreSandboxRunner) StopSession(ctx context.Context, runtimeSessionID string) error {
	err := r.client.Stop(ctx, runtimeSessionID)
	var nf *bedrockagentcoretypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return nil
	}
	return err
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
