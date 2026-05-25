package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronHistoryRedesign_InlineExpand pins the §16 invariants
// (inline-expand 回归 2026-05-25)，取代 v3 的 sheet 浮层契约：
//
//  1. cronExpandedRunId 模块状态存在（同时只展开一行）
//  2. cronTimelineExpand / cronTimelineCollapse / navigateExpandedRun 均存在
//  3. cronTimelineSelectRun 是行 onclick 入口；二次点击触发 collapse
//  4. cronTimelineRowHtml 选中行下方就地 emit `<div class="ctr-detail">`
//  5. ESC handler 优先关行内展开（cronTimelineCollapse），再关 drawer
//  6. ↑↓ 全局快捷键调 navigateExpandedRun
//  7. 全部 sheet 符号已删除（无残余依赖）
//  8. openCronDetail 切 cron 时清 cronExpandedRunId（上下文隔离）
//  9. closeCronDetail 时清 cronExpandedRunId（drawer 关 → 行内展开也无意义）
//
// 这些不变量保证：cron 任务结果显示在记录中行内展开，而不是右侧 popup。
func TestDashboardJS_CronHistoryRedesign_InlineExpand(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. Module-scoped expanded state.
	if !strings.Contains(js, "const cronExpandedRunId = {") {
		t.Error("dashboard.js: cronExpandedRunId 模块状态缺失 — inline-expand 必须有单一 state 源")
	}

	// 2. Lifecycle functions.
	for _, fn := range []string{
		"function cronTimelineExpand(jobId, runId)",
		"function cronTimelineCollapse()",
		"function navigateExpandedRun(direction)",
	} {
		if !strings.Contains(js, fn) {
			t.Errorf("dashboard.js: 缺少 %s — inline-expand 生命周期 API 必备", fn)
		}
	}

	// 3. cronTimelineSelectRun 是行入口，二次点击 collapse。
	if !strings.Contains(js, "function cronTimelineSelectRun(jobId, runId)") {
		t.Error("dashboard.js: 缺少 cronTimelineSelectRun(jobId, runId)")
	}
	rowIdx := strings.Index(js, "function cronTimelineRowHtml(jobId, r, st)")
	if rowIdx < 0 {
		t.Fatal("dashboard.js: cronTimelineRowHtml 不存在")
	}
	rowEnd := rowIdx + 4500
	if rowEnd > len(js) {
		rowEnd = len(js)
	}
	rowBody := js[rowIdx:rowEnd]
	if !strings.Contains(rowBody, "cronTimelineSelectRun(") {
		t.Error("cronTimelineRowHtml: 行 onclick 必须调 cronTimelineSelectRun")
	}

	// 4. Inline expand 标志：选中行 emit ctr-detail 容器（v2 inline 形态回归）。
	if !strings.Contains(rowBody, "ctr-detail") {
		t.Error("cronTimelineRowHtml: 必须 emit `ctr-detail` 容器（行内展开详情块的宿主）")
	}
	if !strings.Contains(rowBody, "isExpanded") {
		t.Error("cronTimelineRowHtml: 必须有 isExpanded 分支控制展开渲染")
	}
	if !strings.Contains(rowBody, "cronExpandedRunId") {
		t.Error("cronTimelineRowHtml: 选中态必须 gate 在 cronExpandedRunId 上")
	}
	if !strings.Contains(rowBody, "cronTimelineDetailHtml(") {
		t.Error("cronTimelineRowHtml: 展开行内必须复用 cronTimelineDetailHtml 渲染详情")
	}

	// 5. ESC handler 优先 collapse 再关 drawer。
	escIdx := strings.Index(js, "if (e.key !== 'Escape') return;")
	if escIdx < 0 {
		t.Fatal("dashboard.js: Global Esc handler 块未找到")
	}
	escEnd := escIdx + 2500
	if escEnd > len(js) {
		escEnd = len(js)
	}
	escBody := js[escIdx:escEnd]
	collapseIdx := strings.Index(escBody, "cronTimelineCollapse()")
	drawerCloseIdx := strings.Index(escBody, "closeCronDetail()")
	if collapseIdx < 0 {
		t.Error("Global Esc handler: 缺少 cronTimelineCollapse() 分支 — 行内展开必须可 ESC 关闭")
	}
	if drawerCloseIdx < 0 {
		t.Error("Global Esc handler: 缺少 closeCronDetail() 分支（v2 已有，不应被本次改动移除）")
	}
	if collapseIdx > 0 && drawerCloseIdx > 0 && collapseIdx > drawerCloseIdx {
		t.Error("Global Esc handler: cronTimelineCollapse 必须在 closeCronDetail 之前（行展开是更靠前的状态）")
	}

	// 6. ↑↓ 全局键盘 handler。
	if !strings.Contains(js, "navigateExpandedRun(e.key === 'ArrowUp' ? 'prev' : 'next')") {
		t.Error("dashboard.js: 缺少 ↑↓ 键盘绑定到 navigateExpandedRun")
	}

	// 7. v3 sheet 符号已删除：禁止再出现 cronRunSheetState / openRunDetailSheet /
	// closeRunDetailSheet / navigateRunSheet / renderRunDetailSheet 任意符号。
	for _, banned := range []string{
		"cronRunSheetState",
		"openRunDetailSheet",
		"closeRunDetailSheet",
		"function navigateRunSheet",
		"function renderRunDetailSheet",
		"syncSheetGeometry",
	} {
		if strings.Contains(js, banned) {
			t.Errorf("dashboard.js: sheet 浮层残余符号 %q 必须已被 inline-expand 替代", banned)
		}
	}

	// 8. openCronDetail 切 cron 时清 cronExpandedRunId。
	openIdx := strings.Index(js, "function openCronDetail(jobId, originRow)")
	if openIdx < 0 {
		t.Fatal("dashboard.js: openCronDetail 不存在")
	}
	openEnd := openIdx + 2500
	if openEnd > len(js) {
		openEnd = len(js)
	}
	openBody := js[openIdx:openEnd]
	if !strings.Contains(openBody, "cronExpandedRunId") {
		t.Error("openCronDetail: 切 cron 时必须清 cronExpandedRunId（避免上下文串台）")
	}

	// 9. closeCronDetail 时清 cronExpandedRunId。
	closeIdx := strings.Index(js, "function closeCronDetail()")
	if closeIdx < 0 {
		t.Fatal("dashboard.js: closeCronDetail 不存在")
	}
	closeEnd := closeIdx + 2500
	if closeEnd > len(js) {
		closeEnd = len(js)
	}
	closeBody := js[closeIdx:closeEnd]
	if !strings.Contains(closeBody, "cronExpandedRunId") {
		t.Error("closeCronDetail: drawer 关闭时必须清 cronExpandedRunId（drawer 是宿主）")
	}
}

// TestDashboardHTML_CronHistoryRedesign_InlineExpandMarkup pins inline-expand
// CSS + 确认 sheet DOM 已被移除：
//
//  1. body 末尾不再有 #cron-run-sheet 容器
//  2. .ctr-detail 容器样式存在，且对长 result 有 max-height 保护
//  3. 移动端 .ctr-detail 收紧 max-height 到 50vh
func TestDashboardHTML_CronHistoryRedesign_InlineExpandMarkup(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// 1. Sheet 容器已删除。
	for _, banned := range []string{
		`id="cron-run-sheet"`,
		`id="cron-run-sheet-backdrop"`,
		`id="crs-prev"`,
		`id="crs-next"`,
		`id="crs-close"`,
		`id="crs-body"`,
		`.cron-run-sheet{`,
		`.cron-run-sheet.is-open`,
	} {
		if strings.Contains(html, banned) {
			t.Errorf("dashboard.html: sheet 残余 %q 必须已被 inline-expand 替代", banned)
		}
	}

	// 2. inline-expand 容器样式必备。
	for _, marker := range []string{
		".ctr-detail{",
		"max-height:60vh",
		"overflow-y:auto",
		".ctr-detail .ctr-final",
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("dashboard.html: inline-expand 样式缺少 %q", marker)
		}
	}

	// 3. 移动端 max-height 收紧。
	if !strings.Contains(html, "max-height:50vh") {
		t.Error("dashboard.html: 移动端 .ctr-detail 应收紧到 max-height:50vh")
	}
}
