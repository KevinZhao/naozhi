// session_ident.js — how the dashboard names a session and labels it: the
// (key, node) map key and node badge, the discovered-card key, the project,
// node and type labels, a node's connection status, and the event kinds kept
// out of the transcript. A leaf (caps.leaves): it imports only contract.js,
// state.js, nz_util.js and ws_manager.js.
import { NZ_CONTRACT } from './contract.js';
import { sessionList } from './state.js';
import { esc } from './nz_util.js';
import { WS_STATES, wsm } from './ws_manager.js';

export function sid(key, node) { return key + '\t' + (node || 'local'); }

export function isMultiNode() {
  const keys = Object.keys(sessionList.nodesData);
  return keys.length > 1 || (keys.length === 1 && keys[0] !== 'local');
}

const NODE_BADGE_COLORS = ['#1f6feb','#0550ae','#1a7f37','#6e40c9','#9a6700','#cf222e'];
export function nodeColor(id) {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return NODE_BADGE_COLORS[h % NODE_BADGE_COLORS.length];
}

// Discovered-card identity is (pid, node): pids repeat across nodes, so every
// key/lookup/removal goes through these helpers (#2431). Key shape is
// '_discovered:<pid>:<node>' — pid stays in slot 1 for parseDiscoveredPid.
export function discoveredKey(pid, node) {
  return '_discovered:' + pid + ':' + (node || 'local');
}
export function isDiscoveredKey(key) {
  return typeof key === 'string' && key.startsWith('_discovered:');
}
export function parseDiscoveredPid(key) {
  return parseInt(key.split(':')[1], 10);
}
export function sameDiscovered(d, pid, node) {
  return d.pid === pid && (d.node || 'local') === (node || 'local');
}
export function findDiscovered(pid, node) {
  return sessionList.discoveredItems.find(d => sameDiscovered(d, pid, node)) || null;
}
export function dropDiscovered(pid, node) {
  sessionList.discoveredItems = sessionList.discoveredItems.filter(d => !sameDiscovered(d, pid, node));
}

// projectDisplayLabel returns the operator-facing name for a project,
// preferring the explicit ProjectConfig.display_name override (R110-P2 /
// #448) and falling back to the directory-derived `p.name` so projects
// without a configured display_name keep their existing UI label.
//
// Pure: returns a string, no escaping. Callers MUST run the result
// through esc() / escAttr() before injecting it into HTML — same
// contract as `p.name`. Truthy guards on both fields tolerate the
// pre-config case (`p.config` undefined on legacy /api/projects shapes
// or remote-merge entries that the cache layer hasn't yet stamped).
export function projectDisplayLabel(p) {
  if (!p) return '';
  const cfg = p.config || {};
  const dn = (cfg.display_name || '').trim();
  if (dn) return dn;
  return p.name || '';
}

// projectDisplayPrefix renders the optional emoji in front of the name.
// Returns "" when the project has no emoji configured. The trailing
// space lives inside the returned string so callers can simply
// concatenate prefix + label without conditional whitespace.
export function projectDisplayPrefix(p) {
  if (!p) return '';
  const cfg = p.config || {};
  const em = (cfg.emoji || '').trim();
  if (!em) return '';
  return em + ' ';
}

// Match a workspace path to a project from projectsData (longest prefix wins)
export function matchProject(workspace) {
  if (!workspace || !sessionList.projectsData || sessionList.projectsData.length === 0) return '';
  const ws = workspace.endsWith('/') ? workspace : workspace + '/';
  let best = '', bestLen = 0;
  for (const p of sessionList.projectsData) {
    const prefix = p.path.endsWith('/') ? p.path : p.path + '/';
    if (ws.startsWith(prefix) && p.path.length > bestLen) {
      best = p.name; bestLen = p.path.length;
    }
  }
  return best;
}

export function sessionTypeTag(label) {
  return '<span class="sc-type-tag">' + esc(label || 'CLI') + '</span>';
}

// getNodeDisplayName returns the human label for a node id. Falls back to the
// raw id for remotes whose display_name the server hasn't populated yet, and
// uses a Chinese '本地' for 'local' to match the rest of the UI.
export function getNodeDisplayName(id) {
  if (!id || id === 'local') return '本地';
  const nd = sessionList.nodesData[id];
  if (nd && nd.display_name) return nd.display_name;
  return id;
}

// getNodeStatus returns a normalized status key (ok/connecting/offline/
// unreachable/error) for a node. 'local' tracks the WS state machine; remotes
// read from the server-side node health snapshot. Falls back to 'offline' when
// the server has no record — safer than pretending the node is reachable.
export function getNodeStatus(id) {
  if (!id || id === 'local') {
    if (wsm.state === WS_STATES.CONNECTED) return 'ok';
    if (wsm.state === WS_STATES.CONNECTING || wsm.state === WS_STATES.AUTH) return 'connecting';
    return 'offline';
  }
  const nd = sessionList.nodesData[id];
  if (!nd) return 'offline';
  return nd.status || 'offline';
}

// statusLabelForNode maps a normalized status to a short Chinese/English label
// used inside the trigger and each dropdown row.
export function statusLabelForNode(status) {
  const m = {
    ok: 'connected', connected: 'connected',
    connecting: 'connecting', authenticating: 'authenticating',
    offline: 'offline', unreachable: 'unreachable',
    error: 'error', disconnected: 'disconnected',
  };
  return m[status] || status;
}

// Kinds kept out of the transcript (clievent kindTable's Internal column says why).
const INTERNAL_EVENT_TYPES = new Set(NZ_CONTRACT.ENUMS.EVENT_TYPE_INTERNAL);
// Unified backend behaviour (supersedes Multi-Backend RFC §8.3 D17): both
// Claude (stream-json) and Kiro (ACP) tool_use events are filtered out of
// the main transcript so the chat reads cleanly. Transient tool activity
// is still surfaced via the running banner (applyEventToTurnState below)
// while the turn is in flight, and the subagent panel still renders the
// rich tool_call progress row via eventHtml(includeInternal=true) so
// operators can drill into per-agent tool runs when needed.
export function isInternalEvent(/** @type {EventEntry} */ e) {
  if (!e || !INTERNAL_EVENT_TYPES.has(e.type)) return false;
  return true;
}
