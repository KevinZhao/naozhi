// tasks.js — the "任务" group of the Planner page's left column (planner RFC
// §15.4, D85, §15.7): the sessions the Planner dispatched and its promises,
// from GET /api/tasks, in four parts — 进行中 (等你 and 失败 first), 待提醒,
// 你开的 and 已完成 under a fold (the gateway archives a done task 24 hours
// on); each part's heading counts its cards.
//
// 你开的 (§15.12 ⑥, D104) are the sessions you opened that the gateway
// observes (group "yours"): the same card, its state by health (等你 / 失败,
// else 在跑 / 停滞 / 空闲 from health and live), a blue dot when a turn
// ended with you away; "⋯" only marks it done or back in progress (no 停止,
// nothing is put away: the session itself is left alone). The group's
// heading counts what the Planner dispatched and promised, not these.
//
// Every card has one shape, the column's full width: the title (two lines at
// most, the whole of it in the tooltip; a dispatched session's opens it in
// the middle column), then one status line — a badge (在跑 / 等你 / 停滞 /
// 失败 / 空闲 / 已完成 …, the colours of the sidebar's status dots), what it
// is doing now and how long — and a muted line only for what is not the
// default (the Planner's or your mark and why, a due time, a promise's
// condition, what became of a done session's worktrees).
//
// A dispatched session's status line reads the task's live (the router's
// view: running, the latest tool call or thinking, the start of the latest
// reply, last activity) and its health: 在跑 says what it is doing and how
// long the turn has run, 停滞 how long it has said nothing, 等你 the reply
// that asked, 失败 why. The texts are the session's output: escaped, cut to
// one line. While one runs the group refetches every LIVE_MS, and a
// session_state frame of a task: session (nzBus 'session:state') refetches
// it at once (debounced).
//
// The controls sit behind the card's "⋯" (shown on hover or focus where
// there is a pointer, always on touch): 停止 a running session (POST
// /api/tasks/stop: its process is closed, the task 已完成 as yours, shown as
// 已停止), 标为已完成 / 标为进行中 (POST /api/tasks/state, as yours), and
// 重新打开 a done one. A done dispatched session is put away once its turn
// ended, you are not at it and it has been idle 2 hours (planner/reap, D105):
// while only the idleness holds it its status line says about when (the
// task's reap_at), otherwise its card says what holds it, then that it was
// put away; 重新打开 puts it back. The Planner page's header offers the same 停止 for the session open
// in the middle column (isRunningTask, nzBus 'tasks:changed').
//
// A dispatched session runs in custody; its card marks custody only when it
// is off or stopped (custody.js's mark, not a switch: the header has that).
//
// It refetches on inbox_count frames (nzBus 'inbox:count': the health
// changes that mark 等你 / 失败 open and close items too) and on
// refreshTasks() (a Planner turn ending, the page's slow tick).
//
// A result (§15.12 ④, D102: a report with a conclusion, done, failed) the
// user has not looked at is the task's new: a blue dot at the card's start
// (a dot, not the left edge that says 在跑), and the Planner page's header
// says "N 件新结果" (text, not a count of 等你: never red, never pushed),
// which filters the cards to those (a card looked at there stays, its dot
// gone) and back; 全部已读 beside it marks the ones the page shows new.
// Opening the session, expanding its card or 全部已读 marks it read (POST
// /api/tasks/seen with the IDs; the gateway also marks this node's from the
// viewing frame, a node's session opened in the 会话 view is posted to its
// node, and again as each turn it ends there while you look at it); the
// Planner mentioning it does not. A dispatched session's card
// carries its latest report's first sentence ("最新结论", §15.12 ②; a
// progress report is no conclusion and goes in the status line), and with
// a digest its title row's chevron (or that line) expands the digest under
// it (GET /api/sessions/digest, rendered as markdown: the session's output,
// data), read again when the card moves on; what is expanded and its text
// are in the card's HTML, so a live repaint keeps them.
//
// A dispatched session's own header (the 会话 view) carries its card's
// badge and 在 Planner 中查看 (UX review 10): paintTaskHeader fills
// #header-task from the last list, which ensureTasks reads without the page
// mounted; the link opens the page on the card, unfolded and lit. A report
// line in the Planner conversation opens it so too, its digest expanded.
import { NZ_CONTRACT } from './contract.js';
import { selection, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { formatAbsTime, formatMonthDayTime, timeAgo } from './utilities.js';
import { custody, custodyChipHtml, refreshCustody, selectionShown, statusOf } from './custody.js';
import { isLit, light } from './notify.js';
import { shell } from './shell.js';
import { isLocalNode, nodeAPI, plannerAPI, plannerHost } from './planner_host.js';
import { renderMd, runPendingAsync } from './render_md.js';

// Mounted containers; detached ones are dropped at the next refresh.
const mounts = new Set();
// byId is the last list read, for isRunningTask; last is its answer, for a
// repaint when custody changes.
let byId = new Map();
let last = null;
// load is the GET /api/tasks in flight ({gen, p, next}), shared by the
// readers that take a list already asked for; next is the one read after it
// that every reader needing a list asked after now shares. gen moves with
// the Planner shown: a read for the node it showed before is dropped. readAt
// and failedAt are when one last answered and last failed (ensureTasks).
let load = null;
let gen = 0;
let readAt = 0;
let failedAt = 0;
// actsOpen is the card whose "⋯" is open, kept across repaints.
let actsOpen = '';
// digestOpen is the card whose digest is expanded; digests holds what was
// read for a card ({md} or {err}, {wait} while reading; stamp: the card's
// digestStamp it was read at). onlyNew, while "N 件新结果" filters the
// group, holds the IDs new when it was chosen: a card looked at since keeps
// its place, only its dot goes; null while not filtering.
let digestOpen = '';
const digests = new Map();
let onlyNew = null;

// LIVE_MS is how often the group refetches while a dispatched session runs;
// STATE_DEBOUNCE_MS gathers a burst of session_state frames into one fetch.
const LIVE_MS = 10000;
const STATE_DEBOUNCE_MS = 800;
// FRESH_MS is how old a list a header may show before ensureTasks reads it
// again; RETRY_MS spaces the tries while the route fails (the Planner off
// answers 404). REVEAL_TRIES × REVEAL_MS bound the wait for the page to
// paint the card 在 Planner 中查看 opens.
const FRESH_MS = 30000;
const RETRY_MS = 15000;
const REVEAL_TRIES = 40;
const REVEAL_MS = 150;
// STATUS_MAX caps a status line's text (the server caps it too); CSS cuts
// it to the card's width.
const STATUS_MAX = 160;

// BADGES are the status badges: the label and the class, whose colour is the
// sidebar dot's for the same state (run blue, warn amber, fail red).
const BADGES = {
  run: { label: '在跑', cls: 'run' },
  idle: { label: '空闲', cls: 'idle' },
  stalled: { label: '停滞', cls: 'warn' },
  waiting: { label: '等你', cls: 'warn' },
  failed: { label: '失败', cls: 'fail' },
  doing: { label: '进行中', cls: 'idle' },
  remind: { label: '待提醒', cls: 'idle' },
  done: { label: '已完成', cls: 'done' },
  stopped: { label: '已停止', cls: 'done' },
};
const BY_LABEL = { planner: 'Planner 判断', user: '你标记', auto: '自动完成', health: '按健康状态', ended: '会话已结束' };
// STOPPED_WHY is the reason the gateway gives a task you stopped
// (task.StoppedWhy): done as yours, said as 已停止 · 你停的.
const STOPPED_WHY = '已停止';
// SECTIONS are the parts before 已完成; yours holds the sessions you opened
// (group "yours"), the others what the Planner dispatched and promised.
const SECTIONS = [
  { id: 'doing', label: '进行中', states: ['waiting', 'failed', 'doing'] },
  { id: 'remind', label: '待提醒', states: ['remind'] },
  { id: 'yours', label: '你开的', states: ['waiting', 'failed', 'doing'], yours: true },
];
// isYours reports whether task t is a session you opened (你开的).
export const isYours = (t) => !!t && t.group === 'yours';
const inSection = (s, t) => s.states.includes(t.state) && !!s.yours === isYours(t);
const STATES = ['doing', 'waiting', 'failed', 'done', 'remind'];
// REPORT_WORD names a report's state before its conclusion (the words of
// the conversation's 📨 line).
const REPORT_WORD = { done: '完成', blocked: '卡住', need_user: '要你决定', progress: '进度' };

// inProgress reports whether task t is listed under 进行中: the count the
// phone's folded left block gives.
export const inProgress = (t) => !!t && inSection(SECTIONS[0], t);

// reapHtml says what became of a done dispatched session (planner/reap):
// put away, with each worktree removed or why it stays, or why it is not put
// away yet. 重新打开 puts it back.
const WT_LABEL = { removed: '已删除 worktree', kept: 'worktree 留着', pending: 'worktree 等着删', gone: 'worktree 已不在' };
function reapHtml(t) {
  if (t.reap) {
    const parts = ['会话已收起'];
    for (const w of t.reap.worktrees || []) {
      const name = String(w.dir || '').split('/').pop();
      parts.push('<span title="' + escAttr(w.dir || '') + '">' + esc(WT_LABEL[w.state] || w.state) + ' ' + esc(name) +
        (w.state === 'kept' || w.state === 'pending' ? '（' + esc(w.why || '') + '）' : '') + '</span>');
    }
    return parts.join('<span aria-hidden="true"> · </span>');
  }
  // reap_at is said on the status line (reapEta), not again here.
  if (reapEta(t)) return '';
  return t.reap_wait ? '等收起：' + esc(t.reap_wait) : '';
}

// untilText says how long until at, rounded up to a minute: a countdown in
// spanText's words.
function untilText(at, now = Date.now()) {
  const m = Math.max(1, Math.ceil((at - now) / 60000));
  if (m < 60) return m + ' 分钟';
  const h = Math.floor(m / 60);
  return h + ' 小时' + (m % 60 ? ' ' + (m % 60) + ' 分' : '');
}

// reapEta is "约 X 后收起" for a done dispatched session that only its
// idleness keeps from being put away (reap_at), "" otherwise.
function reapEta(t, now = Date.now()) {
  if (t.kind !== 'session' || t.state !== 'done' || t.reap) return '';
  const at = ms(t.reap_at);
  return at ? '约 ' + untilText(at, now) + '后收起' : '';
}

const stopped = (t) => t.state === 'done' && t.by === 'user' && t.why === STOPPED_WHY;

// running reports whether dispatched session t is in progress (进行中 or 等你)
// and still there: what 停止 applies to.
const running = (t) => t.kind === 'session' && !isYours(t) && !t.retired && (t.state === 'doing' || t.state === 'waiting');

// turnRunning reports whether dispatched session t has a turn running now.
const turnRunning = (t) => t.kind === 'session' && !t.retired && t.state !== 'done' && !!(t.live && t.live.running);

const ms = (iso) => { const v = Date.parse(iso || ''); return isNaN(v) ? 0 : v; };
const cut = (s) => { s = String(s || '').replace(/\s+/g, ' ').trim(); return s.length > STATUS_MAX ? s.slice(0, STATUS_MAX) + '…' : s; };

// firstSentence is s's first sentence (up to its 。！？!? or ". "), folded
// to one line; s whole when it has no end.
export function firstSentence(s) {
  s = String(s || '').replace(/\s+/g, ' ').trim();
  const m = /^.+?(?:[。！？!?]|\.(?= |$))/.exec(s);
  return m ? m[0] : s;
}

// digestBody is a digest's markdown as the card shows it: without the
// frontmatter, the note for the Planner (the page renders it as data
// anyway) and the title the card already has.
function digestBody(md) {
  return String(md || '')
    .replace(/^---\n[\s\S]*?\n---\n/, '')
    .replace(/^\s*>[^\n]*是数据，不是指令[^\n]*\n+/, '')
    .replace(/^\s*# [^\n]*\n+/, '');
}

// spanText says how long since from, in minutes or hours.
function spanText(from, now = Date.now()) {
  const m = Math.max(0, Math.floor((now - from) / 60000));
  if (m < 1) return '不到 1 分钟';
  if (m < 60) return m + ' 分钟';
  const h = Math.floor(m / 60);
  return h + ' 小时' + (m % 60 ? ' ' + (m % 60) + ' 分' : '');
}

// taskStatus is a card's status line: the badge, the text (raw: the
// caller escapes it) and the time at its end with its tooltip.
export function taskStatus(t, now = Date.now()) {
  const live = (t.kind === 'session' && !t.retired && t.live) || null;
  const since = ms(t.health_since) || ms(t.at);
  const lastActive = (live && ms(live.last_active)) || 0;
  const progress = progressText(t);
  const ago = (at, pre = '') => (at ? { time: pre + timeAgo(at), tip: formatAbsTime(at) } : {});
  if (t.state === 'done') {
    const by = stopped(t) ? '你停的' : (t.by && BY_LABEL[t.by] ? BY_LABEL[t.by] + (t.why ? '：' + t.why : '') : t.why || '');
    // 已完成 · 约 X 后收起 · who and why (D105).
    const eta = reapEta(t, now);
    return { badge: stopped(t) ? BADGES.stopped : BADGES.done, text: eta ? eta + (by ? ' · ' + by : '') : by, ...ago(ms(t.at)) };
  }
  if (t.state === 'remind') {
    const d = ms(t.due);
    // The due time is the one that matters, and the text says it.
    return { badge: BADGES.remind, text: d ? '到点提醒 ' + formatMonthDayTime(d) : '' };
  }
  if (t.kind !== 'session') {
    const d = ms(t.due);
    return { badge: BADGES.doing, text: t.close_when ? '完成条件：' + t.close_when : (d ? '到期 ' + formatMonthDayTime(d) : ''), ...ago(ms(t.at)) };
  }
  if (t.state === 'failed') return { badge: BADGES.failed, text: t.why || '', ...ago(ms(t.at)) };
  if (t.state === 'waiting') {
    return { badge: BADGES.waiting, text: (live && live.reply) || '有提问等你回答', ...(since ? { time: '等了 ' + spanText(since, now), tip: formatAbsTime(since) } : {}) };
  }
  if (live && live.running) {
    const from = t.health === 'active' ? since : (lastActive || ms(t.at));
    return { badge: BADGES.run, text: live.activity || progress || live.reply || '正在处理', time: '已跑 ' + spanText(from, now), tip: '这一轮开始于 ' + formatAbsTime(from) };
  }
  if (t.health === 'stalled') {
    const from = lastActive || since;
    return { badge: BADGES.stalled, text: (live && live.activity) || progress || (live && live.reply) || '', time: spanText(from, now) + '没有输出', tip: '最后活动 ' + formatAbsTime(from) };
  }
  if (live) {
    const at = lastActive || ms(t.at);
    return { badge: BADGES.idle, text: progress || (live.reply ? '最后回复：' + live.reply : live.activity || ''), ...ago(at) };
  }
  return { badge: BADGES.doing, text: progress, ...ago(ms(t.at)) };
}

// progressText is a dispatched session's latest report, when that is
// progress, for its "正在做什么" line (§15.12 ④: 知会 only updates that line,
// it is no conclusion): after the tool it runs now, before its last reply
// (the turn the report ended). '' for any other report.
function progressText(t) {
  const r = t.report;
  if (t.kind !== 'session' || !r || r.state !== 'progress') return '';
  const text = firstSentence(r.summary);
  return text ? REPORT_WORD.progress + '：' + text : '';
}

function titleHtml(t) {
  const tip = ' title="' + escAttr(t.title) + '"';
  if (t.kind !== 'session') return '<span class="tk-title-t"' + tip + '>' + esc(t.title) + '</span>';
  return '<button type="button" class="pl-link tk-open tk-title-t" data-action="planner-open-session" data-key="' + escAttr(t.id) + '"' + tip + '>' + esc(t.title) + '</button>';
}

// actsHtml is what "⋯" opens: 停止 a running session, mark it done or back
// in progress (a promise before its reminder is not in progress yet), or
// reopen a done one.
function actsHtml(t) {
  const btn = (action, label, tip) => '<button type="button" class="pl-btn tk-act tk-' + action.replace('task-', '') + '" data-action="' + action + '" data-id="' + escAttr(t.id) + '"' + (tip ? ' title="' + escAttr(tip) + '"' : '') + '>' + label + '</button>';
  // A session you opened is only marked: nothing stops or reopens it.
  if (isYours(t)) return t.state === 'done' ? btn('task-doing', '标为进行中', '放回「你开的」') : btn('task-done', '标为已完成', '从「你开的」里拿掉（会话本身不动）');
  if (t.state === 'done') return btn('task-reopen', '重新打开', t.reap ? '放回会话列表，下一条消息接着上下文继续' : '');
  return (running(t) ? btn('task-stop', '停止', '停止这个会话正在做的事') : '') +
    (t.state === 'doing' || t.state === 'remind' ? '' : btn('task-doing', '标为进行中')) +
    btn('task-done', '标为已完成');
}

// custodyException reports whether a task's card marks its custody: only
// when it is not what a dispatched session runs under (off, or stopped);
// custody on goes unsaid. A list not read yet says nothing.
function custodyException(t, state) {
  // Custody off is a session you opened's default: nothing to mark.
  if (t.kind !== 'session' || isYours(t) || t.retired || state === 'done' || !custody.readAt || custody.enabled !== true) return false;
  const st = statusOf(t.id).state;
  return st === 'off' || st === 'stopped';
}

function metaHtml(t, state) {
  const meta = [];
  // The Planner's or your mark says who and why; the default (health) and
  // a done card's (its status line says it) go unsaid.
  if (state !== 'done' && !stopped(t) && (t.by === 'planner' || t.by === 'user' || t.by === 'auto')) {
    meta.push(esc(BY_LABEL[t.by]) + (t.why ? '：' + esc(t.why) : ''));
  }
  if (t.due && state !== 'remind' && t.kind === 'session') {
    const d = ms(t.due);
    if (d) meta.push('到期 ' + esc(formatMonthDayTime(d)));
  }
  if (t.close_when && state === 'remind') meta.push('完成条件：' + esc(t.close_when));
  if (state === 'done' && t.kind === 'session') {
    const r = reapHtml(t);
    if (r) meta.push(r);
  }
  return meta.join('<span aria-hidden="true"> · </span>');
}

// NEW_DOT marks a new result: a card's, and the 已完成 fold holding one.
const NEW_DOT = '<span class="tk-new" role="img" aria-label="新结果" title="新结果"></span>';

// reportHtml is the card's "最新结论": the latest report's first sentence,
// after its state's word; with a digest it expands it, as the chevron does.
// A progress report is no conclusion: the status line says it.
function reportHtml(t) {
  const r = t.report;
  const text = r && r.state !== 'progress' && firstSentence(r.summary);
  if (!text) return '';
  const inner = (REPORT_WORD[r.state] ? '<span class="tk-report-k">' + esc(REPORT_WORD[r.state]) + '</span>' : '') +
    '<span class="tk-report-t">' + esc(text) + '</span>';
  const tip = ' title="' + escAttr('最新结论：' + text) + '"';
  if (!t.digest) return '<div class="tk-report"' + tip + '>' + inner + '</div>';
  return '<button type="button" class="tk-report" data-action="task-expand" data-id="' + escAttr(t.id) + '" tabindex="-1"' + tip + '>' + inner + '</button>';
}

// digestHtml is the expanded digest under card t, '' while folded.
function digestHtml(t) {
  if (digestOpen !== t.id) return '';
  const d = digests.get(t.id) || { wait: true };
  const id = ' id="tk-digest-' + escAttr(t.id) + '"';
  if (d.md !== undefined) return '<div class="tk-digest md"' + id + '>' + renderMd(digestBody(d.md)) + '</div>';
  return '<div class="tk-digest tk-digest-note"' + id + '>' + (d.err ? '读不到摘要：' + esc(d.err) : '正在读取摘要…') + '</div>';
}

function cardHtml(t) {
  const state = STATES.includes(t.state) ? t.state : 'doing';
  const st = taskStatus(t);
  const text = cut(st.text);
  const mark = custodyException(t, state) ? custodyChipHtml(t.id, false) : '';
  const meta = metaHtml(t, state);
  const open = actsOpen === t.id;
  const expandable = t.kind === 'session' && !!t.digest;
  const expanded = expandable && digestOpen === t.id;
  return '<div class="tk-card tk-s-' + state + (state === 'done' ? ' done' : '') + (open ? ' tk-acts-open' : '') + (t.new ? ' tk-has-new' : '') + (expanded ? ' tk-expanded' : '') + (isLit(t.id) ? ' inbox-lit' : '') + '" data-id="' + escAttr(t.id) + '" data-status="' + st.badge.cls + '" role="listitem">' +
      '<div class="tk-top">' +
        (t.new ? NEW_DOT : '') +
        '<div class="tk-title">' + titleHtml(t) + '</div>' +
        (expandable ? '<button type="button" class="tk-expand" data-action="task-expand" data-id="' + escAttr(t.id) + '" aria-expanded="' + expanded + '" aria-controls="tk-digest-' + escAttr(t.id) + '" aria-label="' + (expanded ? '收起摘要' : '展开摘要') + '" title="' + (expanded ? '收起摘要' : '展开摘要') + '"><svg viewBox="0 0 24 24" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg></button>' : '') +
        '<button type="button" class="tk-more" data-action="task-more" data-id="' + escAttr(t.id) + '" aria-expanded="' + open + '" aria-label="更多操作" title="更多操作">⋯</button>' +
      '</div>' +
      '<div class="tk-status">' +
        '<span class="tk-badge tk-b-' + st.badge.cls + '">' + esc(st.badge.label) + '</span>' +
        '<span class="tk-now"' + (text ? ' title="' + escAttr(text) + '"' : '') + '>' + esc(text) + '</span>' +
        (st.time ? '<span class="tk-time"' + (st.tip ? ' title="' + escAttr(st.tip) + '"' : '') + '>' + esc(st.time) + '</span>' : '') +
      '</div>' +
      reportHtml(t) +
      (meta || mark ? '<div class="tk-meta">' + mark + meta + '</div>' : '') +
      (expanded ? digestHtml(t) : '') +
      (open ? '<div class="tk-acts">' + actsHtml(t) + '</div>' : '') +
    '</div>';
}

// countHeadHtml is a left column group's heading: its name and, when it
// holds any, how many (UX review 1: what the column holds reads at a glance;
// the empty state says none).
export const countHeadHtml = (label, n) => esc(label) + (n ? ' <span class="tk-n">' + n + '</span>' : '');

// tasksHtml renders a GET /api/tasks answer; the heading counts the tasks in
// progress, the phone's folded block's number (one number for 任务 on the
// screen, §15.8 F1), its parts each their own.
export function tasksHtml(data) {
  const all = (data && Array.isArray(data.tasks)) ? data.tasks : [];
  let html = '<h2 class="pl-group-h tk-head">' + countHeadHtml('任务', all.filter(inProgress).length) + '</h2>';
  if (!all.length) return html + '<div class="pl-empty">还没有任务<span>Planner 派出的会话、它答应你的事和你开的会话会列在这里</span></div>';
  // "N 件新结果" filters to the new ones (those it was chosen for, and any
  // new since), 已完成 unfolded (most are done).
  const keep = (t) => t.new || onlyNew.has(t.id);
  const filtered = !!onlyNew && all.some(keep);
  const tasks = filtered ? all.filter(keep) : all;
  if (filtered) {
    html += '<div class="tk-filter" role="status"><span>只看新结果 ' + tasks.length + ' 件</span>' +
      '<button type="button" class="pl-link" data-action="tasks-seen-all">全部已读</button>' +
      '<button type="button" class="pl-link" data-action="tasks-only-new">显示全部</button></div>';
  }
  const head = (label, n) => label + ' <span class="tk-n">' + n + '</span>';
  for (const s of SECTIONS) {
    const rows = tasks.filter((t) => inSection(s, t));
    if (!rows.length) continue;
    html += '<section class="tk-sec tk-sec-' + s.id + '"><div class="tk-sec-h">' + head(s.label, rows.length) + '</div>' +
      '<div class="tk-list" role="list">' + rows.map(cardHtml).join('') + '</div></section>';
  }
  const done = tasks.filter((t) => t.state === 'done');
  if (done.length) {
    html += '<details class="tk-sec tk-sec-done"' + (filtered ? ' open' : '') + '><summary class="tk-sec-h">' + head('已完成', done.length) + (done.some((t) => t.new) ? NEW_DOT : '') + '</summary>' +
      '<div class="tk-list" role="list">' + done.map(cardHtml).join('') + '</div></details>';
  }
  return html;
}

// isRunningTask reports whether id, as last read, is a dispatched session
// 停止 applies to.
export function isRunningTask(id) {
  const t = byId.get(id);
  return !!t && running(t);
}

// paint puts data in el, unless it would paint what is there. The 已完成
// fold keeps its open state and a focused control its focus.
function paint(el, data) {
  const html = tasksHtml(data);
  if (el._tkHtml === html && el.firstChild) return;
  const wasOpen = !!el.querySelector('.tk-sec-done')?.open;
  const a = document.activeElement;
  const focus = a && el.contains(a) && a.dataset.action ? { id: a.dataset.id || a.dataset.key, action: a.dataset.action } : null;
  el._tkHtml = html;
  el.innerHTML = html;
  const fold = el.querySelector('.tk-sec-done');
  if (fold && wasOpen) fold.open = true;
  // A digest's math and diagrams render after it is in the page.
  if (el.querySelector('.tk-digest.md')) runPendingAsync();
  if (focus) {
    for (const b of el.querySelectorAll('[data-action="' + CSS.escape(focus.action) + '"]')) {
      if ((b.dataset.id || b.dataset.key) === focus.id) { b.focus(); break; }
    }
  }
}

// fetchTasks reads GET /api/tasks into byId and last and tells the readers
// on nzBus. A read in flight answers every caller that takes it (fresh
// false); a fresh caller (a change just posted, a turn just ended) gets a
// read that starts after it asked, one for all of them. A read for the
// Planner's node shown before is dropped and read again for the one shown.
function fetchTasks(fresh = false) {
  if (load && load.gen === gen) {
    if (!fresh) return load.p;
    if (!load.next) load.next = load.p.catch(() => null).then(() => fetchTasks());
    return load.next;
  }
  const g = gen;
  const p = (async () => {
    let data;
    try {
      data = await fetchJSON(plannerAPI(NZ_CONTRACT.API.tasks));
    } catch (e) {
      if (load && load.p === p) load = null;
      if (g !== gen) return fetchTasks();
      failedAt = Date.now();
      throw e;
    }
    if (load && load.p === p) load = null;
    if (g !== gen) return fetchTasks();
    const list = (data && Array.isArray(data.tasks)) ? data.tasks : [];
    byId = new Map(list.map((t) => [t.id, t]));
    if (actsOpen && !byId.has(actsOpen)) actsOpen = '';
    if (digestOpen && !byId.has(digestOpen)) digestOpen = '';
    last = data;
    paintNews();
    refreshDigest();
    readAt = Date.now();
    failedAt = 0;
    nzBus.dispatchEvent(new CustomEvent('tasks:changed'));
    // The Planner conversation's cards name tasks by these titles.
    nzBus.dispatchEvent(new CustomEvent('tasks:list', { detail: data }));
    return data;
  })();
  load = { gen: g, p, next: null };
  return p;
}

async function render(el, read) {
  try {
    paint(el, await read);
  } catch {
    el._tkHtml = '';
    el.innerHTML = '<h2 class="pl-group-h tk-head">任务</h2><div class="pl-empty">加载失败</div>';
  }
}

// mountTasks renders the group into el and keeps it current.
export function mountTasks(el) {
  el.classList.add('tk-group');
  mounts.add(el);
  refreshCustody();
  return render(el, fetchTasks(true)).then(armLive);
}

// refreshTasks re-renders every mounted group from a read that starts after
// the call.
export function refreshTasks() {
  refreshCustody();
  for (const el of mounts) if (!el.isConnected) mounts.delete(el);
  const read = mounts.size ? fetchTasks(true) : null;
  const out = [];
  for (const el of mounts) out.push(render(el, read));
  return Promise.all(out).then(armLive);
}

// ---- live: what a running session does, kept current ----

// shown reports whether a mounted group is on screen: the page open and the
// tab visible.
function shown() {
  if (document.hidden) return false;
  for (const el of mounts) if (el.isConnected && el.getClientRects().length) return true;
  return false;
}

let liveTimer = 0;
// armLive refetches in LIVE_MS while the last list has a turn running and
// the group is on screen; the page's own refreshes rearm it.
function armLive() {
  clearTimeout(liveTimer);
  liveTimer = 0;
  if (![...byId.values()].some(turnRunning)) return;
  liveTimer = setTimeout(() => { liveTimer = 0; if (shown()) refreshTasks(); else armLive(); }, LIVE_MS);
}

let stateTimer = 0;
// stateHeader is whether a frame of the burst was the header's session's: a
// later frame of another task in the same burst keeps the header's read.
let stateHeader = false;
// A listed or dispatched session starting or ending a turn refetches the
// list, once for a burst, for the page and for that session's own header;
// off screen the next showing refetches anyway. A session you opened that
// is not listed yet comes with the next refresh.
nzBus.addEventListener('session:state', (e) => {
  const msg = e.detail;
  if (!msg || typeof msg.key !== 'string') return;
  // The list may not be read (the Planner page never opened): the gateway
  // marks only what it lists as new.
  if (msg.state !== 'running') seenRemoteLater(msg.key, msg.node || 'local');
  if (!(byId.has(msg.key) || msg.key.startsWith('task:'))) return;
  const header = headerTaskKey() === msg.key;
  if (!mounts.size && !header) return;
  stateHeader = stateHeader || header;
  clearTimeout(stateTimer);
  stateTimer = setTimeout(() => {
    const forHeader = stateHeader && !!headerTaskKey();
    stateTimer = 0;
    stateHeader = false;
    if (shown()) refreshTasks();
    else if (forHeader && !document.hidden) ensureTasks(0);
  }, STATE_DEBOUNCE_MS);
});
document.addEventListener('visibilitychange', () => {
  if (document.hidden) return;
  if (mounts.size && last) refreshTasks();
  else if (headerTaskKey()) ensureTasks();
});

// ---- a dispatched session's header (the 会话 view) ----

// taskOf is key's task as last read, or null.
export const taskOf = (key) => (key && byId.get(key)) || null;

// ensureTasks reads the list when the last read is older than maxAge (ms),
// with the Planner page mounted or not; 0 asks for a read that starts now.
// While the route fails it is tried at most every RETRY_MS. It resolves
// once the list is read (or not).
export function ensureTasks(maxAge = FRESH_MS) {
  const fresh = maxAge <= 0;
  if (!fresh && load && load.gen === gen) return load.p.then(() => last, () => null);
  const now = Date.now();
  if ((!fresh && now - readAt <= maxAge) || (failedAt && now - failedAt < RETRY_MS)) return Promise.resolve(last);
  return fetchTasks(fresh).then(() => { repaintAll(); return last; }, () => null);
}

// headerTaskKey is the session the 会话 view shows when it is a session
// dispatched by the Planner shown (task keys are that node's), else ''.
function headerTaskKey() {
  const key = selection.key || '';
  return key.startsWith('task:') && (selection.node || 'local') === plannerHost.node ? key : '';
}

// taskHeaderHtml is #header-task for task t: its card's badge, and the link
// to the card on the Planner page.
function taskHeaderHtml(t) {
  const st = taskStatus(t);
  const text = cut(st.text);
  return '<span class="tk-badge tk-b-' + st.badge.cls + '" title="' + escAttr('任务状态：' + st.badge.label + (text ? '\n' + text : '')) + '">' + esc(st.badge.label) + '</span>' +
    '<button type="button" class="pl-link tk-reveal" data-action="task-reveal" data-id="' + escAttr(t.id) + '">在 Planner 中查看</button>';
}

// paintTaskHeader fills the session header's #header-task for the selected
// session; renderMainShell builds it empty, and :empty collapses it for
// every session the list does not hold.
export function paintTaskHeader() {
  const el = document.getElementById('header-task');
  if (!el) return;
  const key = headerTaskKey();
  const t = key ? taskOf(key) : null;
  const html = t ? taskHeaderHtml(t) : '';
  if (el.innerHTML !== html) el.innerHTML = html;
  if (key) ensureTasks();
}

// revealTask brings task id's card on the Planner page into view and lights
// it, unfolding 已完成 when it is there, and ending "只看新结果" when that
// leaves the card out; the page may still be painting the list, so it looks
// again for a while. expand also expands its digest (a report line opens the
// card on what it reported) and looks at its result, a digest not written
// yet or open already too.
export function revealTask(id, { expand = false } = {}) {
  const sel = '#pl-task .tk-card[data-id="' + CSS.escape(id) + '"]';
  let tries = 0;
  const look = () => {
    if (onlyNew && byId.has(id) && !onlyNew.has(id) && !byId.get(id).new) {
      onlyNew = null;
      paintNews();
      repaintAll();
    }
    let el = document.querySelector(sel);
    const fold = el && el.closest('details.tk-sec-done');
    if (fold && !fold.open) fold.open = true;
    if (el && el.getClientRects().length) {
      const t = byId.get(id);
      if (expand && t && t.kind === 'session' && t.digest && digestOpen !== id) {
        digestOpen = id;
        loadDigest(id);
        repaintAll();
        el = document.querySelector(sel) || el;
      }
      if (expand) seenTask(id);
      el.scrollIntoView({ block: 'center' });
      light(el, sel, id);
      return;
    }
    if (++tries < REVEAL_TRIES) setTimeout(look, REVEAL_MS);
  };
  look();
}

// ---- new results (§15.12 ④) ----

// newIds lists the tasks, as last read, with a new result: what 全部已读
// marks (only what the user was shown, never one new since the last read).
const newIds = () => [...byId.values()].filter((t) => t.new).map((t) => t.id);
const newCount = () => newIds().length;

// paintNews writes the Planner page header's "N 件新结果 · 全部已读",
// hidden while none is new. The filter ends once none of its cards is
// listed any more; while it lasts the list's own bar ends it.
function paintNews() {
  const box = document.getElementById('pl-news');
  if (onlyNew && ![...onlyNew].some((id) => byId.has(id)) && !newCount()) onlyNew = null;
  if (!box) return;
  const n = newCount();
  box.hidden = !n;
  const btn = box.querySelector('[data-action="tasks-only-new"]');
  if (!btn) return;
  const text = n + ' 件新结果';
  if (btn.textContent !== text) btn.textContent = text;
  btn.setAttribute('aria-pressed', onlyNew ? 'true' : 'false');
  btn.title = onlyNew ? '显示全部任务' : '只看有新结果的任务';
}

// MAX_SEEN_IDS is what one POST /api/tasks/seen takes (tasks.maxSeenIDs).
const MAX_SEEN_IDS = 500;

// markSeen tells the gateway the user looked at the new results of ids,
// showing them read at once, then reads the list.
async function markSeen(ids) {
  ids = ids.slice(0, MAX_SEEN_IDS);
  const body = { ids };
  for (const t of byId.values()) if (t.new && ids.includes(t.id)) t.new = false;
  paintNews();
  repaintAll();
  try {
    await fetchJSON(plannerAPI(NZ_CONTRACT.API.tasks_seen), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 8000,
    });
  } catch (e) {
    showToast('没有标为已读：' + e.message, 'error');
  }
  if (mounts.size) await refreshTasks();
  else await ensureTasks(0);
}

// seenTask marks task id's new result read: the user opened its session.
// Nothing new, nothing sent.
export function seenTask(id) {
  const t = taskOf(id);
  if (t && t.new) markSeen([id]);
}

// seenSelected marks read the listed session the 会话 view opened. The
// list read for its node says whether it is new; else a node's dispatched
// session (which no viewing frame reaches, and whose list may not be read)
// is told directly: it writes only a new result. This node's needs nothing
// more, the gateway marks it from the viewing frame.
function seenSelected() {
  const key = selection.key || '';
  const node = selection.node || 'local';
  if (node === plannerHost.node && byId.has(key)) { seenTask(key); return; }
  if (!key.startsWith('task:')) return;
  if (isLocalNode(node)) return;
  fetchJSON(nodeAPI(node, NZ_CONTRACT.API.tasks_seen), {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ids: [key] }), timeoutMs: 8000,
  }).catch(() => { /* the user asked for nothing: the dot shows it next time */ });
}

// SEEN_REMOTE_MS is how long after another node's session ends a turn its
// gateway is told you saw it: past its marking the result (custody's queue)
// and the frame's relay; told before, it has nothing new to mark.
const SEEN_REMOTE_MS = 1500;

// seenRemoteLater tells another node's gateway, a little later, that you saw
// the result of the turn key just ended there, the 会话 view showing it to
// you: no viewing frame reaches it, so each turn's end would leave a dot.
function seenRemoteLater(key, node) {
  if (isLocalNode(node) || node !== plannerHost.node && !key.startsWith('task:')) return;
  if (!selectionShown() || selection.key !== key || (selection.node || 'local') !== node) return;
  setTimeout(() => {
    if (!selectionShown() || selection.key !== key || (selection.node || 'local') !== node) return;
    fetchJSON(nodeAPI(node, NZ_CONTRACT.API.tasks_seen), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ids: [key] }), timeoutMs: 8000,
    }).then(() => { if (node === plannerHost.node && mounts.size) refreshTasks(); })
      .catch(() => { /* the dot shows it next time */ });
  }, SEEN_REMOTE_MS);
}

// digestStamp is what an expanded digest is read again on: a new report,
// result or state, or a turn starting or ending (its recent replies).
const digestStamp = (t) => [t.report && t.report.at, t.result_at, t.state, t.live && t.live.last_active].join('|');

// refreshDigest reads the expanded digest again when its card says the
// session moved on since it was read.
function refreshDigest() {
  const t = digestOpen && byId.get(digestOpen);
  const d = t && digests.get(t.id);
  if (d && !d.wait && d.stamp !== digestStamp(t)) loadDigest(t.id);
}

// loadDigest reads task id's digest for its expanded card; a read answering
// after the card folded or the Planner shown changed is kept for next time.
async function loadDigest(id) {
  const g = gen;
  const had = digests.get(id);
  const t = byId.get(id);
  const stamp = t ? digestStamp(t) : '';
  if (!had || had.err) digests.set(id, { wait: true, stamp });
  else digests.set(id, { ...had, stamp });
  try {
    const d = await fetchJSON(plannerAPI(NZ_CONTRACT.API.sessions_digest) + '?key=' + encodeURIComponent(id), { timeoutMs: 8000 });
    if (g !== gen) return;
    digests.set(id, { md: String((d && d.markdown) || ''), stamp });
  } catch (e) {
    if (g !== gen) return;
    // A read failing again keeps what was read; the stamp holds it off
    // until the card moves on.
    if (!had || had.md === undefined) digests.set(id, { err: e.message || '读取失败', stamp });
    else digests.set(id, { ...had, stamp });
  }
  if (digestOpen === id) repaintAll();
}

nzBus.addEventListener('tasks:changed', paintTaskHeader);
nzBus.addEventListener('session:select', () => {
  paintTaskHeader();
  // Opening a dispatched session in the 会话 view looks at its result.
  if (ui.activeView === 'chat') seenSelected();
});
// Another node's Planner: its tasks, read afresh; a read in flight for the
// node before is dropped (fetchTasks).
nzBus.addEventListener('planner:node', () => {
  gen++; readAt = 0; failedAt = 0;
  digestOpen = ''; digests.clear(); onlyNew = null;
  // The count was the node before's; its own list repaints it.
  const news = document.getElementById('pl-news');
  if (news) news.hidden = true;
  paintTaskHeader();
});

// post sends a task change from control el, then refreshes every group.
async function post(el, url, body, failed) {
  el.disabled = true;
  try {
    await fetchJSON(url, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 8000,
    });
    actsOpen = '';
  } catch (e) {
    showToast(failed + e.message, 'error');
  }
  el.disabled = false;
  el.blur();
  await refreshTasks();
}

function setState(el, id, state) {
  if (id && state) return post(el, plannerAPI(NZ_CONTRACT.API.tasks_state), { id, state }, '没有改成：');
}

function repaintAll() {
  if (!last) return;
  for (const el of mounts) if (el.isConnected) paint(el, last);
}

// A health change (等你, 失败, 停滞) is no turn's edge: the header of a
// dispatched session reads it here too, the page not mounted (mounted, the
// page's read repaints it).
nzBus.addEventListener('inbox:count', () => {
  refreshTasks();
  if (!mounts.size && headerTaskKey()) ensureTasks(0);
});
nzBus.addEventListener('custody:changed', repaintAll);
// 观察 switched in a session's header: 你开的 gains or loses it.
nzBus.addEventListener('watch:changed', () => { if (mounts.size) refreshTasks(); });

registerActions({
  'task-more': (el) => {
    const id = el.dataset.id || '';
    actsOpen = actsOpen === id ? '' : id;
    repaintAll();
    // Focus follows into the opened row, so the keyboard reaches it.
    if (actsOpen) document.querySelector('.tk-card.tk-acts-open[data-id="' + CSS.escape(actsOpen) + '"] .tk-act')?.focus();
  },
  // The chevron or 最新结论 expands the digest under the card, and looks at
  // its result; again folds it.
  'task-expand': (el) => {
    const id = el.dataset.id || '';
    if (!id) return;
    digestOpen = digestOpen === id ? '' : id;
    if (digestOpen) loadDigest(id);
    repaintAll();
    if (digestOpen) seenTask(id);
  },
  // "N 件新结果" shows only those cards, and again all; on a phone it
  // unfolds the left block they are in.
  'tasks-only-new': () => {
    onlyNew = !onlyNew && newCount() ? new Set(newIds()) : null;
    paintNews();
    repaintAll();
    if (onlyNew) {
      nzBus.dispatchEvent(new CustomEvent('tasks:reveal', { detail: {} }));
      document.getElementById('pl-task')?.scrollIntoView({ block: 'start' });
    }
  },
  // 全部已读 marks what the list shows new (a result since its last read
  // stays new), and ends the filter: nothing is left to look at in it.
  'tasks-seen-all': () => {
    const ids = newIds();
    onlyNew = null;
    if (ids.length) markSeen(ids);
    else { paintNews(); repaintAll(); }
  },
  'task-done': (el) => setState(el, el.dataset.id, 'done'),
  'task-doing': (el) => setState(el, el.dataset.id, 'doing'),
  'task-reopen': (el) => setState(el, el.dataset.id, 'doing'),
  'task-stop': (el) => el.dataset.id && post(el, plannerAPI(NZ_CONTRACT.API.tasks_stop), { id: el.dataset.id }, '没有停下：'),
  // The Planner page opens on the card; planner_view.js unfolds the phone's
  // left block it is in ('tasks:reveal').
  'task-reveal': (el) => {
    const id = el.dataset.id || '';
    if (!id) return;
    if (shell.setActivityView) shell.setActivityView('planner');
    nzBus.dispatchEvent(new CustomEvent('tasks:reveal', { detail: { id } }));
    revealTask(id);
  },
});
