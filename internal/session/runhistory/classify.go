package runhistory

import (
	"context"
	"errors"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// Classify maps a Send/SendPassthrough error to an (Outcome, ErrorClass)
// pair. A nil error is a completed run. Timeout sentinels come from the cli
// package (session already depends on cli); generic context errors map to
// runtelemetry's shared error classes so the wire value stays consistent
// with cron/sysession without introducing a session->cron dependency.
func Classify(err error) (Outcome, runtelemetry.ErrorClass) {
	switch {
	case err == nil:
		return OutcomeCompleted, runtelemetry.ErrClassNone
	case errors.Is(err, cli.ErrTotalTimeout), errors.Is(err, cli.ErrNoOutputTimeout):
		return OutcomeTimeout, runtelemetry.ErrClassDeadlineExceeded
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout, runtelemetry.ErrClassDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return OutcomeCanceled, runtelemetry.ErrClassCanceled
	default:
		return OutcomeError, runtelemetry.ErrClassNone
	}
}
