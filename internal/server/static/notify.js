// notify.js — the dashboard exit of the push pipeline (planner RFC §10.1).
// A notify frame becomes a browser notification only while the page is in
// the background and the user has allowed notifications; in the foreground
// the inbox itself shows it. A click brings the tab forward and goes to what
// the item is about (§15.3 ④): its session, scrolled to the item's note
// (an element under #main with data-inbox-item="<id>"; without one the
// session just opens), or with no listed session the Planner page with the
// item's card brought into view (inbox.focus, taken by revealFocus once a
// list paints it; see openPlanner for the drawer). inbox_count frames carry the open "等你" counts (open: every open
// item; badge: decisions and questions, the §10.1 badge), re-dispatched on
// nzBus as 'inbox:count' for the Planner page's badge. The frames only come
// on a change: the page takes the first counts from GET /api/inbox (its
// `counts`) and passes them to onInboxCount.
//
// Deep links go the same way: #session=<key>[&node=<id>][&item=<id>] (the
// morning brief's in-page links, GET /api/planner/briefs) and
// #planner[&item=<id>] (the macOS notification's link, notify.linkFor).
// Such a hash is followed at load once the first sessions list is in, or on
// hashchange, and is then cleared. A #session link to an unlisted session
// opens the Planner on its item, or does nothing without one.
import { NZ_CONTRACT } from './contract.js';
import { wsm } from './ws_manager.js';
import { nzBus } from './nz_util.js';
import { shell } from './shell.js';
import { sessionList } from './state.js';
import { onSessionsApplied } from './session_list.js';
import { sid } from './session_ident.js';

// focus: the item id a notification or link asked to bring into view.
export const inbox = { open: null, badge: null, focus: '' };

// NOTE_TRIES × NOTE_MS bound the wait for an opened session's note to paint.
const NOTE_TRIES = 20;
const NOTE_MS = 250;
// LIT_MS is how long a revealed item stays highlighted (.inbox-lit).
const LIT_MS = 3000;
const lit = { id: '', until: 0 };

const cssEsc = (s) => (window.CSS && CSS.escape ? CSS.escape(s) : s);

function light(el, sel) {
  el.classList.add('inbox-lit');
  setTimeout(() => document.querySelectorAll(sel).forEach((e) => e.classList.remove('inbox-lit')), LIT_MS);
}

// isLit reports whether item id is still highlighted: a list repainting
// within LIT_MS keeps the class on the card it renders.
export function isLit(id) {
  return !!id && id === lit.id && Date.now() < lit.until;
}

// revealFocus brings the focused item's card under root (.pl-card[data-id])
// into view and takes the focus; false while root does not show it yet.
export function revealFocus(root) {
  const sel = '.pl-card[data-id="' + cssEsc(inbox.focus) + '"]';
  const el = inbox.focus && root ? root.querySelector(sel) : null;
  if (!el) return false;
  lit.id = inbox.focus;
  lit.until = Date.now() + LIT_MS;
  inbox.focus = '';
  el.scrollIntoView({ block: 'nearest' });
  light(el, sel);
  return true;
}

// notifySupported: the Notification API exists (absent in some in-app
// browsers and older iOS Safari tabs not added to the home screen).
export function notifySupported() {
  return typeof window.Notification === 'function';
}

// requestNotifyPermission asks for permission; it must run from a user
// gesture (the Planner page's "允许浏览器通知" button). Resolves to the
// resulting permission ('granted' | 'denied' | 'default' | 'unsupported').
export async function requestNotifyPermission() {
  if (!notifySupported()) return 'unsupported';
  if (Notification.permission !== 'default') return Notification.permission;
  try {
    return await Notification.requestPermission();
  } catch {
    return Notification.permission;
  }
}

function openSession(key, node) {
  if (!key || !sessionList.sessionsData[sid(key, node)]) return false;
  if (shell.setActivityView) shell.setActivityView('chat');
  if (shell.selectSession) shell.selectSession(key, node);
  return true;
}

// scrollToNote scrolls the opened session to item id's note once its
// events paint; the session stays at its own scroll without one.
function scrollToNote(id) {
  if (!id) return;
  const sel = '#main [data-inbox-item="' + cssEsc(id) + '"]';
  let tries = 0;
  const look = () => {
    const el = document.querySelector(sel);
    if (el) { el.scrollIntoView({ block: 'center' }); light(el, sel); return; }
    if (++tries < NOTE_TRIES) setTimeout(look, NOTE_MS);
  };
  look();
}

// openPlanner opens the Planner on item id; 'inbox:focus' lets a list
// already painted reveal it. An item goes to the page, whose left column
// lists every open one: the drawer (shell.openPlannerDrawer, §15.3 ②) holds
// the conversation, which shows only the Planner's own decisions. The
// drawer takes an open with no item.
function openPlanner(id) {
  inbox.focus = id || '';
  if (!id && typeof shell.openPlannerDrawer === 'function') shell.openPlannerDrawer();
  else if (shell.setActivityView) shell.setActivityView('planner');
  nzBus.dispatchEvent(new CustomEvent('inbox:focus'));
}

// follow goes to a link or a clicked notification: {key, node, item}.
function follow(link) {
  if (openSession(link.key, link.node)) scrollToNote(link.item);
  else if (!link.key || link.item) openPlanner(link.item);
}

// linkFromHash reads and clears a #session= or #planner link; null for any
// other hash.
function linkFromHash() {
  const params = new URLSearchParams(location.hash.slice(1));
  const key = params.get('session') || '';
  if (!key && !params.has('planner')) return null;
  history.replaceState(history.state, '', location.pathname + location.search);
  return { key, node: params.get('node') || 'local', item: params.get('item') || '' };
}

let hashLink = linkFromHash();
onSessionsApplied(() => {
  if (!hashLink) return;
  follow(hashLink);
  hashLink = null;
});
window.addEventListener('hashchange', () => {
  const link = linkFromHash();
  if (link) follow(link);
});

export function onNotify(msg) {
  if (!document.hidden || !notifySupported() || Notification.permission !== 'granted') return;
  let n;
  try {
    // tag: a second frame for the same item (another tab's socket) replaces
    // rather than stacks.
    n = new Notification(msg.title || '等你', { body: msg.body || '', tag: msg.id || undefined });
  } catch {
    return; // Android Chrome only allows notifications from a service worker
  }
  n.onclick = () => {
    n.close();
    window.focus();
    follow({ key: msg.key || '', node: 'local', item: msg.id || '' });
  };
}

export function onInboxCount(msg) {
  inbox.open = typeof msg.open === 'number' ? msg.open : 0;
  inbox.badge = typeof msg.badge === 'number' ? msg.badge : 0;
  nzBus.dispatchEvent(new CustomEvent('inbox:count', { detail: { open: inbox.open, badge: inbox.badge } }));
}

wsm.on(NZ_CONTRACT.WS.notify, (msg) => onNotify(msg));
wsm.on(NZ_CONTRACT.WS.inbox_count, (msg) => onInboxCount(msg));
