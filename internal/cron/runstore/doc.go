// Package runstore is cron's run history on disk: one CronRun record per
// terminal execution under runs/<jobID>/<runID>.json, a newest-first cache per
// job for the dashboard list poll, and the count + age retention GC. It owns
// the record types, the file format and every read and write of the tree.
// Markers covers the run-inflight markers (runinflight/<runID>.json) that let
// the next boot settle a local run the previous process never finished.
// Deciding when a run is recorded, and what a leftover marker means, stays in
// cron.
//
// The package must not import internal/cron.
package runstore
