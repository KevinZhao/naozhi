// tasks.js — the "任务" group of the Planner page's left column and of the
// 会话 sidebar's bar (planner RFC §15.4, D85): the sessions the Planner
// dispatched and its promises, from GET /api/tasks, in three parts — 进行中
// (等你 and 失败 first), 待提醒, and 已完成 under a fold (the gateway archives
// a done task 24 hours on). A card shows its title, where it comes from, its
// state, who set it and why, and when; its state menu (进行中 / 已完成) posts
// /api/tasks/state as yours, and a done one reopens. A dispatched session
// runs as soon as the Planner dispatches it (D88), so its card has 停止
// (POST /api/tasks/stop: its process is closed, the task 失败 as yours);
// its title opens it: in the Planner page's middle column, or in the chat
// view from the sidebar (mountTasks(el, { open: 'chat' })).
//
// It refetches on inbox_count frames (nzBus 'inbox:count': the health
// changes that mark 等你 / 失败 open and close items too) and on
// refreshTasks() (a Planner turn ending, the page's slow tick).
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { agoText } from './planner_cards.js';
import { formatAbsTime } from './utilities.js';

// Mounted containers → their open target; detached ones are dropped at the
// next refresh.
const mounts = new Map();

const STATE_LABEL = { doing: '进行中', waiting: '等你', failed: '失败', done: '已完成', remind: '待提醒' };
const KIND_LABEL = { session: '派出的会话', promise: '承诺' };
const BY_LABEL = { planner: 'Planner 判断', user: '你标记', auto: '自动完成', health: '按健康状态', ended: '会话已结束' };
const SECTIONS = [
  { id: 'doing', label: '进行中', states: ['waiting', 'failed', 'doing'] },
  { id: 'remind', label: '待提醒', states: ['remind'] },
];

const pad = (n) => (n < 10 ? '0' + n : '' + n);
function mdhm(d) { return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()); }

function titleHtml(t, open) {
  if (t.kind !== 'session') return esc(t.title);
  const action = open === 'chat' ? 'session-select" data-node="local' : 'planner-open-session';
  return '<button type="button" class="pl-link tk-open" data-action="' + action + '" data-key="' + escAttr(t.id) + '" title="打开这个会话">' + esc(t.title) + '</button>';
}

// menuHtml is the state menu: the current state, then 进行中 and 已完成 (a
// promise before its reminder is not in progress yet); a done task reopens
// instead.
function menuHtml(t) {
  if (t.state === 'done') {
    return '<button type="button" class="pl-btn tk-reopen" data-action="task-reopen" data-id="' + escAttr(t.id) + '">重新打开</button>';
  }
  const cur = STATE_LABEL[t.state] || t.state;
  return '<select class="tk-menu" data-action-change="task-state" data-id="' + escAttr(t.id) + '" aria-label="改状态">' +
    (t.state === 'doing' ? '' : '<option value="" selected disabled>' + esc(cur) + '</option>') +
    (t.state === 'remind' ? '' : '<option value="doing"' + (t.state === 'doing' ? ' selected' : '') + '>进行中</option>') +
    '<option value="done">已完成</option></select>';
}

// stopHtml is a dispatched session's 停止, while it is in progress (进行中 or
// 等你) and still there.
function stopHtml(t) {
  if (t.kind !== 'session' || t.retired || (t.state !== 'doing' && t.state !== 'waiting')) return '';
  return '<button type="button" class="pl-btn tk-stop" data-action="task-stop" data-id="' + escAttr(t.id) + '" title="停止这个会话正在做的事">停止</button>';
}

function cardHtml(t, open) {
  const state = STATE_LABEL[t.state] ? t.state : 'doing';
  const at = Date.parse(t.at);
  const meta = [];
  if (t.by && BY_LABEL[t.by]) meta.push(esc(BY_LABEL[t.by]) + (t.why ? '：' + esc(t.why) : ''));
  if (t.due) {
    const d = new Date(t.due);
    if (!isNaN(d.getTime())) meta.push((state === 'remind' ? '到点提醒 ' : '到期 ') + esc(mdhm(d)));
  }
  if (t.close_when) meta.push('完成条件：' + esc(t.close_when));
  return '<div class="tk-card tk-s-' + state + (state === 'done' ? ' done' : '') + '" data-id="' + escAttr(t.id) + '" role="listitem">' +
      '<div class="tk-top">' +
        '<span class="tk-state">' + esc(STATE_LABEL[state]) + '</span>' +
        '<span class="tk-src">' + esc(KIND_LABEL[t.kind] || '') + '</span>' +
        (!isNaN(at) ? '<span class="tk-time" title="' + escAttr(formatAbsTime(at)) + '">' + esc(agoText(at)) + '</span>' : '') +
      '</div>' +
      '<div class="tk-title">' + titleHtml(t, open) + '</div>' +
      '<div class="tk-foot"><div class="tk-meta">' + meta.join('<span aria-hidden="true"> · </span>') + '</div>' + stopHtml(t) + menuHtml(t) + '</div>' +
    '</div>';
}

// tasksHtml renders a GET /api/tasks answer for a group whose session
// titles open in open ('planner' or 'chat').
export function tasksHtml(data, open = 'planner') {
  const tasks = (data && Array.isArray(data.tasks)) ? data.tasks : [];
  const done = tasks.filter((t) => t.state === 'done');
  let html = '<h2 class="pl-group-h tk-head">任务 · ' + (tasks.length - done.length) + '</h2>';
  if (!tasks.length) return html + '<div class="pl-empty">还没有任务<span>Planner 派出的会话和它答应你的事会列在这里</span></div>';
  for (const s of SECTIONS) {
    const rows = tasks.filter((t) => s.states.includes(t.state));
    if (!rows.length) continue;
    html += '<section class="tk-sec tk-sec-' + s.id + '"><div class="tk-sec-h">' + s.label + ' · ' + rows.length + '</div>' +
      '<div class="tk-list" role="list">' + rows.map((t) => cardHtml(t, open)).join('') + '</div></section>';
  }
  if (done.length) {
    html += '<details class="tk-sec tk-sec-done"><summary class="tk-sec-h">已完成 · ' + done.length + '</summary>' +
      '<div class="tk-list" role="list">' + done.map((t) => cardHtml(t, open)).join('') + '</div></details>';
  }
  return html;
}

async function render(el, open) {
  try {
    const data = await fetchJSON(NZ_CONTRACT.API.tasks);
    // The 已完成 fold keeps its open state across refreshes, and a menu in
    // use is not repainted under the pointer.
    if (el.contains(document.activeElement) && document.activeElement.matches('.tk-menu')) return;
    const wasOpen = !!el.querySelector('.tk-sec-done')?.open;
    el.innerHTML = tasksHtml(data, open);
    const fold = el.querySelector('.tk-sec-done');
    if (fold && wasOpen) fold.open = true;
  } catch {
    el.innerHTML = '<h2 class="pl-group-h tk-head">任务</h2><div class="pl-empty">加载失败</div>';
  }
}

// mountTasks renders the group into el and keeps it current; opts.open is
// where a dispatched session opens ('planner', the default, or 'chat').
export function mountTasks(el, opts) {
  const open = (opts && opts.open) || 'planner';
  el.classList.add('tk-group');
  mounts.set(el, open);
  return render(el, open);
}

// refreshTasks re-renders every mounted group.
export function refreshTasks() {
  const out = [];
  for (const [el, open] of mounts) {
    if (!el.isConnected) { mounts.delete(el); continue; }
    out.push(render(el, open));
  }
  return Promise.all(out);
}

// post sends a task change from control el, then refreshes every group.
async function post(el, url, body, failed) {
  el.disabled = true;
  try {
    await fetchJSON(url, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 8000,
    });
  } catch (e) {
    showToast(failed + e.message, 'error');
  }
  el.blur();
  await refreshTasks();
}

function setState(el, id, state) {
  if (id && state) return post(el, NZ_CONTRACT.API.tasks_state, { id, state }, '没有改成：');
}

nzBus.addEventListener('inbox:count', () => { refreshTasks(); });

registerActions({
  'task-state': (el) => setState(el, el.dataset.id, el.value),
  'task-reopen': (el) => setState(el, el.dataset.id, 'doing'),
  'task-stop': (el) => el.dataset.id && post(el, NZ_CONTRACT.API.tasks_stop, { id: el.dataset.id }, '没有停下：'),
});
