// tasks.js — the "任务" group of the Planner page's left column (planner RFC
// §15.4, D85, §15.7): the sessions the Planner dispatched and its promises,
// from GET /api/tasks, in three parts — 进行中 (等你 and 失败 first), 待提醒,
// and 已完成 under a fold (the gateway archives a done task 24 hours on); each
// part's heading counts its cards.
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
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { formatAbsTime, formatMonthDayTime, timeAgo } from './utilities.js';
import { custody, custodyChipHtml, refreshCustody, statusOf } from './custody.js';
import { plannerAPI } from './planner_host.js';

// Mounted containers; detached ones are dropped at the next refresh.
const mounts = new Set();
// byId is the last list read, for isRunningTask; last is its answer, for a
// repaint when custody changes.
let byId = new Map();
let last = null;
// actsOpen is the card whose "⋯" is open, kept across repaints.
let actsOpen = '';

// LIVE_MS is how often the group refetches while a dispatched session runs;
// STATE_DEBOUNCE_MS gathers a burst of session_state frames into one fetch.
const LIVE_MS = 10000;
const STATE_DEBOUNCE_MS = 800;
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
const SECTIONS = [
  { id: 'doing', label: '进行中', states: ['waiting', 'failed', 'doing'] },
  { id: 'remind', label: '待提醒', states: ['remind'] },
];
const STATES = ['doing', 'waiting', 'failed', 'done', 'remind'];

// inProgress reports whether task t is listed under 进行中: the count the
// phone's folded left block gives.
export const inProgress = (t) => !!t && SECTIONS[0].states.includes(t.state);

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
const running = (t) => t.kind === 'session' && !t.retired && (t.state === 'doing' || t.state === 'waiting');

// turnRunning reports whether dispatched session t has a turn running now.
const turnRunning = (t) => t.kind === 'session' && !t.retired && t.state !== 'done' && !!(t.live && t.live.running);

const ms = (iso) => { const v = Date.parse(iso || ''); return isNaN(v) ? 0 : v; };
const cut = (s) => { s = String(s || '').replace(/\s+/g, ' ').trim(); return s.length > STATUS_MAX ? s.slice(0, STATUS_MAX) + '…' : s; };

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
    return { badge: BADGES.run, text: live.activity || live.reply || '正在处理', time: '已跑 ' + spanText(from, now), tip: '这一轮开始于 ' + formatAbsTime(from) };
  }
  if (t.health === 'stalled') {
    const from = lastActive || since;
    return { badge: BADGES.stalled, text: (live && (live.activity || live.reply)) || '', time: spanText(from, now) + '没有输出', tip: '最后活动 ' + formatAbsTime(from) };
  }
  if (live) {
    const at = lastActive || ms(t.at);
    return { badge: BADGES.idle, text: live.reply ? '最后回复：' + live.reply : live.activity || '', ...ago(at) };
  }
  return { badge: BADGES.doing, text: '', ...ago(ms(t.at)) };
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
  if (t.state === 'done') return btn('task-reopen', '重新打开', t.reap ? '放回会话列表，下一条消息接着上下文继续' : '');
  return (running(t) ? btn('task-stop', '停止', '停止这个会话正在做的事') : '') +
    (t.state === 'doing' || t.state === 'remind' ? '' : btn('task-doing', '标为进行中')) +
    btn('task-done', '标为已完成');
}

// custodyException reports whether a task's card marks its custody: only
// when it is not what a dispatched session runs under (off, or stopped);
// custody on goes unsaid. A list not read yet says nothing.
function custodyException(t, state) {
  if (t.kind !== 'session' || t.retired || state === 'done' || !custody.readAt || custody.enabled !== true) return false;
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

function cardHtml(t) {
  const state = STATES.includes(t.state) ? t.state : 'doing';
  const st = taskStatus(t);
  const text = cut(st.text);
  const mark = custodyException(t, state) ? custodyChipHtml(t.id, false) : '';
  const meta = metaHtml(t, state);
  const open = actsOpen === t.id;
  return '<div class="tk-card tk-s-' + state + (state === 'done' ? ' done' : '') + (open ? ' tk-acts-open' : '') + '" data-id="' + escAttr(t.id) + '" data-status="' + st.badge.cls + '" role="listitem">' +
      '<div class="tk-top">' +
        '<div class="tk-title">' + titleHtml(t) + '</div>' +
        '<button type="button" class="tk-more" data-action="task-more" data-id="' + escAttr(t.id) + '" aria-expanded="' + open + '" aria-label="更多操作" title="更多操作">⋯</button>' +
      '</div>' +
      '<div class="tk-status">' +
        '<span class="tk-badge tk-b-' + st.badge.cls + '">' + esc(st.badge.label) + '</span>' +
        '<span class="tk-now"' + (text ? ' title="' + escAttr(text) + '"' : '') + '>' + esc(text) + '</span>' +
        (st.time ? '<span class="tk-time"' + (st.tip ? ' title="' + escAttr(st.tip) + '"' : '') + '>' + esc(st.time) + '</span>' : '') +
      '</div>' +
      (meta || mark ? '<div class="tk-meta">' + mark + meta + '</div>' : '') +
      (open ? '<div class="tk-acts">' + actsHtml(t) + '</div>' : '') +
    '</div>';
}

// tasksHtml renders a GET /api/tasks answer.
export function tasksHtml(data) {
  const tasks = (data && Array.isArray(data.tasks)) ? data.tasks : [];
  let html = '<h2 class="pl-group-h tk-head">任务</h2>';
  if (!tasks.length) return html + '<div class="pl-empty">还没有任务<span>Planner 派出的会话和它答应你的事会列在这里</span></div>';
  const head = (label, n) => label + ' <span class="tk-n">' + n + '</span>';
  for (const s of SECTIONS) {
    const rows = tasks.filter((t) => s.states.includes(t.state));
    if (!rows.length) continue;
    html += '<section class="tk-sec tk-sec-' + s.id + '"><div class="tk-sec-h">' + head(s.label, rows.length) + '</div>' +
      '<div class="tk-list" role="list">' + rows.map(cardHtml).join('') + '</div></section>';
  }
  const done = tasks.filter((t) => t.state === 'done');
  if (done.length) {
    html += '<details class="tk-sec tk-sec-done"><summary class="tk-sec-h">' + head('已完成', done.length) + '</summary>' +
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
  if (focus) {
    for (const b of el.querySelectorAll('[data-action="' + CSS.escape(focus.action) + '"]')) {
      if ((b.dataset.id || b.dataset.key) === focus.id) { b.focus(); break; }
    }
  }
}

async function render(el) {
  try {
    const data = await fetchJSON(plannerAPI(NZ_CONTRACT.API.tasks));
    const list = (data && Array.isArray(data.tasks)) ? data.tasks : [];
    byId = new Map(list.map((t) => [t.id, t]));
    if (actsOpen && !byId.has(actsOpen)) actsOpen = '';
    last = data;
    nzBus.dispatchEvent(new CustomEvent('tasks:changed'));
    // The Planner conversation's cards name tasks by these titles.
    nzBus.dispatchEvent(new CustomEvent('tasks:list', { detail: data }));
    paint(el, data);
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
  return render(el).then(armLive);
}

// refreshTasks re-renders every mounted group.
export function refreshTasks() {
  refreshCustody();
  const out = [];
  for (const el of mounts) {
    if (!el.isConnected) { mounts.delete(el); continue; }
    out.push(render(el));
  }
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
// A dispatched session starting or ending a turn refetches the list, once
// for a burst; off screen the next showing refetches anyway.
nzBus.addEventListener('session:state', (e) => {
  const msg = e.detail;
  if (!msg || typeof msg.key !== 'string' || !msg.key.startsWith('task:') || !mounts.size) return;
  clearTimeout(stateTimer);
  stateTimer = setTimeout(() => { stateTimer = 0; if (shown()) refreshTasks(); }, STATE_DEBOUNCE_MS);
});
document.addEventListener('visibilitychange', () => { if (!document.hidden && mounts.size && last) refreshTasks(); });

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

nzBus.addEventListener('inbox:count', () => { refreshTasks(); });
nzBus.addEventListener('custody:changed', repaintAll);

registerActions({
  'task-more': (el) => {
    const id = el.dataset.id || '';
    actsOpen = actsOpen === id ? '' : id;
    repaintAll();
    // Focus follows into the opened row, so the keyboard reaches it.
    if (actsOpen) document.querySelector('.tk-card.tk-acts-open[data-id="' + CSS.escape(actsOpen) + '"] .tk-act')?.focus();
  },
  'task-done': (el) => setState(el, el.dataset.id, 'done'),
  'task-doing': (el) => setState(el, el.dataset.id, 'doing'),
  'task-reopen': (el) => setState(el, el.dataset.id, 'doing'),
  'task-stop': (el) => el.dataset.id && post(el, plannerAPI(NZ_CONTRACT.API.tasks_stop), { id: el.dataset.id }, '没有停下：'),
});
