// session_notes.js — the gateway's side notes in the session view
// (docs/rfc/planner.md §15.3 ③): the session's health entering 待你回复 /
// 停滞 / 失败 and recovering, its 通知 items opening and closing, the Planner
// dispatching it, its reports to the Planner and what custody did on it
// (switched, held back, stopped, a retry coming; a message it sent is its
// own bubble), and what the Planner wrote to you about the session (its note
// blocks, §15.22 ②: marked Planner, what it goes by folded behind 依据, and
// 没用 to tell it the note did not help), and how far its background work
// got while its turn is over (§15.22 ④: the gateway writes one every 30
// minutes and when a workflow changes phase; a stretch of background work
// shows its latest only). GET /api/sessions/notes
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
import { HEALTH_STATES, esc, escAttr, evidenceCause, evidenceText, fetchJSON, nzBus, nzViews, registerActions, showToast } from './nz_util.js';
import { wsm } from './ws_manager.js';
import { formatTimeFull, formatTimeShort } from './utilities.js';
import { isLocalNode, nodeAPI, nodeRunsPlanner } from './planner_host.js';
import { notePane, settlePane } from './scroll_follow.js';

// REPORT_REFETCH_MS: a turn's report reaches the Planner after the turn
// ends, and reports are not pushed, so the list is fetched again this long
// after a result.
const REPORT_REFETCH_MS = 3000;
// PAIR_MS: a health transition and the 通知 item it opened are journaled in
// one reconcile; within this of each other they are one event, one note.
const PAIR_MS = 2 * 60000;

const ITEM_KINDS = { decision: '待你决定', question: '提问', error: '失败', notice: '' };
const BAD = new Set(['waiting', 'stalled', 'failed']);
// ITEM_STATE is the health state whose entry opens an item of a kind
// (notify.HandleHealth); only those pair. The waiting and stalled items open
// minutes after their transition, so they stay notes of their own.
const ITEM_STATE = { error: 'failed' };

const notes = {
  key: '', // the session the list is for
  node: 'local', // the node it runs on
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
    // Observing (§15.12 ⑥): the Planner's switch, noted; the user's is the chip.
    case 'watch_on': return { icon, tone: 'planner', text: '观察：已打开' + (CUSTODY_BY[n.by] || '') };
    case 'watch_off': return { icon, tone: 'planner', text: '观察：已关闭' + (CUSTODY_BY[n.by] || '') + '，不进任务、不写摘要' };
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
    // The text is the gateway's line (bgtask's State.Progress), the task
    // card's too; 后台中 is the state's word everywhere (D109).
    case 'progress': return n.text ? { icon: '◔', tone: 'progress', text: '后台中 · ' + n.text } : null;
  }
  return null;
}

// pairs matches each item's open note with the health note of the same
// event (the health machine opens the item as it journals the transition),
// and custody's stop with the error item it opened as it stopped (one event,
// said by the stop): skip holds the notes not shown on their own, paired maps
// an open note to its health note and a stop to its item's open note. A
// progress note a later one of its stretch (run) stands for is skipped too:
// the same run said again, further on.
function pairs() {
  const skip = new Set(), paired = new Map();
  const latest = new Map();
  for (const n of notes.list) if (n.kind === 'progress') latest.set(n.run || '', n.id);
  for (const n of notes.list) if (n.kind === 'progress' && latest.get(n.run || '') !== n.id) skip.add(n.id);
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

// plannerNoteHtml is a note the Planner wrote: its words in full, what it
// goes by behind 依据 (a native <details>, so no handler), and 没用. A
// remote node's session shows no 没用: the mark goes to the journal of the
// node the note was written on.
function plannerNoteHtml(n) {
  const mark = n.useless ? '<span class="gw-note-done">已标没用</span>'
    : isLocalNode(notes.node) ? '<button type="button" class="gw-note-btn" data-action="session-note-useless" data-id="' + escAttr(n.id) +
      '" title="告诉 Planner 这条旁注没有帮助；同一会话一天点 3 次，当天不再给它写">没用</button>' : '';
  return '<div class="gw-note gw-planner gw-note-pl' + (n.useless ? ' gw-note-off' : '') + '" role="note" data-note-id="' + escAttr(n.id) +
    '" data-note-time="' + n.ms + '" title="' + escAttr(formatTimeFull(n.ms)) + '">' +
    '<span class="gw-note-icon" aria-hidden="true">✎</span>' +
    '<span class="gw-note-by">Planner</span>' +
    '<span class="gw-note-text">' + esc(n.text || '') + '</span>' +
    (n.why ? '<details class="gw-note-why"><summary>依据</summary><div>' + esc(n.why) + '</div></details>' : '') +
    mark +
    '<span class="gw-note-time">' + esc(formatTimeShort(n.ms)) + '</span></div>';
}

// noteHtml is a note's element. It links nowhere: the note sits in the very
// session its item is about, which the item's card on the Planner page only
// opens again. data-inbox-item keeps a notification's deep link landing here.
function noteHtml(n, paired) {
  if (n.kind === 'planner') return plannerNoteHtml(n);
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
  const known = new Map(notes.list.map((n) => [n.id, n]));
  for (const n of list) {
    const ms = Date.parse(n.time);
    if (!n.id || isNaN(ms)) continue;
    const have = known.get(n.id);
    if (have) {
      // A list read again brings a mark made in another tab.
      if (n.useless && !have.useless) setUseless(have, true);
      continue;
    }
    const add = { ...n, ms };
    notes.list.push(add);
    known.set(n.id, add);
  }
  notes.list.sort((a, b) => a.ms - b.ms);
}

// setUseless marks Planner note n and repaints it where it stands.
function setUseless(n, on) {
  n.useless = on;
  if (!notes.el) return;
  for (const node of notes.el.querySelectorAll(':scope > .gw-note')) {
    if (node.dataset.noteId === n.id) node.outerHTML = noteHtml(n);
  }
}

// 没用: the note shows marked at once, the mark is journaled and the Planner
// told; a mark not kept is taken back.
registerActions({
  'session-note-useless': (el) => {
    const key = notes.key, list = notes.list;
    const n = list.find((x) => x.kind === 'planner' && x.id === el.dataset.id);
    if (!n || n.useless || !isLocalNode(notes.node)) return;
    setUseless(n, true);
    fetchJSON(NZ_CONTRACT.API.sessions_notes_useless, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key, id: n.id }),
    }).catch(() => {
      // Another session's list by now is not this note's to repaint.
      if (list === notes.list) setUseless(n, false);
      else n.useless = false;
      showToast('没记下，请再点一次', 'error');
    });
  },
});

async function fetchNotes() {
  const key = notes.key;
  const gen = ++notes.gen;
  try {
    const res = await fetchJSON(nodeAPI(notes.node, NZ_CONTRACT.API.sessions_notes) + '?key=' + encodeURIComponent(key), { timeoutMs: 8000 });
    if (gen !== notes.gen || key !== notes.key) return;
    addNotes(Array.isArray(res?.notes) ? res.notes : []);
  } catch { return; }
  placeNotes();
}

// startSessionNotes follows the session view's freshly built #events-scroll
// (renderMainShell): this node's sessions, and those of a node running a
// Planner, whose journal it serves through /api/remote/{node}/ (read when
// the session opens: that node's session_note frames do not reach here).
export function startSessionNotes() {
  const el = document.getElementById('events-scroll');
  if (!el || el === notes.el) return;
  notes.observer?.disconnect();
  notes.el = el;
  const node = selection.node || 'local';
  const key = isLocalNode(node) || nodeRunsPlanner(node) ? selection.key || '' : '';
  if (key !== notes.key || node !== notes.node) notes.list = [];
  notes.key = key;
  notes.node = node;
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
  if (!msg.note || msg.key !== notes.key || !isLocalNode(notes.node) || !notes.el) return;
  notePane(notes.el);
  addNotes([msg.note]);
  placeNotes();
  settlePane(notes.el);
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
