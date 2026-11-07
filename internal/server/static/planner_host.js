// planner_host.js — which node runs the Planner the page shows
// (docs/rfc/remote-planner.md §2, §5). The global Planner is one session,
// planner:main, on the node that holds the workspaces and credentials; a
// primary whose own Planner is off shows a node's in place: the page's reads
// and writes go to /api/remote/{node}/… (the node's own handlers, §4), its
// conversation follows the node's planner:main over the relayed event
// stream, and the sessions it names are that node's.
//
// The host is this node while its Planner is on, else the first node
// advertising the `planner` capability (/api/sessions nodes[id].caps). With
// both (D60 says one, §2 ②) the page header offers a switch, the pick kept in
// localStorage. 'planner:node' on nzBus announces a change.
//
// A leaf: it imports only contract.js, state.js and nz_util.js.
import { NZ_CONTRACT } from './contract.js';
import { sessionList } from './state.js';
import { fetchJSON, nzBus } from './nz_util.js';

const STORE = 'nz_plannerNode';
const PLANNER_CAP = 'planner';

export const plannerHost = {
  node: 'local',
  // localOn: this node's Planner is on (GET /api/planner), null until read.
  localOn: null,
};

export const isLocalNode = (node) => !node || node === 'local';

// nodeAPI is route path (/api/…) as node serves it: the route itself here,
// else the node's own handler behind /api/remote/{node}/.
export function nodeAPI(node, path) {
  if (isLocalNode(node)) return path;
  return NZ_CONTRACT.API.remote_node_path___
    .replace('{node}', encodeURIComponent(node))
    .replace('{path...}', path.replace(/^\/api\//, ''));
}

// plannerAPI is route path on the Planner's node.
export const plannerAPI = (path) => nodeAPI(plannerHost.node, path);

// eventsURL is GET /api/sessions/events for key on node, q appended ("&…").
export function eventsURL(key, node, q) {
  return NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(key) +
    (isLocalNode(node) ? '' : '&node=' + encodeURIComponent(node)) + (q || '');
}

// nodeRunsPlanner reports whether node advertises a Planner.
export function nodeRunsPlanner(node) {
  if (isLocalNode(node)) return plannerHost.localOn === true;
  const nd = sessionList.nodesData[node];
  return !!(nd && Array.isArray(nd.caps) && nd.caps.includes(PLANNER_CAP));
}

// plannerNodes lists the nodes a Planner runs on, this one first.
export function plannerNodes() {
  const out = plannerHost.localOn ? ['local'] : [];
  for (const id of Object.keys(sessionList.nodesData).sort()) {
    if (id !== 'local' && nodeRunsPlanner(id)) out.push(id);
  }
  return out;
}

// hostRemote reports whether the Planner shown runs on another node.
export const hostRemote = () => !isLocalNode(plannerHost.node);

// hostOnline reports whether the Planner's node answers: this one always
// (the socket says the rest), a node while its link is up.
export function hostOnline() {
  if (!hostRemote()) return true;
  const nd = sessionList.nodesData[plannerHost.node];
  return !!nd && nd.status === 'ok';
}

// hostName names the Planner's node: '' for this one.
export function hostName() {
  if (!hostRemote()) return '';
  const nd = sessionList.nodesData[plannerHost.node];
  return (nd && nd.display_name) || plannerHost.node;
}

function stored() {
  try { return localStorage.getItem(STORE) || ''; } catch { return ''; }
}

// pick settles the host from what is known: nothing moves while this node's
// switch is unread, so a page does not flip to a node and back.
function pick() {
  if (plannerHost.localOn === null) return;
  const nodes = plannerNodes();
  const want = stored();
  const next = nodes.includes(want) ? want : (nodes[0] || 'local');
  if (next === plannerHost.node) return;
  plannerHost.node = next;
  nzBus.dispatchEvent(new CustomEvent('planner:node', { detail: { node: next } }));
}

// choosePlannerNode is the header switch: show node's Planner from now on.
export function choosePlannerNode(node) {
  try { localStorage.setItem(STORE, node); } catch { /* the pick holds for this page */ }
  pick();
}

// notePlannerNodes re-settles the host after a sessions list (the nodes'
// caps and links) came in.
export function notePlannerNodes() { pick(); }

let localLoading = null;

// readLocalPlanner reads this node's switch once (and after a reconnect,
// which may follow a restart that flipped planner.enabled).
export function readLocalPlanner(force) {
  if (localLoading) return localLoading;
  if (plannerHost.localOn !== null && !force) return Promise.resolve();
  localLoading = (async () => {
    try {
      const s = await fetchJSON(NZ_CONTRACT.API.planner, { timeoutMs: 8000 });
      plannerHost.localOn = !!(s && s.enabled);
    } catch (e) {
      // No Planner route: none here. Anything else is asked again.
      if (e && e.status === 404) plannerHost.localOn = false;
    } finally {
      localLoading = null;
    }
    pick();
  })();
  return localLoading;
}
