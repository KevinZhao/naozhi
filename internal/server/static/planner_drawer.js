// planner_drawer.js — the Planner drawer (docs/rfc/planner.md §15.3 ②):
// Alt+P from any view pulls the Planner conversation over the right side, a
// full-screen panel on phone (opened by the session header's Planner
// button, there being no keyboard); Esc or Alt+P again puts it away.
//
// It is the Planner page's own middle column moved in
// (planner_view.js attachPlannerConversation): the same stream, cards and
// input box, sends through /api/planner/send with the snapshot and the
// receipt, and the explanation when planner.enabled is off or the Planner is
// stopped. A session card leaves the drawer for that session in the chat
// view. The rail's Planner icon still opens the full page, which takes the
// conversation back. The drawer and the workbench never show together:
// opening one puts the other away.
//
// openPlannerDrawer({ item }) is the entry for a push that leads to a 等你
// item without a session (§15.3 ④).
import { sessionList, ui } from './state.js';
import { nzBus, registerActions } from './nz_util.js';
import { sid } from './session_ident.js';
import { attachPlannerConversation, detachPlannerConversation, focusInboxItem } from './planner_view.js';
import { openPlannerSession } from './planner_chat.js';
import { workbench } from './workbench.js';
import { registerShell, shell } from './shell.js';

const $ = (id) => document.getElementById(id);

// returnFocus is what held the focus before the drawer took it.
let returnFocus = null;

function isOpen() {
  const d = $('planner-drawer');
  return !!d && !d.hidden;
}

const phone = () => !!(window.matchMedia && window.matchMedia('(max-width: 768px)').matches);

// hide puts the drawer shell away; the conversation is the caller's.
function hide() {
  const d = $('planner-drawer');
  if (!d || d.hidden) return;
  d.hidden = true;
  document.body.classList.remove('nz-pld-open');
}

// openPlannerDrawer opens the drawer; opts.item marks that open 等你 item
// above the conversation. On the Planner page the conversation is already
// in view, so the item is marked there.
export function openPlannerDrawer(opts) {
  const item = (opts && opts.item) || '';
  if (ui.activeView === 'planner') {
    focusInboxItem(item);
    if (!phone()) $('pl-input')?.focus();
    return;
  }
  const d = $('planner-drawer');
  if (!d) return;
  if (workbench.isOpen()) workbench.close();
  if (!isOpen()) {
    const ae = document.activeElement;
    returnFocus = ae && ae !== document.body ? ae : null;
    d.hidden = false;
    document.body.classList.add('nz-pld-open');
    attachPlannerConversation(d);
  }
  focusInboxItem(item);
  if (!phone()) $('pl-input')?.focus();
}

export function closePlannerDrawer() {
  if (!isOpen()) return;
  const inside = $('planner-drawer').contains(document.activeElement);
  hide();
  detachPlannerConversation();
  if (inside && returnFocus && returnFocus.isConnected) returnFocus.focus();
  returnFocus = null;
}

export function togglePlannerDrawer() {
  if (isOpen()) closePlannerDrawer();
  else openPlannerDrawer();
}

// openSession leaves the drawer for key in the chat view; a session the list
// does not hold opens read-only in the Planner page's middle column.
function openSession(key) {
  if (!key) return;
  if (!sessionList.sessionsData[sid(key, 'local')]) {
    shell.setActivityView('planner');
    openPlannerSession(key);
    return;
  }
  closePlannerDrawer();
  shell.selectSession(key, 'local');
}

// The page took the conversation back: only the shell is left to put away.
nzBus.addEventListener('planner:host', (e) => {
  if (e.detail.host !== 'drawer') { hide(); returnFocus = null; }
});
nzBus.addEventListener('workbench:open', closePlannerDrawer);

// Capture phase: Alt+P works from inside an input too, where it types
// nothing anyone wants (Option+P is "π" on a Mac, hence e.code). Esc is
// taken only when no dialog or lightbox above the drawer wants it.
document.addEventListener('keydown', (e) => {
  if (e.isComposing) return;
  if (e.code === 'KeyP' && e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey) {
    if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
    e.preventDefault();
    if (!e.repeat) togglePlannerDrawer();
    return;
  }
  if (e.key !== 'Escape' || !isOpen()) return;
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay, .lightbox-overlay.active')) return;
  e.preventDefault();
  e.stopPropagation();
  closePlannerDrawer();
}, true);

// notify.js reaches the drawer through the shell: importing this module
// back would close a cycle through planner_view.js.
registerShell({ openPlannerDrawer });

registerActions({
  'planner-drawer-toggle': () => togglePlannerDrawer(),
  'planner-drawer-close': () => closePlannerDrawer(),
  'planner-drawer-page': () => shell.setActivityView('planner'),
});

document.addEventListener('DOMContentLoaded', () => {
  // Ahead of the delegated actions: a session card here leaves the drawer
  // rather than opening in a middle column it does not have.
  $('planner-drawer')?.addEventListener('click', (e) => {
    const card = e.target instanceof Element ? e.target.closest('[data-action="planner-open-session"]') : null;
    if (card) { e.stopPropagation(); openSession(card.dataset.key); return; }
    if (e.target instanceof Element && e.target.closest('[data-action="session-select"]')) closePlannerDrawer();
  });
});
