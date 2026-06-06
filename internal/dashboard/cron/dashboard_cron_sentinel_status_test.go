package cron

import (
	"net/http"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandlerDefaultArm_SentinelHTTPStatus pins R20260603-ARCH-1: the default
// arms of HandleDelete / HandlePause / HandleResume / HandleTrigger previously
// hard-coded http.StatusBadRequest (400), causing ErrSchedulerStopped (should
// be 503) and unknown errors (should be 500) to be mapped to 400.
//
// After the fix the default arm delegates to
// cronpkg.ClassifyError(err).HTTPStatus() instead.  This test asserts:
//   - ErrSchedulerStopped → CodeSchedulerStopped → 503
//   - ErrPromptAlreadySet → CodePromptAlreadySet → 409
//   - unknown error       → CodeUnknown          → 500
//
// All three would have returned 400 before the fix.  The handler code path
// (ClassifyError → HTTPStatus) is the same code that is now called from each
// default arm, so testing ClassifyError.HTTPStatus for these sentinels is
// the correct unit-level coverage of the fix.
//
// Note: ErrSchedulerStopped is only returned by Scheduler.Start(), not by the
// mutation methods themselves, so we cannot trigger it end-to-end through a
// handler HTTP request with the concrete *Scheduler type.  The sentinel
// mapping is tested here at the function level that the handlers now call.
func TestHandlerDefaultArm_SentinelHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "ErrSchedulerStopped_maps_503_not_400",
			err:        cronpkg.ErrSchedulerStopped,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "ErrPromptAlreadySet_maps_409_not_400",
			err:        cronpkg.ErrPromptAlreadySet,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "unknown_error_maps_500_not_400",
			err:        &unknownCronErr{msg: "some unexpected condition"},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code := cronpkg.ClassifyError(tc.err)
			got := code.HTTPStatus()
			if got != tc.wantStatus {
				t.Errorf("ClassifyError(%v).HTTPStatus() = %d, want %d — "+
					"handler default arm maps this sentinel to the wrong HTTP status [R20260603-ARCH-1]",
					tc.err, got, tc.wantStatus)
			}
			// All three must also differ from 400, the pre-fix hard-coded value.
			if got == http.StatusBadRequest {
				t.Errorf("ClassifyError(%v).HTTPStatus() = 400 (pre-fix value) — fix not applied", tc.err)
			}
		})
	}
}

// unknownCronErr is a test-local error type that does not match any cron
// sentinel, exercising the CodeUnknown → 500 path in ClassifyError.
type unknownCronErr struct{ msg string }

func (e *unknownCronErr) Error() string { return e.msg }
