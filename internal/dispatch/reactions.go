package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// reactionAckTimeout bounds how long AddReaction/RemoveReaction can block.
// The reaction is UX sugar on the IM hot path — if the platform API is slow,
// fall back to text notice rather than stall the inbound handler. 3s is
// generous for cross-region HTTP but well below user perception of "stuck".
const reactionAckTimeout = 3 * time.Second

// ackQueuedWithReaction attempts to signal "message queued" by adding a
// reaction on the user's inbound message. Returns true if the reaction
// landed (caller should suppress the text fallback); false if the platform
// lacks Reactor capability, the message has no ID, or the API call failed.
//
// Best-effort by design: a reaction that fails to send is not worth retrying
// — the caller falls back to the rate-limited text notice, and the user
// still learns their message was received.
func (d *Dispatcher) ackQueuedWithReaction(ctx context.Context, msg platform.IncomingMessage, lg *slog.Logger) bool {
	// R260528-BUG-22: emit a Debug trace at every false-return arm so the
	// "fell back to text" decision is investigable from logs alone.
	// Pre-fix only the AddReaction error arm logged; the platform-not-
	// reactor / no-MessageID / no-platform paths fell back silently and
	// any "why didn't I get a reaction?" investigation had to bisect
	// through code reading instead of grep.
	useLg := lg
	if useLg == nil {
		useLg = slog.Default()
	}
	if msg.MessageID == "" {
		useLg.Debug("ack queued reaction skipped", "reason", "no_message_id")
		return false
	}
	p := d.platforms[msg.Platform]
	if p == nil {
		useLg.Debug("ack queued reaction skipped", "reason", "no_platform", "platform", msg.Platform)
		return false
	}
	reactor, ok := platform.AsCapability[platform.Reactor](p)
	if !ok {
		useLg.Debug("ack queued reaction skipped", "reason", "platform_not_reactor", "platform", msg.Platform)
		return false
	}
	// Derive a bounded context so a stalled reaction API can't hold up the
	// webhook handler; the parent ctx still cancels on shutdown.
	rctx, cancel := context.WithTimeout(ctx, reactionAckTimeout)
	defer cancel()
	if err := reactor.AddReaction(rctx, msg.MessageID, platform.ReactionQueued); err != nil {
		// R230-CQ-3: align nil-handling with clearQueuedReactions — fall back to
		// slog.Default() so a missing logger never silently drops the failure.
		useLg.Debug("ack queued reaction skipped", "reason", "api_error", "err", err)
		return false
	}
	return true
}

// ackMergedFollower signals that this user message was merged into another
// message's reply (passthrough head/follower fan-out). Preferred surface is
// a reaction on the user's message; fall back to a short text reply when
// the platform is not reactor-capable. Rate-limited via ShouldNotify so a
// burst of follower acks doesn't spam the chat.
//
// #1784: key is the resolved session key (the same bucket the rest of the
// dispatch path rate-limits on, e.g. handleQueuedNonOwner's ShouldNotify(key)).
// The pre-fix code passed msg.ChatID, which keys a different bucket than the
// per-key MessageQueue tracks — so the cooldown never matched and a follower
// burst could either over- or under-fire the text fallback.
func (d *Dispatcher) ackMergedFollower(ctx context.Context, msg platform.IncomingMessage, key string, mergedCount int, lg *slog.Logger) {
	if d.ackQueuedWithReaction(ctx, msg, lg) {
		return
	}
	if d.queue != nil && !d.queue.ShouldNotify(key) {
		return
	}
	_ = mergedCount // reserved for future reaction variant showing count
	d.replyText(ctx, msg, "已合并到上一条回复。", lg)
}

// clearQueuedReactions removes the "queued" reaction from each drained
// message. Called from ownerLoop after a drain-batch has been processed.
// Errors are logged and swallowed — a lingering reaction is cosmetically
// unfortunate but not user-blocking, and retrying here would require more
// state without meaningful gain.
func (d *Dispatcher) clearQueuedReactions(ctx context.Context, platformName string, queued []QueuedMsg, lg *slog.Logger) {
	if len(queued) == 0 {
		return
	}
	p := d.platforms[platformName]
	if p == nil {
		return
	}
	reactor, ok := platform.AsCapability[platform.Reactor](p)
	if !ok {
		return
	}
	// One shared timeout budget for the whole batch instead of
	// context.WithTimeout per iteration. The old per-message ctx created a
	// runtime timer + *timerCtx heap alloc and goroutine per queued msg.
	// Sharing one ctx also means a stalling IM API cannot drag the full
	// reactionAckTimeout × N — the whole cleanup aborts together, which is
	// the desired behaviour since the reactions are purely cosmetic.
	// R60-PERF-6.
	rctx, cancel := context.WithTimeout(ctx, reactionAckTimeout)
	defer cancel()
	for _, m := range queued {
		if m.MessageID == "" {
			continue
		}
		if rctx.Err() != nil {
			// Batch deadline exceeded; further RemoveReaction calls would
			// fail immediately. Stop iterating so we don't log N identical
			// timeout warnings.
			return
		}
		if err := reactor.RemoveReaction(rctx, m.MessageID, platform.ReactionQueued); err != nil {
			// R230-CQ-3: collapsed if/else into one fallback to match the
			// pattern used in ackQueuedWithReaction / scheduler.go's `lg`.
			useLg := lg
			if useLg == nil {
				useLg = slog.Default()
			}
			useLg.Debug("remove queued reaction failed", "msg_id", m.MessageID, "err", err)
		}
	}
}
