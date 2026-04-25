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
func (d *Dispatcher) ackQueuedWithReaction(ctx context.Context, msg platform.IncomingMessage, log *slog.Logger) bool {
	if msg.MessageID == "" {
		return false
	}
	p := d.platforms[msg.Platform]
	if p == nil {
		return false
	}
	reactor, ok := platform.AsReactor(p)
	if !ok {
		return false
	}
	// Derive a bounded context so a stalled reaction API can't hold up the
	// webhook handler; the parent ctx still cancels on shutdown.
	rctx, cancel := context.WithTimeout(ctx, reactionAckTimeout)
	defer cancel()
	if err := reactor.AddReaction(rctx, msg.MessageID, platform.ReactionQueued); err != nil {
		if log != nil {
			log.Debug("add queued reaction failed, falling back to text", "err", err)
		}
		return false
	}
	return true
}

// clearQueuedReactions removes the "queued" reaction from each drained
// message. Called from ownerLoop after a drain-batch has been processed.
// Errors are logged and swallowed — a lingering reaction is cosmetically
// unfortunate but not user-blocking, and retrying here would require more
// state without meaningful gain.
func (d *Dispatcher) clearQueuedReactions(ctx context.Context, platformName string, queued []QueuedMsg, log *slog.Logger) {
	if len(queued) == 0 {
		return
	}
	p := d.platforms[platformName]
	if p == nil {
		return
	}
	reactor, ok := platform.AsReactor(p)
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
			if log != nil {
				log.Debug("remove queued reaction failed", "msg_id", m.MessageID, "err", err)
			} else {
				slog.Debug("remove queued reaction failed", "msg_id", m.MessageID, "err", err)
			}
		}
	}
}
