// Package metrics exposes process-wide counters and gauges backed by stdlib
// expvar, published on /debug/vars (routed through the dashboard mux by
// internal/server debug_expvar.go behind the same auth + loopback guard as
// pprof).
//
// Design choices:
//
//  1. expvar.Int (atomic int64 + JSON) rather than a custom type: zero
//     dependencies. A Prometheus migration would swap the vars without
//     touching call sites.
//  2. Package-level singletons, so call sites write
//     `metrics.SessionCreateTotal.Add(1)` with no further wiring.
//  3. No labels on the base counters; labeled vectors (labeled.go) bound
//     cardinality explicitly.
package metrics

import "expvar"

var (
	// SessionCreateTotal counts successful spawnSession completions (happy
	// path only; Spawn errors, panic recoveries and exempt sessions excluded).
	SessionCreateTotal = expvar.NewInt("naozhi_session_create_total")

	// SessionEvictTotal counts LRU evictions; rising under load means
	// session.max_procs is too low for the live user population.
	SessionEvictTotal = expvar.NewInt("naozhi_session_evict_total")

	// CLISpawnTotal counts wrapper.Spawn successes. Always ≥ SessionCreateTotal
	// because exempt sessions (planner / scratch) also spawn.
	CLISpawnTotal = expvar.NewInt("naozhi_cli_spawn_total")

	// WSAuthFailTotal counts WebSocket auth_fail replies (both rate-limited and
	// invalid-token branches; see the two subset counters below). A fast rise
	// is the primary brute-force indicator.
	WSAuthFailTotal = expvar.NewInt("naozhi_ws_auth_fail_total")

	// WSAuthFailRateLimitedTotal counts auth_fail replies from the per-IP
	// limiter. A sustained delta with flat *InvalidTokenTotal suggests a
	// looping client (reconnect storm) rather than a credential spray.
	WSAuthFailRateLimitedTotal = expvar.NewInt("naozhi_ws_auth_fail_rate_limited_total")

	// WSAuthFailInvalidTokenTotal counts auth_fail replies for a wrong token
	// after the limiter admitted the attempt — a paced credential spray.
	WSAuthFailInvalidTokenTotal = expvar.NewInt("naozhi_ws_auth_fail_invalid_token_total")

	// ShimRestartTotal counts shim.StartShimWithBackend successes; growth
	// between restarts indicates shim crash / respawn churn.
	ShimRestartTotal = expvar.NewInt("naozhi_shim_restart_total")

	// SpawnPanicRecoveredTotal counts panics absorbed by panicSafeSpawn in
	// session.Router. Each increment pairs with a slog.Error stack trace;
	// non-zero means a bug to investigate.
	SpawnPanicRecoveredTotal = expvar.NewInt("naozhi_spawn_panic_recovered_total")

	// PanicRecoveredTotal counts panics that crossed any recover() boundary
	// (dashboard WS readPump, remote-node send/interrupt, dispatch ownerLoop,
	// feishu cleanupNoncesTick). No per-site split — correlate with the
	// slog.Error stack dumps by timestamp. Superset of SpawnPanicRecoveredTotal.
	PanicRecoveredTotal = expvar.NewInt("naozhi_panic_recovered_total")

	// ShimReconnectGraceBackfillTotal counts deferred JSONL history loads that
	// fired because ReconnectShims had not supplied history within
	// shimReconnectGraceDelay. Non-zero: investigate why the shim was skipped.
	ShimReconnectGraceBackfillTotal = expvar.NewInt("naozhi_shim_reconnect_grace_backfill_total")

	// InterruptSentTotal counts InterruptViaControl outcomes where the
	// control_request reached the CLI. NoSession is deliberately not counted
	// in any Interrupt* counter — a missing key says nothing about interrupts.
	InterruptSentTotal = expvar.NewInt("naozhi_interrupt_sent_total")

	// InterruptNoTurnTotal counts InterruptViaControl outcomes on a session
	// with no active turn (UX hint: interrupt pressed while idle).
	InterruptNoTurnTotal = expvar.NewInt("naozhi_interrupt_no_turn_total")

	// InterruptUnsupportedTotal counts outcomes where the protocol (e.g. ACP)
	// has no stdin-level interrupt and the router fell back to SIGINT.
	InterruptUnsupportedTotal = expvar.NewInt("naozhi_interrupt_unsupported_total")

	// InterruptErrorTotal counts outcomes where the transport write failed
	// (shim socket dead); pair with naozhi_shim_restart_total.
	InterruptErrorTotal = expvar.NewInt("naozhi_interrupt_error_total")

	// EventLogPersistWrittenTotal counts EventEntry records committed to
	// <keyhash>.log by the per-session persister.
	EventLogPersistWrittenTotal = expvar.NewInt("naozhi_eventlog_persist_written_total")

	// EventLogPersistDroppedTotal counts records dropped because the
	// PersistSink channel was full. A sustained delta means the writer is not
	// draining and those events survive only in the in-memory ring.
	EventLogPersistDroppedTotal = expvar.NewInt("naozhi_eventlog_persist_dropped_total")

	// EventLogPersistFsyncTotal counts fsync(log) / fsync(idx) calls; growth
	// well past ~10/s means the debounce is not coalescing.
	EventLogPersistFsyncTotal = expvar.NewInt("naozhi_eventlog_persist_fsync_total")

	// EventLogPersistMalformedLinesTotal counts records schema.MarshalRecord
	// rejected (oversize / encoding failure); the paired slog.Warn has details.
	EventLogPersistMalformedLinesTotal = expvar.NewInt("naozhi_eventlog_persist_malformed_lines_total")

	// EventLogPersistReplayLeakTotal counts batches reaching the PersistSink
	// with replayPhase=true — a sink installed before InjectHistory finished.
	// Must stay 0 in production; the Persister drops the batch.
	EventLogPersistReplayLeakTotal = expvar.NewInt("naozhi_eventlog_persist_replay_leak_total")

	// AttachmentRefBumpTotal counts .meta rewrites by the attachment refcount
	// tracker (docs/rfc/attachment-refcount.md); rapid bumps on one
	// (session, attachment) pair coalesce into a single increment.
	AttachmentRefBumpTotal = expvar.NewInt("naozhi_attachment_ref_bump_total")

	// AttachmentRefClearTotal counts .meta rewrites during session removal
	// (one per attachment the removed session referenced).
	AttachmentRefClearTotal = expvar.NewInt("naozhi_attachment_ref_clear_total")

	// AttachmentRefMetaErrorTotal counts tracker errors writing the .meta
	// sidecar (missing sidecar, ENOSPC, EACCES); affected attachments fall
	// back to upload-only TTL GC.
	AttachmentRefMetaErrorTotal = expvar.NewInt("naozhi_attachment_ref_meta_error_total")

	// AttachmentRefDropTotal counts bumps rejected by the tracker's
	// non-blocking enqueue (channel full); runbook as for the persister drop.
	AttachmentRefDropTotal = expvar.NewInt("naozhi_attachment_ref_drop_total")

	// AttachmentGCReapedTotal counts payloads deleted by the attachment-gc
	// daemon (live mode only; docs/rfc/attachment-gc-daemon.md §6).
	AttachmentGCReapedTotal = expvar.NewInt("naozhi_attachment_gc_reaped_total")

	// AttachmentGCWouldReap{Legacy,NoRefs,Expired}Total bucket would-remove
	// decisions by reason (populated in dry-run and live mode):
	//   - legacy_no_meta: no sidecar, decided by date-dir TTL
	//   - meta_no_refs:   sidecar but no refs — may be a not-yet-bumped active reference (high risk)
	//   - refs_expired:   last ref past refTTL
	AttachmentGCWouldReapLegacyTotal  = expvar.NewInt("naozhi_attachment_gc_would_reap_legacy_total")
	AttachmentGCWouldReapNoRefsTotal  = expvar.NewInt("naozhi_attachment_gc_would_reap_no_refs_total")
	AttachmentGCWouldReapExpiredTotal = expvar.NewInt("naozhi_attachment_gc_would_reap_expired_total")

	// AttachmentGCSweepTotal counts attachment-gc Tick invocations (success + error).
	AttachmentGCSweepTotal = expvar.NewInt("naozhi_attachment_gc_sweep_total")

	// AttachmentGCErrorTotal counts workspace-level GC errors (ReadDir
	// failed); per-file remove failures are logged but not counted.
	AttachmentGCErrorTotal = expvar.NewInt("naozhi_attachment_gc_error_total")

	// CronExecutionSlowTotal counts cron executions exceeding
	// cronSlowThreshold; cron_histogram.go carries the full distribution.
	CronExecutionSlowTotal = expvar.NewInt("naozhi_cron_execution_slow_total")

	// CronSendBudgetDoubledTotal counts cron runs whose Send phase started
	// after the spawn phase had burned more than half of jobTimeout. Spawn ctx
	// and sendCtx deliberately do not share a clock, so one run can stretch to
	// ~2×jobTimeout; pairs with the "cron send budget exceeds job/2" WARN.
	CronSendBudgetDoubledTotal = expvar.NewInt("naozhi_cron_send_budget_doubled_total")

	// CronNotifyPartialTotal counts completion-notice deliveries that stopped
	// before all chunks were sent (replyCtx deadline hit, or a chunk's
	// ReplyWithRetry failed). Distinct from the deterministic
	// cronNotifyMaxChunks truncation WARN (#966).
	CronNotifyPartialTotal = expvar.NewInt("naozhi_cron_notify_partial_total")

	// CronStopBudgetExceeded{GC,Drain,Trigger}Total count Scheduler.Stop()
	// phases that blew their budget (#1083):
	//   - GC      : cold-start GC goroutine wait exceeded gcWaitBudget
	//   - Drain   : cron.Stop() drain exceeded stopBudget
	//   - Trigger : triggerWG.Wait remaining-budget slice exceeded
	CronStopBudgetExceededGCTotal      = expvar.NewInt("naozhi_cron_stop_budget_exceeded_gc_total")
	CronStopBudgetExceededDrainTotal   = expvar.NewInt("naozhi_cron_stop_budget_exceeded_drain_total")
	CronStopBudgetExceededTriggerTotal = expvar.NewInt("naozhi_cron_stop_budget_exceeded_trigger_total")

	// CronRunStartedTotal counts cron run starts (after the CAS gate). Its
	// difference from CronRunEndedTotal (modulo inflight) approximates runs
	// lost to panic / process crash.
	CronRunStartedTotal = expvar.NewInt("naozhi_cron_run_started_total")

	// CronRunEndedTotal counts cron run terminal transitions across all
	// states; the per-state breakdown follows below.
	CronRunEndedTotal = expvar.NewInt("naozhi_cron_run_ended_total")

	// CronRun{Succeeded,Failed,Skipped,TimedOut,Canceled}Total break down cron
	// run terminations by state, mirroring RunState in internal/cron/job.go;
	// a new state needs a new counter here.
	CronRunSucceededTotal = expvar.NewInt("naozhi_cron_run_succeeded_total")
	CronRunFailedTotal    = expvar.NewInt("naozhi_cron_run_failed_total")
	CronRunSkippedTotal   = expvar.NewInt("naozhi_cron_run_skipped_total")
	CronRunTimedOutTotal  = expvar.NewInt("naozhi_cron_run_timed_out_total")
	CronRunCanceledTotal  = expvar.NewInt("naozhi_cron_run_canceled_total")

	// CronSandboxRunFailedTotal counts sandbox-placement runs ending in
	// RunStateFailed (timed-out runs go to CronSandboxRunTimedOutTotal, #2091).
	// Transport failures carry double-run risk, so operators alert on this
	// specifically (agentcore-cloud-sandbox RFC §6.2).
	CronSandboxRunFailedTotal = expvar.NewInt("naozhi_cron_sandbox_run_failed_total")

	// CronSandboxRunTimedOutTotal counts sandbox-placement runs ending in
	// RunStateTimedOut, isolated from local-run timeouts.
	CronSandboxRunTimedOutTotal = expvar.NewInt("naozhi_cron_sandbox_run_timed_out_total")

	// SysessionRunStartedTotal counts sysession daemon run starts (#1723 RFC §6
	// Phase 1.5). Bumped inside emitRunStarted before the nil-broadcaster
	// early return so it never drifts from the broadcast path.
	SysessionRunStartedTotal = expvar.NewInt("naozhi_sysession_run_started_total")

	// SysessionRunEndedTotal counts sysession run terminal transitions; bumped
	// inside emitRunEnded before the nil-broadcaster early return.
	SysessionRunEndedTotal = expvar.NewInt("naozhi_sysession_run_ended_total")

	// SysessionRunnerParseFailTotal counts daemon `claude -p` calls whose
	// stdout was not a parsable json result envelope (truncated / non-JSON);
	// the daemon sees an error instead of a garbage reply.
	SysessionRunnerParseFailTotal = expvar.NewInt("naozhi_sysession_runner_parse_fail_total")

	// CronRunInflight gauges currently executing cron runs. No `_total`
	// suffix so the doc-sync regex treats it as a gauge.
	CronRunInflight = expvar.NewInt("naozhi_cron_run_inflight")

	// CronWatchdogInterruptTimeoutTotal counts deadline-watchdog timeouts where
	// InterruptViaControl did not return within watchdogInterruptTimeoutDefault
	// — a wedged stdin write to the shim; compare against
	// naozhi_shim_restart_total (#1327). Late-but-successful interrupts are
	// not counted here.
	CronWatchdogInterruptTimeoutTotal = expvar.NewInt("naozhi_cron_watchdog_interrupt_timeout_total")

	// Startup phase gauges: milliseconds from process start (t0 in main) to
	// the end of each phase, Set exactly once per process. Values are
	// cumulative, so per-phase duration is the difference between adjacent
	// rows. `_ms` suffix (not `_total`) marks them as gauges for dashboards
	// and the doc-sync regex.

	// StartupPhaseConfigMs is set after config.Load returns.
	StartupPhaseConfigMs = expvar.NewInt("naozhi_startup_phase_config_ms")

	// StartupPhaseRouterMs is set after session.NewRouter returns (sessions.json
	// load, eventlog scan, backend probes) — typically the largest phase.
	StartupPhaseRouterMs = expvar.NewInt("naozhi_startup_phase_router_ms")

	// StartupPhaseShimReconnectMs is set after router.ReconnectShimsCtx returns;
	// worst case ≈ N_shims × 15s handshake timeout.
	StartupPhaseShimReconnectMs = expvar.NewInt("naozhi_startup_phase_shim_reconnect_ms")

	// StartupPhasePlatformsMs is set after platform adapters register and the
	// parallel init WG (transcriber + project scan) drains.
	StartupPhasePlatformsMs = expvar.NewInt("naozhi_startup_phase_platforms_ms")

	// StartupPhaseSchedulerMs is set after scheduler.Start returns (cron store
	// load + jitter planning).
	StartupPhaseSchedulerMs = expvar.NewInt("naozhi_startup_phase_scheduler_ms")

	// StartupPhaseServerMs is set after server.NewWithOptions returns; excludes
	// srv.Start, which runs for the process lifetime.
	StartupPhaseServerMs = expvar.NewInt("naozhi_startup_phase_server_ms")

	// StartupPhaseReadyMs is set just before main blocks on the shutdown
	// select — the "naozhi is up" moment.
	StartupPhaseReadyMs = expvar.NewInt("naozhi_startup_phase_ready_ms")

	// AutoChainOriginsLengthMismatch counts SetPrevSessionOrigins calls that
	// observed a length drift between prevSessionIDs and prevSessionOrigins.
	// Must stay 0: prev_session_ids is append-only.
	AutoChainOriginsLengthMismatch = expvar.NewInt("naozhi_auto_chain_origins_length_mismatch_total")

	// AutoChainRetiredOnStartup counts sessions whose prev_session_ids chain
	// had auto-* segments stripped by the one-time startup cleanup
	// (docs/rfc/project-stable-session-key.md §9.2). Idempotent across restarts.
	AutoChainRetiredOnStartup = expvar.NewInt("naozhi_auto_chain_retired_on_startup_total")

	// ToolCallLeakDetectedTotal counts turns whose result text tripped the
	// leaked-tool-call detector (<invoke> XML written as prose, so nothing
	// executed). Incremented regardless of whether auto-recovery is enabled.
	ToolCallLeakDetectedTotal = expvar.NewInt("naozhi_session_toolcall_leak_detected_total")

	// ToolCallLeakRecoveredTotal counts leaked turns where the auto-continue
	// re-send produced a clean result. Always ≤ ToolCallLeakDetectedTotal.
	ToolCallLeakRecoveredTotal = expvar.NewInt("naozhi_session_toolcall_leak_recovered_total")

	// ToolCallLeakRecoveryFailedTotal counts leaked turns where recovery ran
	// but did not yield a clean result (process died, or leaked again on the
	// single retry). The returned text has the leaked XML stripped.
	ToolCallLeakRecoveryFailedTotal = expvar.NewInt("naozhi_session_toolcall_leak_recovery_failed_total")
)
