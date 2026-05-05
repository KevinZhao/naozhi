// Package metrics exposes a small set of process-wide counters backed by
// stdlib expvar. The goal is operator observability — a naozhi deployment
// promises "10K users" scale but historically shipped zero metrics (only
// pprof), so post-incident analysis relied on parsing journalctl. This
// package adds counters covering the highest-signal lifecycle events:
//
//   - SessionCreateTotal:     successful spawnSession calls
//   - SessionEvictTotal:      LRU eviction frees a slot
//   - CLISpawnTotal:          wrapper.Spawn returns a Process (new CLI child)
//   - WSAuthFailTotal:        WebSocket auth_fail reply emitted (aggregate)
//   - WSAuthFailRateLimitedTotal: subset of WSAuthFailTotal triggered by the
//     per-IP rate limiter (brute-force throttling active). R172-ARCH-D10.
//   - WSAuthFailInvalidTokenTotal: subset of WSAuthFailTotal triggered by a
//     wrong token presented to an otherwise-allowed IP. R172-ARCH-D10.
//   - ShimRestartTotal:       shim.Manager.StartShimWithBackend succeeds
//   - SpawnPanicRecoveredTotal: panicSafeSpawn absorbs a wrapper.Spawn panic
//     (shim exec crash / bogus protocol Init / etc.). A non-zero value is an
//     operator-actionable reliability signal: the recover path keeps naozhi
//     alive but the underlying bug should be investigated. R172-ARCH-D10.
//   - ShimReconnectGraceBackfillTotal: deferred JSONL history load fired for a
//     shim-managed session whose ReconnectShims pass did not supply history
//     within shimReconnectGraceDelay (R53-ARCH-001 fallback path).
//     R172-ARCH-D10.
//   - Interrupt{Sent,NoTurn,Unsupported,Error}Total: per-outcome counts for
//     Router.InterruptSessionViaControl. NoSession is deliberately NOT
//     counted — a key-does-not-exist lookup isn't a signal about interrupt
//     behaviour, and counting it would blur the denominator when computing
//     "interrupts that reached the CLI" ratios. R172-ARCH-D10.
//
// Counters are published via the stdlib expvar package, which auto-registers
// itself on /debug/vars. Exposing them requires routing /debug/vars through
// the dashboard mux — the naozhi HTTP server registers that route via
// internal/server (see debug_expvar.go) behind the same auth + loopback
// guard as pprof.
//
// Design choices:
//
//  1. Use expvar.Int (atomic int64 + JSON marshaling) rather than a custom
//     type. Zero dependencies, stdlib-stable since Go 1.0. A future upgrade
//     to Prometheus client_golang would replace the vars with prometheus
//     counters without touching call sites (accept an interface, return
//     struct).
//  2. Counters are package-level singletons exposed as *expvar.Int so call
//     sites write `metrics.SessionCreateTotal.Add(1)` with no further
//     wiring. This mirrors the stdlib http.DefaultServeMux pattern.
//  3. No labels. expvar is untyped; label cardinality enforcement belongs
//     to a real metrics lib. For MVP observability the absence of labels
//     is a feature (operators can't accidentally blow up memory with
//     per-user tags).
package metrics

import "expvar"

var (
	// SessionCreateTotal counts successful spawnSession completions. Incremented
	// only on the happy path — Spawn errors, panic-safe spawn recoveries, and
	// exempt-session creations are excluded. A burst here shortly before CLI
	// spawn backpressure usually indicates a misbehaving IM client.
	SessionCreateTotal = expvar.NewInt("naozhi_session_create_total")

	// SessionEvictTotal counts LRU evictions. Rising monotonically under load
	// means session cap is too low for the live user population; the cap is
	// controlled by session.max_procs in config.yaml.
	SessionEvictTotal = expvar.NewInt("naozhi_session_evict_total")

	// CLISpawnTotal counts wrapper.Spawn successes. Always ≥ SessionCreateTotal
	// because Spawn is also called for exempt sessions (planner / scratch) that
	// do not go through the normal SessionCreateTotal path. A delta growth much
	// larger than SessionCreateTotal indicates exempt-session churn.
	CLISpawnTotal = expvar.NewInt("naozhi_cli_spawn_total")

	// WSAuthFailTotal counts WebSocket auth_fail replies. Rising fast is a
	// classic credential-spray signal; combined with /api/auth/login Retry-After
	// 429 events in journalctl, it's the primary brute-force indicator.
	// Incremented for BOTH rate-limited and invalid-token branches; use the
	// dedicated *RateLimited / *InvalidToken counters below to tell them apart
	// when triaging whether the limiter is already engaging.
	WSAuthFailTotal = expvar.NewInt("naozhi_ws_auth_fail_total")

	// WSAuthFailRateLimitedTotal counts WS auth_fail replies caused by the
	// per-IP token-bucket limiter firing — the IP may still know a valid
	// token, but its connect-rate blew past the burst. A sustained delta here
	// under constant delta on *InvalidTokenTotal suggests a looping client
	// (e.g. dashboard reconnect storm) rather than a credential spray; the
	// inverse ratio is the brute-force signature. R172-ARCH-D10.
	WSAuthFailRateLimitedTotal = expvar.NewInt("naozhi_ws_auth_fail_rate_limited_total")

	// WSAuthFailInvalidTokenTotal counts WS auth_fail replies caused by the
	// presented token not matching dashboardToken. Unlike *RateLimitedTotal,
	// this increments AFTER the limiter admits the attempt, so a fast-rising
	// counter here specifically signals credential spray on a single IP that
	// is pacing itself under the limiter threshold. R172-ARCH-D10.
	WSAuthFailInvalidTokenTotal = expvar.NewInt("naozhi_ws_auth_fail_invalid_token_total")

	// ShimRestartTotal counts shim.StartShimWithBackend successes. Under
	// zero-downtime restart operators expect this to roughly match the number
	// of live sessions at restart time. Growing between restarts indicates
	// shim crash / respawn churn.
	ShimRestartTotal = expvar.NewInt("naozhi_shim_restart_total")

	// SpawnPanicRecoveredTotal counts panics absorbed by panicSafeSpawn in
	// session.Router (wraps cli.Wrapper.Spawn). Each increment corresponds to
	// a slog.Error("spawnSession: wrapper.Spawn panicked", ...) record with a
	// full stack trace — operators should grep journalctl for those lines to
	// find the root cause. The counter itself is the at-a-glance "has a panic
	// ever happened on this process lifetime?" indicator without scanning
	// logs. R172-ARCH-D10.
	SpawnPanicRecoveredTotal = expvar.NewInt("naozhi_spawn_panic_recovered_total")

	// ShimReconnectGraceBackfillTotal counts deferred JSONL history loads that
	// fired because a shim-managed session was still missing history after
	// shimReconnectGraceDelay elapsed (the R53-ARCH-001 fallback). The happy
	// path — ReconnectShims populates history within the grace window — does
	// NOT increment this counter; only the fallback branch does. A non-zero
	// value means operators should investigate why ReconnectShims skipped the
	// session (shim died between shimManagedKeys() and Discover is the common
	// cause). R172-ARCH-D10.
	ShimReconnectGraceBackfillTotal = expvar.NewInt("naozhi_shim_reconnect_grace_backfill_total")

	// InterruptSentTotal counts InterruptViaControl outcomes where the
	// control_request actually reached the CLI. This is the "happy path" for
	// dashboard interrupt button presses. Combined with the other Interrupt*
	// counters, operators can tell at a glance whether users are hitting
	// interrupt usefully (Sent) or uselessly (NoTurn). R172-ARCH-D10.
	InterruptSentTotal = expvar.NewInt("naozhi_interrupt_sent_total")

	// InterruptNoTurnTotal counts InterruptViaControl outcomes where the
	// session exists but has no active turn. A consistently high delta here
	// relative to InterruptSentTotal indicates users expect interrupt to "do
	// something" on an idle session — a UX hint that the button should be
	// disabled or labelled differently when no turn is running. R172-ARCH-D10.
	InterruptNoTurnTotal = expvar.NewInt("naozhi_interrupt_no_turn_total")

	// InterruptUnsupportedTotal counts InterruptViaControl outcomes where the
	// active protocol (e.g. ACP) has no stdin-level interrupt primitive. The
	// router falls back to SIGINT in this branch; a growing delta here tells
	// operators how much their deployment depends on the SIGINT fallback,
	// which has different semantics (kills the whole CLI). R172-ARCH-D10.
	InterruptUnsupportedTotal = expvar.NewInt("naozhi_interrupt_unsupported_total")

	// InterruptErrorTotal counts InterruptViaControl outcomes where the
	// transport write failed (shim socket dead / broken pipe). A non-zero
	// value almost always means F6's reconcile path has work to do — the
	// shim is likely zombied. Pair with naozhi_shim_restart_total to see
	// whether reconcile is actually clearing them. R172-ARCH-D10.
	InterruptErrorTotal = expvar.NewInt("naozhi_interrupt_error_total")
)
