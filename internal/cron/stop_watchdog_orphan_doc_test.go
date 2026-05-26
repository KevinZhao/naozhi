package cron

import (
	"os"
	"regexp"
	"testing"
)

// TestStopGodoc_EnumeratesWatchdogOrphan 锚定 R250-GO-9 / #1072:
// Stop() 的 godoc 必须显式枚举 runDeadlineWatchdog 这个 third
// intentional-orphan goroutine 站点。修复前 godoc 只列了 triggerWG
// 与 gcWG 两处 leak，操作员读 "deadline fired but interrupt did not
// land" 这种 post-Stop 日志时会漏掉 watchdog goroutine 的存在。
//
// 这条测试钉源码注释而不依赖时序 fake，因为 R250-GO-9 修复方式 (a)
// 就是文档枚举（option (b) 把 watchdog 纳入 triggerWG 是更大的
// 重构，issue 自身允许选 (a)）。任何把 watchdog 注释删回去的回归
// 都让本测试立即报错。
func TestStopGodoc_EnumeratesWatchdogOrphan(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatalf("read scheduler.go: %v", err)
	}
	body := string(src)

	// Stop 的 godoc 段必须出现 runDeadlineWatchdog 字眼，且要在 Stop
	// 函数定义之前——godoc 块只能挂在函数声明上方。
	idxStop := regexp.MustCompile(`func\s+\(\s*s\s+\*Scheduler\s*\)\s+Stop\s*\(`).FindStringIndex(body)
	if idxStop == nil {
		t.Fatalf("scheduler.go 未找到 Stop 方法定义")
	}
	preamble := body[:idxStop[0]]

	if !regexp.MustCompile(`runDeadlineWatchdog`).MatchString(preamble) {
		t.Error("scheduler.go Stop godoc 未枚举 runDeadlineWatchdog 这个 third orphan 站点 (#1072 文档回归)")
	}
	// 必须明确说"intentional-orphan"或类似定性，避免日后被人误删。
	if !regexp.MustCompile(`(?i)intentional-orphan`).MatchString(preamble) {
		t.Error("scheduler.go Stop godoc 缺少 intentional-orphan 定性，#1072 文档锚定不再有效")
	}
}
