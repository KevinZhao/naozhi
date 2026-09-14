package cron

import (
	"strings"
	"testing"
	"time"
)

// TestAppend_OversizeRetry_DiskAndCacheAgree 防回归 #1079 / R250-GO-16：
// over-cap 截断重试路径下，磁盘上落的是 truncated CronRun，缓存行（List 的读源）
// 必须来自同一条记录。
//
// 这个文件原先还有一个 TestAppend_OversizeRetry_SourceAnchor，用四条正则钉住
// runstore.go 里 `summarySrc := run` / `summarySrc = &shrunk` /
// `cacheHeadPush(summarySrc.summary())` 这套写法。它自己的注释坦白了原因：
// CronRunSummary 当时不含任何可被截断的字段，所以「磁盘 vs 缓存不一致」在行为上
// 观测不到，只能钉实现。
//
// 现在不需要了。runstore.go 把 payload 和 summary 收进一个 runRecord，由
// shrinkAndMarshal 从同一个 *CronRun 一次产出，Append 只整体替换 rec；两个
// over-cap 分支（preflight 与 post-marshal）共用同一个 applyShrink。原来那个
// 「改了一个分支忘了改另一个」的失误没有地方可犯了；剩下唯一能写错的地方是
// shrinkAndMarshal 里配对的那一行，而正则锚点对它同样无能为力（#2547）。
//
// 下面这条测试钉住的是可观测的部分：磁盘记录确实被截断，且 Recent 看到的行与
// 磁盘上的那一条是同一条。
func TestAppend_OversizeRetry_DiskAndCacheAgree(t *testing.T) {
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

	// Recent 走缓存行；Get 读磁盘。两者必须指向同一条 run。
	got := s.Recent(jobID, 5)
	if len(got) != 1 {
		t.Fatalf("Recent len=%d want 1 (Append/retry should have landed)", len(got))
	}
	if got[0].RunID != run.RunID {
		t.Fatalf("cache row RunID=%q want %q", got[0].RunID, run.RunID)
	}

	disk, err := s.Get(jobID, run.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(disk.Result) >= len(originalResult) {
		t.Fatalf("disk Result length=%d not truncated; oversize-retry path broken", len(disk.Result))
	}
	// 缓存行必须等于磁盘那条记录的 summary()。实测过：把 summary 取源换成未截断
	// 的 run 时这里不会失败——CronRunSummary 的 11 个字段没有一个受截断影响，所以
	// #1079 那个 bug 今天在行为上确实观测不到（原 anchor 的注释也这么说）。价值在
	// 于自动布防：等 ResultPreview 之类字段加进 CronRunSummary，这一行立刻开始检查
	// 它，而正则锚点不会。
	if want := disk.summary(); got[0] != want {
		t.Errorf("cache row disagrees with the record on disk:\n cache = %+v\n disk  = %+v", got[0], want)
	}
}

// TestAppend_PreflightOverCap_TruncatesWithoutFirstMarshal covers the other
// over-cap entry point (#1111): when Result+Prompt+ErrorMsg alone overshoot the
// cap, Append skips the doomed first marshal and truncates directly. The
// observable consequence is the same — a truncated record on disk and a cache
// row that agrees with it — reached without the post-marshal gate firing.
func TestAppend_PreflightOverCap_TruncatesWithoutFirstMarshal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.maxRunBytes = 2048

	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	// Well past maxRunBytes-fixedFieldsHeadroom, so preflightOverCap is true.
	run.Result = strings.Repeat("Y", 8192)

	s.Append(run)

	got := s.Recent(jobID, 5)
	if len(got) != 1 {
		t.Fatalf("Recent len=%d want 1; the preflight path must still land a record", len(got))
	}
	disk, err := s.Get(jobID, run.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(disk.Result) >= len(run.Result) {
		t.Fatalf("disk Result length=%d not truncated on the preflight path", len(disk.Result))
	}
	if disk.ResultBytes != len(disk.Result) {
		t.Errorf("ResultBytes=%d but stored Result is %d bytes (#2016)", disk.ResultBytes, len(disk.Result))
	}
	if want := disk.summary(); got[0] != want {
		t.Errorf("cache row disagrees with the record on disk:\n cache = %+v\n disk  = %+v", got[0], want)
	}
}
