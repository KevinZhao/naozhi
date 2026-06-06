package cron

// scheduler_budget.go holds the Stop-budget constants split out of
// scheduler.go (move-only, #1282): StopPolicyBudgetThenLeak, defaultStopBudget,
// gcWaitBudget, and the R20260603150052-GO-2 note on the removed package var.
// No behaviour changed — relocated verbatim. Scheduler.Stop() and its godoc
// stay in scheduler.go (pinned by the stop-orphan-godoc anchor tests).

import "time"

// StopPolicy is the documented Stop-overflow strategy this Scheduler
// honours: when the per-call wait budget elapses with goroutines still
// in flight, Stop logs a warning and proceeds to the final persist,
// leaving any orphaned goroutines for the OS to reap on process exit.
//
// Why this is a string constant rather than a typed enum: cron and
// sysession independently document their Stop-overflow strategies
// (sysession uses StopPolicyForceExit — see
// internal/sysession/manager.go) and the divergence is a deliberate
// security decision (Sec-LOW-2: sysession daemons run user-prompt-
// derived strings through a CLI subprocess, so a stuck goroutine
// touching a torn-down router could echo conversation excerpts back
// to a different session's reply path; cron deliveries do not have
// that surface). Mechanically unifying the two via a shared enum
// would invite the wrong "harmonise the strategies" intuition. Each
// package exposes its own string constant operators can grep.
//
// Closes #1060 (R244-ARCH-7) — promotes the implicit decision (live
// only in comments inside Stop's godoc + R49-REL-CRON-STOP-BUDGET
// linkage) to a typed constant operators can reference in alerts /
// runbooks. NOT used in cron's control flow today; intentionally
// doc-only so future "let's check policy at runtime" callers must
// add the comparison and its tests deliberately.
const StopPolicyBudgetThenLeak = "budget_then_leak"

// defaultStopBudget is the production overall deadline Scheduler.Stop()
// will spend waiting on cron.Stop + triggerWG before proceeding to save.
// Shared between both waits (not doubled per wait) so a production
// deployment with execTimeout=3600s cannot pin restart for ≈2 h — the
// prior two-budget design had a worst case of 2×(execTimeout+5s).
// Aligned with session.ShutdownTimeout (30s) so both subsystems agree on
// the upper bound systemd sees. R49-REL-CRON-STOP-BUDGET.
const defaultStopBudget = 30 * time.Second

// gcWaitBudget bounds the cold-start GC goroutine wait in Stop(). Smaller
// than defaultStopBudget because trimAll's IO is short-lived
// (ReadDir + N Removes); a wedge here means a stuck filesystem and we'd
// rather skip the wait than pin systemd TimeoutStopSec.
//
// R247-CR-18: kept as a const because no production / test path needs to
// shorten it. If you find yourself wanting to override per-test, use a
// `*time.Timer` injected via a Scheduler field instead of reintroducing
// a package-level var — package vars under t.Parallel races silently.
const gcWaitBudget = 5 * time.Second

// R20260603150052-GO-2 (#1712): the package-level `var stopBudget`
// (formerly the active budget for Scheduler.Stop) and its WithStopBudget
// seam are gone. NewScheduler now seeds the per-instance
// Scheduler.stopBudget field from the defaultStopBudget const, and the
// Stop() fallback also reads the const — so no production or test path
// touches a mutable global. Tests inject a short budget via
// WithStopBudgetField(s, d) on the constructed instance, which keeps the
// swap local and race-free across t.Parallel Schedulers.
