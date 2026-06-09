// error_class_parity_test.go pins the string coupling between the
// cron-local ErrorClass constants (job.go) and the runtelemetry
// canonical constants (state.go). The two types are NOT aliases —
// cron.ErrorClass is a separate type cast to runtelemetry.ErrorClass
// in scheduler_callbacks.go via string conversion. This test turns that
// implicit string coupling into an explicit compile+test contract so a
// value rename in either package fails loudly here rather than silently
// shipping a mismatched wire value to the dashboard.
//
// Coverage: all 8 cron-specific error class values. The 3 shared values
// (ErrClassDeadlineExceeded / ErrClassCanceled / ErrClassPanic) are
// defined only in runtelemetry and forwarded as cron aliases in job.go —
// their identity is already pinned by runtelemetry's own wire_stability_test.
package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// TestErrorClassParity_CronVsRuntelemetry asserts that each cron-local
// ErrorClass constant carries the same wire string as the corresponding
// runtelemetry constant. Fails if either side is renamed without updating
// the other.
func TestErrorClassParity_CronVsRuntelemetry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cron ErrorClass
		tel  runtelemetry.ErrorClass
	}{
		{
			name: "session_error",
			cron: ErrClassSessionError,
			tel:  runtelemetry.ErrClassCronSessionError,
		},
		{
			name: "send_error",
			cron: ErrClassSendError,
			tel:  runtelemetry.ErrClassCronSendError,
		},
		{
			name: "workdir_unreachable",
			cron: ErrClassWorkDirUnreachable,
			tel:  runtelemetry.ErrClassCronWorkDirUnreachable,
		},
		{
			name: "workdir_outside_root",
			cron: ErrClassWorkDirOutsideRoot,
			tel:  runtelemetry.ErrClassCronWorkDirOutsideRoot,
		},
		{
			name: "overlap_skipped",
			cron: ErrClassOverlapSkipped,
			tel:  runtelemetry.ErrClassCronOverlapSkipped,
		},
		{
			name: "router_missing",
			cron: ErrClassRouterMissing,
			tel:  runtelemetry.ErrClassCronRouterMissing,
		},
		{
			name: "paused_concurrent",
			cron: ErrClassPausedConcurrent,
			tel:  runtelemetry.ErrClassCronPausedConcurrent,
		},
		{
			name: "deleted_concurrent",
			cron: ErrClassDeletedConcurrent,
			tel:  runtelemetry.ErrClassCronDeletedConcurrent,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cronWire := string(tc.cron)
			telWire := string(tc.tel)
			if cronWire != telWire {
				t.Errorf("wire mismatch for %s: cron=%q runtelemetry=%q",
					tc.name, cronWire, telWire)
			}
		})
	}
}
