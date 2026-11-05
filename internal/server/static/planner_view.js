// planner_view.js — the Planner page (docs/rfc/planner.md §10, §10.1, D77;
// §15.1 PR ⑥, §15.4 D85, §15.7). Left: 通知 (the open inbox items of GET
// /api/inbox, decisions and questions first and labelled, each closable;
// decision cards answer through /api/inbox/decide) and the #pl-task
// container 任务 (tasks.js) mounts into. The header's "⋯" menu holds the
// seldom used controls — browser notifications (only while the browser has
// not asked yet), push pause and 停用 Planner — and while pushes are paused
// the header says until when, with 恢复. Middle: the planner:main event
// stream with the morning briefs of GET /api/planner/briefs placed in it by
// the time they were made (D91), its action blocks and the sessions it names
// as cards (planner_cards.js), and the input box and the opened session card
// (planner_chat.js). Right: the workbench, on planner:main or the opened
// session (workbench_panes.js); while it is open the left column folds to a
// 48px strip. On a phone the left column is a collapsible block above the
// stream.
//
// The stream follows a WS subscription to planner:main; GET
// /api/sessions/events loads the first page and polls while the subscription
// is not live. What the gateway writes for the Planner (its receipts) is not
// shown, and its "收到，正在看" to subscribers is the input box's note
// (D91). The gateway's notes in the conversation (due reminders, dispatch
// outcomes) live in its in-memory history, which a restart drops: the first
// page merges in the journaled ones from GET /api/planner/notes by time.
//
// The rail badge counts the inbox's `badge` (notify.js: what waits on you to
// act — questions, decisions and errors, §15.7 S1); the left column's "通知"
// heading counts what its list shows, every open item (`open`). Both are
// seeded from GET /api/inbox on every WS auth_ok and kept by inbox_count
// frames. The 会话 view's sidebar bar (side_wait.js) shows the badge's items
// and count: actionListHtml and fetchInbox, with 'inbox:items' on nzBus
// after the list changes.
//
// The middle column (#pl-center) is one component with two hosts: the page
// and the Planner drawer (planner_drawer.js, §15.3 ②), which gets the very
// nodes moved in. pl.host says which one runs it, so every id and handler
// here serves both; opening the page takes it back from the drawer.
// Layering: never import dashboard.js back; setActivityView is a shell slot.
import { NZ_CONTRACT } from './contract.js';
import { sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, sessionFailureText, showToast } from './nz_util.js';
import { sid } from './session_ident.js';
import { renderEventsWithDividers } from './event_render.js';
import { renderMd, runPendingAsync } from './render_md.js';
import { inbox, isLit, notifySupported, onInboxCount, requestNotifyPermission, revealFocus } from './notify.js';
import { followWorkbenchSubject, noteWorkbenchEvent } from './workbench_panes.js';
import { decisionActsHtml, decorateEvents, paintSessionCards, refreshHealth, webLinkHtml } from './planner_cards.js';
import { clearAck, clearPending, closePlannerSession, mountComposer, noteAck, notePlannerStatus } from './planner_chat.js';
import { onSessionsApplied } from './session_list.js';
import { mountTasks, refreshTasks } from './tasks.js';
import { workbench } from './workbench.js';
import { wsm } from './ws_manager.js';
import { shell } from './shell.js';
import { formatAbsTime, lastDividerTime, timeAgo } from './utilities.js';

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

// KIND_LABEL labels a card by kind: the kinds that wait on you to act. A
// notice, the 通知 group's own kind, is not labelled.
const KIND_LABEL = { decision: '待你决定', question: '提问', error: '错误' };
const KIND_ORDER = { decision: 0, question: 1, error: 2, notice: 3 };

const pl = {
  status: null, // GET /api/planner
  briefs: [], // the sent morning briefs, newest first, GET /api/planner/briefs
  // briefOpen is the fold the user chose per brief day: a first page
  // repaints the stream, and a brief folded stays folded.
  briefOpen: new Map(),
  open: [], // open 通知 items
  pausedUntil: 0,
  customPause: false,
  // lastTime is the newest logged event time taken; seenAtWM the uuids taken
  // at it (`after` and a re-subscribe both re-deliver that millisecond).
  lastTime: 0,
  seenAtWM: new Set(),
  // ack follows the "收到，正在看" by the input box (followAck), null when
  // none is shown.
  ack: null,
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

// waitedText is how long a question or a decision has waited on you.
function waitedText(ms) {
  const m = Math.floor(ms / 60000);
  if (m < 1) return '不到 1 分钟';
  if (m < 60) return m + ' 分钟';
  const h = Math.floor(m / 60);
  if (h < 24) return h + ' 小时';
  return Math.floor(h / 24) + ' 天';
}

// itemWhy is a card's subtitle: for a failed session the cause in words
// (its death reason, else its error class), else the item's own why. Items
// opened before v0.4.1 wrote "失败 · 其他", which says nothing the title
// does not.
function itemWhy(it, target) {
  const cause = it.kind === 'error' && target ? sessionFailureText(target.s) : '';
  return cause || (it.why === '失败' || it.why === '失败 · 其他' ? '' : it.why || '');
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
  return s ? { key, node, s, title: s.user_label || s.summary || s.last_prompt || key, project: s.project || '' } : null;
}

// metaHtml is where an item comes from: its session's workspace and name
// (the name left out when the title says it) with 打开, else a unit that is
// not listed; the Planner's own items need no source.
function metaHtml(it) {
  const target = unitTarget(it.unit);
  if (target) {
    const name = (it.title || '').includes(target.title) ? '' : target.title;
    return [target.project, name].filter(Boolean).map(esc).join(' · ') +
      ' <button type="button" class="pl-link" data-action="session-select" data-key="' + escAttr(target.key) + '" data-node="' + escAttr(target.node) + '">打开</button>';
  }
  return it.unit && it.unit !== PLANNER_KEY ? esc(it.unit) : '';
}

// timeHtml is how long a question or a decision has waited on you, or how
// long ago anything else came in.
function timeHtml(it, kind) {
  const created = Date.parse(it.created);
  if (isNaN(created)) return '';
  const waits = kind === 'decision' || kind === 'question';
  return '<span class="pl-wait" title="' + escAttr(formatAbsTime(created)) + '">' +
    esc(waits ? '等了 ' + waitedText(Date.now() - created) : timeAgo(created)) + '</span>';
}

function itemHtml(it) {
  const kind = KIND_LABEL[it.kind] ? it.kind : 'notice';
  const meta = metaHtml(it);
  const link = webLinkHtml(it.link);
  const acts = decisionActsHtml(it);
  const why = itemWhy(it, unitTarget(it.unit));
  return '<div class="pl-card pl-k-' + kind + (isLit(it.id) ? ' inbox-lit' : '') + '" role="listitem" data-id="' + escAttr(it.id) + '">' +
      '<div class="pl-card-top">' +
        (KIND_LABEL[kind] ? '<span class="pl-kind">' + esc(KIND_LABEL[kind]) + '</span>' : '') +
        timeHtml(it, kind) +
        '<button type="button" class="pl-close btn-close" data-action="planner-inbox-close" data-id="' + escAttr(it.id) + '" title="关闭" aria-label="关闭这一条">&times;</button>' +
      '</div>' +
      '<div class="pl-card-title">' + esc(it.title || '') + '</div>' +
      (why ? '<div class="pl-card-why" title="' + escAttr(why) + '">' + esc(why) + '</div>' : '') +
      (meta || link ? '<div class="pl-card-meta">' + [meta, link].filter(Boolean).join(' · ') + '</div>' : '') +
      acts +
    '</div>';
}

// sortedOpen orders the open items: decisions first, then questions,
// errors and notices, the oldest first within a kind.
function sortedOpen() {
  return pl.open.slice().sort((a, b) =>
    ((KIND_ORDER[a.kind] ?? 3) - (KIND_ORDER[b.kind] ?? 3)) || (Date.parse(a.created) - Date.parse(b.created)));
}

// waitListHtml is the open items' cards, as the left column lists them.
function waitListHtml() {
  const items = sortedOpen();
  return items.length ? items.map(itemHtml).join('') : '<div class="pl-empty">没有通知</div>';
}

// actionListHtml is the cards of the open items that wait on you to act
// (KIND_LABEL's kinds, the badge's), for the 会话 sidebar's bar.
export function actionListHtml() {
  return sortedOpen().filter((it) => KIND_LABEL[it.kind]).map(itemHtml).join('');
}

function noteItems() {
  nzBus.dispatchEvent(new CustomEvent('inbox:items'));
}

// paintCounts: 通知 counts the open items its list shows. A write only on
// change.
function paintCounts() {
  const w = String(inbox.open || 0);
  ['pl-wait-count', 'pl-wait-n', 'pl-strip-wait'].forEach((id) => { const el = $(id); if (el && el.textContent !== w) el.textContent = w; });
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

// notifyToolHtml is 允许浏览器通知 while the browser has not asked yet; once
// allowed or refused there is nothing to do here.
function notifyToolHtml() {
  if (!notifySupported() || Notification.permission !== 'default') return '';
  return '<div class="pl-tool"><button type="button" class="pl-btn" data-action="planner-notify-perm">允许浏览器通知</button></div>';
}

// pausedAt is the pause's end as HH:mm, with the date when not today.
function pausedAt() {
  const d = new Date(pl.pausedUntil);
  const same = d.toDateString() === new Date().toDateString();
  return (same ? '' : (d.getMonth() + 1) + '/' + d.getDate() + ' ') +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}

function pauseToolHtml() {
  if (pl.pausedUntil > Date.now()) {
    return '<button type="button" class="pl-btn" data-action="planner-pause-resume">恢复推送</button>';
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

// closeMenu folds the header's "⋯" menu.
function closeMenu() {
  const more = $('pl-more');
  if (more) more.open = false;
}

// paintTools fills the header's "⋯" menu and, while pushes are paused, its
// 推送暂停到 line.
function paintTools() {
  const paused = $('pl-paused');
  if (paused) {
    const on = pl.pausedUntil > Date.now();
    const html = on ? '<span class="pl-paused-l">推送</span><span>暂停到 ' + esc(pausedAt()) + '</span>' +
      '<span aria-hidden="true">·</span><button type="button" class="pl-link" data-action="planner-pause-resume">恢复</button>' : '';
    if (paused.innerHTML !== html) paused.innerHTML = html;
    paused.title = on ? formatAbsTime(pl.pausedUntil) : '';
    paused.hidden = !on;
  }
  const box = $('pl-tools');
  if (!box) return;
  // Repainting under the custom-pause input would drop what is typed; only
  // the input holding focus counts (Chromium focuses the clicked 自定义).
  if (pl.customPause && document.activeElement?.id === 'pl-pause-hours') return;
  box.innerHTML = notifyToolHtml() +
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
    closeMenu();
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
    // Running is the normal case and goes unsaid.
    else if (s.alive) { text = ''; }
    else { text = '启动中'; cls = 'wait'; }
  }
  if (chip) { chip.textContent = text; chip.className = 'pl-status' + (cls ? ' ' + cls : ''); }
  if (note) {
    // The drawer has neither the left column nor the "⋯" menu: point at the
    // page instead.
    const drawer = pl.host === 'drawer';
    const left = drawer ? 'Planner 页面左栏' : '左栏';
    const menu = drawer ? 'Planner 页面头部的"⋯"' : '头部的"⋯"';
    let html = '';
    if (s && !s.enabled) {
      html = '<b>Planner 没有开启。</b>在配置里设 <code>planner.enabled: true</code> 并重启 naozhi 后，这里是你和 Planner 的对话：早报、它派出的会话与汇报。' + left + '的"通知"照常可用。';
    } else if (s && s.stopped) {
      html = '<b>Planner 已停用。</b>它不再运行，也不收消息；关掉' + menu + '里的"停用 Planner"即可恢复。';
    }
    note.innerHTML = html;
    note.hidden = !html;
  }
}

// ---- 早报 ----

// BRIEF_FOCUS leads the push ID of a morning brief (planner/brief:
// "brief:<day>"), which a notification or #planner link brings into view.
const BRIEF_FOCUS = 'brief:';

function briefHtml(b, t) {
  const d = new Date(t);
  const pad = (n) => String(n).padStart(2, '0');
  const day = b.day || '';
  const open = pl.briefOpen.has(day) ? pl.briefOpen.get(day) : d.toDateString() === new Date().toDateString();
  const when = ' · ' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  return '<details class="pl-brief" data-day="' + escAttr(day) + '" data-time="' + t + '"' + (open ? ' open' : '') + '>' +
    '<summary><b>早报</b>' + esc(when) + '</summary><div class="md">' + renderMd(b.markdown) + '</div></details>';
}

// placeBriefs puts each brief not yet shown into the stream by the time it
// was made (D91): a message among the others, the day's open and the older
// ones folded unless the user chose otherwise (pl.briefOpen).
function placeBriefs() {
  const box = $('pl-stream');
  const scroller = $('pl-events');
  if (!box || !scroller) return;
  const atBottom = scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 40;
  let placed = false;
  for (const b of pl.briefs.slice().reverse()) {
    const t = Date.parse(b.at);
    if (isNaN(t) || box.querySelector(':scope > .pl-brief[data-day="' + CSS.escape(b.day || '') + '"]')) continue;
    const next = [...box.children].find((c) => Number(c.dataset.time || 0) > t);
    if (next) next.insertAdjacentHTML('beforebegin', briefHtml(b, t));
    else box.insertAdjacentHTML('beforeend', briefHtml(b, t));
    placed = true;
  }
  if (!placed) return;
  paintStreamEmpty();
  runPendingAsync();
  if (atBottom) scroller.scrollTop = scroller.scrollHeight;
  revealBrief();
}

// revealBrief opens and scrolls to the brief a push or link asked for
// (inbox.focus "brief:<day>"), once the stream shows it: a first page in
// flight is about to repaint the stream, placeBriefs reveals it then.
function revealBrief() {
  if (!inbox.focus.startsWith(BRIEF_FOCUS) || pl.host !== 'page' || pl.loading) return;
  const day = inbox.focus.slice(BRIEF_FOCUS.length);
  const el = $('pl-stream')?.querySelector(':scope > .pl-brief[data-day="' + CSS.escape(day) + '"]');
  if (!el) return;
  inbox.focus = '';
  el.open = true;
  el.scrollIntoView({ block: 'start' });
}

// fetchBriefs takes the sent briefs; an unreadable list leaves the stream
// as it was.
async function fetchBriefs() {
  let res;
  try {
    res = await fetchJSON(NZ_CONTRACT.API.planner_briefs, { timeoutMs: 8000 });
  } catch { return; }
  const list = res && Array.isArray(res.briefs) ? res.briefs : [];
  pl.briefs = list.filter((b) => b && b.sent && b.markdown);
  if (pl.host) placeBriefs();
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
  const hasEvents = !!box.querySelector('.event, .pl-brief');
  if (hasEvents || !(pl.status && pl.status.enabled)) { if (empty) empty.remove(); return; }
  if (!empty) box.insertAdjacentHTML('beforeend', '<div class="pl-empty">还没有对话</div>');
}

// admitEvent is the same-ms replay gate (scratchAdmitEvent's rule): false
// for a logged event already taken. A uuid-less event is a pushed system
// line (stamped with the push's wall clock) that is never replayed: it is
// taken and leaves the watermark to the log.
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
// dispatch's outcome, your answer to its question, a task you stopped):
// written for the model, with keys and block formats in it, so it is not
// shown (D91); what it reports has its own line or card already.
const RECEIPT_RE = /^<naozhi-receipt>/;
// SCOUT_RE is a scout run waking the Planner with what it found
// (planner/scout): the turn is marked 来自巡检 rather than shown as your bubble.
// MEETING_RE is a pre-meeting brief's material, marked 来自会前简报.
const SCOUT_RE = /^<naozhi-scout findings="(\d+)"/;
const MEETING_RE = /^<naozhi-scout kind="meeting" start="(\d\d:\d\d)"/;
// COALESCE_HEAD leads requests that queued while the Planner was busy and
// were merged into one turn (turn.Coalesce): each follows as "\n[HH:MM] ".
// Every part gets the rules above, the gateway's as their lines and yours
// joined into one bubble.
const COALESCE_HEAD = '[以下是用户在你处理上一条消息期间追加发送的内容]\n';
const COALESCE_SPLIT_RE = /\n\[\d\d:\d\d\] /;
function plannerEvents(e) {
  const text = e.type === 'user' ? e.detail || e.summary || '' : '';
  if (!text.startsWith(COALESCE_HEAD)) {
    const p = withoutSnapshot(e);
    return p ? [p] : [];
  }
  const out = [];
  let mine = null;
  for (const part of text.slice(COALESCE_HEAD.length).split(COALESCE_SPLIT_RE)) {
    const t = part.trim();
    if (!t) continue;
    const p = withoutSnapshot(Object.assign({}, e, { detail: t, summary: t }));
    if (!p) continue;
    if (p.type !== 'user') { delete p.uuid; out.push(p); continue; }
    if (!mine) { mine = p; out.push(mine); continue; }
    mine.detail = [mine.detail, p.detail].filter(Boolean).join('\n\n');
    mine.summary = [mine.summary, p.summary].filter(Boolean).join(' ');
  }
  return out;
}

// withoutSnapshot is e as the conversation shows it, null for a receipt.
function withoutSnapshot(e) {
  if (e.type === 'user' && RECEIPT_RE.test(e.detail || e.summary || '')) return null;
  const sc = e.type === 'user' && SCOUT_RE.exec(e.detail || e.summary || '');
  if (sc) return Object.assign({}, e, { type: 'system', summary: '来自巡检：' + sc[1] + ' 条新发现', detail: '' });
  const mt = e.type === 'user' && MEETING_RE.exec(e.detail || e.summary || '');
  if (mt) return Object.assign({}, e, { type: 'system', summary: '来自会前简报：' + mt[1] + ' 开始的会议', detail: '' });
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

// ACK_TEXT is the gateway's "got it" for a message you sent (planner/agent
// ReceiptText), pushed to subscribers only: it shows by the input box until
// the answer comes, not as a line above your bubble (D91).
const ACK_TEXT = '收到，正在看';
const isAck = (e) => !!e && !e.uuid && e.type === 'system' && e.summary === ACK_TEXT;
// ACK_SLACK_MS is how far before an ack from another tab its message's user
// bubble may be logged: the turn can start before the gateway pushes it.
const ACK_SLACK_MS = 2000;

// followAck takes the ack away once the answer to your message starts: the
// first reply after the user bubble logged past pl.ack.after, your message
// taken into a turn. A turn already running when you sent (a scout run, an
// earlier message) answers something else and leaves it.
function followAck(shown) {
  const a = pl.ack;
  if (!a) return;
  for (const e of shown) {
    const t = e.time || 0;
    if (!a.mine) {
      if (e.type === 'user' && t > a.after) a.mine = t;
    } else if ((e.type === 'text' || e.type === 'result') && t >= a.mine) {
      pl.ack = null;
      clearAck();
      return;
    }
  }
}

// dropAck takes the ack away when no answer will come.
function dropAck() {
  pl.ack = null;
  clearAck();
}

function appendEvents(events, reset) {
  const box = $('pl-stream');
  const scroller = $('pl-events');
  if (!box || !scroller) return;
  const atBottom = reset || scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 40;
  if (reset) { pl.lastTime = 0; pl.seenAtWM.clear(); }
  const ack = events.find(isAck);
  if (ack) {
    if (!pl.ack) pl.ack = { after: (ack.time || 0) - ACK_SLACK_MS, mine: 0 };
    noteAck();
  }
  const fresh = events.filter((e) => admitEvent(e) && !isAck(e));
  const shown = fresh.flatMap(plannerEvents);
  const html = renderEventsWithDividers(shown, reset ? 0 : lastDividerTime(box), RENDER_OPTS);
  followAck(shown);
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
  // After the scroll: a brief a push asked for scrolls to itself.
  if (reset) placeBriefs();
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
    if (gen === pl.gen && pl.host) {
      // The page is in: what it paints is no longer about to be replaced.
      pl.loading = false;
      appendEvents(events, reset);
    }
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
  fetchBriefs();
  fetchInbox();
  if (!pl.timer) {
    pl.timer = setInterval(() => {
      if (document.hidden || !pl.host) return;
      if (!(pl.sub === 'live' && wsm.isConnected())) loadEvents(false);
      if (++pl.ticks % SLOW_TICKS === 0) {
        paintWaiting();
        fetchStatus();
        fetchBriefs();
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
  closeMenu();
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
nzBus.addEventListener('inbox:focus', () => { revealWaiting(); revealBrief(); });
// toggle does not bubble: a brief's fold is taken on the way down.
document.addEventListener('toggle', (e) => {
  const el = e.target;
  if (el.matches?.('#pl-stream > .pl-brief')) pl.briefOpen.set(el.dataset.day || '', el.open);
}, true);
// A message to the Planner is going out: its ack waits for a user bubble
// logged past what the stream has taken so far.
nzBus.addEventListener('planner:sent', () => { pl.ack = { after: pl.lastTime, mine: 0 }; });
// A session listed since a card named it gets its name and state.
onSessionsApplied(() => { if (pl.host) paintSessionCards($('pl-stream')); });
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
  dropAck();
  if (pl.sub) appendEvents([{ type: 'system', time: Date.now(), summary: '没有发出：' + (msg.error || '') }], false);
}, ours);
// A process appearing upgrades a suspended subscription; a dead process or
// a timed-out push loop ends it, and polling covers until the next one.
wsm.on(NZ_CONTRACT.WS.session_state, (msg) => {
  if (msg.state === 'dead') dropAck();
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
      closeMenu();
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
  // The "⋯" menu closes on a click outside it. The path is the one at
  // dispatch: a menu button repaints itself away before this runs.
  document.addEventListener('click', (e) => {
    const more = $('pl-more');
    if (more?.open && !e.composedPath().includes(more)) more.open = false;
  });
});
