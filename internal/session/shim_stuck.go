package session

import "errors"

// ErrShimStuck is returned (wrapped) by Router.GetOrCreate when a preceding
// fresh-mode Reset for the same key found the shim's UNIX socket still bound
// after waitSocketGoneForKey timed out. Callers (the cron scheduler's
// fresh-mode preflight) errors.Is it to surface an actionable error class —
// remediation is operator-side (kill the stuck shim PID / dashboard "force
// reset"), not "wait and retry" (#1324).
//
// Lifetime: the per-key flag is set in finishResetUnlocked / ResetAndRecreate
// and read + cleared by the very next GetOrCreate for the key (success or
// failure); a later GetOrCreate gets the raw spawn error.
var ErrShimStuck = errors.New("session: shim socket still bound after Reset wait")
