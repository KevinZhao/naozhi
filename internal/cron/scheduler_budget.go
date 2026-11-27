package cron

// scheduler_budget.go holds the Stop-budget constants: defaultStopBudget,
// gcWaitBudget. Scheduler.Stop() stays in scheduler.go.

import "time"

// defaultStopBudget is the overall deadline Scheduler.Stop() spends waiting on
// cron.Stop + triggerWG before proceeding to save. Shared between both waits
// (not doubled) so execTimeout=3600s cannot pin a restart for ≈2 h. Aligned
// with session.ShutdownTimeout (30s) so both subsystems agree on the upper
// bound systemd sees. When it elapses with goroutines still in flight, Stop
// logs a warning and proceeds to the final persist, leaving them for the OS to
// reap on exit. sysession's Manager exits instead; the divergence is
// deliberate (sysession daemons run conversation excerpts through a CLI
// subprocess that a stuck goroutine could echo into another reply path; cron
// deliveries have no such surface).
const defaultStopBudget = 30 * time.Second

// gcWaitBudget bounds the cold-start GC goroutine wait in Stop(). Smaller than
// defaultStopBudget because trimAll's IO is short-lived; a wedge means a stuck
// filesystem and we'd rather skip the wait than pin systemd TimeoutStopSec.
// Kept a const: to override per-test, inject via a Scheduler field rather
// than a package-level var (races under t.Parallel).
const gcWaitBudget = 5 * time.Second
