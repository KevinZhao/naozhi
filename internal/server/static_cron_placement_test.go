package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronPlacementSelector pins the placement selector +
// ☁️ badge contracts from agentcore-cloud-sandbox RFC §7.1/§7.2. Per-file
// anchors on cron_view.js only (per-file, no union — the cron_view.js
// split lesson, PR #1954): every fragment asserted here must live in
// cron_view.js itself, so a future script split cannot silently move a
// handler out of load order.
func TestDashboardJS_CronPlacementSelector(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)

	// §7.1: the selector renders in BOTH create and edit modals with
	// distinct ids, default 本机, sandbox labelled with the cloud glyph.
	for _, frag := range []string{
		`buildCronPlacementHtml('', 'cron-placement')`,
		`buildCronPlacementHtml(job.placement || '', 'edit-cron-placement')`,
		`aria-label="运行位置"`,
		`<option value=""`,
		`<option value="sandbox"`,
		"云沙箱 ☁️",
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing placement-selector fragment %q", frag)
		}
	}

	// §7.1 fence: submit paths must client-validate the sandbox×work_dir
	// conflict BEFORE the request (server still re-validates; this pins
	// the precise-toast UX). Create checks the collected workDir; edit
	// checks the EFFECTIVE work_dir (patched value else job's current).
	for _, frag := range []string{
		"云沙箱暂不支持工作目录：请清空",
		"云沙箱暂不支持工作目录：请先清空",
		`('work_dir' in body) ? body.work_dir : (job.work_dir || '')`,
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing sandbox×work_dir fence fragment %q", frag)
		}
	}

	// Pointer-PATCH semantics: edit only sends placement when changed.
	if !strings.Contains(js, "if (newPlacement !== origPlacement)") {
		t.Error("doEditCronJob must PATCH placement only on change (pointer semantics)")
	}
}

// TestDashboardJS_CronPlacementBadge pins the ☁️ badge three-state contract
// (RFC §7.2): success default / failed yellow / transport red+⚠. The badge
// is the UI face of the §6.2 double-run containment — the transport state
// is a safety signal, not decoration.
func TestDashboardJS_CronPlacementBadge(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)

	for _, frag := range []string{
		"function cronPlacementBadgeHtml(placement, errorClass)",
		`if (placement !== 'sandbox') return '';`,
		"pl-transport",
		"pl-failed",
		"☁️ 沙箱 ⚠",
		// badge wired into the job card icon row
		"cronPlacementBadgeHtml(j.placement || '', j.last_error_class || '')",
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing badge fragment %q", frag)
		}
	}

	// Error-class localisation for the three sandbox classes.
	for _, frag := range []string{
		`case 'sandbox_failed': return '云沙箱任务失败';`,
		`case 'sandbox_transport': return '云沙箱断流（状态未知）';`,
		`case 'sandbox_unavailable': return '云沙箱未配置';`,
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing sandbox error-class label %q", frag)
		}
	}
}

// TestDashboardHTML_CronPlacementBadgeCSS pins the three badge state
// classes in the embedded stylesheet.
func TestDashboardHTML_CronPlacementBadgeCSS(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	for _, frag := range []string{
		".cj-placement-sandbox{",
		".cj-placement-sandbox.pl-failed{",
		".cj-placement-sandbox.pl-transport{",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("dashboard.html missing badge CSS %q", frag)
		}
	}
}
