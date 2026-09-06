// auth_modal.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureAuthModal(), called from dashboard's module body.
import { esc, escAttr, fetchJSON, nzState, showToast, trapFocus } from './nz_util.js';

const deps = {
  applyFeatureGates: null,
  cliBackendsByNode: null,
  debouncedFetchSessions: null,
  eagerBindWorkspace: null,
  fetchSessions: null,
  getNodeDisplayName: null,
  getNodeStatus: null,
  isMultiNode: null,
  mobileEnterChat: null,
  navRebuild: null,
  nodeColor: null,
  persistPending: null,
  projectDisplayLabel: null,
  projectDisplayPrefix: null,
  renderMainShell: null,
  sendMessage: null,
  sessionAccessProfiles: null,
  sessionBackends: null,
  sessionNodes: null,
  sessionWorkspaces: null,
  setActiveSessionCard: null,
  setMsgValue: null,
  shortPath: null,
  showNetworkError: null,
  statusLabelForNode: null,
  stopPreviewPolling: null,
  updateStatusBar: null,
  wsm: null,
};
export function configureAuthModal(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('auth_modal dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// --- Auth modal ---

// Auth-prompt de-dupe + debounce. Two guards keep the token modal from
// machine-gunning back open: (1) only ever one overlay at a time, and
// (2) after the operator explicitly dismisses the prompt, suppress
// *background* re-prompts (the 5s /api/sessions poll, WS reconnect) for a
// cooldown window. User-initiated actions (send / upload) pass {auto:false}
// and bypass the cooldown so a click still gets immediate feedback. A
// successful login clears the cooldown.
let _authModalCooldownUntil = 0;
const AUTH_MODAL_COOLDOWN_MS = 60000;

function showAuthModal(opts) {
  opts = opts || {};
  // De-dupe: never stack a second auth prompt over an existing modal.
  if (document.querySelector('.modal-overlay')) return;
  // Debounce: a freshly-dismissed prompt should not be reopened by the
  // next background poll. User actions (auto !== true) always prompt.
  if (opts.auto && Date.now() < _authModalCooldownUntil) return;
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="Dashboard API token">' +
      // R110-P3 brand lockup: `>_` mark + `naozhi` wordmark anchors the
      // login screen so operators recognize they're on the right service.
      // Mirrors the `>_` glyph used in the empty state; pure text (no image
      // asset) keeps the static bundle tiny.
      '<div class="auth-brand">' +
        '<div class="ab-mark" aria-hidden="true">&gt;_</div>' +
        '<div class="ab-wordmark">' +
          '<span class="ab-name">naozhi</span>' +
          '<span class="ab-tag">Claude Code on IM</span>' +
        '</div>' +
      '</div>' +
      '<h3>Dashboard API Token</h3>' +
      // R110-P3 brand/onboarding hint: first-time operators often don't know
      // where the token comes from. Points them at the one configuration
      // surface (dashboard_token in config.yaml). Kept concise; full docs live
      // in README.md and docs/ops/ so the modal stays task-focused.
      '<div class="auth-hint">token 配置于 <code>config.yaml</code> 的 <code>dashboard_token</code> 字段</div>' +
      '<input id="token-input" type="password" placeholder="请输入 dashboard token…" data-action-keydown="token-input-key">' +
      '<div class="modal-btns">' +
        '<button type="button" data-action="auth-dismiss">取消</button>' +
        '<button type="button" class="primary" data-action="token-save">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  setTimeout(() => document.getElementById('token-input').focus(), 100);
}

// dismissAuthModal closes the auth prompt and starts the background-reprompt
// cooldown so the next /api/sessions poll (or WS reconnect) doesn't pop it
// straight back open. The operator can still trigger it immediately via an
// explicit send/upload.
function dismissAuthModal() {
  _authModalCooldownUntil = Date.now() + AUTH_MODAL_COOLDOWN_MS;
  const overlay = document.querySelector('.modal-overlay');
  if (overlay) overlay.remove();
}

async function saveToken() {
  const input = document.getElementById('token-input');
  const t = input && input.value.trim();
  if (!t) return;
  try {
    const r = await fetch(NZ_CONTRACT.API.auth_login, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({token: t})
    });
    if (r.ok) {
      _authModalCooldownUntil = 0; // fresh session — drop any dismiss cooldown
      const overlay = document.querySelector('.modal-overlay');
      if (overlay) overlay.remove();
      deps.wsm.disconnect();
      deps.wsm.connect();
      deps.fetchSessions();
    } else if (r.status === 429) {
      // R110-P2 WS auth rate-limit countdown: the old catch-all else
      // path rendered "invalid token — try again" even when the server
      // was still locking the caller out, misleading users into retrying
      // immediately and racking up more 429s. Read Retry-After (seconds,
      // plain integer as set by dashboard_auth.go) and visually gate the
      // input until the window elapses.
      const raHeader = r.headers.get('Retry-After') || '60';
      let retryAfter = parseInt(raHeader, 10);
      if (!Number.isFinite(retryAfter) || retryAfter <= 0) retryAfter = 60;
      startLoginRetryCountdown(retryAfter);
    } else {
      document.getElementById('token-input').value = '';
      document.getElementById('token-input').placeholder = 'invalid token — try again';
    }
  } catch(e) {
    deps.showNetworkError('', e);
  }
}

// startLoginRetryCountdown disables the auth modal input and save button
// for `seconds` seconds, ticking down a human-readable placeholder each
// second. The tick is driven by setInterval — good enough for a 60s
// countdown, not a precise-timing primitive. Re-entering (second 429
// before the first countdown completes) clears the prior timer via
// dataset.countdownId so we don't stack intervals on the same input.
function startLoginRetryCountdown(seconds) {
  const input = document.getElementById('token-input');
  const saveBtn = document.querySelector('.modal-overlay .modal-btns button.primary');
  if (!input) return;
  input.value = '';
  // Clear any prior countdown timer before starting a new one.
  if (input.dataset.countdownId) {
    clearInterval(parseInt(input.dataset.countdownId, 10));
    delete input.dataset.countdownId;
  }
  input.disabled = true;
  if (saveBtn) saveBtn.disabled = true;
  let remaining = seconds;
  const render = () => {
    input.placeholder = '登录尝试过多，请在 ' + remaining + 's 后重试';
  };
  render();
  const id = setInterval(() => {
    remaining -= 1;
    if (remaining <= 0) {
      clearInterval(id);
      delete input.dataset.countdownId;
      input.disabled = false;
      if (saveBtn) saveBtn.disabled = false;
      input.placeholder = '请输入 dashboard token…';
      input.focus();
      return;
    }
    render();
  }, 1000);
  input.dataset.countdownId = String(id);
}

// startWSAuthRetryCountdown arms the auth rate-limit gate and drives an
// inline sidebar-status countdown instead of a top-of-screen toast. The
// previous toast variant stacked on top of the header on mobile and
// repeated every second; routing the countdown into deps.updateStatusBar keeps
// the signal visible but out of the way. Triggered by an
// auth_fail(Error="too many attempts") message that carries a retry_after
// hint. On expiry the gate clears and deps.wsm.connect() fires once so the user
// doesn't have to click anything — matches the UX-P1 auto-recover spec.
//
// Idempotent: calling twice (e.g. a second in-flight reconnect that races
// through before the gate armed) clears the prior tick interval so the
// countdown reflects the freshest server directive, not a stale one.
let _wsAuthCountdownTimer = null;
function startWSAuthRetryCountdown(seconds) {
  if (typeof deps.wsm === 'undefined' || !deps.wsm) return;
  if (!Number.isFinite(seconds) || seconds <= 0) seconds = 60;
  deps.wsm._authBlockUntil = Date.now() + seconds * 1000;
  if (_wsAuthCountdownTimer) {
    clearInterval(_wsAuthCountdownTimer);
    _wsAuthCountdownTimer = null;
  }
  // Repaint the sidebar immediately so the "鉴权过于频繁 · Ns" row appears
  // without waiting for the next 1s tick. deps.updateStatusBar reads
  // deps.wsm._authBlockUntil directly, so we don't need to pass the remaining
  // seconds around.
  deps.updateStatusBar();
  _wsAuthCountdownTimer = setInterval(() => {
    if (Date.now() >= deps.wsm._authBlockUntil) {
      clearInterval(_wsAuthCountdownTimer);
      _wsAuthCountdownTimer = null;
      deps.wsm._authBlockUntil = 0;
      // Clear the existing reconnect timer so connect() fires immediately
      // rather than waiting out whatever backoff was scheduled alongside
      // the countdown. Reset backoff so post-recovery reconnect behaves
      // like a fresh page load. No toast here — the sidebar status row
      // already moved from "鉴权过于频繁" to "connecting..." which is the
      // user-visible signal.
      if (deps.wsm.reconnectTimer) { clearTimeout(deps.wsm.reconnectTimer); deps.wsm.reconnectTimer = null; }
      deps.wsm.backoff = 1000;
      deps.updateStatusBar();
      deps.wsm.connect();
      return;
    }
    deps.updateStatusBar();
  }, 1000);
}

// fetchCLIBackends retrieves the enabled CLI backends from the server.
// Cached for 60 seconds — the set only changes across naozhi restarts.
// Resolves to null on network/auth failure so the caller can fall back to
// the no-picker flow (single-backend mode).
//
// node (optional) selects which node's manifest to fetch for the node-aware
// new-session picker. Omitted / 'local' returns the local manifest and keeps
// the global nzState.cliBackends cache (which every chip / feature-gate consumer
// reads) warm. A remote node id appends ?node=<id> so the primary proxies to
// that node (picker node-aware fix); the remote result is cached per node in
// deps.cliBackendsByNode and does NOT touch the global nzState.cliBackends / feature gates.
async function fetchCLIBackends(node) {
  const isLocal = !node || node === 'local';
  if (isLocal) {
    if (nzState.cliBackends && Date.now() - nzState.cliBackendsFetchedAt < 60000) {
      return nzState.cliBackends;
    }
  } else {
    const hit = deps.cliBackendsByNode[node];
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
      nzState.cliBackends = manifest;
      nzState.cliBackendsFetchedAt = Date.now();
      // Multi-Backend RFC §8.3 D9-D15: re-apply feature gates whenever the
      // LOCAL backends manifest lands. The boot path fires fetchCLIBackends()
      // in parallel with deps.fetchSessions, so the very first deps.renderMainShell may
      // have run with nzState.cliBackends==null — call gates here so the input
      // controls update once the manifest is available. Remote-node fetches
      // must NOT drive feature gates (the input controls operate on the
      // locally-selected session), so this stays inside the isLocal branch.
      deps.applyFeatureGates();
    } else if (manifest) {
      deps.cliBackendsByNode[node] = { data: manifest, at: Date.now() };
    } else {
      // A null / malformed remote manifest must not be pinned for 60s —
      // drop any stale entry so the next open refetches (#2429).
      delete deps.cliBackendsByNode[node];
    }
    return manifest;
  } catch (e) {
    if (!isLocal) delete deps.cliBackendsByNode[node];
    return null;
  }
}

// renderBackendFetchFailed is the picker-slot fallback when a REMOTE node's
// backends manifest could not be fetched: a one-line notice plus a retry
// button (wired by refreshBackendPicker) instead of silently showing no
// picker as if the node had a single backend.
function renderBackendFetchFailed(node) {
  return '<span class="cp-backend-fail">' + esc(deps.getNodeDisplayName(node)) + ' 后端清单获取失败 ' +
    '<button type="button" class="settings-syslink-btn cp-backend-retry">重试</button></span>';
}

// fetchAccessProfiles caches /api/access-profiles for 60s (same policy as
// fetchCLIBackends). Returns {profiles:[...], default} or null on error / when
// no profiles are configured (single-auth deployments — the picker/chip then
// stay hidden). RFC project-access-profile §8.
async function fetchAccessProfiles() {
  if (nzState.accessProfiles && Date.now() - nzState.accessProfilesFetchedAt < 60000) {
    return nzState.accessProfiles;
  }
  try {
    const data = await fetchJSON(NZ_CONTRACT.API.access_profiles, {credentials: 'same-origin'});
    nzState.accessProfiles = data && Array.isArray(data.profiles) ? data : null;
    nzState.accessProfilesFetchedAt = Date.now();
    return nzState.accessProfiles;
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
function renderAccessProfilePicker(profilesData, opts) {
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
  return '<div style="margin-bottom:12px">' +
    '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="' + escAttr(selectId) + '">访问档</label>' +
    '<select id="' + escAttr(selectId) + '" style="' + PICKER_SELECT_STYLE + '">' +
    options +
    '</select>' +
    '</div>';
}

// getSelectedAccessProfile reads the modal's access-profile <select>. Empty
// string ("" = global default) when the picker isn't rendered or the default
// option is chosen.
function getSelectedAccessProfile() {
  const el = document.getElementById('new-access-profile');
  return el && el.value ? el.value : '';
}

// accessProfileChipInfo resolves an access-profile id to {label,color,tooltip}
// for the session card chip, or null when single-auth mode (≤1 profile) or the
// id is empty. An id not in the registry (deleted profile) renders a neutral
// chip with the raw id so the orphan state is visible. NEVER surfaces env/token.
function accessProfileChipInfo(profileID) {
  if (!nzState.accessProfiles || !Array.isArray(nzState.accessProfiles.profiles)) return null;
  if (nzState.accessProfiles.profiles.length <= 1) return null; // single-auth mode
  if (!profileID) return null; // global default → no chip (matches "no overlay")
  const entry = nzState.accessProfiles.profiles.find(p => p && p.id === profileID);
  if (!entry) {
    return { label: profileID, color: 'var(--nz-text-mute)', tooltip: '访问档未配置: ' + profileID };
  }
  return {
    label: entry.display_name || entry.id,
    color: entry.chip_color || 'var(--nz-accent)',
    tooltip: (entry.display_name || entry.id) + (entry.default_model ? ' · ' + entry.default_model : ''),
  };
}

// accessProfileChipHtml renders the per-session access-profile chip next to the
// backend chip. Empty string when single-auth mode or global default (layout
// unchanged for deployments that don't use profiles). Shows ONLY the display
// label — never auth details (RFC §8.3 / §8.4).
function accessProfileChipHtml(profileID) {
  const info = accessProfileChipInfo(profileID);
  if (!info) return '';
  return '<span class="sc-access-profile-chip" data-access-profile="' + escAttr(profileID || '') +
    '" style="background-color:' + escAttr(info.color) + '" title="' + escAttr(info.tooltip) +
    '">' + esc(info.label) + '</span>';
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
// PICKER_SELECT_STYLE is the shared inline style for the modal backend/agent
// <select> controls (full-width, design-token surface). Defined once so the
// backend and agent pickers can't drift apart (R20260610-UI-5).
const PICKER_SELECT_STYLE = 'width:100%;padding:6px 8px;background:var(--nz-bg-0);color:var(--nz-text);border:1px solid var(--nz-border);border-radius:4px';
// Selects opt out of the native OS chrome (ui-polish-light-theme D6): the
// system-drawn control clashed with the tokenised palette/modal surfaces.
// appearance:none removes the native arrow too, so every <select> using this
// style MUST be wrapped in <span class="picker-select-wrap"> which paints a
// CSS arrow (pointer-events:none, keyboard/AT behaviour untouched).
const PICKER_SELECT_ONLY_STYLE = PICKER_SELECT_STYLE + ';appearance:none;-webkit-appearance:none;padding-right:26px;cursor:pointer;font:inherit;font-size:var(--nz-fs-sm2)';

function renderBackendPicker(backendsData, opts) {
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
  return '<div style="margin-bottom:12px">' +
    '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="' + escAttr(selectId) + '">CLI backend</label>' +
    '<span class="picker-select-wrap"><select id="' + escAttr(selectId) + '" style="' + PICKER_SELECT_ONLY_STYLE + '">' +
    options +
    '</select></span>' +
    '</div>';
}

function getSelectedBackend() {
  const el = document.getElementById('new-backend');
  return el && el.value ? el.value : '';
}

// backendDisplayName resolves a backend ID ("claude" / "kiro" / ...) to the
// CLI display name surfaced by /api/cli/backends ("claude-code" / "kiro").
// Used to populate cli_name on dashboard-only pending sessions BEFORE the
// server has spawned the wrapper — without this, the sidebar icon
// (cliIcon) and header label (deps.renderMainShell / updateHeaderCLI) fall
// through to defaultCLIName ("claude-code") and a kiro session shows the
// claude logomark + "claude-code v..." until the first message lands and
// the server-side SetCLIName broadcasts the correct value.
//
// Resolution order:
//   1. nzState.cliBackends cache (canonical: dashboard already paid for this fetch
//      to render the picker, so the lookup is free).
//   2. Hardcoded ID→display map for the brief boot window where the
//      backend list hasn't resolved yet. Mirrors profile_claude.go /
//      profile_kiro.go DisplayName.
//   3. Backend ID itself as last-resort fallback (better than empty;
//      cliIcon's `=== 'kiro'` branch still works for kiro this way).
function backendDisplayName(backendID) {
  if (!backendID) return '';
  if (nzState.cliBackends && Array.isArray(nzState.cliBackends.backends)) {
    const e = nzState.cliBackends.backends.find(b => b && b.id === backendID);
    if (e && (e.display_name || e.id)) return e.display_name || e.id;
  }
  if (backendID === 'claude') return 'claude-code';
  return backendID;
}

// backendDisplayVersion returns the version string the dashboard should
// show next to the backend display name for a pending session. Pulled
// from the cached /api/cli/backends payload — that endpoint reports the
// installed CLI version for each enabled backend, which is the same
// value the wrapper would set on the session via SetCLIVersion once
// spawned. Empty when nzState.cliBackends has not resolved yet (caller hides
// the version suffix).
function backendDisplayVersion(backendID) {
  if (!backendID || !nzState.cliBackends || !Array.isArray(nzState.cliBackends.backends)) return '';
  const e = nzState.cliBackends.backends.find(b => b && b.id === backendID);
  return (e && e.version) ? e.version : '';
}

// renderNodePicker returns an HTML fragment for a connection (node) <select>,
// or an empty string when only the local node is connected (no meaningful
// choice to offer). It took over the modal slot the agent picker used to
// occupy: the sidebar node selector was removed, so choosing which connection
// a new session lives on now happens here, at creation time. Mirrors
// renderBackendPicker's shape — single <select id="new-node"> consumed by
// getSelectedNode() at submit time — and pre-selects the current nzState.selectedNode
// so the picker matches whatever the operator was last working on.
//
// 'local' is always pinned first (defensive: even if the server's nodes
// payload omits it). Remotes follow, ordered by display name.
function renderNodePicker() {
  if (!deps.isMultiNode()) return '';
  const ids = Object.keys(nzState.nodesData);
  if (ids.indexOf('local') === -1) ids.unshift('local');
  const sorted = ids.slice().sort((a, b) => {
    if (a === 'local' && b !== 'local') return -1;
    if (b === 'local' && a !== 'local') return 1;
    return deps.getNodeDisplayName(a).localeCompare(deps.getNodeDisplayName(b));
  });
  const current = nzState.selectedNode || 'local';
  const options = sorted.map(id => {
    const selected = id === current ? ' selected' : '';
    const status = deps.getNodeStatus(id);
    const label = deps.getNodeDisplayName(id) + ' · ' + deps.statusLabelForNode(status);
    return '<option value="' + escAttr(id) + '"' + selected + '>' + esc(label) + '</option>';
  }).join('');
  return '<div style="margin-bottom:12px">' +
    '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="new-node">连接</label>' +
    '<span class="picker-select-wrap"><select id="new-node" style="' + PICKER_SELECT_ONLY_STYLE + '">' +
    options +
    '</select></span>' +
    '</div>';
}

// getSelectedNode reads the modal's connection <select>. Falls back to the
// current nzState.selectedNode (then 'local') when the picker isn't rendered — i.e.
// single-connection setups where there is nothing to choose.
function getSelectedNode() {
  const el = document.getElementById('new-node');
  const v = el && el.value ? el.value : '';
  return v || nzState.selectedNode || 'local';
}

// wireNodePicker attaches a change listener to the modal's #new-node <select>
// (added imperatively to stay clear of CSP's inline-handler ban). Picking a
// connection updates the global nzState.selectedNode + persists it, then runs the
// caller's onChange so a palette can re-filter its project list to the newly
// chosen node. No-op when the picker isn't present (single-node setups).
function wireNodePicker(onChange) {
  const el = document.getElementById('new-node');
  if (!el) return;
  el.addEventListener('change', function () {
    nzState.selectedNode = el.value || 'local';
    try { localStorage.setItem('nz_selectedNode', nzState.selectedNode); } catch (_) { /* noop */ }
    if (typeof onChange === 'function') onChange();
  });
}

// getSelectedAgent resolves the agent segment for the session key. The agent
// picker was retired (its modal slot now hosts the connection picker), so
// every dashboard-created session uses the default 'general' agent. Kept as a
// single resolution point so buildDashboardSessionKey's call sites don't
// hardcode the literal and a future picker can re-route through here.
function getSelectedAgent() {
  return 'general';
}

// R110-P3 key schema (Round 167) — dashboard sessions historically used
//   'dashboard:direct:<ts>:<projectName>'
// which collides with the 4-segment SessionKey contract: buildSessionOpts
// reads parts[3] as the agentID, so projectName was silently getting looked
// up in the agents registry (returning zero AgentOpts{}) and AgentOpts was
// never actually applied. This helper emits the correct shape:
//   'dashboard:direct:<ts>-<slug>:<agentID>'
// where `<slug>` is the sanitized project/folder name and `<agentID>` maps
// to config.yaml's agents entries. Matches the shape scratch sessions already
// use (dashboard_session.go:860 — 'dashboard:direct:r<hex>:general').
//
// The sanitizer strips colons and control bytes (sanitizeKeyComponent on the
// server rejects them) and normalizes whitespace so the key remains readable
// in logs. Empty slug falls back to 'session' so the chatID segment is never
// empty (SanitizeLogAttr would accept it but downstream UI shows a blank).
function sanitizeKeySlug(s) {
  if (!s) return 'session';
  // Replace ASCII colons + Unicode lookalike colons (FULLWIDTH U+FF1A,
  // PRESENTATION FORM U+FE13, MODIFIER LETTER U+A789, RATIO U+2236) so a
  // project folder containing e.g. 'foo：bar' cannot survive as a
  // colon-like byte into the 4-segment key that strings.SplitN(":",4)
  // relies on server-side. Also strips every non-ASCII codepoint the
  // server-side session.ValidateSessionKey rejects (#2429): C1 controls
  // (U+0080-U+009F), zero-width / LTR-RTL marks (U+200B-U+200F), bidi
  // override / embedding (U+202A-U+202E), Unicode line separators
  // (U+2028/U+2029) and the BOM (U+FEFF), plus the directional isolates
  // (U+2066-U+2069) the IM-path sanitizer drops. A project directory
  // whose name carries a zero-width space would otherwise produce a key
  // the server 400s on first send, leaving a pending card that can never
  // send. The class is written with \uXXXX escapes ONLY - the Go contract
  // test TestDashboardJS_SanitizeKeySlug_CoversServerDenySet parses it
  // and asserts it is a superset of session.DeniedKeyRuneRanges. Then
  // collapse runs of filesystem-hostile chars into single dashes so the
  // key stays short and readable. Cap at 64 bytes to leave plenty of
  // headroom under the 128-byte sanitizeKeyComponent cap.
  let safe = String(s)
    .replace(/[:：︓꞉∶]/g, '-')
    .replace(/[\u0080-\u009f\u200b-\u200f\u2028\u2029\u202a-\u202e\u2066-\u2069\ufeff]/g, '')
    .replace(/[\s/\\?*<>|"\x00-\x1f\x7f]+/g, '-');
  safe = safe.replace(/-+/g, '-').replace(/^-|-$/g, '');
  if (safe.length > 64) safe = safe.slice(0, 64);
  return safe || 'session';
}

// localDateStamp renders a Date as YYYY-MM-DD-HHMMSS in LOCAL time for
// session-key timestamps. The previous toISOString (UTC date) +
// toTimeString (local time) mix dated keys created 00:00–08:00 UTC+8 as
// yesterday (#2429).
function localDateStamp(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + '-' +
    p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds());
}

function buildDashboardSessionKey(timestamp, projectOrFolder, agentID) {
  const slug = sanitizeKeySlug(projectOrFolder);
  const agent = agentID && String(agentID).trim() ? sanitizeKeySlug(agentID) : 'general';
  // chatID segment merges timestamp + slug so parts[3] remains the agentID
  // while still surfacing the project in log lines / sidebar fallbacks.
  return 'dashboard:direct:' + timestamp + '-' + slug + ':' + agent;
}

// resolveSessionKey decides the session key for opening a project
// (RFC docs/rfc/project-stable-session-key.md §4.4). Two modes:
//
//   'continue' — reuse the backend-provided project-stable key
//     (dashboard:pj:<wshash>:<agent>) so the conversation precisely
//     continues the same key's sessionID-rotation chain. The hash is
//     OWNED BY THE BACKEND (stableKey arg); the frontend only swaps the
//     trailing agent segment for the selected agent — a plain string op
//     that never touches the hash, so there is no sha256 to drift.
//
//   'new' — generate a fresh timestamp key (legacy path) so the user
//     gets an independent parallel conversation in the same project.
//
// Falls back to a timestamp key when continue mode is requested but no
// stableKey is available (feature disabled server-side, or a remote /
// path-less project), preserving the pre-feature behaviour. Pure: no
// side effects, unit-testable.
function resolveSessionKey(mode, stableKey, projectOrFolder, agentID, timestamp) {
  const agent = agentID && String(agentID).trim() ? sanitizeKeySlug(agentID) : 'general';
  if (mode === 'continue' && stableKey) {
    const parts = String(stableKey).split(':');
    // Expect dashboard:pj:<hash>:<agent>. Swap parts[3] for the selected
    // agent; if the shape is unexpected, fall through to a timestamp key
    // rather than emit a malformed key.
    if (parts.length === 4 && parts[0] === 'dashboard' && parts[1] === 'pj' && parts[2]) {
      return 'dashboard:pj:' + parts[2] + ':' + agent;
    }
  }
  return buildDashboardSessionKey(timestamp, projectOrFolder, agent);
}

// keyTailDisplay returns the most informative human-readable fallback for a
// session key's trailing display label. Historically dashboard.js used
// `parts[parts.length - 1]` directly, which made sense when the last segment
// was the projectName under the legacy `dashboard:direct:<ts>:<projectName>`
// schema. The Round 167 schema moves the agentID into that slot, so showing
// the bare agentID ('general', 'sonnet', …) as a session label would
// regress the UX: every pending session would read "general" in the header.
//
// The helper looks at the chatID segment (parts[2]) and, when it matches the
// dashboard key shape `<ts>-<slug>` (ts = `YYYY-MM-DD-HHMMSS-N`), prefers the
// trailing slug piece over the terminal agentID. For non-dashboard keys
// (IM platforms, scratch, cron) parts[2] is an opaque chat ID, so we retain
// the legacy tail-segment behaviour. Both behaviours are covered by contract
// tests in static_ux_contract_test.go.
function keyTailDisplay(keyParts) {
  if (!Array.isArray(keyParts) || keyParts.length === 0) return '';
  // Dashboard-shaped keys: platform:chatType:chatID:agentID with chatID of
  // the form `<ts>-<slug>`. ts is `YYYY-MM-DD-HHMMSS-N`, so we need to keep
  // the segment after the last `-` followed by a non-digit to isolate the
  // slug. Simpler heuristic: strip the leading ISO-ish numeric prefix and
  // return the remainder when it exists and isn't empty.
  if (keyParts.length >= 4 && keyParts[0] === 'dashboard' && keyParts[1] === 'direct') {
    const chatID = keyParts[2] || '';
    // Match `<ts>-<slug>` where ts begins with YYYY-MM-DD- (numeric only).
    const m = chatID.match(/^\d{4}-\d{2}-\d{2}-\d+-\d+-(.+)$/);
    if (m && m[1]) return m[1];
    // Fallback for chatID without the ts prefix: show the full chatID
    // (scratch / back-compat keys such as `dashboard:direct:r<hex>:general`).
    if (chatID) return chatID;
  }
  return keyParts[keyParts.length - 1] || '';
}

// nodeFilteredProjects returns the projects that live on the currently
// selected node so the "New session" palette only offers folders that
// physically reside on that node. When the user switches the node selector
// to a remote, the palette retargets to that remote's project list — so
// "create session in this remote workspace" is one click away. Cross-node
// creation is intentionally excluded: opening a project's CLI must happen
// from the node where that project lives.
//
// `node` is normalized the same way the rest of the dashboard does it
// (missing/empty → 'local') so legacy projects without a node field still
// surface when the local node is selected. Single-node hosts (no remotes
// connected) are unaffected: nzState.selectedNode stays 'local' and the filter
// reduces to the previous local-only behaviour.
function nodeFilteredProjects() {
  if (!Array.isArray(nzState.projectsData)) return [];
  const target = nzState.selectedNode || 'local';
  return nzState.projectsData.filter(p => (p.node || 'local') === target);
}

function createNewSession() {
  // Fetch backends upfront so the picker (if any) is ready when the modal
  // renders. Failure falls back to the single-backend UI — cli.backends
  // returns {} on older naozhi which fetchCLIBackends maps to null.
  //
  // Fetch the backend manifest for the CURRENTLY-SELECTED node so the picker
  // pre-selects that node's default backend, not the primary's (picker
  // node-aware fix). A node switch inside the modal re-fetches + repaints the
  // picker via refreshBackendPicker below.
  Promise.all([fetchCLIBackends(nzState.selectedNode), fetchAccessProfiles()]).then(([backendsData, profilesData]) => {
    // nzState.defaultWorkspace 来自 local stats，远程节点没有对应的 client 端字段，
    // 因此选中 remote 时不预填路径，让用户显式输入远程上的工作目录。
    const isLocal = (nzState.selectedNode || 'local') === 'local';
    const ws = isLocal ? (nzState.defaultWorkspace || '') : '';
    const backendPicker = renderBackendPicker(backendsData);
    const accessProfilePicker = renderAccessProfilePicker(profilesData);

    if (!nodeFilteredProjects().length) {
      const overlay = document.createElement('div');
      overlay.className = 'modal-overlay';
      overlay.innerHTML =
        '<div class="modal" role="dialog" aria-modal="true" aria-label="新建会话">' +
          '<h3>New Session</h3>' +
          accessProfilePicker +
          '<div id="new-backend-slot">' + backendPicker + '</div>' +
          renderNodePicker() +
          '<div style="margin-bottom:12px">' +
            '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="new-workspace">工作目录</label>' +
            '<input id="new-workspace" placeholder="' + escAttr(ws) + '" value="' + escAttr(ws) + '" data-action-keydown="create-session-key">' +
          '</div>' +
          '<div class="modal-btns">' +
            '<button type="button" data-action="modal-close">取消</button>' +
            '<button type="button" class="primary" data-action="session-create">创建</button>' +
          '</div>' +
        '</div>';
      document.body.appendChild(overlay);
      trapFocus(overlay);
      // Switching connection retargets where this custom-workspace session
      // lands; the workspace placeholder follows (local prefills the default
      // workspace, remotes clear it since we have no per-node default) and the
      // backend picker repaints against the newly-selected node's manifest.
      wireNodePicker(function () {
        const wsEl = document.getElementById('new-workspace');
        if (wsEl) {
          const nowLocal = (nzState.selectedNode || 'local') === 'local';
          const ph = nowLocal ? (nzState.defaultWorkspace || '') : '';
          wsEl.placeholder = ph;
          if (!wsEl.value) wsEl.value = ph;
        }
        refreshBackendPicker('new-backend-slot');
      });
      // First-open path: a failed REMOTE manifest arrives here as null and
      // renderBackendPicker(null) painted an empty slot. Route through
      // refreshBackendPicker so the retry notice shows on open too (#2429).
      if (!backendsData && (nzState.selectedNode || 'local') !== 'local') refreshBackendPicker('new-backend-slot');
      setTimeout(() => document.getElementById('new-workspace').focus(), 100);
      return;
    }

    openProjectPalette(backendsData, profilesData);
  });
}

// refreshBackendPicker re-fetches the backend manifest for the currently
// selected node and repaints the picker inside the given slot element. Used
// by the new-session flows when the connection picker changes node — without
// this the picker keeps showing (and pre-selecting the default of) whichever
// node was selected when the modal opened. Preserves the user's explicit
// choice when that backend id still exists on the newly-selected node.
function refreshBackendPicker(slotId) {
  const slot = document.getElementById(slotId);
  if (!slot) return;
  const prev = document.getElementById('new-backend');
  const prevChoice = prev ? prev.value : '';
  // Capture the target node at dispatch time. A remote fetch proxies through
  // the primary and can take seconds, during which the operator may switch the
  // connection picker back to a cached node whose fetch resolves first. Without
  // this guard the slower remote response repaints the slot with the WRONG
  // node's manifest + default — reopening the very "backend selected for the
  // wrong node" bug this file fixes, just as a race. Mirrors the events path's
  // dispatchNode guard (fetchSessionEvents).
  const reqNode = nzState.selectedNode;
  fetchCLIBackends(reqNode).then(backendsData => {
    // The modal may have closed while the fetch was in flight.
    if (!document.getElementById(slotId)) return;
    // Drop a stale response whose node no longer matches the selection.
    if (nzState.selectedNode !== reqNode) return;
    if (!backendsData && reqNode && reqNode !== 'local') {
      slot.innerHTML = renderBackendFetchFailed(reqNode);
      const retry = slot.querySelector('.cp-backend-retry');
      if (retry) retry.addEventListener('click', () => refreshBackendPicker(slotId));
      return;
    }
    slot.innerHTML = renderBackendPicker(backendsData, { selectedId: prevChoice });
  });
}

function openProjectPalette(backendsData, profilesData) {
  const backendPicker = renderBackendPicker(backendsData);
  const accessProfilePicker = renderAccessProfilePicker(profilesData || nzState.accessProfiles);
  const nodePicker = renderNodePicker();
  // The access-profile + backend + connection pickers sit inline above the
  // search box so the operator can pick them before choosing a project. The
  // connection picker replaced the agent picker that used to live here;
  // switching it re-filters the project list to that node's projects (see
  // nodeFilteredProjects).
  // The backend slot is always emitted (even empty) so a node switch that
  // moves from a single-backend node to a multi-backend one can inject the
  // picker in-place via refreshBackendPicker. min-width:0 keeps it collapsed
  // when empty so the flex row layout is unchanged for single-backend nodes.
  const pickerSlot = (accessProfilePicker || backendPicker || nodePicker)
    ? '<div class="cmd-palette-backend" style="padding:8px 12px 0;display:flex;gap:12px;flex-wrap:wrap">' +
        (accessProfilePicker ? '<div style="flex:1;min-width:0">' + accessProfilePicker + '</div>' : '') +
        '<div id="cp-backend-slot" style="flex:1;min-width:0">' + backendPicker + '</div>' +
        (nodePicker ? '<div style="flex:1;min-width:0">' + nodePicker + '</div>' : '') +
      '</div>'
    : '';
  const overlay = document.createElement('div');
  overlay.className = 'cmd-palette-overlay';
  overlay.innerHTML =
    '<div class="cmd-palette" role="dialog" aria-label="新建会话">' +
      pickerSlot +
      '<div class="cmd-palette-header">' +
        '<input id="cp-input" type="text" autocomplete="off" spellcheck="false" placeholder="搜索项目或输入路径…">' +
      '</div>' +
      '<div id="cp-list" class="cmd-palette-list" role="listbox"></div>' +
      '<div class="cmd-palette-footer">' +
        '<span><kbd>↑</kbd><kbd>↓</kbd> 切换</span>' +
        '<span><kbd>Enter</kbd> 打开</span>' +
        '<span><kbd>Esc</kbd> 关闭</span>' +
      '</div>' +
    '</div>';
  overlay.addEventListener('click', e => {
    if (e.target === overlay) overlay.remove();
  });
  document.body.appendChild(overlay);
  trapFocus(overlay);

  const state = {overlay, items: [], activeIdx: 0};
  const input = document.getElementById('cp-input');
  input.addEventListener('input', () => renderPaletteList(state, input.value));
  input.addEventListener('keydown', e => handlePaletteKey(e, state, input));
  // Re-filter the project list when the connection changes — the list is
  // scoped to nzState.selectedNode (nodeFilteredProjects), so a node switch must
  // repaint it against the current query. The backend picker also repaints
  // against the newly-selected node's manifest (picker node-aware fix).
  wireNodePicker(function () {
    renderPaletteList(state, input.value);
    refreshBackendPicker('cp-backend-slot');
  });
  // First-open path: see the no-projects modal above — a null remote
  // manifest must surface the retry notice, not an empty slot (#2429).
  if (!backendsData && (nzState.selectedNode || 'local') !== 'local') refreshBackendPicker('cp-backend-slot');
  renderPaletteList(state, '');
  setTimeout(() => input.focus(), 50);
}

function fuzzyMatch(query, text) {
  if (!query) return {score: 0, ranges: []};
  const t = text.toLowerCase();
  const q = query.toLowerCase();
  // Prefer contiguous substring match first.
  const idx = t.indexOf(q);
  if (idx >= 0) return {score: 1000 - idx, ranges: [[idx, idx + q.length]]};
  // Fallback: subsequence match (all chars in order).
  let ti = 0, qi = 0;
  const ranges = [];
  while (ti < t.length && qi < q.length) {
    if (t[ti] === q[qi]) {
      if (ranges.length && ranges[ranges.length - 1][1] === ti) {
        ranges[ranges.length - 1][1] = ti + 1;
      } else {
        ranges.push([ti, ti + 1]);
      }
      qi++;
    }
    ti++;
  }
  if (qi < q.length) return null;
  return {score: 100 - ranges.length, ranges};
}

// matchProjectPath fuzzy-matches the query against the path AS RENDERED
// (deps.shortPath: home prefix collapsed to ~, long paths truncated) so the
// returned ranges index into the same string buildProjectRow highlights.
// Matching the raw path and then painting the ranges onto the short path
// shifted every <mark> by the collapsed prefix length (and could run past
// the end of the string) - #2429. If the visible text does not match but
// the full path does (e.g. the user typed the collapsed /home/<user>
// prefix), the row still qualifies with the full-path score but no
// highlight, so the result set is never narrower than before.
function matchProjectPath(query, path) {
  const shown = fuzzyMatch(query, deps.shortPath(path));
  if (shown) return shown;
  const full = fuzzyMatch(query, path);
  return full ? {score: full.score, ranges: []} : null;
}

function highlight(text, ranges) {
  if (!ranges || !ranges.length) return esc(text);
  let out = '';
  let cursor = 0;
  for (const [s, e] of ranges) {
    out += esc(text.substring(cursor, s)) + '<mark>' + esc(text.substring(s, e)) + '</mark>';
    cursor = e;
  }
  out += esc(text.substring(cursor));
  return out;
}

function renderPaletteList(state, query) {
  const list = document.getElementById('cp-list');
  if (!list) return;
  const q = query.trim();
  const scored = [];
  // Palette is scoped to nzState.selectedNode: switching the node selector retargets
  // the palette so remote workspaces can be opened in one click. See
  // nodeFilteredProjects() for the rationale.
  nodeFilteredProjects().forEach(p => {
    if (!q) {
      scored.push({project: p, nameRanges: [], pathRanges: [], score: 0});
      return;
    }
    const nameM = fuzzyMatch(q, p.name);
    const pathM = matchProjectPath(q, p.path);
    if (!nameM && !pathM) return;
    const score = Math.max(nameM ? nameM.score + 500 : 0, pathM ? pathM.score : 0);
    scored.push({
      project: p,
      nameRanges: nameM ? nameM.ranges : [],
      pathRanges: pathM ? pathM.ranges : [],
      score,
    });
  });
  if (q) {
    scored.sort((a, b) => b.score - a.score);
  } else {
    // R110-P3 palette idle-state ordering: two-tier sort on empty query.
    //   Tier 0: favorites — surface "pinned" projects first. Users already
    //           star projects via the sidebar section-header ⭐ button,
    //           which persists to the backend projects config. Reusing
    //           that signal avoids a second "palette-pin" concept (which
    //           would split the mental model and duplicate state).
    //   Tier 1: everything else by directory filesystem mtime, most-recently-
    //           modified first (dir_mtime, unix ms, stamped by the backend at
    //           scan time). The folder you last touched on disk surfaces at
    //           the top of the non-favorite bucket; un-stat'able / older-remote
    //           entries (dir_mtime 0) sort last, then original nzState.projectsData
    //           order is the final stable tiebreak.
    const withIndex = scored.map((s, i) => ({s, i}));
    withIndex.sort((a, b) => {
      const pa = a.s.project;
      const pb = b.s.project;
      // Tier gate 0: favorite trumps everything else.
      const fa = pa.favorite ? 0 : 1;
      const fb = pb.favorite ? 0 : 1;
      if (fa !== fb) return fa - fb;
      // Tier gate 1: directory mtime descending (most-recently-modified
      // folder first). Missing/zero dir_mtime sorts last so un-stat'able or
      // older-protocol remote entries don't jump to the top.
      const ma = pa.dir_mtime || 0;
      const mb = pb.dir_mtime || 0;
      if (ma !== mb) return mb - ma;
      // Final stable tiebreak: original nzState.projectsData order (input index).
      return a.i - b.i;
    });
    scored.length = 0;
    withIndex.forEach(w => scored.push(w.s));
  }

  const items = [];
  if (!q) items.push({type: 'quick'});
  scored.forEach(s => items.push({type: 'project', data: s}));
  items.push({type: 'custom', query: q});
  state.items = items;
  state.activeIdx = 0;

  // Hover must move the keyboard cursor too (#2429), but only on a REAL
  // pointer move. Chrome re-dispatches mouseenter to whatever row lands under
  // a stationary pointer whenever the list re-renders (palette opening under
  // the cursor, every keystroke re-filtering). Binding activeIdx to
  // mouseenter therefore made Enter open the project row that happened to
  // sit under the mouse instead of row 0 (快速新建), which surfaced as
  // "new session resumes the folder's existing session". mousemove is only
  // fired for actual pointer motion, so drive the cursor from it instead.
  wirePaletteHover(list, state);

  if (!scored.length && q) {
    list.innerHTML = '<div class="cmd-palette-empty">No projects match "' + esc(q) + '"</div>';
    // Still render custom row below.
    const customEl = buildCustomRow(q, 0);
    list.appendChild(customEl);
    state.items = [{type: 'custom', query: q}];
    updateActiveRow(state);
    return;
  }

  list.innerHTML = '';
  items.forEach((it, i) => {
    let row;
    if (it.type === 'quick') {
      row = buildQuickRow(i);
    } else if (it.type === 'project') {
      row = buildProjectRow(it.data, i);
    } else {
      row = buildCustomRow(it.query, i);
    }
    list.appendChild(row);
  });
  updateActiveRow(state);
}

// wirePaletteHover installs (once per list element) a delegated mousemove
// handler that moves the keyboard cursor to the row under the pointer. It is
// deliberately NOT mouseenter: see the comment in renderPaletteList. Idempotent
// so renderPaletteList can call it on every re-render without stacking
// listeners; the handler reads `state` through the list element so a later
// render that swaps state objects keeps working.
function wirePaletteHover(list, state) {
  list._paletteState = state;
  if (list._paletteHoverWired) return;
  list._paletteHoverWired = true;
  list.addEventListener('mousemove', (e) => {
    const row = e.target && e.target.closest ? e.target.closest('.cmd-palette-item') : null;
    if (!row || !list.contains(row)) return;
    const idx = Number(row.dataset.idx);
    const st = list._paletteState;
    if (!st || !Number.isInteger(idx) || idx === st.activeIdx) return;
    setActiveIdx(st, idx);
  });
}

function buildProjectRow(s, idx) {
  const p = s.project;
  const el = document.createElement('div');
  el.className = 'cmd-palette-item' + (p.favorite ? ' is-favorite' : '');
  el.dataset.idx = String(idx);
  const nodeId = p.node || 'local';
  const nodeBadge = nodeId !== 'local'
    ? '<span class="cp-node" style="background:' + deps.nodeColor(nodeId) + '">' + esc(nodeId) + '</span>'
    : '';
  // R110-P3 palette favorite indicator: replace the leading ▸ glyph with
  // a ★ when the project is favorited so the tier-0 ranking is visually
  // explicit. Screen readers see the label via a title on the row icon
  // so the distinction isn't purely visual. Non-favorite projects keep
  // their original ▸ for continuity.
  const icon = p.favorite
    ? '<span class="cp-icon cp-icon-fav" title="已收藏" aria-label="已收藏">★</span>'
    : '<span class="cp-icon">▸</span>';
  // R110-P2 / #448: if the project has a configured emoji, render it
  // before the name so palette rows match the sidebar headers. The
  // raw `p.name` is still the search target (highlighted via
  // s.nameRanges below) so fuzzy queries don't have to know about
  // display_name; the visible label just gets a friendlier prefix.
  // display_name itself is appended as a small parenthetical hint
  // when it differs from the directory name — keeps the dirname
  // visible for operators who think in paths but adds the human
  // label for the rest.
  const emojiPrefix = deps.projectDisplayPrefix(p);
  const displayName = deps.projectDisplayLabel(p);
  const dispHint = (displayName && displayName !== p.name)
    ? ' <span class="cp-name-alias">(' + esc(displayName) + ')</span>'
    : '';
  el.innerHTML =
    icon +
    '<div class="cp-main">' +
      '<div class="cp-name">' + (emojiPrefix ? esc(emojiPrefix) : '') +
        highlight(p.name, s.nameRanges) + dispHint + '</div>' +
      '<div class="cp-path">' + highlight(deps.shortPath(p.path), s.pathRanges) + '</div>' +
    '</div>' + nodeBadge;
  el.addEventListener('click', () => pickPaletteProject(p));
  return el;
}

// quickRowHint is the 「快速新建」 subtitle for the selected node. Only the
// LOCAL default workspace is known client-side (stats.default_workspace);
// a remote node resolves its own default on dispatch, so name the node
// instead of echoing the local path (#2429).
function quickRowHint(node) {
  if (!node || node === 'local') return nzState.defaultWorkspace ? deps.shortPath(nzState.defaultWorkspace) : '';
  return deps.getNodeDisplayName(node) + ' · 默认工作区';
}

function buildQuickRow(idx) {
  const hint = quickRowHint(nzState.selectedNode || 'local');
  const el = document.createElement('div');
  el.className = 'cmd-palette-item';
  el.dataset.idx = String(idx);
  el.innerHTML =
    '<span class="cp-icon">⚡</span>' +
    '<div class="cp-main">' +
      '<div class="cp-name">快速新建</div>' +
      (hint ? '<div class="cp-path">' + esc(hint) + '</div>' : '') +
    '</div>';
  el.addEventListener('click', () => pickPaletteQuick());
  return el;
}

function pickPaletteQuick() {
  // 快速新建跟随当前选中节点：选中 remote 时让 quick session 也落在远程上，
  // 否则用户切到远程 workspace 后再点「快速新建」会意外回退到 local。
  // nzState.defaultWorkspace 仍来自 local 的 stats（接口尚未按节点返回），使用前
  // 兜底为空串，由后端 SessionDispatcher 的远程默认工作目录解析。
  const node = nzState.selectedNode || 'local';
  const workspace = node === 'local' ? (nzState.defaultWorkspace || '') : '';
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'quick') : 'quick';
  // Quick sessions are intentionally one-off (RFC §4.4): always a fresh
  // timestamp key, never a project-stable continuation.
  doCreateInProject(workspace, folderName, node, undefined, undefined, { mode: 'new' });
}

function buildCustomRow(query, idx) {
  const el = document.createElement('div');
  el.className = 'cmd-palette-item';
  el.dataset.idx = String(idx);
  const looksLikePath = query && (query.startsWith('/') || query.startsWith('~'));
  const label = looksLikePath
    ? '打开自定义工作目录：<span style="color:var(--nz-accent)">' + esc(query) + '</span>'
    : '打开自定义工作目录…';
  el.innerHTML =
    '<span class="cp-icon">+</span>' +
    '<div class="cp-main"><div class="cp-name" style="color:var(--nz-text-mute)">' + label + '</div></div>';
  el.addEventListener('click', () => pickPaletteCustom(query));
  return el;
}

// setActiveIdx is the single writer for the palette cursor: it records the
// index in state (what Enter reads) and repaints the .active class (what
// the user sees). Keyboard navigation and hover both route through it.
function setActiveIdx(state, idx) {
  state.activeIdx = idx;
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (!overlay) return;
  overlay.querySelectorAll('.cmd-palette-item').forEach(el => {
    el.classList.toggle('active', Number(el.dataset.idx) === idx);
  });
}

function updateActiveRow(state) {
  setActiveIdx(state, state.activeIdx);
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (!overlay) return;
  const active = overlay.querySelector('.cmd-palette-item.active');
  if (active && active.scrollIntoView) active.scrollIntoView({block: 'nearest'});
}

function handlePaletteKey(e, state, input) {
  if (e.key === 'Escape') {
    e.preventDefault();
    state.overlay.remove();
    return;
  }
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    state.activeIdx = Math.min(state.activeIdx + 1, state.items.length - 1);
    updateActiveRow(state);
    return;
  }
  if (e.key === 'ArrowUp') {
    e.preventDefault();
    state.activeIdx = Math.max(state.activeIdx - 1, 0);
    updateActiveRow(state);
    return;
  }
  if (e.key === 'Enter') {
    e.preventDefault();
    const item = state.items[state.activeIdx];
    if (!item) return;
    if (item.type === 'quick') pickPaletteQuick();
    else if (item.type === 'project') pickPaletteProject(item.data.project);
    else pickPaletteCustom(input.value.trim());
  }
}

function pickPaletteProject(p) {
  const backend = getSelectedBackend();
  const accessProfile = getSelectedAccessProfile();
  const agent = getSelectedAgent();
  // The palette is the "New Session" entry point, so a project row always
  // starts a fresh timestamp-keyed session (mode:'new'). Continuing the
  // project-stable conversation (dashboard:pj:<hash>) is what the sidebar
  // card for that session is for. Until v0.0.78 stats.projects carried no
  // stableKey, so this path always fell back to a fresh key in practice;
  // when the field appeared the row silently started resuming the folder's
  // existing (often running) session — exactly the opposite of what a user
  // clicking "New Session" asked for (#2476).
  doCreateInProject(p.path, p.name, p.node || 'local', backend, agent,
    { mode: 'new', accessProfile: accessProfile });
}

function pickPaletteCustom(initialValue) {
  // Capture the palette's backend choice before we remove the overlay. The
  // Custom Workspace modal re-renders its own copies of the pickers, so we
  // carry the pre-selection forward rather than relying on the palette's DOM
  // (which is about to be nuked). The connection choice rides on the global
  // nzState.selectedNode (wireNodePicker keeps it current), so renderNodePicker below
  // pre-selects it without an explicit hand-off.
  const preselectedBackend = getSelectedBackend();
  const preselectedProfile = getSelectedAccessProfile();
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (overlay) overlay.remove();
  // 选中 remote 节点时不用 local 的 nzState.defaultWorkspace 占位符，避免误导用户。
  const isLocal = (nzState.selectedNode || 'local') === 'local';
  const ws = isLocal ? (nzState.defaultWorkspace || '') : '';
  const prefill = initialValue && (initialValue.startsWith('/') || initialValue.startsWith('~')) ? initialValue : '';
  // Re-render the access-profile + backend + connection pickers inside the
  // modal and pre-select the palette's choices, so switching to Custom
  // Workspace doesn't drop any of them. The backend picker is emitted from
  // the per-node cache (deps.cliBackendsByNode) for the currently-selected node,
  // falling back to the local nzState.cliBackends for the boot window before the
  // per-node fetch resolves; refreshBackendPicker below repaints it against
  // the authoritative manifest and on every node switch (picker node-aware
  // fix).
  const accessProfilePicker = renderAccessProfilePicker(nzState.accessProfiles, { selectedId: preselectedProfile });
  const seedBackends = (nzState.selectedNode && nzState.selectedNode !== 'local' && deps.cliBackendsByNode[nzState.selectedNode])
    ? deps.cliBackendsByNode[nzState.selectedNode].data
    : nzState.cliBackends;
  const picker = renderBackendPicker(seedBackends, { selectedId: preselectedBackend });
  const nodePicker = renderNodePicker();
  const modal = document.createElement('div');
  modal.className = 'modal-overlay';
  modal.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="自定义工作目录">' +
      '<h3>自定义工作目录</h3>' +
      accessProfilePicker +
      '<div id="cw-backend-slot">' + picker + '</div>' +
      nodePicker +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="new-workspace">工作目录路径</label>' +
        '<input id="new-workspace" placeholder="' + escAttr(ws) + '" value="' + escAttr(prefill) + '" data-action-keydown="create-session-key">' +
      '</div>' +
      '<div class="modal-btns">' +
        '<button type="button" data-action="modal-close">取消</button>' +
        '<button type="button" class="primary" data-action="session-create">创建</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(modal);
  trapFocus(modal);
  // Repaint the picker against the selected node's authoritative manifest
  // (the seed above may be stale/local); preserves preselectedBackend when it
  // still exists on that node.
  refreshBackendPicker('cw-backend-slot');
  // Switching connection here clears/prefills the workspace placeholder the
  // same way the no-projects modal does, so a remote pick doesn't dangle the
  // local default path, and repaints the backend picker for the new node.
  wireNodePicker(function () {
    const wsEl = document.getElementById('new-workspace');
    if (wsEl) {
      const nowLocal = (nzState.selectedNode || 'local') === 'local';
      wsEl.placeholder = nowLocal ? (nzState.defaultWorkspace || '') : '';
    }
    refreshBackendPicker('cw-backend-slot');
  });
  setTimeout(() => {
    const el = document.getElementById('new-workspace');
    if (el) { el.focus(); el.select(); }
  }, 50);
}

function doCreateInProject(projectPath, projectName, nodeId, backend, agent, opts) {
  // Read the backend/agent from the still-mounted overlay BEFORE removing it,
  // so callers that omit the explicit argument still get the user's pick.
  if (backend === undefined) backend = getSelectedBackend();
  if (agent === undefined) agent = getSelectedAgent();
  opts = opts || {};
  // Access profile: prefer the explicit opts value; else read the overlay
  // picker before it is torn down (mirrors backend). "" = global default /
  // inherit project binding.
  const accessProfile = (opts.accessProfile !== undefined) ? opts.accessProfile : getSelectedAccessProfile();
  // opts: { mode: 'continue' | 'new', stableKey: string }. Default 'new':
  // every current caller (palette project row, quick session, custom
  // workspace) starts a fresh timestamp-keyed session. 'continue' (resume the
  // backend-supplied dashboard:pj: stableKey, RFC §4.4) currently has no
  // production caller — it is kept for a future explicit "继续对话" entry.
  // Do NOT flip the default back: with stableKey present that resumes the
  // folder's running session from the "New Session" button (#2476).
  const mode = opts.mode || 'new';
  const stableKey = opts.stableKey || '';
  const overlay = document.querySelector('.modal-overlay, .cmd-palette-overlay');
  if (overlay) overlay.remove();
  nzState.sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + nzState.sessionCounter;
  // Continue → backend-provided stable key (precise continuation); new →
  // fresh timestamp key (independent parallel session). resolveSessionKey
  // also falls back to a timestamp key when no stableKey is available.
  const key = resolveSessionKey(mode, stableKey, projectName, agent, ts);

  deps.sessionWorkspaces[key] = projectPath;
  if (nodeId && nodeId !== 'local') deps.sessionNodes[key] = nodeId;
  if (backend) deps.sessionBackends[key] = backend;
  if (accessProfile) deps.sessionAccessProfiles[key] = accessProfile;
  // Durably persist the pending workspace and eagerly bind it server-side so a
  // reload-before-first-send (the proven cwd-fallback trigger) no longer drops
  // the workspace. This is the primary fix path (project palette open).
  deps.persistPending();
  deps.eagerBindWorkspace(key, projectPath, nodeId);

  deps.stopPreviewPolling();
  deps.wsm.unsubscribe();
  nzState.selectedKey = key;
  nzState.selectedNode = nodeId || 'local';
  try { localStorage.setItem('nz_selectedNode', nzState.selectedNode); } catch(_) {}
  nzState.lastEventTime = 0;
  deps.mobileEnterChat();
  deps.setActiveSessionCard(key, nzState.selectedNode);
  deps.renderMainShell();
  deps.navRebuild();
  nzState.lastVersion = 0;
  deps.debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

function doCreateSession() {
  const workspace = document.getElementById('new-workspace').value.trim();
  const backend = getSelectedBackend();
  const accessProfile = getSelectedAccessProfile();
  const agent = getSelectedAgent();
  // Read the connection picker directly so the choice lands even if the
  // change listener never fired (e.g. the operator never re-opened the
  // select). getSelectedNode falls back to nzState.selectedNode / 'local'.
  const targetNode = getSelectedNode();
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'session') : 'session';
  document.querySelector('.modal-overlay').remove();

  nzState.sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + nzState.sessionCounter;
  // R110-P3 key schema (see buildDashboardSessionKey godoc): 4 segments
  // with agentID as the terminal segment so buildSessionOpts picks up the
  // right AgentOpts entry.
  const key = buildDashboardSessionKey(ts, folderName, agent);

  if (workspace) deps.sessionWorkspaces[key] = workspace;
  if (backend) deps.sessionBackends[key] = backend;
  if (accessProfile) deps.sessionAccessProfiles[key] = accessProfile;
  if (targetNode !== 'local') deps.sessionNodes[key] = targetNode;
  // Persist + eager-bind so the custom workspace survives a reload-before-send.
  deps.persistPending();
  if (workspace) deps.eagerBindWorkspace(key, workspace, targetNode);

  deps.stopPreviewPolling();
  deps.wsm.unsubscribe();
  nzState.selectedKey = key;
  nzState.selectedNode = targetNode;
  try { localStorage.setItem('nz_selectedNode', nzState.selectedNode); } catch(_) {}
  nzState.lastEventTime = 0;
  deps.mobileEnterChat();
  deps.setActiveSessionCard(key, targetNode);
  deps.renderMainShell();
  deps.navRebuild();
  nzState.lastVersion = 0;
  deps.debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

// createQuickSession opens a pre-configured session with zero clicks:
// general agent, default backend, default workspace (session.cwd). No modal,
// no palette, no project picker — optimised for "I just want to ask Claude
// something fast" without deciding where it lives first. The user can still
// /cd later if they want a different workspace, or rename the session.
//
// When `initialText` is non-empty, it is dropped into the composer after
// deps.renderMainShell paints AND deps.sendMessage is invoked — so submitQuickAsk
// ships "type in empty-state → Enter → question flies" without the user
// having to click the composer a second time.
//
// deps.renderMainShell is synchronous and writes `#msg-input` into the DOM
// immediately, but we still defer the deps.setMsgValue + deps.sendMessage call by
// one rAF tick so the browser has a chance to flush layout (contenteditable
// focus + selection state is finicky before paint). If `#msg-input` is
// still missing after the tick we ship text back to the caller via the
// optional `onTextStranded` callback so the caller can re-enable its own
// input and surface a toast — prevents silent message loss if a future
// deps.renderMainShell refactor becomes async or conditional.
//
// Rationale: the modal + palette are the right default for project work, but
// they add 2-3 clicks to the common "quick lookup" case. Surfacing this
// entry point — paired with the empty-state quick-ask input — lets the
// palette stay rich without penalising quick queries.
function createQuickSession(initialText, onTextStranded) {
  // Close any lingering modal/palette so repeated entry-point triggers don't
  // stack overlays (e.g. a quick-ask fired while a modal was still mounted).
  document.querySelectorAll('.modal-overlay, .cmd-palette-overlay').forEach(el => el.remove());

  const workspace = nzState.defaultWorkspace || '';
  const agent = 'general';
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'quick') : 'quick';

  nzState.sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + nzState.sessionCounter;
  const key = buildDashboardSessionKey(ts, folderName, agent);

  if (workspace) deps.sessionWorkspaces[key] = workspace;
  // Backend left unset → router falls back to the configured default.
  // Persist so a reload-before-send keeps the workspace. No eager-bind: quick
  // sessions use nzState.defaultWorkspace, so the override would just mirror defaultCWD.
  deps.persistPending();

  deps.stopPreviewPolling();
  deps.wsm.unsubscribe();
  nzState.selectedKey = key;
  nzState.selectedNode = 'local';
  try { localStorage.setItem('nz_selectedNode', nzState.selectedNode); } catch(_) {}
  nzState.lastEventTime = 0;
  deps.mobileEnterChat();
  deps.setActiveSessionCard(key, 'local');
  deps.renderMainShell();
  deps.navRebuild();
  nzState.lastVersion = 0;
  deps.debouncedFetchSessions();
  const text = (initialText || '').trim();
  // requestAnimationFrame ensures the composer DOM produced by deps.renderMainShell
  // is laid out before we write into it. Falls back to setTimeout when rAF
  // is unavailable (shouldn't happen on any supported browser but keeps the
  // branch testable in jsdom-style runners).
  const schedule = typeof requestAnimationFrame === 'function'
    ? requestAnimationFrame
    : (fn) => setTimeout(fn, 16);
  schedule(() => {
    const input = document.getElementById('msg-input');
    if (!input) {
      // Composer never materialised — surface the text back so the caller
      // can restore it rather than leaving the user staring at a blank
      // screen wondering where their question went.
      if (text && typeof onTextStranded === 'function') onTextStranded(text);
      return;
    }
    if (text) {
      deps.setMsgValue(input, text);
      // deps.sendMessage reads from #msg-input directly — no extra threading needed.
      deps.sendMessage();
    } else {
      input.focus();
    }
  });
}

// submitQuickAsk is the Enter-key / submit-button handler for the empty-state
// "问点什么？" composer. Reads the textarea, creates a quick session, and
// forwards the text to deps.sendMessage() in one shot — so the user goes
// "type → Enter → see answer" with zero intermediate clicks.
function submitQuickAsk(e) {
  if (e && e.preventDefault) e.preventDefault();
  const ta = document.getElementById('quick-ask-input');
  if (!ta) return;
  const text = (ta.value || '').trim();
  if (!text) { ta.focus(); return; }
  // Disable while the session spins up so a double-Enter can't fire two
  // sessions. deps.renderMainShell synchronously replaces the empty-state DOM
  // including this textarea, so the "re-enable" obligation falls on the
  // stranded-text callback below (only hit when the composer failed to
  // materialise, an edge case we still want to recover from).
  ta.disabled = true;
  const btn = document.querySelector('.quick-ask-send');
  if (btn) btn.disabled = true;
  createQuickSession(text, function(strandedText) {
    // Composer was supposed to appear but didn't. Put the text back in the
    // quick-ask box, re-enable controls, and tell the user so they can retry.
    // Guarded with existence checks because by this point the DOM may already
    // have been replaced by a late-arriving render.
    const ta2 = document.getElementById('quick-ask-input');
    const btn2 = document.querySelector('.quick-ask-send');
    if (ta2) { ta2.disabled = false; ta2.value = strandedText; ta2.focus(); }
    if (btn2) btn2.disabled = false;
    showToast('发送失败，请重试', 'error');
  });
}

// wireQuickAskInput binds the in-empty-state textarea to Enter-to-submit and
// auto-grow behaviour. Safe to call repeatedly — a data-bound marker prevents
// double-wire after mainEmptyHtml() re-renders on dismiss paths.
//
// autofocus: when true, steal keyboard focus to the textarea so "open the
// page, start typing" works with zero clicks. Dismiss paths pass false
// because the user may already be mid-click on the sidebar to switch to
// another session — grabbing focus 50ms later would intercept keystrokes.
function wireQuickAskInput(autofocus) {
  // Submit binding (#922 / #479): the empty-state form was migrated off the
  // inline `onsubmit=` attribute so script-src no longer needs it. The form is
  // (re)painted via innerHTML on cold start AND on every mainEmptyHtml()
  // repaint, so the DOMContentLoaded header-button binder cannot catch it —
  // wireQuickAskInput is the designated re-wire hook called on all those
  // paths, so the submit handler is bound here. Guarded by a dataset marker
  // to stay idempotent across repeated calls on the same node.
  const form = document.getElementById('quick-ask-form');
  if (form && form.dataset.wired !== '1') {
    form.dataset.wired = '1';
    form.addEventListener('submit', function(e) {
      e.preventDefault();
      submitQuickAsk(e);
    });
  }
  const ta = document.getElementById('quick-ask-input');
  if (!ta || ta.dataset.wired === '1') return;
  ta.dataset.wired = '1';
  ta.addEventListener('keydown', function(e) {
    // Enter (without modifiers) sends; Shift+Enter keeps native newline.
    if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey && !e.altKey && !e.isComposing) {
      e.preventDefault();
      submitQuickAsk(e);
    }
  });
  ta.addEventListener('input', function() {
    ta.style.height = 'auto';
    const next = Math.min(ta.scrollHeight, 200);
    ta.style.height = next + 'px';
  });
  // Autofocus only when the caller asks for it AND we're on a pointer-fine
  // device. On mobile we skip it — iOS Safari pops the keyboard and shifts
  // layout, which is worse UX than "tap to type". On dismiss-path repaints
  // we skip it to avoid intercepting a follow-up click/keystroke the user
  // already aimed at something else.
  if (autofocus && window.matchMedia && window.matchMedia('(pointer: fine)').matches) {
    setTimeout(() => ta.focus(), 50);
  }
}
// Wire on first paint (cold start HTML is already in the DOM). Cold start is
// the one path where autofocus is unambiguously wanted: the user just loaded
// the dashboard, there's no other UI they could be aiming at.
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', () => wireQuickAskInput(true));
} else {
  wireQuickAskInput(true);
}



export {
  PICKER_SELECT_ONLY_STYLE,
  PICKER_SELECT_STYLE,
  accessProfileChipHtml,
  accessProfileChipInfo,
  backendDisplayName,
  backendDisplayVersion,
  createNewSession,
  dismissAuthModal,
  doCreateInProject,
  doCreateSession,
  fetchAccessProfiles,
  fetchCLIBackends,
  getSelectedNode,
  highlight,
  keyTailDisplay,
  openProjectPalette,
  pickPaletteCustom,
  renderAccessProfilePicker,
  renderBackendPicker,
  renderNodePicker,
  saveToken,
  showAuthModal,
  startWSAuthRetryCountdown,
  wireNodePicker,
  wireQuickAskInput,
};
