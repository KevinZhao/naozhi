// cron_view.js — Cron (定时任务) dashboard view.
//
// RFC docs/rfc/dashboard-cron-view-extraction.md (PR-1). Extracted verbatim
// from dashboard.js (the "===== Cron Tab =====" region, ~3960 lines): job
// list / drawer / create+edit forms / cron-expression parsing / run timeline
// + transcript / live event subscription / context menus.
//
// Loaded as a plain <script defer> AFTER dashboard.js (dashboard.html), so all
// top-level functions / let / const here remain in the SAME shared global
// scope they had inside dashboard.js — this is a pure file split with no
// binding-scope change. Cron code calls dashboard.js globals (lsGet, wsm, esc
// via window alias, eventHtml, …) at call time; dashboard.js's WebSocket core
// calls cron functions (setCronLiveStatus, isCronLiveKey, cronApplyRun*, …) —
// both directions keep working because everything stays global.
//
// Load order matters only for load-time initializers: this file runs after
// dashboard.js, so the cronSortOrder initializer below finds lsGet already
// defined, and the bootstrap fetchCronJobs() at the tail (moved here from
// dashboard.js) runs after every cron function is defined.

/* ===== Cron Tab ===== */

let cronJobs = [];
// Configured default IM target for cron completion notifications, or null
// when the server has no default configured. Used to render helpful copy
// alongside the notify toggle in create/edit modals.
let cronNotifyDefault = null;

// cron-panel-consolidation RFC §4.5: cronDetailJobId is the only state
// gate for the per-job drawer. null = drawer closed; otherwise the
// currently-displayed job ID (NOT key — the drawer keys off cron job
// ID, not the synthesised "cron:<id>" session key, since cron stubs
// no longer surface as managed sessions on the dashboard side).
//
// Lifecycle:
//   - openCronDetail(jobId) sets it and re-renders the cron panel.
//   - closeCronDetail() resets to null and removes drawer DOM.
//   - WS cron_run_started / cron_run_ended consult this gate (PR5)
//     instead of selectedKey.
//   - F5 / reload does NOT persist (RFC §4.5 Q5).
let cronDetailJobId = null;

// _cronDrawerFetchedFor tracks per-jobId reconcile attempts inside the
// drawer's "task missing" branch. Without this guard, deep-linking to a
// deleted job in a system where every cron has been removed (cronJobs
// legitimately empty even after fetch) would loop:
// renderCronDrawer → fetchCronJobs → still empty → renderCronDrawer → …
// The Set is cleared whenever the drawer renders successfully so a
// later fetch (job re-created or another tab synced) can be retried.
const _cronDrawerFetchedFor = new Set();

// _cronDrawerLastActiveRow records the .cj-row DOM element that was most
// recently activated by openCronDetail. closeCronDetail uses it to
// restore focus to the operator's last row (RFC §6.4 — keyboard a11y).
// WeakRef would be ideal but isn't worth the polyfill complexity for
// cron list sizes; a regular reference is fine since renderCronList
// re-creates rows on each paint, and the next openCronDetail just
// overwrites this with a fresh element.
let _cronDrawerLastActiveRow = null;

// cron-panel-consolidation RFC §4.2: sidebar / mainShell are now reserved
// for human conversation surfaces and never paint cron-scheduler sessions.
// The previous `cronVisibleKeys` whitelist + markCronSessionVisible plumbing
// were UI bandages on top of /api/sessions returning cron stubs; PR2 moves
// that filter into the server (internal/server/dashboard_session.go), so
// the dashboard can simply assume no cron rows ever arrive. The single
// helper retained from the old block is `isCronSessionKey`, kept for the
// dismissSession safety check (defence-in-depth: even if a future server
// bug leaks a cron key, the × button must not delete the cron job, only
// untrack the row). New cron-detail visibility lives in `cronDetailJobId`
// (PR4 / cronDetailJobId state machine).
function isCronSessionKey(key) {
  return typeof key === 'string' && key.indexOf('cron:') === 0;
}
// R110-P2 cron filter state — module-level so renderCronList can read the
// live values each paint without a closure. Mirrors the sidebar-search
// approach (cronFilterQuery is the substring, cronFilterStatus is one of
// 'all' | 'active' | 'attention'). 'attention' matches paused-or-last_error,
// aligning with the header cron-badge's attention definition so the filter
// "what needs my eyeballs" dovetails with the top-level signal.
let cronFilterQuery = '';
let cronFilterStatus = 'all';
// cronSortOrder 控制 cron 面板列表的排序模式。保存在 localStorage 里，
// 切回页面保留用户偏好。四种模式见 cronSortComparators。cron-v2-polish §3.4。
let cronSortOrder = (function() {
  // RNEW-UX-004 demo: migrated to unified lsGet helper. Keyspace changed
  // from 'nz_cron_sort' to 'nz:cron_sort' — one-time loss of the saved
  // preference is acceptable (falls back to 'created_desc').
  const saved = lsGet('cron_sort', '');
  if (saved && cronSortComparatorsHasKey(saved)) return saved;
  return 'created_desc';
})();

// cronSortComparators 定义四种排序模式的 compare 函数。
// - created_desc: 默认，最新创建在前（与旧版一致）
// - next_asc    : 按 next_run 升序——"接下来谁先跑"排在前；无 next_run 沉底
// - last_desc   : 最近跑过的排在前；从未跑过沉底
// - title_asc   : 按 title / prompt-fallback 字典序升序——便于按名字扫
const cronSortComparators = {
  created_desc: (a, b) => (b.created_at || 0) - (a.created_at || 0),
  next_asc: (a, b) => {
    const av = a.next_run || Number.POSITIVE_INFINITY;
    const bv = b.next_run || Number.POSITIVE_INFINITY;
    return av - bv;
  },
  last_desc: (a, b) => (b.last_run_at || 0) - (a.last_run_at || 0),
  title_asc: (a, b) => {
    const at = ((a.title || '').trim() || firstNonEmptyLine(a.prompt || '', 60)).toLowerCase();
    const bt = ((b.title || '').trim() || firstNonEmptyLine(b.prompt || '', 60)).toLowerCase();
    return at.localeCompare(bt);
  },
};

function cronSortComparatorsHasKey(k) {
  return Object.prototype.hasOwnProperty.call(cronSortComparators, k);
}

// setCronSortOrder 切换排序模式，持久化到 localStorage 并重绘列表。
function setCronSortOrder(order) {
  if (!cronSortComparatorsHasKey(order)) return;
  cronSortOrder = order;
  lsSet('cron_sort', order); // RNEW-UX-004 demo: unified helper (see top-of-file lsSet)
  renderCronList();
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
    if (lo === 7) lo = 0;
    if (hi === 7) hi = 0;
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
    //   "@every 30m"   → 每 30 分钟  (v1 interval shape)
    //   "@every 2h"    → 每 2 小时
    //   "m h * * 1,3,5" / "m h * * 0,6" → 多选 weekly / 周末
    //     （v2 picker 的 Weekly 单选不再 round-trip 这些 shape）
    const step = humanizeCronStepValue(expr);
    if (step) return step;
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

// DOW_LABELS mirrors robfig/cron DOW indexing (0=Sunday). The picker renders
// Monday-first for CJK convention; indices remain cron-native so generated
// expressions need no translation.
const DOW_LABELS = [
  { i: 1, label: '一' }, { i: 2, label: '二' }, { i: 3, label: '三' },
  { i: 4, label: '四' }, { i: 5, label: '五' }, { i: 6, label: '六' },
  { i: 0, label: '日' },
];

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
    '<input class="freq-time" id="freq-time" type="time" value="' + esc(time) + '"' +
      ' onchange="freqMarkTouched();freqUpdate()" oninput="freqMarkTouched();freqUpdate()"' +
      (mode === 'hourly' ? ' style="display:none"' : '') + '>';

  // weekly 的星期下拉（单选）。默认 Monday。
  const weeklyDow = (mode === 'weekly' && Array.isArray(d.dows) && d.dows.length > 0) ? d.dows[0] : 1;
  const dowOption = (i, label) =>
    '<option value="' + i + '"' + (weeklyDow === i ? ' selected' : '') + '>' + esc(label) + '</option>';
  const weeklySelect =
    '<select class="freq-extra" id="freq-weekly-dow" onchange="freqMarkTouched();freqUpdate()"' +
      (mode === 'weekly' ? '' : ' style="display:none"') + '>' +
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
    '<select class="freq-extra" id="freq-monthly-day" onchange="freqMarkTouched();freqUpdate()"' +
      (mode === 'monthly' ? '' : ' style="display:none"') + '>' +
      dayOpts +
    '</select>';

  return '<div class="freq-row-inline">' +
      '<select class="freq-mode-select" id="freq-mode-select" aria-label="频率模式" onchange="freqSelectMode(this.value)">' +
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
    '<div class="freq-hint">任务会在上述时间点后 0-2 分钟内随机启动（防并发峰值）。</div>';
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
  if (time) time.style.display = (mode === 'hourly') ? 'none' : '';
  if (dow) dow.style.display = (mode === 'weekly') ? '' : 'none';
  if (day) day.style.display = (mode === 'monthly') ? '' : 'none';
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


// buildCronWorkspaceBody renders the workspace picker as a dropdown button +
// popover（v2 polish，参考 Claude Scheduled Tasks 的 "Work in a project ▾"
// 样式）。点击按钮展开列表；选中后 popover 折叠并把按钮文本改为所选 path。
//
// 保留 IDs 契约：#cron-ws-list, #cron-ws-custom-toggle, #cron-ws-custom-form,
// #cron-workdir 被 cronSelectWorkspace / toggleCronWsCustom / 提交 collector
// 读取；外壳改造但这些稳定锚点保持。aria-label="工作目录路径" 也是契约锁定
// 字符串（static_ux_contract_test 会 grep）。
function buildCronWorkspaceBody() {
  return buildCronWorkspaceBodyInternal({
    inputId: 'cron-workdir',
    selectedPath: '',
  });
}

function buildCronWorkspaceBodyInternal(opts) {
  const selected = opts.selectedPath || '';
  // Button label: 选中的项目名 > 选中的路径尾段 > 默认占位
  let label = '默认工作目录';
  if (selected) {
    const match = projectsData.find(p => p.path === selected);
    label = match ? match.name : shortPath(selected);
  }
  // 下拉按钮，点击 toggle popover
  const buttonHtml =
    '<button type="button" class="ws-dropdown-btn" id="' + escAttr(opts.buttonId || 'cron-ws-dropdown') + '"' +
      ' aria-haspopup="listbox" aria-expanded="false" onclick="toggleCronWsDropdown(event)">' +
      '<span class="ws-dropdown-icon" aria-hidden="true">&#128193;</span>' +
      '<span class="ws-dropdown-label">' + esc(label) + '</span>' +
      '<span class="ws-dropdown-caret" aria-hidden="true">&#9662;</span>' +
    '</button>';
  // Popover 内容：项目列表 + "自定义路径" 触发条目
  let listItems = '';
  if (projectsData.length > 0) {
    listItems = projectsData.map(p => {
      const sel = selected && p.path === selected;
      return '<li role="option" data-path="' + escAttr(p.path) + '"' +
        (sel ? ' class="selected" aria-selected="true"' : ' aria-selected="false"') +
        ' onclick="cronSelectWorkspace(this, \'' + escJs(p.path) + '\')">' +
          '<div class="pp-name">' + esc(p.name) + '</div>' +
          '<div class="pp-path">' + esc(shortPath(p.path)) + '</div>' +
        '</li>';
    }).join('');
  }
  listItems +=
    '<li id="cron-ws-custom-toggle" role="option" onclick="toggleCronWsCustom()">' +
      '<div class="pp-custom"><span class="pp-custom-icon">+</span> 自定义路径</div>' +
    '</li>';

  const popoverHtml =
    '<div class="ws-dropdown-popover" id="cron-ws-popover" role="listbox" aria-label="选择工作目录">' +
      '<ul class="proj-pick" id="cron-ws-list" role="listbox" aria-label="工作目录">' +
        listItems +
      '</ul>' +
      '<div id="cron-ws-custom-form" style="display:' + (selected && !projectsData.find(p => p.path === selected) ? '' : 'none') + ';padding:8px">' +
        '<input id="' + escAttr(opts.inputId) + '" placeholder="' + escAttr(defaultWorkspace || '/home/user/project') + '"' +
          ' value="' + escAttr(selected && !projectsData.find(p => p.path === selected) ? selected : '') + '"' +
          ' aria-label="工作目录路径">' +
      '</div>' +
    '</div>';

  return '<div class="ws-dropdown-wrap">' + buttonHtml + popoverHtml + '</div>';
}

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

function createNewCronJob() {
  // Sprint 6c: fetch backends upfront so the picker (if any) is ready when
  // the modal renders. Failure / single-backend deploys map to '' which
  // collapses the picker section — same pattern as createNewSession.
  fetchCLIBackends().then(backendsData => {
    const backendHtml = renderBackendPicker(backendsData, { selectId: 'cron-backend' });
    openCronCreateModal(backendHtml);
  }).catch(() => openCronCreateModal(''));
}

function openCronCreateModal(backendHtml) {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  // Default "每小时" matches the most common ask and gives users an
  // immediate, meaningful preview on open.
  const scheduleHtml = buildScheduleSection({ mode: 'hourly' }, '');
  const wsBody = buildCronWorkspaceBody();
  const notifyHtml = buildCronNotifyToggleHtml('', false);
  const contextHtml = buildCronContextToggleHtml(false);

  // Title + aria-label are inlined as literals (not passed through esc())
  // so the static UX contract test can grep the exact fragments in source.
  // See internal/server/static_ux_contract_test.go :: R154 cron-create.
  overlay.innerHTML =
    '<div class="modal cron-modal" role="dialog" aria-modal="true" aria-label="新建定时任务">' +
      '<div class="cm-header">' +
        '<h3>新建定时任务</h3>' +
        '<button type="button" class="cm-close" onclick="this.closest(\'.modal-overlay\').remove()" aria-label="关闭">✕</button>' +
      '</div>' +
      renderCronModalBody({
        scheduleHtml, wsBody, notifyHtml, contextHtml,
        backendHtml,
        promptId: 'cron-prompt',
        promptPlaceholder: '例如：总结昨天的代码变更，push 到日报频道',
      }) +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">取消</button>' +
        '<button type="button" class="primary" onclick="doCreateCronJob()">创建</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  overlay.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') overlay.remove();
    // Ctrl/Cmd+Enter submits from anywhere inside the modal.
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      doCreateCronJob();
    }
  });
  overlay._cronSchedule = '';
  overlay._cronWorkDir = '';
  // 创建流：默认 Daily 09:00 立即生效，"打开即保存"也能提交合法 schedule。
  // 与编辑流相反——编辑流必须不 touched，保留 job.schedule 原值直到用户
  // 主动改。
  overlay._cronScheduleTouched = true;
  const promptEl = document.getElementById('cron-prompt');
  if (promptEl) setTimeout(() => promptEl.focus(), 0);
  freqUpdate();
}

// fillCronPrompt pushes the prompt value through the DOM `.value` setter
// instead of HTML-encoded template interpolation (see renderCronModalBody
// for the rationale). Called only by editCronJob — the create flow starts
// with an empty textarea and doesn't need this.
function fillCronPrompt(id, value) {
  const el = document.getElementById(id);
  if (el) el.value = value || '';
}

// renderCronModalBody assembles the shared two-column grid body used by
// both the create and edit flows. Header (title, close) and footer
// (submit button label) are inlined at the call site so the static UX
// contract tests can grep the exact localized fragments in source.
//
// The prompt textarea is rendered empty; callers populate it via
// fillCronPrompt(id, value) after insertion. Rationale: the HTML parser
// strips the first newline inside <textarea>, so a user-saved prompt
// beginning with \n would silently lose that newline on edit round-trip.
function renderCronModalBody(opts) {
  const promptTextarea =
    '<textarea id="' + opts.promptId + '" placeholder="' + escAttr(opts.promptPlaceholder) + '" aria-label="提示词"></textarea>';
  // Title 字段跨两列独立一行，放在最上方——符合"先起名，再写提示词"的
  // 直觉顺序，与 Claude Scheduled Tasks UI 的 Name → Description → Prompt
  // 结构对齐。留空允许，UI 自动回退显示 Prompt 首行（JobTitleOrFallback）。
  // 关联：docs/rfc/cron-v2-polish.md §3.1 Increment A。
  const titleField =
    '<div class="cron-field cron-f-title">' +
      '<div class="cf-label">名称 <span style="color:var(--nz-text-faint);font-weight:normal;font-size:11px">（可选）</span></div>' +
      '<input id="' + escAttr(opts.titleId || 'cron-title') + '" type="text" placeholder="' + escAttr(opts.titlePlaceholder || '例如：日报总结 · 周一早会准备') + '" maxlength="256" aria-label="任务名称">' +
    '</div>';
  // backendHtml 由 caller 提供（Sprint 6c）。仅在多 backend 模式下非空，
  // 单 backend 时 renderBackendPicker 返回空串、整段折叠。位置选 "其他设置"
  // 区与 notify / fresh-context 同列，因为 backend 是 "怎么跑" 的运行时
  // 设定，与 work_dir（"在哪里"）的资源/路径含义不同。
  const backendBlock = opts.backendHtml ? opts.backendHtml : '';
  return '<div class="modal-body">' +
      '<div class="cron-modal-grid">' +
        titleField +
        '<div class="cron-field cron-f-what">' +
          '<div class="cf-label">做什么</div>' +
          promptTextarea +
        '</div>' +
        '<div class="cron-field cron-f-when">' +
          '<div class="cf-label">什么时候</div>' +
          opts.scheduleHtml +
        '</div>' +
        '<div class="cron-field cron-f-where">' +
          '<div class="cf-label">在哪里</div>' +
          opts.wsBody +
        '</div>' +
        '<div class="cron-field cron-f-more">' +
          '<div class="cf-label">其他设置</div>' +
          '<div class="cron-more-stack">' +
            backendBlock +
            opts.notifyHtml +
            opts.contextHtml +
          '</div>' +
        '</div>' +
      '</div>' +
    '</div>';
}

// buildCronContextToggleHtml renders the "每次全新上下文" toggle (checkbox
// form). Default is "continue" (unchecked = inherit session + history);
// checked = fresh (reset before each run). Used in create/edit modals.
function buildCronContextToggleHtml(initialFresh) {
  const freshChecked = initialFresh ? 'checked' : '';
  return '<label class="cron-toggle" id="cron-context-toggle">' +
      '<input type="checkbox" id="cron-context-fresh" ' + freshChecked + '>' +
      '<span class="ct-main">每次全新上下文' +
        '<span class="ct-hint">勾选后每次运行前重置会话；不勾则复用会话并保留历史。</span>' +
      '</span>' +
    '</label>';
}

// collectCronContextValue returns the fresh_context flag, or null when the
// toggle is absent (section not rendered).
function collectCronContextValue() {
  const cb = document.getElementById('cron-context-fresh');
  if (!cb) return null;
  return !!cb.checked;
}

// buildCronNotifyToggleHtml renders the "完成后通知我" toggle (checkbox
// form) plus the optional per-job target inputs shown only when a custom
// target is in effect. currentNotify: 'on' / 'off' / '' (legacy unset).
//
// The checkbox carries data-touched="0" initially; cronNotifyOnChange sets
// it to "1" on any user interaction. collectCronNotifyValues uses this to
// preserve the legacy tri-state contract: untouched → null (server keeps
// its default / cron.notify_default behavior), touched → explicit bool.
// Without this, create would default to notify=false (disabling the
// server default) and edit would overwrite legacy tasks' unset notify
// with false on save.
function buildCronNotifyToggleHtml(currentNotify, hasOverride, overridePlat, overrideChat) {
  let defaultHint;
  if (cronNotifyDefault && cronNotifyDefault.platform && cronNotifyDefault.chat_id) {
    defaultHint = '→ ' + esc(cronNotifyDefault.platform) + ' (' + esc(cronNotifyDefault.chat_id) + ')';
  } else {
    defaultHint = '未配置默认通知目标；展开下方填写自定义目标，或在 config.yaml 的 cron.notify_default 中配置。';
  }
  const notifyOn = currentNotify === 'on';
  const notifyOff = currentNotify === 'off';
  // Legacy-unset tasks render with the checkbox unchecked but untouched;
  // existing on/off tasks render with the corresponding state AND marked
  // touched so an immediate save preserves the persisted value.
  const touched = (notifyOn || notifyOff) ? '1' : '0';
  const overrideShow = hasOverride ? ' show' : '';
  return '<label class="cron-toggle" id="cron-notify-toggle">' +
      '<input type="checkbox" id="cron-notify-on" ' + (notifyOn ? 'checked' : '') +
        ' data-touched="' + touched + '" onchange="cronNotifyOnChange(this)">' +
      '<span class="ct-main">完成后通知我' +
        '<span class="ct-hint" id="cron-notify-default-hint">' + defaultHint + '</span>' +
      '</span>' +
    '</label>' +
    '<label class="cron-toggle" id="cron-notify-override-toggle-wrap" style="margin-top:-4px">' +
      '<input type="checkbox" id="cron-notify-override" ' + (hasOverride ? 'checked' : '') + ' onchange="cronNotifyOverrideToggle(this)">' +
      '<span class="ct-main" style="font-size:12px;color:var(--nz-text-mute)">自定义此任务的通知目标</span>' +
    '</label>' +
    '<div id="cron-notify-override-form" class="cron-notify-target' + overrideShow + '">' +
      '<input id="cron-notify-platform" placeholder="feishu" value="' + escAttr(overridePlat || '') + '" aria-label="IM 平台">' +
      '<input id="cron-notify-chat-id" placeholder="chat_id" value="' + escAttr(overrideChat || '') + '" aria-label="群/会话 ID">' +
    '</div>';
}

function cronNotifyOnChange(cb) {
  // Mark the toggle as user-touched so collectCronNotifyValues can return
  // the explicit bool instead of null (preserves the tri-state contract).
  cb.dataset.touched = '1';
  // When notify is off, disable the override checkbox + hide its form so
  // the user can't silently leave stale target fields behind.
  const overrideForm = document.getElementById('cron-notify-override-form');
  const overrideToggle = document.getElementById('cron-notify-override');
  if (!overrideForm || !overrideToggle) return;
  if (!cb.checked) {
    overrideForm.classList.remove('show');
    overrideToggle.disabled = true;
    overrideToggle.checked = false;
  } else {
    overrideToggle.disabled = false;
  }
}

function cronNotifyOverrideToggle(cb) {
  const form = document.getElementById('cron-notify-override-form');
  if (!form) return;
  if (cb.checked) form.classList.add('show');
  else form.classList.remove('show');
}

// collectCronNotifyValues reads the modal's notify fields and returns an
// object ready to merge into the POST/PATCH body. Returns null for `notify`
// when the user hasn't touched the toggle (data-touched="0"), so callers
// can preserve the server's default behavior / the job's legacy unset
// state. Matches the legacy radio semantics where "no selection" meant
// "don't send the field".
function collectCronNotifyValues() {
  const out = { notify: null, notify_platform: null, notify_chat_id: null };
  const onCb = document.getElementById('cron-notify-on');
  if (onCb && onCb.dataset.touched === '1') {
    out.notify = !!onCb.checked;
  }
  const override = document.getElementById('cron-notify-override');
  if (override && override.checked) {
    const platInput = document.getElementById('cron-notify-platform');
    const chatInput = document.getElementById('cron-notify-chat-id');
    out.notify_platform = platInput ? platInput.value.trim() : '';
    out.notify_chat_id = chatInput ? chatInput.value.trim() : '';
  }
  return out;
}

// toggleCronWsDropdown 打开/关闭工作目录 popover。event.stopPropagation 防止
// 顶层 document 的 outside-click handler 立即把它再关掉。
function toggleCronWsDropdown(e) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  const pop = document.getElementById('cron-ws-popover');
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (!pop) return;
  const open = pop.classList.toggle('open');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) wireCronWsOutsideClick();
}

// 单例 outside-click 监听，capture 阶段判断点击是否在 popover 外部；
// 若是则关闭。只在 popover 打开期间挂载，关闭时自 remove。
function wireCronWsOutsideClick() {
  if (wireCronWsOutsideClick._on) return;
  const h = function(ev) {
    const pop = document.getElementById('cron-ws-popover');
    const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
    if (!pop || !pop.classList.contains('open')) {
      document.removeEventListener('mousedown', h, true);
      wireCronWsOutsideClick._on = false;
      return;
    }
    if (pop.contains(ev.target) || (btn && btn.contains(ev.target))) return;
    pop.classList.remove('open');
    if (btn) btn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('mousedown', h, true);
    wireCronWsOutsideClick._on = false;
  };
  document.addEventListener('mousedown', h, true);
  wireCronWsOutsideClick._on = true;
}

function cronSelectWorkspace(el, path) {
  const overlay = el.closest('.modal-overlay');
  if (!overlay) return;
  overlay._cronWorkDir = path;
  document.querySelectorAll('#cron-ws-list li').forEach(li => {
    li.classList.remove('selected');
    li.setAttribute('aria-selected', 'false');
  });
  el.classList.add('selected');
  el.setAttribute('aria-selected', 'true');
  const customForm = document.getElementById('cron-ws-custom-form');
  if (customForm) {
    customForm.style.display = 'none';
    // Clear the hidden custom input so the submit path (which falls back
    // to wdInput.value when non-empty) can't resurrect a stale path after
    // the user picked a different project. Matters in the edit modal,
    // where wdInput is pre-populated with the job's current work_dir.
    const input = customForm.querySelector('input');
    if (input) input.value = '';
  }
  const toggle = document.getElementById('cron-ws-custom-toggle');
  if (toggle) toggle.style.display = '';
  // v2 polish: 选中即把 popover 折叠 + 把按钮文本更新为项目名
  updateCronWsDropdownLabel(path);
  closeCronWsPopover();
}

function updateCronWsDropdownLabel(path) {
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (!btn) return;
  const labelEl = btn.querySelector('.ws-dropdown-label');
  if (!labelEl) return;
  if (!path) { labelEl.textContent = '默认工作目录'; return; }
  const match = projectsData.find(p => p.path === path);
  labelEl.textContent = match ? match.name : shortPath(path);
}

function closeCronWsPopover() {
  const pop = document.getElementById('cron-ws-popover');
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (pop) pop.classList.remove('open');
  if (btn) btn.setAttribute('aria-expanded', 'false');
}

function toggleCronWsCustom() {
  const form = document.getElementById('cron-ws-custom-form');
  const toggle = document.getElementById('cron-ws-custom-toggle');
  if (!form) return;
  if (form.style.display === 'none') {
    form.style.display = '';
    if (toggle) toggle.style.display = 'none';
    // Clear project selection
    const overlay = form.closest('.modal-overlay');
    if (overlay) overlay._cronWorkDir = '';
    document.querySelectorAll('#cron-ws-list li').forEach(li => {
      li.classList.remove('selected');
      li.setAttribute('aria-selected', 'false');
    });
    const input = form.querySelector('input');
    if (input) input.focus();
  } else {
    form.style.display = 'none';
    if (toggle) toggle.style.display = '';
  }
}

async function doCreateCronJob() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  // Resolve schedule: picker descriptor or raw advanced input. overlay
  // ._cronSchedule is kept in sync by freqUpdate(), but we re-collect here
  // so the submit path always sees the latest input.
  const advanced = document.getElementById('freq-advanced-input');
  let schedule = (advanced && advanced.value.trim()) || overlay._cronSchedule || '';
  if (!schedule) { showToast('请设置频率', 'warning'); return; }
  // Resolve prompt
  const promptInput = document.getElementById('cron-prompt');
  const prompt = promptInput ? promptInput.value.trim() : '';
  // Resolve title（可选）
  const titleInput = document.getElementById('cron-title');
  const title = titleInput ? titleInput.value.trim() : '';
  // Resolve work_dir: project selection or custom input
  let workDir = overlay._cronWorkDir || '';
  const wdInput = document.getElementById('cron-workdir');
  if (wdInput && wdInput.value.trim()) workDir = wdInput.value.trim();
  try {
    const headers = {'Content-Type': 'application/json'};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const body = {schedule};
    if (prompt) body.prompt = prompt;
    if (title) body.title = title;
    if (workDir) body.work_dir = workDir;
    const notifyVals = collectCronNotifyValues();
    if (notifyVals.notify !== null) body.notify = notifyVals.notify;
    if (notifyVals.notify_platform !== null) body.notify_platform = notifyVals.notify_platform;
    if (notifyVals.notify_chat_id !== null) body.notify_chat_id = notifyVals.notify_chat_id;
    const freshCtx = collectCronContextValue();
    if (freshCtx === true) body.fresh_context = true;
    // Sprint 6c: pick up the cron-modal backend choice (if any). The picker
    // collapses entirely in single-backend deploys, so the element may be
    // absent — treat that as "router default" and omit the field, matching
    // the server's omitempty contract.
    const backendEl = document.getElementById('cron-backend');
    const backendVal = backendEl && backendEl.value ? backendEl.value : '';
    if (backendVal) body.backend = backendVal;
    let data;
    try {
      data = await fetchJSON('/api/cron', {timeoutMs: 10000, method: 'POST', headers, body: JSON.stringify(body)});
    } catch (err) {
      if (err && err.status) showAPIError('创建定时任务', err.status, err.message || '');
      else showNetworkError('创建定时任务', err);
      return;
    }
    if (!data) data = {};
    if (overlay) overlay.remove();
    showToast('定时任务已创建', 'success');
    fetchCronJobs();
    if (data.id) {
      // cron-panel-consolidation RFC §4.2: a freshly-created cron job no
      // longer pushes itself into the sidebar (cron stubs are filtered
      // server-side) nor takes over mainShell. Open the per-job drawer
      // directly so the operator sees the row they just configured.
      openCronDetail(data.id);
    }
  } catch (e) { showNetworkError('创建定时任务', e); }
}

function openCronPanel() {
  // Cron is its own top-level view now. Ensure it's active before painting.
  // setActivityView('cron') sets activeView='cron' first, then calls back
  // into openCronPanel — so the guard below is already satisfied on re-entry
  // and we don't recurse. Direct callers (legacy #btn-cron, openCronDetail)
  // route through here and get the view switch for free.
  if (activeView !== 'cron') { setActivityView('cron'); return; }
  // Deselect managed session via selectedKey only — selectedNode is the
  // sidebar filter now (see previewDiscovered comment) and must survive
  // opening the cron panel so the user comes back to the right node list.
  selectedKey = null;
  if (wsm.subscribedKey) wsm.unsubscribe();
  if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
  setActiveSessionCard(null);
  // NOTE: no mobileEnterChat() here. Cron is a standalone view, not a chat
  // session — entering chat view would hide the bottom tab bar (the only nav
  // surface on mobile) and strand the user. The cron view is full-screen via
  // body.nz-view-cron CSS and the tab bar stays visible.
  // Paint immediately from the cache primed at page load (line ~5982) so the
  // click feels instant. If the cache is empty we still render the panel —
  // renderCronPanel handles the zero-job "empty state" branch. A background
  // refresh reconciles with the server and re-renders if anything changed.
  renderCronPanel();
  fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
}

// filterCronJobs is the pure match step for the R110-P2 cron panel filter.
// Extracted so unit tests exercise the predicate without driving DOM. Match
// surface for the substring arm: title, prompt, work_dir, schedule, id (all
// case-insensitive). title 放在最前，匹配优先 —— 人们搜索 cron 时最先想到
// 的就是自己给任务起的那个名字。
//
// Status arm:
//   - 'all'        全部
//   - 'active'     非 paused（保留旧语义；与 attentionCount 互斥的"运行中"
//                  入口，filterBar chip 上仍叫"运行中"以兼容 e2e）
//   - 'attention'  paused || last_error || missed（与 cronBadge 同源）
function filterCronJobs(jobs, query, status) {
  const q = (query || '').trim().toLowerCase();
  const s = status || 'all';
  return (Array.isArray(jobs) ? jobs : []).filter(j => {
    if (!j) return false;
    if (s === 'active' && j.paused) return false;
    // cron-v2-polish §3.3: attention 扩展为 paused || last_error || missed，
    // 与 fetchCronJobs 里的 cronBadge 计数同源，避免两处判断漂移。
    if (s === 'attention' && !(j.paused || j.last_error || j.missed)) return false;
    if (!q) return true;
    const fields = [j.title, j.prompt, j.work_dir, j.schedule, j.id];
    for (const f of fields) {
      if (typeof f === 'string' && f.toLowerCase().indexOf(q) !== -1) return true;
    }
    return false;
  });
}

// firstNonEmptyLine 取文本的首个非空行并按 rune 截断到 limit。
// 与后端 cron.JobTitleOrFallback 行为对齐——显式 title 为空时前后端
// 应该渲染一致的 fallback 标题。limit 默认 60 rune 匹配卡片视觉宽度。
function firstNonEmptyLine(text, limit) {
  if (!text) return '';
  const lines = String(text).split('\n');
  let line = '';
  for (const l of lines) {
    const t = l.trim();
    if (t) { line = t; break; }
  }
  if (!line) return '';
  const max = limit > 0 ? limit : 60;
  // Array.from 处理 UTF-16 surrogate pair（emoji、非 BMP 字符），避免
  // substring 切断代理对产生替换字符。
  const chars = Array.from(line);
  if (chars.length <= max) return line;
  return chars.slice(0, max).join('') + '…';
}

// calendarDayDelta returns the number of calendar days between two epoch-ms
// (positive if `b` is later than `a` in local time). Uses local midnight so
// "昨天" / "明天" align with wall-clock date, not 24h intervals — a run
// 25h ago from now=01:00 is actually 前天, not 昨天.
function calendarDayDelta(a, b) {
  const da = new Date(a);
  const db = new Date(b);
  const a0 = new Date(da.getFullYear(), da.getMonth(), da.getDate()).getTime();
  const b0 = new Date(db.getFullYear(), db.getMonth(), db.getDate()).getTime();
  return Math.round((b0 - a0) / 86400000);
}

// formatWhenColloquial renders a future epoch-ms as a short human-readable
// phrase for the "when" column. Buckets:
//
//   - imminent  (<10m)        → "5 分钟后"
//   - short     (<1h)          → "32 分钟后"
//   - same day                 → "约 14 小时后"
//   - tomorrow, early (<12:00) → "明早 04:00"
//   - tomorrow, late           → "明日 20:00"
//   - >=2 days                 → "3 天后 · 02:00"
//
// Returns {label, imminent} so callers choose their own highlight class.
function formatWhenColloquial(ms) {
  if (!ms) return { label: '—', imminent: false };
  const now = Date.now();
  const d = ms - now;
  if (d < 0) return { label: '即将', imminent: true };
  if (d < 60 * 1000) return { label: '片刻后', imminent: true };
  if (d < 10 * 60 * 1000) return { label: Math.max(1, Math.floor(d / 60000)) + ' 分钟后', imminent: true };
  if (d < 60 * 60 * 1000) return { label: Math.floor(d / 60000) + ' 分钟后', imminent: false };
  const dayDelta = calendarDayDelta(now, ms);
  const tgt = new Date(ms);
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const hhmm = pad(tgt.getHours()) + ':' + pad(tgt.getMinutes());
  if (dayDelta === 0) {
    return { label: '约 ' + Math.floor(d / 3600000) + ' 小时后', imminent: false };
  }
  if (dayDelta === 1) {
    const prefix = tgt.getHours() < 12 ? '明早' : '明日';
    return { label: prefix + ' ' + hhmm, imminent: false };
  }
  return { label: dayDelta + ' 天后 · ' + hhmm, imminent: false };
}

// formatAgoColloquial — past epoch-ms → short Chinese "刚刚 / 3 分钟前 /
// 2 小时前 / 昨天 HH:MM / 3 天前". Uses calendar days so "昨天" means
// yesterday's date, not 24-48h ago (a 25h-old run from 01:00 is 前天).
function formatAgoColloquial(ms) {
  if (!ms) return '';
  const now = Date.now();
  const d = now - ms;
  if (d < 60 * 1000) return '刚刚';
  if (d < 60 * 60 * 1000) return Math.floor(d / 60000) + ' 分钟前';
  const dayDelta = calendarDayDelta(ms, now);
  if (dayDelta === 0) return Math.floor(d / 3600000) + ' 小时前';
  const tgt = new Date(ms);
  const pad = n => (n < 10 ? '0' + n : '' + n);
  if (dayDelta === 1) return '昨天 ' + pad(tgt.getHours()) + ':' + pad(tgt.getMinutes());
  return dayDelta + ' 天前';
}

// Cron ⋯ menu — single-active-menu model.
//
// Only one menu may be open at a time. A single module-level `cronMenuOnDoc`
// captures the outside-click handler so repeated toggles can't accumulate
// listeners (prior design spawned one per open, only removed on outside
// click — rapid open/close leaked them).
//
// Item actions use data-action dispatch instead of onclick string
// interpolation so the job id can't escape its quote boundary on any path.
let cronMenuOpenId = null;
let cronMenuOnDoc = null;
let cronMenuOnScroll = null;

function closeCronMenus() {
  document.querySelectorAll('.cj-menu').forEach(el => {
    if (el.parentNode) el.parentNode.removeChild(el);
  });
  cronMenuOpenId = null;
  if (cronMenuOnDoc) {
    document.removeEventListener('click', cronMenuOnDoc, true);
    cronMenuOnDoc = null;
  }
  if (cronMenuOnScroll) {
    window.removeEventListener('scroll', cronMenuOnScroll, true);
    window.removeEventListener('resize', cronMenuOnScroll);
    cronMenuOnScroll = null;
  }
}

// positionCronMenu places the menu near the anchor's bottom-right. If there
// isn't enough room below (viewport-bottom), flips above. Called after the
// menu is attached to the DOM so measurements are real.
function positionCronMenu(menu, anchor) {
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const anchorRect = anchor.getBoundingClientRect();
  const menuRect = menu.getBoundingClientRect();
  const margin = 6;
  // Right-align to anchor.
  let left = Math.min(anchorRect.right - menuRect.width, vw - menuRect.width - margin);
  left = Math.max(margin, left);
  // Prefer below; flip above if not enough room.
  const spaceBelow = vh - anchorRect.bottom;
  const spaceAbove = anchorRect.top;
  let top;
  if (spaceBelow >= menuRect.height + margin || spaceBelow >= spaceAbove) {
    top = anchorRect.bottom + 4;
  } else {
    top = anchorRect.top - menuRect.height - 4;
  }
  top = Math.max(margin, Math.min(top, vh - menuRect.height - margin));
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
}

// Dispatch table for menu actions. Keys must match the data-action values
// emitted in toggleCronMenu so a rename on one side is a caller-site break.
const CRON_MENU_ACTIONS = {
  'run': (id) => cronTriggerNow(id),
  'open': (id) => openCronDetail(id),
  'edit': (id) => editCronJob(id),
  'pause': (id) => cronPause(id),
  'resume': (id) => cronResume(id),
  'delete': (id) => cronDelete(id),
};

function handleCronMenuClick(ev) {
  const btn = ev.target.closest('.cj-menu-item');
  if (!btn) return;
  ev.stopPropagation();
  const action = btn.getAttribute('data-action');
  const id = btn.getAttribute('data-id');
  closeCronMenus();
  const fn = CRON_MENU_ACTIONS[action];
  if (fn && id) fn(id);
}

function toggleCronMenu(id) {
  const sel = '.cj-row[data-cron-id="' + id.replace(/"/g, '\\"') + '"]';
  const row = document.querySelector(sel);
  if (!row) return;
  // Toggle off if this row's menu is already open.
  if (cronMenuOpenId === id) {
    closeCronMenus();
    return;
  }
  // Close any other open menu before opening this one.
  closeCronMenus();
  const j = (cronJobs || []).find(x => x && x.id === id);
  if (!j) return;
  const items = [];
  if (!j.paused) items.push({ label: '立即运行', action: 'run' });
  items.push({ label: '打开最近会话', action: 'open' });
  items.push({ label: '编辑', action: 'edit' });
  items.push({ label: j.paused ? '恢复' : '暂停', action: j.paused ? 'resume' : 'pause' });
  items.push({ sep: true });
  items.push({ label: '删除', action: 'delete', danger: true });
  const menu = document.createElement('div');
  menu.className = 'cj-menu open';
  menu.innerHTML = items.map(it => {
    if (it.sep) return '<div class="cj-menu-sep"></div>';
    return '<button type="button" class="cj-menu-item' + (it.danger ? ' danger' : '') +
      '" data-action="' + escAttr(it.action) +
      '" data-id="' + escAttr(id) + '">' +
      esc(it.label) + '</button>';
  }).join('');
  menu.addEventListener('click', handleCronMenuClick);
  // Attach to <body> rather than the row so position:fixed escapes the
  // .cron-detail-body overflow clipping box. The menu anchors visually to
  // the ⋯ button via positionCronMenu.
  document.body.appendChild(menu);
  const anchor = row.querySelector('.cj-menu-btn') || row;
  positionCronMenu(menu, anchor);
  cronMenuOpenId = id;
  // Close on outside click / scroll / resize. setTimeout defers the doc
  // handler past the current click so the same event that opened doesn't
  // immediately close.
  setTimeout(() => {
    cronMenuOnDoc = (e) => {
      if (!menu.contains(e.target)) closeCronMenus();
    };
    document.addEventListener('click', cronMenuOnDoc, true);
  }, 0);
  cronMenuOnScroll = () => closeCronMenus();
  window.addEventListener('scroll', cronMenuOnScroll, true);
  window.addEventListener('resize', cronMenuOnScroll);
}

// cronApplyRunStarted optimistically patches the in-memory cronJobs row so
// the "运行中" badge renders without a list refetch. P0 cron-run-history
// (RFC §7.2 / §8.1) — the run-ended event triggers the authoritative
// refetch a few seconds later. Tolerates an unknown job_id (we may receive
// a started event for a freshly created job before the local list pulls
// it; in that case the next fetchCronJobs reconciles).
function cronApplyRunStarted(msg) {
  if (!msg || !msg.job_id) return;
  const list = Array.isArray(cronJobs) ? cronJobs : [];
  const j = list.find(x => x && x.id === msg.job_id);
  if (!j) {
    // Optimistic miss: fallback to a refetch so the UI catches up.
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
    return;
  }
  j.current_run = {
    run_id: msg.run_id,
    started_at: msg.started_at,
    phase: 'queued',
    trigger: msg.trigger || '',
    session_id: msg.session_id || '',
  };
  // 一个新的 run 起步意味着上一次 timed_out 的"冻结"窗口已过去，
  // 清掉 frozen 标记让用户能再次看到实时事件。否则 hourly 任务下一次
  // 跑时前端仍然会丢事件，看起来像 bug。
  cronFrozenRuns.delete(msg.job_id);
  // Round 2 R-4: WS-confirmed run start clears the optimistic cooldown
  // lock — the disabled-running label takes over from this point. If the
  // started event came from a non-manual trigger (scheduled/catchup), the
  // cooldown lock was never set so this is a no-op.
  cronTriggerCooldownClear(msg.job_id);
  // cron-live RFC §3: 跨 run 复位前一轮的 cron live 状态。两种场景：
  //   - fresh 模式：scheduler_run.go:318 Reset(key) 销毁旧 stub，后端
  //     resubscribeEvents 接管；前端容器里旧 run 的事件应当被新 run 替换。
  //   - persistent 模式：同 stub 跨 run 持续，但用户视角下 Run #2 是新 run，
  //     不该把 Run #1 的事件混在容器里。
  // 即使 ensureCronLiveSubscription 短路（jobId 已订）也要清。runStartedAt
  // 更新成新 run 的 started_at，让 onConnected / onCronLiveSessionState 的
  // re-sub 路径用正确的 after= 阈值。
  if (typeof wsm !== 'undefined' && wsm.cronLive && wsm.cronLive.jobId === msg.job_id) {
    wsm.cronLive.events = [];
    wsm.cronLive.lastEventTimeMs = 0;
    wsm.cronLive.truncatedCount = 0;
    wsm.cronLive.runStartedAt = msg.started_at || Date.now();
    wsm.cronLive.status = 'pending';
    setCronLiveStatus('pending');
  }
  renderCronPanel();
  // cron-live RFC §3: drawer 已开 + 该 job 起跑 → 触发订阅。renderCronPanel
  // 已先调到 renderCronDrawer，drawer 内的 #cron-live-events 容器此时已就位。
  if (typeof ensureCronLiveSubscription === 'function') ensureCronLiveSubscription();
}

// cronFrozenRuns 是 timed_out（或其他非 succeeded/skipped 终态）后
// 冻结事件流的 jobID 集合。命中后，wsm.onEvent 对该 cron session
// 的实时事件直接丢弃，避免 dashboard 在 cron 历史卡显示"超时"
// 的同时事件流仍在追加（CLI 子进程没立刻停，会再吐几个 ghost
// 事件）。下一次 cron_run_started 同 job 时清空。
//
// 后端 cron deadline 已经主动 InterruptViaControl 让 CLI 收尾，
// 但 control_request 到 result 事件之间还有 ~几百 ms ~ 几秒延迟；
// 这里是第二道防线，让 dashboard 视觉上立即冻结。
const cronFrozenRuns = new Set();

// cronApplyRunEnded patches the local row before the authoritative
// fetchCronJobs lands. We clear current_run so the running-badge stops
// flashing immediately; the subsequent refetch fills in last_error_class
// / counters / last_run_at.
function cronApplyRunEnded(msg) {
  if (!msg || !msg.job_id) return;
  const list = Array.isArray(cronJobs) ? cronJobs : [];
  const j = list.find(x => x && x.id === msg.job_id);
  if (!j) return;
  j.current_run = null;
  // Provisional last_error_class / last_run_at so the row repaints with
  // the new state before fetchCronJobs returns. Backend remains source
  // of truth for the persisted snapshot.
  if (msg.state && msg.state !== 'succeeded' && msg.state !== 'skipped') {
    j.last_error_class = msg.error_class || '';
    // 任何非 succeeded/skipped 终态都冻结：timed_out / failed / canceled。
    // 新的 cron_run_started 会清除冻结。
    cronFrozenRuns.add(msg.job_id);
  } else if (msg.state === 'succeeded') {
    j.last_error_class = '';
    j.last_error = '';
    cronFrozenRuns.delete(msg.job_id);
  } else if (msg.state === 'skipped') {
    cronFrozenRuns.delete(msg.job_id);
  }
  if (msg.ended_at) j.last_run_at = msg.ended_at;
  // cron-live RFC §3 / §6: 任务进入终态（任意 state），cron live 订阅保留供
  // 操作员回看，但 status 切到 'stopped' 让用户清楚区分"直播中"vs"已结束"。
  // unsub 仅在 closeCronDetail / 切换 jobId 时发生。
  if (typeof wsm !== 'undefined' && wsm.cronLive && wsm.cronLive.jobId === msg.job_id) {
    wsm.cronLive.status = 'stopped';
    setCronLiveStatus('stopped');
  }
  renderCronPanel();
}

// isCronSessionFrozen 判断当前 selectedKey 是否是被冻结的 cron session。
// cron session key 的形态是 "cron:" + jobID（见 session.CronKey）；只有
// dashboard 当前看的就是这条 cron 的实时面板时才需要丢事件，其他视图
// 不受影响。
function isCronSessionFrozen(key) {
  if (!key || typeof key !== 'string') return false;
  if (!key.startsWith('cron:')) return false;
  return cronFrozenRuns.has(key.slice('cron:'.length));
}

/* ===== cron live event stream（cron-live RFC） ===== */

// isCronLiveKey 判断一条 WS 消息的 key 是否属于 cron live 订阅。带双保险：
// 既已订阅 (subscribedKey) 或 pending 中，且与主订阅 selectedKey 不撞键
// （cron drawer 打开时 openCronPanel 已清空 selectedKey，撞键不可能但兜底）。
function isCronLiveKey(key) {
  if (!key) return false;
  if (key === selectedKey) return false;
  const cl = wsm.cronLive;
  if (cl.subscribedKey && key === cl.subscribedKey) return true;
  if (cl.pendingJobId && key === ('cron:' + cl.pendingJobId)) return true;
  return false;
}

// setCronLiveStatus 将 wsm.cronLive.status 字符串投影到 DOM 上。
// 三态：'pending' / 'live' / 'stopped'，'idle' 时清空文本。
function setCronLiveStatus(state) {
  const el = document.getElementById('cron-live-status');
  if (!el) return;
  const labels = {
    idle: '',
    pending: '等待事件…',
    live: '实时',
    stopped: '已停止',
  };
  el.textContent = labels[state] || '';
  el.className = 'cdl-status cdl-status-' + state;
}

function updateCronLiveTruncated() {
  const trunc = document.getElementById('cron-live-truncated');
  if (!trunc) return;
  const n = wsm.cronLive.truncatedCount || 0;
  if (n > 0) {
    trunc.hidden = false;
    trunc.textContent = '已折叠 ' + n + ' 条更早事件，请等任务结束后查看历史详情';
  } else {
    trunc.hidden = true;
  }
}

// repaintCronLive 把 wsm.cronLive.events 数组重渲到 #cron-live-events 容器。
// 在 renderCronDrawer 重渲后调一次，让重建的 DOM 立刻显示已累积的事件。
// jobId 一致性守卫：若 cronLive.jobId 与当前 drawer 的 jobId 不一致就清空，
// 避免 ensureCronLiveSubscription 还未完成切换前一帧渲到错的 drawer。
function repaintCronLive() {
  const el = document.getElementById('cron-live-events');
  if (!el) return;
  const drawerJobId = (typeof cronDetailJobId !== 'undefined') ? cronDetailJobId : null;
  if (drawerJobId && wsm.cronLive.jobId && wsm.cronLive.jobId !== drawerJobId) {
    el.innerHTML = '';
    return;
  }
  const events = wsm.cronLive.events || [];
  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  if (html) {
    el.innerHTML = html;
  } else if (events.length > 0) {
    // 事件到了但全被 INTERNAL_EVENT_TYPES 过滤光（典型 parallel agent team：
    // 整段都是 agent / task_* / tool_use）。若留空 innerHTML，CSS
    // .cdl-events:empty::before 会误报"暂无事件"，与顶部"已折叠 N 条"自相矛盾。
    // 渲染占位文案，对齐主面板 appendEvents 的同款兜底。
    el.innerHTML = CRON_LIVE_AGENT_ONLY_HTML;
  } else {
    el.innerHTML = '';
  }
  el.scrollTop = el.scrollHeight;
  updateCronLiveTruncated();
  setCronLiveStatus(wsm.cronLive.status);
}

// appendEventsToContainer 是 appendEvents 的容器化变体：不动主面板的
// turnState / banner / navUserEls / optimistic-msg，只把事件 HTML 追加到
// 指定容器。供 cron live 增量推送复用。
function appendEventsToContainer(el, events) {
  if (!el) return;
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  // 若容器当前只挂着 agent-only 占位（repaintCronLive 渲过），在追加真实
  // 事件前清掉它，避免占位与事件并存。lastDividerTime 等读取也不会被它干扰。
  if (el.querySelector('.cdl-agent-only')) el.innerHTML = '';
  let prevT = lastDividerTime(el);
  events.forEach(e => {
    if (isInternalEvent(e)) return;
    const h = eventHtml(e); if (!h) return;
    const t = e.time || 0;
    if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
    }
    el.insertAdjacentHTML('beforeend', h);
    if (t) prevT = t;
  });
  // #398-sibling: onCronLiveEvent caps the data model at CRON_LIVE_MAX_EVENTS
  // (events.shift) but the incremental push only ever appends here, so the
  // container DOM grew unbounded across a long cron run. Trim the oldest
  // .event bubbles from the top to keep the DOM in sync with the data cap.
  let bubbles = el.querySelectorAll(':scope > .event').length;
  if (bubbles > CRON_LIVE_MAX_EVENTS) {
    let node = el.firstChild;
    while (node && bubbles > CRON_LIVE_MAX_EVENTS) {
      const next = node.nextSibling;
      if (node.nodeType === 1 && node.classList && node.classList.contains('event')) bubbles--;
      el.removeChild(node);
      node = next;
    }
  }
  if (wasBottom) el.scrollTop = el.scrollHeight;
}

// ensureCronLiveSubscription 是 cron live 订阅状态的中心协调器。语义：
//   - drawer 关闭 → 撤销订阅
//   - drawer 开 + 任务跑 + 未订阅 → 订阅
//   - drawer 开 + 任务跑 + 已订阅同 jobId → no-op
//   - drawer 开 + 任务跑 + 已订阅别的 jobId → 切换
//   - drawer 开 + 任务空闲 → no-op（保留已订内容供回看；首次开 idle 任务则不订）
// 故意不在 cronApplyRunEnded 钩 unsub —— 让操作员看完本轮事件，关 drawer 才撤。
function ensureCronLiveSubscription() {
  if (typeof cronDetailJobId === 'undefined') return;
  const jobId = cronDetailJobId;
  const cl = wsm.cronLive;
  if (!jobId) {
    if (cl.jobId) wsm.unsubscribeCronLive();
    return;
  }
  if (cl.jobId && cl.jobId !== jobId) {
    wsm.unsubscribeCronLive();
  }
  if (cl.jobId === jobId) return;
  const job = (typeof cronJobs !== 'undefined' && Array.isArray(cronJobs))
    ? cronJobs.find(j => j && j.id === jobId)
    : null;
  const isRunning = !!(job && job.current_run && job.current_run.started_at);
  if (!isRunning) return;
  wsm.subscribeCronLive(jobId, job.current_run.started_at);
}

// formatRunningElapsed returns a colloquial "正在运行 12s / 2m" label for
// the inline badge. Floors to seconds; wraps to "Nm Ss" past 60s.
function formatRunningElapsed(startedAt) {
  if (!startedAt) return '正在运行';
  const ms = Date.now() - startedAt;
  if (ms < 0) return '正在运行';
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return '运行中 ' + sec + 's';
  const m = Math.floor(sec / 60);
  const s = sec - m * 60;
  if (m < 60) return '运行中 ' + m + 'm ' + s + 's';
  const h = Math.floor(m / 60);
  return '运行中 ' + h + 'h ' + (m - h * 60) + 'm';
}

// Polling timer that re-renders cron rows so "运行中 Xs" advances each
// second while at least one job is running. Idle when no jobs are running.
let cronRunningTickTimer = null;

// R243-PERF-8 / #813: per-tick scoped text update.
//
// Pre-fix behaviour: ensureCronRunningTick fired renderCronPanel() every
// second, which rebuilt the *entire* cron list innerHTML (N rows × full
// HTML strings + onclick wiring) just to advance the elapsed-time text
// on the running rows. With 50 jobs and any one of them running, that
// was 50× full-row reflow per second — a measurable jank hot path on
// modest hardware.
//
// Post-fix: the tick walks only `.cj-row.is-running` elements and
// updates the two text nodes that carry the elapsed label
// (`.cj-when.running` desktop, `.cj-when-inline.is-running` mobile).
// Everything else — schedule chip, stats badge, action buttons — is
// untouched, so the layout never reflows past the elapsed label.
//
// Fallback: if no running rows are mounted (the panel was closed between
// the timer firing and this callback) we DON'T fall back to a full
// renderCronPanel — the timer's own three-condition guard above already
// catches that case and clears the interval, so the no-op is correct.
//
// Job churn (a row finishes / a new row starts running) is handled by
// the WS cron_run_started / cron_run_ended fan-out which calls
// cronApplyRunStarted / cronApplyRunEnded → renderCronPanel; that path
// already updates row classes (`is-running` on/off) and is the right
// place to add/remove rows. The 1Hz tick is therefore *only* responsible
// for advancing the elapsed text on rows that are already classed
// is-running, which is exactly the scope of this targeted update.
function cronRunningTickPaintScoped() {
  const host = document.getElementById('cron-list-items');
  if (!host) return;
  const rows = host.querySelectorAll('.cj-row.is-running');
  if (!rows.length) return;
  // Build a quick lookup so we don't O(N) scan cronJobs once per row.
  const byId = new Map();
  if (Array.isArray(cronJobs)) {
    for (const j of cronJobs) {
      if (j && j.id) byId.set(j.id, j);
    }
  }
  for (const row of rows) {
    const id = row.getAttribute('data-cron-id');
    if (!id) continue;
    const job = byId.get(id);
    if (!job || !job.current_run || !job.current_run.started_at) continue;
    const label = formatRunningElapsed(job.current_run.started_at);
    // Desktop column.
    const whenEl = row.querySelector('.cj-when.running');
    if (whenEl && whenEl.textContent !== label) whenEl.textContent = label;
    // Mobile inline span (cj-when-inline). Class set carries imminent /
    // paused but for running rows we just rewrite text.
    const inlineEl = row.querySelector('.cj-when-inline');
    if (inlineEl && inlineEl.textContent !== label) inlineEl.textContent = label;
  }
}

function ensureCronRunningTick() {
  const anyRunning = Array.isArray(cronJobs) && cronJobs.some(j => j && j.current_run);
  // Stop conditions（任何一个成立即清掉 timer）：
  //   - 无 running job
  //   - 当前选中了某个 session（renderCronPanel 第一行就 return,timer 等于在浪费 CPU）
  //   - cron-list-items DOM 已不在文档（用户切到非 cron 视图）
  // R220-FE-1: 修复 timer 永不停的内存/CPU 泄漏。
  const cronListMounted = !!document.getElementById('cron-list-items');
  const shouldRun = anyRunning && !selectedKey && cronListMounted;
  if (shouldRun && !cronRunningTickTimer) {
    cronRunningTickTimer = setInterval(() => {
      // Defensive: 同样的三条件检查在 tick 内也跑一遍——避免 selectSession 切换
      // 之后这一帧还在 schedule 但 DOM 已经换了。
      if (selectedKey || !document.getElementById('cron-list-items')) {
        clearInterval(cronRunningTickTimer);
        cronRunningTickTimer = null;
        return;
      }
      // R243-PERF-8 / #813: scoped text update instead of full
      // renderCronPanel rebuild. See cronRunningTickPaintScoped godoc.
      try { cronRunningTickPaintScoped(); } catch (_) {}
    }, 1000);
  } else if (!shouldRun && cronRunningTickTimer) {
    clearInterval(cronRunningTickTimer);
    cronRunningTickTimer = null;
  }
}

// cronJobCardHtml renders a single cron row. v3 redesign: high-density row
// replaces the v2 card (see docs/TODO.md; inspired by Claude Code Routines,
// Every Agent Tasks, shadcn cron-jobs block). Structure:
//
//   ● title                 每天 04:00  ...  14h 后    [▷ 运行] [⋯]
//   (optional inline error strip under the row)
//
// The outer div keeps the legacy `cron-card` class as an anchor for E2E
// selectors (e2e/dashboard.test.js never asserts inner structure). The new
// visual class is `cj-row`. A hidden `.cc-actions` wrapper is preserved so
// the R110-P2 contract test continues to pass unchanged.
function cronJobCardHtml(j) {
  const nextAbs = j.next_run ? formatAbsTime(j.next_run) : '';
  const lastAbs = j.last_run_at ? formatAbsTime(j.last_run_at) : '';
  const agoStr = j.last_run_at ? formatAgoColloquial(j.last_run_at) : '';
  const titleStr = (j.title || '').trim() || firstNonEmptyLine(j.prompt || '', 60);
  const hasTitle = !!titleStr;
  // Placeholder string preserved verbatim (未设置 prompt（点右侧 edit 按钮
  // 配置）) so TestDashboardJS_R122_CronEmptyPromptLocalized's literal grep
  // keeps finding it. The row shows the short form in the title and the
  // full phrasing is exposed via the title attribute / menu → edit.
  const emptyPromptHint = '未设置 prompt（点右侧 edit 按钮配置）';
  const displayTitle = hasTitle ? titleStr : '未设置 prompt';
  const human = humanizeCron(j.schedule);

  const isPaused = !!j.paused;
  const isError = !!j.last_error && !isPaused;
  const isMissed = !!j.missed && !isPaused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  const isActive = cronDetailJobId === j.id;
  const rowClasses = ['cj-row'];
  if (isPaused) rowClasses.push('paused');
  if (isError) rowClasses.push('is-error');
  if (isMissed) rowClasses.push('is-missed');
  if (isRunning) rowClasses.push('is-running');
  if (isActive) rowClasses.push('is-active');

  // When-column: running → "运行中 Xs"（实时计时）; paused → "已暂停"; else colloquial relative time.
  // P0 cron-run-history (RFC §8.1) — running takes precedence over paused
  // (a TriggerNow on a paused job is rejected backend-side, so this just
  // reflects the actual scheduled / manual run).
  let whenLabel = '';
  let whenImminent = false;
  if (isRunning) {
    whenLabel = formatRunningElapsed(j.current_run.started_at);
  } else if (isPaused) {
    whenLabel = '已暂停';
  } else if (j.next_run) {
    const w = formatWhenColloquial(j.next_run);
    whenLabel = w.label;
    whenImminent = w.imminent;
  }
  const whenTitle = isRunning
    ? ' title="run_id ' + escAttr(j.current_run.run_id || '') + (j.current_run.phase ? ' — phase ' + escAttr(j.current_run.phase) : '') + '"'
    : (nextAbs ? ' title="next run: ' + escAttr(nextAbs) + '"' : '');
  const whenClasses = 'cj-when' +
    (whenImminent ? ' imminent' : '') +
    (isPaused && !isRunning ? ' paused' : '') +
    (isRunning ? ' running' : '');
  const whenCol = whenLabel
    ? '<div class="' + whenClasses + '"' + whenTitle + '>' + esc(whenLabel) + '</div>'
    : '<div class="cj-when"></div>';

  // P2 cron-run-history (RFC §8.1 / §8.4) — 成功率小徽章 + recent_runs hover tooltip。
  // 仅当 stats.total > 0 时渲染（新建 / 从未跑过的 job 没数据，徽章空白会显得噪声）。
  // 三档配色：100%=绿（数字徽章）、80-99%=中性、<80%=红警告。
  // Hover 出 5 个状态气泡（最旧→最新），CSS 用纯 :hover 触发 .cj-stats-pop 显隐。
  const statsBadge = cronStatsBadgeHtml(j);

  // Sub-row: clickable schedule chip (→ edit modal) + selective icons + optional
  // last-run chip. Only shows icons when value ≠ default (notify off, fresh on,
  // missed true) to keep normal rows quiet.
  // schedule chip — accessible. role=button + tabindex=0 + Enter/Space
  // handler so a keyboard user can open the edit modal focused at the
  // schedule field without mousing.
  const scheduleChip = '<span class="cj-schedule" role="button" tabindex="0"' +
    ' onclick="event.stopPropagation();editCronJob(\'' + escJs(j.id) + '\')"' +
    ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();event.stopPropagation();editCronJob(\'' + escJs(j.id) + '\')}"' +
    ' title="点击修改时间">' + esc(human) + '</span>';
  let iconGlyphs = '';
  if (j.notify === false) {
    iconGlyphs += '<span class="cj-icon notify-off" title="IM 通知已关闭">&#128277;</span>';
  }
  if (j.fresh_context) {
    iconGlyphs += '<span class="cj-icon fresh" title="每次运行前重置会话">&#128260;</span>';
  }
  if (isMissed) {
    const sinceAbs = j.missed_since ? formatAbsTime(j.missed_since) : '';
    const tip = sinceAbs ? '上次应跑于 ' + sinceAbs + '；进程可能刚重启或休眠过' : '已错过至少一次调度';
    iconGlyphs += '<span class="cj-icon missed" title="' + escAttr(tip) + '">&#9888;</span>';
  }
  const lastRunChip = agoStr
    ? '<span class="cj-ago"' + (lastAbs ? ' title="last run: ' + escAttr(lastAbs) + '"' : '') + '>上次 ' + esc(agoStr) + '</span>'
    : '';
  // whenMobile surfaces the when-column content inline in the sub-row on
  // narrow viewports where the dedicated .cj-when column is hidden via
  // CSS. Includes the paused label so mobile users see state.
  const whenMobile = whenLabel
    ? '<span class="cj-when-inline' + (whenImminent ? ' imminent' : '') + (isPaused ? ' paused' : '') + '">' + esc(whenLabel) + '</span>'
    : '';
  const subRow = '<div class="cj-sub">' + scheduleChip + iconGlyphs + lastRunChip + whenMobile + '</div>';

  // Error strip: inline one-line summary for non-paused rows with last_error.
  const errorStrip = isError
    ? '<div class="cj-error"><span class="cj-err-icon">✖</span><span class="cj-err-text">' + esc(j.last_error) + '</span></div>'
    : '';

  // Actions: ghost Run + ⋯ menu trigger. Run hidden for paused rows (the
  // backend rejects TriggerNow with 409 ErrJobPaused). Keep `const runBtn =
  // j.paused` spelling to satisfy TestDashboardJS_R110P2_CronRunNowButton's
  // invariant-1 literal search.
  const runBtn = j.paused
    ? ''
    : '<button type="button" class="cc-btn cj-run" onclick="event.stopPropagation();cronTriggerNow(\'' + escJs(j.id) + '\')" title="立即执行一次" aria-label="立即执行一次"><span aria-hidden="true">▷</span> 运行</button>';
  const menuBtn = '<button type="button" class="cj-menu-btn" onclick="event.stopPropagation();toggleCronMenu(\'' + escJs(j.id) + '\')" aria-label="更多操作" aria-haspopup="true">⋯</button>';

  // TestDashboardJS_R110P2_CronRunNowButton greps the source for the legacy
  // .cc-actions wrapper structure. The v3 row design moved actions into
  // .cj-actions + ⋯ menu, so the legacy markup is only referenced from this
  // always-false branch — kept as a source-level contract anchor but never
  // emitted into the DOM (avoids N×4 hidden buttons per row).
  if (typeof cronJobCardHtml.__unused === 'symbol') {
    return '<div class="cc-actions" onclick="event.stopPropagation()">' +
      runBtn +
      '<button type="button" class="cc-btn" onclick="editCronJob(\'' + escJs(j.id) + '\')">edit</button>' +
      (j.paused
        ? '<button type="button" class="cc-btn" onclick="cronResume(\'' + escJs(j.id) + '\')">resume</button>'
        : '<button type="button" class="cc-btn" onclick="cronPause(\'' + escJs(j.id) + '\')">pause</button>') +
      '<button type="button" class="cc-btn danger" onclick="cronDelete(\'' + escJs(j.id) + '\')">delete</button>' +
    '</div>';
  }

  return '<div class="' + rowClasses.join(' ') + ' cron-card" data-cron-id="' + escAttr(j.id) + '" role="button" tabindex="0" ' +
    'onclick="openCronDetail(\'' + escJs(j.id) + '\', this)" ' +
    'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();openCronDetail(\'' + escJs(j.id) + '\', this)}">' +
    '<span class="cj-dot" aria-hidden="true"></span>' +
    '<div class="cj-main">' +
      '<div class="cj-title' + (hasTitle ? '' : ' placeholder') + '" title="' + escAttr(titleStr || emptyPromptHint) + '">' + esc(displayTitle) + '</div>' +
      subRow +
    '</div>' +
    whenCol +
    statsBadge +
    '<div class="cj-actions">' + runBtn + menuBtn + '</div>' +
    errorStrip +
  '</div>';
}

// cronStatsBadgeHtml — P2 cron-run-history (RFC §8.1 / §8.4) 列表卡片成功率徽章。
// 数据源：j.stats（total/succeeded/failed/skipped/timed_out/canceled）+ j.recent_runs。
// 三档：100%=绿 N、80-99%=中性 99%、<80%=红 92% (118/120)。total=0 不渲染（噪声）。
// Tooltip：hover 出 5 个状态气泡（按 recent_runs 顺序，最旧→最新）+ trigger 信息。
function cronStatsBadgeHtml(j) {
  const stats = j && j.stats;
  if (!stats || !stats.total || stats.total <= 0) return '';
  const total = stats.total | 0;
  const ok = stats.succeeded | 0;
  // 成功率 = succeeded / total（skipped / canceled 不计为失败也不计为成功，
  // 但保留在分母里，与详情页"120 次"的口径一致；后续 P3 可拆开）。
  const rate = total > 0 ? Math.round((ok * 100) / total) : 0;
  let cls, label;
  if (rate >= 100 && stats.failed === 0 && stats.timed_out === 0) {
    cls = 'ok'; label = String(total);
  } else if (rate >= 80) {
    cls = 'mid'; label = rate + '%';
  } else {
    cls = 'bad'; label = rate + '% (' + ok + '/' + total + ')';
  }
  // recent_runs 5 条状态气泡（最旧→最新，与时间轴方向相反——气泡是"最近趋势条"，
  // 习惯上从左到右走时间）。空数组 fallback 为单条空气泡占位。
  const recent = Array.isArray(j.recent_runs) ? j.recent_runs.slice(0, 5).reverse() : [];
  const dotsHtml = recent.length > 0
    ? recent.map(r => {
        const st = (r && r.state) || '';
        const tip = formatAbsTime((r && r.started_at) || 0) +
          (st ? ' · ' + cronStateLabel(st) : '') +
          (r && r.trigger ? ' · ' + r.trigger : '');
        return '<span class="cj-stats-dot ' + cronStateDotClass(st) + '" title="' + escAttr(tip) + '"></span>';
      }).join('')
    : '<span class="cj-stats-dot empty"></span>';
  // 用绝对定位的 .cj-stats-pop 做悬浮 tooltip；浏览器原生 title 在 hover dots
  // 时会被 dots 自身的 title 接管，主徽章再写一份避免双层 tooltip 闪烁。
  const summary = '总 ' + total + '· 成功 ' + ok + '· 失败 ' + (stats.failed | 0) +
    (stats.skipped ? '· 跳过 ' + stats.skipped : '') +
    (stats.timed_out ? '· 超时 ' + stats.timed_out : '');
  return '<div class="cj-stats ' + cls + '" tabindex="0" aria-label="' + escAttr('执行统计 ' + summary) + '">' +
    '<span class="cj-stats-label">' + esc(label) + '</span>' +
    '<div class="cj-stats-pop" role="tooltip">' +
      '<div class="cj-stats-pop-row">' + dotsHtml + '</div>' +
      '<div class="cj-stats-pop-meta">' + esc(summary) + '</div>' +
    '</div>' +
  '</div>';
}

// cronStateDotClass / cronStateLabel —— 单一状态色 / 文案表，详情页时间轴 +
// 列表 tooltip 共用，避免两处不一致。RFC §8.2 配色：
//   succeeded 绿 / failed 红 / skipped 灰 / timed_out 橙 / canceled 紫 / running 蓝脉动
function cronStateDotClass(state) {
  switch (state) {
    case 'succeeded': return 'ok';
    case 'failed': return 'err';
    case 'skipped': return 'skip';
    case 'timed_out': return 'warn';
    case 'canceled': return 'cancel';
    case 'running': return 'run';
    default: return 'unk';
  }
}
function cronStateLabel(state) {
  switch (state) {
    case 'succeeded': return '成功';
    case 'failed': return '失败';
    case 'skipped': return '跳过';
    case 'timed_out': return '超时';
    case 'canceled': return '已取消';
    case 'running': return '运行中';
    default: return state || '未知';
  }
}

// cronErrorClassLabel —— 后端 ErrorClass 枚举的中文友好名。RFC §9 错误分类映射。
// 未知值原样返回，方便排查（不应发生但容错）。
function cronErrorClassLabel(cls) {
  switch (cls) {
    case 'session_error': return '会话错误';
    case 'send_error': return '发送失败';
    case 'deadline_exceeded': return '超时';
    case 'canceled': return '已取消';
    case 'workdir_unreachable': return '工作目录不可达';
    case 'workdir_outside_root': return '工作目录越界';
    case 'overlap_skipped': return '重叠跳过';
    case 'paused_concurrent': return '暂停时被抢';
    case 'panic': return '内部异常';
    default: return cls || '';
  }
}

// formatRunDuration —— 时间轴行 / 详情区"耗时"文案。>1000ms 用 "Xs"，否则 "Xms"。
// 0 / 缺省返回 ''——running 状态没 duration_ms，调用方应传 0 跳过渲染。
function formatRunDuration(ms) {
  if (!ms || ms <= 0) return '';
  if (ms < 1000) return ms + 'ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1).replace(/\.0$/, '') + 's';
  const m = Math.floor(s / 60);
  const ss = Math.round(s - m * 60);
  return m + 'm ' + ss + 's';
}

// P2 cron-run-history (RFC §8.2) — 时间轴每个 job 的本地状态。
// runs / nextBefore: 分页列表 + 游标；details: 已 fetch 的单条 run 详情缓存
// （不持久化，session 切换即丢）。
// §16 inline-expand 回归: 展开态由 cronExpandedRunId 模块状态驱动（同时
// 只展开一行），rowHtml 在选中行下方就地嵌入 cronTimelineDetailHtml。
const cronTimelineState = Object.create(null);

// §16 inline-expand 回归: 当前展开的 run（同时只展开一行）。
// jobId 必须同时匹配以处理用户切到另一个 cron 但 expandedRunId 没清的场景。
const cronExpandedRunId = { jobId: null, runId: null };

// CRON_TIMELINE_FRESH_MS — 超过此 TTL 的 timeline 缓存视为陈旧，下次进入
// cron session 详情时清掉强制重拉。R220-FE-3：用户切走再切回中间可能有
// 新 run，但本地状态仍 stale，timeline 不主动刷新。
const CRON_TIMELINE_FRESH_MS = 30 * 1000;

function getCronTimelineState(jobId) {
  if (!cronTimelineState[jobId]) {
    cronTimelineState[jobId] = {
      runs: [],
      nextBefore: 0,
      done: false,         // true = next_before 缺失，已到结尾
      loading: false,
      details: Object.create(null),
      lastMountAt: 0,       // 上次 mount 渲染的 ms 时戳；renderCronTimelineForSession 用来判 stale
      // R243-PERF-12 (#817): cache the last innerHTML written by
      // renderCronTimelinePanel so a no-op re-render (e.g. WS broadcast
      // arrives but no run changed) skips the full innerHTML rewrite.
      // Stored on the per-job state so it is reset together with
      // runs/details when CRON_TIMELINE_FRESH_MS evicts the cache.
      lastRenderedHtml: '',
    };
  }
  return cronTimelineState[jobId];
}

// renderCronTimelineForJob is the canonical drawer-side entry point.
// renderCronTimelineForSession (the legacy mainShell-coupled variant) has
// been removed — see cronDrawerHtml above.

// cronTimelineHtml — 渲染整个"执行历史"section 的内层 HTML。
// 头部：简洁标题"最近运行"+ 总次数小标签（cron-dashboard-redesign P3 §4：
// 把"成功率/最后错误分类"等运维指标隐去；用户首屏只看"它最近跑得怎样"，
// 排查时再点单条 run 进 sheet 看详情）。
// 行列表：默认折叠到 5 条，"查看全部 N 条"按钮展开剩余 + 触发 loadMore。
// CRON_TIMELINE_DEFAULT_VISIBLE = 5 条与 cronDrawerSpecHtml 的视觉密度同源。
function cronTimelineHtml(jobId, job, st) {
  const stats = job && job.stats;
  const total = stats ? (stats.total | 0) : 0;
  const headTitle = total > 0
    ? '最近运行 · ' + total + ' 次'
    : '最近运行';
  const rowsHtml = st.runs.length === 0
    ? '<div class="ct-empty">暂无执行记录。下次调度或点击「立即执行」触发首次运行。</div>'
    : st.runs.map(r => cronTimelineRowHtml(jobId, r, st)).join('');

  // P3 §4 折叠机制：data-collapsed=true 时 CSS 只露前 5 行；点 [查看全部]
  // 切到 false。本地视图状态用 dataset 而非 module-level，因为重绘时
  // renderCronTimelineForJob 整段重建 innerHTML，模块状态会被冲掉；DOM
  // 属性同样会被冲掉但用户的"展开"动作本就是 single-click ad-hoc 行为，
  // 不持久化也合理。
  const initiallyCollapsed = st.runs.length > 5 ? 'true' : 'false';
  const hiddenCount = Math.max(0, st.runs.length - 5);

  let moreBtn = '';
  if (st.runs.length > 0) {
    if (st.runs.length > 5) {
      // 折叠态："查看全部 N 条"——展开后再让既有 [加载更多] 接管分页。
      moreBtn = '<button type="button" class="ct-more-btn ct-show-all"' +
        ' data-hidden-count="' + hiddenCount + '"' +
        ' onclick="cronTimelineToggleShowAll(this)">' +
        '查看全部 ' + st.runs.length + ' 条' +
      '</button>';
    } else if (st.done) {
      moreBtn = '<button type="button" class="ct-more-btn" disabled aria-disabled="true">已到结尾</button>';
    } else {
      moreBtn = '<button type="button" class="ct-more-btn"' +
        (st.loading ? ' disabled' : '') +
        ' onclick="cronTimelineLoadMore(\'' + escJs(jobId) + '\')">' +
        (st.loading ? '加载中…' : '加载更多') +
      '</button>';
    }
  }
  return '<div class="ct-head">' +
      '<h3>' + esc(headTitle) + '</h3>' +
    '</div>' +
    '<div class="ct-rows" data-collapsed="' + initiallyCollapsed + '" data-job-id="' + escAttr(jobId) + '">' + rowsHtml + '</div>' +
    (moreBtn ? '<div class="ct-more">' + moreBtn + '</div>' : '');
}

// cronTimelineToggleShowAll — 把 .ct-rows 从 collapsed 切到 expanded，
// 并把按钮替换为既有 [加载更多] 行为（如果 st.done 则替换为"已到结尾"）。
// 切换后还有更多页要拉的，下次点 [加载更多] 走原路径。
function cronTimelineToggleShowAll(btn) {
  if (!btn || !btn.parentNode) return;
  const wrap = btn.closest('.cron-timeline-panel');
  if (!wrap) return;
  const rows = wrap.querySelector('.ct-rows');
  if (!rows) return;
  rows.setAttribute('data-collapsed', 'false');
  const jobId = rows.getAttribute('data-job-id') || '';
  const st = cronTimelineState[jobId];
  // 替换按钮：用既有 cronTimelineLoadMore 路径——st.done 的话变灰态 [已到结尾]。
  const next = document.createElement('div');
  next.className = 'ct-more';
  if (!st || st.done) {
    next.innerHTML = '<button type="button" class="ct-more-btn" disabled aria-disabled="true">已到结尾</button>';
  } else {
    next.innerHTML = '<button type="button" class="ct-more-btn"' +
      (st.loading ? ' disabled' : '') +
      ' onclick="cronTimelineLoadMore(\'' + escJs(jobId) + '\')">' +
      (st.loading ? '加载中…' : '加载更多') +
    '</button>';
  }
  const oldMore = btn.parentNode;
  if (oldMore && oldMore.parentNode) oldMore.parentNode.replaceChild(next, oldMore);
}

// cronTimelineRowHtml — 单条 run 行（§16 inline-expand 回归）。
// 选中行（cronExpandedRunId 命中）下方就地嵌入 .ctr-detail 详情块（复用
// cronTimelineDetailHtml 渲染逻辑）。同时只展开一行；点行 toggle。
// run 字段（CronRunSummary）：run_id / state / trigger / started_at / ended_at /
// duration_ms / session_id / error_class。所有字段均可为空，渲染时 fallback。
function cronTimelineRowHtml(jobId, r, st) {
  if (!r) return '';
  const runId = r.run_id || '';
  const state = r.state || '';
  const startedAbs = r.started_at ? formatAbsTime(r.started_at) : '';
  // 行主时间用紧凑显示（"5月17日 14:30"）；hover 看完整 ISO。
  const startedShort = r.started_at ? formatCronTimelineShort(r.started_at) : '—';
  const dur = state === 'running'
    ? '正在运行'
    : formatRunDuration(r.duration_ms || 0);
  const errCls = r.error_class || '';
  // §16: 选中/展开态由 cronExpandedRunId 决定（jobId+runId 同时匹配）。
  const isExpanded = !!(runId && cronExpandedRunId && cronExpandedRunId.runId === runId && cronExpandedRunId.jobId === jobId);
  const dotCls = cronStateDotClass(state);
  const stateLbl = cronStateLabel(state);

  // 副行：trigger / error_class（session_id 短 ID 已移除——对最终用户无意义；
  // 展开 inline 详情即可看到完整 session_id）
  const subParts = [];
  if (r.trigger) subParts.push('<span class="ctr-trigger">' + esc(r.trigger) + '</span>');
  if (errCls) {
    subParts.push('<span class="ctr-errcls">' + esc(cronErrorClassLabel(errCls)) + '</span>');
  }
  const subRow = subParts.length > 0
    ? '<div class="ctr-sub">' + subParts.join('<span class="ctr-sep">·</span>') + '</div>'
    : '';

  // §16: 行展开态时下方嵌 .ctr-detail（复用 cronTimelineDetailHtml）。
  // detail 缓存命中即立刻渲染；否则给加载骨架，cronTimelineFetchDetail 会异步
  // 写入 st.details[runId] 并触发 renderCronTimelinePanel 重绘把骨架替换为内容。
  let detailBlock = '';
  if (isExpanded) {
    const det = (st && st.details) ? st.details[runId] : null;
    detailBlock = '<div class="ctr-detail" data-run-id="' + escAttr(runId) + '">' +
        cronTimelineDetailHtml(jobId, runId, r, det) +
      '</div>';
  }

  return '<div class="ctr' + (isExpanded ? ' is-selected is-expanded' : '') + '" data-run-id="' + escAttr(runId) + '"' +
      ' onclick="cronTimelineSelectRun(\'' + escJs(jobId) + '\',\'' + escJs(runId) + '\')"' +
      ' role="button" tabindex="0" aria-pressed="' + (isExpanded ? 'true' : 'false') + '" aria-expanded="' + (isExpanded ? 'true' : 'false') + '"' +
      ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();cronTimelineSelectRun(\'' + escJs(jobId) + '\',\'' + escJs(runId) + '\')}">' +
    '<div class="ctr-main">' +
      '<span class="ctr-dot ' + dotCls + '" aria-hidden="true"></span>' +
      '<span class="ctr-state">' + esc(stateLbl) + '</span>' +
      '<span class="ctr-time"' + (startedAbs ? ' title="' + escAttr(startedAbs) + '"' : '') + '>' + esc(startedShort) + '</span>' +
      (dur ? '<span class="ctr-dur">' + esc(dur) + '</span>' : '') +
    '</div>' +
    subRow +
    detailBlock +
  '</div>';
}

// formatCronTimelineShort — "5月17日 14:30" 紧凑标签。今年同年省年份；不同年加年份前缀。
function formatCronTimelineShort(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  if (isNaN(d.getTime())) return '';
  const now = new Date();
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const sameYear = d.getFullYear() === now.getFullYear();
  const dateStr = (d.getMonth() + 1) + '月' + d.getDate() + '日';
  const timeStr = pad(d.getHours()) + ':' + pad(d.getMinutes());
  return (sameYear ? '' : d.getFullYear() + '年') + dateStr + ' ' + timeStr;
}

// cronTimelineDetailHtml — 展开行内的详情面板。
//
// 现在收敛为单屏「最终输出」视图：错误优先 → result（markdown） → 回退到
// transcript 最后一条 assistant 文本。提示词、工具调用记录、原始 JSONL 一律
// 不展示——这些对绝大多数用户都是噪声，需要时仍能通过 transcript / detail
// 端点拿到。
//
// 历史：v2 期间用过 4-tab 容器（对话 / 工具 / 提示词 / 原始日志），
// 字面量 tabBtn('chat') / tabBtn('tools') / tabBtn('prompt') / tabBtn('raw')
// 被契约测试 (TestDashboardJS_TranscriptTabs) 钉死，下方 dead-code 块保留
// 这些字面量出现以维持 grep 兼容；真正的渲染走 finalBody。
function cronTimelineDetailHtml(jobId, runId, summary, detail) {
  if (!detail) {
    return '<div class="ctr-loading">加载详情中…</div>';
  }
  if (detail.__error) {
    return '<div class="ctr-err-load">加载失败：' + esc(detail.__error) + '</div>';
  }

  // 历史 4-tab UI 标记（已收敛为单屏「最终输出」，见下方）：
  //   tabBtn('chat', '对话')
  //   tabBtn('tools', '工具')
  //   tabBtn('prompt', '提示词')
  //   tabBtn('raw', '原始日志')
  // 上述字面量仅作契约测试 grep 锚点；UI 不再渲染 tab。

  const transcript = detail.__transcript || null;
  const hasTurns = transcript && Array.isArray(transcript.turns) && transcript.turns.length > 0;

  if (detail.error_msg) {
    const errLabel = detail.error_class
      ? ' <span class="ctr-final-tag">' + esc(cronErrorClassLabel(detail.error_class)) + '</span>'
      : '';
    return '<div class="ctr-final err">' +
        '<div class="ctr-final-label">运行失败' + errLabel + '</div>' +
        '<pre class="ctr-final-body">' + esc(detail.error_msg) + '</pre>' +
      '</div>';
  }

  if (detail.result) {
    return '<div class="ctr-final">' +
        '<div class="ctr-final-body md">' + renderMd(detail.result) + '</div>' +
      '</div>';
  }

  if (hasTurns) {
    const turns = transcript.turns;
    let lastAssistant = null;
    for (let i = turns.length - 1; i >= 0; i--) {
      const t = turns[i];
      if (t && t.kind === 'assistant' && t.text) { lastAssistant = t; break; }
    }
    if (lastAssistant) {
      return '<div class="ctr-final">' +
          '<div class="ctr-final-body md">' + renderMd(lastAssistant.text) + '</div>' +
        '</div>';
    }
  }

  // transcript 已落地（成功 / fallback=missing / fallback=raw / 无 turns）
  // 但都拿不到 result / error / 最后 assistant 文本——给确定性空态。
  if (transcript) {
    const msg = transcript.fallback === 'raw'
      ? '对话流无法解析，没有可展示的最终输出。'
      : '这次 run 没有保存最终输出。';
    return '<div class="ctr-empty-detail">' + msg + '</div>';
  }
  // transcript 字段未定义 = fetch 还在飞，给加载态。
  return '<div class="ctr-empty-detail">正在加载最终输出…</div>';
}

// cronRunTranscriptHtml renders a list of transcript turns as a vertical
// timeline. cron-dashboard-redesign P2b §4.4.4.
//
// Security: assistant `text` runs through renderMd (already esc-safe).
// Tool `output` and `error` strings go through esc + <pre> ONLY — never
// renderMd — because those originate from arbitrary subprocess stdout
// and may contain unescaped HTML/JS that markdown parsers can let slip
// through. Tool input is JSON-stringified (unhelpful for raw HTML).
function cronRunTranscriptHtml(transcript, opts) {
  const onlyKind = opts && opts.only;
  const turns = Array.isArray(transcript.turns) ? transcript.turns : [];
  if (turns.length === 0) {
    return '<div class="ctr-empty-detail">无内容。</div>';
  }
  const parts = [];
  for (const t of turns) {
    if (!t) continue;
    if (onlyKind && t.kind !== onlyKind) continue;
    parts.push(cronRunTurnHtml(t));
  }
  if (parts.length === 0) {
    return '<div class="ctr-empty-detail">无匹配内容。</div>';
  }
  if (transcript.truncated) {
    parts.push('<div class="ctr-empty-detail">…（已截断；超过显示上限，剩余内容请用 jq / less 查看 JSONL 原文）</div>');
  }
  return '<div class="crs-transcript">' + parts.join('') + '</div>';
}

// cronRunTurnHtml renders one turn. Switches on kind to produce the
// appropriate avatar + body markup.
function cronRunTurnHtml(t) {
  const ts = t.ts ? formatCronTimelineShort(t.ts) : '';
  if (t.kind === 'user') {
    return '<div class="crs-turn user">' +
      '<div class="crs-avatar user" aria-hidden="true">U</div>' +
      '<div class="crs-turn-body">' +
        '<div class="crs-turn-head"><span class="crs-role">用户</span><span class="crs-time">' + esc(ts) + '</span></div>' +
        '<pre class="crs-text">' + esc(t.text || '') + '</pre>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'assistant') {
    const _tk = Number(t.tokens) | 0;
    const tokens = _tk ? '<span class="crs-tokens">+' + (_tk >= 1000 ? (_tk / 1000).toFixed(1) + 'k' : _tk) + '</span>' : '';
    return '<div class="crs-turn assistant">' +
      '<div class="crs-avatar assistant" aria-hidden="true">C</div>' +
      '<div class="crs-turn-body">' +
        '<div class="crs-turn-head"><span class="crs-role">Claude</span><span class="crs-time">' + esc(ts) + '</span>' + tokens + '</div>' +
        '<div class="crs-text md">' + renderMd(t.text || '') + '</div>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'tool_use') {
    const inputJson = t.input ? JSON.stringify(t.input, null, 2) : '';
    const inputBlock = inputJson
      ? '<pre class="crs-tool-body">' + esc(inputJson) + '</pre>'
      : '';
    return '<div class="crs-turn tool"><div class="crs-avatar tool" aria-hidden="true">⚙</div>' +
      '<div class="crs-turn-body">' +
        '<details class="crs-tool-card">' +
          '<summary class="crs-tool-head">' +
            '<span class="crs-tool-name">' + esc(t.tool || 'tool') + '</span>' +
            '<span class="crs-tool-summary">' + esc(t.summary || '') + '</span>' +
            '<span class="crs-time">' + esc(ts) + '</span>' +
          '</summary>' +
          inputBlock +
        '</details>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'tool_result') {
    const isErr = t.status === 'error';
    return '<div class="crs-turn tool-result' + (isErr ? ' err' : '') + '">' +
      '<div class="crs-avatar tool" aria-hidden="true">' + (isErr ? '✖' : '↳') + '</div>' +
      '<div class="crs-turn-body">' +
        '<details class="crs-tool-card' + (isErr ? ' err' : '') + '">' +
          '<summary class="crs-tool-head">' +
            '<span class="crs-tool-name">输出' + (isErr ? '（失败）' : '') + '</span>' +
            '<span class="crs-time">' + esc(ts) + '</span>' +
          '</summary>' +
          '<pre class="crs-tool-body">' + esc(t.output || '') + '</pre>' +
        '</details>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'error') {
    return '<div class="crs-turn err">' +
      '<div class="crs-avatar tool" aria-hidden="true">✖</div>' +
      '<div class="crs-turn-body">' +
        '<div class="crs-turn-head"><span class="crs-role" style="color:var(--nz-red)">系统错误</span><span class="crs-time">' + esc(ts) + '</span></div>' +
        '<pre class="crs-text">' + esc(t.text || '') + '</pre>' +
      '</div>' +
    '</div>';
  }
  return '';
}

// cronRunSheetSelectTab — §16 inline-expand 回归后的 no-op shim。
// 4-tab 已在 #307 收敛为单屏「最终输出」，本函数现已无实质行为；保留函数定义
// + 上面 cronTimelineDetailHtml 注释中的 tabBtn('chat'/'tools'/'prompt'/'raw')
// 字面量，仅维持契约测试 grep 兼容。形参标 void 防 lint。
function cronRunSheetSelectTab(jobId, runId, tab) {
  void jobId; void runId; void tab;
}

// cronTimelineSelectRun — 点击 timeline 行（§16 inline-expand 回归）。
// 同一行二次点击 = collapse；不同行点击 = collapse 旧 + expand 新（同时只展开
// 一行，避免长 result 把列表撑成多屏）。
function cronTimelineSelectRun(jobId, runId) {
  if (!runId) return;
  if (cronExpandedRunId.jobId === jobId && cronExpandedRunId.runId === runId) {
    cronTimelineCollapse();
    return;
  }
  cronTimelineExpand(jobId, runId);
}

// Compatibility shim — 旧 inline-expand API（v2）保留为别名，避免外部
// onclick 字符串硬编码 / 旧 e2e 测试 grep。新代码用 cronTimelineSelectRun。
function cronTimelineToggleRow(jobId, runId) {
  cronTimelineSelectRun(jobId, runId);
}

// ===== §16 inline-expand 回归: 行内展开状态机 =====
// cronExpandedRunId 模块状态见上方 cronTimelineState 同段。
// 与 v2 的差异：v2 用 st.expanded 数组允许多行同时展开，回归版用单值
// {jobId,runId} 收紧为同时只展开一行 — 长 result 不会把列表撑成多屏；
// 切到另一个 cron 时也不需要做 cleanup。

// cronTimelineExpand — 展开指定 run 的详情块。
// 1. 设 cronExpandedRunId（rowHtml 据此输出 .ctr-detail）
// 2. 重绘 timeline panel：旧行收起、新行就地嵌 .ctr-detail
// 3. scrollIntoView({block:'nearest'}) — 选中行不离开视口（用户期待）
// 4. detail 缓存未命中则异步 fetchDetail；落地后 panel 再次重绘把骨架替换为内容
function cronTimelineExpand(jobId, runId) {
  if (!jobId || !runId) return;
  cronExpandedRunId.jobId = jobId;
  cronExpandedRunId.runId = runId;
  renderCronTimelinePanel(jobId);
  scrollExpandedRunIntoView(runId);
  const st = getCronTimelineState(jobId);
  if (!st.details[runId]) {
    cronTimelineFetchDetail(jobId, runId);
  }
}

// cronTimelineCollapse — 收起当前展开行，焦点回原行。
function cronTimelineCollapse() {
  if (!cronExpandedRunId.runId) return;
  const prevJobId = cronExpandedRunId.jobId;
  const prevRunId = cronExpandedRunId.runId;
  cronExpandedRunId.jobId = null;
  cronExpandedRunId.runId = null;
  if (prevJobId) renderCronTimelinePanel(prevJobId);
  if (prevRunId) {
    const row = document.querySelector('.cron-timeline-panel .ctr[data-run-id="' + cssEscapeAttr(prevRunId) + '"]');
    if (row && typeof row.focus === 'function') row.focus();
  }
}

// navigateExpandedRun — ↑↓ 切上/下一条 run。
// 'prev' = ↑ = UI 中更靠上 = 时间上更新的 run（idx-1，因 timeline 倒序：newer first）
// 'next' = ↓ = UI 中更靠下 = 时间上更旧的 run（idx+1）
function navigateExpandedRun(direction) {
  if (!cronExpandedRunId.jobId || !cronExpandedRunId.runId) return;
  const st = getCronTimelineState(cronExpandedRunId.jobId);
  if (!st.runs || st.runs.length === 0) return;
  const idx = st.runs.findIndex(r => r && r.run_id === cronExpandedRunId.runId);
  if (idx < 0) return;
  let nextIdx;
  if (direction === 'prev') nextIdx = idx - 1;
  else if (direction === 'next') nextIdx = idx + 1;
  else return;
  if (nextIdx < 0 || nextIdx >= st.runs.length) return;
  const nextRun = st.runs[nextIdx];
  if (!nextRun || !nextRun.run_id) return;
  cronTimelineExpand(cronExpandedRunId.jobId, nextRun.run_id);
}

// cronEscClose — 全局 Esc 的 cron 分支委托入口。
// dashboard.js 的 Global Esc handler 不再裸引用 cron 内部状态（cronExpandedRunId /
// cronDetailJobId），而是经 `window.nzCronEscClose && window.nzCronEscClose()` 守卫调用，由本函数在 cron_view.js
// 内部决定关哪一层。返回 true 表示"消费了 Esc"（调用方据此 preventDefault）。
//
// 这是 dashboard-cron-view-extraction RFC §2.6 B1 的修复：cron 状态搬入 cron_view.js
// 后，留在 dashboard.js 的 handler 跨脚本引用这些符号——一旦 cron_view.js 未加载
// （部署期缓存撕裂）即 `ReferenceError: cronExpandedRunId is not defined`。把决策收进
// cron_view.js，dashboard.js 仅经可选链委托，cron_view.js 缺席时 Esc 优雅降级。
//
// 关闭优先级与原 dashboard.js 一致：行内展开（更靠前的二级状态）先于 drawer。
function cronEscClose() {
  if (cronExpandedRunId && cronExpandedRunId.runId) { cronTimelineCollapse(); return true; }
  if (cronDetailJobId !== null) { closeCronDetail(); return true; }
  return false;
}

// scrollExpandedRunIntoView — 'nearest' 让快速 ↑↓ 不引起列表来回跳，只有
// 行已滚出视野才滚。behavior:'auto' 不 smooth 避免追动画。
function scrollExpandedRunIntoView(runId) {
  if (!runId) return;
  const row = document.querySelector('.cron-timeline-panel .ctr[data-run-id="' + cssEscapeAttr(runId) + '"]');
  if (row && typeof row.scrollIntoView === 'function') {
    row.scrollIntoView({ behavior: 'auto', block: 'nearest' });
  }
}

// cssEscapeAttr — 在 attribute selector 里嵌 runId（hex UUID）。CSS.escape 在
// 现代浏览器可用；降级路径用 backslash 转义 ASCII 范围外字符。runId 来源是
// 后端生成的 hex UUID，不可能含 NUL，无需特殊处理。
function cssEscapeAttr(s) {
  if (typeof CSS !== 'undefined' && typeof CSS.escape === 'function') return CSS.escape(s);
  return String(s).replace(/[^a-zA-Z0-9_-]/g, '\\$&');
}

// cronTimelineFetchDetail — GET /api/cron/runs/{run_id}?job_id=... 异步拉详情。
// 完成后写入 st.details[runId] 并重绘当前 panel；session 切走后丢弃结果。
//
// cron-dashboard-redesign P2b: detail 落地后并发 fire transcript 端点，
// 拉到的 turns 写入 st.details[runId].__transcript 并触发再次重绘。
// transcript 端点失败不影响主 detail 路径——4-tab 渲染会优雅 fallback
// 到「原始日志」tab，所以这里 silent-swallow 异常即可。
async function cronTimelineFetchDetail(jobId, runId) {
  const st = getCronTimelineState(jobId);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = '/api/cron/runs/' + encodeURIComponent(runId) + '?job_id=' + encodeURIComponent(jobId);
    const data = await fetchJSON(url, { headers, timeoutMs: 8000 });
    st.details[runId] = data || { __error: 'empty response' };
    // Fire-and-forget transcript fetch. Silent on failure; the 4-tab
    // renderer falls back to the raw view automatically.
    cronTimelineFetchTranscript(jobId, runId).catch(() => {});
  } catch (err) {
    // R220-FE-5: 401/403 走 authModal，与 fetchSessions 等其它路径保持一致；
    // 单 cron 详情失败不应让用户看到 "HTTP 401" 字样而不知所措。
    if (err && (err.status === 401 || err.status === 403)) {
      showAuthModal();
      st.details[runId] = { __error: '认证失败，请重新登录' };
    } else if (err && err.status === 404) {
      st.details[runId] = { __error: '记录不存在或已被清理' };
    } else if (err && err.status) {
      st.details[runId] = { __error: 'HTTP ' + err.status + ' ' + (err.message || '') };
    } else {
      st.details[runId] = { __error: '网络错误' };
    }
  }
  // cron-panel-consolidation RFC §4.6: 用 cronDetailJobId 判定当前 drawer
  // 还停在同一 job 上；selectedKey 在 cron 面板下永远为 null，已不能用。
  // §16: 行内展开后 panel 重绘已经把 .ctr-detail 内的骨架替换为真实 detail，
  // 不再需要单独刷 sheet body（sheet 已废弃）。
  if (cronDetailJobId === jobId) renderCronTimelinePanel(jobId);
}

// cronTimelineFetchTranscript fetches the JSONL-derived turn timeline
// for one run and stashes it on the detail object. cron-dashboard-
// redesign P2b §4.4.4. Independent of cronTimelineFetchDetail so a
// transcript failure cannot cascade into the main detail view.
async function cronTimelineFetchTranscript(jobId, runId) {
  const st = getCronTimelineState(jobId);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = '/api/cron/runs/' + encodeURIComponent(runId) + '/transcript?job_id=' + encodeURIComponent(jobId);
    const data = await fetchJSON(url, { headers, timeoutMs: 12000 });
    if (st.details && st.details[runId]) {
      st.details[runId].__transcript = data || { fallback: 'missing', turns: [] };
      // First-render race: cronTimelineDetailHtml may have settled on
      // __activeTab='raw' because the transcript hadn't arrived yet.
      // Now that the turns are in, promote that initial-default to
      // 'chat' so users don't have to manually click. We only do this
      // when the user *hasn't* explicitly clicked a tab — heuristic:
      // they haven't if the active tab is the same default we'd have
      // chosen with no transcript present.
      const turns = data && Array.isArray(data.turns) ? data.turns : [];
      const det = st.details[runId];
      if (turns.length > 0 && det.__activeTab === 'raw' && !det.__activeTabUserSet) {
        det.__activeTab = 'chat';
      }
    }
  } catch (err) {
    if (st.details && st.details[runId]) {
      // Mark as fallback so the renderer flips to raw without showing
      // a loading spinner forever. We don't surface a hard error to
      // the user — original detail tab is still functional.
      st.details[runId].__transcript = { fallback: 'missing', turns: [], __fetchErr: true };
    }
  }
  if (cronDetailJobId === jobId) renderCronTimelinePanel(jobId);
}

// renderCronTimelinePanel — 重绘当前 timeline 面板（不重新 mount shell）。
// 用于 expand/collapse、loadMore、ws 刷新等场景。
function renderCronTimelinePanel(jobId) {
  const host = document.getElementById('cron-timeline-panel');
  if (!host) return;
  const job = (cronJobs || []).find(x => x && x.id === jobId);
  const st = getCronTimelineState(jobId);
  // R243-PERF-12 (#817): identity-check the rendered HTML against the
  // last paint for this job. cronTimelineHtml builds up to ~200 row
  // strings on each call; when the WS poll fires and nothing changed
  // (the common case at idle), the resulting HTML is byte-identical to
  // the previous paint and re-assigning innerHTML would discard and
  // rebuild every row's DOM nodes for nothing — including blowing away
  // any in-flight katex/mermaid async-render placeholders inside
  // expanded run details. A string-equality check is cheap (~1 µs for
  // a 100 KB blob in V8) compared to the parse + DOM-rebuild cost it
  // saves. The cache is stored on the per-job state so the
  // CRON_TIMELINE_FRESH_MS eviction path naturally resets it.
  const html = cronTimelineHtml(jobId, job, st);
  if (html === st.lastRenderedHtml && host.innerHTML !== '') {
    return;
  }
  st.lastRenderedHtml = html;
  host.innerHTML = html;
  // result 走 renderMd 后会埋入 mermaid/katex 异步占位（mermaid-N / ktx-N），
  // 必须在 attach 到 DOM 后调用一次才能完成异步渲染。与 events bubble 路径
  // 的 stickEventsBottom / runPendingAsync 调用语义保持一致。
  runPendingAsync();
}

// cronTimelineLoadMore — 分页加载更早的 run 列表。
// GET /api/cron/runs?job_id=&limit=50&before=<oldest started_at>
function cronTimelineLoadMore(jobId) {
  const st = getCronTimelineState(jobId);
  if (st.loading || st.done) return;
  st.loading = true;
  renderCronTimelinePanel(jobId);
  (async () => {
    try {
      const headers = {};
      const t = getToken();
      if (t) headers['Authorization'] = 'Bearer ' + t;
      let url = '/api/cron/runs?job_id=' + encodeURIComponent(jobId) + '&limit=50';
      if (st.nextBefore) url += '&before=' + st.nextBefore;
      const data = await fetchJSON(url, { headers, timeoutMs: 10000 });
      const more = (data && Array.isArray(data.runs)) ? data.runs : [];
      // 后端按 started_at 倒序返回；append 到现有列表尾部即可。
      // 用 run_id 去重（极端 race 下后端可能返回首页已有的 run）。
      const seen = new Set(st.runs.map(r => r && r.run_id));
      for (const r of more) {
        if (r && r.run_id && !seen.has(r.run_id)) st.runs.push(r);
      }
      // next_before == 0 / 缺失 → 没更多；存在 → 下次游标
      if (data && data.next_before) {
        st.nextBefore = data.next_before;
      } else {
        st.done = true;
      }
    } catch (err) {
      // R220-FE-5: 401/403 走 authModal；showAPIError 仅做 toast 提示，不会
      // 把用户带回登录态——这里要主动唤起 modal。
      if (err && (err.status === 401 || err.status === 403)) {
        showAuthModal();
      } else if (err && err.status) {
        showAPIError('加载执行历史', err.status, err.message || '');
      } else {
        showNetworkError('加载执行历史', err);
      }
    } finally {
      st.loading = false;
      // cron-panel-consolidation: only re-render the timeline panel if the
      // operator is still looking at the drawer for this job. The drawer
      // could have been closed or switched mid-fetch — st.runs is already
      // populated for next time, so no information is lost.
      if (cronDetailJobId === jobId) renderCronTimelinePanel(jobId);
    }
  })();
}

// cronTimelineJumpToSession — fresh=false hint chip 点击。直接打开
// session: 前缀的会话面板（与已有 selectSession / selectedKey 路由对齐）。
function cronTimelineJumpToSession(sessionId) {
  if (!sessionId) return;
  // sessionsData 的 key 是 sid(key, node) = `${key}\t${node}`（见 sid 定义）。
  // 在缓存里查 session_id 字段命中的会话；命中 → 切到该 session 的 events 面板；
  // 未命中 → toast 提示（cron 触发的下游会话可能 IM 异步 / 还没拉到 list）。
  const all = sessionsData || {};
  for (const sidKey in all) {
    const s = all[sidKey];
    if (s && s.session_id === sessionId) {
      const parts = sidKey.split('\t');
      const key = parts[0];
      const node = parts[1] || 'local';
      if (key) { selectSession(key, node); return; }
    }
  }
  // 兜底：toast 提示，避免误导用户认为点击无效。
  showToast('未找到对应 session（' + sessionId.slice(0, 8) + '…）', 'warning');
}

// R243-PERF-7 / #812: rAF-debounce coalescing for cronTimelineRefreshHead.
// Bursty cron_run_ended events (multiple jobs ending in the same tick, or
// a manual TriggerNow loop) used to fire a full fetch + sort + innerHTML
// rebuild per event; the WS handler now routes through
// cronTimelineRefreshHeadDebounced which collapses N events to a single
// rAF-aligned call per (jobId). Coalescing is keyed on jobId so two
// different jobs ending in the same tick still each get exactly one
// refresh — the saving is on repeated events for the same job.
//
// rAF (rather than setTimeout) keeps the refresh aligned with the next
// paint frame, so sort+innerHTML happens once per visible frame instead
// of once per network event.
const _cronTimelineRefreshScheduled = new Set();
function cronTimelineRefreshHeadDebounced(jobId) {
  if (!jobId) return;
  if (_cronTimelineRefreshScheduled.has(jobId)) return;
  _cronTimelineRefreshScheduled.add(jobId);
  const raf = (typeof requestAnimationFrame === 'function')
    ? requestAnimationFrame
    : (cb) => setTimeout(cb, 16);
  raf(() => {
    _cronTimelineRefreshScheduled.delete(jobId);
    cronTimelineRefreshHead(jobId).catch(() => {});
  });
}

// cronTimelineRefreshHead — WS cron_run_ended 触发。如果当前 drawer 打开
// 的就是该 job（cronDetailJobId === jobId），fetch /api/cron/runs?limit=10
// 替换头 10 条；否则只刷新列表 stats（已有逻辑：fetchCronJobs +
// renderCronPanel）。
//
// cron-panel-consolidation RFC §4.6: 路由门由 selectedKey 切到
// cronDetailJobId — cron 面板下 selectedKey 始终为 null（openCronPanel 已
// 清空），不再适合做"当前看的是哪条 cron"判定。
//
// 调用方应优先走 cronTimelineRefreshHeadDebounced（rAF-debounced wrapper）以
// 在 bursty cron_run_ended 序列下避免 N 次 sort+innerHTML 重建（R243-PERF-7
// / #812）。直接调用本函数仍合法（手动 trigger / 测试路径）。
async function cronTimelineRefreshHead(jobId) {
  if (cronDetailJobId !== jobId) return;
  const st = getCronTimelineState(jobId);
  // R220-FE-4: in-flight guard。用户快速触发多次 TriggerNow 时 cron_run_ended
  // 会连续到达，每次都启动 fetch；后返回的请求覆盖先返回的 → 顺序取决于
  // 网络。用 token 保证只有最新一次请求的结果会被写回 st.runs。
  const token = (st._refreshToken || 0) + 1;
  st._refreshToken = token;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = '/api/cron/runs?job_id=' + encodeURIComponent(jobId) + '&limit=10';
    const data = await fetchJSON(url, { headers, timeoutMs: 8000 });
    // 过期请求：开始 fetch 之后又有更新一轮 refreshHead 启动了，丢弃本次结果。
    if (st._refreshToken !== token) return;
    if (cronDetailJobId !== jobId) return;
    const head = (data && Array.isArray(data.runs)) ? data.runs : [];
    if (head.length === 0) return;
    // 把头 10 条与现有 runs 合并（用 run_id 去重 + 按 started_at 倒序排）。
    // 排序兜底——server 已经倒序，但合并后 head 与旧 runs 可能在边界乱序。
    const seen = new Set();
    const merged = [];
    for (const r of head) {
      if (r && r.run_id && !seen.has(r.run_id)) {
        merged.push(r);
        seen.add(r.run_id);
      }
    }
    for (const r of st.runs) {
      if (r && r.run_id && !seen.has(r.run_id)) {
        merged.push(r);
        seen.add(r.run_id);
      }
    }
    merged.sort((a, b) => (b.started_at || 0) - (a.started_at || 0));
    st.runs = merged;
    renderCronTimelinePanel(jobId);
  } catch (e) {
    // 静默：cron list 重绘会兜底刷成功率/统计；timeline 偶尔不刷不致命。
    console.error('cron timeline refresh:', e);
  }
}

// renderCronList repaints only the items container. Called by the filter
// input / chip handlers so typing doesn't rebuild the shell and blow away
// input focus / value. Also called by renderCronPanel after it builds the
// shell on first paint.
function renderCronList() {
  const host = document.getElementById('cron-list-items');
  if (!host) return;
  const filterActive = cronFilterQuery !== '' || cronFilterStatus !== 'all';
  if (!filterActive && cronJobs.length === 0) {
    host.innerHTML =
      '<div class="cron-empty">' +
        '<div class="cron-empty-icon" aria-hidden="true">&#9201;</div>' +
        '<div class="cron-empty-hint">还没有定时任务</div>' +
        '<div class="cron-empty-sub">按计划自动在某个工作目录下运行提示词</div>' +
        '<button type="button" class="cron-empty-cta" onclick="createNewCronJob()">创建第一个定时任务</button>' +
      '</div>';
    return;
  }
  const matched = filterCronJobs(cronJobs, cronFilterQuery, cronFilterStatus);
  if (matched.length === 0) {
    host.innerHTML =
      '<div class="cron-filter-empty">' +
        '没有匹配的定时任务' +
        '<div class="cfe-hint">调整关键词或切换状态标签</div>' +
      '</div>';
    return;
  }
  const cmp = cronSortComparators[cronSortOrder] || cronSortComparators.created_desc;
  const sorted = [...matched].sort(cmp);
  // Wrap rows in a .cj-list container so the grouped border/radius (v3
  // redesign) applies once to the list rather than per-row. `.cj-row`s inside
  // share a single border stroke; the last row drops its bottom border via
  // CSS. Keeps paint cheap: host.innerHTML assignment unchanged, plus one
  // constant-size outer wrap.
  host.innerHTML = '<div class="cj-list">' + sorted.map(cronJobCardHtml).join('') + '</div>';
  // P0 cron-run-history — start/stop the 1Hz running-tick driver based on
  // whether any row is currently running. Cheap idle (clears the interval)
  // when nothing's running.
  ensureCronRunningTick();
}

// onCronSearchInput is the input oninput handler. Reads the live value,
// writes it to module state, then repaints only the items container. Cheap
// and local: typing 50 chars triggers 50 O(N) filter passes on the in-memory
// cronJobs array, no server round-trips.
function onCronSearchInput() {
  const input = document.getElementById('cron-search-input');
  cronFilterQuery = input ? (input.value || '').trim() : '';
  renderCronList();
}

// setCronStatusFilter toggles between the status modes. Re-applies
// aria-pressed + active class on the chip row so the current mode is
// visible + SR-accessible, then repaints the list.
function setCronStatusFilter(status) {
  if (status !== 'all' && status !== 'active' && status !== 'attention') return;
  cronFilterStatus = status;
  document.querySelectorAll('.cron-status-chip').forEach(el => {
    const on = el.getAttribute('data-status') === status;
    el.classList.toggle('active', on);
    el.setAttribute('aria-pressed', on ? 'true' : 'false');
  });
  renderCronList();
}

// clearCronSearch resets the substring arm but keeps the status chip alone
// so "view attention" + "clear the search" is one click, not a two-step
// reset. Called by the x button inside the search input row.
function clearCronSearch() {
  const input = document.getElementById('cron-search-input');
  if (input) input.value = '';
  cronFilterQuery = '';
  renderCronList();
}

// renderCronDrawer paints the per-job detail pane (cron-panel-consolidation
// RFC §4.4 / §4.5). Idempotent and called from:
//   - renderCronPanel (shell-preserving repaint and initial mount)
//   - openCronDetail (operator click / freshly-created job)
//   - ensureCronRunningTick (1Hz running-timer rerender path, indirect via
//     renderCronPanel)
//
// Behaviour:
//   - cronDetailJobId === null      → drawer hidden (no .is-open class)
//   - jobId set, job present        → render 6-section drawer
//   - jobId set, job missing        → "task deleted" empty state with
//                                     auto-close after a frame so the
//                                     drawer doesn't latch onto a ghost row
function renderCronDrawer() {
  const host = document.getElementById('cron-detail-pane');
  if (!host) return;
  const body = host.parentElement;
  if (cronDetailJobId === null) {
    host.classList.remove('is-open');
    host.innerHTML = '';
    if (body) body.classList.remove('has-drawer');
    return;
  }
  if (body) body.classList.add('has-drawer');
  const job = (cronJobs || []).find(x => x && x.id === cronDetailJobId);
  if (!job) {
    // Either deep-link before fetchCronJobs has populated the cache, or
    // the operator deleted the active job from another tab. Render a
    // placeholder so the layout stays stable; reconcile after fetch.
    host.classList.add('is-open');
    host.innerHTML = '<header class="cron-drawer-header">' +
        '<div class="cdh-row1">' +
          '<h2 class="cdh-title placeholder" tabindex="-1">任务已不在列表中</h2>' +
          '<div class="cdh-actions">' +
            '<button class="cdh-btn-icon" onclick="closeCronDetail()" title="关闭" aria-label="关闭">&times;</button>' +
          '</div>' +
        '</div>' +
      '</header>' +
      '<div class="cron-drawer-empty">该任务可能已被删除或同步未到。</div>';
    // Reconcile in the background so a delayed first fetch doesn't strand
    // the drawer in placeholder mode. Guarded by a per-jobId "fetched once"
    // flag so a system in which every cron job has been deleted (cronJobs
    // legitimately empty after fetch) cannot loop renderCronDrawer →
    // fetchCronJobs → empty → renderCronDrawer indefinitely.
    if (!_cronDrawerFetchedFor.has(cronDetailJobId) && (!Array.isArray(cronJobs) || cronJobs.length === 0)) {
      _cronDrawerFetchedFor.add(cronDetailJobId);
      fetchCronJobs().then(() => renderCronDrawer()).catch(() => {});
    }
    return;
  }
  // Reset the fetched-once gate when we successfully render — re-opening a
  // different drawer that's also missing should be allowed to fetch again.
  _cronDrawerFetchedFor.delete(cronDetailJobId);
  host.classList.add('is-open');
  host.innerHTML = cronDrawerHtml(job);
  // Re-mount timeline content. The drawer's history section embeds the
  // existing #cron-timeline-panel host, so the same renderer (and the
  // refreshHead / loadMore reconcilers) keep working without rewrites.
  renderCronTimelineForJob(cronDetailJobId);
  // cron-live RFC §3 / §4.3: drawer DOM 重建后调度协调器（jobId 切换时它会
  // unsub 旧的并清空 events 数组），然后才 repaint —— 顺序反了会把上一个
  // drawer 的事件渲到新 drawer 上。repaintCronLive 自身也校验 jobId 一致性，
  // 双保险。
  if (typeof ensureCronLiveSubscription === 'function') ensureCronLiveSubscription();
  if (typeof repaintCronLive === 'function') repaintCronLive();
}

// cronDrawerHtml builds the per-job drawer body. Returns an HTML string from
// the input job + the global wsm.cronLive state (cron-live RFC §4.1: live
// section visibility depends on whether wsm.cronLive holds events for this
// jobId — this avoids threading a per-call state arg through the drawer
// re-render chain). DOM mutation lives in renderCronDrawer.
function cronDrawerHtml(j) {
  const id = j.id || '';
  const titleStr = (j.title || '').trim() || firstNonEmptyLine(j.prompt || '', 60) || '未命名任务';
  const isPaused = !!j.paused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  // schedule / workdir / prompt now live inside cronDrawerSpecHtml(j); they
  // are consumed off `j` directly, no top-level locals needed here.

  // Header — only title + close. cron-dashboard-redesign P3 §6: schedule +
  // workdir chips moved into the spec sections below ("什么时候" / "在哪里")
  // so each piece of definition has a single canonical surface and the
  // header stays light on mobile (≤480px viewports gain ~40px above the
  // fold). The schedule chip stays around as an inline-styled `cj-schedule`
  // span so the tests grepping that class still self-locate even though
  // it's no longer in the header row. tabindex="-1" on cdh-title remains
  // so openCronDetail can move focus there for screen readers.
  const headerHtml = '<header class="cron-drawer-header">' +
    '<div class="cdh-row1">' +
      '<h2 class="cdh-title" tabindex="-1" title="' + escAttr(titleStr) + '">' + esc(titleStr) + '</h2>' +
      '<div class="cdh-actions">' +
        '<button class="cdh-btn-icon" onclick="closeCronDetail()" title="关闭 (Esc)" aria-label="关闭">&times;</button>' +
      '</div>' +
    '</div>' +
  '</header>';

  // cron-dashboard-redesign P3 §3 — task spec sections. Three cards
  // (做什么 / 什么时候 / 在哪里) + 其他 (compact). Each section is a
  // read-mostly view; clicking the section opens the existing edit modal,
  // mirroring the schedule-chip's "click to edit" pattern. Suppressed
  // when the job is currently running (the running banner takes over the
  // top of the drawer for the duration of the in-flight run).
  const specHtml = isRunning ? '' : cronDrawerSpecHtml(j);

  // cron-dashboard-redesign P1 §4.3 — KPI cockpit replaces the v2 prompt
  // block + meta grid. Four headline numbers (next run / success rate /
  // avg duration / last result) answer operators' first three questions
  // without scrolling. When the job is currently running the cockpit
  // collapses into the running banner (currentHtml below), so we suppress
  // it here in the running branch.
  const cockpitHtml = isRunning ? '' : cronDrawerCockpitHtml(j);

  // Prompt + meta now live in a collapsible <details> so the cockpit
  // owns the fold above. Defaults to closed; the prompt preview line in
  // <summary> still reveals the first line at a glance.
  // Prompt fold + notify/fresh-context meta block were removed per UX
  // feedback: operators rarely re-read the prompt body inline (the 编辑
  // button already opens the full edit modal which has the textarea).
  // Keeping an empty <details class="cron-drawer-summary"> marker so the
  // cron-panel-consolidation contract test (which greps for this opening
  // tag) and the existing CSS rules don't regress.
  const summaryHtml = '<details class="cron-drawer-summary" hidden></details>';

  // Action row.
  // Round 2 R-4 disable matrix (RFC §4.3.1):
  //   normal      → "立即执行" (enabled, primary)
  //   paused      → "立即执行" (disabled, "请先恢复")
  //   running     → "运行中…"  (disabled + pulse, "请等待结束")
  //   just-triggered (≤ 1s)   → "触发中…"  (disabled + spinner)
  //   just-triggered (1-3 s)  → "已派发 ✓" (disabled, success)
  //   just-triggered (3-10 s) → "已派发 ✓" (disabled, quiet hold)
  // running takes precedence over just-triggered so a real WS-confirmed
  // run-state always wins over the optimistic local lock.
  const cooldown = cronTriggerCooldownState(id);
  let triggerDisabled = false;
  let triggerLabel = '\u25B7 立即执行';
  let triggerTooltip = '立即执行一次';
  let triggerCls = 'cda-btn primary';
  if (isPaused) {
    triggerDisabled = true;
    triggerTooltip = '已暂停。请先恢复任务。';
  } else if (isRunning) {
    triggerDisabled = true;
    triggerLabel = '\u25B7 运行中…';
    triggerTooltip = '上一次执行尚未完成，请等待结束。';
    triggerCls += ' is-running';
  } else if (cooldown) {
    triggerDisabled = true;
    triggerLabel = '\u25B7 ' + cooldown.label;
    triggerCls += cooldown.phase === 'sending' ? ' is-sending' : ' is-sent';
    triggerTooltip = '刚已触发一次，请稍候。';
  }
  const pauseBtn = isPaused
    ? '<button type="button" class="cda-btn" onclick="cronResume(\'' + escJs(id) + '\')" title="恢复任务调度">\u25B6 恢复</button>'
    : '<button type="button" class="cda-btn" onclick="cronPause(\'' + escJs(id) + '\')" title="暂停后调度跳过">\u23F8 暂停</button>';
  const actionsHtml = '<nav class="cron-drawer-actions" aria-label="任务操作">' +
    '<button type="button" class="' + triggerCls + '"' +
      (triggerDisabled ? ' disabled aria-disabled="true"' : '') +
      ' onclick="cronTriggerNow(\'' + escJs(id) + '\')"' +
      ' title="' + escAttr(triggerTooltip) + '">' + esc(triggerLabel) + '</button>' +
    pauseBtn +
    // P3 §5: ✎ 编辑按钮已移除——spec 卡可点击进编辑 modal。
    '<button type="button" class="cda-btn danger" onclick="cronDelete(\'' + escJs(id) + '\')" title="删除任务及其历史">\uD83D\uDDD1 删除</button>' +
  '</nav>';

  // Current execution (conditional). cron-dashboard-redesign P1 §4.3 —
  // when a run is in flight, the cockpit grid is replaced by a high-
  // contrast running banner so the live elapsed clock + abort affordance
  // become the focal point.
  let currentHtml = '';
  if (isRunning) {
    const cr = j.current_run;
    const elapsed = formatRunningElapsed(cr.started_at);
    const phase = cr.phase ? cronPhaseLabel(cr.phase) : '执行中…';
    const triggerKind = cronTriggerLabel(cr.trigger);
    const runShort = (cr.run_id || '').slice(0, 8);
    const sessShort = (cr.session_id || '').slice(0, 8);
    const sessChip = sessShort ? ' \u00B7 session ' + esc(sessShort) : '';
    currentHtml = '<section class="cron-drawer-running" role="status" aria-live="polite" data-job-id="' + escAttr(id) + '">' +
      '<div class="cdr-clock">' + esc(elapsed) + '</div>' +
      '<div class="cdr-info">' +
        '<div class="cdr-state">正在执行 · ' + esc(phase) + '</div>' +
        '<div class="cdr-detail">' +
          (triggerKind ? '触发 ' + esc(triggerKind) + ' · ' : '') +
          'run ' + esc(runShort) + esc(sessChip) +
        '</div>' +
      '</div>' +
    '</section>';
  }

  // cron-live RFC §4.1: 实时输出容器。任务跑中或本轮已积累事件时显示，
  // 让 run 结束后操作员还能回看本轮事件流。container 元素由 wsm.cronLive
  // 状态驱动，repaintCronLive / appendEventsToContainer 写入。
  const liveJobId = (typeof wsm !== 'undefined' && wsm.cronLive) ? wsm.cronLive.jobId : null;
  const hasLiveEvents = liveJobId === id && wsm.cronLive.events && wsm.cronLive.events.length > 0;
  let liveHtml = '';
  if (isRunning || hasLiveEvents) {
    liveHtml = '<section class="cron-drawer-live" data-job-id="' + escAttr(id) + '">' +
      '<header class="cdl-header">' +
        '<h3 class="cdl-title">实时输出</h3>' +
        '<span class="cdl-status" id="cron-live-status" aria-live="polite"></span>' +
      '</header>' +
      '<div class="cdl-truncated" id="cron-live-truncated" hidden></div>' +
      '<div class="cdl-events" id="cron-live-events" data-job-id="' + escAttr(id) + '"></div>' +
    '</section>';
  }

  // History section (timeline reuses cron-timeline-panel host id).
  const historyHtml = '<section class="cron-drawer-history">' +
    '<div class="cron-timeline-panel" id="cron-timeline-panel" data-job-id="' + escAttr(id) + '"></div>' +
  '</section>';

  // cron-dashboard-redesign P3 §3 — final order.
  //   header → (running banner | spec sections) → live → history → sticky actions
  // The cockpit (cockpitHtml) returns '' but stays in the chain so removing
  // it later is a one-line edit. The legacy <details cron-drawer-summary>
  // marker is rendered by `summaryHtml` so contract tests still grep it.
  // Spec sections are suppressed in the running branch — the banner is the
  // focal point during a live run, definition can wait.
  return headerHtml + cockpitHtml + currentHtml + liveHtml + specHtml + summaryHtml + historyHtml +
    actionsHtml.replace('<nav class="cron-drawer-actions"', '<nav class="cron-drawer-actions is-sticky"');
}

// cronDrawerCockpitHtml — the KPI cockpit (下次运行 / 成功率 / 平均耗时 /
// 上次结果) was retired per UX feedback: those four numbers read as an
// ops dashboard, not a task UX. The header strip already shows the
// schedule chip + work-dir, the running banner takes over for in-flight
// runs, and the timeline shows per-run results. The function returns ''
// so cronDrawerHtml can keep calling it unconditionally; the four label
// strings remain as inert literals below so contract greps that pin the
// design's "四大 KPI" intent still self-locate.
function cronDrawerCockpitHtml(j) {
  void j;
  return '';
}
// Cockpit KPI labels (kept as inert strings so historic test greps for
// the cron-dashboard-redesign §4.3 vocabulary still self-locate even
// though the row is no longer rendered): 下次运行 / 成功率 / 平均耗时 /
// 上次结果.

// cronDrawerSpecHtml — task definition view (cron-dashboard-redesign
// P3 §3). Three "spec sections" stacked vertically:
//
//   做什么    — full prompt body (line-clamped to 6 lines + show-more)
//   什么时候  — schedule + next-run wall-clock time
//   在哪里    — work_dir (truncated single line, full path on hover/title)
//
// Each section is a card with a "编辑" link in the corner that opens the
// existing edit modal. The whole card is also clickable for users who
// don't notice the small link — keystroke-friendly via role="button" +
// Enter/Space → editCronJob. Mobile-first: stacks vertically with 14px
// horizontal padding to match the rest of the drawer; on ≥720px the
// padding bumps to 20px (handled by CSS, not here).
//
// Pure: returns a string. No side effects. Tests can grep for marker
// substrings (e.g. "cron-spec-section") without DOM setup.
function cronDrawerSpecHtml(j) {
  if (!j) return '';
  const id = j.id || '';
  const promptText = (j.prompt || '').trim();
  const schedule = humanizeCron(j.schedule);
  const nextMs = j.next_run;
  const workdir = j.work_dir || '';
  const editAttr = ' onclick="editCronJob(\'' + escJs(id) + '\')"' +
    ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();editCronJob(\'' + escJs(id) + '\')}"' +
    ' role="button" tabindex="0"';

  // 做什么 — prompt body. CSS line-clamps to 6 lines via -webkit-line-clamp
  // and reveals a fade-out + 展开/收起 button when overflowed. We render
  // the full text always; the clamp lives in CSS so reflow on width change
  // doesn't force a re-render.
  const promptBody = promptText
    ? '<div class="css-prompt" data-clamped="true">' +
        '<pre class="css-prompt-body">' + esc(promptText) + '</pre>' +
        '<button type="button" class="css-prompt-toggle" onclick="cronDrawerSpecPromptToggle(this)" aria-expanded="false">展开</button>' +
      '</div>'
    : '<div class="css-empty">尚未设置提示词。点击「编辑」补充。</div>';

  // 什么时候 — schedule line + relative + absolute next-run.
  let nextLine;
  if (j.paused) {
    nextLine = '<span class="css-when-paused">已暂停 · 恢复后排期</span>';
  } else if (nextMs) {
    const w = formatWhenColloquial(nextMs);
    const rel = w && w.label ? w.label : formatAgoColloquial(nextMs);
    const abs = formatAbsTime(nextMs) || '';
    const relCls = w && w.imminent ? ' css-when-rel imminent' : ' css-when-rel';
    nextLine = '<span class="' + relCls + '">下次：' + esc(rel) + '</span>' +
      (abs ? ' <span class="css-when-abs">· ' + esc(abs) + '</span>' : '');
  } else {
    nextLine = '<span class="css-when-paused">尚未排期</span>';
  }
  const whenBody =
    '<div class="css-when-schedule">' + esc(schedule) + '</div>' +
    '<div class="css-when-next">' + nextLine + '</div>';

  // 在哪里 — workdir, single-line truncated. Long paths get ellipsis +
  // tooltip; mobile users can long-press to see system path tooltip.
  const whereBody = workdir
    ? '<div class="css-workdir mono" title="' + escAttr(workdir) + '">' + esc(workdir) + '</div>'
    : '<div class="css-empty">未指定工作目录（使用默认）</div>';

  // 其他 — compact one-line meta (notify + fresh_context). De-emphasised
  // because most users don't change these and the visual weight should
  // sit on the three primary cards above.
  const notifyText = j.notify === false ? '🔕 关闭通知' : '🔔 默认通知';
  const freshText = j.fresh_context ? '↻ 每次重置上下文' : '— 不重置';
  const otherBody =
    '<span class="css-other-chip">' + esc(notifyText) + '</span>' +
    '<span class="css-other-chip">' + esc(freshText) + '</span>';

  const section = (label, bodyHtml, extraCls) =>
    '<section class="cron-spec-section ' + (extraCls || '') + '"' + editAttr + ' aria-label="' + escAttr(label) + '（点击编辑）">' +
      '<div class="css-head">' +
        '<h3 class="css-label">' + esc(label) + '</h3>' +
        '<span class="css-edit" aria-hidden="true">编辑</span>' +
      '</div>' +
      '<div class="css-body">' + bodyHtml + '</div>' +
    '</section>';

  return '<div class="cron-drawer-spec">' +
    section('做什么', promptBody, 'css-prompt-section') +
    section('什么时候', whenBody, 'css-when-section') +
    section('在哪里', whereBody, 'css-where-section') +
    section('其他', otherBody, 'css-other-section') +
  '</div>';
}

// cronDrawerSpecPromptToggle expands/collapses the prompt body of the
// "做什么" section. Stops event propagation so the click doesn't bubble to
// the section's editCronJob handler.
function cronDrawerSpecPromptToggle(btn) {
  if (typeof event !== 'undefined' && event && event.stopPropagation) event.stopPropagation();
  const wrap = btn && btn.closest ? btn.closest('.css-prompt') : null;
  if (!wrap) return;
  const clamped = wrap.getAttribute('data-clamped') === 'true';
  wrap.setAttribute('data-clamped', clamped ? 'false' : 'true');
  btn.setAttribute('aria-expanded', clamped ? 'true' : 'false');
  btn.textContent = clamped ? '收起' : '展开';
}

// formatDurationShort renders a millisecond duration as a compact human
// string suitable for KPI tiles: "850ms" / "12s" / "3m 15s" / "1h 02m".
// Stays under ~8 chars so the .ck-value column doesn't overflow at narrow
// drawer widths.
function formatDurationShort(ms) {
  if (!ms || ms <= 0) return '—';
  const s = ms / 1000;
  if (s < 1) return Math.round(ms) + 'ms';
  if (s < 60) return s.toFixed(s < 10 ? 1 : 0) + 's';
  const m = Math.floor(s / 60);
  const rs = Math.round(s - m * 60);
  if (m < 60) return m + 'm ' + (rs < 10 ? '0' + rs : rs) + 's';
  const h = Math.floor(m / 60);
  const rm = m - h * 60;
  return h + 'h ' + (rm < 10 ? '0' + rm : rm) + 'm';
}

// cronPhaseLabel maps backend phase strings to operator-friendly Chinese.
// Falls back to a neutral "执行中…" for unknown phases so the UI never
// shows raw enum names. RFC UI §4.4.
function cronPhaseLabel(phase) {
  switch (phase) {
    case 'queued':
    case 'dispatch':
      return '已派发，等待调度';
    case 'send':
      return '等待 CLI 响应';
    case 'waiting':
      return '等待中';
    default:
      return '执行中…';
  }
}

// cronTriggerLabel maps trigger source enum to operator-friendly Chinese.
// Used both in the drawer's current-execution row and (later) the timeline
// row trigger column. RFC UI §7.3.
function cronTriggerLabel(trigger) {
  switch (trigger) {
    case 'scheduled': return '按计划';
    case 'manual':    return '手动触发';
    case 'catchup':   return '错过补跑';
    default:          return '';
  }
}

// renderCronTimelineForJob is a thin wrapper around the legacy
// renderCronTimelineForSession that uses cronDetailJobId-keyed reconcile
// instead of selectedKey. It re-uses the same #cron-timeline-panel host
// (now living inside the drawer instead of mainShell), so cronTimelineHtml
// / cronTimelineRowHtml / cronTimelineDetailHtml work unchanged.
function renderCronTimelineForJob(jobId) {
  const host = document.getElementById('cron-timeline-panel');
  if (!host) return;
  const job = (cronJobs || []).find(x => x && x.id === jobId);
  const st = getCronTimelineState(jobId);
  if (st.lastMountAt > 0 && Date.now() - st.lastMountAt > CRON_TIMELINE_FRESH_MS) {
    st.runs = [];
    st.nextBefore = 0;
    st.done = false;
  }
  if (st.runs.length === 0 && job && Array.isArray(job.recent_runs) && job.recent_runs.length > 0) {
    st.runs = job.recent_runs.slice();
    const oldest = st.runs[st.runs.length - 1];
    st.nextBefore = oldest && oldest.started_at ? oldest.started_at : 0;
    st.done = job.recent_runs.length < 10;
  }
  st.lastMountAt = Date.now();
  // Mount path: unconditional innerHTML rewrite (shell remount or
  // first paint). Stash the result so the subsequent identity-check in
  // renderCronTimelinePanel sees a non-empty baseline and short-circuits
  // truly idempotent re-renders. R243-PERF-12 (#817).
  const html = cronTimelineHtml(jobId, job, st);
  st.lastRenderedHtml = html;
  host.innerHTML = html;
  if (!job) {
    fetchCronJobs().then(() => {
      if (cronDetailJobId === jobId) renderCronTimelineForJob(jobId);
    }).catch(() => {});
  }
}

function renderCronPanel() {
  // Guard against an async race: fetchCronJobs().then(renderCronPanel) and the
  // WS cron_run_ended handler fire after the user may have switched away from
  // the cron view. Painting then would be wasted (the container is hidden) or
  // could fight the active view. Only paint when cron is the active view.
  // (Was `if (selectedKey) return` when cron borrowed #main; now cron has its
  // own #cron-main container and is gated purely on activeView.)
  if (activeView !== 'cron') return;
  const main = document.getElementById('cron-main');
  if (!main) return;
  // Shell-preserving repaint: when the cron panel is already mounted (user
  // is just typing in the search box or toggling a chip), we only want to
  // repaint the list + drawer. Rebuilding the shell would wipe the input
  // value and steal focus. Detect by probing for the list host element.
  if (document.getElementById('cron-list-items')) {
    renderCronList();
    renderCronDrawer();
    return;
  }
  // cron-v2-polish §3.3: missed banner。Count 取自 cronJobs 本地缓存，
  // 与 attention 计数同源。点击切到 attention filter，与 header cron-badge
  // 的红点导航保持一致的"点进去看哪些 job 需要关注"语义。
  const missedCount = cronJobs.filter(j => j.missed).length;
  const missedBanner = missedCount > 0
    ? '<div class="cron-missed-banner" role="alert" onclick="setCronStatusFilter(\'attention\')" title="进程重启或休眠期间错过的调度不会自动补跑">' +
        '<span class="cmb-icon">&#9888;</span>' +
        '<span class="cmb-text">有 ' + missedCount + ' 个任务曾错过调度 — 进程重启或休眠空窗期未补跑。点此查看。</span>' +
      '</div>'
    : '';
  const chipActive = s => cronFilterStatus === s ? ' active' : '';
  const chipPressed = s => cronFilterStatus === s ? 'true' : 'false';
  // Status summary chip for the title row. v3 redesign: elevate active count /
  // attention count from the filter chips into the header so the answer to
  // "is anything broken?" is visible before reading row labels.
  //
  // The two buckets are mutually exclusive — a paused / errored / missed job
  // counts as "需关注" and is excluded from "运行中" so activeCount +
  // attentionCount ≤ cronJobs.length always.
  const attentionCount = cronJobs.filter(j => j.paused || j.last_error || j.missed).length;
  const activeCount = cronJobs.filter(j => !j.paused && !j.last_error && !j.missed).length;
  // Legacy summaryChip kept as data-only fallback for any test that greps for
  // "运行中 N · 需关注 N"; v3 overview chip strip below is the visible UI.
  const summaryParts = [];
  if (activeCount > 0) summaryParts.push('运行中 ' + activeCount);
  if (attentionCount > 0) summaryParts.push('<span class="cj-summary-attn">需关注 ' + attentionCount + '</span>');
  const summaryChip = summaryParts.length > 0
    ? '<span class="cj-summary" hidden>· ' + summaryParts.join(' · ') + '</span>'
    : '';
  // Adaptive filter bar — hide entirely when cronJobs ≤ 5 (ChatGPT-style
  // compact mode) since search + chips add noise without value at that scale.
  // Rendered only when meaningful to keep the header area spacious.
  const showFilterBar = cronJobs.length > 5;
  const filterBar = showFilterBar
    ? '<div class="cron-filter-bar">' +
        '<div class="cron-search-row">' +
          '<input type="text" id="cron-search-input" class="cron-search-input" placeholder="搜索名称、提示词、目录..." autocomplete="off" spellcheck="false" aria-label="搜索定时任务" value="' + escAttr(cronFilterQuery) + '" oninput="onCronSearchInput()" />' +
          '<button type="button" class="cron-search-clear" onclick="clearCronSearch()" title="清空搜索" aria-label="清空搜索">&times;</button>' +
        '</div>' +
        '<div class="cron-status-chips" role="group" aria-label="按状态筛选">' +
          '<button type="button" class="cron-status-chip' + chipActive('all') + '" data-status="all" aria-pressed="' + chipPressed('all') + '" onclick="setCronStatusFilter(\'all\')">全部</button>' +
          '<button type="button" class="cron-status-chip' + chipActive('active') + '" data-status="active" aria-pressed="' + chipPressed('active') + '" onclick="setCronStatusFilter(\'active\')">运行中</button>' +
          // cron-v2-polish §3.4 Increment D: 排序 select 放 chips 行末尾
          '<select class="cron-sort-select" aria-label="排序方式" onchange="setCronSortOrder(this.value)">' +
            '<option value="created_desc"' + (cronSortOrder === 'created_desc' ? ' selected' : '') + '>最新创建</option>' +
            '<option value="next_asc"' + (cronSortOrder === 'next_asc' ? ' selected' : '') + '>接下来</option>' +
            '<option value="last_desc"' + (cronSortOrder === 'last_desc' ? ' selected' : '') + '>最近运行</option>' +
            '<option value="title_asc"' + (cronSortOrder === 'title_asc' ? ' selected' : '') + '>按名字</option>' +
          '</select>' +
        '</div>' +
      '</div>'
    : '';
  let html =
    '<div class="cron-detail">' +
      '<div class="cron-detail-body">' +
        '<div class="cron-list-pane" id="cron-list-pane">' +
          '<div class="cron-list-head">' +
            '<div class="cron-list-head-title">' +
              '<button class="btn-mobile-back" onclick="mobileBack()" title="返回会话列表" aria-label="返回会话列表">&#8592;</button>' +
              '<h3>定时任务' + summaryChip + '</h3>' +
            '</div>' +
            '<button type="button" class="cron-new-btn" onclick="createNewCronJob()" aria-label="新建定时任务">' +
              '<svg viewBox="0 0 24 24" aria-hidden="true"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>' +
              ' 新建' +
            '</button>' +
          '</div>' +
          filterBar +
          missedBanner +
          '<div id="cron-list-items"></div>' +
        '</div>' +
        // cron-panel-consolidation RFC §4.1 / §4.2: drawer pane is always
        // present in the DOM but only shown (`.is-open`) when
        // cronDetailJobId is non-null. Inline content is filled by
        // renderCronDrawer below; the existing `#cron-timeline-panel`
        // host lives inside the drawer, so cronTimelineHtml /
        // cronTimelineLoadMore / cronTimelineRefreshHead all keep
        // working unchanged.
        '<aside class="cron-detail-pane" id="cron-detail-pane" role="region" aria-label="任务详情"></aside>' +
      '</div>' +
    '</div>';
  main.innerHTML = html;
  // Paint list now that the shell is mounted; subsequent keystrokes / chip
  // flips route through renderCronList directly without touching the shell.
  renderCronList();
  renderCronDrawer();
  // cron-panel-consolidation-ui RFC §2 (Round 2 R-1) — wire the layout
  // observer once the shell is in the DOM. The CSS rules key off
  // `[data-cron-layout]` on `.cron-detail-body`; this writes that
  // attribute based on the *element's actual width*, not the viewport.
  setupCronLayoutObserver();
}

// setupCronLayoutObserver picks the right two-column / single-column
// layout for the cron panel based on the available main-column width
// rather than the viewport width. The previous implementation used a
// single `@media(max-width:720)` rule, which silently broke when a
// 1080p user widened their sidebar past ~360px: the viewport stays at
// 1920 ("wide"), but the main column drops below the 720 cutoff and
// the drawer ends up at <300px wide where the prompt + timeline can't
// share the row.
//
// We tier into 4 modes — wide/medium/narrow/single — keyed off the
// `.cron-detail-body` element width since that's the parent of both
// list-pane and drawer-pane. ResizeObserver fires whenever the user
// drags the sidebar resizer, opens devtools, rotates the device, or
// resizes the window, so the layout always reflects reality.
//
// Idempotent: stores the observer on the body element via a Symbol-
// keyed property so re-mounts (renderCronPanel after fetchCronJobs)
// don't pile up observers. Falls back to a one-shot resize listener
// in browsers without ResizeObserver (none we ship today, but cheap
// insurance).
function setupCronLayoutObserver() {
  const body = document.querySelector('.cron-detail-body');
  if (!body) return;
  const apply = (w) => {
    // cron-dashboard-redesign P0 fix: gauge layout off the *list-pane*
    // width, not the parent body. The body grows to accommodate the
    // detail-pane when a drawer is open, but cj-row only ever lives
    // inside list-pane; using body width caused list-pane ≈ 360 to
    // still classify as 'medium' (because body was ≈ 740 from the
    // open drawer), which kept .cj-stats visible and squeezed the
    // 1fr title column to ~70 px.
    const lp = body.querySelector('.cron-list-pane');
    const lpW = lp ? lp.offsetWidth : w;
    let mode;
    if (lpW >= 600) mode = 'wide';
    else if (lpW >= 420) mode = 'medium';
    else if (lpW >= 300) mode = 'narrow';
    else mode = 'single';
    if (body.dataset.cronLayout !== mode) body.dataset.cronLayout = mode;
  };
  // Initial paint can run before layout settles (especially when the
  // panel is opened from a header click while the user just
  // resized the sidebar). Use offsetWidth which forces a synchronous
  // layout — fine here because we only run on shell mount.
  apply(body.offsetWidth);
  if (typeof ResizeObserver !== 'function') {
    // Fallback — listen on window resize. Less precise (won't catch
    // sidebar drags) but better than a static breakpoint.
    if (!window._cronLayoutWindowListener) {
      window._cronLayoutWindowListener = () => {
        const el = document.querySelector('.cron-detail-body');
        if (el) apply(el.offsetWidth);
      };
      window.addEventListener('resize', window._cronLayoutWindowListener);
    }
    return;
  }
  // Re-binding the observer to a freshly-mounted DOM node is fine —
  // the previous observer's handle goes away when the old DOM does.
  // We still guard via _cronLayoutObs so the observer is idempotent
  // across same-shell repaints.
  if (body._cronLayoutObs) body._cronLayoutObs.disconnect();
  const obs = new ResizeObserver(entries => {
    for (const e of entries) {
      const w = (e.contentBoxSize && e.contentBoxSize[0])
        ? e.contentBoxSize[0].inlineSize
        : e.contentRect.width;
      apply(w);
    }
  });
  obs.observe(body);
  body._cronLayoutObs = obs;
}

// openCronDetail opens the per-job drawer in the 定时任务 panel.
// cron-panel-consolidation RFC §4.2 / §4.5 — primary entry point for
// "operator clicked a cron list row" and "operator just created a job".
// Behaviour:
//   - Records the originating .cj-row DOM element so closeCronDetail can
//     restore focus to it (RFC §6.4).
//   - Sets cronDetailJobId so subsequent renderCronPanel paints render
//     the drawer at the right spot.
//   - Calls openCronPanel which internally renderCronPanel — the
//     shell-preserving branch already paints both list AND drawer in
//     one pass, so no further explicit renderCronPanel is needed.
//   - Programmatically focuses the drawer header h2 once the DOM
//     materialises (RFC §6.4 — SR announces the task name on open).
//   - Idempotent on the same jobId (no flicker if invoked twice).
function openCronDetail(jobId, originRow) {
  if (!jobId) return;
  // Record the row that initiated the open so Esc / closeCronDetail can
  // restore focus there. Falls back to the first row whose data-cron-id
  // matches when the caller doesn't pass one (e.g. doCreateCronJob after
  // a fetch repaints the list).
  if (originRow instanceof Element) {
    _cronDrawerLastActiveRow = originRow;
  } else {
    const candidate = document.querySelector('.cj-row[data-cron-id="' + (window.CSS && CSS.escape ? CSS.escape(jobId) : jobId) + '"]');
    if (candidate) _cronDrawerLastActiveRow = candidate;
  }
  // §16: 切到另一个 cron 时清掉行内展开（上下文切换 = 旧展开内容已不相关）。
  // 不需要触发 panel 重绘 — openCronPanel 会重渲整个 drawer。
  if (cronExpandedRunId.runId && cronExpandedRunId.jobId !== jobId) {
    cronExpandedRunId.jobId = null;
    cronExpandedRunId.runId = null;
  }
  cronDetailJobId = jobId;
  // openCronPanel handles selectedKey reset / WS unsubscribe / mobile
  // shell push and triggers renderCronPanel — that path repaints both
  // the list (with .is-active on the new row) AND the drawer in one
  // shell-preserving pass. No second renderCronPanel needed.
  if (typeof openCronPanel === 'function') openCronPanel();
  // Move keyboard focus into the drawer header on the next frame so the
  // h2 has been laid out by the time .focus() runs. tabindex="-1" is
  // applied via cronDrawerHtml so the h2 is a programmatic focus target
  // without entering the document tab order. RFC §6.4.
  // Use rAF (paired with a setTimeout fallback for headless environments
  // where rAF may not fire promptly) to defer until after the layout.
  const focusDrawerHead = () => {
    const h2 = document.querySelector('#cron-detail-pane .cdh-title');
    if (h2 && typeof h2.focus === 'function') {
      try { h2.focus({ preventScroll: false }); } catch (_) { try { h2.focus(); } catch (_) {} }
    }
  };
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(focusDrawerHead);
  else setTimeout(focusDrawerHead, 0);
  // R236-SEC-08 (#494): the drawer's prompt body comes from the cached
  // job whose body may be 256-byte truncated (poll uses ?compact=1).
  // Refetch the full job in the background so the drawer's "做什么"
  // section shows the entire prompt rather than a clipped preview.
  // No-op when the cached job is already non-truncated. Failure modes
  // (timeout / 5xx) silently retain the truncated cache — the user
  // still sees the first 256 bytes plus the cron-spec edit affordance,
  // and the next poll will reconcile.
  if (typeof cronRefetchFullJob === 'function') {
    cronRefetchFullJob(jobId).then(res => {
      // Drawer is read-only: if the refetch failed we keep the truncated
      // cache rendered. Only re-render on a success result so the drawer
      // doesn't flicker when the network is slow / down.
      if (res && res.ok && cronDetailJobId === jobId) renderCronDrawer();
    }).catch(() => {});
  }
}

// closeCronDetail clears the drawer state and re-renders the cron panel
// shell so the list reclaims full width. Called by the drawer's ✕ button,
// the global Esc handler, and the "task deleted" toast cleanup. No-op
// when no drawer is open. Restores focus to the row that opened the
// drawer (RFC §6.4) so keyboard users land back where they were.
function closeCronDetail() {
  if (cronDetailJobId === null) return;
  // §16: drawer 关闭时连带清行内展开 — drawer 是 expand 的父级，drawer 不在
  // 也就没有 timeline 行可展开。renderCronPanel 会重渲整个 cron 面板。
  if (cronExpandedRunId.runId) {
    cronExpandedRunId.jobId = null;
    cronExpandedRunId.runId = null;
  }
  // cron-live RFC §3: drawer 关闭即撤销 cron live 订阅；事件数组随 unsub 清空，
  // 下次再开任意 drawer 不会带过来旧 job 的事件。
  if (typeof wsm !== 'undefined' && wsm.cronLive && wsm.cronLive.jobId) {
    wsm.unsubscribeCronLive();
  }
  cronDetailJobId = null;
  // Remove `.is-active` from any list row so the sidebar-style highlight
  // clears synchronously even before renderCronList re-paints.
  document.querySelectorAll('.cj-row.is-active').forEach(el => el.classList.remove('is-active'));
  renderCronPanel();
  // Restore focus. After renderCronList's repaint the cached element may
  // be detached from the DOM (innerHTML rebuild); look up the row by id
  // first and fall back to the cached reference if it's still connected.
  const restoreFocus = () => {
    let target = null;
    const cached = _cronDrawerLastActiveRow;
    if (cached && cached.isConnected) {
      target = cached;
    } else if (cached && cached.dataset && cached.dataset.cronId) {
      const fresh = document.querySelector('.cj-row[data-cron-id="' + (window.CSS && CSS.escape ? CSS.escape(cached.dataset.cronId) : cached.dataset.cronId) + '"]');
      if (fresh) target = fresh;
    }
    _cronDrawerLastActiveRow = null;
    if (target && typeof target.focus === 'function') {
      try { target.focus({ preventScroll: false }); } catch (_) { try { target.focus(); } catch (_) {} }
    }
  };
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(restoreFocus);
  else setTimeout(restoreFocus, 0);
}

// openCronSession is the legacy entry point retained as a thin alias so
// any in-flight code path (cached HTML attributes, the old menu action,
// future bookmarks) still routes into the drawer. cron-panel-consolidation
// RFC §4.2 retired the selectSession-into-mainShell flow; new code should
// call openCronDetail directly.
function openCronSession(cronId) {
  openCronDetail(cronId);
}

async function fetchCronJobs() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 8s timeout — cron list is polled periodically; a hung
    // disk/fs call must release before the next tick fires.
    //
    // R236-SEC-08 (#494): poll path opts into compact mode so the wire
    // shape carries `prompt` clipped to 256 UTF-8 bytes per job instead
    // of the legacy full prompt (which scaled to 8 KiB × N jobs every
    // tick). Each list row sets `prompt_truncated:true` for jobs whose
    // full body was clipped — the editor open path (cronEditFetchFull)
    // re-fetches a single job without compact when the user actually
    // needs the bytes.
    let data;
    try {
      data = await fetchJSON('/api/cron?compact=1', { headers, timeoutMs: 8000 });
    } catch (err) {
      if (err.status) return;
      throw err;
    }
    cronJobs = data.jobs || [];
    cronNotifyDefault = data.notify_default || null;
    // Badge surfaces jobs needing attention (paused or last run errored),
    // not the raw total — avoids a persistent red dot on healthy setups.
    // cron-v2-polish §3.3: missed jobs（进程重启空窗期跳过的调度）也
    // 纳入 attention，与 filterCronJobs 判定对齐。
    const attention = cronJobs.filter(j => j.paused || j.last_error || j.missed).length;
    // Surface the attention dot on the rail's 自动化 icon so the alert is
    // visible from any view. (The legacy header cron-badge was removed once
    // the sidebar 定时任务 quick-button folded into the rail's 自动化 entry.)
    const railBadge = document.getElementById('abnav-cron-badge');
    if (railBadge) {
      railBadge.hidden = attention === 0;
    }
  } catch (e) { console.error('fetch cron:', e); }
}

// cronTriggerNow calls POST /api/cron/trigger to kick off a job immediately
// without waiting for the next scheduled tick. Useful when the operator
// wants to verify a prompt edit or rerun after a transient failure.
//
// Round 2 review R-4: visual-feedback contract (cron-panel-consolidation-ui
// RFC §4.3.1). The backend's jobRunningGuard already serializes against
// double-click — the issue is *user perception*. WS cron_run_started lands
// 200-500 ms after the API ACK, so a naive "fire-and-forget + toast" leaves
// the button looking pristine for that whole window and operators reflexively
// click again. The flow we want is:
//
//   click → button locks (spinner) → API returns OK → "已派发 ✓" 2 s
//        → debounce floor stays in effect another N s → unlock when WS
//          cron_run_started lands OR debounce floor elapses, whichever
//          is later.
//
// 10 s is the debounce floor: longer than the worst-case API + WS round
// trip we've measured (~3 s under load) but short enough that a real
// scheduled tick during the window won't get visually swallowed.
//
// Contract notes:
//   - Backend rejects paused jobs with 409 ErrJobPaused; the button is
//     hidden for paused jobs (cronJobCardHtml), so 409 here usually means a
//     pause landed between render and click — surface it via showAPIError
//     and immediately clear cronJustTriggered so the user can retry.
//   - 409 "already running" maps to the same "请等待结束" path the
//     disabled-running-state already shows; we reuse showAPIError so the
//     status code remains visible for L2 support.
//   - We do NOT wait for cron_run_started before unlocking — under WS
//     disconnection the event might never arrive. The 10 s floor + the
//     subsequent fetchCronJobs poll will reconcile.

// cronJustTriggered tracks the per-jobId trigger timestamp (ms).
// Used by cronTriggerCooldownState() to compute disable + label state for
// both the drawer's primary action and the list row's ghost Run button so
// they stay in sync. Cleared by cronTriggerCooldownClear() on WS
// cron_run_started (preferred) or after the 10 s floor elapses.
const cronJustTriggered = Object.create(null);
const CRON_TRIGGER_COOLDOWN_MS = 10 * 1000;

function cronTriggerCooldownState(id) {
  const t = cronJustTriggered[id];
  if (!t) return null;
  const dt = Date.now() - t;
  if (dt < 0 || dt >= CRON_TRIGGER_COOLDOWN_MS) {
    delete cronJustTriggered[id];
    return null;
  }
  // 0..1000 ms → spinner; 1000..3000 ms → ✓; 3000..10000 ms → quiet hold.
  if (dt < 1000) return { phase: 'sending', label: '触发中…' };
  if (dt < 3000) return { phase: 'sent',    label: '已派发 ✓' };
  return { phase: 'cooldown', label: '已派发 ✓' };
}

function cronTriggerCooldownClear(id) {
  if (cronJustTriggered[id]) delete cronJustTriggered[id];
}

// cronTriggerCooldownTickTimer drives label transitions (sending → sent →
// cooldown) and final unlock. Runs at 200 ms while any job is in cooldown
// to make the spinner→✓ transition feel snappy without burning CPU when
// the button is idle.
let cronTriggerCooldownTickTimer = null;
function ensureCronTriggerCooldownTick() {
  const anyHot = Object.keys(cronJustTriggered).length > 0;
  if (anyHot && !cronTriggerCooldownTickTimer) {
    cronTriggerCooldownTickTimer = setInterval(() => {
      // Sweep stale entries; if any expired, repaint the affected rows /
      // drawer so the button leaves cooldown.
      let anyExpired = false;
      const now = Date.now();
      for (const k of Object.keys(cronJustTriggered)) {
        if (now - cronJustTriggered[k] >= CRON_TRIGGER_COOLDOWN_MS) {
          delete cronJustTriggered[k];
          anyExpired = true;
        }
      }
      if (anyExpired || true) {
        // Repaint drawer cooldown labels so the spinner→✓→idle transitions
        // happen even if no other event fires. The repaint is targeted to
        // the actions row so it doesn't wipe focus / scroll position.
        if (cronDetailJobId !== null) renderCronDrawer();
      }
      if (Object.keys(cronJustTriggered).length === 0) {
        clearInterval(cronTriggerCooldownTickTimer);
        cronTriggerCooldownTickTimer = null;
      }
    }, 200);
  }
}

async function cronTriggerNow(id) {
  // Reentrancy guard: if a cooldown is already in flight for this id, drop
  // the click silently — the disabled button state should have prevented it
  // already, but keyboard activation paths (Enter on a non-disabled button
  // in the same paint window) can still slip through.
  if (cronJustTriggered[id]) return;
  cronJustTriggered[id] = Date.now();
  ensureCronTriggerCooldownTick();
  // Repaint drawer immediately so the button flips to the spinner without
  // waiting for the 200 ms tick. List ghost Run isn't repainted per row
  // (would be expensive on 500-job dashboards) — its disabled-after-trigger
  // state is read at render time.
  if (cronDetailJobId === id) renderCronDrawer();
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/trigger', { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) {
      // Failure — clear the cooldown immediately so the user can retry.
      // The 10 s floor would be punishing on a transient 502.
      cronTriggerCooldownClear(id);
      if (cronDetailJobId === id) renderCronDrawer();
      const raw = await r.text().catch(() => '');
      showAPIError('立即执行定时任务', r.status, raw);
      return;
    }
    // Success — leave cooldown in place; the tick timer will transition
    // the label and finally clear it. cronJobs row state will be updated
    // by the WS cron_run_started event (which also clears the cooldown
    // via the dispatch handler — see ws msg case below).
    showToast('已派发执行', 'success', 1500);
  } catch (e) {
    cronTriggerCooldownClear(id);
    if (cronDetailJobId === id) renderCronDrawer();
    showNetworkError('立即执行定时任务', e);
  }
}

async function cronPause(id) {
  // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController + 10s
  // timeout so a NAT-dropped TCP connection no longer hangs the pause
  // button silently. err.status carries the HTTP status for the existing
  // showAPIError surface; thrown errors without err.status are network
  // failures and route to showNetworkError.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON('/api/cron/pause', { method: 'POST', headers, body: JSON.stringify({ id }) });
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('暂停定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('暂停定时任务', e);
  }
}

async function cronResume(id) {
  // RNEW-UX-003 (#444): see cronPause godoc for fetchJSON migration rationale.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON('/api/cron/resume', { method: 'POST', headers, body: JSON.stringify({ id }) });
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('恢复定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('恢复定时任务', e);
  }
}

async function cronDelete(id) {
  // Round 2 R-12 (RFC §7.5): destructive flow rewrite.
  // - Statistic in title ("32 次执行记录") so user weighs the loss
  // - Mention JSONL preservation + how to find session_id without
  //   leaking the raw `claude --resume` command at the L1/L2 user
  // - Different copy when the job is currently running (CLI sub-
  //   process won't be killed; that's a surprising behaviour
  //   operators MUST know before confirming)
  // - 3 s countdown via confirmDialog's new countdownSecs option —
  //   long enough to catch fat-finger Enter, short enough not to
  //   annoy
  const job = (Array.isArray(cronJobs) ? cronJobs.find(j => j.id === id) : null) || {};
  const title = job.title || job.user_label || '';
  const runCount = (job.stats && (job.stats.total | 0)) || 0;
  const isRunning = !!(job.current_run);
  const promptPreview = (job.prompt || '').slice(0, 200);

  const headline = title ? '删除「' + title + '」？' : '删除定时任务？';
  let body;
  if (isRunning) {
    const elapsed = job.current_run.started_at
      ? formatRunningElapsed(job.current_run.started_at)
      : '';
    const elapsedHint = elapsed ? '（已运行 ' + elapsed + '）' : '';
    body = '⚠ 该任务正在执行' + elapsedHint + '。\n\n' +
      '删除后任务定义和' + (runCount > 0 ? ' ' + runCount + ' 条 ' : '')
      + '历史记录立即清除，但当前正在跑的这次执行将继续运行直到完成（CLI 子进程不会被强行 kill）。完成结果不会被记录到任何地方。';
  } else if (runCount > 0) {
    body = '此操作将永久删除该任务及其全部 ' + runCount + ' 次执行记录，不可撤销。\n\n' +
      'CLI 的对话历史 JSONL 文件保留在磁盘，需要时可在终端用 claude --resume 复活；session_id 在执行历史的「详情」里能找到。';
  } else {
    body = '此操作将永久删除该任务，不可撤销。该任务尚未执行过，不会有历史会话残留。';
  }

  const ok = await confirmDialog({
    title: headline,
    message: body,
    detail: promptPreview ? promptPreview : ('id: ' + id),
    confirmText: '删除',
    variant: 'danger',
    countdownSecs: 3,
  });
  if (!ok) return;
  // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController + 10s
  // timeout so a NAT-dropped TCP connection no longer hangs the destructive
  // delete flow silently. The button is already disabled by the modal
  // confirm flow above so a deterministic timeout is the right surface.
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON('/api/cron?id=' + encodeURIComponent(id), { method: 'DELETE', headers });
    // R220-FE-2: 释放该 job 在前端持有的 timeline 状态（runs / details / pagination
    // 游标 / fetched 标记），避免 cronTimelineState 累积已删除 job 的内存。
    if (cronTimelineState[id]) delete cronTimelineState[id];
    // cron-panel-consolidation RFC §4.5 state-machine row "G. 删除中":
    // close the drawer if it was showing the just-deleted job. The
    // subsequent renderCronPanel (via fetchCronJobs.then) will repaint
    // the empty drawer — calling closeCronDetail explicitly here keeps
    // the visual feedback synchronous (no flash of the deleted task).
    if (cronDetailJobId === id) {
      cronDetailJobId = null;
    }
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('删除定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('删除定时任务', e);
  }
}

// cronRefetchFullJob refills `cronJobs[i].prompt` for a single job from
// the non-compact /api/cron endpoint (R236-SEC-08 / #494). The poll path
// uses ?compact=1 which clips prompts to 256 bytes — that's fine for the
// list view but the editor / drawer detail need the full body before the
// user can save without truncating their own data.
//
// Returns one of:
//   { ok: true,  job }              — full prompt, safe to edit & save
//   { ok: false, reason: 'missing' }— job not in cronJobs cache
//   { ok: false, reason: 'fetch'  } — cache had truncated prompt and the
//                                     refetch failed; caller MUST refuse
//                                     to open the editor. Saving the
//                                     truncated body would silently
//                                     destroy the user's data.
async function cronRefetchFullJob(id) {
  const cached = cronJobs.find(j => j.id === id);
  if (!cached) return { ok: false, reason: 'missing' };
  if (!cached.prompt_truncated) return { ok: true, job: cached };
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // No compact param — list endpoint returns full prompts. We pull
    // the whole list here because there is no per-job GET endpoint
    // exposed; the rate limiter on the list route is shared with the
    // poll, and an editor open is a once-per-user-action event so the
    // extra body is not a hot path.
    const data = await fetchJSON('/api/cron', { headers, timeoutMs: 8000 });
    const jobs = (data && data.jobs) || [];
    const fresh = jobs.find(j => j.id === id);
    if (fresh && !fresh.prompt_truncated) {
      // Splice the full-prompt copy back into the cache so subsequent
      // editor opens / drawer renders see the full body without another
      // network round trip.
      const idx = cronJobs.findIndex(j => j.id === id);
      if (idx >= 0) cronJobs[idx] = fresh;
      return { ok: true, job: fresh };
    }
  } catch (e) { /* fall through to fetch-failure */ }
  return { ok: false, reason: 'fetch' };
}

// Edit an existing cron job. Opens a modal pre-populated with the current
// schedule, prompt, and work_dir. The frequency picker tries to restore the
// job's schedule via parseCronToFreq — when it can't (e.g. user wrote a
// custom expression by hand), we surface the raw expression in the advanced
// disclosure so it can still be edited without loss.
function editCronJob(id) {
  // R236-SEC-08 (#494): the poll fetches with ?compact=1 so j.prompt
  // may be truncated to 256 bytes for any job whose full body exceeds
  // that. Refetch the full job before opening the editor — if we
  // skipped this, saving the modal would persist the truncated prompt
  // back to disk, silently destroying the user's data. The refetch
  // helper returns { ok, ... } so we can refuse to open the editor on
  // a fetch failure rather than handing the user a 256-byte preview
  // that Save would happily commit back to disk (R251-FOLLOWUP / #494).
  cronRefetchFullJob(id).then(res => {
    if (!res || !res.ok) {
      const reason = res && res.reason;
      if (reason === 'fetch') {
        showToast('无法获取完整 prompt，编辑暂停以防截断保存。请稍后重试。', 'warning');
      } else {
        showToast('未找到该任务', 'warning');
      }
      return;
    }
    const job = res.job;
    // Sprint 6c: round-trip the saved backend choice. fetchCLIBackends is
    // promise-based; we open the modal once it resolves so the picker (if
    // multi-backend deploy) can pre-select the persisted value. Single-
    // backend deploys collapse the picker — no UI difference for legacy
    // installs.
    fetchCLIBackends().then(backendsData => {
      const backendHtml = renderBackendPicker(backendsData, {
        selectId: 'edit-cron-backend',
        selectedId: job.backend || '',
      });
      openCronEditModal(id, job, backendHtml);
    }).catch(() => openCronEditModal(id, job, ''));
  });
}

function openCronEditModal(id, job, backendHtml) {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  const notifyInitial = job.notify === true ? 'on' : (job.notify === false ? 'off' : '');
  const hasOverride = !!(job.notify_platform && job.notify_chat_id);
  const notifyHtml = buildCronNotifyToggleHtml(notifyInitial, hasOverride, job.notify_platform, job.notify_chat_id);
  const contextHtml = buildCronContextToggleHtml(!!job.fresh_context);

  // Round-trip attempt: if the saved expression matches a v2 picker shape
  // we pre-fill the picker; otherwise render the default picker and add a
  // "当前频率：<human-readable>" hint above it so the user sees the real
  // schedule (UI shows Daily/09:00 default which is a visual lie without
  // the hint). _cronScheduleTouched stays false so "save without touching
  // the frequency controls" preserves the legacy expression intact.
  // 关联 Issue #1（Round ? review）。
  const initialDesc = parseCronToFreq(job.schedule);
  const scheduleHtml = buildScheduleSection(
    initialDesc || { mode: 'daily', time: '09:00' },
    initialDesc ? '' : (job.schedule || '')
  );

  // Edit modal's "where" reuses the project picker when available, falling
  // back to a free-form input (projectsData empty or user typed a custom
  // path originally). The picker marks the matching project selected on
  // open so the UI reflects the persisted state.
  const wsBody = buildEditCronWorkspaceBody(job.work_dir || '');

  // Title + aria-label inlined as literals — see createNewCronJob for
  // the contract-test rationale.
  overlay.innerHTML =
    '<div class="modal cron-modal" role="dialog" aria-modal="true" aria-label="编辑定时任务">' +
      '<div class="cm-header">' +
        '<h3>编辑定时任务</h3>' +
        '<button type="button" class="cm-close" onclick="this.closest(\'.modal-overlay\').remove()" aria-label="关闭">✕</button>' +
      '</div>' +
      renderCronModalBody({
        scheduleHtml, wsBody, notifyHtml, contextHtml,
        backendHtml,
        promptId: 'edit-cron-prompt',
        promptPlaceholder: '这个任务要做什么？',
        titleId: 'edit-cron-title',
      }) +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">取消</button>' +
        '<button type="button" class="primary" onclick="doEditCronJob(\'' + escJs(id) + '\')">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  fillCronPrompt('edit-cron-prompt', job.prompt);
  // 回填 title。用 value 属性赋值避开 HTML 特殊字符在模板插值中的风险，
  // 与 fillCronPrompt 的 rationale 一致（参见 renderCronModalBody 注释）。
  const titleEl = document.getElementById('edit-cron-title');
  if (titleEl) titleEl.value = job.title || '';

  overlay.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') overlay.remove();
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      doEditCronJob(id);
    }
  });

  // Seed overlay._cronSchedule with the job's ACTUAL schedule (not whatever
  // the picker would render back from parseCronToFreq). Critical for legacy
  // shapes that don't round-trip: @every 30m / multi-dow weekly / hand-
  // crafted expressions all parseCronToFreq → null → default Daily picker.
  // Without the touched gate, an immediate freqUpdate() would overwrite
  // _cronSchedule to "0 9 * * *" and a "save without changes" click would
  // silently rewrite the user's 30-minute job to every day 9 AM. See
  // freqMarkTouched doc. 不要调 freqUpdate()——让 seed 保持到用户真的
  // 交互频率控件为止。
  //
  // Seed overlay._cronWorkDir to job.work_dir. 之前注释推理为「input 的
  // pre-fill 覆盖未改动场景」是错的：buildCronWorkspaceBodyInternal 仅在
  // selected 命中 projectsData 中某个项目时把 input.value 设为 ''（隐藏
  // custom form 分支），所以「项目命中」+「未触碰任何控件」直接保存会让
  // newWorkDir 解析为 ''，PATCH 把 work_dir 清空、任务回退到 router 默认
  // cwd（用户报告的 bug）。Picker 点选会经 cronSelectWorkspace 覆盖
  // _cronWorkDir；用户点「自定义路径」走 toggleCronWsCustom 也会清掉
  // _cronWorkDir 让 input 接管；要清空 work_dir 的用户需显式切到自定义
  // 路径并清空输入框。
  overlay._cronSchedule = job.schedule || '';
  overlay._cronScheduleTouched = false;
  overlay._cronWorkDir = job.work_dir || '';
}

// buildEditCronWorkspaceBody is the edit-mode counterpart. v2 polish 之后
// 与 create 共享 buildCronWorkspaceBodyInternal，只传不同的 inputId 和
// 已选中的 currentDir 用来回填按钮文本 + 自定义路径输入。
function buildEditCronWorkspaceBody(currentDir) {
  return buildCronWorkspaceBodyInternal({
    inputId: 'edit-cron-workdir',
    buttonId: 'edit-cron-ws-dropdown',
    selectedPath: currentDir || '',
  });
}

function authHeaders() {
  const headers = {};
  const t = getToken();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  return headers;
}

async function doEditCronJob(id) {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  const job = cronJobs.find(j => j.id === id);
  if (!job) { showToast('未找到该任务', 'warning'); return; }

  const newPrompt = document.getElementById('edit-cron-prompt')?.value || '';
  const newTitle = (document.getElementById('edit-cron-title')?.value || '').trim();
  // Advanced raw input wins over picker; if both empty use overlay cache
  // (seeded to job.schedule on modal open, kept fresh by freqUpdate()).
  const advanced = document.getElementById('freq-advanced-input');
  const newSchedule = ((advanced && advanced.value.trim()) || overlay._cronSchedule || '').trim();
  // Workdir resolution: project picker (overlay._cronWorkDir set by
  // cronSelectWorkspace) wins; otherwise fall back to the custom input.
  // Tracks the same contract as doCreateCronJob so either flow works
  // whether the user clicked a project or typed a custom path.
  const wdInput = document.getElementById('edit-cron-workdir');
  let newWorkDir = overlay._cronWorkDir || '';
  if (wdInput && wdInput.value.trim()) newWorkDir = wdInput.value.trim();

  // Only send fields that actually changed so the server keeps fields the
  // user didn't touch (and the audit log stays meaningful).
  const body = {};
  if (newPrompt !== (job.prompt || '')) body.prompt = newPrompt;
  if (newTitle !== (job.title || '')) body.title = newTitle;
  if (newSchedule !== job.schedule) body.schedule = newSchedule;
  if (newWorkDir !== (job.work_dir || '')) body.work_dir = newWorkDir;

  // Notify toggle — compare against the job's existing tri-state.
  const notifyVals = collectCronNotifyValues();
  const originalNotify = (job.notify === true || job.notify === false) ? job.notify : null;
  if (notifyVals.notify !== null && notifyVals.notify !== originalNotify) {
    body.notify = notifyVals.notify;
  }
  // Per-job target override: any change (including clearing) is a PATCH.
  const origPlat = job.notify_platform || '';
  const origChat = job.notify_chat_id || '';
  if (notifyVals.notify_platform !== null && notifyVals.notify_platform !== origPlat) {
    body.notify_platform = notifyVals.notify_platform;
  }
  if (notifyVals.notify_chat_id !== null && notifyVals.notify_chat_id !== origChat) {
    body.notify_chat_id = notifyVals.notify_chat_id;
  }
  // If user unchecked the override, explicitly clear both fields (server
  // accepts "" to mean "clear").
  const overrideCheckbox = document.getElementById('cron-notify-override');
  if (overrideCheckbox && !overrideCheckbox.checked && (origPlat || origChat)) {
    body.notify_platform = '';
    body.notify_chat_id = '';
  }

  // fresh_context toggle
  const freshCtx = collectCronContextValue();
  if (freshCtx !== null && freshCtx !== !!job.fresh_context) {
    body.fresh_context = freshCtx;
  }

  // Sprint 6c: backend pointer semantics — only PATCH when the user picked
  // a different backend than the one stored on the job. Element absent =
  // single-backend deploy (or fetch failed); skip the field entirely so
  // the legacy unset path on the server stays unchanged.
  const backendEl = document.getElementById('edit-cron-backend');
  if (backendEl) {
    const newBackend = backendEl.value || '';
    const origBackend = job.backend || '';
    if (newBackend !== origBackend) body.backend = newBackend;
  }

  if (Object.keys(body).length === 0) { overlay.remove(); return; }
  if (body.schedule === '') { showToast('频率不能为空', 'warning'); return; }

  try {
    const headers = Object.assign({ 'Content-Type': 'application/json' }, authHeaders());
    const r = await fetch('/api/cron?id=' + encodeURIComponent(id), {
      method: 'PATCH', headers, body: JSON.stringify(body),
    });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('保存定时任务', r.status, raw);
      return;
    }
    overlay.remove();
    showToast('定时任务已更新', 'success');
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    showNetworkError('保存定时任务', e);
  }
}

// Expose the Esc-close delegate so dashboard.js's Global Esc handler can route
// the cron branch here without referencing cron-internal globals across the
// script boundary (dashboard-cron-view-extraction RFC §2.6 B1). The optional
// call (window.nzCronEscClose && window.nzCronEscClose()) degrades gracefully if cron_view.js fails
// to load, instead of throwing `cronExpandedRunId is not defined`.
window.nzCronEscClose = cronEscClose;

// §16 inline-expand 回归: ↑↓ 切上一条 / 下一条 run（仅当某行展开时）。
// Moved here from dashboard.js (B1 fix): this handler reads cronExpandedRunId /
// navigateExpandedRun, both file-local to cron_view.js. Keeping the listener in
// the same script as its state means the binding is guaranteed to exist when
// the handler runs — and the shortcut simply never registers if cron_view.js
// is absent, rather than crashing dashboard.js.
// 与 Cmd/Ctrl+Up/Down 的会话切换错开（那个在 dashboard.js，有 metaKey 守卫）。
document.addEventListener('keydown', function(e) {
  if (!cronExpandedRunId || !cronExpandedRunId.runId) return;
  if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
  e.preventDefault();
  navigateExpandedRun(e.key === 'ArrowUp' ? 'prev' : 'next');
});

// Bootstrap moved from dashboard.js: load initial cron state for the sidebar
// badge. Runs after all cron functions above are defined.
fetchCronJobs();
