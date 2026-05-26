// scheduler_notify.go: IM completion-notice routing for cron runs.
//
// Split out of scheduler.go to keep the dispatch surface (NotifyTarget +
// resolveNotifyTarget priority ladder + deliverNotice + chunked notifyTarget)
// in one place. No behaviour change. Methods stay on *Scheduler so the
// s.platforms / s.notifyDefault fields remain accessible without exporting.

package cron

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// NotifyTarget identifies an IM channel for cron completion notifications.
type NotifyTarget struct {
	Platform string
	ChatID   string
}

// IsSet reports whether both fields are populated.
func (n NotifyTarget) IsSet() bool { return n.Platform != "" && n.ChatID != "" }

// cronNotifyTimeout is the per-target send budget for cron-driven IM replies.
// Distinct from dispatch.platformReplyTimeout (15s) because cron flushes can
// chunk large outputs across multiple ReplyWithRetry calls under cron.Stop's
// 30s in-flight budget — see notifyTarget call site for the shutdown contract.
//
// R245-GO-9 (#851): the per-target 30s budget does NOT extend Stop()'s
// wall-clock past systemd TimeoutStopSec. Stop bounds triggerWG.Wait() with
// stopBudget (default 30s, see scheduler.go ~L978) so a stuck webhook is
// preempted at the budget boundary even though replyCtx itself is rooted
// in context.Background(). Tightening this constant to 5s — the original
// proposal — would risk cutting legitimate large-chunk flushes mid-stream;
// the stopBudget gate is the actual hazard mitigation, this constant is
// only the per-target ceiling. Keep both 30s for symmetry; if a future
// review tightens stopBudget, mirror the change here.
const cronNotifyTimeout = 30 * time.Second

// resolveNotifyTarget picks the IM destination for this execution's
// completion notice. Priority:
//  1. Per-job NotifyPlatform/NotifyChatID (always honored when both set).
//  2. notify==true + scheduler default target.
//  3. notify==false disables delivery even for IM-created jobs.
//  4. notify==nil (unset) preserves legacy behavior: IM-created jobs reply
//     to their own source chat; dashboard-created jobs stay silent.
func (s *Scheduler) resolveNotifyTarget(platName, chatID, notifyPlat, notifyChat string, notify *bool) NotifyTarget {
	// Explicit disable wins over everything.
	if notify != nil && !*notify {
		return NotifyTarget{}
	}

	// Per-job override always wins when fully specified.
	if notifyPlat != "" && notifyChat != "" {
		return NotifyTarget{Platform: notifyPlat, ChatID: notifyChat}
	}

	// Explicit enable: fall back to scheduler default.
	if notify != nil && *notify {
		if s.notifyDefault.IsSet() {
			return s.notifyDefault
		}
		// Enabled but no target anywhere — log once per run so users notice
		// misconfiguration instead of silently dropping notifications.
		slog.Warn("cron notify enabled but no target configured",
			"hint", "set cron.notify_default.platform + chat_id, or provide per-job notify_platform + notify_chat_id")
		return NotifyTarget{}
	}

	// Legacy default (notify==nil): IM-created jobs reply to their source chat.
	// Platform "dashboard" has no registered platform object so this naturally
	// no-ops for dashboard jobs that predate the toggle.
	if platName != "" && chatID != "" {
		return NotifyTarget{Platform: platName, ChatID: chatID}
	}
	return NotifyTarget{}
}

// deliverNotice sends a result/error message to the resolved target.
// No-op when target is unset or the platform is not registered.
//
// R242-GO-13: delivery is dispatched on a goroutine tracked by triggerWG.
// Previously synchronous: the cron-tick callback (or freshContextPreflightP0
// error path) blocked on the IM reply chain (chunk × retry × per-call HTTP),
// extending the run's wall-clock by up to cronNotifyTimeout (30s) before
// the next tick / preflight could proceed. finishRun has already stamped
// the terminal state by the time we reach this call, so the operator-
// facing record is final — the only thing the caller is waiting for is
// the network. Stop() drains triggerWG within the same stopBudget that
// previously bounded the synchronous path, so shutdown latency is
// unchanged.
//
// Add(1) is performed BEFORE the `go` launch so a Stop() landing between
// here and the goroutine's first scheduling tick still observes the
// in-flight delivery and waits for it (matching the contract documented
// in scheduler.go's Stop CONTRACT block — every triggerWG.Add must
// pair with a `defer s.triggerWG.Done()` on its own goroutine).
func (s *Scheduler) deliverNotice(target NotifyTarget, text string) {
	if !target.IsSet() {
		return
	}
	s.triggerWG.Add(1)
	go func() {
		defer s.triggerWG.Done()
		s.notifyTarget(target.Platform, target.ChatID, text)
	}()
}

// notifyTarget sends a message to an arbitrary platform/chat (notify target).
func (s *Scheduler) notifyTarget(plat, chatID, text string) {
	p := s.platforms[plat]
	if p == nil {
		slog.Warn("cron notify: platform not found", "platform", plat)
		return
	}
	// Use Background parent: during shutdown stopCtx is cancelled first, then
	// cron.Stop() waits for in-flight jobs — those must still be able to deliver
	// their IM replies within the 30s bound rather than fail instantly.
	replyCtx, replyCancel := context.WithTimeout(context.Background(), cronNotifyTimeout)
	defer replyCancel()
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = platform.DefaultMaxReplyLen
	}
	chunks := platform.SplitText(text, maxLen)
	for i, chunk := range chunks {
		// R235-GO-5: short-circuit on the shared replyCtx deadline so a long
		// chunk list cannot run past cronNotifyTimeout when each ReplyWithRetry
		// (3 attempts × per-attempt budget) consumes the budget mid-loop.
		if err := replyCtx.Err(); err != nil {
			slog.Warn("cron notify target deadline reached; remaining chunks dropped",
				"platform", plat, "chat", chatID, "err", err,
				"sent", i, "remaining", len(chunks)-i)
			return
		}
		if _, err := platform.ReplyWithRetry(replyCtx, p, platform.OutgoingMessage{
			ChatID: chatID,
			Text:   chunk,
		}, 3); err != nil {
			slog.Warn("cron notify target failed", "platform", plat, "chat", chatID, "err", err)
		}
	}
}
