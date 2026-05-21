package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronHistoryRedesign_RunSheet pins the PR-1 invariants from
// docs/rfc/cron-history-redesign.md §6 — Run-Detail Sheet 共组件:
//
//  1. cronRunSheetState 模块状态存在
//  2. openRunDetailSheet / closeRunDetailSheet / navigateRunSheet / renderRunDetailSheet 均存在
//  3. cronTimelineSelectRun 替代 cronTimelineToggleRow（旧 API 仅作 shim 转发）
//  4. cronTimelineRowHtml 不再 emit `<div class="ctr-detail">` 内联展开块
//  5. ESC handler 优先关 sheet 再关 drawer
//  6. ↑↓ 全局快捷键调 navigateRunSheet
//  7. cronTimelineFetchDetail 在 sheet 命中同一 run 时刷新 sheet
//  8. openCronDetail 切 cron 时关 sheet（清上下文，避免内容串台）
//  9. closeCronDetail 时连带关 sheet
//
// 这些不变量保证 PR-1 的核心 UX：timeline 行不再 inline 展开 → 点击进 sheet →
// ↑↓/Esc/同行二次点击切换或关闭。一旦有人无意中破坏其中任意一条，sheet 行为
// 就会回退到 v2 的 inline-expand 模式，掩盖 review 出来的核心问题。
func TestDashboardJS_CronHistoryRedesign_RunSheet(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. Module-scoped sheet state.
	if !strings.Contains(js, "const cronRunSheetState = {") {
		t.Error("dashboard.js: cronRunSheetState 模块状态缺失 — sheet 必须有单一 state 源")
	}

	// 2. Lifecycle functions.
	for _, fn := range []string{
		"function openRunDetailSheet(jobId, runId)",
		"function closeRunDetailSheet()",
		"function navigateRunSheet(direction)",
		"function renderRunDetailSheet()",
	} {
		if !strings.Contains(js, fn) {
			t.Errorf("dashboard.js: 缺少 %s — sheet 生命周期 API 必备", fn)
		}
	}

	// 3. cronTimelineSelectRun 是新主入口；旧 cronTimelineToggleRow 仅作 shim。
	if !strings.Contains(js, "function cronTimelineSelectRun(jobId, runId)") {
		t.Error("dashboard.js: 缺少 cronTimelineSelectRun(jobId, runId) — 替代 v2 inline-expand 入口")
	}
	// 行 onclick 必须打到 cronTimelineSelectRun（不是旧 Toggle）
	rowIdx := strings.Index(js, "function cronTimelineRowHtml(jobId, r, st)")
	if rowIdx < 0 {
		t.Fatal("dashboard.js: cronTimelineRowHtml 不存在")
	}
	rowEnd := rowIdx + 4000
	if rowEnd > len(js) {
		rowEnd = len(js)
	}
	rowBody := js[rowIdx:rowEnd]
	if !strings.Contains(rowBody, "cronTimelineSelectRun(") {
		t.Error("cronTimelineRowHtml: 行 onclick 必须调 cronTimelineSelectRun（PR-1 §6）")
	}

	// 4. inline expand 必须移除 — 行 markup 不再 emit ctr-detail 容器。
	if strings.Contains(rowBody, "ctr-detail") {
		t.Error("cronTimelineRowHtml: 不能再 emit `ctr-detail` 容器（v2 inline-expand 已废弃，详情进 sheet）")
	}
	if strings.Contains(rowBody, "isExpanded") {
		t.Error("cronTimelineRowHtml: 不能再有 isExpanded 分支（PR-1 行只有选中态，无展开态）")
	}
	// 选中态：is-selected class + cronRunSheetState gate
	if !strings.Contains(rowBody, "is-selected") {
		t.Error("cronTimelineRowHtml: 必须 emit `is-selected` 选中态 class（与 sheet 联动）")
	}
	if !strings.Contains(rowBody, "cronRunSheetState.runId") {
		t.Error("cronTimelineRowHtml: 选中态必须 gate 在 cronRunSheetState 上")
	}

	// 5. ESC handler 优先关 sheet 再关 drawer。
	// 通过更稳定的结构特征定位（"if (e.key !== 'Escape') return;"），
	// 避免依赖注释字符串 — 注释一改测试 fatal，对维护者不友好。
	escIdx := strings.Index(js, "if (e.key !== 'Escape') return;")
	if escIdx < 0 {
		t.Fatal("dashboard.js: Global Esc handler 块未找到（搜索 \"if (e.key !== 'Escape') return;\"）")
	}
	escEnd := escIdx + 2500
	if escEnd > len(js) {
		escEnd = len(js)
	}
	escBody := js[escIdx:escEnd]
	sheetCloseIdx := strings.Index(escBody, "closeRunDetailSheet()")
	drawerCloseIdx := strings.Index(escBody, "closeCronDetail()")
	if sheetCloseIdx < 0 {
		t.Error("Global Esc handler: 缺少 closeRunDetailSheet() 分支 — sheet 必须可 ESC 关闭")
	}
	if drawerCloseIdx < 0 {
		t.Error("Global Esc handler: 缺少 closeCronDetail() 分支（v2 已有，不应被本次 PR 移除）")
	}
	if sheetCloseIdx > 0 && drawerCloseIdx > 0 && sheetCloseIdx > drawerCloseIdx {
		t.Error("Global Esc handler: closeRunDetailSheet 必须在 closeCronDetail 之前（sheet 是更靠前的浮层）")
	}

	// 6. ↑↓ 全局键盘 handler。
	if !strings.Contains(js, "navigateRunSheet(e.key === 'ArrowUp' ? 'prev' : 'next')") {
		t.Error("dashboard.js: 缺少 ↑↓ 键盘绑定到 navigateRunSheet（PR-1 §6 Q3）")
	}

	// 7. cronTimelineFetchDetail 命中 sheet 同一 run 时必须刷新 sheet body。
	fetchIdx := strings.Index(js, "async function cronTimelineFetchDetail(jobId, runId)")
	if fetchIdx < 0 {
		t.Fatal("dashboard.js: cronTimelineFetchDetail 不存在")
	}
	fetchEnd := fetchIdx + 3500
	if fetchEnd > len(js) {
		fetchEnd = len(js)
	}
	fetchBody := js[fetchIdx:fetchEnd]
	if !strings.Contains(fetchBody, "cronRunSheetState.open") || !strings.Contains(fetchBody, "renderRunDetailSheet()") {
		t.Error("cronTimelineFetchDetail: detail 加载完成后必须刷新 sheet（如果 sheet 看的就是同一条 run）")
	}

	// 8. openCronDetail 切 cron 时关 sheet。
	openIdx := strings.Index(js, "function openCronDetail(jobId, originRow)")
	if openIdx < 0 {
		t.Fatal("dashboard.js: openCronDetail 不存在")
	}
	openEnd := openIdx + 2500
	if openEnd > len(js) {
		openEnd = len(js)
	}
	openBody := js[openIdx:openEnd]
	if !strings.Contains(openBody, "closeRunDetailSheet()") {
		t.Error("openCronDetail: 切 cron 时必须关 sheet（避免上下文串台）")
	}

	// 9. closeCronDetail 连带关 sheet。
	closeIdx := strings.Index(js, "function closeCronDetail()")
	if closeIdx < 0 {
		t.Fatal("dashboard.js: closeCronDetail 不存在")
	}
	closeEnd := closeIdx + 2500
	if closeEnd > len(js) {
		closeEnd = len(js)
	}
	closeBody := js[closeIdx:closeEnd]
	if !strings.Contains(closeBody, "closeRunDetailSheet()") {
		t.Error("closeCronDetail: drawer 关闭时必须连带关 sheet（drawer 是 sheet 的父级）")
	}
}

// TestDashboardHTML_CronHistoryRedesign_SheetMarkup pins the sheet DOM + CSS:
//
//  1. body 末尾有 #cron-run-sheet 容器（带 ARIA 属性）
//  2. 桌面 (≥769) sheet 用 position:absolute（Q1 决议 — 仅覆盖 detail-pane）
//  3. 移动 (≤768) sheet 用 bottom-sheet 布局（transform:translateY）
//  4. backdrop（移动遮罩）存在
//  5. 桌面：detail-pane 设 position:relative（让 absolute sheet 能锚定）
func TestDashboardHTML_CronHistoryRedesign_SheetMarkup(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// 1. Sheet 容器。
	if !strings.Contains(html, `id="cron-run-sheet"`) {
		t.Error("dashboard.html: 缺少 #cron-run-sheet 容器")
	}
	if !strings.Contains(html, `id="cron-run-sheet-backdrop"`) {
		t.Error("dashboard.html: 缺少 #cron-run-sheet-backdrop（移动端遮罩）")
	}
	for _, marker := range []string{
		`id="crs-title"`,
		`id="crs-meta"`,
		`id="crs-body"`,
		`id="crs-prev"`,
		`id="crs-next"`,
		`id="crs-close"`,
		`id="crs-copy"`,
		`role="dialog"`,
		`aria-modal="false"`, // 不是 modal — 用户可继续在 timeline 上 ↑↓ 切
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("dashboard.html: sheet 缺少 %s", marker)
		}
	}

	// 2. 桌面 sheet 用 fixed + JS 同步几何到 detail-pane 右半（Playwright 实测后调整）。
	// CSS 不再依赖 absolute 锚定（sheet DOM 在 body 中，无法锚到 detail-pane；
	// 强行 reparent 会被 renderCronDrawer 的 innerHTML 吞掉）。
	// 桌面 transition: translateX(100%) → 0；坐标由 syncSheetGeometry() 算。
	if !strings.Contains(html, ".cron-run-sheet{transform:translateX(100%)") {
		t.Error("dashboard.html: 桌面 .cron-run-sheet 必须以 translateX(100%) 起始隐藏态")
	}

	// 3. 移动：bottom sheet 布局。
	// 在 max-width:768 媒体查询里，sheet 必须用 translateY 上滑。
	if !strings.Contains(html, "translateY(100%)") {
		t.Error("dashboard.html: 移动端 sheet 必须用 translateY(100%) 隐藏态")
	}
	// 移动端 backdrop 在 768 媒体查询内
	if !strings.Contains(html, ".cron-run-sheet-backdrop") {
		t.Error("dashboard.html: 缺少 .cron-run-sheet-backdrop CSS")
	}

	// 4. is-open class 控制显隐
	if !strings.Contains(html, ".cron-run-sheet.is-open") {
		t.Error("dashboard.html: sheet 必须用 .is-open class 触发滑出动画")
	}
}
