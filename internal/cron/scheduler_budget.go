package cron

// scheduler_budget.go holds the Stop-budget constants: StopPolicyBudgetThenLeak,
// defaultStopBudget, gcWaitBudget. Scheduler.Stop() stays in scheduler.go.

import "time"

// StopPolicyBudgetThenLeak is the documented Stop-overflow strategy this
// Scheduler honours: when the wait budget elapses with goroutines still in
// flight, Stop logs a warning and proceeds to the final persist, leaving
// orphaned goroutines for the OS to reap on exit. Deliberately a string
// constant, not an enum shared with sysession's StopPolicyForceExit: the
// divergence is a security decision (sysession daemons run prompt-derived
// strings through a CLI subprocess and a stuck goroutine could echo excerpts
// into another session's reply path; cron deliveries have no such surface), so
// a shared enum would invite the wrong "harmonise" intuition. Doc-only today —
// not consulted in control flow (#1060).
const StopPolicyBudgetThenLeak = "budget_then_leak"

// defaultStopBudget is the overall deadline Scheduler.Stop() spends waiting on
// cron.Stop + triggerWG before proceeding to save. Shared between both waits
// (not doubled) so execTimeout=3600s cannot pin a restart for ≈2 h. Aligned
// with session.ShutdownTimeout (30s) so both subsystems agree on the upper
// bound systemd sees.
const defaultStopBudget = 30 * time.Second

// gcWaitBudget bounds the cold-start GC goroutine wait in Stop(). Smaller than
// defaultStopBudget because trimAll's IO is short-lived; a wedge means a stuck
// filesystem and we'd rather skip the wait than pin systemd TimeoutStopSec.
// Kept a const: to override per-test, inject via a Scheduler field rather
// than a package-level var (races under t.Parallel).
const gcWaitBudget = 5 * time.Second
