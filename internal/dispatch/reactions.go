package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// reactionAckTimeout bounds how long AddReaction/RemoveReaction can block:
// reactions are UX sugar on the IM hot path, so a slow platform API falls back
// to the text notice rather than stalling the inbound handler.
const reactionAckTimeout = 3 * time.Second

// ackQueuedWithReaction signals "message queued" by adding a reaction on the
// user's inbound message. Returns true if it landed (caller suppresses the
// text fallback); false if the platform lacks Reactor capability, the
// message has no ID, or the API call failed. Best-effort: on failure the
// caller falls back to the rate-limited text notice.
func (d *Dispatcher) ackQueuedWithReaction(ctx context.Context, msg platform.IncomingMessage, lg *slog.Logger) bool {
	// Debug-trace every false-return arm so the "fell back to text" decision
	// is investigable from logs alone.
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
		useLg.Debug("ack queued reaction skipped", "reason", "api_error", "err", err)
		return false
	}
	return true
}

// ackMergedFollower signals that this user message was merged into another
// message's reply (passthrough head/follower fan-out): a reaction when the
// platform supports it, else a short text notice rate-limited via
// ShouldNotify. key must be the resolved session key so the cooldown shares
// the bucket the rest of the dispatch path rate-limits on (#1784).
func (d *Dispatcher) ackMergedFollower(ctx context.Context, msg platform.IncomingMessage, key string, mergedCount int, lg *slog.Logger) {
	if d.ackQueuedWithReaction(ctx, msg, lg) {
		return
	}
	_ = mergedCount // reserved for future reaction variant showing count
	d.replyNotice(ctx, msg, key, "已合并到上一条回复。", lg, "merged")
}

// replyNotice sends text, a notice that is not the turn's answer, to msg's
// chat and reports whether it reached Reply. A single-use reply-token platform
// (WeChat/iLink) gets nothing: the notice would spend the token the turn's
// real answer needs (#2136, #2260, #3003). A non-empty rateKey rate-limits
// the notice via ShouldNotify, consulted after that gate so a skipped notice
// leaves the cooldown bucket alone. Every caller has a row in
// TestNotices_SingleUseTokenGate.
func (d *Dispatcher) replyNotice(ctx context.Context, msg platform.IncomingMessage, rateKey, text string, lg *slog.Logger, site string) bool {
	p := d.platforms[msg.Platform]
	if p == nil {
		return false
	}
	if platform.UsesSingleUseReplyToken(p) {
		useLg := lg
		if useLg == nil {
			useLg = slog.Default()
		}
		useLg.Debug("notice skipped", "reason", "single_use_token", "site", site, "platform", msg.Platform)
		return false
	}
	if rateKey != "" && !d.turns.ShouldNotify(rateKey) {
		return false
	}
	return d.replyText(ctx, msg, text, lg)
}

// clearQueuedReaction removes the "queued" (HOURGLASS) reaction from one
// message whose request was dropped (imOrigin.Dropped); otherwise the
// HOURGLASS lingers until the platform's reaction-cache TTL. Best-effort and
// nil-safe: failures are logged at Debug and swallowed.
func (d *Dispatcher) clearQueuedReaction(ctx context.Context, platformName, messageID string, lg *slog.Logger) {
	if messageID == "" {
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
	rctx, cancel := context.WithTimeout(ctx, reactionAckTimeout)
	defer cancel()
	if err := reactor.RemoveReaction(rctx, messageID, platform.ReactionQueued); err != nil {
		useLg := lg
		if useLg == nil {
			useLg = slog.Default()
		}
		useLg.Debug("remove passthrough queued reaction failed", "msg_id", messageID, "err", err)
	}
}

// clearQueuedReactions removes the "queued" reaction from each message a
// turn answered (imDelivery.Finish). Errors are logged and swallowed — a
// lingering reaction is cosmetic.
func (d *Dispatcher) clearQueuedReactions(ctx context.Context, platformName string, messageIDs []string, lg *slog.Logger) {
	if len(messageIDs) == 0 {
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
	// One shared timeout budget for the whole batch: a stalling IM API cannot
	// drag reactionAckTimeout × N, and the reactions are purely cosmetic.
	rctx, cancel := context.WithTimeout(ctx, reactionAckTimeout)
	defer cancel()
	for _, id := range messageIDs {
		if id == "" {
			continue
		}
		if rctx.Err() != nil {
			// Batch deadline exceeded; stop rather than log N identical failures.
			return
		}
		if err := reactor.RemoveReaction(rctx, id, platform.ReactionQueued); err != nil {
			useLg := lg
			if useLg == nil {
				useLg = slog.Default()
			}
			useLg.Debug("remove queued reaction failed", "msg_id", id, "err", err)
		}
	}
}
