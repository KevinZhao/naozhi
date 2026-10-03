// features.js — backend feature-flag lookups (Multi-Backend RFC §8.2/§8.3).
// A leaf (caps.leaves): no DOM access, no load-time side effects. Reads
// serverInfo, selection and sessionList from state.js, and sid from
// session_ident.js. Extracted from dashboard.js (S19-P, #3025, ruling D1:
// lands here, not auth_modal.js).
import { serverInfo, selection, sessionList } from './state.js';
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
  const backendID = (sess && sess.backend) || serverInfo.cliBackends.default || '';
  return featureForBackend(backendID, name);
}
