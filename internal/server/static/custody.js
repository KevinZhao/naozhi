// custody.js — custody in the dashboard (planner RFC §5.4, D93): the gateway
// replying for you on a session you are not at. It keeps GET /api/custody
// (the pause and every session in custody) for the session header's chip,
// the one place custody shows as a switch (D94); a task card marks it only
// when off or stopped, a failed row and the health table say 托管已停. The
// chip switches a session through PUT /api/custody; the Planner page's one
// pause (PUT /api/inbox/pause) pauses custody with the pushes. Shown only
// with the Planner on (custody runs only then) and for the sessions of the
// node it runs on: with the Planner on another node (planner_host.js) the
// list, the switch and 撤回 are that node's, through /api/remote/{node}/.
//
// Beside the chip is 观察 (§15.12 ⑥, D104), a second switch: the gateway
// observing a session you opened (its digest, its card in 任务's 你开的)
// sends nothing, so it is on by default; GET /api/custody lists, besides
// the sessions in custody, those with it turned off (so the switch shows
// once the list is read), and PUT /api/custody {key, watch} switches it. A
// dispatched session is a task already: no switch there.
//
// It also tells the gateway which session you are at: a viewing frame names
// the session this tab has open while the tab is visible and focused ('' for
// none), renewed well within custody.ViewFor; a closed socket ends it on the
// server. Custody holds back on a session while any tab is at it.
//
// A message custody sent is its body alone, the event marked source
// "custody" with custody_reason and custody_n (D94); before that the text
// led with "⚙ 托管 · <原因> · 第 n/8 次", which custodyMessage still splits
// off. The bubble names custody as its sender, and the latest answer it gave
// on your behalf (按建议执行) can be taken back with one click: POST
// /api/custody/undo finds that answer on the gateway and sends the session
// the words of SPEC D93 in your name.
// Layering: never import dashboard.js back.
import { NZ_CONTRACT } from './contract.js';
import { selection, sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { isDiscoveredKey, sid } from './session_ident.js';
import { isMobile } from './utilities.js';
import { wsm } from './ws_manager.js';
import { isLocalNode, nodeAPI, plannerAPI, plannerHost } from './planner_host.js';

// VIEW_RENEW_MS renews a viewing report: the gateway lets one lapse after a
// minute (custody.ViewFor). VIEW_CHECK_MS is how often the tab looks again
// at what it shows, for the changes no event announces.
const VIEW_RENEW_MS = 30000;
// UNDO_URL is custody's 撤回; the route ships with the suggested answers
// (Engine.Undo), so the contract may not name it yet.
const UNDO_URL = NZ_CONTRACT.API.custody_undo || NZ_CONTRACT.API.custody + '/undo';
const VIEW_CHECK_MS = 2000;

// REASON_SUGGESTED is custody.ReasonSuggested: an answer to a question,
// given by its recommended option.
const REASON_SUGGESTED = '按建议执行';
const PREFIX_RE = /^⚙ 托管 · (.+?) · 第 (\d+)\/(\d+) 次[ \t]*(?:\n|$)/;
const NOT_IN_CUSTODY = /^(planner|sys|scout):/;
// NOT_WATCHED are the sessions coverable that are not observed (custody's
// watchable): a dispatched one is a task already, a cron run and a scratch
// session are one-shot.
const NOT_WATCHED = /^(task|cron|scratch):/;

// A session custody holds back on (you are at it, or just sent it a message)
// is still in custody: it reads 开, the hold is in the chip's title.
const STATE_TEXT = { off: '关', on: '开', deferred: '开', stopped: '已停', paused: '已暂停' };
// RECOMMENDED_RE is the recommended option's marker, read by custody on the
// raw label and left out wherever the label is shown.
const RECOMMENDED_RE = /\s*(?:\((?:Recommended|recommended)\)|（推荐）)/g;

// optionText is an option's label as shown: without the recommended marker.
export const optionText = (label) => String(label || '').replace(RECOMMENDED_RE, '').trim();

export const custody = {
  enabled: null, // the Planner on (GET /api/planner), null until known
  paused: false,
  pausedAt: 0,
  byKey: new Map(), // key → custody.Status of the sessions in custody or with 观察 off
  loading: null,
  readAt: 0, // when GET /api/custody was last read
  listed: false, // byKey holds a list read (the Planner node's): 观察's state is known
};

// The session this tab last reported it is at, and when.
const view = { key: '', at: 0 };
// The custody answers taken back from this tab, by bubble.
const undone = new Set();

const changed = () => nzBus.dispatchEvent(new CustomEvent('custody:changed'));

// coverable reports whether custody can cover key on node: one of the
// Planner's node's sessions the dashboard lists, not the Planner, a daemon
// or a scout.
export function coverable(key, node) {
  if (custody.enabled !== true || !key || (node || 'local') !== plannerHost.node) return false;
  if (isDiscoveredKey(key) || NOT_IN_CUSTODY.test(key)) return false;
  return !!sessionList.sessionsData[sid(key, plannerHost.node)];
}

// watchable reports whether the gateway can observe key on node: one
// custody covers, not dispatched, not a cron run or a scratch session.
export function watchable(key, node) {
  return coverable(key, node) && !NOT_WATCHED.test(key);
}

// watchOf reports whether key is observed as GET /api/custody last said
// it: on unless listed with it off (the list holds those).
export function watchOf(key) {
  const st = custody.byKey.get(key);
  return !st || st.watch !== false;
}

// statusOf is key's custody as GET /api/custody last said it.
export function statusOf(key) {
  return custody.byKey.get(key) || { key, on: false, state: 'off', sent: 0, max: 8 };
}

// stateText is a status in a word or two: 开 / 关 / 已停 / 已暂停.
export const stateText = (st) => STATE_TEXT[st.state] || STATE_TEXT.off;

function chipTitle(st, toggle) {
  const lines = [];
  switch (st.state) {
    case 'on':
    case 'deferred':
      lines.push('托管开着：有推荐选项的提问按建议回答，回合停下交给 Planner 判断');
      lines.push('你在看或刚给它发过消息时，先不替你回话');
      break;
    case 'stopped': lines.push('托管已停' + (st.why ? '：' + st.why : '，等你接手')); break;
    case 'paused': lines.push('推送与托管已暂停（在 Planner 页面的"⋯"里恢复）'); break;
    default: lines.push('托管关着：这个会话停下时等你处理');
  }
  if (toggle) lines.push(st.state === 'stopped' ? '点一下重新开始托管（重新计数）' : st.on ? '点一下关掉托管' : '点一下打开托管');
  return lines.join('\n');
}

// custodyChipHtml is key's chip: the header's switch, or with toggle false
// the same mark as plain text (a task card, a table cell).
export function custodyChipHtml(key, toggle) {
  const st = statusOf(key);
  // A stop's reason is in the title only (§15.10 Q4): the chip says 已停.
  const inner = '<span class="custody-chip-l">托管</span><span class="custody-chip-s">' + esc(stateText(st)) + '</span>';
  const cls = 'custody-chip custody-' + escAttr(st.state === 'deferred' ? 'on' : st.state);
  const title = escAttr(chipTitle(st, toggle));
  if (!toggle) return '<span class="' + cls + '" title="' + title + '">' + inner + '</span>';
  return '<button type="button" class="' + cls + '" data-action="custody-toggle" data-key="' + escAttr(key) + '" aria-pressed="' + (st.on ? 'true' : 'false') +
    '" title="' + title + '" aria-label="' + escAttr('托管：' + stateText(st) + (st.state === 'stopped' && st.why ? '，' + st.why : '')) + '">' + inner + '</button>';
}

// watchChipHtml is key's 观察 switch, beside the custody chip.
export function watchChipHtml(key) {
  const on = watchOf(key);
  const title = on ? '观察开着：网关看着它、写摘要、放进任务的「你开的」；不发任何消息\n点一下关掉' : '观察关着：不进任务、不写摘要\n点一下打开';
  return '<button type="button" class="watch-chip watch-' + (on ? 'on' : 'off') + '" data-action="watch-toggle" data-key="' + escAttr(key) +
    '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + escAttr(title) + '" aria-label="' + (on ? '观察：开' : '观察：关') + '">' +
    '<span class="custody-chip-l">观察</span><span class="custody-chip-s">' + (on ? '开' : '关') + '</span></button>';
}

// paintCustodyChip fills the session header's #header-custody for the
// selected session, custody's chip and the 观察 switch; renderMainShell
// builds it empty.
export function paintCustodyChip() {
  const el = document.getElementById('header-custody');
  if (!el) return;
  const { key, node } = selection;
  // 观察 waits for a list read: off is told only by the list, so before
  // it every session would read 开 and a click could only turn it off.
  const html = (coverable(key, node) ? custodyChipHtml(key, true) : '') + (custody.listed && watchable(key, node) ? watchChipHtml(key) : '');
  if (el.innerHTML !== html) el.innerHTML = html;
}

async function plannerEnabled() {
  if (custody.enabled !== null) return custody.enabled;
  try {
    const s = await fetchJSON(plannerAPI(NZ_CONTRACT.API.planner), { timeoutMs: 8000 });
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
  if (Array.isArray(res?.sessions)) {
    custody.byKey = new Map(res.sessions.filter((s) => s && s.key).map((s) => [s.key, s]));
    custody.listed = true;
  }
}

// refreshCustody reads GET /api/custody again; one read at a time.
export function refreshCustody() {
  if (custody.loading) return custody.loading;
  custody.loading = (async () => {
    try {
      if (!(await plannerEnabled())) return;
      noteList(await fetchJSON(plannerAPI(NZ_CONTRACT.API.custody), { timeoutMs: 8000 }));
    } catch { /* the next refresh tries again */ }
    finally { custody.loading = null; custody.readAt = Date.now(); changed(); reportViewing(); }
  })();
  return custody.loading;
}

const failText = (e) => String(e?.message || e).replace(/^HTTP \d+: /, '').trim();

// setCustodyPaused pauses or resumes custody alone (/api/custody/pause, the
// one pause's alias since D94: the Planner page ends a pause from before the
// two were one with it), then reads every session's custody again.
export async function setCustodyPaused(paused) {
  try {
    noteList(await fetchJSON(plannerAPI(NZ_CONTRACT.API.custody_pause), { method: paused ? 'PUT' : 'DELETE', timeoutMs: 8000 }));
  } catch (e) {
    showToast((paused ? '暂停托管' : '恢复托管') + '失败：' + failText(e), 'error');
  }
  changed();
  await refreshCustody();
}

// toggleToast says what a click on the chip just did: on a phone there is
// no title to hover, and the switch takes effect at once (D88).
export function toggleToast(was, on) {
  if (was === 'stopped') return '已重新开始托管，重新计数';
  // Retrying after an error is every session's (D94), not the switch's.
  return on ? '托管已打开：有推荐选项的提问按建议回答，停下时交给 Planner；再点一下关掉' : '托管已关：停下时等你处理（出错照样自动重试）';
}

async function toggle(el) {
  const key = el.dataset.key;
  if (!key) return;
  const st = statusOf(key);
  // A stopped one turns on afresh: the user's switch restarts the count.
  const on = st.state === 'stopped' ? true : !st.on;
  el.disabled = true;
  try {
    const res = await fetchJSON(plannerAPI(NZ_CONTRACT.API.custody), {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key, on }), timeoutMs: 8000,
    });
    if (res && res.key) custody.byKey.set(res.key, res);
    showToast(toggleToast(st.state, on), 'success', 4000);
  } catch (e) {
    showToast((on ? '没有打开托管：' : '没有关掉托管：') + failText(e), 'error');
  }
  el.disabled = false;
  changed();
}

// watchToast says what a click on 观察 just did (no title to hover on a
// phone).
export const watchToast = (on) => on ? '观察已开：写摘要、放进任务的「你开的」；不发任何消息' : '观察已关：不进任务、不写摘要（托管不受影响）';

async function toggleWatch(el) {
  const key = el.dataset.key;
  if (!key) return;
  const watch = !watchOf(key);
  el.disabled = true;
  try {
    const res = await fetchJSON(plannerAPI(NZ_CONTRACT.API.custody), {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key, watch }), timeoutMs: 8000,
    });
    if (res && res.key) custody.byKey.set(res.key, res);
    // The gateway's answer is what it is now.
    showToast(watchToast(res && typeof res.watch === 'boolean' ? res.watch : watch), 'success', 4000);
  } catch (e) {
    showToast((watch ? '没有打开观察：' : '没有关掉观察：') + failText(e), 'error');
  }
  el.disabled = false;
  changed();
  // 任务's 你开的 gains or loses the session.
  nzBus.dispatchEvent(new CustomEvent('watch:changed', { detail: { key, watch } }));
}

// ---- you are at the session ----

// viewedKey is this node's session this tab shows you now, '' when it shows
// none, another node's, or you are not at the tab: the session view's (the
// session list on a phone shows none), or the one opened on the Planner
// page (shownSession).
function viewedKey() {
  if (custody.enabled !== true) return '';
  const s = shownSession();
  const key = s && isLocalNode(s.node) ? s.key : '';
  return key && !isDiscoveredKey(key) ? key : '';
}

// shownSession is the session this tab shows you now, {key, node}, null
// when it shows none or you are not at the tab: the 会话 view's selection
// (the session list on a phone shows none), or the one open on the Planner
// page beside its conversation, of the node whose Planner it shows. One
// judgment for both ends: viewedKey's viewing frame tells this node's
// gateway, and another node's, which no frame reaches, hears of the
// results you saw from tasks.js (sessionShown).
export function shownSession() {
  if (document.visibilityState !== 'visible' || !document.hasFocus()) return null;
  if (ui.activeView === 'chat') {
    if (!selection.key || isMobile() && !document.body.classList.contains('mobile-chat-view')) return null;
    return { key: selection.key, node: selection.node || 'local' };
  }
  if (ui.activeView === 'planner') {
    const pane = document.getElementById('pl-sess');
    const key = pane && !pane.hidden ? pane.dataset.key || '' : '';
    const node = (pane && pane.dataset.node) || 'local';
    return key && node === plannerHost.node ? { key, node } : null;
  }
  return null;
}

// sessionShown reports whether this tab shows you session key of node now
// (shownSession).
export function sessionShown(key, node) {
  const s = shownSession();
  return !!s && s.key === key && s.node === (node || 'local');
}

// reportViewing sends a viewing frame when what this tab shows changed, or
// to renew it. A move changes the gateway's 已暂缓 of the sessions left and
// entered, so the list is read again; so is a 已暂缓 older than a renewal,
// which lapses on the gateway (ViewFor, NearbyFor) with nothing announcing it.
function reportViewing() {
  if (!wsm.isConnected()) {
    // The gateway forgot this tab with its socket.
    if (view.key) { view.key = ''; changed(); }
    return;
  }
  const key = viewedKey();
  const now = Date.now();
  if (key === view.key && (!key || now - view.at < VIEW_RENEW_MS)) {
    if (now - custody.readAt >= VIEW_RENEW_MS && [...custody.byKey.values()].some((st) => st.state === 'deferred')) refreshCustody();
    return;
  }
  if (!wsm.send({ type: NZ_CONTRACT.WS.viewing, key })) return;
  const moved = key !== view.key;
  view.key = key;
  view.at = now;
  if (moved) { changed(); refreshCustody(); }
}

// ---- custody's messages ----

// custodyMessage is a custody message e (or its text) as the bubble shows
// it: the reason, its count (0 when not known) and the body. The event's own
// custody_reason / custody_n come first; a text from before them leads with
// the prefix, split off here. null for a text with neither.
export function custodyMessage(e) {
  const ev = typeof e === 'string' ? { detail: e } : (e || {});
  const text = ev.detail || ev.summary || '';
  if (ev.custody_reason) return { reason: ev.custody_reason, n: Number(ev.custody_n) || 0, body: text.trim() };
  const m = PREFIX_RE.exec(text);
  if (!m) return null;
  return { reason: m[1], n: Number(m[2]), body: text.slice(m[0].length).trim() };
}

// isSuggestedMessage reports whether e (or its text) is an answer custody
// gave by the recommended option: the bubbles 撤回 can apply to.
export const isSuggestedMessage = (e) => custodyMessage(e)?.reason === REASON_SUGGESTED;

// custodySourceText is the faint line naming custody on its message: 托管 ·
// 原因 · 第 n 次 (the avatar is the ⚙ already; the cap stays the gateway's).
export function custodySourceText(m) {
  return m ? '托管 · ' + m.reason + (m.n ? ' · 第 ' + m.n + ' 次' : '') : '托管';
}

// suggestedChoice is the option an answer by the recommended option chose,
// as shown: the trailing period and the recommended marker left out.
export const suggestedChoice = (body) => optionText(String(body || '').replace(/[.。]\s*$/, ''));

// undoHtml is 撤回 for custody's answer id in session sk (the line it ends:
// a bubble's or an answered question card's), or its done mark.
export function undoHtml(sk, id) {
  if (undone.has(id)) return '<span class="custody-undone">已发撤回</span>';
  return '<button type="button" class="pl-link custody-undo" data-action="custody-undo" data-key="' + escAttr(sk.key) + '" data-node="' + escAttr(sk.node || 'local') +
    '" data-id="' + escAttr(id) + '" title="以你的名义让会话撤回这次按建议选的选项，先停下等你决定">撤回</button>';
}

// custodyBubbleHtml is custody message e's bubble content: who sent it and
// why, faint, then its body (text, the event's shown text); an answer given
// by the recommended option says so, with 撤回 (shown on the session's
// latest one only, the one the gateway takes back: .custody-suggested-msg in
// the CSS). renderBody renders the body text. sk is the session the bubble
// is in, id the bubble's.
export function custodyBubbleHtml(e, text, sk, id, renderBody) {
  const m = custodyMessage(Object.assign({}, e, { detail: text, summary: text }));
  if (!m) return '<div class="custody-source">托管</div>' + renderBody(text);
  const src = '<div class="custody-source">' + esc(custodySourceText(m)) + '</div>';
  if (m.reason !== REASON_SUGGESTED || !m.body) return src + renderBody(m.body);
  return src + '<div class="custody-suggested">已按建议执行：<span class="custody-choice">' + esc(suggestedChoice(m.body)) + '</span>' +
    '<span class="custody-sep" aria-hidden="true">·</span>' + undoHtml(sk, id) + '</div>';
}

// UNDO_FAIL says why the gateway took nothing back, by HTTP status.
const UNDO_FAIL = {
  404: '这个会话里没有可撤回的按建议执行',
  400: '这个会话不归托管管',
  503: 'Planner 没开，托管没有运行',
};

async function undo(el) {
  const { key, node, id } = el.dataset;
  if (!key) return;
  el.disabled = true;
  const payload = { key };
  try {
    // A session on the Planner's node is that node's custody's to take back.
    const res = await fetchJSON(nodeAPI(node, UNDO_URL), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload), timeoutMs: 15000,
    });
    if (id) undone.add(id);
    el.outerHTML = '<span class="custody-undone" title="' + escAttr('已撤回：' + (res?.answer || '')) + '">已发撤回</span>';
  } catch (e) {
    el.disabled = false;
    showToast('没有发出撤回：' + (UNDO_FAIL[e.status] || failText(e)), 'error');
  }
}

registerActions({
  'custody-toggle': toggle,
  'watch-toggle': toggleWatch,
  'custody-undo': undo,
});

nzBus.addEventListener('custody:changed', paintCustodyChip);
// Another node's Planner: its custody.
nzBus.addEventListener('planner:node', () => {
  custody.enabled = null;
  custody.byKey = new Map();
  custody.listed = false;
  custody.paused = false;
  refreshCustody();
});
// What custody did on a session (session_notes.js passes its notes on).
nzBus.addEventListener('custody:note', () => { refreshCustody(); });
nzBus.addEventListener('session:select', () => { reportViewing(); paintCustodyChip(); refreshCustody(); });
wsm.onReady(() => { view.key = ''; reportViewing(); refreshCustody(); });
wsm.onStateChange(() => reportViewing());
document.addEventListener('visibilitychange', reportViewing);
window.addEventListener('focus', reportViewing);
window.addEventListener('blur', reportViewing);
setInterval(reportViewing, VIEW_CHECK_MS);
