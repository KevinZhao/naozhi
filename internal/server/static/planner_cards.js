// planner_cards.js — the cards the Planner conversation renders in place of
// raw text (docs/rfc/planner.md §6.2, §10 D77, §10.1; §15.1 PR ⑬):
//
//   - action blocks (```naozhi-action fences) become readable cards. A card
//     is what was done (D91): the gateway adds a line only when a block did
//     not run or was held back. A block that is not JSON of a known type
//     shows as 看不懂; whether a block runs is the gateway's call, not this
//     reading's. Cards name tasks and workspaces, never their IDs: a name
//     not known yet leaves the row out.
//   - sessions the conversation names — a handed-over session
//     (<naozhi-session-ref>, rewritten to a fence by planner_view.js), a
//     `task:` key or a listed session's key in the Planner's text, the sender
//     of a report — become one-line cards (名字 · 健康状态 · 最后活动) that
//     open the session in the page's middle column, once per turn.
//
// Health comes from GET /api/system/health, refetched at most every
// HEALTH_MIN_MS; a session the table does not hold yet falls back to its
// listed state. Task titles come from the 任务 list (tasks.js 'tasks:list').
import { NZ_CONTRACT } from './contract.js';
import { sessionList } from './state.js';
import { HEALTH_STATES, esc, escAttr, evidenceText, fetchJSON, nzBus, sessionHealthState } from './nz_util.js';
import { sid } from './session_ident.js';
import { formatAbsTime, formatMonthDayTime, timeAgo } from './utilities.js';

const PLANNER_KEY = 'planner:main';
const HEALTH_MIN_MS = 20000;
// MAX_CARDS bounds the session cards under one bubble.
const MAX_CARDS = 6;
const TASK_KEY_RE = /\btask:[A-Za-z0-9_-]+/g;
// WORKSPACE_ID_RE is a workspace registry ID (planner/workspace), named by
// GET /api/planner's workspaces.
const WORKSPACE_ID_RE = /^ws_[a-z0-9]{8}$/;
// TURN_START opens a turn: your bubble, a session's report, or a time
// divider. A session is carded where the turn's replies and lines (a report
// among them) first name it, and after that only named; a session you hand
// over keeps its card in your bubble too.
const TURN_START = '.user, .peer_message, .event-time-divider';

const pc = {
  health: new Map(), // unit id → health.Unit
  healthAt: 0,
  healthLoading: false,
  tasks: new Map(), // task id → title, as 任务 last listed them
  workspaces: {}, // workspace ID → name, GET /api/planner's
};

// ---- session cards ----

// listedTitle is a listed local session's name, '' for one not listed.
function listedTitle(key) {
  const s = sessionList.sessionsData[sid(key, 'local')];
  return (s && (s.user_label || s.summary || s.last_prompt)) || '';
}

// KIND_NAMES name a unit nothing else names by its kind (D92): never its key.
const KIND_NAMES = [['task:', '派出的任务'], ['cron:', '定时任务'], ['sys:', '系统任务'], ['external:', '终端里的会话'], ['systemic:', '系统']];

function kindName(key) {
  const k = KIND_NAMES.find(([prefix]) => key.startsWith(prefix));
  return k ? k[1] : '会话';
}

// sessionTitle is a session's name: listed, else its 任务 title or the
// health table's name (a cron job's title); one known to none is named by
// its kind, never its key (D91, D92).
export function sessionTitle(key) {
  return listedTitle(key) || pc.tasks.get(key) || pc.health.get(key)?.name || kindName(key);
}

// unitName is an inbox item's unit as sessionTitle names it; a remote
// node's unit is "<node>/<key>".
export function unitName(unit) {
  const i = unit.indexOf('/');
  return pc.health.get(unit)?.name || sessionTitle(i > 0 ? unit.slice(i + 1) : unit);
}

// workspaceName is a registered workspace's name, '' for an ID not known.
export function workspaceName(id) {
  return (id && pc.workspaces[id]) || '';
}

// noteWorkspaces takes the registry's names from GET /api/planner and names
// the dispatch cards that were waiting for them; true when they changed.
export function noteWorkspaces(names) {
  const next = names && typeof names === 'object' ? names : {};
  if (JSON.stringify(next) === JSON.stringify(pc.workspaces)) return false;
  pc.workspaces = next;
  paintSessionCards(document);
  return true;
}

// taskName is a task's title (a dispatched session's name, a promise's
// text), '' while neither the session list nor 任务 has it.
function taskName(id) {
  return (id.startsWith('task:') && listedTitle(id)) || pc.tasks.get(id) || '';
}

// healthOf is the unit's state, else one read off the listed session.
function healthOf(key, s) {
  const u = pc.health.get(key);
  if (u && HEALTH_STATES[u.state]) return HEALTH_STATES[u.state];
  return s ? HEALTH_STATES[sessionHealthState(s)] || HEALTH_STATES.idle : null;
}

// sessionStatusHtml is a session's one status line: 名字 · 健康状态 · 最后活动
// (and what it is doing while it runs); withName false leaves the name to a
// heading that already says it.
export function sessionStatusHtml(key, withName = true) {
  const s = sessionList.sessionsData[sid(key, 'local')];
  const st = healthOf(key, s);
  const u = pc.health.get(key);
  const last = (u && u.last_active && Date.parse(u.last_active)) || (s && s.last_active) || 0;
  let tail = '';
  if (s && s.state === 'running' && s.last_activity) tail = s.last_activity;
  else if (last) tail = '最后活动 ' + timeAgo(last);
  return (withName ? '<span class="pl-scard-name">' + esc(sessionTitle(key)) + '</span>' : '') +
    (st ? '<span class="hl-state ' + st.cls + '"' + (u && u.evidence ? ' title="' + escAttr(evidenceText(u.evidence, u.error_class)) + '"' : '') + '>' + esc(st.label) + '</span>' : '') +
    (tail ? '<span class="pl-scard-time"' + (last ? ' title="' + escAttr(formatAbsTime(last)) + '"' : '') + '>' + esc(tail) + '</span>' : '');
}

export function sessionCardHtml(key) {
  return '<button type="button" class="pl-scard" data-action="planner-open-session" data-key="' + escAttr(key) + '">' +
    '<span class="pl-scard-line">' + sessionStatusHtml(key) + '</span>' +
    '<span class="pl-scard-open" aria-hidden="true">打开 ›</span></button>';
}

// paintSessionCards refreshes the status lines of the cards under root, and
// adds the 任务 row to the status cards whose task got a name since.
export function paintSessionCards(root) {
  if (!root) return;
  root.querySelectorAll('.pl-scard[data-key]').forEach((el) => {
    const line = el.querySelector('.pl-scard-line');
    const html = sessionStatusHtml(el.dataset.key);
    if (line && line.innerHTML !== html) line.innerHTML = html;
  });
  root.querySelectorAll('.pl-act-status[data-task]:not([data-named])').forEach((el) => {
    const name = taskName(el.dataset.task);
    if (!name) return;
    el.dataset.named = '1';
    el.querySelector('.pl-act-rows')?.insertAdjacentHTML('afterbegin', '<dt>任务</dt><dd>' + esc(name) + '</dd>');
  });
  root.querySelectorAll('.pl-act-dispatch[data-ws]').forEach((el) => {
    const name = workspaceName(el.dataset.ws);
    if (!name) return;
    delete el.dataset.ws;
    el.querySelector('.pl-act-rows')?.insertAdjacentHTML('afterbegin', '<dt>工作区</dt><dd>' + esc(name) + '</dd>');
  });
}

nzBus.addEventListener('tasks:list', (e) => {
  const tasks = (e.detail && Array.isArray(e.detail.tasks)) ? e.detail.tasks : [];
  for (const t of tasks) if (t && t.id && t.title) pc.tasks.set(t.id, t.title);
  paintSessionCards(document);
});

// refreshHealth refetches the health table (at most every HEALTH_MIN_MS
// unless forced) and repaints the cards under root.
export async function refreshHealth(root, force) {
  if (pc.healthLoading || (!force && Date.now() - pc.healthAt < HEALTH_MIN_MS)) return;
  pc.healthLoading = true;
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.system_health, { timeoutMs: 8000 });
    pc.health = new Map(((res && res.units) || []).map((u) => [u.id, u]));
  } catch { /* the listed state stands in */ } finally {
    pc.healthAt = Date.now();
    pc.healthLoading = false;
  }
  paintSessionCards(root);
  // The names it carries (a cron job's title) are the inbox cards' too.
  nzBus.dispatchEvent(new CustomEvent('planner:health'));
}

// mentionedKeys lists the sessions a bubble names: the sender of a report,
// then `task:` keys and listed local sessions' keys in its text.
function mentionedKeys(ev) {
  const keys = [];
  const add = (k) => { if (k && k !== PLANNER_KEY && !keys.includes(k)) keys.push(k); };
  ev.querySelectorAll('.peer-link[data-key]').forEach((b) => add(b.dataset.key));
  const content = ev.querySelector('.event-content');
  const text = content ? content.textContent : '';
  if (!text) return keys;
  for (const m of text.matchAll(TASK_KEY_RE)) add(m[0]);
  for (const s of Object.values(sessionList.sessionsData)) {
    if (s && s.key && (s.node || 'local') === 'local' && text.includes(s.key)) add(s.key);
  }
  return keys;
}

// ---- action blocks (§6.2) ----

const ACTION_TYPES = new Set(['dispatch', 'promise', 'ask', 'status', 'notify', 'custody']);
const STATUS_LABEL = { done: '已完成', doing: '进行中', failed: '失败' };

const isStr = (v) => typeof v === 'string';
const str = (v) => (isStr(v) ? v : '');

// webLinkHtml opens an http(s) page the Planner names (a mail, a Slack
// message) in a new tab; anything else is no link.
export function webLinkHtml(href, cls) {
  if (!isStr(href) || !/^https?:\/\//i.test(href)) return '';
  return '<a class="pl-link' + (cls ? ' ' + cls : '') + '" href="' + escAttr(href) + '" target="_blank" rel="noopener noreferrer">打开原文</a>';
}

// decisionActsHtml is an item's answer buttons (a decision's actions, or a
// Planner question's options, §6.2 ask): the left column's card and the
// conversation's repeat of it share it.
export function decisionActsHtml(it) {
  if (!it || !it.decision || !Array.isArray(it.decision.actions)) return '';
  return '<div class="pl-card-acts">' + it.decision.actions.map((a, i) =>
    '<button type="button" class="pl-btn' + (i === 0 ? ' pri' : '') + '" data-action="planner-inbox-decide" data-id="' + escAttr(it.id) + '" data-act="' + escAttr(a) + '">' +
      esc(a) + '</button>').join('') + '</div>';
}

// parseAction reads a block for display only: one JSON object of a known
// type, else why not. Whether a block runs is the gateway's call.
export function parseAction(raw) {
  let a;
  try { a = JSON.parse(raw); } catch { return { error: '不是合法的 JSON' }; }
  if (!a || typeof a !== 'object' || Array.isArray(a)) return { error: '不是 JSON 对象' };
  if (!ACTION_TYPES.has(a.type)) return { error: '未知类型 ' + str(a.type) };
  return { action: a };
}

function rowsHtml(rows) {
  return '<dl class="pl-act-rows">' + rows.filter((r) => r[1]).map((r) =>
    '<dt>' + esc(r[0]) + '</dt><dd>' + esc(r[1]) + '</dd>').join('') + '</dl>';
}

// dueText is a due time the browser reads, in local time; else as written.
function dueText(due) {
  const t = due ? Date.parse(due) : NaN;
  return isNaN(t) ? due : formatMonthDayTime(t);
}

// actionCardHtml renders one block.
function actionCardHtml(raw) {
  const { action: a, error } = parseAction(raw);
  if (!a) {
    return '<div class="pl-act pl-act-bad"><div class="pl-act-h">看不懂的动作块</div>' +
      '<div class="pl-act-why">' + esc(error) + '</div><pre class="pl-act-raw">' + esc(raw) + '</pre></div>';
  }
  if (a.type === 'dispatch') {
    const title = str(a.title);
    const ws = str(a.workspace);
    // A workspace written by its ID is named once the registry is in.
    const name = WORKSPACE_ID_RE.test(ws) ? workspaceName(ws) : ws;
    return '<div class="pl-act pl-act-dispatch"' + (name || !ws ? '' : ' data-ws="' + escAttr(ws) + '"') + '><div class="pl-act-h">派出会话' + (title ? ' · ' + esc(title) : '') + '</div>' +
      rowsHtml([['工作区', name], ['模型', str(a.model)], ['第一条消息', str(a.message)]]) + '</div>';
  }
  if (a.type === 'promise') {
    return '<div class="pl-act pl-act-promise"><div class="pl-act-h">登记承诺</div>' +
      rowsHtml([['内容', str(a.text)], ['到期', dueText(str(a.due))], ['完成条件', str(a.close_when)]]) + '</div>';
  }
  if (a.type === 'status') {
    // A task with no name yet gets its row from paintSessionCards.
    const task = str(a.task);
    const name = task ? taskName(task) : '';
    return '<div class="pl-act pl-act-status" data-task="' + escAttr(task) + '"' + (name || !task ? ' data-named="1"' : '') + '>' +
      '<div class="pl-act-h">Planner 判断 · ' + esc(STATUS_LABEL[a.state] || str(a.state)) + '</div>' +
      rowsHtml([['任务', name], ['依据', str(a.why)]]) + '</div>';
  }
  if (a.type === 'custody') {
    // The session by its card (its name, never the key); a go-ahead also
    // says what it was told.
    const key = str(a.key);
    const text = str(a.text);
    const head = text ? '托管 · 让它继续' : (a.on === false ? '关掉托管' : '打开托管');
    return '<div class="pl-act pl-act-custody"><div class="pl-act-h">' + head + '</div>' +
      (text ? '<div class="pl-act-q">' + esc(text) + '</div>' : '') +
      (key ? '<div class="pl-scards">' + sessionCardHtml(key) + '</div>' : '') + '</div>';
  }
  if (a.type === 'notify') {
    const brief = a.level === 'brief';
    const link = webLinkHtml(a.link);
    return '<div class="pl-act pl-act-notify"><div class="pl-act-h">' + (brief ? '记入早报' : '提醒你') + '</div>' +
      '<div class="pl-act-q">' + esc(str(a.title)) + '</div>' +
      (a.why ? '<div class="pl-act-why">' + esc(str(a.why)) + '</div>' : '') +
      (link ? '<div class="pl-act-link">' + link + '</div>' : '') + '</div>';
  }
  const opts = (Array.isArray(a.options) ? a.options : []).filter(isStr).map((o) => '<span class="pl-act-opt">' + esc(o) + '</span>').join('');
  return '<div class="pl-act pl-act-ask"><div class="pl-act-h">问你</div>' +
    '<div class="pl-act-q">' + esc(str(a.question)) + '</div>' +
    (opts ? '<div class="pl-act-opts">' + opts + '</div>' : '') + '</div>';
}

// ---- decoration ----

// decorateEvents turns the fences and the session mentions of the bubbles
// under root not yet seen into cards: a session named again within its turn
// (TURN_START) gets no second card, its key folds to its name.
export function decorateEvents(root) {
  if (!root) return;
  let turn = new Set(); // the keys carded in the turn so far
  root.querySelectorAll('.event, .event-time-divider').forEach((ev) => {
    if (ev.matches(TURN_START)) turn = new Set();
    if (ev.matches('.event:not([data-pl-dec])')) decorateEvent(ev, turn);
    if (!ev.matches('.user')) ev.querySelectorAll('.pl-scard[data-key]').forEach((b) => turn.add(b.dataset.key));
  });
}

function decorateEvent(ev, turn) {
  ev.dataset.plDec = '1';
  ev.querySelectorAll('code[data-lang="naozhi-session-ref"]').forEach((code) => {
    let ref = null;
    try { ref = JSON.parse(code.textContent); } catch { /* left as code */ }
    const wrap = code.closest('.md-code-wrap');
    if (!wrap || !ref || !isStr(ref.key) || !ref.key) return;
    wrap.outerHTML = '<div class="pl-handoff"><span class="pl-handoff-l">交给 Planner</span>' + sessionCardHtml(ref.key) + '</div>';
  });
  ev.querySelectorAll('code[data-lang="naozhi-action"]').forEach((code) => {
    const wrap = code.closest('.md-code-wrap');
    if (wrap) wrap.outerHTML = actionCardHtml(code.textContent);
  });
  if (!ev.matches('.text, .peer_message, .system')) return;
  const carded = new Set([...ev.querySelectorAll('.pl-scard[data-key]')].map((b) => b.dataset.key));
  const named = mentionedKeys(ev);
  const keys = named.filter((k) => !carded.has(k) && !turn.has(k)).slice(0, MAX_CARDS);
  if (keys.length) {
    ev.querySelector('.event-content')?.insertAdjacentHTML('beforeend', '<div class="pl-scards">' + keys.map(sessionCardHtml).join('') + '</div>');
  }
  if (ev.matches('.text')) foldKeys(ev, new Set([...carded, ...keys, ...named.filter((k) => turn.has(k))]));
}

// emptyBracketRE is a bracket the folded keys emptied: "（`a` 和 `b`）".
const emptyBracketRE = /\s*[（(]\s*(?:[和与及、，,；;]\s*)*[）)]/g;

// foldKeys drops from a reply the backticked keys it names sessions by
// after their names (§6.4): the cards under it carry them, and the user
// reads names, not keys. A key with no text before it stands for the name;
// a bracket of keys alone goes with them.
function foldKeys(ev, keys) {
  const emptied = new Set();
  ev.querySelectorAll('.event-content code').forEach((code) => {
    const key = code.textContent.trim();
    if (code.closest('pre, .pl-scards') || !keys.has(key)) return;
    const prev = code.previousSibling;
    const next = code.nextSibling;
    if (prev?.nodeType !== Node.TEXT_NODE || !prev.data.trim()) {
      code.replaceWith(sessionTitle(key));
      return;
    }
    if (prev?.nodeType === Node.TEXT_NODE && next?.nodeType === Node.TEXT_NODE && /[（(]$/.test(prev.data) && /^[）)]/.test(next.data)) {
      prev.data = prev.data.slice(0, -1);
      next.data = next.data.slice(1);
    }
    if (prev?.nodeType === Node.TEXT_NODE) prev.data = prev.data.trimEnd();
    // The separator before a key that closes a bracket or the line has
    // nothing left to separate: "（名字 work，`key`）" reads "（名字 work）".
    if (!next || next.nodeType === Node.TEXT_NODE && /^\s*[）)]/.test(next.data)) prev.data = prev.data.replace(/[，,、；;]$/, '');
    emptied.add(code.parentNode);
    code.remove();
  });
  emptied.forEach((parent) => {
    parent.normalize();
    parent.childNodes.forEach((n) => {
      if (n.nodeType === Node.TEXT_NODE) n.data = n.data.replace(emptyBracketRE, '');
    });
  });
}
