// planner_view.js — the Planner page (docs/rfc/planner.md §10, §10.1, D77;
// §15.1 PR ⑥). Left: 等你 (the open inbox items of GET /api/inbox, each
// closable; decision cards answer through /api/inbox/decide) and the
// #pl-promise container 答应你的 mounts into, then the browser-notification,
// push-pause and 停用 Planner controls. Middle: the 早报 slot and the
// planner:main event stream, its action blocks and the sessions it names as
// cards (planner_cards.js), and the input box and the opened session card
// (planner_chat.js). Right: the workbench, on planner:main or the opened
// session (workbench_panes.js); while it is open the left column folds to a
// 48px strip. On a phone the left column is a collapsible block above the
// stream.
//
// The stream follows a WS subscription to planner:main (receipts are pushed
// to subscribers only, never logged); GET /api/sessions/events loads the
// first page and polls while the subscription is not live. The gateway's
// notes in the conversation (due reminders, dispatch outcomes) live in its
// in-memory history, which a restart drops: the first page merges in the
// journaled ones from GET /api/planner/notes by time.
//
// The rail badge counts the inbox's `badge` (notify.js: decisions and
// questions, §10.1); the left column's "等你" heading counts what its list
// shows, every open item (`open`). Both are seeded from GET /api/inbox on
// every WS auth_ok and kept by inbox_count frames.
// Layering: never import dashboard.js back; setActivityView is a shell slot.
import { NZ_CONTRACT } from './contract.js';
import { sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { sid } from './session_ident.js';
import { renderEventsWithDividers } from './event_render.js';
import { runPendingAsync } from './render_md.js';
import { inbox, notifySupported, onInboxCount, requestNotifyPermission } from './notify.js';
import { followWorkbenchSubject, noteWorkbenchEvent } from './workbench_panes.js';
import { decisionActsHtml, decorateEvents, paintSessionCards, refreshHealth, setInboxItems } from './planner_cards.js';
import { clearPending, closePlannerSession, mountComposer, notePlannerStatus } from './planner_chat.js';
import { mountPromises, refreshPromises } from './promises.js';
import { workbench } from './workbench.js';
import { wsm } from './ws_manager.js';
import { shell } from './shell.js';
import { formatAbsTime, lastDividerTime } from './utilities.js';

const PLANNER_KEY = 'planner:main';
// EVENTS_LIMIT is the first page of the conversation; later events come in
// by the subscription or the poll's `after` cursor.
const EVENTS_LIMIT = 200;
// POLL_MS: without a live subscription the stream is polled while the page
// is visible; every SLOW_TICKS-th tick also repaints the waiting times and
// the status, and retries a subscription that is not up.
const POLL_MS = 3000;
const SLOW_TICKS = 10;
// PROMISE_REFRESH_MS is how long after a Planner turn ends 答应你的 is
// refetched: the turn's promise blocks are registered off the read loop.
const PROMISE_REFRESH_MS = 1500;

const KIND_LABEL = { decision: '决定', question: '提问', error: '错误', notice: '通知' };
const KIND_ORDER = { decision: 0, question: 1, error: 2, notice: 3 };

const pl = {
  status: null, // GET /api/planner
  open: [], // open 等你 items
  pausedUntil: 0,
  customPause: false,
  // lastTime is the newest logged event time taken; seenAtWM the uuids taken
  // at it (`after` and a re-subscribe both re-deliver that millisecond).
  lastTime: 0,
  seenAtWM: new Set(),
  // sub is the planner:main subscription: '' none, 'pending' sent,
  // 'suspended' acked without a process (no live events), 'live'.
  sub: '',
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
  const acts = decisionActsHtml(it);
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

// paintCounts: 等你 counts the open items its list shows, 答应你的 the open
// cards mounted in #pl-promise. A write only on change: #pl-promise-count
// sits under the observer that calls this.
function paintCounts() {
  const w = String(inbox.open || 0);
  const p = String(document.querySelectorAll('#pl-promise .pm-card:not(.closed)').length);
  const put = (ids, v) => ids.forEach((id) => { const el = $(id); if (el && el.textContent !== v) el.textContent = v; });
  put(['pl-wait-count', 'pl-wait-n', 'pl-strip-wait'], w);
  put(['pl-promise-count', 'pl-promise-n', 'pl-strip-promise'], p);
}

function paintWaiting() {
  const box = $('pl-wait');
  if (!box) return;
  const items = pl.open.slice().sort((a, b) =>
    ((KIND_ORDER[a.kind] ?? 3) - (KIND_ORDER[b.kind] ?? 3)) || (Date.parse(a.created) - Date.parse(b.created)));
  box.innerHTML = items.length ? items.map(itemHtml).join('') : '<div class="pl-empty">没有等你的事</div>';
  paintCounts();
  paintInlineCards(items);
}

// paintInlineCards repeats the Planner's open cards (dispatch confirmations,
// its questions) at the end of the conversation, where they were asked; the
// left column keeps them too, and either answers.
function paintInlineCards(items) {
  const box = $('pl-cards');
  if (!box) return;
  const mine = items.filter((it) => it.unit === PLANNER_KEY && it.decision);
  const html = mine.length ? '<div class="pl-cards-h">待你确认 · ' + mine.length + '</div>' + mine.map(itemHtml).join('') : '';
  if (box.innerHTML !== html) box.innerHTML = html;
  box.hidden = !html;
}

async function fetchInbox() {
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.inbox, { timeoutMs: 8000 });
    pl.open = (res && Array.isArray(res.open)) ? res.open : [];
    setInboxItems(pl.open, (res && Array.isArray(res.closed)) ? res.closed : [], $('pl-stream'));
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
  notePlannerStatus(pl.status);
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

// admitEvent is the same-ms replay gate (scratchAdmitEvent's rule): false
// for a logged event already taken. A uuid-less event is a pushed system
// line (a receipt, stamped with the push's wall clock) that is never
// replayed: it is taken and leaves the watermark to the log.
function admitEvent(e) {
  if (!e || !e.uuid) return !!e;
  const t = e.time || 0;
  if (t < pl.lastTime || (t === pl.lastTime && pl.seenAtWM.has(e.uuid))) return false;
  if (t > pl.lastTime) { pl.lastTime = t; pl.seenAtWM.clear(); }
  pl.seenAtWM.add(e.uuid);
  return true;
}

// SNAPSHOT_RE is the <naozhi-snapshot> block the gateway puts before every
// message to the Planner (planner/agent Send); the bubble shows what you wrote.
// A session handed over with it (<naozhi-session-ref>) leads the message and
// becomes a fence that planner_cards.js turns into its card. The logged
// bubble keeps only the message's head (detail 2000 runes, summary 120), so
// either block may arrive cut.
const SNAPSHOT_RE = /^<naozhi-snapshot[\s>][\s\S]*?<\/naozhi-snapshot>\s*/;
const SNAPSHOT_CUT_RE = /^<naozhi-snapshot[\s>][\s\S]*$/;
const SESSION_REF_RE = /^<naozhi-session-ref>(\{[^\n]*?\})<\/naozhi-session-ref>\s*/;
const SESSION_REF_CUT_RE = /^<naozhi-session-ref>[\s\S]*$/;
const GATEWAY_HEAD_RE = /^<naozhi-(?:snapshot[\s>]|session-ref>)/;
// RECEIPT_RE is a gateway receipt to the Planner (planner/agent Tell: a
// dispatch's outcome, your answer to its card): not something you wrote, so
// it shows as a gateway line rather than your bubble.
const RECEIPT_RE = /^<naozhi-receipt>\s*([\s\S]*?)\s*(?:<\/naozhi-receipt>\s*)?$/;
function withoutSnapshot(e) {
  const r = e.type === 'user' && RECEIPT_RE.exec(e.detail || e.summary || '');
  if (r) return Object.assign({}, e, { type: 'system', summary: '回执给 Planner：' + r[1], detail: '' });
  if (e.type !== 'user' || !GATEWAY_HEAD_RE.test(e.detail || e.summary || '')) return e;
  let ref = '';
  let detail = (e.detail || '').replace(SESSION_REF_RE, (_, r) => { ref = r; return ''; });
  const cut = SNAPSHOT_CUT_RE.test(detail.replace(SNAPSHOT_RE, ''));
  detail = detail.replace(SNAPSHOT_RE, '').replace(SNAPSHOT_CUT_RE, '').replace(SESSION_REF_CUT_RE, '');
  if (cut) detail = '（消息过长，气泡里显示不下）';
  if (ref) detail = '```naozhi-session-ref\n' + ref + '\n```\n' + detail;
  const handed = /^<naozhi-session-ref>/.test(e.summary || '');
  let summary = (e.summary || '').replace(SESSION_REF_RE, '').replace(SESSION_REF_CUT_RE, '');
  summary = summary.replace(SNAPSHOT_RE, '').replace(SNAPSHOT_CUT_RE, '');
  if (handed) summary = '交给 Planner' + (summary ? '：' + summary : '');
  return Object.assign({}, e, { detail, summary });
}

const RENDER_OPTS = { key: PLANNER_KEY };

function appendEvents(events, reset) {
  const box = $('pl-stream');
  const scroller = $('pl-events');
  if (!box || !scroller) return;
  const atBottom = reset || scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 40;
  if (reset) { pl.lastTime = 0; pl.seenAtWM.clear(); }
  const fresh = events.filter(admitEvent);
  const html = renderEventsWithDividers(fresh.map(withoutSnapshot), reset ? 0 : lastDividerTime(box), RENDER_OPTS);
  if (reset) box.innerHTML = html;
  else if (fresh.length) {
    box.querySelector(':scope > .pl-empty')?.remove();
    if (fresh.some((e) => e.type === 'user')) clearPending(box);
    if (fresh.some((e) => e.type === 'result')) setTimeout(refreshPromises, PROMISE_REFRESH_MS);
    box.insertAdjacentHTML('beforeend', html);
    fresh.forEach(noteWorkbenchEvent);
  }
  decorateEvents(box);
  paintStreamEmpty();
  runPendingAsync();
  if (atBottom) scroller.scrollTop = scroller.scrollHeight;
}

// subscribe asks for planner:main's live events after the last one taken.
function subscribe() {
  const msg = { type: NZ_CONTRACT.WS.subscribe, key: PLANNER_KEY };
  if (pl.lastTime) msg.after = pl.lastTime;
  else msg.limit = EVENTS_LIMIT;
  pl.sub = wsm.send(msg) ? 'pending' : '';
}

function unsubscribe() {
  if (pl.sub) wsm.send({ type: NZ_CONTRACT.WS.unsubscribe, key: PLANNER_KEY });
  pl.sub = '';
}

// NOTE_SAME_MS: a note the history still holds (no restart since it was
// written) was logged within this of its journal line.
const NOTE_SAME_MS = 5000;

// withNotes merges the journaled notes into a first page, as the system
// lines they were: those the history no longer holds, and only from the
// page's first event on (older ones belong to pages not loaded). An empty
// page (a restart left no history) takes them all.
function withNotes(events, notes) {
  const floor = events.length ? (events[0].time || 0) : 0;
  const logged = events.filter((e) => e && e.type === 'system');
  const extra = [];
  for (const n of notes) {
    const t = Date.parse(n && n.time);
    if (!n || !n.text || isNaN(t) || t < floor) continue;
    if (logged.some((e) => Math.abs((e.time || 0) - t) <= NOTE_SAME_MS && e.summary && n.text.startsWith(e.summary))) continue;
    extra.push({ type: 'system', time: t, summary: n.text });
  }
  if (!extra.length) return events;
  return events.concat(extra).sort((a, b) => (a.time || 0) - (b.time || 0));
}

// loadEvents fetches the first page (reset), with the journaled notes, or
// what came after the last event taken. A reset supersedes a poll still in
// flight.
async function loadEvents(reset) {
  if (reset) pl.gen++;
  else if (pl.loading) return;
  const gen = pl.gen;
  pl.loading = true;
  const q = reset || !pl.lastTime ? '&limit=' + EVENTS_LIMIT : '&after=' + pl.lastTime;
  // An unreadable journal leaves the page as the history has it.
  const notes = reset ? fetchJSON(NZ_CONTRACT.API.planner_notes, { timeoutMs: 8000 }).catch(() => null) : null;
  try {
    // No planner:main session yet (it never had a turn) is no history, and
    // the notes still show.
    const res = await fetchJSON(NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(PLANNER_KEY) + q, { timeoutMs: 8000 })
      .catch((e) => { if (reset && e && e.status === 404) return []; throw e; });
    let events = Array.isArray(res) ? res : [];
    const n = notes && await notes;
    if (n && Array.isArray(n.notes) && n.notes.length) events = withNotes(events, n.notes);
    if (gen === pl.gen && ui.activeView === 'planner') appendEvents(events, reset);
  } catch { /* the next tick retries */ } finally {
    if (gen === pl.gen) pl.loading = false;
  }
}

// ---- page lifecycle ----

export function openPlannerPanel() {
  if (ui.activeView !== 'planner') { shell.setActivityView('planner'); return; }
  mountComposer();
  mountPromises($('pl-promise'));
  paintStatus();
  paintWaiting();
  paintTools();
  refreshHealth($('planner-main'), false);
  // The subscription follows the first page: its `after` is the page's end.
  loadEvents(true).then(() => { if (ui.activeView === 'planner' && !pl.sub) subscribe(); });
  fetchStatus();
  fetchInbox();
  followWorkbenchSubject();
  if (!pl.timer) {
    pl.timer = setInterval(() => {
      if (document.hidden || ui.activeView !== 'planner') return;
      if (!(pl.sub === 'live' && wsm.isConnected())) loadEvents(false);
      if (++pl.ticks % SLOW_TICKS === 0) {
        paintWaiting();
        fetchStatus();
        refreshHealth($('planner-main'), false);
        paintSessionCards($('planner-main'));
        if (!pl.sub) subscribe();
      }
    }, POLL_MS);
  }
}

// closePlannerPanel runs on leaving the page (ui.activeView already moved):
// the workbench was showing planner:main, so it closes and re-points.
export function closePlannerPanel() {
  if (pl.timer) { clearInterval(pl.timer); pl.timer = 0; }
  unsubscribe();
  closePlannerSession(false);
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
  paintCounts();
  if (!pl.seeding && ui.activeView === 'planner') fetchInbox();
});
// A new socket holds no subscriptions: re-subscribe after the last event
// taken, the history frame fills the gap.
wsm.onReady(() => {
  fetchInbox();
  pl.sub = '';
  if (pl.timer) subscribe();
});

// planner:main's frames are all claimed, so the session stream's bookkeeping
// never sees them (a late ack would pose as its subscription); they count
// only while the page holds a subscription.
const ours = (msg) => msg.key === PLANNER_KEY;
wsm.on(NZ_CONTRACT.WS.subscribed, (msg) => {
  if (pl.sub) pl.sub = msg.reason === 'suspended' ? 'suspended' : 'live';
}, ours);
wsm.on(NZ_CONTRACT.WS.error, () => { pl.sub = ''; }, ours);
wsm.on(NZ_CONTRACT.WS.history, (msg) => { if (pl.sub) appendEvents(msg.events || [], false); }, ours);
wsm.on(NZ_CONTRACT.WS.event, (msg) => { if (pl.sub && msg.event) appendEvents([msg.event], false); }, ours);
wsm.on(NZ_CONTRACT.WS.send_error, (msg) => {
  if (pl.sub) appendEvents([{ type: 'system', time: Date.now(), summary: '没有发出：' + (msg.error || '') }], false);
}, ours);
// A process appearing upgrades a suspended subscription; a dead process or
// a timed-out push loop ends it, and polling covers until the next one.
wsm.on(NZ_CONTRACT.WS.session_state, (msg) => {
  if (msg.state === 'running' && pl.sub === 'suspended') subscribe();
  else if (pl.sub && (msg.state === 'dead' || msg.reason === 'subscription_timeout')) pl.sub = '';
}, ours);

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
    // The left column's card and the conversation's dispatch card answer
    // the same item.
    const card = el.closest('.pl-card, .pl-act');
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
      notePlannerStatus(pl.status);
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
  // The strip and the phone heading count what 答应你的 renders.
  const promises = $('pl-promise');
  if (promises) new MutationObserver(paintCounts).observe(promises, { childList: true, subtree: true });
});
