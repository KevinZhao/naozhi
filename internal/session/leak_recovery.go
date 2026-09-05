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
// DEFAULT-OFF deliberately: the CLI runs with --dangerously-skip-permissions,
// so a leaked destructive call (rm, force-push, Write) is INERT, but
// auto-continue asks the model to re-issue it as a real invocation, which
// WOULD execute with no human gate. Canary the recovery/destructive-surprise
// metrics before considering a default flip. Truthiness: envpolicy.EnvTruthy.
const leakRecoveryEnvVar = "NAOZHI_LEAK_RECOVERY"

// leakContinuePrompt is injected verbatim as the recovery turn's user message.
// Wrapped in <system-reminder> so dashboard.js hides the bubble and it reads
// as a system nudge; English avoids flipping the model's reply language. It
// contains "invoke" but NOT the `call\n<invoke name="` anchor shape, so echoing
// it cannot re-trip leakguard.Detect. The last sentence is an escape hatch for
// the rare semantic false positive (intentional example).
const leakContinuePrompt = "<system-reminder>Your previous turn wrote a tool call as plain text (an invoke block appeared in the reply body) instead of actually invoking the tool, so nothing executed and the task is unfinished. Do not paste tool-call XML as prose. Re-issue that exact tool call now as a real tool invocation and continue until the task is complete. If that block was an intentional example for the user, simply say so and stop.</system-reminder>"

// leakRecoveryEnabled reports whether auto-continue-on-leak is switched on.
// Read per-decision (not cached) so an operator can toggle it live without a
// restart; one getenv on a path that just ran a regex is negligible.
func leakRecoveryEnabled() bool {
	return envpolicy.EnvTruthy(os.Getenv(leakRecoveryEnvVar))
}

// recoverLeakedToolcall inspects a completed turn's result and, when the model
// leaked a tool call into prose, auto-continues by re-sending a nudge on the
// SAME live process via resend. Returns result unchanged when disabled, not
// applicable, or not a leak. Re-sends exactly ONCE (the recovered result is
// never re-inspected, so a leak-on-every-turn model cannot loop); on re-send
// error or a second leak, returns the stripped text so no-fold channels stay
// readable. CostUSD is the CLI's cumulative per-incarnation total, so the
// recovered turn's value already includes turn 1 — never sum them (#2355).
// resend must stay on the same process: legacy Send holds sendMu;
// SendPassthrough uses priority="next" so a racing user message cannot jump ahead.
func (s *ManagedSession) recoverLeakedToolcall(
	ctx context.Context,
	proc processIface,
	result *cli.SendResult,
	resend func(context.Context, string) (*cli.SendResult, error),
) *cli.SendResult {
	// MergedCount > 1 is a passthrough follower slot sharing a head's result —
	// it must never trigger its own recovery.
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
	// Re-check liveness right before the re-send: a cron watchdog may have
	// cancelled ctx / killed the process since the turn completed, and
	// recovering into it would burn the settle window for nothing.
	if ctx.Err() != nil || !proc.Alive() {
		return result
	}

	slog.Warn("leak-recovery: detected leaked tool call, auto-continuing",
		"key", s.key)

	rec, err := resend(ctx, leakContinuePrompt)
	if err != nil {
		// Process died mid-recovery: hand back the original body, XML stripped.
		metrics.ToolCallLeakRecoveryFailedTotal.Add(1)
		slog.Warn("leak-recovery: re-send failed", "key", s.key, "err", err)
		return strippedResult(result)
	}
	if rec == nil || rec.Text == "" || leakguard.Detect(rec.Text) {
		// Second-order leak (or empty). cap=1: do NOT retry.
		metrics.ToolCallLeakRecoveryFailedTotal.Add(1)
		slog.Warn("leak-recovery: model leaked again on retry (cap=1, giving up)",
			"key", s.key)
		if rec == nil || rec.Text == "" {
			return strippedResult(result)
		}
		// rec.CostUSD is cumulative (already includes turn 1) — do NOT add
		// result.CostUSD on top.
		return strippedResult(rec)
	}

	metrics.ToolCallLeakRecoveredTotal.Add(1)
	slog.Info("leak-recovery: recovered", "key", s.key)
	return &cli.SendResult{
		Text:      rec.Text,
		SessionID: firstNonEmpty(rec.SessionID, result.SessionID),
		// Cumulative total already includes the leaked turn; never sum (#2355).
		// ModelUsage carries the same cumulative semantics and MUST travel with
		// it, otherwise the recovered turn's per-model delta is lost. Sharing
		// the map is safe: ReadEvent copies the Event out of its pool before
		// returning, so nothing else mutates it.
		CostUSD:     rec.CostUSD,
		ModelUsage:  rec.ModelUsage,
		MergedCount: rec.MergedCount,
	}
}

// strippedResult returns a copy of r with the leaked tool-call XML removed from
// Text, preserving all other fields.
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
