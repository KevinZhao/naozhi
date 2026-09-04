// error_class.go centralises the mapping from Scheduler sentinel errors to a
// stable error code + HTTP status, so dashboard handlers share one classifier
// instead of per-handler errors.Is switches (#780). Adding a sentinel means:
// add a constant, extend ClassifyError, extend errCodeHTTP.
package cron

import (
	"errors"
	"net/http"
)

// ErrCode is a stable enum identifying a cron-side failure category. The
// string form is stable — callers may persist or wire-serialise it. The Code*
// prefix mirrors http.Status* so it does not collide with the Err* sentinels.
type ErrCode string

const (
	// CodeOK is the zero value; ClassifyError(nil) returns CodeOK so callers
	// can treat the function as a total mapping. HTTP 200.
	CodeOK ErrCode = ""

	// CodeJobNotFound — ErrJobNotFound chain. HTTP 404.
	CodeJobNotFound ErrCode = "job_not_found"

	// CodeAmbiguousPrefix — ErrAmbiguousPrefix chain (IM /cron <prefix>
	// matched multiple jobs). HTTP 409.
	CodeAmbiguousPrefix ErrCode = "ambiguous_prefix"

	// CodeJobAlreadyPaused — PauseJob on an already-paused job. HTTP 409.
	CodeJobAlreadyPaused ErrCode = "job_already_paused"

	// CodeJobNotPaused — ResumeJob on a job that is not paused. HTTP 409.
	CodeJobNotPaused ErrCode = "job_not_paused"

	// CodeJobPaused — TriggerNow on a paused job. HTTP 409.
	CodeJobPaused ErrCode = "job_paused"

	// CodeJobNoPrompt — TriggerNow on a job with no prompt configured. HTTP 422.
	CodeJobNoPrompt ErrCode = "job_no_prompt"

	// CodePersistFailed — post-mutation persist failed after the in-memory
	// mutation happened. HTTP 500: a restart would replay the un-persisted
	// state, so the operator MUST inspect logs.
	CodePersistFailed ErrCode = "persist_failed"

	// CodeInvalidPrompt — ValidatePromptStrict policy violation. HTTP 400.
	CodeInvalidPrompt ErrCode = "invalid_prompt"

	// CodePromptAlreadySet — SetJobPrompt on a job that already has a
	// non-empty prompt (use UpdateJob to change it). HTTP 409.
	CodePromptAlreadySet ErrCode = "prompt_already_set"

	// CodeSchedulerStopped — a mutation/Start arrived after Stop() latched.
	// HTTP 503: retrying against this instance will never succeed.
	CodeSchedulerStopped ErrCode = "scheduler_stopped"

	// CodeUnknown — non-nil error not matching any known sentinel. HTTP 500.
	CodeUnknown ErrCode = "unknown"
)

// ClassifyError maps a Scheduler-returned error to its ErrCode by walking
// the chain via errors.Is. Returns CodeOK on nil and CodeUnknown for non-nil
// errors matching no sentinel.
//
// ErrPersistFailed is checked first: it can appear alongside a state
// sentinel (mutation already applied, disk write failed) and the operator
// action it demands must win.
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

// errCodeHTTP is the authoritative mapping from ErrCode to HTTP status.
// Adding a new ErrCode constant requires a corresponding entry here;
// TestErrCodeHTTP_Exhaustive will fail at test time if one is missing.
var errCodeHTTP = map[ErrCode]int{
	CodeOK:               http.StatusOK,
	CodeJobNotFound:      http.StatusNotFound,
	CodeAmbiguousPrefix:  http.StatusConflict,
	CodeJobAlreadyPaused: http.StatusConflict,
	CodeJobNotPaused:     http.StatusConflict,
	CodeJobPaused:        http.StatusConflict,
	CodeJobNoPrompt:      http.StatusUnprocessableEntity,
	CodePersistFailed:    http.StatusInternalServerError,
	CodeInvalidPrompt:    http.StatusBadRequest,
	CodePromptAlreadySet: http.StatusConflict,
	CodeSchedulerStopped: http.StatusServiceUnavailable,
	CodeUnknown:          http.StatusInternalServerError,
}

// HTTPStatus returns the HTTP status code dashboard handlers should emit
// for this ErrCode. Unknown codes map to 500 so a forgotten errCodeHTTP
// entry stays observable rather than silently 200ing.
func (c ErrCode) HTTPStatus() int {
	if s, ok := errCodeHTTP[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}
