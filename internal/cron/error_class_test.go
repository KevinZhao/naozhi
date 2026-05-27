// error_class_test.go pins the R238-ARCH-15 (#780) classifier contract:
// every cron sentinel maps to its expected ErrCode and HTTP status,
// nil maps to CodeOK / 200, and unknown errors map to CodeUnknown / 500.
// The classifier is the single source of truth dashboard handlers will
// converge to; drift from this test signals a missed handler update.
package cron

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestClassifyError_NilIsCodeOK(t *testing.T) {
	t.Parallel()
	if got := ClassifyError(nil); got != CodeOK {
		t.Errorf("ClassifyError(nil) = %q, want %q", got, CodeOK)
	}
	if got := CodeOK.HTTPStatus(); got != http.StatusOK {
		t.Errorf("CodeOK.HTTPStatus = %d, want 200", got)
	}
}

// TestClassifyError_AllSentinels exercises every documented sentinel.
// Adding a new sentinel without extending the switch in ClassifyError
// will flip its row to CodeUnknown — the table-driven case catches the
// regression at the test level rather than waiting for a dashboard
// handler to silently 500 in production.
func TestClassifyError_AllSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		code   ErrCode
		status int
	}{
		{"ErrJobNotFound", ErrJobNotFound, CodeJobNotFound, http.StatusNotFound},
		{"ErrAmbiguousPrefix", ErrAmbiguousPrefix, CodeAmbiguousPrefix, http.StatusConflict},
		{"ErrJobAlreadyPaused", ErrJobAlreadyPaused, CodeJobAlreadyPaused, http.StatusConflict},
		{"ErrJobNotPaused", ErrJobNotPaused, CodeJobNotPaused, http.StatusConflict},
		{"ErrJobPaused", ErrJobPaused, CodeJobPaused, http.StatusConflict},
		{"ErrJobNoPrompt", ErrJobNoPrompt, CodeJobNoPrompt, http.StatusUnprocessableEntity},
		{"ErrPersistFailed", ErrPersistFailed, CodePersistFailed, http.StatusInternalServerError},
		{"ErrInvalidPrompt", ErrInvalidPrompt, CodeInvalidPrompt, http.StatusBadRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyError(tc.err); got != tc.code {
				t.Errorf("ClassifyError(%s) = %q, want %q", tc.name, got, tc.code)
			}
			// Wrapped variant — fmt.Errorf with %w must classify the same.
			wrapped := fmt.Errorf("scheduler context: %w", tc.err)
			if got := ClassifyError(wrapped); got != tc.code {
				t.Errorf("ClassifyError(wrapped %s) = %q, want %q", tc.name, got, tc.code)
			}
			if got := tc.code.HTTPStatus(); got != tc.status {
				t.Errorf("%s.HTTPStatus = %d, want %d", tc.code, got, tc.status)
			}
		})
	}
}

// TestClassifyError_UnknownErrorMapsToCodeUnknown verifies the default
// branch surfaces unrecognised errors as 500 rather than silently
// mapping to 200 / dropping context.
func TestClassifyError_UnknownErrorMapsToCodeUnknown(t *testing.T) {
	t.Parallel()
	custom := errors.New("foo: not a cron sentinel")
	if got := ClassifyError(custom); got != CodeUnknown {
		t.Errorf("ClassifyError(custom) = %q, want %q", got, CodeUnknown)
	}
	if got := CodeUnknown.HTTPStatus(); got != http.StatusInternalServerError {
		t.Errorf("CodeUnknown.HTTPStatus = %d, want 500", got)
	}
}

// TestClassifyError_PersistFailedTakesPrecedence pins the documented
// order: ErrPersistFailed wins over downstream sentinels because it
// represents the "in-memory mutation succeeded but disk write failed"
// path that operators must triage on its own. A future caller that
// wraps both (errors.Join, multi-wrap) should still surface the
// persist failure first.
func TestClassifyError_PersistFailedTakesPrecedence(t *testing.T) {
	t.Parallel()
	// Wrap PersistFailed AROUND another sentinel so the chain contains
	// both — ClassifyError must pick PersistFailed.
	combo := fmt.Errorf("%w: secondary %w", ErrPersistFailed, ErrJobAlreadyPaused)
	if got := ClassifyError(combo); got != CodePersistFailed {
		t.Errorf("ClassifyError(persist+already_paused) = %q, want %q", got, CodePersistFailed)
	}
}

// TestClassifyError_RealMutationFailures shape-tests the integration
// with the Scheduler: build a *Scheduler that actually returns
// ErrJobNotFound from PauseJobByID and verify the classifier picks it
// up off the wrapper. Catches the case where Scheduler returns a
// formatted-string variant that has lost its sentinel identity (a
// regression we have shipped before — see persist_failure_test.go).
func TestClassifyError_RealMutationFailures(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	_, err := s.PauseJobByID("does-not-exist")
	if err == nil {
		t.Fatal("PauseJobByID on missing id should fail")
	}
	if got := ClassifyError(err); got != CodeJobNotFound {
		t.Errorf("ClassifyError(PauseJobByID-missing) = %q, want %q (err=%v)", got, CodeJobNotFound, err)
	}
}
