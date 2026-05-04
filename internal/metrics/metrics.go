// Package metrics exposes a small set of process-wide counters backed by
// stdlib expvar. The goal is operator observability — a naozhi deployment
// promises "10K users" scale but historically shipped zero metrics (only
// pprof), so post-incident analysis relied on parsing journalctl. This
// package adds five counters covering the highest-signal lifecycle events:
//
//   - SessionCreateTotal:  successful spawnSession calls
//   - SessionEvictTotal:   LRU eviction frees a slot
//   - CLISpawnTotal:       wrapper.Spawn returns a Process (new CLI child)
//   - WSAuthFailTotal:     WebSocket auth_fail reply emitted
//   - ShimRestartTotal:    shim.Manager.StartShimWithBackend succeeds
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
	WSAuthFailTotal = expvar.NewInt("naozhi_ws_auth_fail_total")

	// ShimRestartTotal counts shim.StartShimWithBackend successes. Under
	// zero-downtime restart operators expect this to roughly match the number
	// of live sessions at restart time. Growing between restarts indicates
	// shim crash / respawn churn.
	ShimRestartTotal = expvar.NewInt("naozhi_shim_restart_total")
)
