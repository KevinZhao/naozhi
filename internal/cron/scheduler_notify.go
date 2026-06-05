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

	"github.com/naozhi/naozhi/internal/metrics"
)

// NotifyTarget identifies an IM channel for cron completion notifications.
type NotifyTarget struct {
	Platform string
	ChatID   string
}

// IsSet reports whether both fields are populated.
func (n NotifyTarget) IsSet() bool { return n.Platform != "" && n.ChatID != "" }

// NotifySource enumerates which branch of resolveNotifyDecision selected
// the target. R241-ARCH-12 (#520): the 5-branch decision tree was
// inline-opaque — callers (dashboard, debug logging) could not inspect
// why a particular target was selected. Returning the source alongside
// the target lets diagnostic surfaces explain the resolution without
// duplicating the priority ladder logic.
type NotifySource int

const (
	// NotifySourceNone — no target selected. Either notify==false (explicit
	// disable), notify==true with no default configured, or notify==nil
	// with a non-IM platName ("dashboard") / empty fields.
	NotifySourceNone NotifySource = iota

	// NotifySourceExplicitDisable — notify==false short-circuited above
	// every other branch.
	NotifySourceExplicitDisable

	// NotifySourcePerJobOverride — both NotifyPlatform and NotifyChatID
	// are set on the job; this overrides any default and any source-chat
	// fallback regardless of notify tristate.
	NotifySourcePerJobOverride

	// NotifySourceDefault — notify==true selected the scheduler-wide
	// notify_default target.
	NotifySourceDefault

	// NotifySourceDefaultMissing — notify==true but no default configured;
	// no target produced and a Warn was emitted (caller must NOT log it
	// twice).
	NotifySourceDefaultMissing

	// NotifySourceLegacySourceChat — notify==nil (unset) and platName/chatID
	// are non-empty IM coords; legacy behaviour: reply to source chat.
	NotifySourceLegacySourceChat

	// NotifySourceDashboardSilent — notify==nil and platName=="dashboard";
	// dashboard-created jobs stay silent unless an explicit target is set.
	NotifySourceDashboardSilent
)

// String returns a stable lower_snake identifier suitable for slog keys
// and dashboard tooltips. Stable across versions — dashboards may match
// on these strings.
func (s NotifySource) String() string {
	switch s {
	case NotifySourceExplicitDisable:
		return "explicit_disable"
	case NotifySourcePerJobOverride:
		return "per_job_override"
	case NotifySourceDefault:
		return "default"
	case NotifySourceDefaultMissing:
		return "default_missing"
	case NotifySourceLegacySourceChat:
		return "legacy_source_chat"
	case NotifySourceDashboardSilent:
		return "dashboard_silent"
	default:
		return "none"
	}
}

// NotifyDecision pairs the resolved NotifyTarget with the source branch
// that produced it. R241-ARCH-12 (#520).
type NotifyDecision struct {
	Target NotifyTarget
	Source NotifySource
}

// cronNotifyTimeout is defined in tuning.go (R249-CR-16 #959 / R249-ARCH-23
// #987), which documents its relationship to the inner PlatformReplyMaxAttempts
// retry budget and the stopBudget shutdown contract alongside the other cron
// tuning knobs.

// The per-call retry budget for platform.ReplyWithRetry now lives on
// limits.PlatformReplyMaxAttempts (R20260527-ARCH-8) so cron's
// notifyTarget and dispatch's reply paths share a single source of
// truth instead of mirrored "KEEP-IN-SYNC" copies.

// cronNotifyMaxChunks bounds how many chunks notifyTarget will attempt
// to deliver from a single CronRun result. R236-SEC-15 (#568): the
// composite worst case is chunks × PlatformReplyMaxAttempts × per-attempt
// platformReplyTimeout, which can exceed cronNotifyTimeout (30s) when
// a chatty job emits many small chunks under a slow platform. The
// existing replyCtx.Err() check inside the loop already cuts off mid-
// flush on deadline, but a hard chunk cap (1) bounds the worst-case
// alloc / per-chunk slog volume on success and (2) makes the eventual
// truncated payload a known shape rather than "whatever fit in 30s".
//
// 5 was picked as the smallest value that comfortably covers realistic
// cron output (a single 4-page result at platform.DefaultMaxReplyLen
// chunks to ~3-4 messages on Feishu/iOS). Operators with chronically
// long results should lean on the dashboard run-detail panel rather
// than IM as the surface of record; the truncation WARN below makes
// the cap visible so it doesn't silently drop output.
const cronNotifyMaxChunks = 5

// resolveNotifyTarget picks the IM destination for this execution's
// completion notice. Priority:
//  1. Per-job NotifyPlatform/NotifyChatID (always honored when both set).
//  2. notify==true + scheduler default target.
//  3. notify==false disables delivery even for IM-created jobs.
//  4. notify==nil (unset) preserves legacy behavior: IM-created jobs reply
//     to their own source chat; dashboard-created jobs stay silent.
//
// Thin wrapper around resolveNotifyDecision; preserved as the historical
// caller surface so existing call sites remain a single map lookup. New
// callers wanting to inspect *which branch* selected the target should
// call resolveNotifyDecision directly. R241-ARCH-12 (#520).
func (s *Scheduler) resolveNotifyTarget(platName, chatID, notifyPlat, notifyChat string, notify *bool) NotifyTarget {
	return s.resolveNotifyDecision(platName, chatID, notifyPlat, notifyChat, notify).Target
}

// resolveNotifyDecision exposes both the chosen NotifyTarget and the
// branch (NotifySource) that selected it. The 5-branch decision tree
// was previously inline-opaque (R241-ARCH-12, #520); callers that want
// to debug "why did this run go silent / fan out to dashboard" can now
// log decision.Source rather than recomputing the priority ladder.
//
// Behaviour mirrors the previous resolveNotifyTarget exactly — including
// the slog.Warn for "enabled but no target", which still fires only on
// the NotifySourceDefaultMissing branch so the warning frequency does
// not change. Callers MUST NOT re-emit a warning when they observe
// NotifySourceDefaultMissing.
func (s *Scheduler) resolveNotifyDecision(platName, chatID, notifyPlat, notifyChat string, notify *bool) NotifyDecision {
	// Explicit disable wins over everything.
	if notify != nil && !*notify {
		return NotifyDecision{Source: NotifySourceExplicitDisable}
	}

	// Per-job override always wins when fully specified.
	if notifyPlat != "" && notifyChat != "" {
		return NotifyDecision{
			Target: NotifyTarget{Platform: notifyPlat, ChatID: notifyChat},
			Source: NotifySourcePerJobOverride,
		}
	}

	// Explicit enable: fall back to scheduler default.
	if notify != nil && *notify {
		if s.notifyDefault.IsSet() {
			return NotifyDecision{Target: s.notifyDefault, Source: NotifySourceDefault}
		}
		// Enabled but no target anywhere — log once per run so users notice
		// misconfiguration instead of silently dropping notifications.
		slog.Warn("cron notify enabled but no target configured",
			"hint", "set cron.notify_default.platform + chat_id, or provide per-job notify_platform + notify_chat_id")
		return NotifyDecision{Source: NotifySourceDefaultMissing}
	}

	// Legacy default (notify==nil): IM-created jobs reply to their source chat.
	// Dashboard-created jobs have platName="dashboard" which is never a
	// registered IM platform — short-circuit here so notifyTarget doesn't
	// fire a per-tick "platform not found" WARN for every dashboard job.
	if platName == "dashboard" {
		return NotifyDecision{Source: NotifySourceDashboardSilent}
	}
	if platName != "" && chatID != "" {
		return NotifyDecision{
			Target: NotifyTarget{Platform: platName, ChatID: chatID},
			Source: NotifySourceLegacySourceChat,
		}
	}
	return NotifyDecision{Source: NotifySourceNone}
}

// deliverNotice sends a result/error message to the resolved target.
// No-op when target is unset or the platform is not registered.
//
// R242-GO-14 (#575): caller is the cron-tick goroutine (or
// freshContextPreflightP0's error path); we Add(1) to triggerWG and spawn
// notifyTarget on a child goroutine. Stop() drains triggerWG with the
// stopBudget (~30s, see scheduler.go Stop CONTRACT) and notifyTarget's
// replyCtx is parented on s.stopCtx, so an in-flight notify is implicitly
// "drained" by Stop's wait — there is no separate notify-drain channel.
// The cron-tick goroutine itself does NOT block on this call (we return
// once the goroutine is spawned), but it DOES contribute to the same
// triggerWG that bounds shutdown. Callers must not assume completion-by-
// return; observability lives in slog only.
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
	// limits.PlatformReplyMaxAttempts retry budget on a zero-byte chunk. The
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
	// R20260527122801-GO-014: short-circuit before SplitText alloc when
	// stopCtx already cancelled — the parent goroutine may have been
	// scheduled before Stop fired, so deliverNotice's triggerWG.Add(1)
	// can land on a dead Scheduler and pay for SplitText's chunk walk
	// of a long result before the existing replyCtx.Err() check at
	// line ~188 catches it. The lower-level guard stays as the
	// authoritative cancel observation; this is an early bail for
	// the alloc.
	if s.stopCtx != nil && s.stopCtx.Err() != nil {
		return
	}
	// R20260527-PERF-1 (#1116): empty text is a no-op. deliverNotice already
	// guards its async path, but notifyTarget is also reachable directly
	// (in-package call sites + future callers) where platform.SplitText("",
	// maxLen) returns [""] and the loop below would burn one
	// limits.PlatformReplyMaxAttempts retry budget pushing a zero-byte chunk.
	// Short-circuit here so the empty-text contract holds at this layer too,
	// independent of how the caller reached us.
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
	// R249-ARCH-23 (#987): cronNotifyTimeout is the OUTER per-target ceiling.
	// The INNER retry budget is limits.PlatformReplyMaxAttempts (inside each
	// ReplyWithRetry below), shared with dispatch. The composite worst case for
	// a multi-chunk flush is cronNotifyMaxChunks × PlatformReplyMaxAttempts ×
	// per-attempt platformReplyTimeout; replyCtx (bound here) and the
	// replyCtx.Err() check in the loop cut it off so it cannot outrun the
	// per-target deadline. See the budget table at the top of tuning.go.
	replyCtx, replyCancel := context.WithTimeout(parent, cronNotifyTimeout)
	defer replyCancel()
	// #725: the PlatformReplier adapter owns the platform.DefaultMaxReplyLen
	// fallback (MaxReplyLength returns the default when the platform reports
	// <=0) and the SplitText delegation, so cron no longer imports platform.
	maxLen := r.MaxReplyLength()
	chunks := r.Split(text, maxLen)
	// R236-SEC-15 (#568): cap the chunk count before the loop. The
	// composite chunks × retries × per-attempt budget can otherwise
	// exceed cronNotifyTimeout when a chatty job lands on a slow
	// platform; capping makes the worst case bounded and surfaces the
	// truncation in slog so operators see the dropped tail.
	totalChunks := len(chunks)
	dropped := 0
	if totalChunks > cronNotifyMaxChunks {
		dropped = totalChunks - cronNotifyMaxChunks
		chunks = chunks[:cronNotifyMaxChunks]
		slog.Warn("cron notify: chunk count exceeds cap; tail dropped",
			"platform", plat, "chat", chatID,
			"total", totalChunks, "cap", cronNotifyMaxChunks,
			"dropped", dropped)
	}
	delivered := 0
	for i, chunk := range chunks {
		// R235-GO-5: short-circuit on the shared replyCtx deadline so a long
		// chunk list cannot run past cronNotifyTimeout when each ReplyWithRetry
		// (limits.PlatformReplyMaxAttempts × per-attempt budget) consumes the budget mid-loop.
		if err := replyCtx.Err(); err != nil {
			// R249-CR-26 (#966): record partial delivery so a rising delta
			// surfaces "IM recipients are seeing truncated cron output".
			metrics.CronNotifyPartialTotal.Add(1)
			// R236-SEC-15 follow-up: fold the cap-dropped tail (`dropped`)
			// into the remaining count. `chunks` was already truncated to
			// cronNotifyMaxChunks above, so `len(chunks)-i` alone would
			// undercount what the recipient never saw — operators reading
			// this WARN must see the full undelivered tail.
			slog.Warn("cron notify target deadline reached; remaining chunks dropped",
				"platform", plat, "chat", chatID, "err", err,
				"sent", delivered, "remaining", len(chunks)-i+dropped)
			return
		}
		// #725: r.Reply passes replyCtx through unchanged to
		// platform.ReplyWithRetry (with limits.PlatformReplyMaxAttempts),
		// so the R243-SEC-14 (#799) stopCtx parent chain still short-circuits
		// a hung webhook the moment Scheduler.Stop cancels stopCtx.
		if _, err := r.Reply(replyCtx, chatID, chunk); err != nil {
			// R250-CR-18: abort on first chunk failure. Subsequent sends
			// would interleave with foreign messages the user receives in
			// the meantime, so partial delivery is worse than a clean
			// truncation. Aggregate the count into a single WARN so log
			// readers can match one line to one dropped notify.
			//
			// R249-CR-26 (#966): same partial-delivery counter as the
			// deadline branch above — operators alert on the aggregate
			// rather than distinguishing deadline-vs-failure (the WARN
			// carries that detail for the journalctl drill-down).
			metrics.CronNotifyPartialTotal.Add(1)
			// R236-SEC-15 follow-up: report the ORIGINAL chunk count
			// (len(chunks)+dropped) as total so the WARN reflects every
			// chunk the recipient was supposed to receive, not just the
			// post-cap subset. Without this a cap-truncated message that
			// then fails mid-send under-reports the true loss.
			slog.Warn("cron notify partial: chunks dropped after send failure",
				"platform", plat, "chat", chatID, "err", err,
				"delivered", delivered, "total", len(chunks)+dropped,
				"failed_index", i)
			return
		}
		delivered++
	}
}
