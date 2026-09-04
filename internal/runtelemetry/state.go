// Package runtelemetry owns the run lifecycle event vocabulary shared by
// cron, sysession and future schedulers, so all producers register a single
// Broadcaster instead of growing the hub surface per subsystem.
//
// Wire compatibility — IMPORTANT: every constant's string value IS the WS
// payload "state" / "error_class" / "trigger" field; there is no encoding
// step, and dashboard.js keys off these literals.
//
// This package MUST NOT import any other internal/* package.
package runtelemetry

// Subsystem identifies the producer of a run event so a single broadcaster
// can route to the right WS payload and OwnerID sanitiser.
type Subsystem string

const (
	SubsystemCron      Subsystem = "cron"
	SubsystemSysession Subsystem = "sysession"
	// Reserved, not yet emitted: SubsystemPlanner, SubsystemSystem.
)

// RunState is the terminal classification of a single run. Values are
// wire-stable; new states need a coordinated dashboard.js update.
type RunState string

const (
	RunStateSucceeded RunState = "succeeded"
	RunStateFailed    RunState = "failed"
	RunStateSkipped   RunState = "skipped"
	RunStateTimedOut  RunState = "timed_out"
	RunStateCanceled  RunState = "canceled"
)

// ErrorClass is the machine-readable failure dimension (wire value).
// Cross-subsystem classes carry no prefix; subsystem-specific ones keep
// their wire string verbatim ("session_error", NOT "cron.session_error").
// Adding a class MUST update wire_stability_test.go; two constants with the
// same wire string is a test failure.
type ErrorClass string

const (
	ErrClassNone             ErrorClass = ""
	ErrClassDeadlineExceeded ErrorClass = "deadline_exceeded"
	ErrClassCanceled         ErrorClass = "canceled"
	ErrClassPanic            ErrorClass = "panic"

	// cron-specific.
	ErrClassCronSessionError       ErrorClass = "session_error"
	ErrClassCronSendError          ErrorClass = "send_error"
	ErrClassCronWorkDirUnreachable ErrorClass = "workdir_unreachable"
	ErrClassCronWorkDirOutsideRoot ErrorClass = "workdir_outside_root"
	ErrClassCronOverlapSkipped     ErrorClass = "overlap_skipped"

	// cron sandbox placement (agentcore-cloud-sandbox RFC §6.1): distinct
	// because they differ in replay safety — "sandbox_failed" is the CLI's
	// attested failure (replay reasonably safe); "sandbox_transport" means
	// the stream broke without attestation (§6.2 containment, red badge).
	// RunState stays "failed"; the badge derives from error_class.
	ErrClassCronSandboxFailed    ErrorClass = "sandbox_failed"
	ErrClassCronSandboxTransport ErrorClass = "sandbox_transport"
	// ErrClassCronSandboxUnavailable: placement=sandbox but no sandbox
	// executor wired. Permanent config error, not a transient failure.
	ErrClassCronSandboxUnavailable ErrorClass = "sandbox_unavailable"

	// sysession-specific.
	ErrClassSysessionUpstream   ErrorClass = "upstream"
	ErrClassSysessionValidation ErrorClass = "validation"
)

// TriggerKind names how a run was initiated. TriggerCatchup is reserved for
// a future missed-schedule replay path; consumers must treat unknown
// trigger strings as forward-compatible.
type TriggerKind string

const (
	TriggerScheduled TriggerKind = "scheduled"
	TriggerManual    TriggerKind = "manual"
	TriggerCatchup   TriggerKind = "catchup"
)
