// error_class.go centralises the mapping from Scheduler sentinel errors
// to a stable error code + HTTP status. R238-ARCH-15 (#780): every cron
// dashboard handler used to write a 6-arm `errors.Is` switch on
// (ErrJobNotFound / ErrAmbiguousPrefix / ErrJobAlreadyPaused /
// ErrJobNotPaused / ErrJobPaused / ErrJobNoPrompt / ErrPersistFailed /
// ErrInvalidPrompt) — repeated verbatim at five HTTP entry points,
// drifting subtly between them as new sentinels were added.
//
// This file is the single in-package classifier. ClassifyError walks
// the error chain via errors.Is and returns a code; (Code).HTTPStatus
// returns the HTTP status the dashboard handlers historically picked
// per branch. The legacy sentinels stay (callers that already
// errors.Is against them keep working — drop is left for a separate
// follow-up). Adding a new sentinel now means: add a constant here,
// extend ClassifyError, extend (Code).HTTPStatus once. Handlers will
// pick up the new mapping automatically.
package cron

import (
	"errors"
	"net/http"
)

// ErrCode is a stable enum identifying a cron-side failure category.
// Used by ClassifyError to surface a single value that downstream HTTP
// / IM handlers can map to a user-facing response. The string form is
// stable — callers may persist or wire-serialise it (e.g. error_code
// field in JSON responses) without churning when new codes are added.
//
// Naming: Code* prefix mirrors Go's stdlib http.Status* constants so
// the code reads "ErrCode == cron.CodeJobNotFound" without ambiguity
// with the sentinel name.
type ErrCode string

const (
	// CodeOK is the zero value; ClassifyError(nil) returns CodeOK so
	// callers can treat the function as a total mapping. Maps to 200.
	CodeOK ErrCode = ""

	// CodeJobNotFound — ErrJobNotFound chain. HTTP 404.
	CodeJobNotFound ErrCode = "job_not_found"

	// CodeAmbiguousPrefix — ErrAmbiguousPrefix chain (IM /cron <prefix>
	// matched multiple jobs). HTTP 409 (conflict-style; the request was
	// well-formed but cannot be satisfied without operator
	// disambiguation).
	CodeAmbiguousPrefix ErrCode = "ambiguous_prefix"

	// CodeJobAlreadyPaused — PauseJob on a job that is already paused.
	// HTTP 409 (state conflict).
	CodeJobAlreadyPaused ErrCode = "job_already_paused"

	// CodeJobNotPaused — ResumeJob on a job that is not paused. HTTP 409.
	CodeJobNotPaused ErrCode = "job_not_paused"

	// CodeJobPaused — TriggerNow on a paused job. HTTP 409.
	CodeJobPaused ErrCode = "job_paused"

	// CodeJobNoPrompt — TriggerNow on a job with no prompt configured.
	// HTTP 422 (unprocessable: request well-formed, target state
	// missing the required prompt field).
	CodeJobNoPrompt ErrCode = "job_no_prompt"

	// CodePersistFailed — post-mutation persist returned an error
	// (in-memory mutation already happened). HTTP 500: a restart would
	// replay the un-persisted state, so the operator MUST inspect logs.
	CodePersistFailed ErrCode = "persist_failed"

	// CodeInvalidPrompt — ValidatePromptStrict policy violation.
	// HTTP 400 (input validation).
	CodeInvalidPrompt ErrCode = "invalid_prompt"

	// CodePromptAlreadySet — SetJobPrompt on a job that already has a
	// non-empty prompt (use UpdateJob to change it). HTTP 409 (state
	// conflict: the target already holds a prompt). R103901-ARCH-2: was
	// previously falling through to CodeUnknown→500.
	CodePromptAlreadySet ErrCode = "prompt_already_set"

	// CodeSchedulerStopped — a mutation/Start arrived after Stop() latched.
	// HTTP 503 (service unavailable: the scheduler is shutting down /
	// gone, retrying against this instance will never succeed).
	// R103901-ARCH-2: was previously falling through to CodeUnknown→500.
	CodeSchedulerStopped ErrCode = "scheduler_stopped"

	// CodeUnknown — non-nil error not matching any known sentinel.
	// HTTP 500: surface the failure and let the caller log the raw
	// error for triage.
	CodeUnknown ErrCode = "unknown"
)

// ClassifyError maps a Scheduler-returned error to its ErrCode by
// walking the chain via errors.Is. Returns CodeOK on a nil error and
// CodeUnknown for non-nil errors that do not match any sentinel.
//
// Order matters: ErrPersistFailed wraps the raw marshal error and
// usually appears alongside CodeOK-class state mutations that already
// happened — checked first so a "persisted PauseJob then disk wrote
// failed" surfaces as CodePersistFailed (operator action) rather than
// the secondary ErrJobAlreadyPaused that the prior errors.Is chain
// might match (the rollback path doesn't wrap that, but future
// rollback variants might).
//
// New sentinel? Append a case here, add a constant above, extend
// (ErrCode).HTTPStatus.
func ClassifyError(err error) ErrCode {
	if err == nil {
		return CodeOK
	}
	switch {
	case errors.Is(err, ErrPersistFailed):
		return CodePersistFailed
	case errors.Is(err, ErrJobNotFound):
		return CodeJobNotFound
	case errors.Is(err, ErrAmbiguousPrefix):
		return CodeAmbiguousPrefix
	case errors.Is(err, ErrJobAlreadyPaused):
		return CodeJobAlreadyPaused
	case errors.Is(err, ErrJobNotPaused):
		return CodeJobNotPaused
	case errors.Is(err, ErrJobPaused):
		return CodeJobPaused
	case errors.Is(err, ErrJobNoPrompt):
		return CodeJobNoPrompt
	case errors.Is(err, ErrInvalidPrompt):
		return CodeInvalidPrompt
	case errors.Is(err, ErrPromptAlreadySet):
		return CodePromptAlreadySet
	case errors.Is(err, ErrSchedulerStopped):
		return CodeSchedulerStopped
	default:
		return CodeUnknown
	}
}

// HTTPStatus returns the HTTP status code dashboard handlers should
// emit for this ErrCode. Centralising the mapping eliminates the
// per-handler 6-arm switch documented above. Maps unknown codes to
// http.StatusInternalServerError so a forgotten case stays observable
// rather than silently 200ing.
func (c ErrCode) HTTPStatus() int {
	switch c {
	case CodeOK:
		return http.StatusOK
	case CodeJobNotFound:
		return http.StatusNotFound
	case CodeAmbiguousPrefix,
		CodeJobAlreadyPaused,
		CodeJobNotPaused,
		CodeJobPaused:
		return http.StatusConflict
	case CodeJobNoPrompt:
		return http.StatusUnprocessableEntity
	case CodePersistFailed:
		return http.StatusInternalServerError
	case CodeInvalidPrompt:
		return http.StatusBadRequest
	case CodePromptAlreadySet:
		return http.StatusConflict
	case CodeSchedulerStopped:
		return http.StatusServiceUnavailable
	case CodeUnknown:
		return http.StatusInternalServerError
	default:
		// Forward-compat: a future ErrCode constant added without
		// extending this switch should default to 500 — surface the
		// gap rather than silently mapping to 200.
		return http.StatusInternalServerError
	}
}
