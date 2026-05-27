package session

import "errors"

// ErrShimStuck is returned (wrapped) by Router.GetOrCreate when a
// preceding fresh-mode Reset for the same key found the shim's UNIX
// socket still bound after waitSocketGoneForKey timed out (2s) and the
// follow-up StartShim therefore hit the "refusing to clobber" guard.
//
// Callers (notably the cron scheduler's fresh-mode preflight at
// scheduler_run.go:1093) can errors.Is(err, ErrShimStuck) to surface a
// distinct, actionable error class to the operator instead of the
// generic ErrClassSessionError + "执行跳过，请稍后重试。" notice. The
// remediation is operator-side (kill the stuck shim PID, the dashboard
// "force reset" button) — not a "wait and retry" the user-visible
// notice currently implies.
//
// Lifetime: the per-key stuck flag is set inside finishResetUnlocked /
// ResetAndRecreate when waitSocketGoneForKey returns false, and read +
// cleared by the very next GetOrCreate for the same key (success or
// failure). A second GetOrCreate after the flag was consumed gets the
// raw spawn error without ErrShimStuck attached, matching the "the
// shim eventually freed up" branch.
//
// (#1324 — R20260527122801-CR-12)
var ErrShimStuck = errors.New("session: shim socket still bound after Reset wait")
