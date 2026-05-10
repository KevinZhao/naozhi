package metrics

import (
	"encoding/json"
	"expvar"
	"testing"
)

// TestCountersRegisteredUnderStableNames pins the expvar names the operator
// runbook (docs/ops/pprof.md expvar section) depends on. Renaming any of
// these breaks dashboards and grep-based alert rules.
func TestCountersRegisteredUnderStableNames(t *testing.T) {
	t.Parallel()
	want := []string{
		"naozhi_session_create_total",
		"naozhi_session_evict_total",
		"naozhi_cli_spawn_total",
		"naozhi_ws_auth_fail_total",
		"naozhi_ws_auth_fail_rate_limited_total",
		"naozhi_ws_auth_fail_invalid_token_total",
		"naozhi_shim_restart_total",
		"naozhi_spawn_panic_recovered_total",
		"naozhi_panic_recovered_total",
		"naozhi_shim_reconnect_grace_backfill_total",
		"naozhi_interrupt_sent_total",
		"naozhi_interrupt_no_turn_total",
		"naozhi_interrupt_unsupported_total",
		"naozhi_interrupt_error_total",
		"naozhi_eventlog_persist_written_total",
		"naozhi_eventlog_persist_dropped_total",
		"naozhi_eventlog_persist_fsync_total",
		"naozhi_eventlog_persist_malformed_lines_total",
		"naozhi_eventlog_persist_replay_leak_total",
		"naozhi_attachment_ref_bump_total",
		"naozhi_attachment_ref_clear_total",
		"naozhi_attachment_ref_meta_error_total",
		"naozhi_attachment_ref_drop_total",
		// RNEW-OPS-414: startup phase timing gauges. Same expvar.Int
		// storage as the counters above, so they belong in the same
		// stable-names pin; the `_ms` suffix distinguishes them as
		// gauges semantically for dashboards.
		"naozhi_startup_phase_config_ms",
		"naozhi_startup_phase_router_ms",
		"naozhi_startup_phase_shim_reconnect_ms",
		"naozhi_startup_phase_platforms_ms",
		"naozhi_startup_phase_scheduler_ms",
		"naozhi_startup_phase_server_ms",
		"naozhi_startup_phase_ready_ms",
	}
	for _, name := range want {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v := expvar.Get(name)
			if v == nil {
				t.Fatalf("counter %q not registered with expvar", name)
			}
			if _, ok := v.(*expvar.Int); !ok {
				t.Fatalf("counter %q is %T, want *expvar.Int", name, v)
			}
		})
	}
}

// TestCountersIncrement pins that Add(1) actually increments — the symbols
// are tiny wrappers but a future refactor that replaces expvar.Int with a
// custom type must keep the observable behaviour.
func TestCountersIncrement(t *testing.T) {
	// Not t.Parallel: counters are process-wide and other tests in the
	// binary may mutate them. We capture start values and assert the
	// delta only, which is safe for concurrent readers.
	counters := map[string]*expvar.Int{
		"session_create":                SessionCreateTotal,
		"session_evict":                 SessionEvictTotal,
		"cli_spawn":                     CLISpawnTotal,
		"ws_auth_fail":                  WSAuthFailTotal,
		"ws_auth_fail_rate_limited":     WSAuthFailRateLimitedTotal,
		"ws_auth_fail_invalid_token":    WSAuthFailInvalidTokenTotal,
		"shim_restart":                  ShimRestartTotal,
		"spawn_panic_recovered":         SpawnPanicRecoveredTotal,
		"panic_recovered":               PanicRecoveredTotal,
		"shim_reconnect_grace_backfill": ShimReconnectGraceBackfillTotal,
		"interrupt_sent":                InterruptSentTotal,
		"interrupt_no_turn":             InterruptNoTurnTotal,
		"interrupt_unsupported":         InterruptUnsupportedTotal,
		"interrupt_error":               InterruptErrorTotal,
		"eventlog_persist_written":      EventLogPersistWrittenTotal,
		"eventlog_persist_dropped":      EventLogPersistDroppedTotal,
		"eventlog_persist_fsync":        EventLogPersistFsyncTotal,
		"eventlog_persist_malformed":    EventLogPersistMalformedLinesTotal,
		"eventlog_persist_replay_leak":  EventLogPersistReplayLeakTotal,
		"attachment_ref_bump":           AttachmentRefBumpTotal,
		"attachment_ref_clear":          AttachmentRefClearTotal,
		"attachment_ref_meta_error":     AttachmentRefMetaErrorTotal,
		"attachment_ref_drop":           AttachmentRefDropTotal,
		// RNEW-OPS-414: startup phase gauges share the expvar.Int storage
		// with the counters, so Add-based delta observation still holds
		// — in production these are written via Set once per process,
		// but the underlying integer semantics are identical.
		"startup_phase_config":         StartupPhaseConfigMs,
		"startup_phase_router":         StartupPhaseRouterMs,
		"startup_phase_shim_reconnect": StartupPhaseShimReconnectMs,
		"startup_phase_platforms":      StartupPhasePlatformsMs,
		"startup_phase_scheduler":      StartupPhaseSchedulerMs,
		"startup_phase_server":         StartupPhaseServerMs,
		"startup_phase_ready":          StartupPhaseReadyMs,
	}
	for name, c := range counters {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			start := c.Value()
			c.Add(1)
			c.Add(2)
			if got := c.Value() - start; got != 3 {
				t.Errorf("%s: delta %d, want 3", name, got)
			}
		})
	}
}

// TestCountersJSONEncodable pins that every counter marshals as a JSON
// number, not a string or object. expvar's /debug/vars endpoint emits each
// counter via its MarshalJSON, and operators' jq scripts assume numeric
// output.
func TestCountersJSONEncodable(t *testing.T) {
	t.Parallel()
	for _, c := range []*expvar.Int{
		SessionCreateTotal, SessionEvictTotal, CLISpawnTotal,
		WSAuthFailTotal, WSAuthFailRateLimitedTotal, WSAuthFailInvalidTokenTotal,
		ShimRestartTotal, SpawnPanicRecoveredTotal, PanicRecoveredTotal,
		ShimReconnectGraceBackfillTotal,
		InterruptSentTotal, InterruptNoTurnTotal, InterruptUnsupportedTotal,
		InterruptErrorTotal,
		EventLogPersistWrittenTotal, EventLogPersistDroppedTotal,
		EventLogPersistFsyncTotal, EventLogPersistMalformedLinesTotal,
		EventLogPersistReplayLeakTotal,
		AttachmentRefBumpTotal, AttachmentRefClearTotal,
		AttachmentRefMetaErrorTotal, AttachmentRefDropTotal,
		// RNEW-OPS-414: startup phase gauges use expvar.Int too, so the
		// same JSON-number shape pin applies.
		StartupPhaseConfigMs, StartupPhaseRouterMs, StartupPhaseShimReconnectMs,
		StartupPhasePlatformsMs, StartupPhaseSchedulerMs, StartupPhaseServerMs,
		StartupPhaseReadyMs,
	} {
		raw := c.String() // expvar.Int.String returns its JSON form
		var n json.Number
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("counter %p JSON %q not a number: %v", c, raw, err)
		}
	}
}
