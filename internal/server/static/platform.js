// platform.js — the nz:-prefixed localStorage helpers. A leaf with no imports
// and no load-time DOM access, so node tests can import it. Use these for new
// keys only: legacy 'nz_' / 'naozhi_' keys stay as they are so persisted user
// state survives upgrades. All three swallow quota/disabled errors.
const LS_PREFIX = 'nz:';
export function lsSet(key, value) { try { localStorage.setItem(LS_PREFIX + key, JSON.stringify(value)); } catch (e) { /* quota / disabled */ } }
export function lsGet(key, fallback) { try { const v = localStorage.getItem(LS_PREFIX + key); return v == null ? fallback : JSON.parse(v); } catch (e) { return fallback; } }
export function lsRemove(key) { try { localStorage.removeItem(LS_PREFIX + key); } catch (e) {} }
