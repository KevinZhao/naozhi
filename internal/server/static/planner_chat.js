// planner_chat.js — talking from the Planner page and handing work to it
// (docs/rfc/planner.md §10 D77, §10.1 输入 / 先回执 / 状态三层; §15.1 PR ⑬):
//
//   - the page's input box (#pl-composer) sends to the Planner through
//     POST /api/planner/send (the gateway adds the snapshot and pushes its
//     "收到，正在看" to the planner:main stream); that receipt (D63) shows as
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
//     reference with what is typed. A session opened here is already in
//     front of the Planner, so its input box has no 交给 Planner.
//
// The switch (GET /api/planner) is read once the socket is up and whenever
// the Planner page reads it; with planner.enabled off the chat view's button
// is hidden, and stopped it is disabled.
// Layering: never import dashboard.js back; setActivityView is a shell slot.
import { NZ_CONTRACT } from './contract.js';
import { selection, sessionList, ui } from './state.js';
import { esc, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { isDiscoveredKey, sid } from './session_ident.js';
import { eventHtml } from './event_render.js';
import { hydrateAskAnsweredFromHistory, lockRenderedAskCards } from './ask_card.js';
import { runPendingAsync } from './render_md.js';
import { ICONS } from './icons.js';
import { refreshHealth, sessionStatusHtml, sessionTitle } from './planner_cards.js';
import { isRunningTask } from './tasks.js';
import { followPlannerSubject, noteWorkbenchEvent } from './workbench_panes.js';
import { wsm } from './ws_manager.js';
import { shell } from './shell.js';
import { getMsgValue, setMsgValue } from './utilities.js';

const PLANNER_KEY = 'planner:main';
// SESS_LIMIT is the opened session's first page; SESS_KEEP the events kept
// to re-render when 原始记录 flips; SESS_POLL_MS its poll.
const SESS_LIMIT = 200;
const SESS_KEEP = 500;
const SESS_POLL_MS = 2000;
// NOTE_MS is how long the chat view's "已交给 Planner" note stays.
const NOTE_MS = 8000;

const st = {
  status: null, // GET /api/planner; null until read
  statusLoading: false,
  sending: false,
  // acked: the gateway took a message to the Planner whose answer has not
  // come yet.
  acked: false,
  // sess is the session opened in the middle column, or null.
  sess: null,
  noteTimer: 0,
};

const $ = (id) => document.getElementById(id);

// apiError is the user-facing text of a failed fetchJSON: the JSON error a
// handler wrote, else the status line.
function apiError(e) {
  const m = /^HTTP \d+: ([\s\S]*)$/.exec(e && e.message || '');
  if (!m) return (e && e.message) || '';
  try { const j = JSON.parse(m[1]); if (j && j.error) return j.error; } catch { /* plain text */ }
  return m[1].trim() || e.message;
}

function plannerUsable() {
  return !!(st.status && st.status.enabled && !st.status.stopped);
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
    st.status = await fetchJSON(NZ_CONTRACT.API.planner, { timeoutMs: 8000 });
  } catch {
    // An unreadable switch hides the button until the next connect.
    st.status = st.status || { enabled: false };
  } finally {
    st.statusLoading = false;
  }
  notePlannerStatus();
}

async function sendToPlanner(text, ref) {
  const body = { text };
  if (ref) body.ref = ref;
  // Before the POST: the stream's watermark now is what the answer follows.
  nzBus.dispatchEvent(new CustomEvent('planner:sent'));
  await fetchJSON(NZ_CONTRACT.API.planner_send, {
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
  const scroller = box.closest('.pl-events');
  if (scroller) scroller.scrollTop = scroller.scrollHeight;
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

function sessionListed(key) {
  return !!sessionList.sessionsData[sid(key, 'local')];
}

// mountComposer fills #pl-composer once.
export function mountComposer() {
  const box = $('pl-composer');
  if (!box || box.dataset.mounted) return;
  box.dataset.mounted = '1';
  box.innerHTML =
    '<div class="pl-target"><span class="pl-target-to" id="pl-target-to"></span>' +
      '<span class="pl-target-note" id="pl-target-note"></span></div>' +
    '<div class="pl-input-row">' +
      '<textarea id="pl-input" rows="1" aria-label="消息输入框"></textarea>' +
      '<button type="button" class="btn-icon btn-send" id="pl-send" data-action="planner-composer-send" title="发送" aria-label="发送消息">' + ICONS.send + '</button>' +
    '</div>';
  box.hidden = false;
  const input = $('pl-input');
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendFromComposer(); }
  });
  input.addEventListener('input', () => fitInput(input));
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
  if (st.sess) {
    // A card may name a session that does not exist (yet): a send would
    // create it as an ordinary session under that key.
    const listed = sessionListed(st.sess.key);
    to.textContent = '→ ' + sessionTitle(st.sess.key);
    if (note) note.textContent = listed ? '' : '不在会话列表里，不能发消息';
    input.placeholder = listed ? '追问或调整方向…' : '';
    input.disabled = !listed;
    send.disabled = st.sending || !listed;
    return;
  }
  // The Planner's own page needs no "→ Planner".
  to.textContent = '';
  const usable = plannerUsable();
  if (note) note.textContent = !st.status ? '' : !st.status.enabled ? '未开启 Planner' : st.status.stopped ? '已停用 Planner' : st.sending ? '发送中…' : st.acked ? '收到，正在看' : '';
  input.placeholder = usable || !st.status ? '交代、追问…' : '';
  input.disabled = !!st.status && !usable;
  send.disabled = st.sending || (!!st.status && !usable);
}

async function sendFromComposer() {
  const input = $('pl-input');
  if (!input || st.sending) return;
  const text = input.value.trim();
  if (!text) return;
  const key = composerTarget();
  if (key !== PLANNER_KEY && !sessionListed(key)) return;
  st.sending = true;
  paintComposer();
  const box = key === PLANNER_KEY ? $('pl-stream') : $('pl-sess-stream');
  addPending(box, text);
  input.value = '';
  fitInput(input);
  try {
    if (key === PLANNER_KEY) {
      await sendToPlanner(text);
      st.acked = true;
    } else {
      await fetchJSON(NZ_CONTRACT.API.sessions_send, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key, text }), timeoutMs: 15000,
      });
      if (st.sess && st.sess.key === key) loadSession(false);
    }
  } catch (e) {
    clearPending(box);
    if (!input.value) { input.value = text; fitInput(input); }
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
  const atBottom = reset || scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 40;
  // An AskUserQuestion followed by a user event was answered: its card
  // renders locked (answering it posts to #pl-sess's key, ask_card.js).
  hydrateAskAnsweredFromHistory(p.events);
  if (reset) {
    box.innerHTML = sessHtml(p.events) || '<div class="pl-empty">还没有记录</div>';
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
  if (atBottom) scroller.scrollTop = scroller.scrollHeight;
}

function paintSessionStatus() {
  const p = st.sess;
  if (!p) return;
  const name = $('pl-sess-name');
  if (name) name.textContent = sessionTitle(p.key);
  const line = $('pl-sess-line');
  if (line) {
    // The breadcrumb names it.
    const html = sessionStatusHtml(p.key, false);
    if (line.innerHTML !== html) line.innerHTML = html;
  }
  paintStop();
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
    const evs = await fetchJSON(NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(p.key) + q, { timeoutMs: 8000 });
    if (st.sess !== p || gen !== p.gen) return;
    const fresh = sessEvents(Array.isArray(evs) ? evs : []);
    paintSessionPane(reset, fresh);
  } catch { /* the next tick retries */ } finally {
    if (st.sess === p && gen === p.gen) p.loading = false;
  }
}

// openPlannerSession opens key in the middle column.
export function openPlannerSession(key) {
  if (!key || key === PLANNER_KEY) return;
  if (st.sess && st.sess.key === key) return;
  closePlannerSession(true);
  st.sess = { key, events: [], uuids: new Set(), lastTime: 0, raw: false, loading: false, gen: 0, timer: 0 };
  $('planner-main')?.classList.add('pl-sess-open');
  const pane = $('pl-sess');
  if (pane) { pane.hidden = false; pane.dataset.key = key; }
  const box = $('pl-sess-stream');
  if (box) box.innerHTML = '<div class="pl-empty">正在加载…</div>';
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
// workbench on its subject (another session is about to open).
export function closePlannerSession(quiet) {
  const p = st.sess;
  if (!p) return;
  clearInterval(p.timer);
  st.sess = null;
  $('planner-main')?.classList.remove('pl-sess-open');
  const pane = $('pl-sess');
  if (pane) { pane.hidden = true; delete pane.dataset.key; }
  const box = $('pl-sess-stream');
  if (box) box.innerHTML = '';
  paintStop();
  if (!quiet) {
    paintComposer();
    followPlannerSubject(PLANNER_KEY);
  }
}

// ---- 交给 Planner ----

// handoffKey is the chat view's session that can be handed over: a local
// one naozhi runs.
function handoffKey() {
  const key = selection.key;
  if (!key || isDiscoveredKey(key) || (selection.node && selection.node !== 'local') || key.startsWith('planner:')) return '';
  return key;
}

// paintHandoffButton shows #btn-handoff (renderMainShell's input row) for
// the selected session, and the header's #btn-pld (the Planner drawer's
// entry on phone) while planner.enabled is on.
export function paintHandoffButton() {
  const entry = $('btn-pld');
  if (entry) entry.hidden = !(st.status && st.status.enabled);
  const btn = $('btn-handoff');
  if (!btn) return;
  if (!st.status) { fetchPlannerStatus(); btn.hidden = true; return; }
  btn.hidden = !st.status.enabled;
  const key = handoffKey();
  btn.disabled = st.sending || !plannerUsable() || !key;
  btn.title = st.status.stopped ? '已停用 Planner（在 Planner 页面恢复）'
    : !key ? '只能交出本机由 naozhi 运行的会话'
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

async function handoff(key, text, done) {
  if (st.sending) return;
  st.sending = true;
  paintHandoffButton();
  paintComposer();
  try {
    await sendToPlanner(text, key);
    done();
  } catch (e) {
    showToast('没有交给 Planner：' + apiError(e), 'error');
  } finally {
    st.sending = false;
    paintHandoffButton();
    paintComposer();
  }
}

registerActions({
  'planner-composer-send': () => sendFromComposer(),
  'planner-open-session': (el) => openPlannerSession(el.dataset.key),
  'planner-close-session': () => closePlannerSession(false),
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
  'planner-goto': () => {
    $('handoff-note')?.remove();
    shell.setActivityView('planner');
  },
});

// A new socket may follow a restart that flipped planner.enabled.
wsm.onReady(fetchPlannerStatus);
nzBus.addEventListener('session:select', () => paintHandoffButton());
nzBus.addEventListener('tasks:changed', paintStop);
