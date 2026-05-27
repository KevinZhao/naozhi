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
// preempted at the budget boundary.
//
// R243-SEC-14 (#799): replyCtx now chains to s.stopCtx (notifyTarget,
// this file) so a hung webhook short-circuits the moment Stop fires
// instead of waiting for the per-target timer. The constant stays the
// per-target ceiling for normal operation; combined with the chained
// parent, a stuck reply costs at most min(cronNotifyTimeout, time-since-
// stopCancel) wall-clock. Keep at 30s for symmetry with stopBudget; if
// a future review tightens stopBudget, mirror the change here.
const cronNotifyTimeout = 30 * time.Second

// platformReplyMaxAttempts mirrors dispatch.platformReplyMaxAttempts. Both
// represent the same per-call retry budget for platform.ReplyWithRetry —
// dispatch's chunk-loop and cron's notifyTarget share the IM platform
// envelope, so the two values must move together. Kept as a local mirror
// (rather than re-exporting from dispatch) to avoid pulling internal/dispatch
// into the cron import graph just for one int. R20260526-CR-003.
//
// KEEP-IN-SYNC: if you bump dispatch.platformReplyMaxAttempts (currently 3),
// bump this too. Conversely a future review that promotes either side to a
// platform-package export should collapse both call sites onto it.
const platformReplyMaxAttempts = 3

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
	// Dashboard-created jobs have platName="dashboard" which is never a
	// registered IM platform — short-circuit here so notifyTarget doesn't
	// fire a per-tick "platform not found" WARN for every dashboard job.
	if platName == "dashboard" {
		return NotifyTarget{}
	}
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
	// R20260526-CR-017: empty text is a no-op — short-circuit before
	// triggerWG.Add so an empty notice does not spawn a goroutine that
	// then walks platform.SplitText("", maxLen) → [""] and consumes one
	// platformReplyMaxAttempts retry budget on a zero-byte chunk. The
	// empty-text path is reachable when a non-failing run produced no
	// IM-visible output (e.g. a job that wrote only to disk and an
	// upstream caller still routed the empty Result through).
	// The early return MUST land before triggerWG.Add(1); otherwise a
	// concurrent Stop() observing the just-incremented counter would
	// block on triggerWG.Wait until the empty-send goroutine drains.
	if text == "" {
		return
	}
	s.triggerWG.Add(1)
	go func() {
		defer s.triggerWG.Done()
		s.notifyTarget(target.Platform, target.ChatID, text)
	}()
}

// notifyTarget sends a message to an arbitrary platform/chat (notify target).
//
// R250-CR-18 (#1151): aborts the chunk loop on the first ReplyWithRetry
// failure rather than continuing to push subsequent chunks. Once any chunk
// fails the user's reading order is already broken (they would see
// chunk[0]+chunk[3]+chunk[4] interleaved with whatever else lands in the
// channel between retries), so finishing the message is just adding noise.
// A single aggregated WARN ("cron notify partial: K/N chunks delivered")
// replaces the prior "one WARN per failed chunk" stream so operators can
// match a single log line to a single dropped message instead of having
// to reconstruct chunk boundaries from N independent warnings.
func (s *Scheduler) notifyTarget(plat, chatID, text string) {
	p := s.platforms[plat]
	if p == nil {
		slog.Warn("cron notify: platform not found", "platform", plat)
		return
	}
	// R243-SEC-14 (#799): replyCtx chains to s.stopCtx so a hung webhook
	// POST short-circuits the moment Scheduler.Stop() cancels stopCtx —
	// previously parented on Background, which left triggerWG.Wait pinned
	// at the full stopBudget (30s) waiting for the per-target timer to
	// expire even though the operator had already signalled shutdown.
	// The per-target ceiling stays at cronNotifyTimeout so a slow-but-
	// progressing chunk-flush during normal operation is unchanged; only
	// the shutdown path observes the new short-circuit. Cancelled mid-
	// flush appears to ReplyWithRetry as a context error and the existing
	// "cron notify partial" WARN aggregator records the partial delivery
	// — same observability shape as a chunk-failure mid-stream.
	parent := s.stopCtx
	if parent == nil {
		// Defensive: a Scheduler that was never NewScheduler'd (e.g. a
		// hand-constructed test fake) won't have stopCtx wired. Fall back
		// to Background so the per-target timeout still bounds the call;
		// production paths (NewScheduler) always set stopCtx so the
		// fallback is dead code in normal operation but keeps the package
		// usable from narrow unit tests.
		parent = context.Background()
	}
	replyCtx, replyCancel := context.WithTimeout(parent, cronNotifyTimeout)
	defer replyCancel()
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = platform.DefaultMaxReplyLen
	}
	chunks := platform.SplitText(text, maxLen)
	delivered := 0
	for i, chunk := range chunks {
		// R235-GO-5: short-circuit on the shared replyCtx deadline so a long
		// chunk list cannot run past cronNotifyTimeout when each ReplyWithRetry
		// (platformReplyMaxAttempts × per-attempt budget) consumes the budget mid-loop.
		if err := replyCtx.Err(); err != nil {
			slog.Warn("cron notify target deadline reached; remaining chunks dropped",
				"platform", plat, "chat", chatID, "err", err,
				"sent", delivered, "remaining", len(chunks)-i)
			return
		}
		if _, err := platform.ReplyWithRetry(replyCtx, p, platform.OutgoingMessage{
			ChatID: chatID,
			Text:   chunk,
		}, platformReplyMaxAttempts); err != nil {
			// R250-CR-18: abort on first chunk failure. Subsequent sends
			// would interleave with foreign messages the user receives in
			// the meantime, so partial delivery is worse than a clean
			// truncation. Aggregate the count into a single WARN so log
			// readers can match one line to one dropped notify.
			slog.Warn("cron notify partial: chunks dropped after send failure",
				"platform", plat, "chat", chatID, "err", err,
				"delivered", delivered, "total", len(chunks),
				"failed_index", i)
			return
		}
		delivered++
	}
}
