// session_notes.js — the gateway's side notes in the session view
// (docs/rfc/planner.md §15.3 ③): the session's health entering 待你回复 /
// 停滞 / 失败 and recovering, its 通知 items opening and closing, the Planner
// dispatching it and its reports to the Planner. GET /api/sessions/notes
// lists them (read back from the Planner journal, so a restart keeps them)
// and session_note frames add the new ones. Shown to you only: nothing here
// reaches the CLI.
//
// A note is not a session event. It never enters event_stream.js or
// running_banner.js, so it starts no turn and moves no cursor: it is a
// .gw-note element (not .event) placed among the bubbles by time. An observer
// on #events-scroll places the notes again after a repaint, a page of earlier
// events or a trim; a note older than the first bubble waits while earlier
// events are still to load. data-note-id is a note's stable anchor and
// data-inbox-item its 通知 item: a notification's click scrolls to
// #main [data-inbox-item="<id>"] (notify.js, §15.3 ④).
import { NZ_CONTRACT } from './contract.js';
import { selection } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, nzViews, registerActions } from './nz_util.js';
import { wsm } from './ws_manager.js';
import { shell } from './shell.js';
import { inbox } from './notify.js';
import { HEALTH_ERROR_CLASSES, HEALTH_STATES } from './system_view.js';
import { formatTimeFull, formatTimeShort } from './utilities.js';

// REPORT_REFETCH_MS: a turn's report reaches the Planner after the turn
// ends, and reports are not pushed, so the list is fetched again this long
// after a result.
const REPORT_REFETCH_MS = 3000;

const ITEM_KINDS = { decision: '待你决定', question: '提问', error: '错误', notice: '' };
const BAD = new Set(['waiting', 'stalled', 'failed']);

const notes = {
  key: '', // the session the list is for
  list: [], // its notes, oldest first, each with ms
  gen: 0,
  el: null, // the #events-scroll observed
  observer: null,
  refetch: 0,
};

const stateLabel = (s) => (HEALTH_STATES[s] || {}).label || s;

// noteBody is a note's icon, tone and text.
function noteBody(n) {
  switch (n.kind) {
    case 'health': {
      if (!BAD.has(n.to)) return { icon: '✓', tone: 'ok', text: '已从「' + stateLabel(n.from) + '」恢复' };
      const cls = HEALTH_ERROR_CLASSES[n.error_class];
      return { icon: n.to === 'waiting' ? '?' : '⚠', tone: n.to === 'failed' ? 'bad' : 'warn', text: '进入「' + stateLabel(n.to) + '」' + (cls ? '（' + cls + '）' : '') };
    }
    case 'inbox': {
      // The title is gone once the inbox no longer keeps the item.
      if (n.event === 'open') return { icon: '◆', tone: 'inbox', text: ['通知', ITEM_KINDS[n.item_kind] ?? '一条', n.title].filter(Boolean).join(' · ') };
      return { icon: '◇', tone: 'ok', text: ['通知 · 已关闭' + (n.closed_by === 'auto' ? '（条件已消失）' : ''), n.title].filter(Boolean).join(' · ') };
    }
    case 'dispatch': return { icon: '↗', tone: 'planner', text: 'Planner 派出' };
    case 'report': return { icon: '↩', tone: 'planner', text: '已向 Planner 汇报' };
  }
  return null;
}

// closedItems are the items a close note in the list shows closed: an open
// note pushed before its close keeps no stale link.
function closedItems() {
  return new Set(notes.list.filter((n) => n.kind === 'inbox' && n.event === 'close').map((n) => n.item));
}

function noteHtml(n, closed) {
  const b = noteBody(n);
  if (!b) return '';
  const link = n.kind === 'inbox' && n.event === 'open' && n.open && !closed.has(n.item)
    ? ' <button type="button" class="gw-note-link" data-action="session-note-item" data-id="' + escAttr(n.item) + '">查看</button>'
    : '';
  return '<div class="gw-note gw-' + b.tone + '" role="note" data-note-id="' + escAttr(n.id) + '" data-note-time="' + n.ms + '"' +
    (n.item ? ' data-inbox-item="' + escAttr(n.item) + '"' : '') +
    ' title="' + escAttr(formatTimeFull(n.ms) + (n.evidence ? ' · ' + n.evidence : '')) + '">' +
    '<span class="gw-note-icon" aria-hidden="true">' + esc(b.icon) + '</span>' +
    '<span class="gw-note-text">' + esc(b.text) + '</span>' + link +
    '<span class="gw-note-time">' + esc(formatTimeShort(n.ms)) + '</span></div>';
}

const isNote = (node) => node.classList?.contains('gw-note');
const isDivider = (node) => node.classList?.contains('event-time-divider');

// placeNotes inserts the listed notes the pane does not show yet, each
// before the first bubble newer than it (and that bubble's divider).
function placeNotes() {
  const el = notes.el;
  if (!el || !el.isConnected || notes.key !== selection.key || !notes.list.length) return;
  // An agent drill-in owns the pane.
  if (nzViews.agent?.activeTaskID()) return;
  const shown = new Set([...el.querySelectorAll(':scope > .gw-note')].map((n) => n.dataset.noteId));
  const bubbles = [...el.querySelectorAll(':scope > .event[data-time]')];
  const first = bubbles.length ? Number(bubbles[0].dataset.time) : 0;
  const btn = document.getElementById('earlier-events-btn');
  const more = !!btn && el.contains(btn) && btn.dataset.state !== 'done';
  const closed = closedItems();
  for (const n of notes.list) {
    if (shown.has(n.id) || (more && (!first || n.ms < first))) continue;
    const html = noteHtml(n, closed);
    if (!html) continue;
    let ref = bubbles.find((b) => Number(b.dataset.time) > n.ms) || null;
    // Step back over the divider heading that bubble and the notes after n.
    while (ref) {
      const prev = ref.previousElementSibling;
      if (prev && (isDivider(prev) || (isNote(prev) && Number(prev.dataset.noteTime) > n.ms))) ref = prev;
      else break;
    }
    if (ref) ref.insertAdjacentHTML('beforebegin', html);
    else el.insertAdjacentHTML('beforeend', html);
    shown.add(n.id);
  }
}

// relink drops the 查看 link of the notes whose item a close note closed.
function relink() {
  const closed = closedItems();
  notes.el?.querySelectorAll('.gw-note-link').forEach((a) => { if (closed.has(a.dataset.id)) a.remove(); });
}

function addNotes(list) {
  const known = new Set(notes.list.map((n) => n.id));
  for (const n of list) {
    const ms = Date.parse(n.time);
    if (!n.id || known.has(n.id) || isNaN(ms)) continue;
    notes.list.push({ ...n, ms });
    known.add(n.id);
  }
  notes.list.sort((a, b) => a.ms - b.ms);
}

async function fetchNotes() {
  const key = notes.key;
  const gen = ++notes.gen;
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.sessions_notes + '?key=' + encodeURIComponent(key), { timeoutMs: 8000 });
    if (gen !== notes.gen || key !== notes.key) return;
    addNotes(Array.isArray(res?.notes) ? res.notes : []);
  } catch { return; }
  placeNotes();
  relink();
}

// startSessionNotes follows the session view's freshly built #events-scroll
// (renderMainShell): local sessions only, remote nodes keep no journal here.
export function startSessionNotes() {
  const el = document.getElementById('events-scroll');
  if (!el || el === notes.el) return;
  notes.observer?.disconnect();
  notes.el = el;
  const key = selection.node === 'local' || !selection.node ? selection.key || '' : '';
  if (key !== notes.key) notes.list = [];
  notes.key = key;
  if (!key) { notes.observer = null; return; }
  // Our own inserts call back too; the second pass finds nothing to add.
  notes.observer = new MutationObserver(placeNotes);
  notes.observer.observe(el, { childList: true });
  placeNotes();
  fetchNotes();
}

wsm.on(NZ_CONTRACT.WS.session_note, (msg) => {
  if (!msg.note || msg.key !== notes.key || !notes.el) return;
  const el = notes.el;
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  addNotes([msg.note]);
  placeNotes();
  relink();
  if (wasBottom) el.scrollTop = el.scrollHeight;
});

// Frames pushed while the socket was down are lost: list again on a new one.
wsm.onReady(() => {
  if (notes.key && notes.el?.isConnected) fetchNotes();
});

// A turn's end may carry a report to the Planner; fetch again shortly after.
nzBus.addEventListener('session:event', (e) => {
  if (e.detail?.ev?.type !== 'result' || !notes.key || notes.key !== selection.key) return;
  clearTimeout(notes.refetch);
  notes.refetch = setTimeout(fetchNotes, REPORT_REFETCH_MS);
});

// 查看 opens the Planner page on the item through the hand-off a
// notification without a session takes (§15.3 ④): inbox.focus names the
// item, 'inbox:focus' tells a list already painted.
registerActions({
  'session-note-item': (el) => {
    inbox.focus = el.dataset.id || '';
    shell.setActivityView('planner');
    nzBus.dispatchEvent(new CustomEvent('inbox:focus'));
  },
});
