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

	// PanicRecoveredTotal is a global counter for panics that crossed a
	// recover() boundary anywhere in the process. Dimensional split by call
	// site is NOT provided — operators can correlate with slog.Error stack
	// dumps (which every recover site already emits) by timestamp. Distinct
	// from SpawnPanicRecoveredTotal which is spawn-specific and a subset of
	// this total.
	//
	// Wired into the highest-signal recover sites (dashboard WS readPump,
	// federated remote-node send/interrupt goroutines, dispatch ownerLoop,
	// feishu cleanupNoncesTick). Non-zero is an operator-actionable
	// reliability signal: the recover path kept naozhi alive but the
	// underlying bug should be investigated. OBS1.
	PanicRecoveredTotal = expvar.NewInt("naozhi_panic_recovered_total")

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

	// EventLogPersistWrittenTotal counts individual EventEntry records
	// successfully committed to <keyhash>.log by the per-session
	// persister. Rising in lock-step with conversation traffic is the
	// expected signal — a pause while traffic continues means either
	// the persister goroutine stalled or the PersistSink channel is
	// saturated (see EventLogPersistDroppedTotal for that path). RFC §6.3.
	EventLogPersistWrittenTotal = expvar.NewInt("naozhi_eventlog_persist_written_total")

	// EventLogPersistDroppedTotal counts EventEntry records dropped
	// because the per-session PersistSink channel was full when the
	// Append hot path tried to enqueue. A sustained non-zero delta
	// here is an operator-actionable signal: the disk or writer
	// goroutine is not draining fast enough and live dashboard events
	// are being lost from the persistent tier (they survive only in
	// the in-memory ring). RFC §3.2.3 / §6.3.
	EventLogPersistDroppedTotal = expvar.NewInt("naozhi_eventlog_persist_dropped_total")

	// EventLogPersistFsyncTotal counts fsync(log) / fsync(idx) calls
	// the persister has issued. Two fsyncs per debounce window in
	// normal operation (log first, idx second); a value that grows
	// well past the expected ~10/s rate indicates the debounce is
	// not coalescing (misconfiguration of FlushInterval) or there
	// are many tiny Flush() calls forcing out-of-band fsyncs. RFC §6.3.
	EventLogPersistFsyncTotal = expvar.NewInt("naozhi_eventlog_persist_fsync_total")

	// EventLogPersistMalformedLinesTotal counts records the persister
	// refused to write because schema.MarshalRecord rejected them
	// (oversize record, encoding failure). Zero in steady state; a
	// delta implies an upstream caller is producing malformed entries
	// and the corresponding slog.Warn has the offending UUID / size.
	// RFC §6.3.
	EventLogPersistMalformedLinesTotal = expvar.NewInt("naozhi_eventlog_persist_malformed_lines_total")

	// EventLogPersistReplayLeakTotal counts batches that reached the
	// PersistSink with replayPhase=true. In production this MUST stay
	// at 0: a non-zero value means some caller installed the sink
	// BEFORE InjectHistory completed, violating RFC §3.2.2's ordering
	// contract. The Persister drops the batch (preventing the
	// infinite-persist loop), so dashboard behaviour doesn't degrade
	// visibly, but a monitor paging on `*ReplayLeakTotal != 0` is
	// recommended so the underlying bug gets a fix. RFC §3.2.3.
	EventLogPersistReplayLeakTotal = expvar.NewInt("naozhi_eventlog_persist_replay_leak_total")

	// AttachmentRefBumpTotal counts .meta rewrites performed by the
	// attachment refcount tracker (see docs/rfc/attachment-refcount.md).
	// Each increment represents one ReferencingKeyHashes / LastReferencedAt
	// update; coalesce collapses N rapid bumps on the same (session,
	// attachment) pair into a single increment. Delta rate roughly
	// tracks "new image events reaching disk" multiplied by the number
	// of distinct attachments referenced.
	AttachmentRefBumpTotal = expvar.NewInt("naozhi_attachment_ref_bump_total")

	// AttachmentRefClearTotal counts .meta rewrites performed during
	// session removal (OnSessionRemoved walks the workspace dir and
	// drops the keyhash from every attachment that references it).
	// A single session deletion may bump this by the number of
	// attachments it touched. Steady zero in normal operation; a
	// delta matches an operator clicking × on a dashboard card.
	AttachmentRefClearTotal = expvar.NewInt("naozhi_attachment_ref_clear_total")

	// AttachmentRefMetaErrorTotal counts tracker errors writing the
	// .meta sidecar — usually missing sidecar (legacy attachments
	// without the refcount fields), ENOSPC, or permission denied.
	// Non-zero steady-state is a signal the tracker cannot keep up:
	// attachments will fall back to upload-only TTL GC.
	AttachmentRefMetaErrorTotal = expvar.NewInt("naozhi_attachment_ref_meta_error_total")

	// AttachmentRefDropTotal counts bumps rejected by the tracker's
	// non-blocking enqueue path (channel at capacity). Mirrors the
	// event-log Persister's drop counter — operator runbook is the
	// same: investigate disk latency / writer stall.
	AttachmentRefDropTotal = expvar.NewInt("naozhi_attachment_ref_drop_total")

	// CronExecutionSlowTotal counts cron executions that exceeded
	// cronSlowThreshold wall-clock — a poor-man's histogram for R208-OBS1
	// residual. Counter is monotonic (never resets); correlate with
	// naozhi_cron_execution_failed_total (existing) to classify slow-vs-
	// failed outcomes. A full histogram would require expvar gauge
	// infrastructure; this counter lets ops add a Grafana single-stat
	// alert without schema churn.
	CronExecutionSlowTotal = expvar.NewInt("naozhi_cron_execution_slow_total")

	// CronRunStartedTotal counts cron run starts (after CAS gate, before
	// IM notify). Pairs with CronRunEndedTotal — the difference (modulo
	// inflight) approximates "runs interrupted by panic / process crash".
	// Wired in scheduler.executeOpt's started branch.
	CronRunStartedTotal = expvar.NewInt("naozhi_cron_run_started_total")

	// CronRunEndedTotal counts cron run terminal transitions across all
	// states (succeeded/failed/skipped/timed_out/canceled). Aggregated
	// counter — per-state breakdown lives on naozhi_cron_run_<state>_total
	// below. Wired in recordResult / overlap-skip branches.
	CronRunEndedTotal = expvar.NewInt("naozhi_cron_run_ended_total")

	// CronRunSucceededTotal / CronRunFailedTotal / CronRunSkippedTotal /
	// CronRunTimedOutTotal / CronRunCanceledTotal — per-state breakdown of
	// cron run terminations. The state taxonomy mirrors RunState in
	// internal/cron/job.go; adding a new state requires a new counter
	// here so dashboards stay in lockstep.
	CronRunSucceededTotal = expvar.NewInt("naozhi_cron_run_succeeded_total")
	CronRunFailedTotal    = expvar.NewInt("naozhi_cron_run_failed_total")
	CronRunSkippedTotal   = expvar.NewInt("naozhi_cron_run_skipped_total")
	CronRunTimedOutTotal  = expvar.NewInt("naozhi_cron_run_timed_out_total")
	CronRunCanceledTotal  = expvar.NewInt("naozhi_cron_run_canceled_total")

	// CronRunInflight is a gauge of currently executing cron runs. Bumped
	// on CAS-gate success, decremented on terminal transition. Naming uses
	// no `_total` suffix so the doc-sync regex treats it as a gauge.
	CronRunInflight = expvar.NewInt("naozhi_cron_run_inflight")

	// --- Startup phase timing gauges (RNEW-OPS-414) -----------------------
	//
	// Cold-start observability: historically the only signal operators had
	// for a slow boot was grepping journalctl timestamps across slog lines.
	// These gauges record milliseconds from process start (t0 captured in
	// main) to the end of each logical phase, set exactly once per process.
	// Values are cumulative (phase N ms = total ms from t0 through phase N)
	// so operators can read the table top-to-bottom and see per-phase
	// duration as the difference between adjacent rows.
	//
	// Gauge semantics (not counter): values are written once via Set, never
	// Add'd. Naming uses `_ms` suffix — NOT `_total` — so dashboards treat
	// them as gauges and the `*_total` doc-sync regex correctly ignores
	// them. Using expvar.Int (int64 millis) keeps zero dependencies and
	// avoids float encoding surprises downstream. Prometheus migration
	// path: swap to `prometheus.NewGauge` with `_milliseconds` suffix, or
	// to `_seconds` converted to float.
	//
	// Measurement pattern at each call site:
	//     metrics.StartupPhaseXxxMs.Set(time.Since(t0).Milliseconds())
	// Set takes <1µs and cannot block boot.

	// StartupPhaseConfigMs is set after config.Load returns — captures
	// flag parsing + YAML read + env-file resolution cost. Unusually high
	// (>500ms on a warm disk) points at a very large config or slow
	// filesystem.
	StartupPhaseConfigMs = expvar.NewInt("naozhi_startup_phase_config_ms")

	// StartupPhaseRouterMs is set after session.NewRouter returns —
	// captures sessions.json load, eventlog dir scan, wrapper map
	// assembly, and backend version probes. This is typically the largest
	// phase on a warm host with many persisted sessions.
	StartupPhaseRouterMs = expvar.NewInt("naozhi_startup_phase_router_ms")

	// StartupPhaseShimReconnectMs is set after router.ReconnectShimsCtx
	// returns — captures stat() + handshake against each surviving shim
	// from the previous naozhi run. Worst case ≈ N_shims × 15s handshake
	// timeout; a slow value here means shim sockets are stuck (SIGTERM
	// arriving now will abort cleanly thanks to the ctx plumbing).
	StartupPhaseShimReconnectMs = expvar.NewInt("naozhi_startup_phase_shim_reconnect_ms")

	// StartupPhasePlatformsMs is set after platform adapters are
	// registered AND the parallel init WG (transcriber + project scan)
	// has drained — so a slow transcribe.New or projects.Scan is visible
	// here rather than hidden inside a goroutine.
	StartupPhasePlatformsMs = expvar.NewInt("naozhi_startup_phase_platforms_ms")

	// StartupPhaseSchedulerMs is set after scheduler.Start returns —
	// captures cron store load + jitter planning. A slow value here
	// usually means the cron store file grew large; see
	// cron.StorePath.
	StartupPhaseSchedulerMs = expvar.NewInt("naozhi_startup_phase_scheduler_ms")

	// StartupPhaseServerMs is set after server.NewWithOptions returns —
	// captures route registration, WS hub wiring, and dashboard asset
	// mounting. Not including srv.Start (HTTP listen loop) because that
	// runs in a goroutine for the remainder of the process lifetime.
	StartupPhaseServerMs = expvar.NewInt("naozhi_startup_phase_server_ms")

	// StartupPhaseReadyMs is set just before main blocks on the shutdown
	// select — the effective "naozhi is up" moment. Compare against
	// systemd's START_USEC to cross-check TimeoutStartSec margin.
	StartupPhaseReadyMs = expvar.NewInt("naozhi_startup_phase_ready_ms")

	// AutoChainSpawnAttachTotal counts spawnSession invocations that
	// auto-attached one or more prev_session_ids via the workspace
	// auto-chain feature (docs/rfc/auto-workspace-chain.md). Each
	// increment is one session newly receiving a chain.
	AutoChainSpawnAttachTotal = expvar.NewInt("naozhi_auto_chain_spawn_attach_total")

	// AutoChainBackfillAttachTotal counts sessions that received a
	// chain via the one-shot startup backfill (NewRouter →
	// runAutoChainBackfillOnce). Steady-state delta should be 0 —
	// the counter only ticks at startup.
	AutoChainBackfillAttachTotal = expvar.NewInt("naozhi_auto_chain_backfill_attach_total")

	// AutoChainBackfillSkippedNoWorkspace / NoCandidates / TOCTOUDrop /
	// AlreadyFilled — per-reason skip breakdown for the startup backfill.
	// Together they account for every candidate session inspected by
	// runAutoChainBackfillOnce that did not result in an attach. expvar
	// has no native label support, so split counters are the idiomatic
	// alternative (see naozhi_cron_run_<state>_total above).
	AutoChainBackfillSkippedNoWorkspace   = expvar.NewInt("naozhi_auto_chain_backfill_skipped_no_workspace_total")
	AutoChainBackfillSkippedNoCandidates  = expvar.NewInt("naozhi_auto_chain_backfill_skipped_no_candidates_total")
	AutoChainBackfillSkippedTOCTOUDrop    = expvar.NewInt("naozhi_auto_chain_backfill_skipped_toctou_drop_total")
	AutoChainBackfillSkippedAlreadyFilled = expvar.NewInt("naozhi_auto_chain_backfill_skipped_already_filled_total")

	// AutoChainTOCTOUCollisionTotal counts the number of sessionIDs that
	// were dropped by the spawn-path or backfill-path phase-3 re-check
	// (after pickWorkspaceChain returned them but before the lock-held
	// apply). Steady non-zero values mean cron / sys / sibling spawns
	// are racing chain attach frequently — investigate scheduler
	// throughput.
	AutoChainTOCTOUCollisionTotal = expvar.NewInt("naozhi_auto_chain_toctou_collision_total")

	// AutoChainOriginsLengthMismatch counts ManagedSession.SetPrevSessionOrigins
	// invocations that observed a length drift between prevSessionIDs and
	// prevSessionOrigins (RFC v3 Arch-MINOR-1). Steady-state must be 0;
	// any increment indicates a code path violated the append-only
	// invariant on prev_session_ids.
	AutoChainOriginsLengthMismatch = expvar.NewInt("naozhi_auto_chain_origins_length_mismatch_total")
)
