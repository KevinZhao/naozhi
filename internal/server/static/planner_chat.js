// planner_chat.js — talking from the Planner page and handing work to it
// (docs/rfc/planner.md §10 D77, §10.1 输入 / 先回执 / 状态三层; §15.1 PR ⑬):
//
//   - the page's input box (#pl-composer) sends to the Planner through
//     POST /api/planner/send, the images and PDFs pasted, dropped or picked
//     into it going along as file_ids (composer_files.js's upload tray, with
//     the chat view's limits). The gateway adds the snapshot and pushes its
//     "收到，正在看" to the planner:main stream; that receipt (D63) shows as
//     the box's note until the answer comes (D91). With a session card
//     opened in the middle column it sends to that session (POST
//     /api/sessions/send).
//   - a session card opens in the middle column under the breadcrumb
//     "Planner › 会话名" (the only place it is named, besides the input box's
//     "→ 会话名"), with 停止 while it is a dispatched session in progress
//     (tasks.js's task-stop): its status line, then its milestones (the
//     bubbles the chat view shows), and 原始记录 adds the tool calls. Its
//     events are polled while the page is visible; the workbench follows it.
//   - 交给 Planner, beside the chat view's input, sends the session's
//     reference with what is typed and the attachments in that input. A
//     session opened here is already in front of the Planner, so its input
//     box has no 交给 Planner.
//   - 单独聊 (§15.14 ⑥, D117): the box's "发给 [Planner ▾]" set to 新对话
//     sends that one message to a new ordinary session (the sidebar's quick
//     session: session.cwd, the default agent and model; no snapshot, none
//     of the Planner's tools), labelled "单独聊：<its first sentence>", and
//     opens it here; the switch goes back to Planner. A ↗ 追问 on a Planner
//     reply becomes one too (aside_drawer.js, 转成单独聊). Either leaves a
//     line in the Planner conversation (POST /api/planner/aside, a 动态 that
//     opens it again; it never reaches planner:main's context) and is one
//     of 任务's 你开的. Its header has 交给 Planner (its card with a line you
//     add: a turn you started) and 结束 (the sidebar's 移除, so it goes to
//     the history); the title is renamed from the breadcrumb. A session is a
//     单独聊 by its label's "单独聊：".
//
// The switch (GET /api/planner) is read once the socket is up and whenever
// the Planner page reads it; with planner.enabled off the chat view's button
// is hidden, and stopped it is disabled.
// Layering: never import dashboard.js back; setActivityView is a shell slot.
import { NZ_CONTRACT } from './contract.js';
import { composer, selection, serverInfo, sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, isComposingKey, nzBus, registerActions, showToast } from './nz_util.js';
import { isDiscoveredKey, sid } from './session_ident.js';
import { onSessionsApplied } from './session_list.js';
import { eventHtml } from './event_render.js';
import { hydrateAskAnsweredFromHistory, lockRenderedAskCards } from './ask_card.js';
import { runPendingAsync } from './render_md.js';
import { ICONS } from './icons.js';
import { firstSentence, refreshHealth, sessionStatusHtml, sessionTitle } from './planner_cards.js';
import { closeTaskActs, dropTask, isRunningTask, refreshTasks, seenTask, taskOf } from './tasks.js';
import { followPlannerSubject, noteWorkbenchEvent } from './workbench_panes.js';
import { wsm } from './ws_manager.js';
import { shell } from './shell.js';
import { followPane, notePane, settlePane } from './scroll_follow.js';
import { dropSidebarRow, getMsgValue, isMobile, promptDialog, pushView, repaintSidebar, replaceView, restoreSidebarRow, setMsgValue, setPendingLabel } from './utilities.js';
import { awaitPendingOrients, clearFiles, createFileTray, filesBlockSend, handleFiles, renderFilePreviews } from './composer_files.js';
import { eventsURL, hostOnline, hostRemote, isLocalNode, nodeAPI, plannerAPI, plannerHost } from './planner_host.js';
import { quickSessionKey } from './auth_modal.js';
import { asideFromScratch, closePlannerScratch } from './aside_drawer.js';

const PLANNER_KEY = 'planner:main';
// SESS_LIMIT is the opened session's first page; SESS_KEEP the events kept
// to re-render when 原始记录 flips; SESS_POLL_MS its poll.
const SESS_LIMIT = 200;
const SESS_KEEP = 500;
const SESS_POLL_MS = 2000;
// NOTE_MS is how long the chat view's "已交给 Planner" note stays.
const NOTE_MS = 8000;
// ASIDE is a 单独聊's label prefix; ASIDE_TITLE_RUNES caps the title after it
// and LABEL_BYTES the whole label (sessionview.MaxUserLabelBytes). The label
// is what makes the session a 单独聊 after a reload, so a new session is
// waited for up to LABEL_WAIT_MS (the send answers before its spawn puts the
// key in the list, and a backend's handshake or a node can take a while),
// asking again after LABEL_RETRY_MS, doubled up to LABEL_RETRY_MAX_MS.
const ASIDE = '单独聊：';
const ASIDE_TITLE_RUNES = 30;
const LABEL_BYTES = 128;
const LABEL_WAIT_MS = 90000;
const LABEL_RETRY_MS = 500;
const LABEL_RETRY_MAX_MS = 5000;

const st = {
  status: null, // GET /api/planner; null until read
  statusLoading: false,
  sending: false,
  // acked: the gateway took a message to the Planner whose answer has not
  // come yet.
  acked: false,
  // sess is the session opened in the middle column, or null.
  sess: null,
  // to is where the box sends with no session opened: 'planner', or 'new'
  // for a 单独聊.
  to: 'planner',
  // asides are the 单独聊 this tab opened, key → title: one until the list
  // has its label.
  asides: new Map(),
  noteTimer: 0,
};

const $ = (id) => document.getElementById(id);

// tray is the input box's attachments; the box carries data-file-tray so
// the shared file actions (dashboard.js) reach it.
const TRAY = 'planner';
const tray = createFileTray(TRAY, 'pl-file-preview', 'pl-file-input');

// drafts keeps the unsent text and attachments of each target the one box
// left (a node's Planner, or a session by node and key), so they come back
// with it and never go to the next one.
const drafts = new Map();

// draftOf is the box's target as drafts keys it: the session opened, else
// the Planner shown (发给 新对话 is picked with the text typed, so it shares
// it). planner:main is the same key on every node, so the node goes in.
const draftKey = (node, key) => node + ' ' + key;
const draftOf = () => (st.sess ? draftKey(st.sess.node, st.sess.key) : draftKey(plannerHost.node, PLANNER_KEY));

// boxOf is the target the box's text was written for: once the node
// switched, draftOf already names the next one.
let boxOf = '';
const boxTarget = () => boxOf || draftOf();

// stashDraft moves the box's text and attachments into drafts, emptying it.
function stashDraft() {
  const input = $('pl-input');
  const text = input ? input.value : '';
  const files = tray.files;
  const k = boxTarget();
  if (text || files.length) drafts.set(k, { text, files });
  else drafts.delete(k);
  tray.files = [];
  if (input) { input.value = ''; fitInput(input); }
  renderFilePreviews(tray);
}

// restoreDraft puts the target's own draft back into the box.
function restoreDraft() {
  boxOf = draftOf();
  const d = drafts.get(boxOf);
  drafts.delete(boxOf);
  tray.files = d ? d.files : [];
  const input = $('pl-input');
  if (input) { input.value = d ? d.text : ''; fitInput(input); }
  renderFilePreviews(tray);
}

// dropDraftFiles takes files a send took out of every stashed draft: the
// box may have moved to another target while it was out.
function dropDraftFiles(files) {
  for (const [k, d] of drafts) {
    d.files = d.files.filter((f) => !files.includes(f));
    if (!d.text && !d.files.length) drafts.delete(k);
  }
}

// ---- the phone's back ----
//
// On a phone the session opened in the middle column and the unfolded left
// block each push a history entry (as mobileEnterChat does for the chat
// view), so the system back (Android's key, the browser's back, iOS's swipe)
// closes them as the breadcrumb does instead of leaving the dashboard. The
// block unfolded over an open session stacks on the session's entry (over),
// so folding it shows the session again; a session opened from the block
// takes the block's place.
const ENTRY_SESS = 'pl-sess';
const ENTRY_LEFT = 'pl-left';

function ownEntry() {
  const v = history.state && history.state.view;
  return v === ENTRY_SESS || v === ENTRY_LEFT ? v : '';
}

function pushEntry(view) {
  const cur = ownEntry();
  if (!isMobile() || cur === view) return;
  if (view === ENTRY_LEFT && cur === ENTRY_SESS) pushView({ view, over: true });
  // Back onto the session's entry, which the session opening now takes.
  else if (cur === ENTRY_LEFT && history.state.over) history.back();
  else if (cur) replaceView({ view });
  else pushView({ view });
}

// popEntry drops view's entry when the page closes it itself; leaving the
// page leaves it (a back then would land on whatever view comes next).
function popEntry(view) {
  if (isMobile() && ownEntry() === view && ui.activeView === 'planner') history.back();
}

window.addEventListener('popstate', () => {
  if (!isMobile()) return;
  const v = ownEntry();
  if (st.sess && v !== ENTRY_SESS) closePlannerSession(false, true);
  if (v !== ENTRY_LEFT) showPlannerLeft(false, true);
});

// apiError is the user-facing text of a failed fetchJSON: the JSON error a
// handler wrote, else the status line.
function apiError(e) {
  const m = /^HTTP \d+: ([\s\S]*)$/.exec(e && e.message || '');
  if (!m) return (e && e.message) || '';
  try { const j = JSON.parse(m[1]); if (j && j.error) return j.error; } catch { /* plain text */ }
  return m[1].trim() || e.message;
}

// filesConsumed is a refusal that took the uploads already (files_consumed):
// their IDs are gone, so the attachments must be added again.
function filesConsumed(e) {
  const m = /^HTTP \d+: ([\s\S]*)$/.exec(e && e.message || '');
  try { return !!(m && JSON.parse(m[1]).files_consumed); } catch { return false; }
}

function plannerUsable() {
  return !!(st.status && st.status.enabled && !st.status.stopped) && hostOnline();
}

// ---- the switch ----

// notePlannerStatus takes GET /api/planner's answer (planner_view.js reads
// it too) and repaints what depends on it.
export function notePlannerStatus(s) {
  if (s) st.status = s;
  paintComposer();
  paintHandoffButton();
}

async function fetchPlannerStatus() {
  if (st.statusLoading) return;
  st.statusLoading = true;
  try {
    st.status = await fetchJSON(plannerAPI(NZ_CONTRACT.API.planner), { timeoutMs: 8000 });
  } catch {
    // An unreadable switch hides the button until the next connect.
    st.status = st.status || { enabled: false };
  } finally {
    st.statusLoading = false;
  }
  notePlannerStatus();
}

async function sendToPlanner(text, ref, fileIDs) {
  const body = { text };
  if (ref) body.ref = ref;
  if (fileIDs && fileIDs.length) body.file_ids = fileIDs;
  // Before the POST: the stream's watermark now is what the answer follows.
  nzBus.dispatchEvent(new CustomEvent('planner:sent'));
  await fetchJSON(plannerAPI(NZ_CONTRACT.API.planner_send), {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 15000,
  });
}

// ---- pending bubbles ----

// addPending shows what was just sent until the logged user event replaces
// it (clearPending, from the stream that takes it).
function addPending(box, text) {
  if (!box || !text) return;
  box.querySelector(':scope > .pl-empty')?.remove();
  box.insertAdjacentHTML('beforeend', '<div class="event user pl-pending"><span class="event-icon">' + ICONS.user + '</span>' +
    '<div class="event-content">' + esc(text) + '</div></div>');
  followPane(box.closest('.pl-events'));
}

export function clearPending(box) {
  box?.querySelector('.pl-pending')?.remove();
}

// noteAck shows the gateway's receipt for a message to the Planner by the
// input box; clearAck takes it away once the answer to that message (or a
// failure) comes, which planner_view.js tells from the stream.
export function noteAck() {
  st.acked = true;
  paintComposer();
}

export function clearAck() {
  if (!st.acked) return;
  st.acked = false;
  paintComposer();
}

// ---- the Planner page's input box ----

function composerTarget() {
  return st.sess ? st.sess.key : PLANNER_KEY;
}

// putAway reports whether key is a done task's session that was put away
// (planner/reap): out of the session list, its record kept, which the pane
// still shows; 重新打开 puts it back (D136).
function putAway(key) {
  const t = taskOf(key);
  return !!(t && t.reap);
}

// sessionListed reports whether key is listed on the Planner's node, the
// node every session it names runs on.
function sessionListed(key) {
  return st.asides.has(key) || !!sessionList.sessionsData[sid(key, plannerHost.node)];
}

// titleOf names a session for the breadcrumb and the box: a 单独聊 this tab
// just opened by its title until its label is set.
function titleOf(key) {
  const row = sessionList.sessionsData[sid(key, plannerHost.node)];
  if (st.asides.has(key) && !(row && row.user_label)) return ASIDE + st.asides.get(key);
  if (!row && st.sess && st.sess.key === key && st.sess.title) return st.sess.title;
  return sessionTitle(key);
}

// endedAside reports whether the session opened is a 单独聊 that ended: its
// line opened it, and the list no longer has it (结束 put it in the history).
function endedAside() {
  return !!(st.sess && st.sess.title && !sessionListed(st.sess.key));
}

// mountComposer fills #pl-composer once.
export function mountComposer() {
  const box = $('pl-composer');
  if (!box || box.dataset.mounted) return;
  box.dataset.mounted = '1';
  box.dataset.fileTray = TRAY;
  boxOf = draftOf();
  box.innerHTML =
    '<div class="pl-target" id="pl-target" hidden><span class="pl-target-to" id="pl-target-to"></span>' +
      '<span class="pl-target-note" id="pl-target-note"></span></div>' +
    '<div class="file-preview" id="pl-file-preview"></div>' +
    '<input type="file" id="pl-file-input" accept="image/*,application/pdf" multiple class="nz-hidden" data-action-change="file-input-change">' +
    '<div class="pl-input-row">' +
      '<label class="pl-to" id="pl-to-wrap" title="新对话开一个单独聊：普通会话，不进 Planner 的上下文"><span class="pl-to-k">发给</span>' +
        '<select id="pl-to" aria-label="发给"><option value="planner">Planner</option><option value="new">新对话</option></select></label>' +
      '<button type="button" class="btn-icon" id="pl-attach" data-action="file-picker" title="上传图片或 PDF" aria-label="上传图片或 PDF">' + ICONS.attach + '</button>' +
      '<textarea id="pl-input" rows="1" aria-label="消息输入框"></textarea>' +
      '<button type="button" class="btn-icon btn-send" id="pl-send" data-action="planner-composer-send" title="发送" aria-label="发送消息">' + ICONS.send + '</button>' +
    '</div>';
  box.hidden = false;
  const input = $('pl-input');
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !isComposingKey(e)) { e.preventDefault(); sendFromComposer(); }
  });
  input.addEventListener('input', () => fitInput(input));
  $('pl-to').addEventListener('change', (e) => {
    st.to = e.target.value === 'new' ? 'new' : 'planner';
    paintComposer();
    input.focus();
  });
  // Dropped files join the box's attachments, as in the chat view's input.
  box.addEventListener('dragover', (e) => { e.preventDefault(); box.classList.add('pl-drop'); });
  box.addEventListener('dragleave', () => box.classList.remove('pl-drop'));
  box.addEventListener('drop', (e) => {
    e.preventDefault();
    box.classList.remove('pl-drop');
    // #pl-attach says whether this box takes files: not to another node,
    // which cannot read the uploads kept here (remote-planner.md §7).
    if (!$('pl-attach').disabled) handleFiles(e.dataTransfer.files, tray);
    else if (!input.disabled && e.dataTransfer.files.length) showToast('发给别的节点的消息不能带附件', 'warning');
  });
  paintComposer();
}

function fitInput(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 160) + 'px';
}

function paintComposer() {
  const to = $('pl-target-to');
  const input = $('pl-input');
  const send = $('pl-send');
  if (!to || !input || !send) return;
  const note = $('pl-target-note');
  // The row above the input takes room only while it says something.
  const row = () => { const r = $('pl-target'); if (r) r.hidden = !to.textContent && !(note && note.textContent); };
  // "发给" picks the target only with no session opened.
  const pick = $('pl-to');
  const wrap = $('pl-to-wrap');
  if (wrap) wrap.hidden = !!st.sess;
  if (pick) pick.value = st.to;
  if (st.sess) {
    // A card may name a session that does not exist (yet): a send would
    // create it as an ordinary session under that key.
    const listed = sessionListed(st.sess.key);
    to.textContent = '→ ' + titleOf(st.sess.key);
    if (note) note.textContent = listed ? '' : endedAside() ? '已结束，在历史里' : putAway(st.sess.key) ? '已收起，记录还在；要接着做先点上面的“重新打开”' : '不在会话列表里，不能发消息';
    input.placeholder = listed ? '追问或调整方向…' : '';
    input.disabled = !listed;
    $('pl-attach').disabled = input.disabled || hostRemote();
    send.disabled = st.sending || !listed;
    row();
    return;
  }
  // The Planner's own page needs no "→ Planner".
  to.textContent = '';
  if (st.to === 'new') {
    // A 单独聊 does not need the Planner running, only its page (the line
    // it leaves) and the node.
    const ok = !!(st.status && st.status.enabled) && hostOnline();
    // Offline is said once, by the header and the note over the stream.
    if (note) note.textContent = !hostOnline() ? '' : st.sending ? '发送中…' : '开一个单独聊：普通会话，不进 Planner 的上下文';
    input.placeholder = ok ? '问点别的…' : '';
    input.disabled = !ok;
    $('pl-attach').disabled = input.disabled || hostRemote();
    if (pick) pick.disabled = st.sending;
    send.disabled = st.sending || !ok;
    row();
    return;
  }
  if (pick) pick.disabled = st.sending || !hostOnline() || !(st.status && st.status.enabled);
  const usable = plannerUsable();
  // Offline, off and stopped are said once, by the header's chip and the
  // note over the stream (paintStatus); the input is disabled besides.
  if (note) note.textContent = !hostOnline() || !st.status || !st.status.enabled || st.status.stopped ? '' : st.sending ? '发送中…' : st.acked ? '收到，正在看' : '';
  input.placeholder = usable || !st.status ? '交代、追问…' : '';
  input.disabled = (!!st.status || !hostOnline()) && !usable;
  // The uploads stay in this node's store, which the Planner's node cannot
  // read (remote-planner.md §7).
  $('pl-attach').disabled = input.disabled || hostRemote();
  send.disabled = st.sending || (!!st.status && !usable);
  row();
}

async function sendFromComposer() {
  const input = $('pl-input');
  if (!input || st.sending) return;
  const text = input.value.trim();
  if (!text && !tray.files.length) return;
  const aside = !st.sess && st.to === 'new';
  const key = aside ? '' : composerTarget();
  if (key && key !== PLANNER_KEY && !sessionListed(key)) return;
  if (filesBlockSend(tray)) return;
  st.sending = true;
  paintComposer();
  const box = aside ? null : key === PLANNER_KEY ? $('pl-stream') : $('pl-sess-stream');
  const target = boxTarget();
  addPending(box, text);
  input.value = '';
  fitInput(input);
  try {
    // An auto-orient still running lands before the send takes the upload.
    await awaitPendingOrients(tray);
    const files = tray.files.slice();
    const fileIDs = files.map((f) => f.id).filter(Boolean);
    try {
      if (aside) {
        await startAside(text, fileIDs);
      } else if (key === PLANNER_KEY) {
        await sendToPlanner(text, '', fileIDs);
        st.acked = true;
      } else {
        const body = { key, text };
        if (hostRemote()) body.node = plannerHost.node;
        if (fileIDs.length) body.file_ids = fileIDs;
        await fetchJSON(NZ_CONTRACT.API.sessions_send, {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 15000,
        });
        if (st.sess && st.sess.key === key) loadSession(false);
      }
      clearFiles(tray, files);
      dropDraftFiles(files);
    } catch (e) {
      if (filesConsumed(e)) {
        clearFiles(tray, files);
        dropDraftFiles(files);
        showToast('附件已失效，请重新添加后再发送', 'warning');
      }
      throw e;
    }
  } catch (e) {
    clearPending(box);
    // The text goes back to the target it was written for: the box may show
    // another one by now.
    if (boxTarget() === target) {
      if (!input.value) { input.value = text; fitInput(input); }
    } else if (text) {
      const d = drafts.get(target);
      if (!d) drafts.set(target, { text, files: [] });
      else if (!d.text) d.text = text;
    }
    showToast('没有发出：' + apiError(e), 'error');
  } finally {
    st.sending = false;
    paintComposer();
    input.focus();
  }
}

// ---- a session opened in the middle column ----

function sessEvents(evs) {
  const p = st.sess;
  if (!p) return [];
  const fresh = [];
  for (const e of evs) {
    if (!e) continue;
    if (e.uuid) {
      if (p.uuids.has(e.uuid)) continue;
      p.uuids.add(e.uuid);
    } else if ((e.time || 0) <= p.lastTime) continue;
    if ((e.time || 0) > p.lastTime) p.lastTime = e.time;
    fresh.push(e);
  }
  p.events.push(...fresh);
  if (p.events.length > SESS_KEEP) p.events.splice(0, p.events.length - SESS_KEEP);
  return fresh;
}

function sessHtml(events) {
  const p = st.sess;
  const opts = { key: p.key, includeInternal: p.raw };
  return events.map((e) => eventHtml(e, opts)).join('');
}

function paintSessionPane(reset, fresh) {
  const p = st.sess;
  const box = $('pl-sess-stream');
  const scroller = $('pl-sess-events');
  if (!p || !box || !scroller) return;
  notePane(scroller);
  // An AskUserQuestion followed by a user event was answered: its card
  // renders locked (answering it posts to #pl-sess's key, ask_card.js).
  hydrateAskAnsweredFromHistory(p.events);
  if (p.pending && p.events.some((e) => e.type === 'user')) p.pending = '';
  if (reset) {
    box.innerHTML = sessHtml(p.events) || (p.pending ? '' : '<div class="pl-empty">还没有记录</div>');
    // A 单独聊 just started: its first message until the log has it.
    if (p.pending) addPending(box, p.pending);
  } else if (fresh.length) {
    const html = sessHtml(fresh);
    if (html) {
      box.querySelector(':scope > .pl-empty')?.remove();
      if (fresh.some((e) => e.type === 'user')) {
        clearPending(box);
        lockRenderedAskCards(box);
      }
      box.insertAdjacentHTML('beforeend', html);
    }
    fresh.forEach(noteWorkbenchEvent);
  }
  runPendingAsync();
  paintSessionStatus();
  // Opened at the newest; after that the pane follows until its reader
  // scrolls up (scroll_follow.js).
  if (reset) followPane(scroller);
  else settlePane(scroller);
}

function paintSessionStatus() {
  const p = st.sess;
  if (!p) return;
  const name = $('pl-sess-name');
  if (name) name.textContent = titleOf(p.key);
  const line = $('pl-sess-line');
  if (line) {
    // The breadcrumb names it. A session put away says so, with the way
    // back: its status would read as one that is simply gone.
    const html = putAway(p.key)
      ? '<span class="pl-sess-away">已收起</span><span class="pl-sess-away-why">做完后收进了历史，记录都在</span>' +
        '<button type="button" class="pl-btn" data-action="task-reopen" data-id="' + escAttr(p.key) + '" title="放回会话列表，下一条消息接着上下文继续">重新打开</button>'
      : sessionStatusHtml(p.key, false);
    if (line.innerHTML !== html) line.innerHTML = html;
  }
  paintStop();
  paintAsideActs();
  const raw = $('pl-sess-raw');
  if (raw) {
    raw.setAttribute('aria-pressed', p.raw ? 'true' : 'false');
    raw.textContent = p.raw ? '只看里程碑' : '原始记录';
  }
  paintComposer();
}

// paintStop shows the header's 停止 while the opened session is a dispatched
// one in progress, as 任务 last read it.
function paintStop() {
  const btn = $('pl-sess-stop');
  if (!btn) return;
  const key = st.sess ? st.sess.key : '';
  btn.dataset.id = key;
  btn.hidden = !key || !isRunningTask(key);
}

async function loadSession(reset) {
  const p = st.sess;
  if (!p || (p.loading && !reset)) return;
  if (reset) p.gen++;
  const gen = p.gen;
  p.loading = true;
  const q = reset || !p.lastTime ? '&limit=' + SESS_LIMIT : '&after=' + p.lastTime;
  try {
    const evs = await fetchJSON(eventsURL(p.key, p.node, q), { timeoutMs: 8000 });
    if (st.sess !== p || gen !== p.gen) return;
    const fresh = sessEvents(Array.isArray(evs) ? evs : []);
    paintSessionPane(reset, fresh);
  } catch {
    // The next tick retries. A first read that fails says so: "正在加载…"
    // left standing reads as a session that cannot be found.
    if (st.sess === p && gen === p.gen && !p.events.length) {
      const box = $('pl-sess-stream');
      if (box && box.querySelector(':scope > .pl-empty') && !p.pending) {
        box.innerHTML = '<div class="pl-empty">读不到这个会话的记录<span>它不在会话列表里了；在“任务”里重新打开，或到历史里找它</span></div>';
      }
    }
  } finally {
    if (st.sess === p && gen === p.gen) p.loading = false;
  }
}

// showPlannerLeft unfolds (open) or folds the phone's left block, which
// unfolded hides the middle column. keepEntry leaves the history entry to
// the caller: a back that folded it, or a session taking its place.
export function showPlannerLeft(open, keepEntry) {
  const main = $('planner-main');
  if (!main || main.classList.contains('pl-left-open') === open) return;
  main.classList.toggle('pl-left-open', open);
  $('pl-left-toggle')?.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (keepEntry) return;
  if (open) pushEntry(ENTRY_LEFT);
  else popEntry(ENTRY_LEFT);
}

// openPlannerSession opens key in the middle column, folding the phone's left
// block a 任务 title was tapped in, and marks its new result read. pending
// is a message just sent to it, shown until its log has a user event.
// opts.raw opens it on its raw record, tool calls and all (D124: a card's
// 原始记录, its digest's 看原始记录), scrolled to the latest, also when it is
// open already. Every other opening starts on its milestones: the switch is
// not remembered, or a title clicked later would open raw too.
export function openPlannerSession(key, pending, opts) {
  if (!key || key === PLANNER_KEY) return;
  const raw = !!(opts && opts.raw);
  showPlannerLeft(false, true);
  pushEntry(ENTRY_SESS);
  // Opening it looks at its new result (§15.12 ④), also a remote node's,
  // which no viewing frame reaches.
  seenTask(key);
  if (st.sess && st.sess.key === key) {
    // Switched or on its raw record already, it lands on the latest.
    if (raw) {
      st.sess.raw = true;
      paintSessionPane(true, []);
    }
    return;
  }
  if (st.sess) closePlannerSession(true);
  else stashDraft();
  // title: what the 单独聊 line it was opened from calls it, for once the
  // list no longer does (ended).
  st.sess = { key, node: plannerHost.node, events: [], uuids: new Set(), lastTime: 0, raw, loading: false, gen: 0, timer: 0, pending: pending || '',
    title: (opts && opts.title) || '' };
  restoreDraft();
  $('planner-main')?.classList.add('pl-sess-open');
  const pane = $('pl-sess');
  if (pane) { pane.hidden = false; pane.dataset.key = key; pane.dataset.node = plannerHost.node; }
  const box = $('pl-sess-stream');
  if (box) box.innerHTML = '<div class="pl-empty">正在加载…</div>';
  if (pending) addPending(box, pending);
  paintSessionStatus();
  followPlannerSubject(key);
  loadSession(true);
  refreshHealth($('pl-sess-line')?.parentElement);
  st.sess.timer = setInterval(() => {
    if (document.hidden || ui.activeView !== 'planner') return;
    loadSession(false);
  }, SESS_POLL_MS);
}

// closePlannerSession goes back to the Planner conversation; quiet keeps the
// workbench on its subject (another session is about to open, and takes its
// history entry and the box). fromBack: a back closed it, its entry is gone.
export function closePlannerSession(quiet, fromBack) {
  const p = st.sess;
  if (!p) return;
  clearInterval(p.timer);
  stashDraft();
  st.sess = null;
  if (!quiet) {
    restoreDraft();
    if (!fromBack) popEntry(ENTRY_SESS);
  }
  $('planner-main')?.classList.remove('pl-sess-open');
  const pane = $('pl-sess');
  if (pane) { pane.hidden = true; delete pane.dataset.key; delete pane.dataset.node; }
  const box = $('pl-sess-stream');
  if (box) box.innerHTML = '';
  paintStop();
  paintAsideActs();
  if (!quiet) {
    paintComposer();
    followPlannerSubject(PLANNER_KEY);
  }
}

// ---- 单独聊 ----

// isAside reports whether key, on the Planner's node, is a 单独聊: its label
// says so, or this tab just opened it.
function isAside(key) {
  if (!key) return false;
  if (st.asides.has(key)) return true;
  const row = sessionList.sessionsData[sid(key, plannerHost.node)];
  return !!row && String(row.user_label || '').startsWith(ASIDE);
}

// asideTitle is a 单独聊's title from what it was opened with: its first
// sentence, cut to fit a label.
function asideTitle(text) {
  return fitTitle(firstSentence(String(text || '').replace(/\s+/g, ' ').trim())) || '新对话';
}

// fitTitle cuts title to ASIDE_TITLE_RUNES and, with the prefix, to a
// label's LABEL_BYTES, marking a cut with "…".
function fitTitle(title) {
  const chars = [...title];
  let t = chars.length > ASIDE_TITLE_RUNES ? chars.slice(0, ASIDE_TITLE_RUNES).join('') + '…' : title;
  const enc = new TextEncoder();
  while (t && enc.encode(ASIDE + t).length > LABEL_BYTES) t = [...t].slice(0, -2).join('') + '…';
  return t;
}

// labelAside names key's session "单独聊：<title>", asking again while the
// session is not there yet (404) until LABEL_WAIT_MS has passed. The title
// shows at once, its sidebar card's too, and is kept over the polls that
// answer before the PATCH, a new session's first listing included
// (setPendingLabel); a label that did not go puts the one before back.
async function labelAside(key, node, title) {
  const body = { key, label: ASIDE + title };
  if (!isLocalNode(node)) body.node = node;
  const settle = setPendingLabel(key, node, body.label);
  const repaint = () => { if (st.sess && st.sess.key === key) paintSessionStatus(); };
  repaint();
  const until = Date.now() + LABEL_WAIT_MS;
  for (let wait = LABEL_RETRY_MS; ; wait = Math.min(wait * 2, LABEL_RETRY_MAX_MS)) {
    try {
      await fetchJSON(NZ_CONTRACT.API.sessions_label, {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 8000,
      });
      settle(true);
      repaint();
      return true;
    } catch (e) {
      if (!e || e.status !== 404 || Date.now() + wait > until) {
        settle(false);
        repaint();
        return false;
      }
      await new Promise((r) => setTimeout(r, wait));
    }
  }
}

// registerAside labels a new 单独聊 and leaves its line in the Planner
// conversation (on the Planner's node), then opens it here.
async function registerAside(key, title, pending) {
  const node = plannerHost.node;
  st.asides.set(key, title);
  sessionList.lastVersion = 0;
  shell.debouncedFetchSessions();
  // The Alt+P drawer has no middle column: there the 单独聊 would stack
  // under the Planner conversation and take its input box. It opens on the
  // page, as the drawer's session cards do.
  if (!$('planner-main')?.contains($('pl-center'))) shell.setActivityView('planner');
  openPlannerSession(key, pending);
  const [labeled, noted] = await Promise.all([
    labelAside(key, node, title),
    fetchJSON(plannerAPI(NZ_CONTRACT.API.planner_aside), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key, title }), timeoutMs: 8000,
    }).then(() => true, () => false),
  ]);
  if (!labeled) showToast('单独聊已开，但没能标上标题，刷新后它会显示成普通会话', 'warning');
  else if (!noted) showToast('单独聊已开，但没能记进 Planner 的动态', 'warning');
}

// startAside sends text (and fileIDs) to a new ordinary session, the
// sidebar's quick session: session.cwd, the default agent and model. The
// switch goes back to Planner once it went out.
async function startAside(text, fileIDs) {
  const local = isLocalNode(plannerHost.node);
  const workspace = local ? (serverInfo.defaultWorkspace || '') : '';
  const key = quickSessionKey(workspace);
  const body = { key, text };
  if (workspace) body.workspace = workspace;
  if (!local) body.node = plannerHost.node;
  if (fileIDs.length) body.file_ids = fileIDs;
  await fetchJSON(NZ_CONTRACT.API.sessions_send, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 15000,
  });
  st.to = 'planner';
  registerAside(key, asideTitle(text || (fileIDs.length ? '图片' : '')), text);
}

// paintAsideActs shows the opened 单独聊's 交给 Planner, 结束 and title edit.
function paintAsideActs() {
  const key = st.sess ? st.sess.key : '';
  const aside = isAside(key);
  for (const id of ['pl-sess-hand', 'pl-sess-end', 'pl-sess-rename']) {
    const b = $(id);
    if (b) { b.hidden = !aside; b.dataset.key = key; }
  }
  const hand = $('pl-sess-hand');
  if (hand) hand.disabled = !aside || st.sending || !plannerUsable() || !sessionListed(key);
  $('planner-main')?.classList.toggle('pl-aside-open', aside);
}

// renameAside edits the opened 单独聊's title, the label after "单独聊：".
async function renameAside() {
  const p = st.sess;
  if (!p || !isAside(p.key)) return;
  const row = sessionList.sessionsData[sid(p.key, p.node)] || {};
  const label = String(row.user_label || '');
  const current = label.startsWith(ASIDE) ? label.slice(ASIDE.length) : label;
  const next = await promptDialog({ title: '改标题', defaultValue: current, placeholder: '单独聊的标题', confirmText: '保存', maxLength: ASIDE_TITLE_RUNES });
  if (next === null) return;
  const title = fitTitle(next.replace(/\s+/g, ' ').trim());
  if (!title || title === current) return;
  if (!(await labelAside(p.key, p.node, title))) showToast('没有改成标题', 'error');
}

// handAside hands the opened 单独聊 to the Planner: its card and a line you
// add (none is fine), a turn you started; the page goes back to the
// conversation, where the answer comes.
async function handAside() {
  const p = st.sess;
  if (!p || !isAside(p.key) || st.sending) return;
  const line = await promptDialog({ title: '交给 Planner', message: '会附上这次单独聊，可以补一句话（可不填）', placeholder: '比如：记下结论', confirmText: '交给' });
  if (line === null || !st.sess || st.sess.key !== p.key) return;
  st.sending = true;
  paintAsideActs();
  try {
    await sendToPlanner(line.trim(), p.key, []);
    st.acked = true;
    closePlannerSession(false);
  } catch (e) {
    showToast('没有交给 Planner：' + apiError(e), 'error');
  } finally {
    st.sending = false;
    paintAsideActs();
    paintComposer();
  }
}

// endAside ends the opened 单独聊: put away to the history (the server's
// Router.Archive, as a done task is put away), not removed as the sidebar's
// 移除 does, so its images and question cards stay and the history reopens
// it under its key. The view closes and the sidebar card goes at once, as
// does its 你开的 card in 任务 (dropTask), kept off the lists while the
// request is in flight (optimisticDeleteKeys: the archive is synchronous, a
// poll answering before it still lists the key); what was typed there is
// dropped only once it ended. A refused end puts the cards back and opens the
// 单独聊 again, unless another session took the middle column meanwhile, and
// reads the list again: a request that failed late (a timeout, a lost relay)
// may have ended it all the same. 404 is a session no longer listed: ended.
async function endAside() {
  const p = st.sess;
  if (!p || !isAside(p.key) || p.ending) return;
  p.ending = true;
  const skey = sid(p.key, p.node);
  const title = st.asides.get(p.key);
  sessionList.optimisticDeleteKeys.add(skey);
  const row = dropSidebarRow(p.key, p.node);
  st.asides.delete(p.key);
  if (st.sess === p) closePlannerSession(false);
  repaintSidebar();
  const taskDone = plannerHost.node === p.node ? dropTask(p.key) : null;
  try {
    await fetchJSON(nodeAPI(p.node, NZ_CONTRACT.API.planner_aside_end), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key: p.key }), timeoutMs: 20000,
    });
  } catch (e) {
    if (!e || e.status !== 404) {
      sessionList.optimisticDeleteKeys.delete(skey);
      restoreSidebarRow(row);
      if (title !== undefined) st.asides.set(p.key, title);
      repaintSidebar();
      taskDone?.(false);
      if (!st.sess && plannerHost.node === p.node) openPlannerSession(p.key);
      showToast('没有结束单独聊：' + apiError(e), 'error');
      sessionList.lastVersion = 0;
      shell.debouncedFetchSessions();
      return;
    }
  }
  sessionList.optimisticDeleteKeys.delete(skey);
  // What was typed there has nowhere to go now.
  const dk = draftKey(p.node, p.key);
  drafts.get(dk)?.files.forEach((f) => { if (f.blobUrl) URL.revokeObjectURL(f.blobUrl); });
  drafts.delete(dk);
  sessionList.lastVersion = 0;
  shell.debouncedFetchSessions();
  // No frame says it was put away, and with no turn running 你开的 would
  // keep its card until the next read.
  if (taskDone) taskDone(true);
  else refreshTasks();
  showToast('单独聊已结束，移到了历史', 'success');
}

// fromScratch makes the ↗ 追问 on a Planner reply a 单独聊: the drawer's
// scratch, with its exchange, becomes the session. The scratch runs on this
// node, so only this node's Planner takes it.
async function fromScratch() {
  if (!isLocalNode(plannerHost.node)) return;
  const r = await asideFromScratch();
  if (!r) return;
  await registerAside(r.key, asideTitle(r.ask || r.quote), '');
}

// ---- 交给 Planner ----

// handoffKey is the chat view's session that can be handed over: one naozhi
// runs on the Planner's node, the only sessions it can read.
function handoffKey() {
  const key = selection.key;
  if (!key || isDiscoveredKey(key) || (selection.node || 'local') !== plannerHost.node || key.startsWith('planner:')) return '';
  return key;
}

// watchedInput is the #msg-input whose edits repaint #btn-handoff: typing,
// and the draft restores, sends and dictation that set it without an input
// event.
let watchedInput = null;

// paintHandoffButton shows #btn-handoff (renderMainShell's input row) for
// the selected session while planner.enabled is on; with nothing typed there
// is nothing to hand over, so it is disabled (and hidden on phone, by CSS on
// data-empty).
export function paintHandoffButton() {
  const btn = $('btn-handoff');
  if (!btn) return;
  const input = $('msg-input');
  if (input && input !== watchedInput) {
    watchedInput = input;
    new MutationObserver(paintHandoffButton).observe(input, { childList: true, characterData: true, subtree: true });
  }
  if (!st.status) { fetchPlannerStatus(); btn.hidden = true; return; }
  btn.hidden = !st.status.enabled;
  const key = handoffKey();
  const empty = !getMsgValue(input);
  btn.disabled = st.sending || !plannerUsable() || !key || empty;
  btn.toggleAttribute('data-empty', empty);
  btn.title = st.status.stopped ? '已停用 Planner（在 Planner 页面恢复）'
    : !key ? (isLocalNode(plannerHost.node) ? '只能交出本机由 naozhi 运行的会话' : '只能交出 Planner 所在机器上由 naozhi 运行的会话')
      : empty ? '先在输入框里写要交给 Planner 的事'
        : '把这个会话和输入框里的话一起交给 Planner';
}

function showHandoffNote() {
  const area = $('input-area');
  if (!area) return;
  let note = $('handoff-note');
  if (!note) {
    area.insertAdjacentHTML('afterbegin', '<div class="handoff-note" id="handoff-note" role="status"></div>');
    note = $('handoff-note');
  }
  note.innerHTML = '已交给 Planner <button type="button" class="pl-link" data-action="planner-goto">去 Planner 页面 ›</button>';
  clearTimeout(st.noteTimer);
  st.noteTimer = setTimeout(() => $('handoff-note')?.remove(), NOTE_MS);
}

// handoff sends key with text and the chat view's input's attachments.
async function handoff(key, text, done) {
  if (st.sending) return;
  if (filesBlockSend()) return;
  st.sending = true;
  paintHandoffButton();
  paintComposer();
  let files = [];
  try {
    await awaitPendingOrients();
    files = hostRemote() ? [] : composer.pendingFiles.slice();
    await sendToPlanner(text, key, files.map((f) => f.id).filter(Boolean));
    clearFiles(undefined, files);
    done();
  } catch (e) {
    if (filesConsumed(e)) {
      clearFiles(undefined, files);
      showToast('附件已失效，请重新添加后再发送', 'warning');
    }
    showToast('没有交给 Planner：' + apiError(e), 'error');
  } finally {
    st.sending = false;
    paintHandoffButton();
    paintComposer();
  }
}

registerActions({
  'planner-composer-send': () => sendFromComposer(),
  'planner-open-session': (el) => {
    const raw = el.dataset.raw === '1';
    // A card's "⋯" it was picked from has done its part.
    if (raw) closeTaskActs();
    openPlannerSession(el.dataset.key, '', { raw, title: el.dataset.title || '' });
  },
  'planner-close-session': () => closePlannerSession(false),
  'planner-aside-handoff': () => handAside(),
  'planner-aside-end': () => endAside(),
  'planner-aside-rename': () => renameAside(),
  'scratch-aside': () => fromScratch(),
  'planner-sess-raw': () => {
    const p = st.sess;
    if (!p) return;
    p.raw = !p.raw;
    paintSessionPane(true, []);
  },
  'planner-handoff': () => {
    const key = handoffKey();
    if (!key) return;
    const input = $('msg-input');
    const text = getMsgValue(input);
    handoff(key, text, () => {
      if (input && getMsgValue(input) === text) setMsgValue(input, '');
      showHandoffNote();
    });
  },
  // An example of the empty conversation fills the input box and sends
  // nothing: the text without its "…", the caret where it was.
  'planner-example': (el) => {
    const input = $('pl-input');
    if (!input || input.disabled) return;
    const raw = el.dataset.text || '';
    const at = raw.indexOf('…');
    input.value = raw.replace('…', '');
    fitInput(input);
    input.focus();
    const caret = at < 0 ? input.value.length : at;
    input.setSelectionRange(caret, caret);
  },
  'planner-goto': () => {
    $('handoff-note')?.remove();
    shell.setActivityView('planner');
  },
});

// A new socket may follow a restart that flipped planner.enabled.
wsm.onReady(fetchPlannerStatus);
// Another node's Planner: its switch, its sessions.
nzBus.addEventListener('planner:node', () => {
  st.status = null;
  st.to = 'planner';
  // The box goes with the Planner it was written for.
  if (st.sess) closePlannerSession(false);
  else { stashDraft(); restoreDraft(); }
  // An aside on the last node's Planner reply: its 转成单独聊 has no Planner here.
  closePlannerScratch();
  fetchPlannerStatus();
});
nzBus.addEventListener('session:select', () => paintHandoffButton());
// 重新打开 put it back: the status line and the input box follow.
nzBus.addEventListener('tasks:changed', () => { paintStop(); if (st.sess) paintSessionStatus(); });
// A 单独聊 opened elsewhere is one once the list has its label.
onSessionsApplied(paintAsideActs);
