// session_notes.js — the gateway's side notes in the session view
// (docs/rfc/planner.md §15.3 ③): the session's health entering 待你回复 /
// 停滞 / 失败 and recovering, its 通知 items opening and closing, the Planner
// dispatching it, its reports to the Planner and what custody did on it
// (switched, held back, stopped, a retry coming; a message it sent is its
// own bubble). GET /api/sessions/notes
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
import { HEALTH_STATES, esc, escAttr, evidenceCause, evidenceText, fetchJSON, nzBus, nzViews } from './nz_util.js';
import { wsm } from './ws_manager.js';
import { formatTimeFull, formatTimeShort } from './utilities.js';

// REPORT_REFETCH_MS: a turn's report reaches the Planner after the turn
// ends, and reports are not pushed, so the list is fetched again this long
// after a result.
const REPORT_REFETCH_MS = 3000;
// PAIR_MS: a health transition and the 通知 item it opened are journaled in
// one reconcile; within this of each other they are one event, one note.
const PAIR_MS = 2 * 60000;

const ITEM_KINDS = { decision: '待你决定', question: '提问', error: '错误', notice: '' };
const BAD = new Set(['waiting', 'stalled', 'failed']);
// ITEM_STATE is the health state whose entry opens an item of a kind
// (notify.HandleHealth); only those pair. The waiting and stalled items open
// minutes after their transition, so they stay notes of their own.
const ITEM_STATE = { error: 'failed' };

const notes = {
  key: '', // the session the list is for
  list: [], // its notes, oldest first, each with ms
  gen: 0,
  el: null, // the #events-scroll observed
  observer: null,
  refetch: 0,
};

const stateLabel = (s) => (HEALTH_STATES[s] || {}).label || s;

// CUSTODY_BY says who switched custody (custody.By*); the user's goes unsaid.
const CUSTODY_BY = { planner: '（Planner）', auto: '（自动）' };

// custodyBody is a custody note's icon, tone and text; a message custody
// sent shows as its bubble, not a note.
function custodyBody(n) {
  const icon = '⚙';
  switch (n.event) {
    case 'on': return { icon, tone: 'planner', text: '托管：已打开' + (CUSTODY_BY[n.by] || '') };
    case 'off': return { icon, tone: 'planner', text: '托管：已关闭' + (CUSTODY_BY[n.by] || '') };
    case 'deferred': return { icon, tone: 'planner', text: '托管：已暂缓（' + (n.evidence || '你在看') + '）' };
    case 'stopped': return { icon, tone: 'warn', text: '托管：已停' + (n.evidence ? ' · ' + n.evidence : '，等你接手') };
    case 'retry': return { icon, tone: 'planner', text: '托管：出错' + (n.evidence ? '（' + n.evidence + '）' : '') + '，稍后自动重试' + (n.n ? ' · 第 ' + n.n + ' 次' : '') };
  }
  return null;
}

// healthBody is a health note's icon, tone and text: the state entered and
// its cause, or the recovery.
function healthBody(n) {
  if (!BAD.has(n.to)) return { icon: '✓', tone: 'ok', text: '已从「' + stateLabel(n.from) + '」恢复' };
  const cause = evidenceCause(n.evidence, n.error_class);
  return { icon: n.to === 'waiting' ? '?' : '⚠', tone: n.to === 'failed' ? 'bad' : 'warn', text: '进入「' + stateLabel(n.to) + '」' + (cause ? '：' + cause : '') };
}

// noteBody is a note's icon, tone and text; an item's open note that pairs
// with the health note of the same event reads as that health note.
function noteBody(n, paired) {
  switch (n.kind) {
    case 'health': return healthBody(n);
    case 'inbox': {
      if (paired) return healthBody(paired);
      // The title is gone once the inbox no longer keeps the item.
      if (n.event === 'open') return { icon: '◆', tone: 'inbox', text: ['通知', ITEM_KINDS[n.item_kind] ?? '一条', n.title].filter(Boolean).join(' · ') };
      return { icon: '◇', tone: 'ok', text: ['通知 · 已关闭' + (n.closed_by === 'auto' ? '（条件已消失）' : ''), n.title].filter(Boolean).join(' · ') };
    }
    case 'dispatch': return { icon: '↗', tone: 'planner', text: 'Planner 派出' };
    case 'report': return { icon: '↩', tone: 'planner', text: '已向 Planner 汇报' };
    case 'custody': return custodyBody(n);
  }
  return null;
}

// pairs matches each item's open note with the health note of the same
// event (the health machine opens the item as it journals the transition),
// and custody's stop with the error item it opened as it stopped (one event,
// said by the stop): skip holds the notes not shown on their own, paired maps
// an open note to its health note and a stop to its item's open note.
function pairs() {
  const skip = new Set(), paired = new Map();
  for (const n of notes.list) {
    if (n.kind !== 'custody' || n.event !== 'stopped') continue;
    const o = notes.list.find((x) => x.kind === 'inbox' && x.event === 'open' && x.item_kind === 'error' && !skip.has(x.id) && Math.abs(x.ms - n.ms) <= PAIR_MS);
    if (o) { skip.add(o.id); paired.set(n.id, o); }
  }
  for (const n of notes.list) {
    const to = n.kind === 'inbox' && n.event === 'open' && !skip.has(n.id) && ITEM_STATE[n.item_kind];
    if (!to) continue;
    let h = null;
    for (const x of notes.list) {
      if (x.kind !== 'health' || x.to !== to || skip.has(x.id) || Math.abs(x.ms - n.ms) > PAIR_MS) continue;
      if (!h || Math.abs(x.ms - n.ms) < Math.abs(h.ms - n.ms)) h = x;
    }
    if (h) { skip.add(h.id); paired.set(n.id, h); }
  }
  return { skip, paired };
}

// noteHtml is a note's element. It links nowhere: the note sits in the very
// session its item is about, which the item's card on the Planner page only
// opens again. data-inbox-item keeps a notification's deep link landing here.
function noteHtml(n, paired) {
  const b = noteBody(n, paired);
  if (!b) return '';
  const h = paired || n;
  // A custody note's evidence is its reason, already in its text.
  const ev = n.kind === 'custody' ? '' : evidenceText(h.evidence, h.error_class);
  // A stop stands for its item's open note: the item's deep link lands here.
  const item = n.item || (paired && paired.item);
  return '<div class="gw-note gw-' + b.tone + '" role="note" data-note-id="' + escAttr(n.id) + '" data-note-time="' + n.ms + '"' +
    (item ? ' data-inbox-item="' + escAttr(item) + '"' : '') + (paired ? ' data-paired="1"' : '') +
    ' title="' + escAttr(formatTimeFull(n.ms) + (ev ? ' · ' + ev : '')) + '">' +
    '<span class="gw-note-icon" aria-hidden="true">' + esc(b.icon) + '</span>' +
    '<span class="gw-note-text">' + esc(b.text) + '</span>' +
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
  const { skip, paired } = pairs();
  // A pair can complete after either half was placed (the open note follows
  // its health note by a frame): drop the health note, and an open note
  // placed unpaired, which is placed again below as the pair.
  for (const node of el.querySelectorAll(':scope > .gw-note')) {
    const id = node.dataset.noteId;
    if (skip.has(id) || (paired.has(id) && !node.dataset.paired)) node.remove();
  }
  const shown = new Set([...el.querySelectorAll(':scope > .gw-note')].map((n) => n.dataset.noteId));
  const bubbles = [...el.querySelectorAll(':scope > .event[data-time]')];
  const first = bubbles.length ? Number(bubbles[0].dataset.time) : 0;
  const btn = document.getElementById('earlier-events-btn');
  const more = !!btn && el.contains(btn) && btn.dataset.state !== 'done';
  for (const n of notes.list) {
    if (skip.has(n.id) || shown.has(n.id) || (more && (!first || n.ms < first))) continue;
    const html = noteHtml(n, paired.get(n.id));
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
  // Custody's notes, on any session, also change what custody.js shows.
  if (msg.note?.kind === 'custody') nzBus.dispatchEvent(new CustomEvent('custody:note', { detail: msg }));
  if (!msg.note || msg.key !== notes.key || !notes.el) return;
  const el = notes.el;
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  addNotes([msg.note]);
  placeNotes();
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
