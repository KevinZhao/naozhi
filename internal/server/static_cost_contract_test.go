package server

import (
	"os"
	"strings"
	"testing"
)

// TestStaticCostLedgerContract pins the dashboard's cost-ledger wiring
// (docs/rfc/cost-ledger.md §8): the 服务概览 card reads /api/cost/summary
// with a session-sum fallback, cron drawers fetch the per-job 30-day figure,
// and each file defines the helpers it calls (multi-script split rule).
func TestStaticCostLedgerContract(t *testing.T) {
	t.Parallel()
	dash, err := os.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatal(err)
	}
	cron, err := os.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/api/cost/summary?group_by=unit", "function summarizeCostBuckets", "function costStatHtml",
		"function refreshCostSummary", "近 30 天花费", "累计花费", "costCardTitle(c)", "svc-stat-sub",
	} {
		if !strings.Contains(string(dash), want) {
			t.Errorf("dashboard.js missing %q", want)
		}
	}
	for _, want := range []string{
		"/api/cost/summary?group_by=job", "function cronJobCostRefresh", "function cronJobLedgerCostHtml",
		"cronJobCostRefresh(jobId)", "ct-cost-ledger",
	} {
		if !strings.Contains(string(cron), want) {
			t.Errorf("cron_view.js missing %q", want)
		}
	}
	for _, want := range []string{".svc-stat-sub{", ".ct-cost-ledger{"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("dashboard.html missing style %q", want)
		}
	}
	// Units never mix: the USD figure and the credits sub-line are separate nodes.
	if !strings.Contains(string(dash), "c.credits.toFixed(2)) + ' credits") {
		t.Error("credits must render on their own sub-line, never summed into the USD value")
	}
}
