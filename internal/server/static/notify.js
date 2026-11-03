// notify.js — the dashboard exit of the push pipeline (planner RFC §10.1).
// A notify frame becomes a browser notification only while the page is in
// the background and the user has allowed notifications; in the foreground
// the inbox itself shows it. A click brings the tab forward and opens the
// session the item is about. inbox_count frames carry the open "等你" counts
// (open: every open item; badge: decisions and questions, the §10.1 badge),
// re-dispatched on nzBus as 'inbox:count' for the Planner page's badge. The
// frames only come on a change: the page takes the first counts from
// GET /api/inbox (its `counts`) and passes them to onInboxCount.
import { NZ_CONTRACT } from './contract.js';
import { wsm } from './ws_manager.js';
import { nzBus } from './nz_util.js';
import { shell } from './shell.js';
import { sessionList } from './state.js';
import { sid } from './session_ident.js';

export const inbox = { open: null, badge: null };

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

function openFromNotification(key) {
  window.focus();
  if (!key || !sessionList.sessionsData[sid(key, 'local')]) return;
  if (shell.setActivityView) shell.setActivityView('chat');
  if (shell.selectSession) shell.selectSession(key, 'local');
}

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
    openFromNotification(msg.key || '');
  };
}

export function onInboxCount(msg) {
  inbox.open = typeof msg.open === 'number' ? msg.open : 0;
  inbox.badge = typeof msg.badge === 'number' ? msg.badge : 0;
  nzBus.dispatchEvent(new CustomEvent('inbox:count', { detail: { open: inbox.open, badge: inbox.badge } }));
}

wsm.on(NZ_CONTRACT.WS.notify, (msg) => onNotify(msg));
wsm.on(NZ_CONTRACT.WS.inbox_count, (msg) => onInboxCount(msg));
