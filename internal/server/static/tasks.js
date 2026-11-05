// tasks.js — the "任务" group of the Planner page's left column (planner RFC
// §15.4, D85, §15.7): the sessions the Planner dispatched and its promises,
// from GET /api/tasks, in three parts — 进行中 (等你 and 失败 first), 待提醒,
// and 已完成 under a fold (the gateway archives a done task 24 hours on); no
// part counts its cards. A card shows its title (a dispatched session's
// opens it in the middle column; a promise's is plain text, so the two tell
// apart without a label), who set its state and why, and when. Its state is
// said once: by the state menu (进行中 / 已完成, posting /api/tasks/state as
// yours), or on a done card in words, which reopens. A dispatched session
// runs as soon as the Planner dispatches it (D88), so its card has 停止
// (POST /api/tasks/stop: its process is closed, the task 已完成 as yours,
// shown as 已停止); the Planner page's header offers the same 停止 for the
// session opened in the middle column (isRunningTask, nzBus 'tasks:changed').
//
// It refetches on inbox_count frames (nzBus 'inbox:count': the health
// changes that mark 等你 / 失败 open and close items too) and on
// refreshTasks() (a Planner turn ending, the page's slow tick).
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { formatAbsTime, formatMonthDayTime, timeAgo } from './utilities.js';

// Mounted containers; detached ones are dropped at the next refresh.
const mounts = new Set();
// byId is the last list read, for isRunningTask.
let byId = new Map();

const STATE_LABEL = { doing: '进行中', waiting: '等你', failed: '失败', done: '已完成', remind: '待提醒' };
const BY_LABEL = { planner: 'Planner 判断', user: '你标记', auto: '自动完成', health: '按健康状态', ended: '会话已结束' };
// STOPPED_WHY is the reason the gateway gives a task you stopped
// (task.StoppedWhy): done as yours, said as 已停止 · 你停的.
const STOPPED_WHY = '已停止';
const SECTIONS = [
  { id: 'doing', label: '进行中', states: ['waiting', 'failed', 'doing'] },
  { id: 'remind', label: '待提醒', states: ['remind'] },
];


const stopped = (t) => t.state === 'done' && t.by === 'user' && t.why === STOPPED_WHY;

// running reports whether dispatched session t is in progress (进行中 or 等你)
// and still there: what 停止 applies to.
const running = (t) => t.kind === 'session' && !t.retired && (t.state === 'doing' || t.state === 'waiting');

function titleHtml(t) {
  if (t.kind !== 'session') return esc(t.title);
  return '<button type="button" class="pl-link tk-open" data-action="planner-open-session" data-key="' + escAttr(t.id) + '" title="打开这个会话">' + esc(t.title) + '</button>';
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

function stopHtml(t) {
  if (!running(t)) return '';
  return '<button type="button" class="pl-btn tk-stop" data-action="task-stop" data-id="' + escAttr(t.id) + '" title="停止这个会话正在做的事">停止</button>';
}

function cardHtml(t) {
  const state = STATE_LABEL[t.state] ? t.state : 'doing';
  const at = Date.parse(t.at);
  const meta = [];
  if (stopped(t)) meta.push('你停的');
  else if (t.by && BY_LABEL[t.by]) meta.push(esc(BY_LABEL[t.by]) + (t.why ? '：' + esc(t.why) : ''));
  if (t.due) {
    const d = new Date(t.due);
    if (!isNaN(d.getTime())) meta.push((state === 'remind' ? '到点提醒 ' : '到期 ') + esc(formatMonthDayTime(d.getTime())));
  }
  if (t.close_when) meta.push('完成条件：' + esc(t.close_when));
  return '<div class="tk-card tk-s-' + state + (state === 'done' ? ' done' : '') + '" data-id="' + escAttr(t.id) + '" role="listitem">' +
      '<div class="tk-top">' +
        (state === 'done' ? '<span class="tk-state">' + (stopped(t) ? STOPPED_WHY : STATE_LABEL.done) + '</span>' : '') +
        '<div class="tk-title">' + titleHtml(t) + '</div>' +
        (!isNaN(at) ? '<span class="tk-time" title="' + escAttr(formatAbsTime(at)) + '">' + esc(timeAgo(at)) + '</span>' : '') +
      '</div>' +
      '<div class="tk-foot"><div class="tk-meta">' + meta.join('<span aria-hidden="true"> · </span>') + '</div>' + stopHtml(t) + menuHtml(t) + '</div>' +
    '</div>';
}

// tasksHtml renders a GET /api/tasks answer.
export function tasksHtml(data) {
  const tasks = (data && Array.isArray(data.tasks)) ? data.tasks : [];
  let html = '<h2 class="pl-group-h tk-head">任务</h2>';
  if (!tasks.length) return html + '<div class="pl-empty">还没有任务<span>Planner 派出的会话和它答应你的事会列在这里</span></div>';
  for (const s of SECTIONS) {
    const rows = tasks.filter((t) => s.states.includes(t.state));
    if (!rows.length) continue;
    html += '<section class="tk-sec tk-sec-' + s.id + '"><div class="tk-sec-h">' + s.label + '</div>' +
      '<div class="tk-list" role="list">' + rows.map(cardHtml).join('') + '</div></section>';
  }
  const done = tasks.filter((t) => t.state === 'done');
  if (done.length) {
    html += '<details class="tk-sec tk-sec-done"><summary class="tk-sec-h">已完成</summary>' +
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

async function render(el) {
  try {
    const data = await fetchJSON(NZ_CONTRACT.API.tasks);
    byId = new Map(((data && Array.isArray(data.tasks)) ? data.tasks : []).map((t) => [t.id, t]));
    nzBus.dispatchEvent(new CustomEvent('tasks:changed'));
    // The Planner conversation's cards name tasks by these titles.
    nzBus.dispatchEvent(new CustomEvent('tasks:list', { detail: data }));
    // The 已完成 fold keeps its open state across refreshes, and a menu in
    // use is not repainted under the pointer.
    if (el.contains(document.activeElement) && document.activeElement.matches('.tk-menu')) return;
    const wasOpen = !!el.querySelector('.tk-sec-done')?.open;
    el.innerHTML = tasksHtml(data);
    const fold = el.querySelector('.tk-sec-done');
    if (fold && wasOpen) fold.open = true;
  } catch {
    el.innerHTML = '<h2 class="pl-group-h tk-head">任务</h2><div class="pl-empty">加载失败</div>';
  }
}

// mountTasks renders the group into el and keeps it current.
export function mountTasks(el) {
  el.classList.add('tk-group');
  mounts.add(el);
  return render(el);
}

// refreshTasks re-renders every mounted group.
export function refreshTasks() {
  const out = [];
  for (const el of mounts) {
    if (!el.isConnected) { mounts.delete(el); continue; }
    out.push(render(el));
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
  el.disabled = false;
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
