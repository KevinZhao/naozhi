package session

import (
	"context"
	"log/slog"
	"os"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/leakguard"
	"github.com/naozhi/naozhi/internal/metrics"
)

// leakRecoveryEnvVar is the operator opt-in for auto-continuing a turn that
// stalled because the model wrote tool-call XML as prose (a "leaked tool
// call") instead of a structured tool_use, so nothing executed.
//
// It ships DEFAULT-OFF deliberately. naozhi runs the CLI with
// --dangerously-skip-permissions, so a leaked destructive call (rm, force-push,
// Write) is currently INERT — it was never executed. Auto-continue asks the
// model to re-issue that exact call as a real invocation, which WOULD execute
// it with no human gate. Gate behind this flag, canary the recovery-success
// rate and destructive-surprise rate via the metrics counters, and only then
// consider flipping the default. Truthiness follows envpolicy.EnvTruthy (any
// non-empty, non-"0"/"false"/"off" value enables it).
const leakRecoveryEnvVar = "NAOZHI_LEAK_RECOVERY"

// leakContinuePrompt is injected verbatim as the recovery turn's user message.
//
// It is wrapped in <system-reminder> for two reasons: (1) dashboard.js already
// hides user bubbles whose body starts with <system-reminder> (see the filters
// at dashboard.js ~2496 / ~3563), so no ghost "continue" bubble appears in the
// transcript with zero JS changes; (2) it reads as a system nudge rather than
// operator speech. English is used to avoid flipping the model's reply
// language mid-conversation.
//
// The text itself contains the word "invoke" but NOT the `call\n<invoke name="`
// anchor shape, so echoing it back cannot re-trip leakguard.Detect. The final
// sentence gives the model an explicit "this was an intentional example, stop"
// escape hatch to soften the rare semantic false positive.
const leakContinuePrompt = "<system-reminder>Your previous turn wrote a tool call as plain text (an invoke block appeared in the reply body) instead of actually invoking the tool, so nothing executed and the task is unfinished. Do not paste tool-call XML as prose. Re-issue that exact tool call now as a real tool invocation and continue until the task is complete. If that block was an intentional example for the user, simply say so and stop.</system-reminder>"

// leakRecoveryEnabled reports whether auto-continue-on-leak is switched on.
// Read per-decision (not cached) so an operator can toggle it live via the
// environment on the next turn without a restart — the cost is one getenv on
// the only path that also just ran a regex over the whole result, so it is
// negligible.
func leakRecoveryEnabled() bool {
	return envpolicy.EnvTruthy(os.Getenv(leakRecoveryEnvVar))
}

// recoverLeakedToolcall inspects a completed turn's result and, when the model
// leaked a tool call into prose (stalling the turn), auto-continues by
// re-sending a nudge on the SAME live process via the provided resend closure.
//
// Contract:
//   - Returns result unchanged when recovery is disabled, not applicable, or
//     the text is not a leak (the overwhelmingly common case — one cheap regex).
//   - On a genuine leak with recovery enabled, re-sends exactly ONCE (cap=1,
//     structural: the recovered result is never re-inspected, so a
//     leak-on-every-turn model cannot loop). Returns the clean follow-up result
//     with SessionID = recovered-or-original and CostUSD = the recovered turn's
//     value (NOT summed — see note below).
//
// COST SEMANTICS (#2355 review MEDIUM): SendResult.CostUSD is the CLI's
// cumulative total_cost_usd for the process incarnation, NOT a per-turn delta.
// The recovery re-send runs on the SAME live process, so rec.CostUSD already
// INCLUDES result.CostUSD. Summing them would double-count turn 1. We therefore
// return rec.CostUSD as-is. Session /health billing is unaffected either way —
// it is driven by finishRun/TurnCostDelta, which is called once per turn (both
// the leaked turn and the recovery turn) and computes deltas independently of
// this return value. The consumer that DID care is cron (CronRun.CostUSD
// persists this field verbatim), which previously over-reported on recovered
// runs.
//   - When the re-send errors (process died) or leaks again, returns the
//     original/recovered text with the leaked XML stripped so no-fold channels
//     (feishu / weixin) stay readable, and records a recovery_failed.
//
// resend must re-enter the send path on the SAME live process:
//   - legacy Send:     WriteMessage + read to next result (holds sendMu, serial)
//   - SendPassthrough: priority="next" so the nudge is enqueued immediately
//     after the leaked turn and a racing user message cannot jump the FIFO
//     ahead of it.
func (s *ManagedSession) recoverLeakedToolcall(
	ctx context.Context,
	proc processIface,
	result *cli.SendResult,
	resend func(context.Context, string) (*cli.SendResult, error),
) *cli.SendResult {
	// Gate: applicable only to a live single-slot turn carrying leaked text.
	// MergedCount > 1 with empty Text is a passthrough follower slot sharing a
	// head's result — it must never trigger its own recovery.
	if result == nil || result.Text == "" || result.MergedCount > 1 {
		return result
	}
	if !leakguard.Detect(result.Text) {
		return result
	}

	// Detection fires regardless of the flag so the counter quantifies the
	// true model-regression rate even while recovery is dark-launched off.
	metrics.ToolCallLeakDetectedTotal.Add(1)

	if !leakRecoveryEnabled() {
		return result
	}
	// Re-check liveness immediately before the re-send: a cron deadline
	// watchdog may have cancelled ctx / killed the process between the turn
	// completing and here. Recovering into a dead process or cancelled context
	// would burn the settle window for a result that never arrives.
	if ctx.Err() != nil || !proc.Alive() {
		return result
	}

	slog.Warn("leak-recovery: detected leaked tool call, auto-continuing",
		"key", s.key)

	rec, err := resend(ctx, leakContinuePrompt)
	if err != nil {
		// Process died mid-recovery. Hand back the original body with the XML
		// stripped so IM channels do not show the raw <invoke> wall.
		metrics.ToolCallLeakRecoveryFailedTotal.Add(1)
		slog.Warn("leak-recovery: re-send failed", "key", s.key, "err", err)
		return strippedResult(result)
	}
	if rec == nil || rec.Text == "" || leakguard.Detect(rec.Text) {
		// Second-order leak (or empty). cap=1: do NOT retry — the recovered
		// result is never re-inspected for a further re-send.
		metrics.ToolCallLeakRecoveryFailedTotal.Add(1)
		slog.Warn("leak-recovery: model leaked again on retry (cap=1, giving up)",
			"key", s.key)
		if rec == nil || rec.Text == "" {
			// Recovery produced nothing usable — fall back to the original
			// turn's stripped body. result.CostUSD is that turn's cumulative
			// value; the recovery re-send (if it ran) added its own finishRun
			// delta already, so no summing here.
			return strippedResult(result)
		}
		// The recovered turn leaked again; hand back its stripped body. Its
		// CostUSD is the cumulative-so-far value (already includes turn 1), so
		// it is the correct total — do NOT add result.CostUSD on top.
		return strippedResult(rec)
	}

	metrics.ToolCallLeakRecoveredTotal.Add(1)
	slog.Info("leak-recovery: recovered", "key", s.key)
	return &cli.SendResult{
		Text:      rec.Text,
		SessionID: firstNonEmpty(rec.SessionID, result.SessionID),
		// rec.CostUSD is the process's cumulative total (already includes the
		// leaked turn's cost); return it as-is rather than summing. (#2355 MEDIUM)
		CostUSD:     rec.CostUSD,
		MergedCount: rec.MergedCount,
	}
}

// strippedResult returns a copy of r with the leaked tool-call XML removed from
// Text, preserving all other fields. Used on the failure paths so a no-fold
// channel is handed clean prose instead of a wall of <invoke> XML.
func strippedResult(r *cli.SendResult) *cli.SendResult {
	if r == nil {
		return nil
	}
	prose, _, found := leakguard.Strip(r.Text)
	if !found {
		return r
	}
	out := *r
	out.Text = prose
	return &out
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
