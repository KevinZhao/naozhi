// backend_catalog.js — what the server offers a new session: the CLI
// backends manifest and the access profiles (both cached 60s in serverInfo),
// the two <select> pickers built from them, and the access-profile chip label.
// A leaf (caps.leaves): the new-session dialog, project settings and the cron
// editor all read it, and it imports only other leaves.
import { NZ_CONTRACT } from './contract.js';
import { serverInfo } from './state.js';
import { esc, escAttr, fetchJSON } from './nz_util.js';
import { applyFeatureGates } from './utilities.js';

// fetchCLIBackends retrieves the enabled CLI backends from the server.
// Cached for 60 seconds — the set only changes across naozhi restarts.
// Resolves to null on network/auth failure so the caller can fall back to
// the no-picker flow (single-backend mode).
//
// node (optional) selects which node's manifest to fetch for the node-aware
// new-session picker. Omitted / 'local' returns the local manifest and keeps
// the global serverInfo.cliBackends cache (which every chip / feature-gate consumer
// reads) warm. A remote node id appends ?node=<id> so the primary proxies to
// that node (picker node-aware fix); the remote result is cached per node in
// serverInfo.cliBackendsByNode and does NOT touch the global serverInfo.cliBackends / feature gates.
export async function fetchCLIBackends(node) {
  const isLocal = !node || node === 'local';
  if (isLocal) {
    if (serverInfo.cliBackends && Date.now() - serverInfo.cliBackendsFetchedAt < 60000) {
      return serverInfo.cliBackends;
    }
  } else {
    const hit = serverInfo.cliBackendsByNode[node];
    if (hit && Date.now() - hit.at < 60000) {
      return hit.data;
    }
  }
  try {
    // RNEW-UX-003: default 10s timeout is fine here — this fetch is cached
    // for 60s and only fires at modal-open time, not on a poll.
    const url = isLocal
      ? NZ_CONTRACT.API.cli_backends
      : NZ_CONTRACT.API.cli_backends + '?node=' + encodeURIComponent(node);
    const data = await fetchJSON(url, {credentials: 'same-origin'});
    const manifest = data && Array.isArray(data.backends) ? data : null;
    if (isLocal) {
      serverInfo.cliBackends = manifest;
      serverInfo.cliBackendsFetchedAt = Date.now();
      // Multi-Backend RFC §8.3 D9-D15: re-apply feature gates whenever the
      // LOCAL backends manifest lands. The boot path fires fetchCLIBackends()
      // in parallel with fetchSessions, so the very first renderMainShell may
      // have run with serverInfo.cliBackends==null — call gates here so the input
      // controls update once the manifest is available. Remote-node fetches
      // must NOT drive feature gates (the input controls operate on the
      // locally-selected session), so this stays inside the isLocal branch.
      applyFeatureGates();
    } else if (manifest) {
      serverInfo.cliBackendsByNode[node] = { data: manifest, at: Date.now() };
    } else {
      // A null / malformed remote manifest must not be pinned for 60s —
      // drop any stale entry so the next open refetches (#2429).
      delete serverInfo.cliBackendsByNode[node];
    }
    return manifest;
  } catch (e) {
    if (!isLocal) delete serverInfo.cliBackendsByNode[node];
    return null;
  }
}

// fetchAccessProfiles caches /api/access-profiles for 60s (same policy as
// fetchCLIBackends). Returns {profiles:[...], default} or null on error / when
// no profiles are configured (single-auth deployments — the picker/chip then
// stay hidden). RFC project-access-profile §8.
export async function fetchAccessProfiles() {
  if (serverInfo.accessProfiles && Date.now() - serverInfo.accessProfilesFetchedAt < 60000) {
    return serverInfo.accessProfiles;
  }
  try {
    const data = await fetchJSON(NZ_CONTRACT.API.access_profiles, {credentials: 'same-origin'});
    serverInfo.accessProfiles = data && Array.isArray(data.profiles) ? data : null;
    serverInfo.accessProfilesFetchedAt = Date.now();
    return serverInfo.accessProfiles;
  } catch (e) {
    return null;
  }
}

// renderAccessProfilePicker returns an HTML fragment for an access-profile
// <select>, or empty string when 0/1 profiles are configured (nothing to
// choose — single-auth deployments see no extra control, mirroring
// renderBackendPicker's ≤1 rule). A profile whose secret_ok is false (a
// referenced *_FILE is missing) is shown disabled with a "⚠ 凭证缺失" suffix so
// the user can't pick a broken profile before sending (RFC P1-f). The picker
// NEVER shows env values — only display_name/id.
//
// opts (all optional):
//   - selectId: element id (default 'new-access-profile').
//   - selectedId: pre-selected profile id; falls back to "(全局默认)" empty option.
export function renderAccessProfilePicker(profilesData, opts) {
  if (!profilesData || !Array.isArray(profilesData.profiles)) return '';
  const list = profilesData.profiles;
  if (list.length <= 1) return '';
  const o = opts || {};
  const selectId = o.selectId || 'new-access-profile';
  // Which option starts selected. When the caller passes selectedId (even ""),
  // honour it verbatim — the project-settings panel uses "" to mean an explicit
  // "(global default)". When the caller omits selectedId entirely (new-session
  // flows), fall back to the server-configured default_access_profile so a
  // deployment that sets one gets it pre-selected instead of the bare empty
  // option. `default` may name a profile that isn't in `list` (e.g. deleted) —
  // harmless, no option matches and the empty option stays selected.
  const effectiveSelected = ('selectedId' in o) ? o.selectedId : (profilesData.default || '');
  // Empty option = global default (no overlay). Always offered so a project
  // pinned to a profile can still be overridden back to the default per-session.
  let options = '<option value="">（全局默认）</option>';
  options += list.map(p => {
    const id = p.id || '';
    const selected = (effectiveSelected && id === effectiveSelected) ? ' selected' : '';
    const broken = p.secret_ok === false;
    const disabled = broken ? ' disabled' : '';
    const label = (p.display_name || id) + (broken ? ' ⚠ 凭证缺失' : '');
    return '<option value="' + escAttr(id) + '"' + selected + disabled + '>' + esc(label) + '</option>';
  }).join('');
  return '<div class="nz-field">' +
    '<label class="nz-field-label" for="' + escAttr(selectId) + '">访问档</label>' +
    '<select id="' + escAttr(selectId) + '" class="nz-picker-select">' +
    options +
    '</select>' +
    '</div>';
}

// accessProfileChipInfo resolves an access-profile id to {label,color,tooltip}
// for the session card chip, or null when single-auth mode (≤1 profile) or the
// id is empty. An id not in the registry (deleted profile) renders a neutral
// chip with the raw id so the orphan state is visible. NEVER surfaces env/token.
export function accessProfileChipInfo(profileID) {
  if (!serverInfo.accessProfiles || !Array.isArray(serverInfo.accessProfiles.profiles)) return null;
  if (serverInfo.accessProfiles.profiles.length <= 1) return null; // single-auth mode
  if (!profileID) return null; // global default → no chip (matches "no overlay")
  const entry = serverInfo.accessProfiles.profiles.find(p => p && p.id === profileID);
  if (!entry) {
    return { label: profileID, color: 'var(--nz-text-mute)', tooltip: '访问档未配置: ' + profileID };
  }
  return {
    label: entry.display_name || entry.id,
    color: entry.chip_color || 'var(--nz-accent)',
    tooltip: (entry.display_name || entry.id) + (entry.default_model ? ' · ' + entry.default_model : ''),
  };
}

// renderBackendPicker returns an HTML fragment for a backend <select>, or
// an empty string when only one backend is enabled. The selected value is
// surfaced via document.getElementById(opts.selectId).value at submit time.
//
// opts (all optional):
//   - selectId: id of the <select> element. Defaults to 'new-backend' so
//     existing call sites (createNewSession / openProjectPalette /
//     pickPaletteCustom) keep working unchanged. The cron editor passes
//     'cron-backend' / 'edit-cron-backend' to avoid id collisions when
//     more than one modal is open simultaneously (defensive — modals are
//     usually exclusive but trapFocus ordering plus future stacking
//     should not silently corrupt the wrong picker).
//   - selectedId: if non-empty, this backend ID is pre-selected instead
//     of backendsData.default. Used by the cron edit modal to round-trip
//     a saved Job.Backend choice. Falls through to default when the
//     value doesn't match any enabled backend (e.g. operator removed
//     that backend from config.yaml).

export function renderBackendPicker(backendsData, opts) {
  if (!backendsData || !Array.isArray(backendsData.backends)) return '';
  const list = backendsData.backends;
  if (list.length <= 1) return '';
  const o = opts || {};
  const selectId = o.selectId || 'new-backend';
  const defaultID = backendsData.default || (list[0] && list[0].id) || '';
  // Pre-select the saved value when it matches a current enabled backend;
  // otherwise fall back to default. Iterating once keeps the lookup cheap.
  let preselect = defaultID;
  if (o.selectedId) {
    for (const b of list) {
      if (b && b.id === o.selectedId) { preselect = o.selectedId; break; }
    }
  }
  const options = list.map(b => {
    const selected = b.id === preselect ? ' selected' : '';
    const label = (b.display_name || b.id) + (b.version ? ' ' + b.version : '') + (b.available === false ? ' (unavailable)' : '');
    const disabled = b.available === false ? ' disabled' : '';
    return '<option value="' + escAttr(b.id) + '"' + selected + disabled + '>' + esc(label) + '</option>';
  }).join('');
  return '<div class="nz-field">' +
    '<label class="nz-field-label" for="' + escAttr(selectId) + '">CLI backend</label>' +
    '<span class="picker-select-wrap"><select id="' + escAttr(selectId) + '" class="nz-picker-select nz-picker-select-only">' +
    options +
    '</select></span>' +
    '</div>';
}
