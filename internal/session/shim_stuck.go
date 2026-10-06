package session

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// ErrShimStuck is returned (wrapped) by Router.GetOrCreate when a preceding
// fresh-mode Reset for the same key, or the wait before a rejected resume's
// fresh retry, found the shim's UNIX socket still bound after
// waitSocketGoneForKey timed out. Callers (the cron scheduler's fresh-mode
// preflight) errors.Is it: remediation is operator-side (kill the stuck shim
// PID / dashboard "force reset"), not "wait and retry" (#1324).
//
// Lifetime: the per-key flag is set in finishResetUnlocked / ResetAndRecreate
// and read + cleared by the very next GetOrCreate for the key (success or
// failure); a later GetOrCreate gets the raw spawn error.
var ErrShimStuck = errors.New("session: shim socket still bound after Reset wait")

// shimGoneWait bounds how long a released key waits for its shim socket to go
// before the key is flagged shim-stuck.
const shimGoneWait = 2 * time.Second

// shimGoneWaitOverride replaces shimGoneWait when non-zero. Only tests set it,
// for socket fixtures that never go away (shortenShimGoneWait).
var shimGoneWaitOverride atomic.Int64

// waitSocketGoneForKey waits up to shimGoneWait for the shim socket derived
// from key to disappear; returns false on timeout. Socket naming lives behind
// cli.WaitSocketGoneForKey so this package does not reach into internal/shim
// (#711). Reset callers use the false branch to mark the key shim-stuck (#1324).
func waitSocketGoneForKey(key string) bool {
	wait := shimGoneWait
	if d := shimGoneWaitOverride.Load(); d > 0 {
		wait = time.Duration(d)
	}
	return cli.WaitSocketGoneForKey(key, wait)
}
