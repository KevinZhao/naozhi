// features.js — backend feature-flag lookups (Multi-Backend RFC §8.2/§8.3)
// and which backend a session runs on before the server lists it. A leaf
// (caps.leaves): no DOM access, no load-time side effects. Reads state.js,
// and sid from session_ident.js.
import { perSession, serverInfo, selection, sessionList } from './state.js';
import { sid } from './session_ident.js';

// featureForBackend resolves a backend feature flag. Missing/unknown
// backend/no cache yet all default to false (spec: "missing key == false").
// Each recognized name (askuser, passthrough, embedded_context, image_input,
// audio_input, mcp_http, mcp_sse) needs its own hard-coded caller.
export function featureForBackend(backendID, name) {
  if (!serverInfo.cliBackends || !Array.isArray(serverInfo.cliBackends.backends)) return false;
  if (!backendID) backendID = serverInfo.cliBackends.default || '';
  const entry = serverInfo.cliBackends.backends.find(b => b && b.id === backendID);
  if (!entry || !entry.features) return false;
  return entry.features[name] === true;
}

// featureForCurrent reads the active session's backend feature flag; true
// in single-backend mode (length<=1) so claude-only deployments keep all
// historical behavior.
export function featureForCurrent(name) {
  if (!serverInfo.cliBackends || !Array.isArray(serverInfo.cliBackends.backends)) return true;
  if (serverInfo.cliBackends.backends.length <= 1) return true; // single-backend mode
  const sess = sessionList.sessionsData[sid(selection.key, selection.node)];
  const backendID = (sess && sess.backend) || pendingBackendID(selection.key, selection.node) || serverInfo.cliBackends.default || '';
  return featureForBackend(backendID, name);
}

// autoBackendID is the backend the picker's 自动 resolves to in backendsData:
// profileID's ("" = default_access_profile) default_backend when enabled,
// else the router default. agents[].backend and project pins are not visible.
export function autoBackendID(backendsData, profileID) {
  const list = backendsData.backends, ap = serverInfo.accessProfiles, pid = profileID || (ap && ap.default) || '';
  const prof = pid && ap && Array.isArray(ap.profiles) ? ap.profiles.find(p => p && p.id === pid) : null;
  const on = id => !!id && list.some(b => b && b.id === id);
  return on(prof && prof.default_backend) ? prof.default_backend : on(backendsData.default) ? backendsData.default : ((list[0] && list[0].id) || '');
}

// pendingBackendID is the backend a session this browser created will spawn
// on while the server does not list it: the explicit pick, else 自动. '' once
// listed (sess.backend rules), with one backend, or on a remote node, whose
// manifest and profiles are not the ones cached here.
export function pendingBackendID(key, node) {
  if (perSession.backends[key]) return perSession.backends[key];
  const n = node || perSession.nodes[key] || 'local', m = serverInfo.cliBackends;
  if (!key || n !== 'local' || sessionList.sessionsData[sid(key, n)] || !m || !Array.isArray(m.backends) || m.backends.length < 2) return '';
  return autoBackendID(m, perSession.accessProfiles[key]);
}
