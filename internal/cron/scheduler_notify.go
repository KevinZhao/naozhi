// scheduler_notify.go: IM completion-notice routing for cron runs — the
// NotifyTarget + resolveNotifyTarget priority ladder, deliverNotice and the
// chunked notifyTarget. Methods stay on *Scheduler so the notifySender
// snapshot / s.notifyDefault fields remain accessible without exporting.

package cron

import (
	"context"
	"log/slog"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/replyfmt"
)

// NotifyTarget identifies an IM channel for cron completion notifications.
type NotifyTarget struct {
	Platform string
	ChatID   string
}

// IsSet reports whether both fields are populated.
func (n NotifyTarget) IsSet() bool { return n.Platform != "" && n.ChatID != "" }

// cronNotifyTimeout is defined in tuning.go alongside the other cron tuning
// knobs, which document its relationship to the inner
// limits.PlatformReplyMaxAttempts retry budget and the stopBudget contract.

// cronNotifyMaxChunks bounds how many chunks notifyTarget will deliver from a
// single CronRun result: chunks × PlatformReplyMaxAttempts × per-attempt
// timeout can otherwise exceed cronNotifyTimeout on a slow platform (#568).
// The cap bounds worst-case alloc / slog volume and makes the truncated
// payload a known shape; 5 comfortably covers realistic cron output (a
// 4-page result chunks to ~3-4 messages). The truncation WARN in
// notifyTarget makes the cap visible.
const cronNotifyMaxChunks = 5

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

// deliverNotice sends a result/error message to the resolved target. No-op
// when target is unset, text is empty, or the platform is not registered.
//
// Delivery runs on a goroutine tracked by triggerWG so the cron-tick caller
// never blocks on the IM reply chain (chunk × retry × HTTP); finishRun has
// already stamped the terminal state, so the record is final. Stop() drains
// triggerWG within stopBudget and notifyTarget's replyCtx is parented on
// s.stopCtx, so an in-flight notify is implicitly drained by Stop's wait.
// Add(1) happens BEFORE the `go` launch so a Stop() racing the goroutine's
// first schedule still observes it. Completion is observable via slog only.
func (s *Scheduler) deliverNotice(target NotifyTarget, text string) {
	if !target.IsSet() {
		return
	}
	// Empty text short-circuits BEFORE triggerWG.Add(1): otherwise the
	// goroutine would Split("") → [""] and burn a retry budget on a zero-byte
	// chunk, and a concurrent Stop() would block on triggerWG.Wait until it
	// drained.
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
// The chunk loop aborts on the first Reply failure: once any chunk fails the
// reader's ordering is already broken, so pushing the rest is noise. One
// aggregated WARN ("cron notify partial: K/N chunks delivered") replaces a
// per-chunk stream so operators match one log line to one dropped message
// (#1151).
func (s *Scheduler) notifyTarget(plat, chatID, text string) {
	// Early bail before the Split alloc when stopCtx is already cancelled —
	// deliverNotice's goroutine may be scheduled after Stop fired. The
	// replyCtx.Err() check in the loop stays the authoritative observation.
	if s.stopCtx != nil && s.stopCtx.Err() != nil {
		return
	}
	// Empty text is a no-op at this layer too (notifyTarget is reachable
	// directly): Split("") → [""] would burn a retry budget on a zero-byte
	// chunk (#1116).
	if text == "" {
		return
	}
	sender := s.configMaps().notifySender
	if sender == nil {
		slog.Warn("cron notify: platform not found", "platform", plat)
		return
	}
	r, ok := sender.Lookup(plat)
	if !ok {
		slog.Warn("cron notify: platform not found", "platform", plat)
		return
	}
	// #799: replyCtx is parented on s.stopCtx (not context.Background) so a
	// hung webhook POST short-circuits the moment Scheduler.Stop cancels
	// stopCtx instead of pinning triggerWG.Wait for the full stopBudget. The
	// per-target ceiling stays cronNotifyTimeout; a mid-flush cancel surfaces
	// through the "cron notify partial" WARN like any chunk failure.
	parent := s.stopCtx
	if parent == nil {
		// Only a hand-constructed test fake lacks stopCtx (NewScheduler always
		// sets it); fall back so the per-target timeout still bounds the call.
		parent = context.Background()
	}
	// cronNotifyTimeout is the OUTER per-target ceiling; the INNER retry budget
	// is limits.PlatformReplyMaxAttempts inside each Reply. replyCtx plus the
	// replyCtx.Err() loop check keep the composite from outrunning the
	// deadline (budget table in tuning.go).
	replyCtx, replyCancel := context.WithTimeout(parent, cronNotifyTimeout)
	defer replyCancel()
	// The PlatformReplier adapter owns the DefaultMaxReplyLen fallback and the
	// SplitText delegation, so cron never imports platform (#725).
	maxLen := r.MaxReplyLength()
	// Single-use-token platforms (e.g. WeChat iLink) deliver only ONE message
	// per inbound turn; fanning into N chunks would silently lose 2..N.
	// Collapse to one rune-safe-truncated message with a visible "…(truncated)"
	// marker and return before the chunk loop (#2181).
	if r.UsesSingleUseReplyToken() {
		if utf8.RuneCountInString(text) > maxLen {
			text = replyfmt.TruncateForSingleReply(text, maxLen)
		}
		if _, err := r.Reply(replyCtx, chatID, text); err != nil {
			slog.Warn("cron notify: single-use reply send failed",
				"platform", plat, "chat", osutil.SanitizeForLog(chatID, 64), "err", err) // chatID is attacker-influenced
		}
		return
	}
	// Reserve room for the "\n— [i/N]" page suffix BEFORE splitting, the same way
	// the IM reply path does. cron used to split at the raw limit and append
	// nothing: a notification spread over four messages arrived with no "[2/4]",
	// so a recipient could not tell whether they had the whole thing (J2 #2548).
	// Reserving after the split would push full chunks past hard API ceilings
	// (#2008), which is why replyfmt computes the width in two passes.
	splitLen, suppressSuffix := replyfmt.ReserveForPageSuffix(maxLen, utf8.RuneCountInString(text))
	chunks := r.Split(text, splitLen)
	// Cap the chunk count before the loop (#568) and surface the truncation in
	// slog so operators see the dropped tail.
	totalChunks := len(chunks)
	dropped := 0
	if totalChunks > cronNotifyMaxChunks {
		dropped = totalChunks - cronNotifyMaxChunks
		chunks = chunks[:cronNotifyMaxChunks]
		slog.Warn("cron notify: chunk count exceeds cap; tail dropped",
			"platform", plat, "chat", osutil.SanitizeForLog(chatID, 64), // chatID is attacker-influenced
			"total", totalChunks, "cap", cronNotifyMaxChunks,
			"dropped", dropped)
	}
	delivered := 0
	for i, chunk := range chunks {
		// Short-circuit on the shared replyCtx deadline so a long chunk list
		// cannot run past cronNotifyTimeout when each Reply consumes budget.
		if err := replyCtx.Err(); err != nil {
			// Fold the cap-dropped tail into "remaining" so the WARN reports
			// the full undelivered tail, not just the post-cap subset.
			slog.Warn("cron notify target deadline reached; remaining chunks dropped",
				"platform", plat, "chat", osutil.SanitizeForLog(chatID, 64), "err", err, // chatID is attacker-influenced
				"sent", delivered, "remaining", len(chunks)-i+dropped)
			return
		}
		// Page suffix on the ORIGINAL chunk count, so a cap-truncated notify still
		// says "[2/9]" rather than renumbering to the delivered subset — the
		// recipient needs to know a tail is missing.
		if totalChunks > 1 && !suppressSuffix {
			chunk += replyfmt.PageSuffix(i+1, totalChunks)
		}
		// r.Reply passes replyCtx through unchanged (#725), so the #799 stopCtx
		// chain still short-circuits a hung webhook on Stop.
		if _, err := r.Reply(replyCtx, chatID, chunk); err != nil {
			// Abort on first chunk failure (#1151).
			// Report the ORIGINAL chunk count (len(chunks)+dropped) as total so
			// a cap-truncated message that then fails does not under-report.
			slog.Warn("cron notify partial: chunks dropped after send failure",
				"platform", plat, "chat", osutil.SanitizeForLog(chatID, 64), "err", err, // chatID is attacker-influenced
				"delivered", delivered, "total", len(chunks)+dropped,
				"failed_index", i)
			return
		}
		delivered++
	}
}
