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
	// cron extraction (PR-1): cron render helpers moved to cron_view.js, but the
	// global Esc handler (assertion 5) stayed in dashboard.js. Assert on the union.
	dashData, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	cronData, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(dashData) + "\n" + string(cronData)

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
	rowBody, ok := sliceFunctionBody(js, rowIdx)
	if !ok {
		t.Fatal("dashboard.js: cronTimelineRowHtml 函数体边界 `}\\n` 未找到 — 解析失败,断言无意义")
	}
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

	// 5. ESC handler 优先 collapse 再关 drawer。Global Esc handler 留在
	// dashboard.js（已包含在上面的 union 里）。
	escIdx := strings.Index(js, "if (e.key !== 'Escape') return;")
	if escIdx < 0 {
		t.Fatal("dashboard.js: Global Esc handler 块未找到")
	}
	// Esc handler 不是函数定义,而是 function(e){...} 体内的第一行 anchor。
	// 改用显式右花括号收敛符 `\n});\n`(addEventListener 注册的 IIFE 收尾形态)。
	escEndRel := strings.Index(js[escIdx:], "\n});\n")
	if escEndRel < 0 {
		t.Fatal("dashboard.js: Esc handler 闭合 `\\n});\\n` 未找到 — 解析失败,断言无意义")
	}
	escBody := js[escIdx : escIdx+escEndRel]
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
	openBody, ok := sliceFunctionBody(js, openIdx)
	if !ok {
		t.Fatal("dashboard.js: openCronDetail 函数体边界 `}\\n` 未找到 — 解析失败,断言无意义")
	}
	if !strings.Contains(openBody, "cronExpandedRunId") {
		t.Error("openCronDetail: 切 cron 时必须清 cronExpandedRunId（避免上下文串台）")
	}

	// 9. closeCronDetail 时清 cronExpandedRunId。
	closeIdx := strings.Index(js, "function closeCronDetail()")
	if closeIdx < 0 {
		t.Fatal("dashboard.js: closeCronDetail 不存在")
	}
	closeBody, ok := sliceFunctionBody(js, closeIdx)
	if !ok {
		t.Fatal("dashboard.js: closeCronDetail 函数体边界 `}\\n` 未找到 — 解析失败,断言无意义")
	}
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

// TestSliceFunctionBody_BoundaryCases pins the sliceFunctionBody helper
// itself (R244-CR-P3-1 / #1062) so a regression in the parser can't
// silently false-pass the redesign-pin tests above. Covers:
//
//  1. balanced top-level body returns content up to and including `}\n`;
//  2. nested `{`/`}` blocks are tracked correctly (depth must hit 0);
//  3. unterminated body returns ok=false (caller should t.Fatal);
//  4. closing brace at EOF (no trailing newline) is accepted.
func TestSliceFunctionBody_BoundaryCases(t *testing.T) {
	cases := []struct {
		name   string
		js     string
		idx    int
		want   string
		wantOK bool
	}{
		{
			name:   "simple-balanced",
			js:     "function f() {\n  return 1;\n}\nrest",
			idx:    0,
			want:   "function f() {\n  return 1;\n}\n",
			wantOK: true,
		},
		{
			name:   "nested-braces",
			js:     "fn() {\n  if (x) { y(); }\n  return { a: 1 };\n}\ntail",
			idx:    0,
			want:   "fn() {\n  if (x) { y(); }\n  return { a: 1 };\n}\n",
			wantOK: true,
		},
		{
			name:   "unterminated",
			js:     "function f() {\n  no close",
			idx:    0,
			want:   "",
			wantOK: false,
		},
		{
			name:   "close-at-eof",
			js:     "fn() {\n}",
			idx:    0,
			want:   "fn() {\n}",
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sliceFunctionBody(tc.js, tc.idx)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// sliceFunctionBody slices js[idx:] up to the first `}\n` (function-end
// boundary) that is preceded by a balanced run of inner `{`/`}`. R244-CR-P3-1
// (#1062): replaces magic 4000/2500-byte windows that silently false-passed
// when a function grew larger than the window. Returns ok=false if no closing
// boundary is found, so the caller can t.Fatal instead of asserting against a
// truncated body.
//
// 实现策略: 从 idx 开始线性扫描,跟踪 `{` `}` 计数。第一次出现 `{` 计数从 1
// 起跳;之后每遇 `{` +1,每遇 `}` -1;当计数归零且其后是 `\n` 即为函数结束。
// JS 内字符串/正则/注释里的花括号会被误计入,但 dashboard.js 形态稳定且本检测
// 用于"窗口够不够大",误差只会让 ok=false 触发显式 fatal,不会静默漏断言。
func sliceFunctionBody(js string, idx int) (string, bool) {
	depth := 0
	started := false
	for i := idx; i < len(js); i++ {
		c := js[i]
		switch c {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				if i+1 < len(js) && js[i+1] == '\n' {
					return js[idx : i+2], true
				}
				// allow EOF immediately after closing brace
				if i+1 == len(js) {
					return js[idx : i+1], true
				}
			}
		}
	}
	return "", false
}
