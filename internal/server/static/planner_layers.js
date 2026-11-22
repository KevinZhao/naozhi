// planner_layers.js — the Planner timeline's two layers (docs/rfc/planner.md
// §15.14 ②, D113; revises D103's 汇报一行). 对话, the default, shows the
// turns you started — your message, a session you handed over, your answer
// to a question — with the Planner's whole reply. Every other turn is one
// 动态 entry: a receipt (a report, a dispatch's outcome, custody's), a
// session's SendMessage, a due reminder, the receipt of blocks not run or
// refused, a scout run, a pre-meeting brief, a web lookup's answer, the
// line a 单独聊 you opened left (§15.14 ⑥: its line opens it), a session
// you opened ending a turn (§15.22 ①, D138), which is an entry only when
// the Planner had something to say. The
// entries between two of yours fold into one row
// "▸ HH:MM–HH:MM · N 条动态（…）", which unfolds to one line each (time, task
// or source, the Planner's first sentence, "→ 等你" when the turn asked you
// or notified you), a line to the turn's whole reply. A shift change is one
// separator line whose fold holds the handoff, never your bubble. 全部 is
// the timeline as it was, everything unfolded, for looking into things; the
// choice is this device's (localStorage). The page, the Alt+P drawer and a
// phone share the one stream (#pl-center), so all three have both.
//
// A turn is told by what started it, never by the model: its user entry's
// source (planner/agent Source.Entry, contract.js PLANNER_TURN_*), a
// SendMessage by its entry type, the gateway's report, reminder and 单独聊
// lines by their own format. An entry with no source is yours, a merge of requests
// of different sources, or older than the field: it is read part by part by
// the gateway's markers, and what cannot be told stays in 对话.
//
// planner_view.js puts an invisible marker (.pl-turn, markerHtml) before
// each turn's first element and one (.pl-end, endHtml) where a turn's result
// came; layerStream reads the stream in order, each element belonging to the
// marker before it (a report or reminder line to its own), and folds. A
// SendMessage the CLI delivered before the turn under way had its result is
// part of that turn (a queued_command), so it is an entry of its own and the
// turn goes on as it began. layerStream runs after every repaint of the
// stream and is idempotent: a row whose entries did not change is the same
// node, so what was opened or selected inside it stays.
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, registerActions } from './nz_util.js';
import { renderMd, runPendingAsync } from './render_md.js';
import { lsGet, lsSet } from './platform.js';
import { formatAbsTime, formatTimeShort } from './utilities.js';
import { firstSentence, sessionTitle } from './planner_cards.js';

const DYNAMIC = new Set(NZ_CONTRACT.ENUMS.PLANNER_TURN_DYNAMIC);
const SHIFT = new Set(NZ_CONTRACT.ENUMS.PLANNER_TURN_SHIFT);
// ANSWER is the entry source of your answer to a question (TellAnswer): a
// receipt in the text, yours by its source.
const ANSWER = 'answer';

// The gateway's markers, as planner_view.js reads them for the bubbles.
const RECEIPT_HEAD_RE = /^<naozhi-receipt>/;
const RECEIPT_END_RE = /<\/naozhi-receipt>\s*$/;
const SCOUT_RE = /^<naozhi-scout findings="/;
const MEETING_RE = /^<naozhi-scout kind="meeting"/;
const LOOKUP_RE = /^<naozhi-lookup /;
const PATROL_RE = /^<naozhi-patrol /;
// WATCH_RE is the gateway's word that a session you opened ended a turn
// (planner/agent TellWatch), WATCH_KEY_RE each session's line in it.
const WATCH_RE = /^<naozhi-watch>/;
const WATCH_KEY_RE = /^\{"key":"([^"]+)"/gm;
// NOTHING_RE is the Planner's whole reply to one it has nothing to say on
// (the prompt's 无).
const NOTHING_RE = /^无[。.]?$/;
// SHIFT_ASK_RE is the shift change's request for a handoff (planner/agent
// handoffPrompt), HANDOFF_RE the new conversation's first message with it
// (handoffMessage).
const SHIFT_ASK_RE = /^现在换班：/;
// A handoff's day names its summary: the shift day, with "-v<N>" or
// "-c<n>" after it for a change an upgrade or the size forced
// (agent.changeShift).
const HANDOFF_RE = /^<naozhi-handoff day="(\d{4}-\d\d-\d\d)[^"]*">\n?([\s\S]*?)(?:<\/naozhi-handoff>|$)/;
// ANSWER_RE is your answer as the gateway tells it (planner/action decideAsk):
// the question, the answer, then what became of a relay, which starts with
// one of the gateway's own sentences.
const ANSWER_RE = /^<naozhi-receipt>\s*你问用户的「([\s\S]*?)」，用户的回答：([\s\S]*?)(?:。(?:网关|它已经收起|task:)[\s\S]*)?\s*(?:<\/naozhi-receipt>)?\s*$/;
const COALESCE_HEAD = '[以下是用户在你处理上一条消息期间追加发送的内容]\n';
const COALESCE_SPLIT_RE = /\n\[\d\d:\d\d\] /;
// REPORT_NOTE_RE is the gateway's line for a session's report (planner/action
// "📨 <key> 汇报：…"), REMINDER_NOTE_RE its line for a due promise
// (wireup PromiseReminder), SEND_ERROR_RE the page's own line for a message
// of yours that did not go out, which stays in view whatever turn it lands in.
const REPORT_NOTE_RE = /^📨 (\S+) 汇报：/;
// ASIDE_NOTE_RE is its line for a 单独聊 you opened (planner/agent
// AsideNote, §15.14 ⑥): an entry whose line opens that session.
const ASIDE_NOTE_RE = /^💬 (\S+) 你开了单独聊：([\s\S]*)$/;
const REMINDER_NOTE_RE = /^到点提醒（/;
const SEND_ERROR_RE = /^没有发出：/;
// KEY_RE finds the session a receipt is about: a dispatched session's key,
// or a dashboard or IM session's (custody's receipts).
const KEY_RE = /\btask:[A-Za-z0-9_-]+|\b(?:dashboard|feishu|slack|discord|weixin):[a-z]+:[\w.#@-]+:[\w-]+/;
// SNAPSHOT_ANY_RE is a snapshot block wherever it sits (a wake block leads
// and the snapshot follows it), cut off or whole: the sessions it lists are
// not what the turn is about.
const SNAPSHOT_ANY_RE = /<naozhi-snapshot(?:\s[^>]*\/>|[\s>][\s\S]*?(?:<\/naozhi-snapshot>|$))/g;
// BY_SOURCE are the turns named by their source alone: what a scout run, a
// pre-meeting brief, a lookup or a project patrol's deep check brought in
// names sessions of its own, and the morning tidy lists every session that
// moved.
const BY_SOURCE = new Set(['scout', 'briefing', 'lookup', 'morning', 'patrol']);

// SOURCE_NAME names an entry by where it came from when no session does.
const SOURCE_NAME = {
  receipt: '网关回执', unrun: '没执行的动作', refusal: '被拒的动作', scout: '巡检', briefing: '会前简报',
  lookup: '网页查询', reminder: '到点提醒', morning: '晨间整理', peer: '会话消息', report: '汇报', aside: '单独聊',
  watch: '你开的会话', patrol: '项目巡查',
};
// MAX_NAMED is how many tasks or sources a row's parentheses name.
const MAX_NAMED = 3;
const MODE_KEY = 'plannerLayer';

const lay = {
  mode: lsGet(MODE_KEY, 'talk') === 'all' ? 'all' : 'talk',
  // running is whether the Planner is on a turn as the server last said
  // (GET /api/planner, a session_state frame), null when it has not: a
  // turn cut off without a result is over once it says no.
  running: null,
  busy: false,
  rows: new Set(), // the rows unfolded, by their first entry's ID
  lines: new Set(), // the entries unfolded to their whole reply
  shifts: new Set(), // the shift changes unfolded to their handoff
  handoffs: new Map(), // shift entry ID → the handoff as its message held it
  onChange: null, // planner_view.js's repaint after a mode change
};

const hm = (t) => {
  const d = new Date(t);
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
};

// partKind is what started one request: the whole message, or one part of
// a merge. In a merge only a receipt seen to its end is known to be one:
// past the bubble's cut a part of yours may follow.
function partKind(t, merged) {
  if (RECEIPT_HEAD_RE.test(t)) {
    if (ANSWER_RE.test(t)) return ANSWER;
    return merged && !RECEIPT_END_RE.test(t) ? 'user' : 'receipt';
  }
  if (MEETING_RE.test(t)) return 'briefing';
  if (SCOUT_RE.test(t)) return 'scout';
  if (LOOKUP_RE.test(t)) return 'lookup';
  if (PATROL_RE.test(t)) return 'patrol';
  if (WATCH_RE.test(t)) return 'watch';
  if (SHIFT_ASK_RE.test(t) || HANDOFF_RE.test(t)) return 'shift';
  return 'user';
}

// textKind reads a user entry without a source: a merge is yours when any
// of its parts is, else the shift change's when one part is.
function textKind(text) {
  if (!text.startsWith(COALESCE_HEAD)) return partKind(text, false);
  const kinds = text.slice(COALESCE_HEAD.length).split(COALESCE_SPLIT_RE).map((p) => p.trim()).filter(Boolean)
    .map((p) => partKind(p, true));
  if (!kinds.length || kinds.some((k) => k === 'user' || k === ANSWER)) return 'user';
  if (kinds.includes('shift')) return 'shift';
  return kinds[0];
}

// answerOf is your answer in a receipt the gateway wrote of it, null for
// any other text.
export function answerOf(text) {
  const m = ANSWER_RE.exec(String(text || ''));
  return m ? { question: m[1].trim(), answer: m[2].trim() } : null;
}

// plannerHead is what an event starts, null for one that starts nothing:
// a turn (a user entry, a SendMessage) or an entry of its own (the
// gateway's report, reminder and 单独聊 lines, the page's send error).
export function plannerHead(e) {
  if (!e) return null;
  const at = e.time || 0;
  const id = e.uuid || 't' + at + (e.type === 'system' ? 's' : '');
  if (e.type === 'peer_message') return { id, at, kind: 'peer', layer: 'dyn', key: e.peer_key || '', name: e.peer || '' };
  if (e.type === 'system') {
    const text = e.summary || '';
    const m = REPORT_NOTE_RE.exec(text);
    if (m) return { id, at, kind: 'report', layer: 'dyn', alone: true, key: m[1] };
    if (REMINDER_NOTE_RE.test(text)) return { id, at, kind: 'reminder', layer: 'dyn', alone: true };
    const a = ASIDE_NOTE_RE.exec(text);
    if (a) return { id, at, kind: 'aside', layer: 'dyn', alone: true, key: a[1], title: a[2].trim() };
    if (SEND_ERROR_RE.test(text)) return { id, at, kind: 'note', layer: 'talk', alone: true };
    return null;
  }
  if (e.type !== 'user') return null;
  const text = e.detail || e.summary || '';
  const src = e.source || '';
  let kind;
  if (DYNAMIC.has(src)) kind = src;
  else if (SHIFT.has(src)) kind = 'shift';
  else if (src) kind = src === ANSWER ? ANSWER : 'user';
  else kind = textKind(text);
  const head = { id, at, kind, layer: kind === 'shift' ? 'shift' : DYNAMIC.has(kind) ? 'dyn' : 'talk' };
  if (kind === 'watch') {
    // One session's turn end is named by it; several told as one are not.
    const keys = [...text.matchAll(WATCH_KEY_RE)];
    if (keys.length === 1) head.key = keys[0][1];
  } else if (head.layer === 'dyn' && !BY_SOURCE.has(kind)) head.key = (KEY_RE.exec(text.replace(SNAPSHOT_ANY_RE, '')) || [''])[0];
  if (kind === 'shift') {
    const h = HANDOFF_RE.exec(text);
    if (h) {
      head.day = h[1];
      lay.handoffs.set(id, h[2].trim());
    } else if (SHIFT_ASK_RE.test(text)) head.ask = true;
  }
  return head;
}

// markerHtml is the invisible element a turn's rendering starts with; its
// data-time places a morning brief before it, not inside the turn.
export function markerHtml(h) {
  return '<div class="pl-turn" hidden data-pl-id="' + escAttr(h.id) + '" data-pl-kind="' + escAttr(h.kind) +
    '" data-pl-layer="' + h.layer + '" data-time="' + (h.at || 0) + '"' +
    (h.alone ? ' data-pl-alone="1"' : '') + (h.key ? ' data-pl-key="' + escAttr(h.key) + '"' : '') +
    (h.name ? ' data-pl-name="' + escAttr(h.name) + '"' : '') + (h.day ? ' data-pl-day="' + escAttr(h.day) + '"' : '') +
    (h.ask ? ' data-pl-ask="1"' : '') + (h.title ? ' data-pl-title="' + escAttr(h.title) + '"' : '') + '></div>';
}

// endHtml is the invisible element where a turn's result came, for a
// result event; '' for any other.
export function endHtml(e) {
  return e && e.type === 'result' ? '<div class="pl-end" hidden data-time="' + (e.time || 0) + '"></div>' : '';
}

// noteRunning takes what the server says of the Planner's process: on a
// turn (true), idle or gone (false); anything else leaves it unknown.
export function noteRunning(on) {
  lay.running = typeof on === 'boolean' ? on : null;
}

// plannerBusy reports whether the Planner is on a turn you did not start
// (处理汇报中), as the last layerStream saw it.
export const plannerBusy = () => lay.busy;

export const layerMode = () => lay.mode;

// ---- reading the stream ----

function itemOf(m) {
  const d = m.dataset;
  return { id: d.plId || '', kind: d.plKind || '', layer: d.plLayer || 'talk', alone: !!d.plAlone, at: Number(d.time || 0),
    key: d.plKey || '', name: d.plName || '', day: d.plDay || '', ask: !!d.plAsk, title: d.plTitle || '', marker: m, els: [], run: null };
}

// shows reports whether an element of a turn you started is something to
// read: a hidden empty reply or a time divider is not, and leaves a run of
// 动态 whole.
const shows = (c) => !c.matches('.pl-ev-empty, .event-time-divider, .pl-turn, .pl-end');

// layerStream folds the stream (#pl-stream) as the mode says and works out
// whether the Planner is on a turn you did not start.
export function layerStream(box) {
  if (!box) return;
  // A row built again as events stream in: its button holding the focus
  // gets it back.
  const f = document.activeElement;
  const fRow = f && box.contains(f) ? f.closest('.pl-dyn, .pl-shift') : null;
  const refocus = fRow ? { id: fRow.dataset.id, item: f.closest('.pl-dyn-item')?.dataset.id || '', act: f.dataset.action || '' } : null;
  const old = new Map();
  const rowKey = (n) => (n.matches('.pl-dyn') ? 'dyn ' : 'shift ') + n.dataset.id;
  box.querySelectorAll(':scope > .pl-dyn, :scope > .pl-shift').forEach((n) => old.set(rowKey(n), n));
  box.querySelectorAll(':scope > .pl-folded').forEach((n) => n.classList.remove('pl-folded'));
  const talk = lay.mode === 'talk';
  box.classList.toggle('pl-talk', talk);
  const runs = [];
  let run = null;
  let cur = null; // the turn the elements belong to
  let open = false; // cur has had no result yet
  let alone = null; // an entry of its own, until its element
  let last = null; // the last turn begun
  const close = () => { if (run) runs.push(run); run = null; };
  const join = (it, node) => {
    if (it.run && it.run !== run) return; // already in a row above
    if (!run || run.type !== it.layer) { close(); run = { type: it.layer, items: [], first: node }; }
    if (!it.run) { it.run = run; run.items.push(it); }
  };
  for (const c of [...box.children]) {
    if (c.matches('.pl-dyn, .pl-shift')) continue;
    if (c.matches('.pl-end')) { open = false; continue; }
    if (c.matches('.pl-turn')) {
      const it = itemOf(c);
      // A SendMessage before the turn under way ended came into that turn.
      if (it.kind === 'peer' && cur && open) it.alone = true;
      if (it.alone) alone = it;
      else { cur = it; alone = null; last = it; open = true; }
      if (it.layer !== 'talk') join(it, c);
      continue;
    }
    // A brief, the bubble of what you just sent, the empty conversation's
    // text: in view whatever turn they fall in. The morning brief comes as
    // the shift changes: the change stays one line above it.
    if (c.matches('.pl-brief') && run && run.type === 'shift') continue;
    if (c.matches('.pl-brief, .pl-pending, .pl-empty')) { close(); continue; }
    const it = alone || cur;
    if (alone && c.matches('.event')) alone = null;
    if (!it || it.layer === 'talk') {
      if (shows(c)) close();
      continue;
    }
    it.els.push(c);
    join(it, c);
    if (talk) c.classList.add('pl-folded');
  }
  close();
  lay.busy = !!last && last.layer !== 'talk' && open && lay.running !== false;
  const kept = new Set();
  if (talk) {
    for (const r of runs) {
      // A watch turn the Planner had nothing to say on is no entry; a row
      // of nothing else is no row.
      const under = (it) => it === last && open && lay.running !== false;
      const items = r.type === 'dyn' ? mergeReports(r.items).filter((it) => !saidNothing(it, under(it))) : r.items;
      if (!items.length) continue;
      const fresh = r.type === 'dyn' ? rowEl(items, last) : shiftEl(items);
      const prev = old.get(rowKey(fresh));
      const el = prev && prev.dataset.sig === fresh.dataset.sig ? keep(prev, fresh) : fresh;
      if (el === fresh && prev) carryOpen(prev, fresh);
      kept.add(el);
      // Moving a node in place would drop a selection inside it.
      if (el.nextElementSibling !== r.first) r.first.before(el);
    }
  }
  for (const n of old.values()) if (!kept.has(n)) n.remove();
  if (refocus) focusAgain(box, refocus);
  runPendingAsync();
}

// keep is a row as it was, its entries the ones read now: what it says
// (dataset.sig) and the bubbles of each entry did not change, and the
// copies of an unfolded line stay as the user left them.
function keep(prev, fresh) {
  prev._items = fresh._items;
  return prev;
}

// carryOpen moves the unfolded lines of a row that changed to its new
// copy, where the entry still has the same bubbles: a card opened inside
// stays open.
function carryOpen(prev, fresh) {
  prev.querySelectorAll('.pl-dyn-item').forEach((box) => {
    const full = box.querySelector(':scope > .pl-dyn-full');
    const to = full && fresh.querySelector('.pl-dyn-item[data-id="' + CSS.escape(box.dataset.id) + '"]');
    if (!to || to.dataset.n !== box.dataset.n) return;
    to.querySelector(':scope > .pl-dyn-full')?.remove();
    to.appendChild(full);
  });
}

function focusAgain(box, r) {
  const row = box.querySelector(':scope > [data-id="' + CSS.escape(r.id) + '"]');
  if (!row) return;
  const scope = r.item ? row.querySelector('.pl-dyn-item[data-id="' + CSS.escape(r.item) + '"]') : row;
  const el = scope && r.act ? scope.querySelector('[data-action="' + CSS.escape(r.act) + '"]') : null;
  el?.focus({ preventScroll: true });
}

// saidNothing reports whether a watch turn (a session you opened ended a
// turn, D138) left nothing to show: no card, no line of the gateway's, and
// a last reply that is empty or 无. One still under way with neither card
// nor line counts too, so an entry does not show 处理中… and then go.
function saidNothing(it, under) {
  if (it.kind !== 'watch') return false;
  let reply = '';
  for (const el of it.els) {
    if (!el.matches('.event') || el.matches('.pl-ev-empty')) continue;
    if (el.matches('.event.system') || el.querySelector('.pl-act')) return false;
    if (el.matches('.event.text')) reply = textOf(el);
  }
  return under || !reply || NOTHING_RE.test(reply);
}

// mergeReports makes a session's report one entry: the gateway's line and
// the receipt turn that tells the Planner of it, side by side in either
// order, are about the same key.
function mergeReports(items) {
  const out = [];
  for (const it of items) {
    const prev = out[out.length - 1];
    const pair = prev && prev.key && prev.key === it.key && !prev.with &&
      ((prev.kind === 'report' && it.kind === 'receipt') || (prev.kind === 'receipt' && it.kind === 'report'));
    if (!pair) { out.push(it); continue; }
    const note = prev.kind === 'report' ? prev : it;
    const turn = prev.kind === 'report' ? it : prev;
    out[out.length - 1] = Object.assign({}, note, { with: turn, els: [...prev.els, ...it.els], at: prev.at, id: prev.id });
  }
  return out;
}

// entryName is the task or source an entry is about.
function entryName(it) {
  if (it.kind === 'aside') return SOURCE_NAME.aside;
  if (it.key) return sessionTitle(it.key);
  if (it.kind === 'peer' && it.name) return it.name;
  return SOURCE_NAME[it.kind] || '动态';
}

// textOf is what an element says, without the cards and buttons in it.
function textOf(el) {
  const c = el.querySelector('.event-content');
  if (!c) return '';
  const copy = c.cloneNode(true);
  copy.querySelectorAll('.pl-scards, .event-copy-btn, .event-ask-btn, .pl-act, .pl-handoff, .md-code-wrap').forEach((n) => n.remove());
  return copy.textContent.replace(/\s+/g, ' ').trim();
}

// entryLine is an entry's one sentence: the Planner's reply's first, else
// what the line or message it came with said.
function entryLine(it, last) {
  const els = it.els;
  for (const el of els) {
    if (!el.matches('.event.text') || el.matches('.pl-ev-empty')) continue;
    const t = textOf(el) || (el.querySelector('.pl-act')?.textContent || '').replace(/\s+/g, ' ').trim();
    if (t) return firstSentence(t);
  }
  if (lay.busy && (it === last || it.with === last)) return '处理中…';
  for (const el of els) {
    const rep = el.querySelector('.pl-report-t');
    if (rep) return firstSentence(rep.textContent);
    const peer = el.querySelector('.pl-peer-first');
    if (peer) return peer.textContent.trim();
    if (el.matches('.event.system, .event.peer_message')) {
      const t = textOf(el);
      if (t) return firstSentence(t);
    }
  }
  return '';
}

// asksYou reports whether an entry's turn asked you something or notified
// you: its card waits in 等你 as any question does (the morning brief's
// notices do not).
const asksYou = (it) => it.els.some((el) => el.querySelector('.pl-act-ask, .pl-act-notify:not(.pl-act-brief)'));

// whenText is a row's span: its first entry's time, to its last's.
function whenText(items) {
  const a = items[0].at, b = items[items.length - 1].at;
  const from = formatTimeShort(a);
  if (!b || (hm(a) === hm(b) && new Date(a).toDateString() === new Date(b).toDateString())) return from;
  return from + '–' + (new Date(a).toDateString() === new Date(b).toDateString() ? hm(b) : formatTimeShort(b));
}

// namedText is the parentheses: the tasks or sources the entries are
// about, in order, a repeated one counted, at most MAX_NAMED.
function namedText(names) {
  const counts = new Map();
  for (const n of names) counts.set(n, (counts.get(n) || 0) + 1);
  const list = [...counts].slice(0, MAX_NAMED).map(([n, k]) => (k > 1 ? n + ' ' + k + ' 条' : n));
  return list.join(' · ') + (counts.size > MAX_NAMED ? ' 等' : '');
}

function rowEl(items, last) {
  const id = items[0].id;
  const open = lay.rows.has(id);
  const names = items.map(entryName);
  const row = document.createElement('div');
  row.className = 'pl-dyn';
  row.dataset.id = id;
  row.dataset.time = String(items[0].at || 0);
  row.innerHTML = '<button type="button" class="pl-dyn-h" data-action="planner-dyn" aria-expanded="' + open + '" title="' + escAttr(formatAbsTime(items[0].at)) + '">' +
      '<span class="pl-dyn-mark" aria-hidden="true">' + (open ? '▾' : '▸') + '</span> <span class="pl-dyn-when">' + esc(whenText(items)) + '</span> · ' +
      items.length + ' 条动态<span class="pl-dyn-who">（' + esc(namedText(names)) + '）</span></button>' +
    '<div class="pl-dyn-list"' + (open ? '' : ' hidden') + '>' + items.map((it, i) => lineHtml(it, names[i], last)).join('') + '</div>';
  row.dataset.sig = [whenText(items), namedText(names), ...items.map((it, i) => it.id + ':' + it.els.length + ':' + names[i] + ':' + entryLine(it, last) + ':' + asksYou(it))].join('|');
  for (const it of items) if (lay.lines.has(it.id)) fillLine(row.querySelector('.pl-dyn-item[data-id="' + CSS.escape(it.id) + '"]'), it);
  row._items = items;
  return row;
}


function lineHtml(it, name, last) {
  const open = lay.lines.has(it.id);
  const one = entryLine(it, last);
  // A 单独聊's line is the way back into it, not a reply to unfold; its
  // title names it once it ended and the list no longer does.
  const act = it.kind === 'aside' && it.key
    ? 'data-action="planner-open-session" data-key="' + escAttr(it.key) + '"' + (it.title ? ' data-title="单独聊：' + escAttr(it.title) + '"' : '')
    : 'data-action="planner-dyn-line" aria-expanded="' + open + '"';
  return '<div class="pl-dyn-item" data-id="' + escAttr(it.id) + '" data-n="' + it.els.length + '">' +
    '<button type="button" class="pl-dyn-line" ' + act + ' title="' + escAttr(formatAbsTime(it.at)) + '">' +
      '<span class="pl-dyn-t">' + esc(hm(it.at)) + '</span><span class="pl-dyn-src">' + esc(name) + '</span>' +
      (one ? '<span class="pl-dyn-one">' + esc(one) + '</span>' : '') +
      (asksYou(it) ? '<span class="pl-dyn-wait">→ 等你</span>' : '') +
      (it.kind === 'aside' ? '<span class="pl-dyn-go" aria-hidden="true">▸</span>' : '') + '</button></div>';
}

// fillLine unfolds an entry to its whole reply: its bubbles as the 全部
// view shows them, copied, cards and all (their buttons act by data-action).
function fillLine(box, it) {
  if (!box || box.querySelector('.pl-dyn-full')) return;
  const full = document.createElement('div');
  full.className = 'pl-dyn-full';
  for (const el of it.els) {
    if (!el.matches('.event') || el.matches('.pl-ev-empty')) continue;
    const copy = el.cloneNode(true);
    copy.classList.remove('pl-folded');
    full.appendChild(copy);
  }
  if (!full.childNodes.length) full.innerHTML = '<div class="pl-dyn-none">这一条没有回答</div>';
  box.appendChild(full);
}

// shiftEl is the shift change's line: —— MM-DD 换班 · 看交接 ▸ ——, its fold
// the handoff: the outgoing conversation's summary, else the one the new
// conversation was given.
function shiftEl(items) {
  const id = items[0].id;
  const open = lay.shifts.has(id);
  const withDay = items.find((it) => it.day);
  const day = withDay ? withDay.day.slice(5) : mmdd(items[0].at);
  const el = document.createElement('div');
  el.className = 'pl-shift';
  el.dataset.id = id;
  el.dataset.time = String(items[0].at || 0);
  el.innerHTML = '<button type="button" class="pl-shift-h" data-action="planner-shift" aria-expanded="' + open + '" title="' + escAttr(formatAbsTime(items[0].at)) + '">' +
    '—— ' + esc(day) + ' 换班 · 看交接 <span class="pl-dyn-mark" aria-hidden="true">' + (open ? '▾' : '▸') + '</span> ——</button>' +
    '<div class="pl-shift-body"' + (open ? '' : ' hidden') + '></div>';
  el.dataset.sig = [day, ...items.map((it) => it.id + ':' + it.els.length)].join('|');
  el._items = items;
  if (open) fillShift(el);
  return el;
}

const mmdd = (t) => {
  const d = new Date(t);
  return String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
};

function fillShift(el) {
  const body = el.querySelector('.pl-shift-body');
  if (!body || body.childNodes.length) return;
  const items = el._items || [];
  const asked = items.find((it) => it.ask);
  const replies = asked ? asked.els.filter((n) => n.matches('.event.text') && !n.matches('.pl-ev-empty')) : [];
  if (replies.length) {
    for (const r of replies) {
      const copy = r.cloneNode(true);
      copy.classList.remove('pl-folded');
      body.appendChild(copy);
    }
    return;
  }
  const given = items.map((it) => lay.handoffs.get(it.id)).find(Boolean);
  body.innerHTML = given ? '<div class="md">' + renderMd(given) + '</div>' : '<div class="pl-dyn-none">这次换班没有交接摘要</div>';
  runPendingAsync();
}

// paintLayerSwitch writes 对话 | 全部 as the mode is.
export function paintLayerSwitch() {
  document.querySelectorAll('.pl-layer [data-layer]').forEach((b) => {
    b.setAttribute('aria-pressed', String(b.dataset.layer === lay.mode));
  });
}

export function setOnLayerChange(fn) { lay.onChange = fn; }

registerActions({
  'planner-layer': (el) => {
    const mode = el.dataset.layer === 'all' ? 'all' : 'talk';
    if (mode === lay.mode) return;
    lay.mode = mode;
    lsSet(MODE_KEY, mode);
    paintLayerSwitch();
    if (lay.onChange) lay.onChange();
  },
  'planner-dyn': (el) => {
    const row = el.closest('.pl-dyn');
    const list = row?.querySelector('.pl-dyn-list');
    if (!row || !list) return;
    const open = list.hidden;
    list.hidden = !open;
    if (open) lay.rows.add(row.dataset.id); else lay.rows.delete(row.dataset.id);
    el.setAttribute('aria-expanded', String(open));
    el.querySelector('.pl-dyn-mark').textContent = open ? '▾' : '▸';
  },
  'planner-dyn-line': (el) => {
    const box = el.closest('.pl-dyn-item');
    const row = el.closest('.pl-dyn');
    if (!box || !row) return;
    const id = box.dataset.id;
    const full = box.querySelector('.pl-dyn-full');
    if (full) {
      full.remove();
      lay.lines.delete(id);
      el.setAttribute('aria-expanded', 'false');
      return;
    }
    const it = (row._items || []).find((x) => x.id === id);
    if (!it) return;
    lay.lines.add(id);
    fillLine(box, it);
    el.setAttribute('aria-expanded', 'true');
    runPendingAsync();
  },
  'planner-shift': (el) => {
    const sep = el.closest('.pl-shift');
    const body = sep?.querySelector('.pl-shift-body');
    if (!sep || !body) return;
    const open = body.hidden;
    body.hidden = !open;
    if (open) { lay.shifts.add(sep.dataset.id); fillShift(sep); } else lay.shifts.delete(sep.dataset.id);
    el.setAttribute('aria-expanded', String(open));
    el.querySelector('.pl-dyn-mark').textContent = open ? '▾' : '▸';
  },
});
