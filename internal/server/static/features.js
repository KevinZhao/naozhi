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
// on, or did spawn on until the server lists it (sentPicks): the explicit pick
// (any node), else 自动. '' once listed (sess.backend rules), sent with no
// sentPicks entry (a key not created here), with one backend, or for 自动 on a
// remote node, whose manifest and profiles are not the ones cached here.
export function pendingBackendID(key, node) {
  const sent = perSession.sentPicks[key], pick = perSession.backends[key] || (sent && sent.backend);
  if (pick) return pick;
  const n = node || perSession.nodes[key] || 'local', m = serverInfo.cliBackends, s = sid(key, n);
  if (!key || n !== 'local' || sessionList.sessionsData[s] || (!sent && (perSession.lastSent[s] || perSession.httpSendPending.has(s))) || !m || !Array.isArray(m.backends) || m.backends.length < 2) return '';
  return autoBackendID(m, sent ? sent.accessProfile : perSession.accessProfiles[key]);
}
