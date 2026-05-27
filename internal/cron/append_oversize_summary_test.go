package cron

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestAppend_OversizeRetry_DiskAndCacheConsistent 防回归 #1079 / R250-GO-16:
// over-cap 截断重试路径下，磁盘上落的是 truncated CronRun。修复前
// cacheHeadPush(run.summary()) 用的是原 *run，summary() 仅含 RunID /
// State 等不可变字段所以当前未触发用户可见 bug，但维护性上"磁盘 vs
// 缓存读源不一致"是一颗未来雷：CronRunSummary 一旦加入 ResultPreview /
// ErrorMsgPreview 之类字段，旧代码会立即让 List 看到未截断字符串而磁
// 盘只有截断版。本测试钉住磁盘记录确实被截断 + 修复后代码读 summarySrc
// （而非 run）这两个不变量。
func TestAppend_OversizeRetry_DiskAndCacheConsistent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	// 把 cap 卡到刚好让原 4 KiB Result 触发 retry，但 truncated record
	// （≤ 256 runes per field + 元数据）能落盘。
	s.maxRunBytes = 2048

	jobID := mustGenerateID()
	originalResult := strings.Repeat("X", 4096)
	run := makeRun(jobID, time.Now())
	run.Result = originalResult

	s.Append(run)

	// 磁盘记录必然被截断。Append 自身是 over-cap 才走 retry，
	// 这里同时验证 retry 落盘成功（List 能看到 1 条）。
	got := s.Recent(jobID, 5)
	if len(got) != 1 {
		t.Fatalf("Recent len=%d want 1 (Append/retry should have landed)", len(got))
	}

	disk, err := s.Get(jobID, run.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(disk.Result) >= len(originalResult) {
		t.Fatalf("disk Result length=%d not truncated; oversize-retry path broken", len(disk.Result))
	}

	// 原 *run 的 Result 没动（caller 持有，可能复用），但内部缓存路径
	// 必须用 shrunk 的 summary —— 当前 CronRunSummary 不含可比字段，
	// 所以这里加一道源码 anchor，钉住 cacheHeadPush 的参数源是
	// summarySrc 而不是 run。一旦未来有人把它改回 run.summary()，
	// 这条测试立即报错。
}

// TestAppend_OversizeRetry_SourceAnchor 钉住 #1079 修复方式：
// runstore.go 中 over-cap retry 路径必须把 cacheHeadPush 的 summary 源
// 重绑到 shrunk 副本，而不是仍读原 *run。
func TestAppend_OversizeRetry_SourceAnchor(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("runstore.go")
	if err != nil {
		t.Fatalf("read runstore.go: %v", err)
	}
	body := string(src)

	// summarySrc 变量必须存在，并且 cacheHeadPush 必须用它而不是 run.summary()
	if !regexp.MustCompile(`summarySrc\s*:=\s*run`).MatchString(body) {
		t.Error("runstore.go 缺少 summarySrc := run 默认绑定 (#1079)")
	}
	if !regexp.MustCompile(`summarySrc\s*=\s*&shrunk`).MatchString(body) {
		t.Error("runstore.go 缺少 summarySrc = &shrunk 重绑 (#1079 oversize retry path)")
	}
	if regexp.MustCompile(`cacheHeadPush\([^)]*run\.summary\(\)`).MatchString(body) {
		t.Error("runstore.go cacheHeadPush 仍在用 run.summary()；必须用 summarySrc.summary() (#1079 retry path divergence)")
	}
	if !regexp.MustCompile(`cacheHeadPush\([^)]*summarySrc\.summary\(\)`).MatchString(body) {
		t.Error("runstore.go cacheHeadPush 没用 summarySrc.summary() (#1079)")
	}
}
