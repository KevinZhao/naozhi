package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronCompactPoll pins the dashboard-side half of the
// R236-SEC-08 (#494) compact-mode contract:
//
//  1. The 1 Hz poll path (fetchCronJobs) hits /api/cron?compact=1 so the
//     wire shape carries 256-byte clipped prompts instead of the prior
//     8 KiB × N-job bandwidth.
//  2. cronRefetchFullJob exists as the lazy refetch helper that swaps a
//     truncated cache row for the full body.
//  3. editCronJob wires through cronRefetchFullJob before opening the
//     modal so saving doesn't silently persist a 256-byte truncation
//     back into cron_jobs.json.
//  4. openCronDetail kicks off cronRefetchFullJob in the background so
//     the drawer's prompt section ends up showing the full body without
//     blocking the open animation.
//
// Without these gates the compact opt-in either silently corrupts saves
// (case 3 — the load-bearing data-loss invariant) or shows a clipped
// preview where the user expects the whole prompt (cases 1, 2, 4).
func TestDashboardJS_CronCompactPoll(t *testing.T) {
	t.Parallel()

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. Poll URL carries ?compact=1.
	if !strings.Contains(js, "/api/cron?compact=1") {
		t.Error("dashboard.js: fetchCronJobs must hit /api/cron?compact=1 (R236-SEC-08 / #494): " +
			"the bandwidth-bounded wire shape is opt-in via this exact query param; " +
			"any other shape (compact=true, no param) regresses to the legacy 400 KiB/s polling.")
	}

	// 2. Refetch helper exists.
	if !strings.Contains(js, "async function cronRefetchFullJob(") {
		t.Error("dashboard.js: cronRefetchFullJob(id) must exist — it is the lazy " +
			"full-prompt fetch the editor / drawer use to re-hydrate truncated rows " +
			"before the user can save.")
	}

	// 3. Editor open path routes through the refetch helper. We grep for
	// the call site rather than the function definition so a future
	// rename of editCronJob's body still keeps the wiring under test.
	if !strings.Contains(js, "cronRefetchFullJob(id).then(job =>") {
		t.Error("dashboard.js: editCronJob must `cronRefetchFullJob(id).then(...)` before " +
			"opening the modal; otherwise saving a truncated cache row would silently " +
			"persist 256 bytes back to disk (data-loss regression for #494).")
	}

	// 4. Drawer open path triggers a background refetch.
	if !strings.Contains(js, "cronRefetchFullJob(jobId).then") {
		t.Error("dashboard.js: openCronDetail must call cronRefetchFullJob(jobId) so the " +
			"drawer's 做什么 section shows the full prompt rather than the cached " +
			"256-byte preview.")
	}
}
