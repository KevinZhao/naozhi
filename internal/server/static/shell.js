// shell.js — the upcall table: the root modules' orchestration functions
// that a module they import needs to call back into (a plain import would be
// a cycle). A leaf (caps.leaves) with no imports. dashboard.js fills every
// slot once at load with registerShell({...}); a lower module calls
// shell.X(...). js-ratchet holds each slot to a root's own top-level
// function or const that does more than forward, and each user to a module
// the root reaches through imports (else it should import the function).
export const shell = Object.seal({
  selectSession: null,
  setActivityView: null,
});

export function registerShell(impl) {
  for (const k of Object.keys(impl)) {
    if (!(k in shell)) throw new Error('shell has no slot ' + k);
  }
  for (const k of Object.keys(shell)) {
    if (typeof impl[k] !== 'function') throw new Error('shell slot missing: ' + k);
  }
  for (const k of Object.keys(shell)) shell[k] = impl[k];
}
