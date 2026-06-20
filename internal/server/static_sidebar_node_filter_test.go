package server

import (
	"regexp"
	"strings"
	"testing"
)

// TestDashboardJS_RenderSidebar_NoOrphanActiveFilter is the regression guard for
// the "left sidebar won't render" crash introduced by #2180.
//
// Root cause: #2180 ("移除侧边栏节点选择器") removed the per-node sidebar filter.
// It deleted the `const activeFilter = isMultiNode() && selectedNode` declaration
// (and switched `allItems` to the unfiltered list), but left ONE reference to the
// now-undefined symbol behind in the favorite-projects loop:
//
//	if (activeFilter && pNode !== selectedNode) return;
//
// At runtime renderSidebar() threw `ReferenceError: activeFilter is not defined`
// the moment it reached projectsData.forEach, aborting the whole render. The
// session cards never replaced the 3 skeleton placeholders — the sidebar "would
// not refresh" even though /api/sessions returned valid data.
//
// Invariant: `activeFilter` must not appear in renderSidebar as a bare reference
// without a matching declaration. The fix removed the orphan reference outright
// (the unfiltered list means every favorite header always renders), so the symbol
// should be gone entirely. Reading dashboard.js with comments stripped means the
// godoc-style prose above (which names the symbol to document the fix) cannot
// produce a false positive.
//
// This test fails on the pre-fix code (reference present, declaration absent) and
// passes on the fixed code (symbol absent). It mirrors TestDashboardJS_CronKeydownDecoupled
// — the same "deleted a symbol, left a bare reference" failure class.
func TestDashboardJS_RenderSidebar_NoOrphanActiveFilter(t *testing.T) {
	t.Parallel()

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	code := stripJSComments(string(data))

	refRe := regexp.MustCompile(`\bactiveFilter\b`)
	declRe := regexp.MustCompile(`\b(?:const|let|var)\s+activeFilter\b`)

	refs := refRe.FindAllStringIndex(code, -1)
	decls := declRe.FindAllStringIndex(code, -1)

	// Any reference at all REQUIRES a declaration. The #2180 regression had
	// 1 reference and 0 declarations — the exact shape this catches.
	if len(refs) > 0 && len(decls) == 0 {
		t.Errorf("dashboard.js 引用了 activeFilter（%d 处，代码非注释）但没有任何 "+
			"const/let/var 声明 — 这正是 #2180 的孤儿引用回归：renderSidebar 运行到 "+
			"projectsData.forEach 时抛 `ReferenceError: activeFilter is not defined`，"+
			"整个左侧会话列表渲染中断。删除残留引用，或重新声明该变量。", len(refs))
	}
}

// TestDashboardJS_RenderSidebar_FavoriteLoopUnfiltered pins the positive side of
// the #2180 fix: the sidebar now lists every connected node's sessions together
// (no per-node filter), so the favorite-projects loop must inject EVERY favorite's
// header regardless of node. The loop body must therefore NOT gate header creation
// on a selectedNode equality check — that gate (the old activeFilter branch) is
// exactly what referenced the deleted symbol and what would re-hide cross-node
// favorites if reintroduced.
//
// Guard scope is the renderSidebar function body (665 → next top-level function)
// so an unrelated selectedNode comparison elsewhere (WS message matching, active
// card tracking) does not trip this.
func TestDashboardJS_RenderSidebar_FavoriteLoopUnfiltered(t *testing.T) {
	t.Parallel()

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	full := string(data)

	const marker = "function renderSidebar(data) {"
	start := strings.Index(full, marker)
	if start < 0 {
		t.Fatalf("dashboard.js: 找不到 renderSidebar 定义 (%q) — 函数被重命名？测试需同步更新", marker)
	}
	// End at the next top-level function declaration (column-0 "function ").
	rest := full[start+len(marker):]
	end := len(rest)
	if idx := strings.Index(rest, "\nfunction "); idx >= 0 {
		end = idx
	}
	body := stripJSComments(rest[:end])

	// The favorite-projects loop must not re-introduce a per-node suppression
	// like `pNode !== selectedNode` (the old filter gate). Its presence means
	// someone restored the node filter — which both re-hides cross-node
	// favorites and (if paired with a deleted declaration) re-creates the
	// ReferenceError.
	if regexp.MustCompile(`pNode\s*!==\s*selectedNode`).MatchString(body) {
		t.Error("renderSidebar: favorite-projects 循环重新出现 `pNode !== selectedNode` 过滤门 — " +
			"#2180 已取消侧边栏按节点过滤（所有连接的会话同列），收藏 header 应无条件渲染。" +
			"该门正是 activeFilter 孤儿引用的来源。")
	}
}
