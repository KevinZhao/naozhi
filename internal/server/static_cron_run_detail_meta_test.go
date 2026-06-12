package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronRunDetailMetaBar pins the §7.3 run-detail meta bar +
// §7.5 cost小字 contracts on cron_view.js (per-file, no union — the
// cron_view.js split lesson). The meta bar is the UI face of PR-1's
// run-record receipt; the cost helpers are the §7.5 visibility.
func TestDashboardJS_CronRunDetailMetaBar(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)

	for _, frag := range []string{
		// meta bar builder reads the detail endpoint's `sandbox` receipt.
		"function cronSandboxMetaBarHtml(sb)",
		"const metaBar = cronSandboxMetaBarHtml(detail.sandbox);",
		"return metaBar + body;",
		// receipt fields surfaced.
		"sb.image_version",
		"sb.memory_peak_bytes",
		"sb.cost_usd",
		"sb.exit_status",
		// cost helpers (§7.5).
		"function formatCostUSD(usd)",
		"function formatBytes(n)",
		// per-run cost chip + monthly aggregate.
		"r.cost_usd",
		"function cronTimelineCostSummaryHtml(runs)",
		"ct-cost-sum",
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing meta-bar/cost fragment %q", frag)
		}
	}
}

// TestDashboardHTML_CronRunDetailMetaCSS pins the meta-bar CSS classes.
func TestDashboardHTML_CronRunDetailMetaCSS(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	for _, frag := range []string{
		".ctr-meta-bar{",
		".ctr-meta-item{",
		".ctr-meta-item.ctr-meta-cost{",
		".ct-cost-sum{",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("dashboard.html missing meta CSS %q", frag)
		}
	}
}
