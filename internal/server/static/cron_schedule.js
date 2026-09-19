// cron_schedule.js — schedule expressions and the frequency picker
// (#2715 D4 follow-up: cron_view.js four-region split, region 1).
//
// Owns everything that understands a cron expression: the humanize family
// (expression → 中文 label, with deliberate fallback-to-raw for shapes the
// v2 picker cannot round-trip), parse/build between expressions and picker
// descriptors, the picker's DOM + touched-state discipline
// (_cronScheduleTouched lives on the modal overlay, seeded by the edit
// flow), and the scheduler-timezone annotations every schedule surface
// shares. cron_view.js renders WITH these; nothing here reads or writes the
// job list, the drawer, or the timeline.

import {
  esc,
} from './nz_util.js';

// Scheduler timezone from GET /api/cron (timezone = IANA name, e.g.
// Asia/Shanghai; timezone_abbr = CST; timezone_label = "Asia/Shanghai
// (UTC+08:00)"). Cron expressions are evaluated in THIS zone, so when the
// browser sits in a different offset every schedule surface (card chip /
// drawer 什么时候 / editor picker) is annotated via cronTimezoneSuffix().
let cronTimezone = '';
let cronTimezoneAbbr = '';
let cronTimezoneLabel = '';

// cronTimezoneOffsetMinutes parses the UTC offset out of timezone_label
// ("... (UTC+08:00)"). Returns null when the label is absent / unparsable.
function cronTimezoneOffsetMinutes() {
  const m = /UTC([+-])(\d{2}):(\d{2})/.exec(cronTimezoneLabel || '');
  if (!m) return null;
  const sign = m[1] === '-' ? -1 : 1;
  return sign * (parseInt(m[2], 10) * 60 + parseInt(m[3], 10));
}

// cronTimezoneDiffers — true when the browser's current UTC offset differs
// from the scheduler's. Offset (not IANA name) is the comparison because two
// zones on the same offset render identical wall-clock times and need no
// annotation. Unknown server offset → false (never annotate on guesswork).
function cronTimezoneDiffers() {
  const server = cronTimezoneOffsetMinutes();
  if (server === null) return false;
  return server !== -new Date().getTimezoneOffset();
}

// cronTimezoneUTCTag — the bare "UTC+08:00" from timezone_label, '' if absent.
function cronTimezoneUTCTag() {
  const m = /UTC[+-]\d{2}:\d{2}/.exec(cronTimezoneLabel || '');
  return m ? m[0] : '';
}

// cronTimezoneHasName — false when the scheduler runs on time.Local
// (loc.String() === "Local") or the name is missing; "Local" is meaningless to
// a browser user so those cases fall back to the UTC±HH:MM offset.
function cronTimezoneHasName() {
  return !!cronTimezone && cronTimezone !== 'Local';
}

// cronTimezoneSuffix — " (CST)" / " (Asia/Shanghai)" / " (UTC+08:00)" appended
// to schedule text when the browser zone differs from the server zone; ''
// otherwise. Preference: abbr → IANA name → UTC offset (Local / empty abbr
// never leak as " (Local)" or " ()").
function cronTimezoneSuffix() {
  if (!cronTimezoneDiffers()) return '';
  const tag = cronTimezoneAbbr || (cronTimezoneHasName() ? cronTimezone : cronTimezoneUTCTag());
  return tag ? ' (' + tag + ')' : '';
}

// cronTimezoneNote — sentence-form variant for the editor freq-hint:
// "时间按服务器时区 Asia/Shanghai (UTC+08:00) 计算。" or '' when zones match.
// A "Local (UTC+08:00)" label collapses to "UTC+08:00".
function cronTimezoneNote() {
  if (!cronTimezoneDiffers()) return '';
  const label = cronTimezoneHasName() ? (cronTimezoneLabel || cronTimezone) : cronTimezoneUTCTag();
  return label ? '时间按服务器时区 ' + label + ' 计算，与浏览器本地时间不同。' : '';
}

// Pads an integer to two digits (e.g. 7 -> "07"). Used for HH/MM rendering.
function pad2(n) { return (n < 10 ? '0' : '') + n; }

// parseCronToFreq inspects a schedule expression and, when it matches one of
// our canonical frequency shapes, returns a descriptor the frequency picker
// can restore. Returning null means "we don't recognize this — fall back to
// the raw expression editor." This is intentionally narrow: we only recognize
// the exact shapes buildFreqSchedule emits, so round-tripping is lossless.
// parseCronToFreq identifies the descriptor that buildFreqSchedule would have
// produced this expression from, so edit-modal can restore the picker state.
// Return null means the expression can't round-trip — legacy jobs with
// interval/custom shapes now degrade to the default Daily picker on edit
// (acceptable: user re-picks once and the new shape is persisted).
function parseCronToFreq(expr) {
  if (!expr) return null;
  const s = expr.trim();
  // Hourly: "0 * * * *"
  if (s === '0 * * * *') return { mode: 'hourly' };
  const parts = s.split(/\s+/);
  if (parts.length !== 5) return null;
  const [mm, hh, dom, mon, dow] = parts;
  if (!/^\d+$/.test(mm) || !/^\d+$/.test(hh)) return null;
  const minute = parseInt(mm, 10);
  const hour = parseInt(hh, 10);
  if (minute > 59 || hour > 23) return null;
  if (mon !== '*') return null;
  const hhmm = pad2(hour) + ':' + pad2(minute);
  if (dom === '*' && dow === '*') return { mode: 'daily', time: hhmm };
  if (dow === '*' && /^\d+$/.test(dom)) {
    const d = parseInt(dom, 10);
    if (d >= 1 && d <= 31) return { mode: 'monthly', day: d, time: hhmm };
  }
  if (dom === '*' && dow !== '*') {
    // "1-5" → Weekdays shortcut
    if (dow === '1-5') return { mode: 'weekdays', time: hhmm };
    // Weekend shortcut "0,6" 或反写 "6,0"
    if (dow === '0,6' || dow === '6,0' || dow === '6,7' || dow === '7,6') {
      // 周末没有 v2 picker 模式——返回 null 让上层走 legacy hint，保留
      // 原 schedule 不乱改；humanizeCron 会把它识别为 "周末 HH:MM"。
      return null;
    }
    const days = parseDowField(dow);
    // v2 picker 的 Weekly 是单选。多选 (dows.length>1 且非 weekdays/
    // weekend shortcut) 无法 round-trip —— 返回 null 触发 legacy hint
    // 路径，保留原 schedule，防止"Weekly 星期一"的视觉误导把用户在周
    // 一三五跑的任务静默改成只在周一跑。
    if (days && days.length === 1) return { mode: 'weekly', dows: days, time: hhmm };
  }
  return null;
}

// parseDowField parses robfig/cron DOW: "1-5", "1,3,5", "0". Sunday is 0
// (robfig convention). 7 is normalized to 0 defensively; returns null on any
// malformed input so the caller falls back to raw-expression editing.
function parseDowField(field) {
  const result = new Set();
  for (const part of field.split(',')) {
    if (/^\d+$/.test(part)) {
      let n = parseInt(part, 10);
      if (n === 7) n = 0;
      if (n < 0 || n > 6) return null;
      result.add(n);
      continue;
    }
    const m = part.match(/^(\d+)-(\d+)$/);
    if (!m) return null;
    let lo = parseInt(m[1], 10), hi = parseInt(m[2], 10);
    // R20260610-CR-001: only normalize 7→0 when both ends are 7 (7-7 → Sunday).
    // Validating after one-sided normalization silently turned the illegal
    // range 7-5 into 0-5; single-7 ranges (0-7, 7-5) are rejected, matching
    // robfig's 0-6 dow range bounds.
    if (lo === 7 && hi === 7) { lo = 0; hi = 0; }
    if (lo > hi || lo < 0 || hi > 6) return null;
    for (let i = lo; i <= hi; i++) result.add(i);
  }
  if (result.size === 0) return null;
  return [...result].sort((a, b) => a - b);
}

// buildFreqSchedule assembles a cron expression from a frequency descriptor.
// Returns {expr, err}. err is a human-readable message when the descriptor
// is invalid (e.g. no weekday selected).
//
// v2 polish: interval mode 被移除（对普通用户概念太重）；新增 hourly
// （整点每小时）和 weekdays（Mon-Fri shortcut）。
function buildFreqSchedule(desc) {
  if (!desc) return { err: '请选择频率' };
  if (desc.mode === 'hourly') {
    return { expr: '0 * * * *' };
  }
  if (desc.mode === 'daily') {
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' * * *' };
  }
  if (desc.mode === 'weekdays') {
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' * * 1-5' };
  }
  if (desc.mode === 'weekly') {
    if (!desc.dows || desc.dows.length === 0) return { err: '至少选择一个星期几' };
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' * * ' + [...desc.dows].sort((a, b) => a - b).join(',') };
  }
  if (desc.mode === 'monthly') {
    const d = parseInt(desc.day, 10);
    if (!Number.isFinite(d) || d < 1 || d > 31) return { err: '日期必须是 1-31' };
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' ' + d + ' * *' };
  }
  return { err: '未知频率模式' };
}

function parseHHMM(s) {
  if (!s) return null;
  const m = s.match(/^(\d{1,2}):(\d{1,2})$/);
  if (!m) return null;
  const h = parseInt(m[1], 10), mm = parseInt(m[2], 10);
  if (h < 0 || h > 23 || mm < 0 || mm > 59) return null;
  return { h, m: mm };
}

// humanizeCron renders a cron expression as a short natural-language label
// for the card list. Falls back to the raw expression when it doesn't match
// a recognized shape.
function humanizeCron(expr) {
  const d = parseCronToFreq(expr);
  if (!d) {
    // parseCronToFreq only recognizes shapes the v2 frequency-picker
    // round-trips. 以下几种 hand-written / legacy shapes 不 round-trip
    // 但可以 humanize 成人类可读标签，保留给列表和 legacy hint 显示：
    //   "*/N * * * *"  → 每 N 分钟（humanizeCronStepValue）
    //   "0 */N * * *"  → 每 N 小时
    //   "M * * * *"    → 每小时 :MM（humanizeCronHourlyMinute，#2435）
    //   "@every 30m"   → 每 30 分钟  (v1 interval shape)
    //   "@every 2h"    → 每 2 小时
    //   "m h * * 1,3,5" / "m h * * 0,6" → 多选 weekly / 周末
    //     （v2 picker 的 Weekly 单选不再 round-trip 这些 shape）
    const step = humanizeCronStepValue(expr);
    if (step) return step;
    const hourlyAt = humanizeCronHourlyMinute(expr);
    if (hourlyAt) return hourlyAt;
    const legacy = humanizeCronLegacyEvery(expr);
    if (legacy) return legacy;
    const multiDow = humanizeCronMultiDow(expr);
    if (multiDow) return multiDow;
    return expr;
  }
  if (d.mode === 'hourly') return '每小时';
  if (d.mode === 'daily') return '每天 ' + d.time;
  if (d.mode === 'weekdays') return '工作日 ' + d.time;
  if (d.mode === 'weekly') {
    const names = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
    const set = new Set(d.dows);
    if (d.dows.length === 5 && [1,2,3,4,5].every(x => set.has(x))) return '工作日 ' + d.time;
    if (d.dows.length === 2 && set.has(0) && set.has(6)) return '周末 ' + d.time;
    return d.dows.map(i => names[i]).join('、') + ' ' + d.time;
  }
  if (d.mode === 'monthly') return '每月 ' + d.day + ' 日 ' + d.time;
  return expr;
}

// humanizeCronStepValue recognizes robfig/cron "step-value" shapes that the
// frequency-picker intentionally doesn't round-trip, but which operators DO
// write by hand (copy-pasted from crontab man pages, AI-generated configs,
// IM commands). Display-only — NEVER used to construct a schedule back
// from a descriptor, so the picker's round-trip invariant stays intact.
//
// Supported shapes (all 5-field cron; 6-field with seconds would be nice
// but the backend cronParser explicitly omits Second so that won't parse
// anyway — see internal/cron/job.go cronParser config):
//   "*\/N * * * *"   → 每 N 分钟          (e.g. "*\/15 * * * *")
//   "0 *\/N * * *"   → 每 N 小时（整点）   (e.g. "0 *\/6 * * *")
//
// Returns '' for anything else so the caller can fall back to raw.
// Escaped *\/ in comments to keep this JS from looking like a block
// close; at runtime it's just /*\/N/.
// humanizeCronMultiDow 为 parseCronToFreq 不再 round-trip 的多选 weekly
// shape（v2 Weekly 是单选；周末 / 周一三五等历史数据仍要能人类读）生成
// 中文标签。display-only，不构造回 schedule。
function humanizeCronMultiDow(expr) {
  if (!expr) return '';
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return '';
  const [mm, hh, dom, mon, dow] = parts;
  if (!/^\d+$/.test(mm) || !/^\d+$/.test(hh)) return '';
  if (mon !== '*' || dom !== '*' || dow === '*') return '';
  const days = parseDowField(dow);
  if (!days || days.length < 2) return '';
  const time = pad2(parseInt(hh, 10)) + ':' + pad2(parseInt(mm, 10));
  const names = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
  const set = new Set(days);
  if (days.length === 2 && set.has(0) && set.has(6)) return '周末 ' + time;
  return days.map(i => names[i]).join('、') + ' ' + time;
}

// humanizeCronLegacyEvery 识别 v1 的 @every 表达式并本地化为中文标签。
// 仅 display-only（卡片 cc-human / 编辑模态的 legacy hint）；v2 picker
// 已删掉 interval 模式，所以这个 shape 不会被 buildFreqSchedule 重新
// 产生。仅在 parseCronToFreq 返回 null 的 fallback 链里用。
function humanizeCronLegacyEvery(expr) {
  if (!expr) return '';
  const m = expr.trim().match(/^@every\s+(\d+)(m|h)$/i);
  if (!m) return '';
  const n = parseInt(m[1], 10);
  const unit = m[2].toLowerCase();
  if (!Number.isFinite(n) || n < 1) return '';
  return unit === 'h' ? ('每 ' + n + ' 小时') : ('每 ' + n + ' 分钟');
}

function humanizeCronStepValue(expr) {
  if (!expr) return '';
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return '';
  const [mm, hh, dom, mon, dow] = parts;
  if (dom !== '*' || mon !== '*' || dow !== '*') return '';
  // "*/N * * * *" — every N minutes, N must be 2..59 and > minCronInterval (5).
  // We don't guard the 5-minute backend floor here: that's the scheduler's
  // job to reject invalid jobs at create time. The label just describes
  // what the user wrote.
  let m = mm.match(/^\*\/(\d+)$/);
  if (m && hh === '*') {
    const n = parseInt(m[1], 10);
    if (n >= 2 && n <= 59) return '每 ' + n + ' 分钟';
  }
  // "0 */N * * *" — every N hours on the hour, N must be 2..23.
  m = hh.match(/^\*\/(\d+)$/);
  if (m && mm === '0') {
    const n = parseInt(m[1], 10);
    if (n >= 2 && n <= 23) return '每 ' + n + ' 小时';
  }
  return '';
}

// humanizeCronHourlyMinute recognizes "M * * * *" (hourly at minute M,
// M in 1..59) → "每小时 :MM". "0 * * * *" is the picker's Hourly mode and is
// already handled by parseCronToFreq; display-only like the other fallbacks.
function humanizeCronHourlyMinute(expr) {
  if (!expr) return '';
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return '';
  const [mm, hh, dom, mon, dow] = parts;
  if (hh !== '*' || dom !== '*' || mon !== '*' || dow !== '*') return '';
  if (!/^\d+$/.test(mm)) return '';
  const n = parseInt(mm, 10);
  if (n < 1 || n > 59) return '';
  return '每小时 :' + pad2(n);
}

// buildFreqPickerHtml renders the Claude-style compact Frequency row:
//
//   [Frequency ▾] [time] [extra: weekday ▾ / day-of-month ▾]
//
// v2 polish: 彻底移除"cron 表达式"概念和 interval 模式（5/15/30 分钟这种对
// 初级用户过于工程化），只保留 Hourly / Daily / Weekdays / Weekly / Monthly
// 五档——覆盖绝大多数实际用例，表达方式清晰。preset 按钮 / 多次运行预览 /
// 高级 raw cron 输入全部删除，对齐 Claude Scheduled Tasks 的简洁直觉。
//
// 后端约束：cron.minCronInterval=5m，Hourly (60m) 及以上都满足，无需前端
// 再提示。Monthly 的日期超过当月最后一天时 robfig/cron 自动跳过，无需警告
// 文案污染 UI。
//
// initial 是可选的 descriptor 用来回填（编辑流），默认 Daily 9:00。
function buildFreqPickerHtml(initial) {
  const d = initial || { mode: 'daily', time: '09:00' };
  const mode = d.mode || 'daily';
  const modeOption = (m, label) =>
    '<option value="' + m + '"' + (mode === m ? ' selected' : '') + '>' + esc(label) + '</option>';

  // time 从当前 descriptor 取；hourly 不需要 time（置为空 placeholder）。
  // onchange/oninput 先 freqMarkTouched() 再 freqUpdate()——只有用户真的
  // 动过控件才写 overlay._cronSchedule。见 freqMarkTouched 注释的数据
  // 损坏场景。
  const time = d.time || '09:00';
  const timeInput =
    '<input class="freq-time' + (mode === 'hourly' ? ' nz-hidden' : '') + '" id="freq-time" type="time" value="' + esc(time) + '"' +
      ' data-action-change="cron-freq-update" data-action-input="cron-freq-update"' + '>';

  // weekly 的星期下拉（单选）。默认 Monday。
  const weeklyDow = (mode === 'weekly' && Array.isArray(d.dows) && d.dows.length > 0) ? d.dows[0] : 1;
  const dowOption = (i, label) =>
    '<option value="' + i + '"' + (weeklyDow === i ? ' selected' : '') + '>' + esc(label) + '</option>';
  const weeklySelect =
    '<select class="freq-extra' + (mode === 'weekly' ? '' : ' nz-hidden') + '" id="freq-weekly-dow" data-action-change="cron-freq-update"' + '>' +
      dowOption(1, '星期一') + dowOption(2, '星期二') + dowOption(3, '星期三') +
      dowOption(4, '星期四') + dowOption(5, '星期五') + dowOption(6, '星期六') +
      dowOption(0, '星期日') +
    '</select>';

  // monthly 的日期下拉
  const monthlyDay = (mode === 'monthly' && d.day) ? d.day : 1;
  let dayOpts = '';
  for (let i = 1; i <= 31; i++) {
    dayOpts += '<option value="' + i + '"' + (monthlyDay === i ? ' selected' : '') + '>' + i + ' 日</option>';
  }
  const monthlySelect =
    '<select class="freq-extra' + (mode === 'monthly' ? '' : ' nz-hidden') + '" id="freq-monthly-day" data-action-change="cron-freq-update"' + '>' +
      dayOpts +
    '</select>';

  return '<div class="freq-row-inline">' +
      '<select class="freq-mode-select" id="freq-mode-select" aria-label="频率模式" data-action-change="cron-freq-mode">' +
        modeOption('hourly', 'Hourly') +
        modeOption('daily', 'Daily') +
        modeOption('weekdays', 'Weekdays') +
        modeOption('weekly', 'Weekly') +
        modeOption('monthly', 'Monthly') +
      '</select>' +
      timeInput +
      weeklySelect +
      monthlySelect +
    '</div>' +
    '<div class="freq-hint">任务会在上述时间点后 0-2 分钟内随机启动（防并发峰值）。' + esc(cronTimezoneNote()) + '</div>';
}

// freqCurrentDescriptor reads the picker state back into a descriptor.
// Returns null when the picker is absent.
//
// Descriptor shapes:
//   hourly   -> { mode:'hourly' }
//   daily    -> { mode:'daily',  time:'HH:MM' }
//   weekdays -> { mode:'weekdays', time:'HH:MM' }   // Mon-Fri，buildFreqSchedule 会展开成 dows=[1..5]
//   weekly   -> { mode:'weekly', time:'HH:MM', dows:[N] }  // 单选
//   monthly  -> { mode:'monthly', time:'HH:MM', day:N }
function freqCurrentDescriptor() {
  const sel = document.getElementById('freq-mode-select');
  if (!sel) return null;
  const mode = sel.value;
  const time = (document.getElementById('freq-time') || {}).value || '09:00';
  if (mode === 'hourly') {
    return { mode };
  }
  if (mode === 'daily') {
    return { mode, time };
  }
  if (mode === 'weekdays') {
    return { mode, time };
  }
  if (mode === 'weekly') {
    const dow = parseInt((document.getElementById('freq-weekly-dow') || {}).value, 10);
    return { mode, time, dows: Number.isFinite(dow) ? [dow] : [1] };
  }
  if (mode === 'monthly') {
    const day = parseInt((document.getElementById('freq-monthly-day') || {}).value, 10);
    return { mode, time, day: Number.isFinite(day) ? day : 1 };
  }
  return null;
}

// freqSelectMode 切换频率模式。根据模式显示/隐藏 time / weekly-dow /
// monthly-day 三个辅助控件。hourly 无 time（整点即跑）。
// 用户主动切 mode 算 "touched"——之后 freqUpdate 才开始把 picker 结果
// 写入 overlay._cronSchedule；见 freqMarkTouched 的注释。
function freqSelectMode(mode) {
  const time = document.getElementById('freq-time');
  const dow = document.getElementById('freq-weekly-dow');
  const day = document.getElementById('freq-monthly-day');
  // Class toggle, matching the renderer's .nz-hidden (#2559 D6-3): mixing an
  // inline display with the class would leave the stale inline value winning.
  if (time) time.classList.toggle('nz-hidden', mode === 'hourly');
  if (dow) dow.classList.toggle('nz-hidden', mode !== 'weekly');
  if (day) day.classList.toggle('nz-hidden', mode !== 'monthly');
  freqMarkTouched();
  freqUpdate();
}

// freqMarkTouched 标记用户真的交互过频率控件。编辑流里，打开 modal 时
// overlay._cronSchedule 被 seed 成 job.schedule 的原始值（可能是无法
// round-trip 的 legacy shape，如 @every 30m 或 * * * * 1,3,5）；只有
// 用户真的动过 freq-mode-select / freq-time / freq-weekly-dow /
// freq-monthly-day 才允许 freqUpdate 覆盖这个 seed——否则"打开旧任务
// 未改频率即保存"会把原 schedule 静默改成 UI 默认的 Daily 09:00，
// 造成数据损坏。
// 创建流：createNewCronJob 显式调用 freqMarkTouched() 让初始 Daily 09:00
// 立刻写入，保证"打开即保存"能提交合法 schedule。
function freqMarkTouched() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  overlay._cronScheduleTouched = true;
}

// freqUpdate refreshes overlay._cronSchedule from the current picker state.
// v2 polish: advanced raw-cron input and multi-run preview 已移除；submit
// 路径只需要一个 cron expression，由 freqCurrentDescriptor + buildFreqSchedule
// 产出即可。
//
// Gating by _cronScheduleTouched：不动用户"未触碰"的 seed（见
// freqMarkTouched 注释的数据损坏场景）。
function freqUpdate() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  if (!overlay._cronScheduleTouched) return;
  const desc = freqCurrentDescriptor();
  const { expr } = buildFreqSchedule(desc);
  overlay._cronSchedule = expr || '';
}

// v2 polish: previewFreqSchedule / doPreviewFreq / renderFreqPreview /
// freqToggleAdvanced 在改造后全部删除。多次运行预览 + raw cron 表达式
// 入口已从 modal 中移除（对初级用户过于工程化）；submit 路径不再需要
// 经过 preview 即可判定 schedule 是否合法——后端 validateSchedule 会在
// AddJob 时兜底返回 400。

// buildScheduleSection renders the frequency picker. v2 polish 之后只剩下
// 单行 picker（mode select + time + optional weekday/day-of-month），没有
// 预览面板和 raw cron 入口。
//
// initialRawExpr 非空表示调用方检测到了一个"无法被 v2 picker round-trip"
// 的老 schedule（@every 30m、* * * * 1,3,5 等）——picker 渲染默认 Daily
// 09:00，但 overlay._cronScheduleTouched 会被编辑流置为 false，原 schedule
// 保留在 overlay._cronSchedule 里，直到用户真的动一次控件才覆盖。
// 为了避免"UI 显示 Daily 09:00 但实际不是"的视觉误导，我们在 picker 上方
// 插一条轻量 hint："当前频率：每 30 分钟（动下方控件即切换到新频率）"。
function buildScheduleSection(initialDesc, initialRawExpr) {
  const pickerHtml = buildFreqPickerHtml(initialDesc);
  if (!initialRawExpr) return pickerHtml;
  const human = humanizeCron(initialRawExpr);
  return '<div class="freq-legacy-hint" role="note">' +
      '当前频率：<b>' + esc(human) + '</b>' +
      '<span class="freq-legacy-sub">这是 v1 的老格式。如需修改，请用下方控件选一个新频率。</span>' +
    '</div>' +
    pickerHtml;
}

// setCronTimezoneMeta publishes the scheduler timezone from GET /api/cron
// list meta. Module state lives here with its readers; fetchCronJobs
// (cron_view.js) is the single writer.
function setCronTimezoneMeta(data) {
  cronTimezone = (data && data.timezone) || '';
  cronTimezoneAbbr = (data && data.timezone_abbr) || '';
  cronTimezoneLabel = (data && data.timezone_label) || '';
}

export {
  buildScheduleSection,
  freqMarkTouched,
  freqSelectMode,
  freqUpdate,
  humanizeCron,
  parseCronToFreq,
  setCronTimezoneMeta,
  cronTimezoneSuffix,
};
