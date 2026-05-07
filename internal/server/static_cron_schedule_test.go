package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronEditScheduleSafety 守住"编辑老任务不改频率即保存
// 绝不改 schedule"这条契约。相关 bug 见 Round ? review Issue #1 (cron-v2
// refactor)。
//
// 基本不变式（通过字符串锚点断言）：
//  1. freqUpdate 内部必须检查 _cronScheduleTouched 再写 _cronSchedule
//  2. editCronJob 打开 modal 时显式把 _cronScheduleTouched 置为 false 并
//     seed _cronSchedule 为 job.schedule 原值（不经 picker round-trip）
//  3. createNewCronJob 打开 modal 时把 _cronScheduleTouched 置为 true
//     （打开即保存也能提交合法默认 schedule）
//  4. freqSelectMode / time 输入 / weekly-dow / monthly-day 的
//     onchange/oninput 都要调 freqMarkTouched
//  5. editCronJob 打开后不调用 freqUpdate()（避免覆盖 seed）
func TestDashboardJS_CronEditScheduleSafety(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) freqUpdate gating
	if !strings.Contains(js, "if (!overlay._cronScheduleTouched) return;") {
		t.Error("freqUpdate must early-return when !_cronScheduleTouched — " +
			"otherwise editing a legacy job and saving without changing the " +
			"frequency rewrites schedule to the picker default (silent data loss)")
	}

	// 2) editCronJob seed contract
	if !strings.Contains(js, "overlay._cronScheduleTouched = false;") {
		t.Error("editCronJob must seed _cronScheduleTouched=false to protect " +
			"the job's original schedule until user interacts with freq controls")
	}

	// 3) createNewCronJob default schedule activation
	if !strings.Contains(js, "overlay._cronScheduleTouched = true;") {
		t.Error("createNewCronJob must set _cronScheduleTouched=true so the " +
			"default Daily 09:00 picker value is committed to _cronSchedule " +
			"before the user clicks save (otherwise open-then-save submits empty)")
	}

	// 4) Freq controls must mark touched on change
	//    time input (both onchange and oninput fire in browsers — same handler chain)
	if !strings.Contains(js, `onchange="freqMarkTouched();freqUpdate()" oninput="freqMarkTouched();freqUpdate()"`) {
		t.Error("time input must call freqMarkTouched before freqUpdate in both onchange and oninput")
	}
	//    weekly / monthly selects
	if !strings.Contains(js, `id="freq-weekly-dow" onchange="freqMarkTouched();freqUpdate()"`) {
		t.Error("weekly-dow select must call freqMarkTouched before freqUpdate")
	}
	if !strings.Contains(js, `id="freq-monthly-day" onchange="freqMarkTouched();freqUpdate()"`) {
		t.Error("monthly-day select must call freqMarkTouched before freqUpdate")
	}
	//    freqSelectMode must call freqMarkTouched
	if !strings.Contains(js, "freqMarkTouched();\n  freqUpdate();") {
		// 允许 formatter 在两句之间插别的，只要顺序正确且同时存在
		mIdx := strings.Index(js, "function freqSelectMode(")
		if mIdx < 0 {
			t.Fatal("freqSelectMode function missing")
		}
		end := mIdx + 1000
		if end > len(js) {
			end = len(js)
		}
		body := js[mIdx:end]
		mtIdx := strings.Index(body, "freqMarkTouched()")
		fuIdx := strings.Index(body, "freqUpdate()")
		if mtIdx < 0 || fuIdx < 0 || mtIdx > fuIdx {
			t.Error("freqSelectMode must call freqMarkTouched() before freqUpdate()")
		}
	}

	// 5) editCronJob MUST NOT call freqUpdate() as part of the modal-open
	//    sequence. We check by window: between "function editCronJob(" and
	//    the next top-level function ("async function doEditCronJob"), the
	//    substring "freqUpdate()" must be absent.
	editStart := strings.Index(js, "function editCronJob(")
	editEnd := strings.Index(js, "async function doEditCronJob(")
	if editStart < 0 || editEnd < 0 || editEnd <= editStart {
		t.Fatal("could not locate editCronJob function bounds")
	}
	editBody := js[editStart:editEnd]
	// 过滤掉注释行再检查——注释里解释"为什么不调 freqUpdate()"是期望的，
	// 真实调用必须在非注释的语句里出现。
	var codeLines []string
	for _, line := range strings.Split(editBody, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		codeLines = append(codeLines, line)
	}
	codeOnly := strings.Join(codeLines, "\n")
	if strings.Contains(codeOnly, "freqUpdate()") {
		t.Error("editCronJob must NOT call freqUpdate() at modal-open time — " +
			"that would immediately overwrite the seed _cronSchedule with the " +
			"picker's default Daily 09:00, undoing the touched-gate protection")
	}
}

// TestDashboardJS_CronHumanizeFallbacks 守住 humanizeCron 对几类 v2 picker
// 不 round-trip 的 legacy shape 必须给出可读中文标签（而非原 cron
// 表达式），用于卡片列表和 legacy hint 的视觉展示。
func TestDashboardJS_CronHumanizeFallbacks(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		// @every N[mh] → 每 N 分钟 / 每 N 小时
		"function humanizeCronLegacyEvery(",
		`return unit === 'h' ? ('每 ' + n + ' 小时') : ('每 ' + n + ' 分钟')`,
		// multi-dow weekly → 周一、周三、周五 HH:MM
		"function humanizeCronMultiDow(",
		// legacy hint in edit modal must call humanizeCron so users see what
		// their current non-round-trippable schedule actually means
		"const human = humanizeCron(initialRawExpr)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("humanize fallback missing: %s", want)
		}
	}
}

// TestDashboardJS_CronParseRoundTripGuards 守住 parseCronToFreq 对会引起
// 视觉误导的 shape 返回 null，走 legacy hint 路径（见 Issue #2）。
//
//   - 多选 weekly (dows.length > 1) → null（v2 picker 单选会丢 day 信息）
//   - 周末 "0,6" → null（v2 picker 无 Weekend mode）
func TestDashboardJS_CronParseRoundTripGuards(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// parseCronToFreq 的 weekly 分支必须限制 dows.length === 1
	if !strings.Contains(js, "if (days && days.length === 1) return { mode: 'weekly'") {
		t.Error("parseCronToFreq must return weekly only for single-day selections — " +
			"multi-day shapes (0 9 * * 1,3,5) shown in Weekly(星期一) single-select UI " +
			"would visually lie about the actual schedule")
	}
	// Weekend shortcut 也要返回 null
	if !strings.Contains(js, `dow === '0,6' || dow === '6,0'`) {
		t.Error("parseCronToFreq must reject Weekend shortcut '0,6' to force legacy-hint path")
	}
}
