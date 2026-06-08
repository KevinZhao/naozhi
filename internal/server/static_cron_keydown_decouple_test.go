package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronKeydownDecoupled is the regression guard for the
// `cronExpandedRunId is not defined` runtime crash
// (dashboard-cron-view-extraction RFC §2.6 B1).
//
// Root cause: PR-1 moved the inline-expand state `cronExpandedRunId` (and the
// cron drawer state `cronDetailJobId`) out of dashboard.js into cron_view.js,
// but left dashboard.js's two GLOBAL keydown handlers (Esc + ↑↓) referencing
// those symbols by bare name. dashboard.js and cron_view.js load as two
// separate classic <script> tags, so the top-level `const cronExpandedRunId`
// is only visible across the boundary once cron_view.js has actually executed.
// When cron_view.js fails to load (deploy-time cache skew: a stale
// dashboard.html missing the cron_view.js <script> tag, paired with a fresh
// dashboard.js), pressing Esc or ↑/↓ throws
// `ReferenceError: cronExpandedRunId is not defined`, which the global error
// handler surfaces as the "页面遇到异常" toast.
//
// Fix + invariant: dashboard.js's global keydown handlers must NOT reference
// any cron-internal symbol across the script boundary. The Esc branch routes
// through the optional `window.nzCronEscClose && window.nzCronEscClose()`
// delegate, and the ↑↓ handler lives in cron_view.js next to the state it
// reads. cron_view.js absent ⇒ the cron shortcuts simply don't fire;
// dashboard.js never throws.
//
// This test reads dashboard.js ALONE (not the union with cron_view.js) so it
// fails on the pre-fix code and passes on the fixed code. Comments are stripped
// with the shared stripJSComments helper (handles // and /* */, respects string
// literals) so the file's own prose — which deliberately names the banned
// symbols to document the decoupling — does not produce false positives.
func TestDashboardJS_CronKeydownDecoupled(t *testing.T) {
	t.Parallel()

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	code := stripJSComments(string(data))

	// cron-internal symbols that live in cron_view.js. dashboard.js referencing
	// any of these by bare name is a cross-script ReferenceError waiting to
	// happen the moment cron_view.js fails to load. The Esc/↑↓ handlers must
	// reach cron behavior only through the window.nzCronEscClose delegate.
	for _, sym := range []string{
		"cronExpandedRunId",
		"navigateExpandedRun",
		"cronTimelineCollapse",
		"cronTimelineExpand",
	} {
		if strings.Contains(code, sym) {
			t.Errorf("dashboard.js 裸引用 cron 内部符号 %q（代码，非注释）—\n"+
				"  → cron_view.js 未加载时这是 ReferenceError，正是 "+
				"`cronExpandedRunId is not defined` 的根因。改经 window.nzCronEscClose() "+
				"委托，或把 handler 迁入 cron_view.js（B1 解耦）。", sym)
		}
	}

	// Positive: the Esc delegate must be present (so the cron Esc branch still
	// works) and guarded (so an absent cron_view.js degrades gracefully rather
	// than throwing "window.nzCronEscClose is not a function").
	if !strings.Contains(code, "window.nzCronEscClose && window.nzCronEscClose()") {
		t.Error("dashboard.js: Global Esc handler 必须经 guarded `window.nzCronEscClose && " +
			"window.nzCronEscClose()` 委托关 cron 层 — 裸调用在 cron_view.js 未加载时仍会抛错")
	}
}

// TestCronViewJS_OwnsKeydownAndEscDelegate pins the other half of the B1
// decoupling: cron_view.js owns the cron keyboard surface it depends on.
//   - exports window.nzCronEscClose = cronEscClose (the delegate dashboard.js calls)
//   - registers its own ↑↓ keydown listener (moved from dashboard.js)
//   - that listener calls navigateExpandedRun, which is defined here
func TestCronViewJS_OwnsKeydownAndEscDelegate(t *testing.T) {
	t.Parallel()

	cronData, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	cronJS := string(cronData)
	dashData, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	dashCode := stripJSComments(string(dashData))

	if !strings.Contains(cronJS, "window.nzCronEscClose = cronEscClose") {
		t.Error("cron_view.js: 必须导出 window.nzCronEscClose = cronEscClose（dashboard.js 委托入口）")
	}
	if !strings.Contains(cronJS, "function cronEscClose()") {
		t.Error("cron_view.js: 缺少 function cronEscClose() — Esc 关闭决策必须收在 cron 模块内")
	}
	// The ↑↓ handler must be registered in cron_view.js (next to the
	// cronExpandedRunId state it reads), NOT in dashboard.js. Assert the call
	// site exists here and is absent from dashboard.js — robust against trivial
	// reformatting of the ternary direction argument (which assertion 2 of
	// TestDashboardJS_CronHistoryRedesign_InlineExpand pins separately).
	if !strings.Contains(cronJS, "navigateExpandedRun(") {
		t.Error("cron_view.js: ↑↓ handler 必须在此调 navigateExpandedRun（与它读的 cronExpandedRunId 同一 <script>）")
	}
	if strings.Contains(dashCode, "navigateExpandedRun(") {
		t.Error("dashboard.js: navigateExpandedRun 调用点必须迁出到 cron_view.js — " +
			"留在此处即跨脚本裸引用 cronExpandedRunId（B1 回归）")
	}
	if !strings.Contains(cronJS, "document.addEventListener('keydown'") {
		t.Error("cron_view.js: 必须 document.addEventListener('keydown', …) 注册 ↑↓ 全局快捷键")
	}
}

// TestDashboardHTML_LoadsCronViewAfterDashboard pins the HTML load contract
// that the JS-only tests above can't see: dashboard.html must actually ship a
// <script> tag for cron_view.js, loaded AFTER dashboard.js. This closes the
// original crash vector directly — the `cronExpandedRunId is not defined`
// report came from a session where cron_view.js never executed. The JS
// decoupling makes dashboard.js survive that, but the cron UI still needs
// cron_view.js present and correctly ordered to function at all. If a future
// edit drops the tag or reorders it before dashboard.js (cron_view.js calls
// dashboard.js globals at load time), this fails instead of shipping a broken
// dashboard.
func TestDashboardHTML_LoadsCronViewAfterDashboard(t *testing.T) {
	t.Parallel()

	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	const dashTag = `<script defer src="/static/dashboard.js">`
	const cronTag = `<script defer src="/static/cron_view.js">`

	dashIdx := strings.Index(html, dashTag)
	cronIdx := strings.Index(html, cronTag)
	if dashIdx < 0 {
		t.Fatalf("dashboard.html: 缺少 %s", dashTag)
	}
	if cronIdx < 0 {
		t.Fatalf("dashboard.html: 缺少 %s — cron_view.js 必须随 dashboard 一起加载，"+
			"否则 cron UI 整个不工作（原始崩溃向量）", cronTag)
	}
	if cronIdx < dashIdx {
		t.Error("dashboard.html: cron_view.js 必须在 dashboard.js 之后加载 — " +
			"cron_view.js 在 load 期调用 dashboard.js 的全局函数（lsGet/wsm/esc…），" +
			"提前加载会反向 ReferenceError")
	}
}
