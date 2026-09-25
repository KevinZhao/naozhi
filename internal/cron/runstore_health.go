package cron

// RunStoreHealth is the cron run store's loss counters, for /health. Append
// cannot fail the run it records (history is best-effort), so these counters
// plus an Error log line are all a lost record leaves behind — which is only a
// signal if something reads them (#2792).
type RunStoreHealth struct {
	// Enabled is false when the scheduler persists no history (StorePath
	// empty, or the runs/ root was refused as a symlink); the counters are
	// then meaningless and /health omits the section.
	Enabled bool
	// WriteFailedDiskFull / WriteFailedOther split record-write failures so
	// ENOSPC is distinguishable from EACCES / I/O errors (#1338).
	WriteFailedDiskFull int64
	WriteFailedOther    int64
	// HistoryDropped counts records dropped because even the truncated retry
	// payload exceeded the per-record cap (#964).
	HistoryDropped int64
	// CacheStaleEvictions counts recent-cache rows evicted by the approximate
	// time source rather than disk mtime; a growing value against disk-side
	// trims means the approximation evicts rows whose files are still kept (#962).
	CacheStaleEvictions int64
}
