// side_wait.js — the "等你 N" bar atop the 会话 view's sidebar (planner RFC
// §15.3 ①, §15.7 S1): only what waits on you to act — questions, decisions
// and errors, the inbox's badge, the same count as the rail's Planner badge
// and the phone's bottom bar. A click unfolds their cards in place, where
// their own buttons close and answer them; with none the bar is not
// rendered. 任务 is not here: the sidebar's "Planner 派出" group lists the
// dispatched sessions, and the Planner page all of it.
//
// The count follows inbox_count frames (nzBus 'inbox:count'); the list is
// fetched when the bar unfolds, refetched on a count change while it is
// unfolded, and repainted on 'inbox:items'.
import { nzBus } from './nz_util.js';
import { inbox } from './notify.js';
import { actionListHtml, fetchInbox } from './planner_view.js';

const $ = (id) => document.getElementById(id);

const sw = {
  open: false,
  counts: '', // the last inbox_count seen, "open/badge"
};

function setOpen(open) {
  sw.open = open;
  $('sw-toggle')?.setAttribute('aria-expanded', open ? 'true' : 'false');
  $('side-wait')?.classList.toggle('sw-open', open);
  const list = $('sw-wait');
  if (list) list.hidden = !open;
}

function paintCount() {
  const bar = $('side-wait');
  if (!bar) return;
  const n = inbox.badge || 0;
  const v = String(n);
  const el = $('sw-n');
  if (el && el.textContent !== v) el.textContent = v;
  bar.hidden = !n;
  // Gone empty, it unfolds afresh when something comes in again.
  if (!n && sw.open) setOpen(false);
}

function paintList() {
  const box = $('sw-wait');
  if (!box || !sw.open) return;
  const html = actionListHtml();
  if (box.innerHTML !== html) box.innerHTML = html;
}

function toggle() {
  setOpen(!sw.open);
  if (sw.open) { paintList(); fetchInbox(); }
}

nzBus.addEventListener('inbox:count', (e) => {
  paintCount();
  // fetchInbox seeds the counts it fetched: the same counts do not refetch.
  const c = e.detail.open + '/' + e.detail.badge;
  if (c === sw.counts) return;
  sw.counts = c;
  if (sw.open) fetchInbox();
});
nzBus.addEventListener('inbox:items', paintList);

document.addEventListener('DOMContentLoaded', () => {
  $('sw-toggle')?.addEventListener('click', toggle);
  paintCount();
});
