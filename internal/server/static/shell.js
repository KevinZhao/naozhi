// shell.js — the upcall table: the root modules' orchestration functions
// that a module they import needs to call back into (a plain import would be
// a cycle). A leaf (caps.leaves) with no imports. Each root fills its own
// group of slots once at load with registerShell({...}); a lower module calls
// shell.X(...). js-ratchet holds each slot to a root's own top-level
// function or const that does more than forward, and each user to a module
// the root reaches through imports (else it should import the function).
export const shell = Object.seal({
  renderMainHeader: null,
  renderMainShell: null,
  selectSession: null,
  setActivityView: null,
  openCronPanel: null,
  renderCronPanel: null,
});

// The slots each root registers in one call: dashboard.js, then cron_view.js.
export const SHELL_GROUPS = [
  ['renderMainHeader', 'renderMainShell', 'selectSession', 'setActivityView'],
  ['openCronPanel', 'renderCronPanel'],
];

export function registerShell(impl) {
  const group = SHELL_GROUPS.find((g) => g.some((k) => k in impl)) || [];
  const stray = Object.keys(impl).find((k) => !group.includes(k));
  if (stray) throw new Error(stray in shell ? 'shell slot ' + stray + ' belongs to another root' : 'shell has no slot ' + stray);
  const bad = group.find((k) => typeof impl[k] !== 'function' || shell[k] !== null);
  if (bad) throw new Error((shell[bad] === null ? 'shell slot missing: ' : 'shell slot registered twice: ') + bad);
  if (!group.length) throw new Error('shell: registration names no slot');
  for (const k of group) shell[k] = impl[k];
}
