// promises.js — the "答应你的" group of the Planner page's left column
// (planner RFC §3.6, §10.1): the promises the Planner made, the earliest due
// first and undated ones last, each with its due time and state, closed only
// by the user (D67). mountPromises(el) renders the group into el and keeps it
// current: a due reminder opens a "等你" item, so inbox_count frames
// (nzBus 'inbox:count') refresh it, and refreshPromises() does on demand
// (a promise action block landing).
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';

// Mounted containers; detached ones are dropped at the next refresh.
const mounts = new Set();

// recentClosed is how many closed promises the group lists under its fold.
const recentClosed = 5;

const pad = (n) => (n < 10 ? '0' + n : '' + n);
function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}
function hm(d) { return pad(d.getHours()) + ':' + pad(d.getMinutes()); }
function mdhm(d) { return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + hm(d); }

// dueInfo is a promise's due line and its state class: overdue (red),
// due today (amber), later, or none.
export function dueInfo(p, now = Date.now()) {
  if (!p.due) return { text: '无到期时间', cls: '' };
  const due = new Date(p.due);
  if (isNaN(due.getTime())) return { text: '', cls: '' };
  if (p.state === 'open' && due.getTime() <= now) return { text: '已到期 · ' + mdhm(due), cls: 'overdue' };
  if (sameDay(due, new Date(now))) return { text: '今天 ' + hm(due) + ' 到期', cls: 'today' };
  return { text: mdhm(due) + ' 到期', cls: '' };
}

function cardHtml(p, now) {
  const due = dueInfo(p, now);
  const open = p.state === 'open';
  const meta = [];
  if (due.text) meta.push('<span class="pm-due ' + due.cls + '">' + esc(due.text) + '</span>');
  if (p.close_when) meta.push('<span>完成条件：' + esc(p.close_when) + '</span>');
  if (p.source === 'user') meta.push('<span>你登记的</span>');
  if (!open) meta.push('<span>已关闭</span>');
  return '<div class="pm-card' + (open ? '' : ' closed') + '" data-id="' + escAttr(p.id) + '">' +
    '<div class="pm-text">' + esc(p.text) + '</div>' +
    '<div class="pm-meta">' + meta.join('<span aria-hidden="true"> · </span>') + '</div>' +
    (open ? '<button type="button" class="fv-btn pm-close" data-action="promise-close" data-id="' + escAttr(p.id) + '">关闭</button>' : '') +
    '</div>';
}

// promisesHtml renders a GET /api/promises answer.
export function promisesHtml(data, now = Date.now()) {
  const open = (data && data.open) || [];
  const closed = ((data && data.closed) || []).slice(0, recentClosed);
  let html = '<div class="pm-head">答应你的 · ' + open.length + '</div>';
  html += open.length
    ? open.map((p) => cardHtml(p, now)).join('')
    : '<div class="pm-empty">暂时没有答应你的事</div>';
  if (closed.length) {
    html += '<details class="pm-closed"><summary>最近关闭 · ' + closed.length + '</summary>' +
      closed.map((p) => cardHtml(p, now)).join('') + '</details>';
  }
  return html;
}

export function loadPromises() {
  return fetchJSON(NZ_CONTRACT.API.promises);
}

export function closePromise(id) {
  return fetchJSON(NZ_CONTRACT.API.promises_close, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id }),
  });
}

async function render(el) {
  try {
    const data = await loadPromises();
    // The "最近关闭" fold keeps its open state across refreshes.
    const fold = el.querySelector('.pm-closed');
    const wasOpen = !!(fold && fold.open);
    el.innerHTML = promisesHtml(data);
    const next = el.querySelector('.pm-closed');
    if (next && wasOpen) next.open = true;
  } catch {
    el.innerHTML = '<div class="pm-head">答应你的</div><div class="pm-empty">加载失败</div>';
  }
}

// mountPromises renders the group into el and keeps it current.
export function mountPromises(el) {
  el.classList.add('pm-group');
  mounts.add(el);
  return render(el);
}

// refreshPromises re-renders every mounted group.
export function refreshPromises() {
  const out = [];
  for (const el of mounts) {
    if (!el.isConnected) { mounts.delete(el); continue; }
    out.push(render(el));
  }
  return Promise.all(out);
}

nzBus.addEventListener('inbox:count', () => { refreshPromises(); });

registerActions({
  'promise-close': async (btn) => {
    const id = btn.dataset.id;
    if (!id || btn.disabled) return;
    btn.disabled = true;
    try {
      await closePromise(id);
    } catch (e) {
      // 409: closed elsewhere already; the refresh shows it.
      if (e.status !== 409) showToast('关闭失败', 'error');
    }
    await refreshPromises();
  },
});
