// planner_view.js — the Planner page (docs/rfc/planner.md §10, §10.1, D77;
// §15.1 PR ⑥, §15.4 D85, §15.7). Left: 等你 (the open inbox items of GET
// /api/inbox that wait on you to act, decisions and questions first and
// labelled, each closable; decision cards answer through /api/inbox/decide)
// and the #pl-task container 任务 (tasks.js) mounts into. The header's "⋯"
// menu holds the seldom used controls — browser notifications (only while
// the browser has not asked yet), the one pause of pushes and custody (D94:
// no push goes out and no session is replied for while it holds) and 停用
// Planner — and while paused the header says so, with 恢复. Middle: the planner:main event
// stream with the morning briefs of GET /api/planner/briefs placed in it by
// the time they were made (D91), its action blocks and the sessions it names
// as cards (planner_cards.js), and the input box and the opened session card
// (planner_chat.js). Right: the workbench, on planner:main or the opened
// session (workbench_panes.js); it opens only from its button and leaves the
// left column as it is (D91). On a phone the left column is a block above
// the stream that, unfolded, takes the stream's place.
//
// The stream follows a WS subscription to planner:main; GET
// /api/sessions/events loads the first page, the older ones as the stream is
// scrolled to its top, and polls while the subscription is not live. What the gateway writes for the Planner (its receipts) is not
// shown, and its "收到，正在看" to subscribers is the input box's note
// (D91). The gateway's notes in the conversation (due reminders, dispatch
// outcomes) live in its in-memory history, which a restart drops: the first
// page merges in the journaled ones from GET /api/planner/notes by time.
//
// The rail badge counts the inbox's `badge` (notify.js: what waits on you to
// act — questions, decisions and errors, §15.7 S1), the one count of what is
// yours to handle, and the left column's 等你 lists those items alone (D94:
// a notice goes to the morning brief). It is seeded from GET /api/inbox on every WS auth_ok and kept
// by inbox_count frames. The 会话 view's sidebar bar (side_wait.js) shows the
// badge's items and count: actionListHtml and fetchInbox, with 'inbox:items'
// on nzBus after the list changes.
//
// The middle column (#pl-center) is one component with two hosts: the page
// and the Planner drawer (planner_drawer.js, §15.3 ②), which gets the very
// nodes moved in. pl.host says which one runs it, so every id and handler
// here serves both; opening the page takes it back from the drawer.
// Layering: never import dashboard.js back; setActivityView is a shell slot.
import { NZ_CONTRACT } from './contract.js';
import { sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, sessionFailureText, sessionTitleText, showToast } from './nz_util.js';
import { sid } from './session_ident.js';
import { leadingTimeDivider, renderEventsWithDividers } from './event_render.js';
import { dedupEarlierPage, hasMoreHeader } from './event_stream.js';
import { renderMd, runPendingAsync } from './render_md.js';
import { inbox, isLit, light, notifySupported, onInboxCount, onNotify, requestNotifyPermission, revealFocus } from './notify.js';
import { followWorkbenchSubject, noteWorkbenchEvent } from './workbench_panes.js';
import { decisionActsHtml, decorateEvents, noteWorkspaces, paintSessionCards, refreshHealth, unitName, webLinkHtml, workspaceName } from './planner_cards.js';
import { clearAck, clearPending, closePlannerSession, mountComposer, noteAck, notePlannerStatus, showPlannerLeft } from './planner_chat.js';
import { onSessionsApplied } from './session_list.js';
import { inProgress, mountTasks, refreshTasks } from './tasks.js';
import { initPlannerResize } from './planner_resize.js';
import { custody, refreshCustody, setCustodyPaused } from './custody.js';
import { workbench } from './workbench.js';
import { choosePlannerNode, eventsURL, hostName, hostOnline, hostRemote, isLocalNode, nodeAPI, notePlannerNodes, plannerAPI, plannerHost, plannerNodes, readLocalPlanner } from './planner_host.js';
import { wsm } from './ws_manager.js';
import { shell } from './shell.js';
import { EARLIER_PAGE_LIMIT, EVENT_DIVIDER_GAP_MS, formatAbsTime, formatMonthDayTime, lastDividerTime, timeAgo } from './utilities.js';

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

// KIND_LABEL labels a card by kind: the kinds that wait on you to act, the
// only ones listed. A notice is not labelled.
const KIND_LABEL = { decision: '待你决定', question: '提问', error: '错误' };
const KIND_ORDER = { decision: 0, question: 1, error: 2, notice: 3 };

const pl = {
  status: null, // GET /api/planner
  briefs: [], // the sent morning briefs, newest first, GET /api/planner/briefs
  // briefOpen is the fold the user chose per brief day: a first page
  // repaints the stream, and a brief folded stays folded.
  briefOpen: new Map(),
  open: [], // the open inbox items
  tasksActive: 0, // the tasks 进行中, as 任务 last listed them
  notice: '', // the notice a push or link opened, shown in 等你 (waitListHtml)
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
  // subNode is the node the subscription is on (plannerHost.node then).
  subNode: 'local',
  loading: false,
  gen: 0, // bumped by each first-page load
  timer: 0,
  ticks: 0,
  // seeding: onInboxCount runs from our own fetch, so its inbox:count must
  // not refetch.
  seeding: false,
  // host runs the conversation: 'page', 'drawer', or '' (not shown).
  host: '',
  // older is the cursor for the pages before the first (seedOlder); notes
  // the journaled notes the first page fetched, for the older pages too.
  older: { ms: 0, keys: [], more: false, loading: false },
  notes: [],
  // revealed is the day of the brief a push or link scrolled to: an older
  // page that moves it from the stream's top keeps it in view.
  revealed: '',
};

const $ = (id) => document.getElementById(id);

// ---- 等你 ----

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
// or a remote node's, whose unit is "<node>/<key>"; an item a node's inbox
// holds (it.node) is about that node's session.
function unitTarget(unit, from) {
  if (!unit) return null;
  if (!isLocalNode(from)) {
    const s = sessionList.sessionsData[sid(unit, from)];
    return s ? { key: unit, node: from, s, title: sessionTitleText(s) || unit, project: s.project || '' } : null;
  }
  let key = unit, node = 'local';
  let s = sessionList.sessionsData[sid(key, node)];
  const i = unit.indexOf('/');
  if (!s && i > 0) {
    node = unit.slice(0, i);
    key = unit.slice(i + 1);
    s = sessionList.sessionsData[sid(key, node)];
  }
  return s ? { key, node, s, title: sessionTitleText(s) || key, project: s.project || '' } : null;
}

// metaHtml is where an item comes from: its session's workspace and name
// (the name left out when the title says it) with 打开, else the workspace
// and name of a unit that is not listed, never its ID (D92); the Planner's
// own items need no source.
function metaHtml(it) {
  const title = it.title || '';
  const target = unitTarget(it.unit, it.node);
  if (target) {
    const name = title.includes(target.title) ? '' : target.title;
    return [target.project, name].filter(Boolean).map(esc).join(' · ') +
      ' <button type="button" class="pl-link" data-action="session-select" data-key="' + escAttr(target.key) + '" data-node="' + escAttr(target.node) + '">打开</button>';
  }
  if (!it.unit || it.unit === PLANNER_KEY) return '';
  const name = unitName(it.unit);
  return [workspaceName(it.ws), title.includes(name) ? '' : name].filter(Boolean).map(esc).join(' · ');
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

// itemRef is an item's ID on the page: its own for this node's inbox,
// "<node>|<id>" for the Planner's node's (the two inboxes number apart);
// itemOf reads one back.
const itemRef = (it) => (isLocalNode(it.node) ? it.id : it.node + '|' + it.id);
function itemOf(ref) {
  const i = ref.indexOf('|');
  return i > 0 ? { node: ref.slice(0, i), id: ref.slice(i + 1) } : { node: 'local', id: ref };
}

function itemHtml(it) {
  const kind = KIND_LABEL[it.kind] ? it.kind : 'notice';
  const meta = metaHtml(it);
  const link = webLinkHtml(it.link);
  const acts = decisionActsHtml(Object.assign({}, it, { id: itemRef(it) }));
  const why = itemWhy(it, unitTarget(it.unit, it.node));
  const ref = itemRef(it);
  return '<div class="pl-card pl-k-' + kind + (isLit(ref) ? ' inbox-lit' : '') + '" role="listitem" data-id="' + escAttr(ref) + '">' +
      '<div class="pl-card-top">' +
        (KIND_LABEL[kind] ? '<span class="pl-kind">' + esc(KIND_LABEL[kind]) + '</span>' : '') +
        timeHtml(it, kind) +
        '<button type="button" class="pl-close btn-close" data-action="planner-inbox-close" data-id="' + escAttr(ref) + '" title="关闭" aria-label="关闭这一条">&times;</button>' +
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

// actionItems are the open items that wait on you to act (KIND_LABEL's
// kinds: questions — custody's stops among them —, decisions and errors), the
// badge's count. A notice is the morning brief's (D94): no list holds it.
const actionItems = () => sortedOpen().filter((it) => KIND_LABEL[it.kind]);

// waitListHtml is the left column's 等你: the cards actionListHtml lists,
// and the notice a push or link opened (inbox.focus), which no list holds
// otherwise: it stays at the end, uncounted, until it closes.
function waitListHtml() {
  if (inbox.focus && pl.open.some((it) => itemRef(it) === inbox.focus && !KIND_LABEL[it.kind])) pl.notice = inbox.focus;
  const notice = pl.notice ? pl.open.find((it) => itemRef(it) === pl.notice) : null;
  if (!notice) pl.notice = '';
  const html = actionListHtml() + (notice ? itemHtml(notice) : '');
  return html || '<div class="pl-empty">没有等你的事</div>';
}

// actionListHtml is the cards of the items that wait on you to act, for the
// left column and the 会话 sidebar's bar.
export function actionListHtml() {
  return actionItems().map(itemHtml).join('');
}

// paintLeftToggle writes the phone's folded left block: 等你 N · 任务 M,
// the counts of what it holds (the badge's, and the tasks in progress).
function paintLeftToggle() {
  const el = $('pl-left-toggle')?.querySelector('span');
  if (!el) return;
  const text = '等你 ' + actionItems().length + ' · 任务 ' + pl.tasksActive;
  if (el.textContent !== text) el.textContent = text;
}

function noteItems() {
  nzBus.dispatchEvent(new CustomEvent('inbox:items'));
}

function paintWaiting() {
  const box = $('pl-wait');
  if (!box) return;
  box.innerHTML = waitListHtml();
  paintInlineCards(sortedOpen());
  paintLeftToggle();
  revealWaiting();
}

// revealWaiting brings the focused item into view (notify.js), unfolding
// the phone's left block when it is folded.
function revealWaiting() {
  if (!inbox.focus || ui.activeView !== 'planner' || !$('pl-wait')?.querySelector('.pl-card[data-id="' + CSS.escape(inbox.focus) + '"]')) return;
  showPlannerLeft(true);
  revealFocus($('pl-wait'));
}

// paintInlineCards repeats the Planner's open questions with options at the
// end of the conversation, where they were asked; the left column keeps them
// too, and either answers.
function paintInlineCards(items) {
  const box = $('pl-cards');
  if (!box) return;
  const mine = items.filter((it) => it.unit === PLANNER_KEY && (it.node || 'local') === plannerHost.node && it.decision);
  const html = mine.length ? '<div class="pl-cards-h">待你回答 · ' + mine.length + '</div>' + mine.map(itemHtml).join('') : '';
  if (box.innerHTML !== html) box.innerHTML = html;
  box.hidden = !html;
}

// fetchInbox lists the inbox of the node the Planner shown runs on: each
// node's Planner has its own 等你 (remote-planner.md §2), so another node's
// never mixes in. Items carry their node; a node offline keeps what it
// listed last.
export async function fetchInbox() {
  const host = plannerHost.node;
  const local = isLocalNode(host);
  if (!local && !hostOnline()) return;
  let res;
  try {
    res = await fetchJSON(plannerAPI(NZ_CONTRACT.API.inbox), { timeoutMs: 8000 });
  } catch { return; }
  if (host !== plannerHost.node) return;
  pl.open = ((res && Array.isArray(res.open)) ? res.open : []).map((it) => Object.assign({}, it, { node: host }));
  if (!local) notifyNewRemote(host, pl.open);
  pl.pausedUntil = res && res.paused_until ? Date.parse(res.paused_until) || 0 : 0;
  if (res && res.counts) {
    pl.seeding = true;
    try { onInboxCount(local ? res.counts : null, local ? null : res.counts); } finally { pl.seeding = false; }
  }
  if (pl.host) { paintWaiting(); paintTools(); }
  noteItems();
}

// remoteSeen is the Planner's node's open items already seen (by ref): this
// node pushes no frame for that node's items, so a new one that waits on you
// is notified from here (notify.js onNotify, in the background only). null
// until the first list, which notifies nothing.
let remoteSeen = null;
function notifyNewRemote(node, items) {
  const refs = new Set(items.map(itemRef));
  if (remoteSeen) {
    for (const it of items) {
      if (!remoteSeen.has(itemRef(it)) && KIND_LABEL[it.kind]) {
        onNotify({ title: it.title || '通知', body: it.why || '', id: itemRef(it), key: it.unit || '', node });
      }
    }
  }
  remoteSeen = refs;
}

// REMOTE_INBOX_MS: with the Planner on another node its inbox is polled, for
// the badge and the notifications this node's frames do not carry.
const REMOTE_INBOX_MS = 15000;

// ---- the Planner's node ----

// paintHost names the node the Planner runs on in the header, a switch
// when more than one runs one (remote-planner.md §2, §5): each node's Planner
// manages that node only, and the switch picks which one this page works.
function paintHost() {
  const el = $('pl-host');
  if (!el) return;
  const nodes = plannerNodes();
  let html = '';
  if (nodes.length > 1) {
    html = '<select class="pl-host-pick" data-action-change="planner-host" aria-label="看哪台机器的 Planner">' +
      nodes.map((n) => '<option value="' + escAttr(n) + '"' + (n === plannerHost.node ? ' selected' : '') + '>' +
        esc(n === 'local' ? '本机' + (sessionList.nodesData.local?.display_name && sessionList.nodesData.local.display_name !== 'Local' ? ' · ' + sessionList.nodesData.local.display_name : '') : (sessionList.nodesData[n]?.display_name || n)) + '</option>').join('') + '</select>';
  } else if (hostRemote()) {
    html = '<span class="pl-host-name" title="Planner 运行在这台机器上">' + esc(hostName()) + '</span>';
  }
  if (el.innerHTML !== html) el.innerHTML = html;
  el.hidden = !html;
}

// hostWasOnline is the Planner's node's link as last painted: coming back
// re-reads what the page shows.
let hostWasOnline = true;

// onHostChanged re-runs what the page shows for the Planner now shown.
function onHostChanged() {
  pl.status = null;
  pl.briefs = [];
  pl.notes = [];
  pl.open = [];
  remoteSeen = null;
  hostWasOnline = hostOnline();
  paintHost();
  const center = $('pl-center');
  if (pl.host && center?.parentElement) hostConversation(pl.host, center.parentElement);
  else fetchInbox();
  if (pl.host === 'page') refreshTasks();
  refreshCustody();
}

// ---- controls ----

// notifyToolHtml is 允许浏览器通知 while the browser has not asked yet; once
// allowed or refused there is nothing to do here.
function notifyToolHtml() {
  if (!notifySupported() || Notification.permission !== 'default') return '';
  return '<div class="pl-tool"><button type="button" class="pl-btn" data-action="planner-notify-perm">允许浏览器通知</button></div>';
}

// pausedAt is the pause's end as HH:mm, with the date (M/D) when not today.
function pausedAt() {
  const at = formatMonthDayTime(pl.pausedUntil);
  return new Date(pl.pausedUntil).toDateString() === new Date().toDateString() ? at.slice(at.indexOf(' ') + 1) : at;
}

// pauseToolHtml is the one pause (D94): pushes and custody together, for
// a while or until resumed.
function pauseToolHtml() {
  if (paused()) {
    return '<button type="button" class="pl-btn" data-action="planner-pause-resume">恢复</button>';
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

// paused reports whether pushes and custody are paused: PUT /api/inbox/pause
// pauses both; a custody pause from before they were one also counts.
const paused = () => pl.pausedUntil > Date.now() || (custody.paused && !!pl.status?.enabled);

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

// pausedText is the header's line while paused: until when, or (a custody
// pause from before the two were one) with no end.
function pausedText() {
  return pl.pausedUntil > Date.now() ? '已暂停到 ' + pausedAt() : '已暂停';
}

// paintTools fills the header's "⋯" menu and, while pushes and custody are
// paused, its 已暂停到 … · 恢复 line.
function paintTools() {
  const line = $('pl-paused');
  if (line) {
    const on = paused();
    const html = on ? '<span>' + esc(pausedText()) + '</span>' +
      '<span aria-hidden="true">·</span><button type="button" class="pl-link" data-action="planner-pause-resume">恢复</button>' : '';
    if (line.innerHTML !== html) line.innerHTML = html;
    line.title = on ? '推送与托管已暂停' + (pl.pausedUntil > Date.now() ? '，到 ' + formatAbsTime(pl.pausedUntil) : '') : '';
    line.hidden = !on;
  }
  const box = $('pl-tools');
  if (!box) return;
  // Repainting under the custom-pause input would drop what is typed; only
  // the input holding focus counts (Chromium focuses the clicked 自定义).
  if (pl.customPause && document.activeElement?.id === 'pl-pause-hours') return;
  box.innerHTML = notifyToolHtml() +
    '<div class="pl-tool" title="暂停期间不推送，也不替任何会话回话；正在跑的回合不受影响"><span class="pl-tool-l">暂停推送与托管</span><div class="pl-tool-btns">' + pauseToolHtml() + '</div></div>' +
    '<div class="pl-tool">' + stopToolHtml() + '</div>';
}

async function pause(forVal) {
  try {
    const put = (url) => fetchJSON(url, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ for: forVal }), timeoutMs: 8000,
    });
    const res = await put(plannerAPI(NZ_CONTRACT.API.inbox_pause));
    pl.pausedUntil = res && res.paused_until ? Date.parse(res.paused_until) || 0 : 0;
    pl.customPause = false;
    closeMenu();
  } catch (e) {
    showToast('暂停失败：' + e.message, 'error');
  }
  paintTools();
  // The gateway paused custody with the pushes: the chips follow.
  refreshCustody();
}

// resume ends the pause of pushes and custody; a custody pause from before
// the two were one is ended too.
async function resume() {
  try {
    await fetchJSON(plannerAPI(NZ_CONTRACT.API.inbox_pause), { method: 'DELETE', timeoutMs: 8000 });
    pl.pausedUntil = 0;
    closeMenu();
  } catch (e) {
    showToast('恢复失败：' + e.message, 'error');
  }
  await refreshCustody();
  if (custody.paused) await setCustodyPaused(false);
  paintTools();
}

// ---- center ----

function paintStatus() {
  const s = pl.status;
  const chip = $('pl-status');
  const note = $('pl-note');
  let text = '', cls = '';
  const away = hostRemote() && !hostOnline();
  if (away) { text = '离线'; cls = 'off'; } else if (s) {
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
    if (away) {
      html = '<b>' + esc(hostName()) + ' 离线。</b>Planner 和它派出的会话都在那台机器上，照常运行；连上后这里自动刷新。';
    } else if (s && !s.enabled) {
      html = '<b>Planner 没有开启。</b>在配置里设 <code>planner.enabled: true</code> 并重启 naozhi 后，这里是你和 Planner 的对话：早报、它派出的会话与汇报。' + left + '的"等你"照常可用。';
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
  const day = b.day || '';
  const open = pl.briefOpen.has(day) ? pl.briefOpen.get(day) : new Date(t).toDateString() === new Date().toDateString();
  const when = ' · ' + formatMonthDayTime(t);
  return '<details class="pl-brief' + (isLit(BRIEF_FOCUS + day) ? ' inbox-lit' : '') + '" data-day="' + escAttr(day) + '" data-time="' + t + '"' + (open ? ' open' : '') + '>' +
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

// revealBrief opens, scrolls to and highlights the brief a push or link
// asked for (inbox.focus "brief:<day>"), once the stream shows it: a first
// page in flight is about to repaint the stream, placeBriefs reveals it then.
function revealBrief() {
  if (!inbox.focus.startsWith(BRIEF_FOCUS) || pl.host !== 'page' || pl.loading) return;
  const id = inbox.focus;
  const day = id.slice(BRIEF_FOCUS.length);
  const sel = '#pl-stream > .pl-brief[data-day="' + CSS.escape(day) + '"]';
  const el = document.querySelector(sel);
  if (!el) return;
  inbox.focus = '';
  el.open = true;
  pl.briefOpen.set(day, true);
  pl.revealed = day;
  el.scrollIntoView({ block: 'start' });
  light(el, sel, id);
}

// fetchBriefs takes the sent briefs; an unreadable list leaves the stream
// as it was.
async function fetchBriefs() {
  let res;
  try {
    res = await fetchJSON(plannerAPI(NZ_CONTRACT.API.planner_briefs), { timeoutMs: 8000 });
  } catch { return; }
  const list = res && Array.isArray(res.briefs) ? res.briefs : [];
  pl.briefs = list.filter((b) => b && b.sent && b.markdown);
  if (pl.host) placeBriefs();
}

async function fetchStatus() {
  const host = plannerHost.node;
  try {
    const s = await fetchJSON(plannerAPI(NZ_CONTRACT.API.planner), { timeoutMs: 8000 });
    if (host !== plannerHost.node) return;
    pl.status = s;
  } catch {
    if (hostRemote()) paintStatus();
    return;
  }
  notePlannerStatus(pl.status);
  const named = noteWorkspaces(pl.status && pl.status.workspaces);
  if (pl.host) { paintStatus(); paintTools(); paintStreamEmpty(); if (named) paintWaiting(); }
}

function paintStreamEmpty() {
  const box = $('pl-stream');
  if (!box) return;
  const empty = box.querySelector(':scope > .pl-empty');
  const hasEvents = !!box.querySelector('.event, .pl-brief');
  if (hasEvents || !(pl.status && pl.status.enabled)) { if (empty) empty.remove(); return; }
  if (!empty) box.insertAdjacentHTML('beforeend', '<div class="pl-empty">这里是你和 Planner 的对话：早报、它派出的会话与汇报。<span>直接说要做的事，或在会话里点"交给 Planner"。</span></div>');
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
// message to the Planner (planner/agent Send), or its one-tag stand-in
// <naozhi-snapshot unchanged since="HH:MM" at="…"/> when it says what the
// last one said; the bubble shows what you wrote.
// A session handed over with it (<naozhi-session-ref>) leads the message and
// becomes a fence that planner_cards.js turns into its card. The logged
// bubble keeps only the message's head (detail 2000 runes, summary 120), so
// either block may arrive cut.
const SNAPSHOT_RE = /^<naozhi-snapshot(?:\s[^>]*\/>|[\s>][\s\S]*?<\/naozhi-snapshot>)\s*/;
const SNAPSHOT_CUT_RE = /^<naozhi-snapshot[\s>][\s\S]*$/;
const SESSION_REF_RE = /^<naozhi-session-ref>(\{[^\n]*?\})<\/naozhi-session-ref>\s*/;
const SESSION_REF_CUT_RE = /^<naozhi-session-ref>[\s\S]*$/;
const GATEWAY_HEAD_RE = /^<naozhi-(?:snapshot[\s>]|session-ref>)/;
const IMAGES_ONLY_RE = /^\[\+\d+ image\(s\)\]$/;
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
    // The turn's images are yours: they stay on your bubble.
    if (p.type !== 'user') { delete p.uuid; delete p.images; delete p.image_paths; out.push(p); continue; }
    if (!mine) { mine = p; out.push(mine); continue; }
    mine.detail = [mine.detail, p.detail].filter(Boolean).join('\n\n');
    mine.summary = [mine.summary, p.summary].filter(Boolean).join(' ');
  }
  return out;
}

// CLI_NOISE are the CLI's own system lines (its API retries), which say
// nothing to you about the Planner's work.
const CLI_NOISE = new Set(['api_retry']);

// withoutSnapshot is e as the conversation shows it, null for a receipt, CLI
// noise or a permission the Planner's own tool was refused (nothing for you
// to do; a session's view still shows its own).
function withoutSnapshot(e) {
  if (e.type === 'user' && RECEIPT_RE.test(e.detail || e.summary || '')) return null;
  if (e.type === 'system' && CLI_NOISE.has(e.summary)) return null;
  if (e.type === 'permission_denied') return null;
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
  // Pictures with no text: the snapshot took the space before the summary's
  // " [+N image(s)]", which the bubble drops only after one.
  if (IMAGES_ONLY_RE.test(summary)) summary = '';
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
  // A first page shorter than the stream leaves nothing to scroll up by.
  if (reset && nearTop()) loadOlder();
}

// subscribe asks for planner:main's live events after the last one taken.
function subscribe() {
  const msg = { type: NZ_CONTRACT.WS.subscribe, key: PLANNER_KEY };
  if (hostRemote()) msg.node = plannerHost.node;
  pl.subNode = plannerHost.node;
  if (pl.lastTime) msg.after = pl.lastTime;
  else msg.limit = EVENTS_LIMIT;
  pl.sub = wsm.send(msg) ? 'pending' : '';
}

function unsubscribe() {
  if (pl.sub) {
    const msg = { type: NZ_CONTRACT.WS.unsubscribe, key: PLANNER_KEY };
    if (!isLocalNode(pl.subNode)) msg.node = pl.subNode;
    wsm.send(msg);
  }
  pl.sub = '';
}

// NOTE_SAME_MS: a note the history still holds (no restart since it was
// written) was logged within this of its journal line.
const NOTE_SAME_MS = 5000;

// withNotes merges the journaled notes into a page, as the system lines they
// were: those the history no longer holds, timed in [floor, ceil) (ceil 0:
// no end). A page's floor is its first event, older notes belonging to the
// pages before it, and 0 for the oldest page or an empty one (a restart left
// no history).
function withNotes(events, notes, floor, ceil) {
  const logged = events.filter((e) => e && e.type === 'system');
  const extra = [];
  for (const n of notes) {
    const t = Date.parse(n && n.time);
    if (!n || !n.text || isNaN(t) || t < floor || (ceil && t >= ceil)) continue;
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
  const notes = reset ? fetchJSON(plannerAPI(NZ_CONTRACT.API.planner_notes), { timeoutMs: 8000 }).catch(() => null) : null;
  let more = null;
  try {
    // No planner:main session yet (it never had a turn) is no history, and
    // the notes still show.
    const res = await fetchJSON(eventsURL(PLANNER_KEY, plannerHost.node, q),
      { timeoutMs: 8000, onResponse: (r) => { more = hasMoreHeader(r); } })
      .catch((e) => { if (reset && e && e.status === 404) return []; throw e; });
    let events = Array.isArray(res) ? res : [];
    const n = notes && await notes;
    if (gen === pl.gen && pl.host) {
      if (reset) {
        more = more ?? events.length >= EVENTS_LIMIT;
        pl.notes = n && Array.isArray(n.notes) ? n.notes : [];
        seedOlder(events, more);
        events = withNotes(events, pl.notes, more && events.length ? (events[0].time || 0) : 0, 0);
      }
      // The page is in: what it paints is no longer about to be replaced.
      pl.loading = false;
      appendEvents(events, reset);
    }
  } catch { /* the next tick retries */ } finally {
    if (gen === pl.gen) pl.loading = false;
  }
}

// ---- older pages ----

// OLDER_EDGE_PX: the stream scrolled to within this of its top loads the
// page before the oldest one shown, through the session view's load-earlier
// paging (GET /api/sessions/events?before=, event_stream.js's cursor).
const OLDER_EDGE_PX = 80;
const OLDER_RETRY_MS = 5000;

// seedOlder starts the cursor at a first page's head; more is whether the
// server holds older events.
function seedOlder(events, more) {
  const c = dedupEarlierPage(events, 0, []);
  pl.older = { ms: c.oldestMS, keys: c.seenKeys, more: !!(more && c.oldestMS), loading: false };
}

// nearTop reports whether the stream shows its top, or does not scroll.
function nearTop() {
  const scroller = $('pl-events');
  return !!scroller && scroller.offsetParent !== null && scroller.scrollTop < OLDER_EDGE_PX;
}

// loadOlder prepends the page before the cursor, with the journaled notes
// of its span, and goes on while the stream still shows its top.
async function loadOlder() {
  const o = pl.older;
  if (!o.more || o.loading || !pl.host || pl.loading || Date.now() < (o.retryAt || 0)) return;
  o.loading = true;
  const gen = pl.gen;
  const limit = EARLIER_PAGE_LIMIT + o.keys.length;
  let more = null;
  try {
    const res = await fetchJSON(eventsURL(PLANNER_KEY, plannerHost.node, '&before=' + (o.ms + 1) + '&limit=' + limit),
      { timeoutMs: 8000, onResponse: (r) => { more = hasMoreHeader(r); } });
    if (gen !== pl.gen || pl.older !== o || !pl.host) return;
    const raw = Array.isArray(res) ? res : [];
    const page = dedupEarlierPage(raw, o.ms, o.keys);
    const ceil = o.ms;
    o.ms = page.oldestMS;
    o.keys = page.seenKeys;
    // A page that brought nothing new ends the paging rather than asking
    // for the same one again.
    o.more = !!page.events.length && (more ?? raw.length >= limit);
    prependEvents(withNotes(page.events, pl.notes, o.more ? o.ms : 0, ceil));
  } catch {
    // Scrolling up again retries, not before OLDER_RETRY_MS.
    o.retryAt = Date.now() + OLDER_RETRY_MS;
    return;
  } finally { o.loading = false; }
  if (nearTop()) loadOlder();
}

// prependEvents puts an older page above the stream, the reader kept on
// what they were reading. The briefs placed at the top go back in by time,
// and a revealed one still in view is what the reader is reading.
function prependEvents(events) {
  const box = $('pl-stream');
  const scroller = $('pl-events');
  if (!box || !scroller || !events.length) return;
  const html = renderEventsWithDividers(events.flatMap(plannerEvents), 0, RENDER_OPTS);
  if (!html) return;
  const fromBottom = scroller.scrollHeight - scroller.scrollTop;
  const anchor = revealedTop();
  for (const c of [...box.children]) {
    if (c.matches('.event')) break;
    if (c.matches('.pl-brief')) c.remove();
  }
  const lead = leadingTimeDivider(box);
  const frag = document.createElement('div');
  frag.innerHTML = html;
  const newest = lastDividerTime(frag);
  box.prepend(...frag.childNodes);
  if (lead && newest && Number(lead.dataset.time || 0) - newest < EVENT_DIVIDER_GAP_MS) lead.remove();
  decorateEvents(box);
  placeBriefs();
  runPendingAsync();
  const moved = anchor !== null && revealedEl();
  if (moved) scroller.scrollTop += moved.getBoundingClientRect().top - anchor;
  else scroller.scrollTop = scroller.scrollHeight - fromBottom;
}

function revealedEl() {
  return pl.revealed ? $('pl-stream')?.querySelector(':scope > .pl-brief[data-day="' + CSS.escape(pl.revealed) + '"]') : null;
}

// revealedTop is where the revealed brief shows in the stream's viewport,
// null once it is out of view, which forgets it.
function revealedTop() {
  const r = revealedEl()?.getBoundingClientRect();
  const view = $('pl-events')?.getBoundingClientRect();
  if (r && view && r.bottom > view.top && r.top < view.bottom) return r.top;
  pl.revealed = '';
  return null;
}

// ---- the conversation and its hosts ----

function startConversation() {
  mountComposer();
  paintHost();
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
  followWorkbenchSubject();
}

// A changed count means an item opened or closed: refetch the list shown.
nzBus.addEventListener('inbox:count', (e) => {
  const n = e.detail.badge;
  const badge = $('abnav-planner-count');
  if (badge) { badge.textContent = n > 99 ? '99+' : String(n); badge.hidden = !n; }
  if (!pl.seeding && pl.host) fetchInbox();
});
// The phone's folded left block counts the tasks in progress.
nzBus.addEventListener('tasks:list', (e) => {
  const tasks = (e.detail && Array.isArray(e.detail.tasks)) ? e.detail.tasks : [];
  pl.tasksActive = tasks.filter(inProgress).length;
  paintLeftToggle();
});
// The health table names the units no list holds.
nzBus.addEventListener('planner:health', () => { if (pl.host) paintWaiting(); });
nzBus.addEventListener('custody:changed', () => { if (pl.host) paintTools(); });
// A push or link without an item lands on the conversation, even where the
// phone's left block was left unfolded or a session opened in its place.
nzBus.addEventListener('inbox:focus', () => {
  if (!inbox.focus && ui.activeView === 'planner') {
    showPlannerLeft(false);
    closePlannerSession(false);
  }
  // A notice the focus names is listed only once it is (waitListHtml).
  if (inbox.focus) paintWaiting();
  else revealWaiting();
  revealBrief();
});
// toggle does not bubble: a brief's fold is taken on the way down.
document.addEventListener('toggle', (e) => {
  const el = e.target;
  if (el.matches?.('#pl-stream > .pl-brief')) pl.briefOpen.set(el.dataset.day || '', el.open);
}, true);
// A message to the Planner is going out: its ack waits for a user bubble
// logged past what the stream has taken so far.
nzBus.addEventListener('planner:sent', () => { pl.ack = { after: pl.lastTime, mine: 0 }; });
// A session listed since a card named it gets its name and state; the
// nodes it brings may move the Planner, or bring its node back.
onSessionsApplied(() => {
  notePlannerNodes();
  paintHost();
  const online = hostOnline();
  if (online !== hostWasOnline) {
    hostWasOnline = online;
    paintStatus();
    notePlannerStatus(pl.status);
    if (online) onHostChanged();
  }
  if (pl.host) paintSessionCards($('pl-stream'));
});
nzBus.addEventListener('planner:node', onHostChanged);
readLocalPlanner();
setInterval(() => { if (hostRemote()) fetchInbox(); }, REMOTE_INBOX_MS);
// A new socket holds no subscriptions: re-subscribe after the last event
// taken, the history frame fills the gap.
wsm.onReady(() => {
  readLocalPlanner(true);
  fetchInbox();
  pl.sub = '';
  if (pl.timer) subscribe();
});

// planner:main's frames are all claimed, so the session stream's bookkeeping
// never sees them (a late ack would pose as its subscription); they count
// only while the page holds a subscription.
const ours = (msg) => msg.key === PLANNER_KEY && (msg.node || 'local') === (pl.subNode || 'local');
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
    const ref = el.dataset.id;
    const { node, id } = itemOf(ref);
    el.disabled = true;
    try {
      await fetchJSON(nodeAPI(node, NZ_CONTRACT.API.inbox_close), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }), timeoutMs: 8000 });
      pl.open = pl.open.filter((it) => itemRef(it) !== ref);
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
      const { node, id } = itemOf(el.dataset.id);
      await fetchJSON(nodeAPI(node, NZ_CONTRACT.API.inbox_decide), {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id, action: el.dataset.act }), timeoutMs: 15000,
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
  'planner-host': (el) => choosePlannerNode(el.value),
  'planner-pause': (el) => pause(el.dataset.for),
  'planner-pause-custom': () => { pl.customPause = true; paintTools(); $('pl-pause-hours')?.focus(); },
  'planner-pause-custom-cancel': () => { pl.customPause = false; paintTools(); },
  'planner-pause-custom-ok': () => {
    const h = Number($('pl-pause-hours')?.value);
    if (!(h >= 1 && h <= 168)) { showToast('填 1 到 168 小时', 'warning'); return; }
    pause(Math.round(h) + 'h');
  },
  'planner-pause-resume': () => resume(),
  'planner-stop': async (el) => {
    const stopped = el.checked;
    el.disabled = true;
    try {
      pl.status = await fetchJSON(plannerAPI(NZ_CONTRACT.API.planner_state), {
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
  initPlannerResize();
  $('pl-events')?.addEventListener('scroll', () => {
    if (pl.revealed) revealedTop();
    if (nearTop()) loadOlder();
  }, { passive: true });
  $('pl-left-toggle')?.addEventListener('click', () => {
    showPlannerLeft(!$('planner-main')?.classList.contains('pl-left-open'));
  });
  // The "⋯" menu closes on a click outside it. The path is the one at
  // dispatch: a menu button repaints itself away before this runs.
  document.addEventListener('click', (e) => {
    const more = $('pl-more');
    if (more?.open && !e.composedPath().includes(more)) more.open = false;
  });
});
