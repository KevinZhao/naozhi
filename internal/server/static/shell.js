// shell.js — the upcall table: the root modules' orchestration functions
// that a module they import needs to call back into (a plain import would be
// a cycle). A leaf with no imports. Each root fills its slots
// once at load with registerShell({...}); a lower module calls shell.X(...).
export const shell = Object.seal({
  renderMainHeader: null,
  renderMainShell: null,
  selectSession: null,
  setActivityView: null,
  openCronPanel: null,
  renderCronPanel: null,
  debouncedFetchSessions: null,
  fetchSessions: null,
  renderSidebar: null,
  updateStatusBar: null,
  fetchEvents: null,
});

export function registerShell(impl) {
  for (const k in impl) {
    if (!(k in shell) || shell[k] !== null || typeof impl[k] !== 'function') throw new Error('shell: bad slot ' + k);
    shell[k] = impl[k];
  }
}
