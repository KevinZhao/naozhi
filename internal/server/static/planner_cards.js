// planner_cards.js — the cards the Planner conversation renders in place of
// raw text (docs/rfc/planner.md §6.2, §10 D77, §10.1; §15.1 PR ⑬):
//
//   - action blocks (```naozhi-action fences) become readable cards. A card
//     is what was done (D91): the gateway adds a line only when a block did
//     not run or was held back. A block that is not JSON of a known type
//     shows as 看不懂; whether a block runs is the gateway's call, not this
//     reading's. Cards name tasks and workspaces, never their IDs: a name
//     not known yet leaves the row out (a status line says the task's kind).
//     A dispatch folds to one line (派出 · 标题 · 工作区 · 目标: its goal, else
//     its message's first sentence; §15.12 ⑤, D103), the rest in its fold; a
//     status 进行中 is the task card's only (no line in the conversation, an
//     emptied bubble hidden), done / failed one line "✓ X 已完成 / ✗ X 失败",
//     the reason in its tooltip.
//   - the gateway's report line ("📨 <key> 汇报：<状态> · <summary>", §15.12
//     ③ ⑤) reads by the session's name and opens its 任务 card; a message a
//     session sent by SendMessage (peer_message) folds to its sender and first
//     sentence. The 会话 view renders both as before (event_render.js).
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
import { HEALTH_STATES, backgroundTasksText, esc, escAttr, evidenceText, fetchJSON, nzBus, registerActions, sessionHealthState, sessionTitleText, showToast } from './nz_util.js';
import { sid } from './session_ident.js';
import { formatAbsTime, formatMonthDayTime, timeAgo } from './utilities.js';
import { plannerAPI, plannerHost } from './planner_host.js';

const PLANNER_KEY = 'planner:main';
const HEALTH_MIN_MS = 20000;
// MAX_CARDS bounds the session cards under one bubble.
const MAX_CARDS = 6;
const TASK_KEY_RE = /\btask:[A-Za-z0-9_-]+/g;
// SESSION_KEY_RE is a dashboard or IM session's key ({platform}:{chatType}:
// {chatID}:{agentId}) the Planner wrote out: it becomes the session's card.
const SESSION_KEY_RE = /\b(?:dashboard|feishu|slack|discord|weixin):[a-z]+:[\w.#@-]+:[\w-]+/g;
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

// listedTitle is the name of a session listed on the Planner's node (every
// session the conversation names runs there), '' for one not listed.
function listedTitle(key) {
  const s = sessionList.sessionsData[sid(key, plannerHost.node)];
  return sessionTitleText(s);
}

// KIND_NAMES name a unit nothing else names by its kind (D92): never its key.
const KIND_NAMES = [['task:', '派出的任务'], ['cron:', '定时任务'], ['sys:', '系统任务'], ['external:', '终端里的会话'], ['systemic:', '系统']];

function kindName(key) {
  const k = KIND_NAMES.find(([prefix]) => key.startsWith(prefix));
  return k ? k[1] : '会话';
}

// The health table is the Planner's node's.
nzBus.addEventListener('planner:node', () => { pc.health = new Map(); pc.healthAt = 0; pc.tasks = new Map(); });

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

// BG_STATE is an idle session's state while its background tasks run
// (后台中, D109): the turn is over, its work is not.
const BG_STATE = { label: '后台中', cls: 'bg' };

// healthOf is the unit's state, else one read off the listed session; an
// idle one with background tasks running reads 后台中, as its sidebar row
// does (session_list.js rowStatusText).
function healthOf(key, s) {
  const u = pc.health.get(key);
  let st = null;
  if (u && HEALTH_STATES[u.state]) st = u.state;
  else if (s) st = HEALTH_STATES[sessionHealthState(s)] ? sessionHealthState(s) : 'idle';
  if (!st) return null;
  if (st === 'idle' && !(s && s.state === 'running') && backgroundN(u, s) > 0) return BG_STATE;
  return HEALTH_STATES[st];
}

// backgroundN is how many background tasks the session has open, by the
// health table's unit or the listed session's health.
function backgroundN(u, s) {
  return Math.max((u && u.background_n) || 0, (s && s.health && s.health.background_n) || 0);
}

// sessionStatusHtml is a session's one status line: 名字 · 健康状态 · when it
// was last active, its time alone (none while it runs: the state says it, and the tool it is in is not
// news here); withName false leaves the name to a heading that already says it.
export function sessionStatusHtml(key, withName = true) {
  const s = sessionList.sessionsData[sid(key, plannerHost.node)];
  const st = healthOf(key, s);
  const u = pc.health.get(key);
  const last = (u && u.last_active && Date.parse(u.last_active)) || (s && s.last_active) || 0;
  const tail = last && !(s && s.state === 'running') ? timeAgo(last) : '';
  const tip = st === BG_STATE ? backgroundTasksText(backgroundN(u, s), (u && u.background_tasks) || (s && s.health && s.health.background_tasks))
    : u && u.evidence ? evidenceText(u.evidence, u.error_class) : '';
  return (withName ? '<span class="pl-scard-name">' + esc(sessionTitle(key)) + '</span>' : '') +
    (st ? '<span class="hl-state ' + st.cls + '"' + (tip ? ' title="' + escAttr(tip) + '"' : '') + '>' + esc(st.label) + '</span>' : '') +
    (tail ? '<span class="pl-scard-time" title="' + escAttr('最后活动 ' + formatAbsTime(last)) + '">' + esc(tail) + '</span>' : '');
}

export function sessionCardHtml(key) {
  return '<button type="button" class="pl-scard" data-action="planner-open-session" data-key="' + escAttr(key) + '">' +
    '<span class="pl-scard-line">' + sessionStatusHtml(key) + '</span>' +
    '<span class="pl-scard-open" aria-hidden="true">›</span></button>';
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
    const b = el.querySelector('.pl-act-name');
    if (b) b.textContent = name;
  });
  root.querySelectorAll('.pl-act-dispatch[data-ws]').forEach((el) => {
    const name = workspaceName(el.dataset.ws);
    if (!name) return;
    delete el.dataset.ws;
    const head = el.querySelector('.pl-act-ws');
    if (head) { head.textContent = ' · ' + wsLabel(name); head.title = name; }
    el.querySelector('.pl-act-rows')?.insertAdjacentHTML('afterbegin', '<dt>工作区</dt><dd>' + esc(name) + '</dd>');
  });
  root.querySelectorAll('.pl-report-name[data-key], .pl-relay-name[data-key]').forEach((el) => {
    const name = sessionTitle(el.dataset.key);
    if (el.textContent !== name) el.textContent = name;
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
    const res = await fetchJSON(plannerAPI(NZ_CONTRACT.API.system_health), { timeoutMs: 8000 });
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
// then `task:` keys, other session keys (SESSION_KEY_RE) and listed local
// sessions' keys in its text.
function mentionedKeys(ev) {
  const keys = [];
  const add = (k) => { if (k && k !== PLANNER_KEY && !keys.includes(k)) keys.push(k); };
  ev.querySelectorAll('.peer-link[data-key]').forEach((b) => add(b.dataset.key));
  const content = ev.querySelector('.event-content');
  const text = content ? content.textContent : '';
  if (!text) return keys;
  for (const m of text.matchAll(TASK_KEY_RE)) add(m[0]);
  for (const m of text.matchAll(SESSION_KEY_RE)) add(m[0]);
  for (const s of Object.values(sessionList.sessionsData)) {
    if (s && s.key && (s.node || 'local') === plannerHost.node && text.includes(s.key)) add(s.key);
  }
  return keys;
}

// ---- action blocks (§6.2) ----

const ACTION_TYPES = new Set(['dispatch', 'promise', 'ask', 'status', 'notify', 'custody', 'relay', 'remember', 'lookup']);
const STATUS_LABEL = { done: '已完成', doing: '进行中', failed: '失败' };
const STATUS_MARK = { done: '✓', failed: '✗' };

const isStr = (v) => typeof v === 'string';
const str = (v) => (isStr(v) ? v : '');

// LINE_MAX caps a dispatch's one line in characters, as the gateway caps
// its goal (planner/action goalRunes, D103): one past it is cut to the
// "…", so the card says what the task card and the digest say.
const LINE_MAX = 60;
const oneLine = (s) => s.replace(/\s+/g, ' ').trim();
function capped(s) {
  const chars = Array.from(s);
  return chars.length > LINE_MAX ? chars.slice(0, LINE_MAX - 1).join('') + '…' : s;
}

// wsLabel is a workspace in a dispatch's one line: a path by its last part,
// the rows below saying it whole.
const wsLabel = (name) => name.replace(/\/+$/, '').split('/').pop() || name;

// firstSentence is a dispatch's message's first sentence (to its 。！？!? or
// ". ", within its first paragraph), the goal of a block that gives none.
function firstSentence(s) {
  const para = oneLine(String(s || '').trim().split(/\n\s*\n/)[0] || '');
  const m = /^.+?(?:[。！？!?]|\.(?= |$))/.exec(para);
  return capped(m ? m[0] : para);
}

// unnamedTask names a status block's task no list names yet by its kind.
const unnamedTask = (task) => (task.startsWith('task:') ? sessionTitle(task) : '任务');

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
    const goal = capped(oneLine(str(a.goal)));
    const line = goal || firstSentence(str(a.message));
    return '<details class="pl-act pl-act-dispatch"' + (name || !ws ? '' : ' data-ws="' + escAttr(ws) + '"') + '>' +
      '<summary class="pl-act-h"><span class="pl-act-k">派出</span>' + (title ? ' · <b>' + esc(title) + '</b>' : '') +
        (name ? '<span class="pl-act-ws" title="' + escAttr(name) + '"> · ' + esc(wsLabel(name)) + '</span>' : '<span class="pl-act-ws"></span>') +
        (line ? '<span class="pl-act-goal" title="' + escAttr(line) + '"> · ' + esc(line) + '</span>' : '') + '</summary>' +
      rowsHtml([['工作区', name], ['目标', goal], ['模型', str(a.model)], ['附上', a.with_user_words === true ? '你这一轮的原话' : ''],
        ['放行写工具', mcpToolsText(a.mcp_tools)], ['模式', a.ultracode === true ? 'ultracode（每一轮可多代理编排）' : ''],
        ['第一条消息', str(a.message)]]) + '</details>';
  }
  if (a.type === 'relay') {
    // One line like a dispatch (转达你的决定 · 会话 · 说明的第一句); the
    // fold says whose words lead what the session got and holds the
    // Planner's whole text and the session card.
    const key = str(a.key);
    const text = str(a.text);
    const line = firstSentence(text);
    return '<details class="pl-act pl-act-relay">' +
      '<summary class="pl-act-h"><span class="pl-act-k">转达你的决定</span>' +
        (key ? ' · <b class="pl-relay-name" data-key="' + escAttr(key) + '">' + esc(sessionTitle(key)) + '</b>' : '') +
        (line ? '<span class="pl-act-goal" title="' + escAttr(line) + '"> · ' + esc(line) + '</span>' : '') + '</summary>' +
      '<div class="pl-act-why">附上你这一轮的原话，再加上 Planner 的说明：</div>' +
      '<div class="pl-act-q">' + esc(text) + '</div>' +
      (key ? '<div class="pl-scards">' + sessionCardHtml(key) + '</div>' : '') + '</details>';
  }
  if (a.type === 'remember') {
    // 记下了 · name · 工作区, the text in the fold with 撤回 (§15.12 ⑧): the
    // gateway deletes the file and its MEMORY.md line. A block the gateway
    // refused gets its own line, as every card's.
    const ws = str(a.workspace);
    const where = ws.toLowerCase() === 'global' ? 'Planner 自己的记忆' : (WORKSPACE_ID_RE.test(ws) ? workspaceName(ws) : ws);
    const line = firstSentence(str(a.text));
    return '<details class="pl-act pl-act-remember">' +
      '<summary class="pl-act-h"><span class="pl-act-k">记下了</span>' +
        (where ? '<span class="pl-act-ws" title="' + escAttr(where) + '"> · ' + esc(wsLabel(where)) + '</span>' : '') +
        (line ? '<span class="pl-act-goal" title="' + escAttr(line) + '"> · ' + esc(line) + '</span>' : '') + '</summary>' +
      rowsHtml([['记在', where], ['名字', str(a.name)], ['内容', str(a.text)]]) +
      '<div class="pl-card-acts"><button type="button" class="pl-btn" data-action="planner-memory-forget" data-ws="' + escAttr(ws) + '" data-name="' + escAttr(str(a.name)) + '">撤回</button></div>' +
      '</details>';
  }
  if (a.type === 'lookup') {
    // One line: 查一下 · the question; the answer comes later as the query
    // card (lookupLine, §15.12 ⑨).
    const q = oneLine(str(a.q));
    const why = str(a.why);
    return '<div class="pl-act pl-act-lookup pl-act-line"' + (why ? ' title="' + escAttr('为什么查：' + why) + '"' : '') + '>' +
      '<span class="pl-act-k">查一下</span>' + (q ? ' · <span class="pl-act-goal">' + esc(q) + '</span>' : '') + '</div>';
  }
  if (a.type === 'promise') {
    return '<div class="pl-act pl-act-promise"><div class="pl-act-h">登记承诺</div>' +
      rowsHtml([['内容', str(a.text)], ['到期', dueText(str(a.due))], ['完成条件', str(a.close_when)]]) + '</div>';
  }
  if (a.type === 'status') {
    // 进行中 only moves the task card (§15.12 ⑤): nothing in the
    // conversation. A task with no name yet is named by its kind until
    // paintSessionCards has one.
    if (a.state === 'doing') return '';
    const task = str(a.task);
    const name = task ? taskName(task) : '';
    const why = str(a.why);
    const mark = STATUS_MARK[a.state] || '·';
    return '<div class="pl-act pl-act-status pl-act-line pl-st-' + escAttr(STATUS_MARK[a.state] ? a.state : 'other') + '" data-task="' + escAttr(task) + '"' + (name || !task ? ' data-named="1"' : '') +
      (why ? ' title="' + escAttr('依据：' + why) + '"' : '') + '>' +
      '<span class="pl-act-mark" aria-hidden="true">' + mark + '</span> <b class="pl-act-name">' + esc(name || unnamedTask(task)) + '</b> ' +
      esc(STATUS_LABEL[a.state] || str(a.state)) + '</div>';
  }
  if (a.type === 'custody') {
    // The session by its card (its name, never the key); a go-ahead also
    // says what it was told.
    const key = str(a.key);
    const text = str(a.text);
    const head = text ? '托管 · 让它继续' : a.watch === false ? '不再观察' : (a.on === false ? '关掉托管' : '打开托管');
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
  const task = str(a.task);
  return '<div class="pl-act pl-act-ask"><div class="pl-act-h">问你</div>' +
    '<div class="pl-act-q">' + esc(str(a.question)) + '</div>' +
    (opts ? '<div class="pl-act-opts">' + opts + '</div>' : '') +
    (task ? '<div class="pl-act-why">你的回答会直接送到：</div><div class="pl-scards">' + sessionCardHtml(task) + '</div>' : '') + '</div>';
}

// mcpToolsText is a dispatch's mcp_tools as one line: server: tool、tool；...
function mcpToolsText(tools) {
  if (!tools || typeof tools !== 'object' || Array.isArray(tools)) return '';
  return Object.entries(tools).map(([server, names]) => {
    const list = Array.isArray(names) ? names.map(str).filter(Boolean).join('、') : '';
    return server + (list ? '：' + list : '');
  }).join('；');
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
  tidyEmpty(root);
}

// tidyEmpty mends what the hidden empty bubbles (.pl-ev-empty) would leave:
// a reply after them shows the robot (.pl-ev-lead) unless the bubble shown
// before them was a reply too, and a time divider that only they follow
// goes (.pl-div-empty). The bubbles around change as pages come in on
// either end, so each pass reads the whole stream.
export function tidyEmpty(root) {
  if (!root) return;
  let div = null;
  let shown = false;
  const endDiv = () => { if (div) div.classList.toggle('pl-div-empty', !shown); };
  for (const c of root.children) {
    if (c.matches('.event-time-divider')) {
      endDiv();
      div = c;
      shown = false;
      continue;
    }
    const empty = c.matches('.pl-ev-empty');
    if (!empty) shown = true;
    if (!c.matches('.event.text') || empty) continue;
    let prev = c.previousElementSibling;
    if (!prev || !prev.matches('.pl-ev-empty')) { c.classList.remove('pl-ev-lead'); continue; }
    while (prev && prev.matches('.pl-ev-empty')) prev = prev.previousElementSibling;
    c.classList.toggle('pl-ev-lead', !(prev && prev.matches('.event.text')));
  }
  endDiv();
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
    // A block with no line in the conversation (status 进行中) leaves a
    // marker only, for the blank lines after it to go with it.
    if (wrap) wrap.outerHTML = actionCardHtml(code.textContent) || '<span class="pl-act-gone"></span>';
  });
  ev.querySelectorAll('.pl-act, .pl-handoff').forEach(trimAround);
  ev.querySelectorAll('.pl-act-gone').forEach((m) => {
    while (blankNode(m.nextSibling)) m.nextSibling.remove();
    m.remove();
  });
  const content = ev.querySelector('.event-content');
  if (ev.matches('.system') && content && (reportLine(ev, content) || lookupLine(ev, content))) return;
  if (!ev.matches('.text, .peer_message, .system')) return;
  const carded = new Set([...ev.querySelectorAll('.pl-scard[data-key]')].map((b) => b.dataset.key));
  const named = mentionedKeys(ev);
  const keys = named.filter((k) => !carded.has(k) && !turn.has(k)).slice(0, MAX_CARDS);
  if (keys.length && content) {
    // Above the bubble's own buttons (复制): the cards belong to its text.
    const html = '<div class="pl-scards">' + keys.map(sessionCardHtml).join('') + '</div>';
    const acts = content.querySelector(':scope > .event-copy-btn, :scope > .event-ask-btn');
    if (acts) acts.insertAdjacentHTML('beforebegin', html);
    else content.insertAdjacentHTML('beforeend', html);
  }
  if (ev.matches('.peer_message') && content) foldPeer(content);
  if (ev.matches('.text')) {
    const folded = new Set([...carded, ...keys, ...named.filter((k) => turn.has(k))]);
    foldKeys(ev, folded);
    foldTextKeys(ev, folded);
    // A reply that was only blocks with no line (status 进行中) leaves no
    // empty bubble behind.
    if (content && emptyContent(content)) ev.classList.add('pl-ev-empty');
  }
}

// blankNode is a blank text node or the markdown's blank line / break.
const blankNode = (n) => !!n && (n.nodeType === Node.TEXT_NODE ? !n.data.trim() : n.matches('.md-blank, br'));

// emptyContent reports whether a bubble's content holds nothing to read:
// blank lines, its own buttons (复制, 追问) and elements with no text and no
// picture.
function emptyContent(content) {
  return [...content.childNodes].every((n) => blankNode(n) ||
    (n.nodeType === Node.ELEMENT_NODE && (n.matches('.event-copy-btn, .event-ask-btn') || (!n.textContent.trim() && !n.querySelector('img, svg, canvas, video')))));
}

// REPORT_NOTE_RE is the gateway's line for a session's naozhi-report
// (planner/action: "📨 <key> 汇报：<状态> · <summary>", §15.12 ③): the key
// becomes the session's name, and the line opens its 任务 card.
const REPORT_NOTE_RE = /^📨 (\S+) 汇报：([\s\S]*)$/;

// LOOKUP_NOTE_RE is the line planner_view.js makes of a lookup's answer to
// the Planner (<naozhi-lookup run=…>, planner/reader): "🔎 <run> <question>".
// It becomes the query card, filled from the run's record.
const LOOKUP_NOTE_RE = /^🔎 (\d{8}T\d{6}-[0-9a-f]{4}) ([\s\S]*)$/;
const CONFIDENCE_LABEL = { high: '可信度高', medium: '可信度中', low: '可信度低' };

// hostOf is a source link's site, its text in the card.
function hostOf(url) {
  try { return new URL(url).hostname.replace(/^www\./, ''); } catch { return ''; }
}

// lookupLine renders a lookup's answer as the query card (question · the
// answer's first sentence, the source links; the whole answer and each
// fact's quote in the fold), and reports whether the line was one.
function lookupLine(ev, content) {
  const m = LOOKUP_NOTE_RE.exec(content.textContent.trim());
  if (!m) return false;
  const run = m[1];
  ev.classList.add('pl-lookup-ev');
  content.innerHTML = lookupCardHtml(run, { q: m[2], state: 'loading' });
  fetchJSON(plannerAPI(NZ_CONTRACT.API.planner_lookup) + '?run=' + encodeURIComponent(run), { timeoutMs: 8000 })
    .then((rec) => { content.innerHTML = lookupCardHtml(run, rec || {}); })
    .catch(() => { content.innerHTML = lookupCardHtml(run, { q: m[2], state: 'gone' }); });
  return true;
}

function lookupCardHtml(run, rec) {
  const q = oneLine(str(rec.q));
  const head = '<span class="pl-act-k">网页查询</span>' + (q ? ' · <b>' + esc(q) + '</b>' : '');
  const res = rec.result && typeof rec.result === 'object' ? rec.result : null;
  if (!res) {
    const why = rec.state === 'failed' ? '没查成：' + str(rec.error)
      : rec.state === 'loading' ? '正在读取结果…' : rec.state === 'running' ? '正在查…' : '这次查询的记录已经不在了';
    return '<div class="pl-lookup pl-lookup-' + escAttr(str(rec.state) || 'gone') + '" data-run="' + escAttr(run) + '"><div class="pl-act-h">' + head + '</div>' +
      '<div class="pl-act-why">' + esc(why) + '</div></div>';
  }
  const answer = str(res.answer);
  const facts = (Array.isArray(res.facts) ? res.facts : []).filter((f) => f && typeof f === 'object');
  // One link per site, its first page; the fold has every page.
  const seen = new Set();
  const links = facts.map((f) => str(f.url)).filter((u) => /^https?:\/\//i.test(u) && !seen.has(hostOf(u) || u) && seen.add(hostOf(u) || u))
    .map((u) => '<a class="pl-link" href="' + escAttr(u) + '" target="_blank" rel="noopener noreferrer">' + esc(hostOf(u) || u) + '</a>').join('');
  const conf = CONFIDENCE_LABEL[res.confidence] || '';
  // The gateway cut what was past the limits (planner/reader ParseResult).
  const cut = Array.isArray(res.truncated) && res.truncated.length
    ? '<span class="pl-lookup-conf pl-lookup-cut" title="' + escAttr('超过长度上限被截断：' + res.truncated.map(str).join('、')) + '">已截断</span>' : '';
  const factsHtml = facts.map((f) => {
    const u = str(f.url);
    return '<li><div class="pl-lookup-claim">' + esc(str(f.claim)) + '</div>' +
      (f.quote ? '<blockquote class="pl-lookup-quote">' + esc(str(f.quote)) + '</blockquote>' : '') +
      (/^https?:\/\//i.test(u) ? '<a class="pl-link" href="' + escAttr(u) + '" target="_blank" rel="noopener noreferrer">' + esc(u) + '</a>' : '') + '</li>';
  }).join('');
  return '<details class="pl-act pl-lookup" data-run="' + escAttr(run) + '">' +
    '<summary class="pl-act-h">' + head +
      '<div class="pl-lookup-one">' + esc(firstSentence(answer)) +
        (conf ? '<span class="pl-lookup-conf pl-lookup-' + escAttr(res.confidence) + '">' + esc(conf) + '</span>' : '') + cut + '</div></summary>' +
    '<div class="pl-lookup-ans">' + esc(answer) + '</div>' +
    (factsHtml ? '<ol class="pl-lookup-facts">' + factsHtml + '</ol>' : '') + '</details>' +
    (links ? '<div class="pl-lookup-src">来源：' + links + '</div>' : '');
}

// reportLine renders a report line in place of the system line's text, and
// reports whether it was one. The line names its session already: no card.
function reportLine(ev, content) {
  const m = REPORT_NOTE_RE.exec(content.textContent.trim());
  if (!m) return false;
  const key = m[1];
  const rest = m[2].replace(/\s·\s*$/, '');
  ev.classList.add('pl-report-ev');
  content.innerHTML = '<button type="button" class="pl-report" data-action="planner-open-task" data-key="' + escAttr(key) + '" title="打开任务卡">' +
    '<span aria-hidden="true">📨</span> <b class="pl-report-name" data-key="' + escAttr(key) + '">' + esc(sessionTitle(key)) + '</b> 汇报：' +
    '<span class="pl-report-t">' + esc(rest) + '</span></button>';
  return true;
}

// foldPeer folds a cross-session message on the Planner page to its sender
// and first sentence (§15.12 ⑤): the sender's line as text and the sentence
// are the fold's summary, the whole message and the sender's card (its
// link, carded first: mentionedKeys) the body. A summary holds no button,
// it is one itself, so the line's link (.peer-source) goes. A message of
// one sentence has nothing to fold.
function foldPeer(content) {
  const src = content.querySelector(':scope > .peer-source');
  if (!src) return;
  const body = [];
  for (let n = src.nextSibling; n; n = n.nextSibling) {
    if (n.nodeType === Node.ELEMENT_NODE && n.matches('.event-copy-btn, .event-ask-btn')) break;
    body.push(n);
  }
  const text = oneLine(body.filter((n) => !(n.nodeType === Node.ELEMENT_NODE && n.matches('.pl-scards'))).map((n) => n.textContent).join(' '));
  const first = firstSentence(text);
  if (!text || first === text) return;
  const d = document.createElement('details');
  d.className = 'pl-peer';
  const sum = document.createElement('summary');
  sum.className = 'pl-peer-h';
  const from = document.createElement('span');
  from.className = 'pl-peer-from';
  from.textContent = oneLine(src.textContent);
  const lead = document.createElement('span');
  lead.className = 'pl-peer-first';
  lead.textContent = first;
  sum.append(from, lead);
  const wrap = document.createElement('div');
  wrap.className = 'pl-peer-body';
  wrap.append(...body);
  src.remove();
  d.append(sum, wrap);
  content.prepend(d);
}

// trimAround drops the blank lines and line breaks the markdown left around a
// card that took a fence's place: the card's own margin spaces it, and
// cards in a row join into one block (views.css).
function trimAround(card) {
  while (blankNode(card.previousSibling)) card.previousSibling.remove();
  while (blankNode(card.nextSibling)) card.nextSibling.remove();
}

// foldTextKeys puts a session's name where a reply wrote its key out as
// plain text (not in code): the card under the reply opens it.
function foldTextKeys(ev, keys) {
  const content = ev.querySelector('.event-content');
  if (!content || !keys.size) return;
  const walker = document.createTreeWalker(content, NodeFilter.SHOW_TEXT, {
    acceptNode: (n) => (n.parentElement?.closest('pre, code, .pl-scards, .pl-handoff, .pl-act') ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT),
  });
  const nodes = [];
  while (walker.nextNode()) nodes.push(walker.currentNode);
  for (const n of nodes) {
    let t = n.data;
    for (const k of keys) if (t.includes(k)) t = t.split(k).join(sessionTitle(k));
    if (t !== n.data) n.data = t;
  }
}

// emptyBracketRE is a bracket the folded keys emptied: "（`a` 和 `b`）".
const emptyBracketRE = /\s*[（(]\s*(?:[和与及、，,；;]\s*)*[）)]/g;

// listSepRE is the text between two items of a list: "`a`、`b`", "`a` 和 `b`".
const listSepRE = /^\s*[、，,；;和与及]\s*$/;

// lineBefore is the text of node's line up to node: its inline siblings back
// to a line break, through the <strong>/<em> it sits in, so a bold name
// still reads as the name before a key.
function lineBefore(node) {
  let s = '';
  const line = () => s.slice(s.lastIndexOf('\n') + 1);
  for (let n = node; n && !n.matches('li, td, th, blockquote, .event-content'); n = n.parentElement) {
    for (let p = n.previousSibling; p; p = p.previousSibling) {
      if (p.nodeType === Node.ELEMENT_NODE && p.matches('br, div, ul, ol, pre, table, blockquote')) return line();
      s = p.textContent + s;
    }
  }
  return line();
}

// knownNames are the names a reply may have written a session by: the
// Planner reads a session's whole summary or the 任务 title, while the
// dashboard shows a shortened title, so any of them counts. Empty for a
// session known to none, which only its kind names.
function knownNames(key) {
  const s = sessionList.sessionsData[sid(key, plannerHost.node)];
  const names = [s?.user_label, s?.summary, listedTitle(key), pc.tasks.get(key), pc.health.get(key)?.name];
  const out = [];
  names.forEach((n) => {
    const t = String(n || '').trim();
    if (!t) return;
    out.push(t);
    // An automatic title cut short ends in "…": the text before it is the name.
    if (t.endsWith('…') && t.length > 1) out.push(t.slice(0, -1).trimEnd());
  });
  return out;
}

// namedBefore tells whether the line before a key already gave the
// session's name: "名字 `key`", "名字：`key`", or the key in a bracket
// after it, "名字（…，`key`）". For a session known to none any text before
// the key is taken as its name, as before: its kind name ("派出的任务") would
// only repeat what the text said. In a list it is still named, or the
// separator before the next key would be left bare.
function namedBefore(before, key, inList) {
  const names = knownNames(key);
  const head = before.replace(/\s*[：:]?\s*$/, '');
  if (!names.length) return !inList && /[^\s、，,；;（(：:]$/.test(head);
  if (names.some((n) => head.endsWith(n))) return true;
  const open = Math.max(before.lastIndexOf('（'), before.lastIndexOf('('));
  const out = open >= 0 && !/[）)]/.test(before.slice(open)) && before.slice(0, open).trimEnd();
  return out !== false && names.some((n) => out.endsWith(n));
}

// startsWithName is whether a bracket's text opens with name as a word of
// its own: "（iso 周报，20 分钟）", not "（iso 周报整理）".
function startsWithName(text, name) {
  return text.startsWith(name) && /^(?:$|[\s，,、；;：:])/.test(text.slice(name.length));
}

// foldKeys puts names in place of the backticked keys a reply names
// sessions by (§6.4, §15.12 ①): the cards under it carry the keys, and the
// user reads names. A key that only repeats the name the text gave right
// before it goes, and so does a bracket holding only keys; dropping any
// other would cut the sentence's object ("派出了。") or leave "：、、".
function foldKeys(ev, keys) {
  const emptied = new Set();
  const codes = [...ev.querySelectorAll('.event-content code')]
    .filter((code) => !code.closest('pre, .pl-scards') && keys.has(code.textContent.trim()));
  // Decided before any key is folded: folding changes the siblings.
  const runs = [];
  codes.forEach((code) => {
    const sep = code.previousSibling;
    const run = runs[runs.length - 1];
    if (run && sep?.nodeType === Node.TEXT_NODE && listSepRE.test(sep.data) && sep.previousSibling === run[run.length - 1]) run.push(code);
    else runs.push([code]);
  });
  const text = (n) => (n?.nodeType === Node.TEXT_NODE ? n.data : '');
  const drop = new Set();
  runs.forEach((run) => {
    const bracketed = /[（(]\s*$/.test(text(run[0].previousSibling)) && /^\s*[）)]/.test(text(run[run.length - 1].nextSibling));
    run.forEach((code) => {
      if (bracketed || namedBefore(lineBefore(code), code.textContent.trim(), run.length > 1)) drop.add(code);
    });
  });
  codes.forEach((code) => {
    const name = sessionTitle(code.textContent.trim());
    const prev = code.previousSibling;
    const next = code.nextSibling;
    if (!drop.has(code)) {
      // "`key`（名字…）" names it right after: the name once, not twice.
      const m = next?.nodeType === Node.TEXT_NODE && /^\s*[（(]\s*([^（）()]*?)\s*[）)]/.exec(next.data);
      if (m && m[1] === name) next.data = next.data.slice(m[0].length);
      if (!m || !startsWithName(m[1], name) || m[1] === name) {
        code.replaceWith(name);
        return;
      }
    }
    if (prev?.nodeType === Node.TEXT_NODE) {
      if (next?.nodeType === Node.TEXT_NODE && /[（(]$/.test(prev.data) && /^[）)]/.test(next.data)) {
        prev.data = prev.data.slice(0, -1);
        next.data = next.data.slice(1);
      }
      prev.data = prev.data.trimEnd();
      if (/[：:]$/.test(prev.data)) {
        // "名字：`key`" said the name: the colon goes when nothing is left
        // after it, and what is left closes up to it.
        const after = next?.nodeType === Node.TEXT_NODE ? next.data : next && !next.matches('br') ? next.textContent : '';
        if (/^\s*(?:[，,。.、；;！!？?）)]|$)/.test(after)) prev.data = prev.data.slice(0, -1);
        else if (next?.nodeType === Node.TEXT_NODE) next.data = next.data.trimStart();
      }
      // The separator before a key that closes a bracket or the line has
      // nothing left to separate: "（名字 work，`key`）" reads "（名字 work）".
      if (!next || next.nodeType === Node.TEXT_NODE && /^\s*[）)]/.test(next.data)) prev.data = prev.data.replace(/[，,、；;]$/, '');
    }
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

// 撤回 a memory the Planner kept: the gateway takes the file and its index
// line back; one already gone (taken back before, or never kept) says so.
registerActions({
  'planner-memory-forget': async (el) => {
    el.disabled = true;
    try {
      await fetchJSON(plannerAPI(NZ_CONTRACT.API.planner_memory_forget), {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ workspace: el.dataset.ws || '', name: el.dataset.name || '' }), timeoutMs: 8000,
      });
      el.textContent = '已撤回';
    } catch (e) {
      const status = e && e.status;
      if (status === 404) {
        el.textContent = '已撤回';
        showToast('这条记忆已经撤回，或没有记下', 'info');
        return;
      }
      el.disabled = false;
      showToast(status === 409 ? '这条记忆不是 Planner 记的，没有撤回' : '撤回失败，稍后再试', 'error');
    }
  },
});
