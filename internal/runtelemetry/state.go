// Package runtelemetry owns the cross-subsystem run lifecycle event types
// shared between cron, sysession, and (future) planner / system schedulers.
//
// Why a dedicated package: cron and sysession historically each defined
// their own RunState / ErrorClass / TriggerKind / *RunStartedEvent /
// *RunEndedEvent. The dashboard hub then exposed two parallel broadcast
// methods (BroadcastCronRun* / BroadcastDaemonRun*) that did 95% the same
// thing. Centralising the event vocabulary lets both producers register a
// single Broadcaster, and lets new subsystems plug in without growing the
// hub surface.
//
// Wire compatibility — IMPORTANT: every constant's string value IS the WS
// payload "state" / "error_class" / "trigger" field. There is no encoding
// step. Values are deliberately chosen to match the pre-merge cron and
// sysession wires verbatim, so existing dashboard.js handlers keep working
// without a coordinated frontend rev.
//
// This package MUST NOT import any other internal/* package; it is a
// leaf-level vocabulary package.
package runtelemetry

// Subsystem identifies the producer of a run event so a single broadcaster
// can route to the right per-subsystem WS payload (cron_run_* vs
// daemon_run_*) and select the right OwnerID sanitiser.
type Subsystem string

const (
	SubsystemCron      Subsystem = "cron"
	SubsystemSysession Subsystem = "sysession"
	// Reserved (not yet emitted by any producer):
	//   SubsystemPlanner  — future planner-auto-start scheduler
	//   SubsystemSystem   — future system-session daemon orchestrator
)

// RunState is the terminal classification of a single run.
//
// Values are wire-stable; both cron and sysession already use these
// strings on the wire pre-merge. New states require a coordinated wire
// schema bump and dashboard.js update.
type RunState string

const (
	RunStateSucceeded RunState = "succeeded"
	RunStateFailed    RunState = "failed"
	RunStateSkipped   RunState = "skipped"
	RunStateTimedOut  RunState = "timed_out"
	RunStateCanceled  RunState = "canceled"
)

// ErrorClass is the machine-readable failure dimension. Each constant's
// string value IS the WS payload `error_class` field — no encoding step.
//
// Naming convention:
//   - Cross-subsystem (shared semantics): no prefix.
//     ("canceled", "deadline_exceeded", "panic", "")
//   - Subsystem-specific: value mirrors the existing pre-merge wire string
//     verbatim. "session_error" stays "session_error", NOT
//     "cron.session_error" — the dashboard JS already keys off these
//     literals and changing the wire is out-of-scope for this RFC.
//
// Adding a new ErrorClass MUST update wire_stability_test.go to re-pin
// the freeze. Two constants with the same wire string is a test failure
// (enforced by wire_stability_test).
type ErrorClass string

const (
	ErrClassNone             ErrorClass = ""
	ErrClassDeadlineExceeded ErrorClass = "deadline_exceeded"
	ErrClassCanceled         ErrorClass = "canceled"
	ErrClassPanic            ErrorClass = "panic"

	// cron-specific (wire values match current cron package).
	ErrClassCronSessionError       ErrorClass = "session_error"
	ErrClassCronSendError          ErrorClass = "send_error"
	ErrClassCronWorkDirUnreachable ErrorClass = "workdir_unreachable"
	ErrClassCronWorkDirOutsideRoot ErrorClass = "workdir_outside_root"
	ErrClassCronOverlapSkipped     ErrorClass = "overlap_skipped"
	ErrClassCronRouterMissing      ErrorClass = "router_missing"
	ErrClassCronPausedConcurrent   ErrorClass = "paused_concurrent"
	ErrClassCronDeletedConcurrent  ErrorClass = "deleted_concurrent"

	// sysession-specific (wire values match current sysession package).
	ErrClassSysessionUpstream   ErrorClass = "upstream"
	ErrClassSysessionValidation ErrorClass = "validation"
)

// TriggerKind names how a run was initiated.
//
// TriggerCatchup is reserved for a future missed-schedule replay path
// (cron P3); no production code emits it today. Consumers must treat
// unknown trigger strings as forward-compatible and not assume the set
// is closed.
type TriggerKind string

const (
	TriggerScheduled TriggerKind = "scheduled"
	TriggerManual    TriggerKind = "manual"
	TriggerCatchup   TriggerKind = "catchup"
)
