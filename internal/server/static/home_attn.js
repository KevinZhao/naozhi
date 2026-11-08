// home_attn.js — the idle Home's summary of what needs you (UX review 5,
// planner RFC §15.13 v0.7.0): one line "等你 N · 运行中 N · 失败 N" over
// the latest three items that wait on you, each a click away from its card
// or its session, in #home-attn of the empty main area (dashboard.html's
// cold start and utilities.js mainEmptyHtml carry the slot alike).
//
// 等你 is the one count of what is yours to handle (D91 / D94): the inbox's
// badge, the rail badge's and the sidebar bar's number, never a second red
// one; its items are the Planner left column's (planner_view.js
// waitingItems). With the Planner off the inbox answers 404 and the badge
// stays unknown: the line says 运行中 and 失败 only. 运行中 and 失败 count
// the sessions the sidebar lists by their status dots (sessionHealthState).
// With nothing to say the slot stays empty (:empty hides it).
//
// It repaints on inbox_count frames ('inbox:count'), a new item list
// ('inbox:items'), every sessions poll, and each repaint of the empty main
// area ('home:shown', from wireQuickAskInput). The Planner page refetches
// the items only while it shows, so a changed count refetches them here
// while the card is on screen.
import { selection, sessionList } from './state.js';
import { esc, escAttr, nzBus, registerActions, sessionHealthState } from './nz_util.js';
import { formatAbsTime, timeAgo } from './utilities.js';
import { followItem, inbox } from './notify.js';
import { fetchInbox, waitingItems } from './planner_view.js';
import { onSessionsApplied } from './session_list.js';

// SHOWN is how many of the items the card lists.
const SHOWN = 3;

// counts are the summary line's numbers; wait is null with the Planner off.
function counts() {
  let running = 0, failed = 0;
  for (const s of sessionList.allSessionsCache || []) {
    const st = sessionHealthState(s);
    if (st === 'active') running++;
    else if (st === 'failed') failed++;
  }
  return { wait: typeof inbox.badge === 'number' ? inbox.badge : null, running, failed };
}

const created = (it) => Date.parse(it.created) || 0;

function rowHtml(it) {
  const at = created(it);
  return '<button type="button" class="ha-row" data-action="home-attn-open" data-ref="' + escAttr(it.ref) + '" data-id="' + escAttr(it.id) +
      '" data-key="' + escAttr(it.key) + '" data-node="' + escAttr(it.node) + '">' +
      '<span class="ha-kind ha-k-' + escAttr(it.kind) + '">' + esc(it.label || '') + '</span>' +
      '<span class="ha-title" title="' + escAttr(it.title) + '">' + esc(it.title) + '</span>' +
      (at ? '<span class="ha-time" title="' + escAttr(formatAbsTime(at)) + '">' + esc(timeAgo(at)) + '</span>' : '') +
    '</button>';
}

// homeAttnHtml is the card, '' when every count is 0.
export function homeAttnHtml() {
  const c = counts();
  if (!c.wait && !c.running && !c.failed) return '';
  const part = (cls, label, n) => '<span class="ha-n ha-n-' + cls + '">' + label + ' <b>' + n + '</b></span>';
  const parts = [];
  if (c.wait !== null) parts.push(part('wait', '等你', c.wait));
  parts.push(part('run', '运行中', c.running), part('fail', '失败', c.failed));
  // The latest first: the left column keeps its own order, by kind.
  const items = c.wait ? waitingItems().sort((a, b) => created(b) - created(a)) : [];
  const more = items.length > SHOWN
    ? '<button type="button" class="pl-link ha-all" data-action="home-attn-all">在 Planner 页看全部 ' + items.length + ' 条</button>'
    : '';
  return '<div class="ha-card" role="region" aria-label="要你处理的事">' +
      '<div class="ha-counts">' + parts.join('<span class="ha-sep" aria-hidden="true">·</span>') + '</div>' +
      (items.length ? '<div class="ha-list" role="list">' + items.slice(0, SHOWN).map(rowHtml).join('') + '</div>' : '') +
      more +
    '</div>';
}

function paint() {
  const el = document.getElementById('home-attn');
  if (!el || selection.key) return;
  const html = homeAttnHtml();
  if (el.innerHTML !== html) el.innerHTML = html;
}

// seen is the last inbox_count taken, "open/badge": fetchInbox seeds the
// counts it fetched, and the same counts do not refetch.
let seen = '';
nzBus.addEventListener('inbox:count', (e) => {
  paint();
  const c = e.detail.open + '/' + e.detail.badge;
  if (c === seen) return;
  seen = c;
  // The card may be empty (hidden) until the items come: the empty main
  // area showing is what counts.
  if (!selection.key && document.getElementById('home-attn')?.parentElement?.getClientRects().length) fetchInbox();
});
nzBus.addEventListener('inbox:items', paint);
nzBus.addEventListener('home:shown', paint);
onSessionsApplied(paint);
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', paint);
else paint();

registerActions({
  'home-attn-open': (el) => followItem(el.dataset.key || '', el.dataset.node || 'local', el.dataset.id || '', el.dataset.ref || ''),
  'home-attn-all': () => followItem('', '', '', ''),
});
