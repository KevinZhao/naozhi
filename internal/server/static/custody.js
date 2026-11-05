// custody.js — custody in the dashboard (planner RFC §5.4, D93): the gateway
// replying for you on a session you are not at. It keeps GET /api/custody
// (the pause and every session in custody) for the session header's chip,
// the Planner page's task cards and the system view's health table; the chip
// switches a session through PUT /api/custody, the Planner page's "⋯" menu
// pauses every session through /api/custody/pause. Shown only with the
// Planner on (custody runs only then) and for this node's sessions.
//
// It also tells the gateway which session you are at: a viewing frame names
// the session this tab has open while the tab is visible and focused ('' for
// none), renewed well within custody.ViewFor; a closed socket ends it on the
// server. Custody holds back on a session while any tab is at it.
//
// A message custody sent carries the prefix "⚙ 托管 · <原因> · 第 n/8 次"
// (custody.Prefix): custodyMessage splits it off so the bubble names custody
// as its sender, and an answer it gave on your behalf (按建议执行) can be
// taken back with one click, which sends the session the words of SPEC D93.
// Layering: never import dashboard.js back.
import { NZ_CONTRACT } from './contract.js';
import { selection, sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { isDiscoveredKey, sid } from './session_ident.js';
import { isMobile } from './utilities.js';
import { wsm } from './ws_manager.js';

// VIEW_RENEW_MS renews a viewing report: the gateway lets one lapse after a
// minute (custody.ViewFor). VIEW_CHECK_MS is how often the tab looks again
// at what it shows, for the changes no event announces.
const VIEW_RENEW_MS = 30000;
const VIEW_CHECK_MS = 2000;

// REASON_SUGGESTED is custody.ReasonSuggested: an answer to a question,
// given by its recommended option.
const REASON_SUGGESTED = '按建议执行';
const PREFIX_RE = /^⚙ 托管 · (.+?) · 第 (\d+)\/(\d+) 次[ \t]*(?:\n|$)/;
const NOT_IN_CUSTODY = /^(planner|sys|scout):/;

const STATE_TEXT = { off: '关', on: '开', deferred: '已暂缓', stopped: '已停', paused: '已暂停' };

export const custody = {
  enabled: null, // the Planner on (GET /api/planner), null until known
  paused: false,
  pausedAt: 0,
  byKey: new Map(), // key → custody.Status of the sessions in custody
  loading: null,
};

// The session this tab last reported it is at, and when.
const view = { key: '', at: 0 };
// The custody answers taken back from this tab, by bubble.
const undone = new Set();

const changed = () => nzBus.dispatchEvent(new CustomEvent('custody:changed'));

// coverable reports whether custody can cover key on node: one of this
// node's sessions the dashboard lists, not the Planner, a daemon or a scout.
export function coverable(key, node) {
  if (custody.enabled !== true || !key || (node && node !== 'local')) return false;
  if (isDiscoveredKey(key) || NOT_IN_CUSTODY.test(key)) return false;
  return !!sessionList.sessionsData[sid(key, 'local')];
}

// statusOf is key's custody as this tab shows it: in custody but at the
// session (this tab reports it) reads as held back, as the gateway holds it.
export function statusOf(key) {
  const st = custody.byKey.get(key) || { key, on: false, state: 'off', sent: 0, max: 8 };
  if (st.state === 'on' && view.key === key) return { ...st, state: 'deferred', why: '你在看' };
  return st;
}

// stateText is a status in a word or two: 开 / 关 / 已暂缓 / 已停 / 已暂停.
export const stateText = (st) => STATE_TEXT[st.state] || STATE_TEXT.off;

function chipTitle(st, toggle) {
  const lines = [];
  switch (st.state) {
    case 'on': lines.push('托管开着：出错时自动重试，有推荐选项的提问按建议回答，回合停下交给 Planner 判断'); break;
    case 'deferred': lines.push('托管暂缓：你在看这个会话，或 5 分钟内给它发过消息，托管先不替你回话；你离开后恢复'); break;
    case 'stopped': lines.push('托管已停：' + (st.why || '') + '，等你接手'); break;
    case 'paused': lines.push('托管已全部暂停（在 Planner 页面的"⋯"里恢复）'); break;
    default: lines.push('托管关着：这个会话出错或停下时等你处理');
  }
  if (st.on && st.max) lines.push('这条任务链已自动发了 ' + (st.sent || 0) + '/' + st.max + ' 次');
  if (toggle) lines.push(st.state === 'stopped' ? '点一下重新开始托管（重新计数）' : st.on ? '点一下关掉托管' : '点一下打开托管');
  return lines.join('\n');
}

// custodyChipHtml is key's chip: the header's switch, or with toggle false
// the same mark as plain text (a task card, a table cell).
export function custodyChipHtml(key, toggle) {
  const st = statusOf(key);
  const why = st.state === 'stopped' && st.why ? st.why : st.state === 'deferred' ? st.why : '';
  const inner = '<span class="custody-chip-l">托管</span><span class="custody-chip-s">' + esc(stateText(st)) + '</span>' +
    (why ? '<span class="custody-chip-why">' + esc(why) + '</span>' : '');
  const cls = 'custody-chip custody-' + escAttr(st.state);
  const title = escAttr(chipTitle(st, toggle));
  if (!toggle) return '<span class="' + cls + '" title="' + title + '">' + inner + '</span>';
  return '<button type="button" class="' + cls + '" data-action="custody-toggle" data-key="' + escAttr(key) + '" aria-pressed="' + (st.on ? 'true' : 'false') +
    '" title="' + title + '" aria-label="' + escAttr('托管：' + stateText(st) + (why ? '，' + why : '')) + '">' + inner + '</button>';
}

// paintCustodyChip fills the session header's #header-custody for the
// selected session; renderMainShell builds it empty.
export function paintCustodyChip() {
  const el = document.getElementById('header-custody');
  if (!el) return;
  const html = coverable(selection.key, selection.node) ? custodyChipHtml(selection.key, true) : '';
  if (el.innerHTML !== html) el.innerHTML = html;
}

async function plannerEnabled() {
  if (custody.enabled !== null) return custody.enabled;
  try {
    const s = await fetchJSON(NZ_CONTRACT.API.planner, { timeoutMs: 8000 });
    custody.enabled = !!(s && s.enabled);
  } catch (e) {
    // No Planner route (404): no custody. Anything else is asked again.
    if (e.status === 404) custody.enabled = false;
  }
  return custody.enabled === true;
}

function noteList(res) {
  custody.paused = !!res?.paused;
  custody.pausedAt = res?.paused_at ? Date.parse(res.paused_at) || 0 : 0;
  if (Array.isArray(res?.sessions)) custody.byKey = new Map(res.sessions.filter((s) => s && s.key).map((s) => [s.key, s]));
}

// refreshCustody reads GET /api/custody again; one read at a time.
export function refreshCustody() {
  if (custody.loading) return custody.loading;
  custody.loading = (async () => {
    try {
      if (!(await plannerEnabled())) return;
      noteList(await fetchJSON(NZ_CONTRACT.API.custody, { timeoutMs: 8000 }));
    } catch { /* the next refresh tries again */ }
    finally { custody.loading = null; changed(); reportViewing(); }
  })();
  return custody.loading;
}

const failText = (e) => String(e?.message || e).replace(/^HTTP \d+: /, '').trim();

// setCustodyPaused pauses or resumes custody for every session, then reads
// every session's custody again: each one's state follows the pause.
export async function setCustodyPaused(paused) {
  try {
    noteList(await fetchJSON(NZ_CONTRACT.API.custody_pause, { method: paused ? 'PUT' : 'DELETE', timeoutMs: 8000 }));
  } catch (e) {
    showToast((paused ? '暂停托管' : '恢复托管') + '失败：' + failText(e), 'error');
  }
  changed();
  await refreshCustody();
}

async function toggle(el) {
  const key = el.dataset.key;
  if (!key) return;
  const st = statusOf(key);
  // A stopped one turns on afresh: the user's switch restarts the count.
  const on = st.state === 'stopped' ? true : !st.on;
  el.disabled = true;
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.custody, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key, on }), timeoutMs: 8000,
    });
    if (res && res.key) custody.byKey.set(res.key, res);
  } catch (e) {
    showToast((on ? '没有打开托管：' : '没有关掉托管：') + failText(e), 'error');
  }
  el.disabled = false;
  changed();
}

// ---- you are at the session ----

// viewedKey is the session this tab shows you now, '' when it shows none or
// you are not at the tab: the session view's (the session list on a phone
// shows none), or the one opened on the Planner page.
function viewedKey() {
  if (custody.enabled !== true) return '';
  if (document.visibilityState !== 'visible' || !document.hasFocus()) return '';
  let key = '';
  if (ui.activeView === 'chat') {
    if (isMobile() && !document.body.classList.contains('mobile-chat-view')) return '';
    if (selection.node && selection.node !== 'local') return '';
    key = selection.key || '';
  } else if (ui.activeView === 'planner') {
    key = document.getElementById('pl-sess')?.dataset.key || '';
  }
  return key && !isDiscoveredKey(key) ? key : '';
}

// reportViewing sends a viewing frame when what this tab shows changed, or
// to renew it.
function reportViewing() {
  if (!wsm.isConnected()) {
    // The gateway forgot this tab with its socket.
    if (view.key) { view.key = ''; changed(); }
    return;
  }
  const key = viewedKey();
  const now = Date.now();
  if (key === view.key && (!key || now - view.at < VIEW_RENEW_MS)) return;
  if (!wsm.send({ type: NZ_CONTRACT.WS.viewing, key })) return;
  const moved = key !== view.key;
  view.key = key;
  view.at = now;
  if (moved) changed();
}

// ---- custody's messages ----

// custodyMessage splits a custody message's prefix off its text: the
// reason, its count of max, and the body; null for any other text.
export function custodyMessage(text) {
  const m = PREFIX_RE.exec(text || '');
  if (!m) return null;
  return { reason: m[1], n: Number(m[2]), max: Number(m[3]), body: text.slice(m[0].length).trim() };
}

// undoText is what taking back custody's answer choice says (SPEC D93).
export const undoText = (choice) => '撤回刚才按建议选的 ' + choice + '，先停下等我决定';

// custodyBubbleHtml is a custody message's bubble content: who sent it and
// why, faint, then its body; an answer given by the recommended option
// says so, with 撤回. renderBody renders the body text. sk is the session
// the bubble is in, id the bubble's.
export function custodyBubbleHtml(text, sk, id, renderBody) {
  const m = custodyMessage(text);
  if (!m) return '<div class="custody-source">⚙ 托管</div>' + renderBody(text);
  const src = '<div class="custody-source">⚙ 托管 · ' + esc(m.reason) + ' · 第 ' + m.n + '/' + m.max + ' 次</div>';
  if (m.reason !== REASON_SUGGESTED || !m.body) return src + renderBody(m.body);
  const choice = m.body.replace(/[.。]\s*$/, '');
  const done = undone.has(id);
  const btn = done
    ? '<span class="custody-undone">已发撤回</span>'
    : '<button type="button" class="pl-link custody-undo" data-action="custody-undo" data-key="' + escAttr(sk.key) + '" data-node="' + escAttr(sk.node || 'local') +
      '" data-choice="' + escAttr(choice) + '" data-id="' + escAttr(id) + '" title="' + escAttr('给会话发：' + undoText(choice)) + '">撤回</button>';
  return src + '<div class="custody-suggested">已按建议执行：<span class="custody-choice">' + esc(choice) + '</span>' +
    '<span class="custody-sep" aria-hidden="true">·</span>' + btn + '</div>';
}

async function undo(el) {
  const { key, node, choice, id } = el.dataset;
  if (!key || !choice) return;
  el.disabled = true;
  const payload = { key, text: undoText(choice) };
  if (node && node !== 'local') payload.node = node;
  try {
    await fetchJSON(NZ_CONTRACT.API.sessions_send, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload), timeoutMs: 15000,
    });
    if (id) undone.add(id);
    el.outerHTML = '<span class="custody-undone">已发撤回</span>';
  } catch (e) {
    el.disabled = false;
    showToast('没有发出撤回：' + failText(e), 'error');
  }
}

registerActions({
  'custody-toggle': toggle,
  'custody-undo': undo,
});

nzBus.addEventListener('custody:changed', paintCustodyChip);
// What custody did on a session (session_notes.js passes its notes on).
nzBus.addEventListener('custody:note', () => { refreshCustody(); });
nzBus.addEventListener('session:select', () => { reportViewing(); paintCustodyChip(); refreshCustody(); });
wsm.onReady(() => { view.key = ''; reportViewing(); refreshCustody(); });
wsm.onStateChange(() => reportViewing());
document.addEventListener('visibilitychange', reportViewing);
window.addEventListener('focus', reportViewing);
window.addEventListener('blur', reportViewing);
setInterval(reportViewing, VIEW_CHECK_MS);
