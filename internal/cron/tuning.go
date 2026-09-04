package cron

import "time"

// tuning.go centralises the cron scheduler's wall-clock / ratio tuning knobs.
// Per-Scheduler overrides (where they exist) live on SchedulerConfig; the
// constants below are the compiled-in defaults.
//
// Notify-path interplay (#987): one notifyTarget delivery can run up to
// cronNotifyMaxChunks × limits.PlatformReplyMaxAttempts × platformReplyTimeout
// before cronNotifyTimeout's replyCtx cuts it off (OUTER ceiling vs INNER
// retry budget). Keep cronNotifyTimeout aligned with stopBudget so a hung
// reply cannot outlive Stop()'s systemd TimeoutStopSec window.

// defaultCronSlowThreshold is the wall-clock budget beyond which a successful
// cron execution is counted as "slow" (metrics.CronExecutionSlowTotal): an
// order of magnitude above a typical interactive agent turn. Overridable per
// Scheduler via SchedulerConfig.SlowThreshold so ExecTimeout=300s deployments
// are not flooded with slow-alerts (#519).
const defaultCronSlowThreshold = 30 * time.Second

// spawnElapsedWarnRatio is the fraction of jobTimeout the spawn phase
// (router.GetOrCreate) may consume before the "send budget exceeds job/2"
// warning + CronSendBudgetDoubledTotal bump. At 0.5 the in-flight wall clock
// can reach ~2*jobTimeout (spawn + fresh-budget Send) — the doubling pattern
// operators of 300s+ jobs need a runbook signal for. Lower to surface it
// earlier; raise to suppress noise on cold fresh-context spawns.
const spawnElapsedWarnRatio = 0.5

// minSendBudget is the lower bound on the send-phase context budget once
// spawn has eaten most of jobTimeout: sendCtx is clamped to
// (jobTimeout - time.Since(spawnStart)) so a run cannot take ~2×jobTimeout
// wall-clock, but never below this floor so a slow cold-start spawn does not
// turn straight into "send timed out" (#1311). 30s covers one Send round-trip
// on a healthy CLI; the spawnElapsedWarnRatio warn already flags slow spawns.
const minSendBudget = 30 * time.Second

// cronNotifyTimeout is the per-target send budget for cron-driven IM replies:
// the OUTER ceiling around a chunked flush (cronNotifyMaxChunks ×
// limits.PlatformReplyMaxAttempts × platformReplyTimeout, #987), distinct from
// dispatch.platformReplyTimeout (15s) because cron flushes chunk large
// outputs. It does not extend Stop() past systemd TimeoutStopSec: stopBudget
// bounds triggerWG.Wait (#851) and replyCtx chains to s.stopCtx so a hung
// webhook short-circuits when Stop fires (#799). Keep at 30s for symmetry
// with stopBudget; if stopBudget is tightened, mirror the change here.
const cronNotifyTimeout = 30 * time.Second

// sandboxStopTimeout bounds a single sandbox StopSession (microVM teardown)
// network call during pending reconcile / delete-stop / replay. 30s mirrors
// cronNotifyTimeout / stopBudget so a hung Stop cannot outlive Stop()'s
// systemd TimeoutStopSec window.
const sandboxStopTimeout = 30 * time.Second

// sandboxReconcileWorkers caps the concurrent orphan-Stop fan-out in
// reconcileSandboxPending (#2142): each orphan's StopSession is an
// independent ~30s network call, so serial N×30s could stall the startup
// reconcile pass for minutes, while an unbounded fan-out could open too many
// in-flight Stops at once. Caps peak concurrency only, not correctness.
const sandboxReconcileWorkers = 4
