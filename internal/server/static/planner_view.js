// planner_view.js — the Planner page (docs/rfc/planner.md §10, §10.1, D77;
// §15.1 PR ⑥). Left: 等你 (the open inbox items of GET /api/inbox, each
// closable; decision cards answer through /api/inbox/decide) and 答应你的
// (commitments; none are recorded yet), then the browser-notification,
// push-pause and 停用 Planner controls. Middle: the 早报 slot and the
// planner:main event stream, read-only. Right: the workbench, on planner:main
// (workbench_panes.js); while it is open the left column folds to a 48px
// strip. On a phone the left column is a collapsible block above the stream.
//
// The activity-bar badge is the inbox's `badge` count (notify.js), seeded
// from GET /api/inbox on every WS auth_ok and kept by inbox_count frames.
// Layering: never import dashboard.js back; setActivityView is a shell slot.
import { NZ_CONTRACT } from './contract.js';
import { sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { sid } from './session_ident.js';
import { eventAlreadyRendered, renderEventsWithDividers } from './event_render.js';
import { runPendingAsync } from './render_md.js';
import { notifySupported, onInboxCount, requestNotifyPermission } from './notify.js';
import { followWorkbenchSubject, noteWorkbenchEvent } from './workbench_panes.js';
import { workbench } from './workbench.js';
import { wsm } from './ws_manager.js';
import { shell } from './shell.js';
import { formatAbsTime, lastDividerTime } from './utilities.js';

const PLANNER_KEY = 'planner:main';
// EVENTS_LIMIT is the first page of the conversation; later events come in
// by the poll's `after` cursor.
const EVENTS_LIMIT = 200;
// POLL_MS: the stream follows the Planner while the page is visible; every
// SLOW_TICKS-th tick also repaints the waiting times and the status.
const POLL_MS = 3000;
const SLOW_TICKS = 10;

const KIND_LABEL = { decision: '决定', question: '提问', error: '错误', notice: '通知' };
const KIND_ORDER = { decision: 0, question: 1, error: 2, notice: 3 };
const ACTION_LABEL = { approve: '批准', reject: '驳回' };

const pl = {
  status: null, // GET /api/planner
  open: [], // open 等你 items
  promises: [], // 答应你的
  pausedUntil: 0,
  customPause: false,
  lastTime: 0, // newest event time rendered
  loading: false,
  gen: 0, // bumped by each first-page load
  timer: 0,
  ticks: 0,
  // seeding: onInboxCount runs from our own fetch, so its inbox:count must
  // not refetch.
  seeding: false,
};

const $ = (id) => document.getElementById(id);

// ---- 等你 ----

function waitedText(ms) {
  const m = Math.floor(ms / 60000);
  if (m < 1) return '不到 1 分钟';
  if (m < 60) return m + ' 分钟';
  const h = Math.floor(m / 60);
  if (h < 24) return h + ' 小时';
  return Math.floor(h / 24) + ' 天';
}

// unitTarget is the listed local session an item is about, or null.
function unitTarget(unit) {
  const s = unit ? sessionList.sessionsData[sid(unit, 'local')] : null;
  return s ? { key: unit, title: s.user_label || s.summary || s.last_prompt || unit, project: s.project || '' } : null;
}

function itemHtml(it) {
  const created = Date.parse(it.created);
  const kind = KIND_LABEL[it.kind] ? it.kind : 'notice';
  const target = unitTarget(it.unit);
  const meta = target
    ? [target.project, target.title].filter(Boolean).map(esc).join(' · ') +
      ' <button type="button" class="pl-link" data-action="session-select" data-key="' + escAttr(target.key) + '" data-node="local">打开</button>'
    : (it.unit ? esc(it.unit === PLANNER_KEY ? 'Planner' : it.unit) : '');
  let acts = '';
  if (it.kind === 'decision' && it.decision && Array.isArray(it.decision.actions)) {
    acts = '<div class="pl-card-acts">' + it.decision.actions.map((a, i) =>
      '<button type="button" class="pl-btn' + (i === 0 ? ' pri' : '') + '" data-action="planner-inbox-decide" data-id="' + escAttr(it.id) + '" data-act="' + escAttr(a) + '">' +
        esc(ACTION_LABEL[a] || a) + '</button>').join('') + '</div>';
  }
  return '<div class="pl-card pl-k-' + kind + '" role="listitem" data-id="' + escAttr(it.id) + '">' +
      '<div class="pl-card-top">' +
        '<span class="pl-kind">' + esc(KIND_LABEL[kind]) + '</span>' +
        (!isNaN(created) ? '<span class="pl-wait" title="' + escAttr(formatAbsTime(created)) + '">等了 ' + esc(waitedText(Date.now() - created)) + '</span>' : '') +
        '<button type="button" class="pl-close btn-close" data-action="planner-inbox-close" data-id="' + escAttr(it.id) + '" title="关闭" aria-label="关闭这一条">&times;</button>' +
      '</div>' +
      '<div class="pl-card-title">' + esc(it.title || '') + '</div>' +
      (it.why ? '<div class="pl-card-why">' + esc(it.why) + '</div>' : '') +
      (meta ? '<div class="pl-card-meta">' + meta + '</div>' : '') +
      acts +
    '</div>';
}

function paintCounts() {
  const w = String(pl.open.length), p = String(pl.promises.length);
  for (const id of ['pl-wait-count', 'pl-wait-n', 'pl-strip-wait']) { const el = $(id); if (el) el.textContent = w; }
  for (const id of ['pl-promise-count', 'pl-promise-n', 'pl-strip-promise']) { const el = $(id); if (el) el.textContent = p; }
}

function paintWaiting() {
  const box = $('pl-wait');
  if (!box) return;
  const items = pl.open.slice().sort((a, b) =>
    ((KIND_ORDER[a.kind] ?? 3) - (KIND_ORDER[b.kind] ?? 3)) || (Date.parse(a.created) - Date.parse(b.created)));
  box.innerHTML = items.length ? items.map(itemHtml).join('') : '<div class="pl-empty">没有等你的事</div>';
  paintCounts();
}

function paintPromises() {
  const box = $('pl-promise');
  if (!box) return;
  box.innerHTML = pl.promises.length
    ? pl.promises.map((c) => '<div class="pl-card" role="listitem">' + esc(c.title || '') + '</div>').join('')
    : '<div class="pl-empty">还没有答应你的事<span>你交代给 Planner 的承诺会列在这里</span></div>';
  paintCounts();
}

async function fetchInbox() {
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.inbox, { timeoutMs: 8000 });
    pl.open = (res && Array.isArray(res.open)) ? res.open : [];
    pl.pausedUntil = res && res.paused_until ? Date.parse(res.paused_until) || 0 : 0;
    if (res && res.counts) {
      pl.seeding = true;
      try { onInboxCount(res.counts); } finally { pl.seeding = false; }
    }
  } catch { return; }
  if (ui.activeView === 'planner') { paintWaiting(); paintTools(); }
}

// ---- controls ----

function notifyToolHtml() {
  if (!notifySupported()) return '<span class="pl-tool-note">这个浏览器不支持通知</span>';
  const p = Notification.permission;
  if (p === 'granted') return '<span class="pl-tool-note">浏览器通知已允许</span>';
  if (p === 'denied') return '<span class="pl-tool-note" title="在浏览器的网站设置里改为允许">浏览器通知已被拒绝</span>';
  return '<button type="button" class="pl-btn" data-action="planner-notify-perm">允许浏览器通知</button>';
}

function pauseToolHtml() {
  if (pl.pausedUntil > Date.now()) {
    const d = new Date(pl.pausedUntil);
    const same = d.toDateString() === new Date().toDateString();
    const at = (same ? '' : (d.getMonth() + 1) + '/' + d.getDate() + ' ') +
      String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
    return '<span class="pl-tool-note" title="' + escAttr(formatAbsTime(pl.pausedUntil)) + '">已暂停到 ' + esc(at) + '，期间只进"等你"</span>' +
      '<button type="button" class="pl-btn" data-action="planner-pause-resume">恢复推送</button>';
  }
  if (pl.customPause) {
    return '<label class="pl-pause-custom"><input type="number" id="pl-pause-hours" min="1" max="168" value="3" aria-label="暂停小时数"> 小时</label>' +
      '<button type="button" class="pl-btn pri" data-action="planner-pause-custom-ok">暂停</button>' +
      '<button type="button" class="pl-btn" data-action="planner-pause-custom-cancel">取消</button>';
  }
  return '<button type="button" class="pl-btn" data-action="planner-pause" data-for="1h">1 小时</button>' +
    '<button type="button" class="pl-btn" data-action="planner-pause" data-for="today">今天</button>' +
    '<button type="button" class="pl-btn" data-action="planner-pause-custom">自定义</button>';
}

function stopToolHtml() {
  const s = pl.status;
  const on = !!(s && s.enabled);
  return '<label class="pl-switch' + (on ? '' : ' off') + '"' + (on ? '' : ' title="planner.enabled 未开启"') + '>' +
    '<input type="checkbox" id="pl-stop" data-action-change="planner-stop"' + (s && s.stopped ? ' checked' : '') + (on ? '' : ' disabled') + '>' +
    '<span>停用 Planner</span></label>';
}

function paintTools() {
  const box = $('pl-tools');
  if (!box) return;
  // Repainting under an open custom-pause input would drop what is typed.
  if (pl.customPause && box.contains(document.activeElement)) return;
  box.innerHTML =
    '<div class="pl-tool">' + notifyToolHtml() + '</div>' +
    '<div class="pl-tool"><span class="pl-tool-l">暂停推送</span><div class="pl-tool-btns">' + pauseToolHtml() + '</div></div>' +
    '<div class="pl-tool">' + stopToolHtml() + '</div>';
}

async function pause(forVal) {
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.inbox_pause, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ for: forVal }), timeoutMs: 8000,
    });
    pl.pausedUntil = res && res.paused_until ? Date.parse(res.paused_until) || 0 : 0;
    pl.customPause = false;
  } catch (e) {
    showToast('暂停推送失败：' + e.message, 'error');
  }
  paintTools();
}

// ---- center ----

function paintStatus() {
  const s = pl.status;
  const chip = $('pl-status');
  const note = $('pl-note');
  let text = '', cls = '';
  if (s) {
    if (!s.enabled) { text = '未开启'; cls = 'off'; }
    else if (s.stopped) { text = '已停用'; cls = 'off'; }
    else if (s.alive) { text = '在线'; cls = 'on'; }
    else { text = '启动中'; cls = 'wait'; }
  }
  if (chip) { chip.textContent = text; chip.className = 'pl-status' + (cls ? ' ' + cls : ''); }
  if (note) {
    let html = '';
    if (s && !s.enabled) {
      html = '<b>Planner 没有开启。</b>在配置里设 <code>planner.enabled: true</code> 并重启 naozhi 后，这里是你和 Planner 的对话：早报、回执、它派出的会话与汇报。左栏的"等你"照常可用。';
    } else if (s && s.stopped) {
      html = '<b>Planner 已停用。</b>它不再运行，也不收消息；关掉左栏的"停用 Planner"即可恢复。';
    }
    note.innerHTML = html;
    note.hidden = !html;
  }
  const brief = $('pl-brief');
  if (brief && !brief.childElementCount) {
    brief.innerHTML = '<div class="pl-brief-empty"><b>早报</b> · 还没有（工作日 08:30 之后生成）</div>';
  }
}

async function fetchStatus() {
  try {
    pl.status = await fetchJSON(NZ_CONTRACT.API.planner, { timeoutMs: 8000 });
  } catch { return; }
  if (ui.activeView === 'planner') { paintStatus(); paintTools(); paintStreamEmpty(); }
}

function paintStreamEmpty() {
  const box = $('pl-stream');
  if (!box) return;
  const empty = box.querySelector(':scope > .pl-empty');
  const hasEvents = !!box.querySelector('.event');
  if (hasEvents || !(pl.status && pl.status.enabled)) { if (empty) empty.remove(); return; }
  if (!empty) box.insertAdjacentHTML('beforeend', '<div class="pl-empty">还没有对话</div>');
}

function appendEvents(events, reset) {
  const box = $('pl-stream');
  const scroller = $('pl-events');
  if (!box || !scroller) return;
  const atBottom = reset || scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 40;
  const fresh = reset ? events : events.filter((e) => !eventAlreadyRendered(box, e.uuid));
  for (const e of events) if (e && e.time > pl.lastTime) pl.lastTime = e.time;
  if (reset) box.innerHTML = renderEventsWithDividers(fresh, 0);
  else if (fresh.length) {
    box.querySelector(':scope > .pl-empty')?.remove();
    box.insertAdjacentHTML('beforeend', renderEventsWithDividers(fresh, lastDividerTime(box)));
    fresh.forEach(noteWorkbenchEvent);
  }
  paintStreamEmpty();
  runPendingAsync();
  if (atBottom) scroller.scrollTop = scroller.scrollHeight;
}

// loadEvents fetches the first page (reset) or what came after the last
// event shown. A reset supersedes a poll still in flight.
async function loadEvents(reset) {
  if (reset) pl.gen++;
  else if (pl.loading) return;
  const gen = pl.gen;
  pl.loading = true;
  const q = reset || !pl.lastTime ? '&limit=' + EVENTS_LIMIT : '&after=' + pl.lastTime;
  try {
    const events = await fetchJSON(NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(PLANNER_KEY) + q, { timeoutMs: 8000 });
    if (gen === pl.gen && ui.activeView === 'planner') appendEvents(Array.isArray(events) ? events : [], reset);
  } catch { /* the next tick retries */ } finally {
    if (gen === pl.gen) pl.loading = false;
  }
}

// ---- page lifecycle ----

export function openPlannerPanel() {
  if (ui.activeView !== 'planner') { shell.setActivityView('planner'); return; }
  paintStatus();
  paintWaiting();
  paintPromises();
  paintTools();
  pl.lastTime = 0;
  loadEvents(true);
  fetchStatus();
  fetchInbox();
  followWorkbenchSubject();
  if (!pl.timer) {
    pl.timer = setInterval(() => {
      if (document.hidden || ui.activeView !== 'planner') return;
      loadEvents(false);
      if (++pl.ticks % SLOW_TICKS === 0) { paintWaiting(); fetchStatus(); }
    }, POLL_MS);
  }
}

// closePlannerPanel runs on leaving the page (ui.activeView already moved):
// the workbench was showing planner:main, so it closes and re-points.
export function closePlannerPanel() {
  if (pl.timer) { clearInterval(pl.timer); pl.timer = 0; }
  pl.customPause = false;
  if (workbench.isOpen()) workbench.close();
  $('planner-main')?.classList.remove('pl-wb-open', 'pl-left-peek');
  followWorkbenchSubject();
}

nzBus.addEventListener('workbench:open', () => {
  if (ui.activeView === 'planner') $('planner-main')?.classList.add('pl-wb-open');
});
nzBus.addEventListener('workbench:close', () => {
  $('planner-main')?.classList.remove('pl-wb-open', 'pl-left-peek');
});
// A changed count means an item opened or closed: refetch the list shown.
nzBus.addEventListener('inbox:count', (e) => {
  const n = e.detail.badge;
  const badge = $('abnav-planner-count');
  if (badge) { badge.textContent = n > 99 ? '99+' : String(n); badge.hidden = !n; }
  if (!pl.seeding && ui.activeView === 'planner') fetchInbox();
});
wsm.onReady(() => { fetchInbox(); });

registerActions({
  'planner-inbox-close': async (el) => {
    const id = el.dataset.id;
    el.disabled = true;
    try {
      await fetchJSON(NZ_CONTRACT.API.inbox_close, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }), timeoutMs: 8000 });
      pl.open = pl.open.filter((it) => it.id !== id);
      paintWaiting();
    } catch (e) {
      el.disabled = false;
      showToast('关闭失败：' + e.message, 'error');
    }
    fetchInbox();
  },
  'planner-inbox-decide': async (el) => {
    const card = el.closest('.pl-card');
    card?.querySelectorAll('.pl-card-acts button').forEach((b) => { b.disabled = true; });
    try {
      await fetchJSON(NZ_CONTRACT.API.inbox_decide, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: el.dataset.id, action: el.dataset.act }), timeoutMs: 15000,
      });
    } catch (e) {
      showToast('没有生效：' + e.message, 'error');
    }
    fetchInbox();
  },
  'planner-notify-perm': async () => {
    await requestNotifyPermission();
    paintTools();
  },
  'planner-pause': (el) => pause(el.dataset.for),
  'planner-pause-custom': () => { pl.customPause = true; paintTools(); $('pl-pause-hours')?.focus(); },
  'planner-pause-custom-cancel': () => { pl.customPause = false; paintTools(); },
  'planner-pause-custom-ok': () => {
    const h = Number($('pl-pause-hours')?.value);
    if (!(h >= 1 && h <= 168)) { showToast('填 1 到 168 小时', 'warning'); return; }
    pause(Math.round(h) + 'h');
  },
  'planner-pause-resume': async () => {
    try {
      await fetchJSON(NZ_CONTRACT.API.inbox_pause, { method: 'DELETE', timeoutMs: 8000 });
      pl.pausedUntil = 0;
    } catch (e) {
      showToast('恢复推送失败：' + e.message, 'error');
    }
    paintTools();
  },
  'planner-stop': async (el) => {
    const stopped = el.checked;
    el.disabled = true;
    try {
      pl.status = await fetchJSON(NZ_CONTRACT.API.planner_state, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ stopped }), timeoutMs: 15000,
      });
    } catch (e) {
      showToast((stopped ? '停用' : '恢复') + '失败：' + e.message, 'error');
    }
    paintStatus();
    paintTools();
    paintStreamEmpty();
  },
});

document.addEventListener('DOMContentLoaded', () => {
  $('pl-strip-expand')?.addEventListener('click', () => $('planner-main')?.classList.toggle('pl-left-peek'));
  $('pl-left-toggle')?.addEventListener('click', (e) => {
    const main = $('planner-main');
    if (!main) return;
    const open = main.classList.toggle('pl-left-open');
    e.currentTarget.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
});
