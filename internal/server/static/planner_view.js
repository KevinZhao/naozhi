// planner_view.js — the Planner page (docs/rfc/planner.md §10, §10.1, D77;
// §15.1 PR ⑥, §15.4 D85). Left: 通知 (the open inbox items of GET
// /api/inbox, decisions and questions first and labelled, each closable;
// decision cards answer through /api/inbox/decide) and the #pl-task
// container 任务 (tasks.js) mounts into, then the browser-notification,
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
// questions, §10.1); the left column's "通知" heading counts what its list
// shows, every open item (`open`). Both are seeded from GET /api/inbox on
// every WS auth_ok and kept by inbox_count frames. The 会话 view's sidebar
// bar (side_wait.js) shows the same list and counts: waitCount, waitListHtml
// and fetchInbox, with 'inbox:items' on nzBus after the list changes.
//
// The middle column (#pl-center) is one component with two hosts: the page
// and the Planner drawer (planner_drawer.js, §15.3 ②), which gets the very
// nodes moved in. pl.host says which one runs it, so every id and handler
// here serves both; opening the page takes it back from the drawer.
// Layering: never import dashboard.js back; setActivityView is a shell slot.
import { NZ_CONTRACT } from './contract.js';
import { sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { sid } from './session_ident.js';
import { renderEventsWithDividers } from './event_render.js';
import { runPendingAsync } from './render_md.js';
import { inbox, isLit, notifySupported, onInboxCount, requestNotifyPermission, revealFocus } from './notify.js';
import { followWorkbenchSubject, noteWorkbenchEvent } from './workbench_panes.js';
import { decisionActsHtml, decorateEvents, paintSessionCards, refreshHealth } from './planner_cards.js';
import { clearPending, closePlannerSession, mountComposer, notePlannerStatus } from './planner_chat.js';
import { mountTasks, refreshTasks } from './tasks.js';
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
// TASK_REFRESH_MS is how long after a Planner turn ends 任务 is refetched:
// the turn's promise and status blocks are run off the read loop.
const TASK_REFRESH_MS = 1500;

const KIND_LABEL = { decision: '待你决定', question: '提问', error: '错误', notice: '通知' };
const KIND_ORDER = { decision: 0, question: 1, error: 2, notice: 3 };

const pl = {
  status: null, // GET /api/planner
  open: [], // open 通知 items
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
  // host runs the conversation: 'page', 'drawer', or '' (not shown).
  host: '',
  // focusId is the 通知 item marked above the conversation; focusAt when it
  // was asked for, inboxAt when pl.open was last read (an older list cannot
  // tell it is gone), focusSeen once it showed.
  focusId: '',
  focusAt: 0,
  inboxAt: 0,
  focusSeen: false,
};

const $ = (id) => document.getElementById(id);

// ---- 通知 ----

function waitedText(ms) {
  const m = Math.floor(ms / 60000);
  if (m < 1) return '不到 1 分钟';
  if (m < 60) return m + ' 分钟';
  const h = Math.floor(m / 60);
  if (h < 24) return h + ' 小时';
  return Math.floor(h / 24) + ' 天';
}

// unitTarget is the listed session an item is about, or null: a local one,
// or a remote node's, whose unit is "<node>/<key>".
function unitTarget(unit) {
  if (!unit) return null;
  let key = unit, node = 'local';
  let s = sessionList.sessionsData[sid(key, node)];
  const i = unit.indexOf('/');
  if (!s && i > 0) {
    node = unit.slice(0, i);
    key = unit.slice(i + 1);
    s = sessionList.sessionsData[sid(key, node)];
  }
  return s ? { key, node, title: s.user_label || s.summary || s.last_prompt || key, project: s.project || '' } : null;
}

function itemHtml(it) {
  const created = Date.parse(it.created);
  const kind = KIND_LABEL[it.kind] ? it.kind : 'notice';
  const target = unitTarget(it.unit);
  const meta = target
    ? [target.project, target.title].filter(Boolean).map(esc).join(' · ') +
      ' <button type="button" class="pl-link" data-action="session-select" data-key="' + escAttr(target.key) + '" data-node="' + escAttr(target.node) + '">打开</button>'
    : (it.unit ? esc(it.unit === PLANNER_KEY ? 'Planner' : it.unit) : '');
  const acts = decisionActsHtml(it);
  return '<div class="pl-card pl-k-' + kind + (isLit(it.id) ? ' inbox-lit' : '') + '" role="listitem" data-id="' + escAttr(it.id) + '">' +
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

// waitCount is the "通知" count the left column shows: every open item.
export function waitCount() {
  return inbox.open || 0;
}

// sortedOpen orders the open items: decisions first, then questions,
// errors and notices, the oldest first within a kind.
function sortedOpen() {
  return pl.open.slice().sort((a, b) =>
    ((KIND_ORDER[a.kind] ?? 3) - (KIND_ORDER[b.kind] ?? 3)) || (Date.parse(a.created) - Date.parse(b.created)));
}

// waitListHtml is the open items' cards, as the left column lists them.
export function waitListHtml() {
  const items = sortedOpen();
  return items.length ? items.map(itemHtml).join('') : '<div class="pl-empty">没有通知</div>';
}

function noteItems() {
  nzBus.dispatchEvent(new CustomEvent('inbox:items'));
}

// paintCounts: 通知 counts the open items its list shows, 任务 the cards
// not done mounted in #pl-task. A write only on change.
function paintCounts() {
  const w = String(waitCount());
  const p = String(document.querySelectorAll('#pl-task .tk-card:not(.done)').length);
  const put = (ids, v) => ids.forEach((id) => { const el = $(id); if (el && el.textContent !== v) el.textContent = v; });
  put(['pl-wait-count', 'pl-wait-n', 'pl-strip-wait'], w);
  put(['pl-task-n', 'pl-strip-task'], p);
}

function paintWaiting() {
  const box = $('pl-wait');
  if (!box) return;
  box.innerHTML = waitListHtml();
  paintCounts();
  paintInlineCards(sortedOpen());
  paintFocus();
  revealWaiting();
}

// revealWaiting brings the focused item into view (notify.js), unfolding
// the phone's left block or the workbench's strip when it is folded.
function revealWaiting() {
  if (!inbox.focus || ui.activeView !== 'planner' || !$('pl-wait')?.querySelector('.pl-card[data-id="' + CSS.escape(inbox.focus) + '"]')) return;
  const main = $('planner-main');
  if (main?.classList.contains('pl-wb-open')) main.classList.add('pl-left-peek');
  if (main && !main.classList.contains('pl-left-open')) {
    main.classList.add('pl-left-open');
    $('pl-left-toggle')?.setAttribute('aria-expanded', 'true');
  }
  revealFocus($('pl-wait'));
}

// paintInlineCards repeats the Planner's open questions with options at the
// end of the conversation, where they were asked; the left column keeps them
// too, and either answers.
function paintInlineCards(items) {
  const box = $('pl-cards');
  if (!box) return;
  const mine = items.filter((it) => it.unit === PLANNER_KEY && it.decision && it.id !== pl.focusId);
  const html = mine.length ? '<div class="pl-cards-h">待你回答 · ' + mine.length + '</div>' + mine.map(itemHtml).join('') : '';
  if (box.innerHTML !== html) box.innerHTML = html;
  box.hidden = !html;
}

// paintFocus shows the marked item, until it closes.
function paintFocus() {
  const box = $('pl-focus');
  if (!box) return;
  const it = pl.focusId ? pl.open.find((x) => x.id === pl.focusId) : null;
  if (it) pl.focusSeen = true;
  else if (pl.focusSeen) pl.focusId = '';
  let html = '';
  if (it) html = '<div class="pl-cards-h">通知</div>' + itemHtml(it);
  else if (pl.focusId && pl.inboxAt >= pl.focusAt) html = '<div class="pl-empty">这一条已经不在"通知"里了</div>';
  if (box.innerHTML !== html) box.innerHTML = html;
  box.hidden = !html;
}

// focusInboxItem marks the open 通知 item id above the conversation (a push
// leads to it, §15.3 ④); '' clears the mark. A session open in the middle
// column hides the mark and takes the input box, so it goes back to the
// Planner conversation first.
export function focusInboxItem(id) {
  if (id) closePlannerSession(false);
  pl.focusId = id || '';
  pl.focusAt = Date.now();
  pl.focusSeen = false;
  paintWaiting();
  if (pl.focusId) fetchInbox();
}

export async function fetchInbox() {
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.inbox, { timeoutMs: 8000 });
    pl.open = (res && Array.isArray(res.open)) ? res.open : [];
    pl.inboxAt = Date.now();
    pl.pausedUntil = res && res.paused_until ? Date.parse(res.paused_until) || 0 : 0;
    if (res && res.counts) {
      pl.seeding = true;
      try { onInboxCount(res.counts); } finally { pl.seeding = false; }
    }
  } catch { return; }
  if (pl.host) { paintWaiting(); paintTools(); }
  noteItems();
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
    return '<span class="pl-tool-note" title="' + escAttr(formatAbsTime(pl.pausedUntil)) + '">已暂停到 ' + esc(at) + '，期间只进"通知"</span>' +
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
    // The drawer has no left column: point at the page instead.
    const drawer = pl.host === 'drawer';
    const left = drawer ? 'Planner 页面左栏' : '左栏';
    let html = '';
    if (s && !s.enabled) {
      html = '<b>Planner 没有开启。</b>在配置里设 <code>planner.enabled: true</code> 并重启 naozhi 后，这里是你和 Planner 的对话：早报、回执、它派出的会话与汇报。' + left + '的"通知"照常可用。';
    } else if (s && s.stopped) {
      html = '<b>Planner 已停用。</b>它不再运行，也不收消息；关掉' + (drawer ? ' ' : '') + left + '的"停用 Planner"即可恢复。';
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
  if (pl.host) { paintStatus(); paintTools(); paintStreamEmpty(); }
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
// dispatch's outcome, your answer to its question, a task you stopped): not
// something you wrote, so
// it shows as a gateway line rather than your bubble.
const RECEIPT_RE = /^<naozhi-receipt>\s*([\s\S]*?)\s*(?:<\/naozhi-receipt>\s*)?$/;
// SCOUT_RE is a scout run waking the Planner with what it found
// (planner/scout): the turn is marked 来自巡检 rather than shown as your bubble.
const SCOUT_RE = /^<naozhi-scout findings="(\d+)"/;
function withoutSnapshot(e) {
  const r = e.type === 'user' && RECEIPT_RE.exec(e.detail || e.summary || '');
  if (r) return Object.assign({}, e, { type: 'system', summary: '回执给 Planner：' + r[1], detail: '' });
  const sc = e.type === 'user' && SCOUT_RE.exec(e.detail || e.summary || '');
  if (sc) return Object.assign({}, e, { type: 'system', summary: '来自巡检：' + sc[1] + ' 条新发现', detail: '' });
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
    if (fresh.some((e) => e.type === 'result')) setTimeout(refreshTasks, TASK_REFRESH_MS);
    box.insertAdjacentHTML('beforeend', html);
    // The workbench follows planner:main on the page only; beside the
    // drawer it shows the chat view's session.
    if (pl.host === 'page') fresh.forEach(noteWorkbenchEvent);
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
    if (gen === pl.gen && pl.host) appendEvents(events, reset);
  } catch { /* the next tick retries */ } finally {
    if (gen === pl.gen) pl.loading = false;
  }
}

// ---- the conversation and its hosts ----

function startConversation() {
  mountComposer();
  paintStatus();
  paintWaiting();
  refreshHealth($('pl-center'), false);
  // The subscription follows the first page: its `after` is the page's end.
  loadEvents(true).then(() => { if (pl.host && !pl.sub) subscribe(); });
  fetchStatus();
  fetchInbox();
  if (!pl.timer) {
    pl.timer = setInterval(() => {
      if (document.hidden || !pl.host) return;
      if (!(pl.sub === 'live' && wsm.isConnected())) loadEvents(false);
      if (++pl.ticks % SLOW_TICKS === 0) {
        paintWaiting();
        fetchStatus();
        refreshHealth($('pl-center'), false);
        paintSessionCards($('pl-center'));
        // A task's 等你 / 失败 follow health, which pushes no frame.
        if (pl.host === 'page') refreshTasks();
        if (!pl.sub) subscribe();
      }
    }, POLL_MS);
  }
}

function stopConversation() {
  if (pl.timer) { clearInterval(pl.timer); pl.timer = 0; }
  unsubscribe();
  closePlannerSession(false);
  pl.host = '';
  pl.focusId = '';
  paintFocus();
}

// hostConversation moves #pl-center into box and runs it there for host;
// the host it leaves hears it on nzBus 'planner:host' (detail.host).
function hostConversation(host, box) {
  const center = $('pl-center');
  if (!center || !box) return;
  const prev = pl.host;
  if (prev) stopConversation();
  if (center.parentElement !== box) box.appendChild(center);
  pl.host = host;
  if (prev && prev !== host) nzBus.dispatchEvent(new CustomEvent('planner:host', { detail: { host } }));
  startConversation();
}

// attachPlannerConversation runs the conversation in the drawer's box.
export function attachPlannerConversation(box) {
  hostConversation('drawer', box);
}

// detachPlannerConversation stops it there and puts it back on the page.
export function detachPlannerConversation() {
  if (pl.host !== 'drawer') return;
  stopConversation();
  const center = $('pl-center');
  if (center) $('planner-main')?.appendChild(center);
}

// ---- page lifecycle ----

export function openPlannerPanel() {
  if (ui.activeView !== 'planner') { shell.setActivityView('planner'); return; }
  hostConversation('page', $('planner-main'));
  mountTasks($('pl-task'));
  paintTools();
  followWorkbenchSubject();
}

// closePlannerPanel runs on leaving the page (ui.activeView already moved):
// the workbench was showing planner:main, so it closes and re-points.
export function closePlannerPanel() {
  if (pl.host === 'page') stopConversation();
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
  if (!pl.seeding && pl.host) fetchInbox();
});
nzBus.addEventListener('inbox:focus', revealWaiting);
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
      noteItems();
    } catch (e) {
      el.disabled = false;
      showToast('关闭失败：' + e.message, 'error');
    }
    fetchInbox();
  },
  'planner-inbox-decide': async (el) => {
    // The left column's card and the conversation's repeat of it answer
    // the same item.
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
  // The strip and the phone heading count what 任务 renders.
  const tasks = $('pl-task');
  if (tasks) new MutationObserver(paintCounts).observe(tasks, { childList: true, subtree: true });
});
