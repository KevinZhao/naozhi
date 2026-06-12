package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronSnapshotPanel pins the §7.3 input-snapshot panel
// contracts on cron_view.js (per-file, no union). The panel is the replay
// preview; the secret-refs render must show NAMES only (§5.1 — the server
// never sends values, and the JS has none to leak).
func TestDashboardJS_CronSnapshotPanel(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)

	for _, frag := range []string{
		"function cronSnapshotPanelHtml(snap)",
		"if (!snap || !snap.available) return '';",
		"function cronTimelineFetchSnapshot(jobId, runId)",
		"/snapshot?job_id=",
		"detail.__snapshot",
		// panel renders secret REF names (not values).
		"snap.secret_refs",
		"输入快照（可重放）",
		// snapshot panel is appended to the detail body.
		"const snapshotPanel = cronSnapshotPanelHtml(detail.__snapshot);",
		"return body + snapshotPanel;",
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing snapshot-panel fragment %q", frag)
		}
	}
}

// TestDashboardHTML_CronSnapshotPanelCSS pins the panel CSS classes.
func TestDashboardHTML_CronSnapshotPanelCSS(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	for _, frag := range []string{
		".ctr-snapshot{",
		".ctr-snap-body{",
		".ctr-snap-pre{",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("dashboard.html missing snapshot CSS %q", frag)
		}
	}
}
