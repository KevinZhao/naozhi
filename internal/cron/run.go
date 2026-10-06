package cron

import "github.com/naozhi/naozhi/internal/cron/runstore"

// CronRun is the persistent record of a single cron job execution, written
// once by finishRun on the terminal transition. The record and its on-disk
// store live in internal/cron/runstore; the aliases keep cron.CronRun the
// public name.
type CronRun = runstore.CronRun

// CronRunSummary is the slim CronRun shape list endpoints and the cron list
// view's recent_runs field return.
type CronRunSummary = runstore.CronRunSummary

// RunStoreHealth is the run store's loss counters, for /health.
type RunStoreHealth = runstore.Health

// ErrCorruptRun is returned when a run record fails to parse or exceeds the
// size cap; list APIs skip it and GC removes it.
var ErrCorruptRun = runstore.ErrCorruptRun
