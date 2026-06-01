package cron

import "time"

// tuning.go centralises the cron scheduler's wall-clock / ratio tuning knobs
// so operators have one file to consult instead of grepping the call graph.
// R249-CR-16 (#959): these were previously scattered across scheduler_run.go
// and scheduler_notify.go. Per-Scheduler overrides (where they exist) live on
// SchedulerConfig; the constants below are the compiled-in defaults.
//
// ┌───────────────────────────┬─────────┬──────────────────────────────────────────────┬──────────────────────────────────────────────┐
// │ Constant                  │ Default │ Raise it to…                                   │ Lower it to…                                   │
// ├───────────────────────────┼─────────┼────────────────────────────────────────────────┼────────────────────────────────────────────────┤
// │ defaultCronSlowThreshold  │ 30s     │ silence slow-alerts for legitimately long jobs │ flag more runs as "slow" (earlier inspection)  │
// │   (override: Slow-          │         │ (e.g. ExecTimeout=300s deployments)            │                                                │
// │    Threshold)             │         │                                                │                                                │
// │ spawnElapsedWarnRatio     │ 0.5     │ suppress spawn-budget warns on cold fresh-     │ surface near-doubling of per-run budget earlier│
// │                           │         │ context runs that spawn slowly                 │                                                │
// │ minSendBudget             │ 30s     │ give a single Send round-trip more headroom    │ tighten total wall-clock when spawn ate budget │
// │                           │         │ after a slow spawn                             │                                                │
// │ cronNotifyTimeout         │ 30s     │ allow chattier IM flushes per target           │ bound a stuck webhook tighter (mind stopBudget)│
// └───────────────────────────┴─────────┴────────────────────────────────────────────────┴────────────────────────────────────────────────┘
//
// Notify-path interplay (R249-ARCH-23, #987): a single notifyTarget delivery
// can run up to cronNotifyMaxChunks × limits.PlatformReplyMaxAttempts ×
// per-attempt platformReplyTimeout before cronNotifyTimeout's replyCtx cuts it
// off. cronNotifyTimeout is the OUTER per-target ceiling; PlatformReplyMaxAttempts
// (the retry count) is the INNER budget. Keep cronNotifyTimeout aligned with
// stopBudget (scheduler.go) so a hung reply cannot outlive Stop()'s
// systemd-TimeoutStopSec window.

// defaultCronSlowThreshold is the wall-clock budget beyond which a
// successful cron execution is counted as "slow"
// (metrics.CronExecutionSlowTotal). 30s is picked as an order-of-magnitude
// above a typical interactive agent turn; jobs that regularly tip over are
// candidates for timeout / workflow inspection. R208-OBS1.
//
// R241-ARCH-11 (#519): the threshold is now per-Scheduler-configurable
// via SchedulerConfig.SlowThreshold so deployments running with
// ExecTimeout=300s do not flood operators with a daily slow-alert per
// successful long job. The package const stays as the default — callers
// that omit SlowThreshold (or pass 0/negative) keep the legacy 30s
// behaviour. Production wiring (cmd/naozhi) reads cron.slow_threshold
// from config so operators can raise the threshold without recompiling.
const defaultCronSlowThreshold = 30 * time.Second

// spawnElapsedWarnRatio is the fraction of jobTimeout the spawn phase
// (router.GetOrCreate) is allowed to consume before we emit the
// "send budget exceeds job/2" warning + bump CronSendBudgetDoubledTotal.
//
// 0.5 chosen because once spawn alone has consumed half the per-run
// budget, the in-flight wall clock can reach ~2*jobTimeout (spawn +
// fresh-budget Send), which is the doubling pattern operators of 300s+
// jobs need a runbook signal for. Lower the ratio (e.g. 0.4) to surface
// near-doubling earlier; raise (e.g. 0.7) to suppress noise on cold
// fresh-context runs that legitimately spawn slowly. R247-CR-28.
const spawnElapsedWarnRatio = 0.5

// minSendBudget is the lower bound on the per-run send-phase context budget
// when spawn already consumed most of jobTimeout. R20260527122801-CR-2 (#1311):
// historically sendCtx used the full jobTimeout regardless of how long spawn
// took, so a 5min jobTimeout could yield ~10min wall-clock + jitter in the
// worst case (spawn ~5min then send another ~5min). Operators reported
// systemd TimeoutStopSec being exceeded as a result. We now clamp sendCtx
// to (jobTimeout - time.Since(spawnStart)), bounded below by minSendBudget so
// a flaky cold-start spawn doesn't immediately turn into a "send timed out"
// without operator signal — the historical concern documented at the
// sendCtx assignment in scheduler_run.go. 30s is enough for a single Send
// round-trip on a healthy CLI; the spawnElapsedWarnRatio warn already alerts
// operators when spawn is eating the budget.
const minSendBudget = 30 * time.Second

// cronNotifyTimeout is the per-target send budget for cron-driven IM replies.
// Distinct from dispatch.platformReplyTimeout (15s) because cron flushes can
// chunk large outputs across multiple ReplyWithRetry calls under cron.Stop's
// 30s in-flight budget — see notifyTarget call site for the shutdown contract.
//
// R249-ARCH-23 (#987): this is the OUTER per-target ceiling; the INNER retry
// budget is limits.PlatformReplyMaxAttempts (shared with dispatch). The
// composite worst case for a multi-chunk flush is
// cronNotifyMaxChunks × PlatformReplyMaxAttempts × platformReplyTimeout, which
// replyCtx (bound to cronNotifyTimeout) cuts off mid-flush — see
// cronNotifyMaxChunks and the table at the top of this file.
//
// R245-GO-9 (#851): the per-target 30s budget does NOT extend Stop()'s
// wall-clock past systemd TimeoutStopSec. Stop bounds triggerWG.Wait() with
// stopBudget (default 30s, see scheduler.go ~L978) so a stuck webhook is
// preempted at the budget boundary.
//
// R243-SEC-14 (#799): replyCtx now chains to s.stopCtx (notifyTarget,
// scheduler_notify.go) so a hung webhook short-circuits the moment Stop fires
// instead of waiting for the per-target timer. The constant stays the
// per-target ceiling for normal operation; combined with the chained
// parent, a stuck reply costs at most min(cronNotifyTimeout, time-since-
// stopCancel) wall-clock. Keep at 30s for symmetry with stopBudget; if
// a future review tightens stopBudget, mirror the change here.
const cronNotifyTimeout = 30 * time.Second
