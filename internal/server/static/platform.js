// platform.js — the browser services any dashboard module may need: the auth
// token and its fetch headers, and the nz:-prefixed localStorage helpers. A
// leaf with no imports and no load-time DOM access, so ws_manager.js and node
// tests can import it (scripts/check-ws-receivers.mjs R7).

// RNEW-UX-004: unified localStorage helper. Use these for NEW keys only —
// legacy 'nz_' / 'naozhi_' call sites are intentionally left alone to
// preserve persisted user state across upgrades. LS_SCHEMA is reserved for
// future breaking changes (bump + migrate on read). All three helpers
// swallow quota/disabled errors so callers never need their own try/catch.
const LS_PREFIX = 'nz:';
export function lsSet(key, value) { try { localStorage.setItem(LS_PREFIX + key, JSON.stringify(value)); } catch (e) { /* quota / disabled */ } }
export function lsGet(key, fallback) { try { const v = localStorage.getItem(LS_PREFIX + key); return v == null ? fallback : JSON.parse(v); } catch (e) { return fallback; } }
export function lsRemove(key) { try { localStorage.removeItem(LS_PREFIX + key); } catch (e) {} }

export function getToken() { return ''; }
// authHeaders builds the Authorization header set for fetch calls.
export function authHeaders() {
  const headers = /** @type {Record<string, string>} */ ({});
  const t = getToken();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  return headers;
}
