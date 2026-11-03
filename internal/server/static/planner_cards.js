// planner_cards.js — the cards the Planner conversation renders in place of
// raw text (docs/rfc/planner.md §6.2, §10 D77, §10.1; §15.1 PR ⑬):
//
//   - action blocks (```naozhi-action fences) become readable cards. A
//     dispatch card shows the state of its confirmation, the inbox decision
//     item the left column's 等你 lists, and answers it with the same
//     /api/inbox/decide action. The item is the one whose source is
//     `dispatch:<event uuid>#<n>`, n counting the event's naozhi-action
//     blocks from 0 — the identity the gateway gives the confirmation. A
//     block that is not JSON of a known type shows as 看不懂; whether a
//     block runs is the gateway's call, not this reading's.
//   - sessions the conversation names — a handed-over session
//     (<naozhi-session-ref>, rewritten to a fence by planner_view.js), a
//     `task:` key or a listed session's key in the Planner's text, the sender
//     of a report — become one-line cards (名字 · 健康状态 · 最后活动) that
//     open the session in the page's middle column.
//
// Health comes from GET /api/system/health, refetched at most every
// HEALTH_MIN_MS; a session the table does not hold yet falls back to its
// listed state.
import { NZ_CONTRACT } from './contract.js';
import { sessionList } from './state.js';
import { esc, escAttr, fetchJSON } from './nz_util.js';
import { sid } from './session_ident.js';
import { HEALTH_STATES } from './system_view.js';
import { formatAbsTime } from './utilities.js';

const PLANNER_KEY = 'planner:main';
const HEALTH_MIN_MS = 20000;
// MAX_CARDS bounds the session cards under one bubble.
const MAX_CARDS = 6;
const TASK_KEY_RE = /\btask:[A-Za-z0-9_-]+/g;

const pc = {
  health: new Map(), // unit id → health.Unit
  healthAt: 0,
  healthLoading: false,
  inboxOpen: [],
  inboxClosed: [],
};

// ---- session cards ----

export function agoText(ms) {
  const m = Math.floor((Date.now() - ms) / 60000);
  if (m < 1) return '刚刚';
  if (m < 60) return m + ' 分钟前';
  const h = Math.floor(m / 60);
  if (h < 24) return h + ' 小时前';
  return Math.floor(h / 24) + ' 天前';
}

export function sessionTitle(key) {
  const s = sessionList.sessionsData[sid(key, 'local')];
  return (s && (s.user_label || s.summary || s.last_prompt)) || key;
}

// healthOf is the unit's state, else one read off the listed session.
function healthOf(key, s) {
  const u = pc.health.get(key);
  if (u && HEALTH_STATES[u.state]) return HEALTH_STATES[u.state];
  if (!s) return null;
  if (s.state === 'running') return HEALTH_STATES.active;
  if (s.state === 'dead') return { label: '已结束', cls: 'off' };
  return HEALTH_STATES.idle;
}

// sessionStatusHtml is a session's one status line: 名字 · 健康状态 · 最后活动
// (and what it is doing while it runs).
export function sessionStatusHtml(key) {
  const s = sessionList.sessionsData[sid(key, 'local')];
  const st = healthOf(key, s);
  const u = pc.health.get(key);
  const last = (u && u.last_active && Date.parse(u.last_active)) || (s && s.last_active) || 0;
  let tail = '';
  if (!s && !u) tail = '不在会话列表里';
  else if (s && s.state === 'running' && s.last_activity) tail = s.last_activity;
  else if (last) tail = '最后活动 ' + agoText(last);
  return '<span class="pl-scard-name">' + esc(sessionTitle(key)) + '</span>' +
    (st ? '<span class="hl-state ' + st.cls + '"' + (u && u.evidence ? ' title="' + escAttr(u.evidence) + '"' : '') + '>' + esc(st.label) + '</span>' : '') +
    (tail ? '<span class="pl-scard-time"' + (last ? ' title="' + escAttr(formatAbsTime(last)) + '"' : '') + '>' + esc(tail) + '</span>' : '');
}

export function sessionCardHtml(key) {
  return '<button type="button" class="pl-scard" data-action="planner-open-session" data-key="' + escAttr(key) + '">' +
    '<span class="pl-scard-line">' + sessionStatusHtml(key) + '</span>' +
    '<span class="pl-scard-open" aria-hidden="true">打开 ›</span></button>';
}

// paintSessionCards refreshes the status lines of the cards under root.
export function paintSessionCards(root) {
  if (!root) return;
  root.querySelectorAll('.pl-scard[data-key]').forEach((el) => {
    const line = el.querySelector('.pl-scard-line');
    const html = sessionStatusHtml(el.dataset.key);
    if (line && line.innerHTML !== html) line.innerHTML = html;
  });
}

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

const ACTION_LABEL = { approve: '批准', reject: '驳回' };
const ACTION_TYPES = new Set(['dispatch', 'promise', 'ask']);

const isStr = (v) => typeof v === 'string';
const str = (v) => (isStr(v) ? v : '');

// decisionActsHtml is an item's answer buttons (a decision's actions, or a
// Planner question's options, §6.2 ask); the left column's card and the
// conversation's dispatch card share it.
export function decisionActsHtml(it) {
  if (!it || !it.decision || !Array.isArray(it.decision.actions)) return '';
  return '<div class="pl-card-acts">' + it.decision.actions.map((a, i) =>
    '<button type="button" class="pl-btn' + (i === 0 ? ' pri' : '') + '" data-action="planner-inbox-decide" data-id="' + escAttr(it.id) + '" data-act="' + escAttr(a) + '">' +
      esc(ACTION_LABEL[a] || a) + '</button>').join('') + '</div>';
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
  return isNaN(t) ? due : formatAbsTime(t);
}

// actionCardHtml renders one block; source is the inbox source of its
// dispatch confirmation, "" when the bubble has no uuid.
function actionCardHtml(raw, source) {
  const { action: a, error } = parseAction(raw);
  if (!a) {
    return '<div class="pl-act pl-act-bad"><div class="pl-act-h">看不懂的动作块</div>' +
      '<div class="pl-act-why">' + esc(error) + '</div><pre class="pl-act-raw">' + esc(raw) + '</pre></div>';
  }
  if (a.type === 'dispatch') {
    const title = str(a.title);
    return '<div class="pl-act pl-act-dispatch" data-source="' + escAttr(source) + '">' +
      '<div class="pl-act-h">派活提议' + (title ? ' · ' + esc(title) : '') + '</div>' +
      rowsHtml([['工作区', str(a.workspace)], ['模型', str(a.model)], ['第一条消息', str(a.message)]]) +
      '<div class="pl-act-state"></div></div>';
  }
  if (a.type === 'promise') {
    return '<div class="pl-act pl-act-promise"><div class="pl-act-h">登记承诺</div>' +
      rowsHtml([['内容', str(a.text)], ['到期', dueText(str(a.due))], ['完成条件', str(a.close_when)]]) +
      '<div class="pl-act-note">登记后列在左栏"答应你的"</div></div>';
  }
  const opts = (Array.isArray(a.options) ? a.options : []).filter(isStr).map((o) => '<span class="pl-act-opt">' + esc(o) + '</span>').join('');
  return '<div class="pl-act pl-act-ask"><div class="pl-act-h">问你</div>' +
    '<div class="pl-act-q">' + esc(str(a.question)) + '</div>' +
    (opts ? '<div class="pl-act-opts">' + opts + '</div>' : '') +
    '<div class="pl-act-note">在左栏"等你"里回答</div></div>';
}

// setInboxItems takes the inbox the left column shows (open and recently
// closed) and repaints the dispatch cards under root from it.
export function setInboxItems(open, closed, root) {
  pc.inboxOpen = open || [];
  pc.inboxClosed = closed || [];
  paintDispatchCards(root);
}

// dispatchItem is the decision item the dispatch block with this source
// opened, open or recently closed.
function dispatchItem(source) {
  if (!source) return null;
  const is = (it) => it.source === source && it.kind === 'decision' && it.decision;
  return pc.inboxOpen.find(is) || pc.inboxClosed.find(is) || null;
}

function dispatchStateHtml(it) {
  if (!it) return '<span class="pl-act-note">状态未知：确认卡片不在"等你"里（还没生成，或已归档）</span>';
  if (it.state === 'open') return decisionActsHtml(it) + '<span class="pl-act-note">左栏"等你"里是同一张</span>';
  const answer = it.decision.answer;
  const label = answer === 'approve' ? '已批准' : answer === 'reject' ? '已驳回' : '已关闭';
  const key = it.decision.payload && it.decision.payload.key;
  return '<span class="pl-act-done' + (answer === 'approve' ? ' ok' : '') + '">' + label + '</span>' +
    (answer === 'approve' && isStr(key) && key ? sessionCardHtml(key) : '');
}

function paintDispatchCards(root) {
  if (!root) return;
  root.querySelectorAll('.pl-act-dispatch').forEach((card) => {
    const box = card.querySelector('.pl-act-state');
    // Answering disables the buttons; their state repaints from the refetch.
    if (box) box.innerHTML = dispatchStateHtml(dispatchItem(card.dataset.source));
  });
}

// ---- decoration ----

// decorateEvents turns the fences and the session mentions of the bubbles
// under root not yet seen into cards.
export function decorateEvents(root) {
  if (!root) return;
  root.querySelectorAll('.event:not([data-pl-dec])').forEach((ev) => {
    ev.dataset.plDec = '1';
    ev.querySelectorAll('code[data-lang="naozhi-session-ref"]').forEach((code) => {
      let ref = null;
      try { ref = JSON.parse(code.textContent); } catch { /* left as code */ }
      const wrap = code.closest('.md-code-wrap');
      if (!wrap || !ref || !isStr(ref.key) || !ref.key) return;
      wrap.outerHTML = '<div class="pl-handoff"><span class="pl-handoff-l">交给 Planner</span>' + sessionCardHtml(ref.key) + '</div>';
    });
    const uuid = ev.dataset.uuid || '';
    ev.querySelectorAll('code[data-lang="naozhi-action"]').forEach((code, n) => {
      const wrap = code.closest('.md-code-wrap');
      if (wrap) wrap.outerHTML = actionCardHtml(code.textContent, uuid ? 'dispatch:' + uuid + '#' + n : '');
    });
    if (!ev.matches('.text, .peer_message, .system')) return;
    const carded = new Set([...ev.querySelectorAll('.pl-scard[data-key]')].map((b) => b.dataset.key));
    const keys = mentionedKeys(ev).filter((k) => !carded.has(k)).slice(0, MAX_CARDS);
    if (keys.length) {
      ev.querySelector('.event-content')?.insertAdjacentHTML('beforeend', '<div class="pl-scards">' + keys.map(sessionCardHtml).join('') + '</div>');
    }
  });
  paintDispatchCards(root);
}
