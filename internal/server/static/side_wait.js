// side_wait.js — the "通知 N · 任务 N" bar atop the 会话 view's sidebar
// (planner RFC §15.3 ①, §15.4): the Planner page's left column where you
// already are. Its counts and cards are the left column's (planner_view.js
// waitCount / waitListHtml, the tasks not done tasks.js renders); a click
// unfolds them in place, where the cards' own buttons close, approve /
// reject, answer and change a task's state, a dispatched session opening in
// the chat view, and with nothing open it is one thin line.
//
// The counts follow inbox_count frames (nzBus 'inbox:count'); the list is
// fetched when the bar unfolds, refetched on a count change while it is
// unfolded, and repainted on 'inbox:items'.
import { nzBus } from './nz_util.js';
import { fetchInbox, waitCount, waitListHtml } from './planner_view.js';
import { mountTasks } from './tasks.js';

const $ = (id) => document.getElementById(id);

const sw = {
  open: false,
  counts: '', // the last inbox_count seen, "open/badge"
};

function put(id, v) {
  const el = $(id);
  if (el && el.textContent !== v) el.textContent = v;
}

function paintCounts() {
  const bar = $('side-wait');
  if (!bar) return;
  const w = waitCount();
  const p = document.querySelectorAll('#sw-task .tk-card:not(.done)').length;
  put('sw-wait-n', String(w));
  put('sw-task-n', String(p));
  put('sw-wait-count', String(w));
  bar.classList.toggle('sw-zero', !w && !p);
}

function paintList() {
  const box = $('sw-wait');
  if (!box || !sw.open) return;
  const html = waitListHtml();
  if (box.innerHTML !== html) box.innerHTML = html;
}

function toggle() {
  sw.open = !sw.open;
  $('side-wait')?.classList.toggle('sw-open', sw.open);
  $('sw-toggle')?.setAttribute('aria-expanded', sw.open ? 'true' : 'false');
  const body = $('sw-body');
  if (body) body.hidden = !sw.open;
  if (sw.open) { paintList(); fetchInbox(); }
}

nzBus.addEventListener('inbox:count', (e) => {
  paintCounts();
  // fetchInbox seeds the counts it fetched: the same counts do not refetch.
  const c = e.detail.open + '/' + e.detail.badge;
  if (c === sw.counts) return;
  sw.counts = c;
  if (sw.open) fetchInbox();
});
nzBus.addEventListener('inbox:items', paintList);

document.addEventListener('DOMContentLoaded', () => {
  $('sw-toggle')?.addEventListener('click', toggle);
  const tasks = $('sw-task');
  if (tasks) {
    mountTasks(tasks, { open: 'chat' });
    new MutationObserver(paintCounts).observe(tasks, { childList: true, subtree: true });
  }
  paintCounts();
});
