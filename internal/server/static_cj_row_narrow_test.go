package server

import (
	"strings"
	"testing"
)

// TestDashboardHTML_CjRowNarrowCompression — 锁住窄屏 cj-row grid 修复，避免后续
// 重构把 narrow / single 模式下隐藏 cj-when + cj-stats 的规则丢掉。
//
// 背景：grid-template-columns 18px minmax(0,1fr) auto auto auto 在窄 list-pane
// (≤ 320px) 下三个 auto 列总宽 ~180px 把 1fr (cj-main) 挤到 ~0.4px，title
// 被压成竖排单字。Playwright 实测 1100×800 viewport (data-cron-layout="narrow")
// 复现确认。修法是在 narrow / single 媒体查询里隐藏 cj-when + cj-stats，
// 复用现有 mobile @640 的 3 列布局策略。
func TestDashboardHTML_CjRowNarrowCompression(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// narrow + single 必须 emit 3 列 grid（与 mobile 同款）
	if !strings.Contains(html, `.cron-detail-body[data-cron-layout="narrow"] .cj-row`) {
		t.Error("dashboard.html: 缺少 narrow data-cron-layout 下的 cj-row grid override — 窄屏会重新压扁第 2 列")
	}
	if !strings.Contains(html, `.cron-detail-body[data-cron-layout="single"] .cj-row`) {
		t.Error("dashboard.html: 缺少 single data-cron-layout 下的 cj-row grid override")
	}

	// 必须隐藏 cj-when 和 cj-stats（不然 1fr 仍被挤）
	for _, mode := range []string{"narrow", "single"} {
		whenSel := `.cron-detail-body[data-cron-layout="` + mode + `"] .cj-when`
		statsSel := `.cron-detail-body[data-cron-layout="` + mode + `"] .cj-stats`
		if !strings.Contains(html, whenSel) {
			t.Errorf("dashboard.html: %s 模式必须 hide cj-when 列（避免 1fr 列被挤压）", mode)
		}
		if !strings.Contains(html, statsSel) {
			t.Errorf("dashboard.html: %s 模式必须 hide cj-stats 列（避免 1fr 列被挤压）", mode)
		}
	}

	// 窄屏要 fallback 到 sub-row 内的 cj-when-inline 显示时间（不然时间信息丢失）
	if !strings.Contains(html, `.cron-detail-body[data-cron-layout="narrow"] .cj-sub .cj-when-inline`) {
		t.Error("dashboard.html: narrow 模式必须 reveal cj-when-inline 让时间信息保留")
	}
}
