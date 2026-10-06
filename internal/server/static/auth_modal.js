// auth_modal.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block and the export block below are the only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Shared state is read from the state.js objects and helpers are
// imported. session_list.js imports this module, so the session_list functions
// it calls back are shell slots, as is dashboard's renderMainShell.
import { NZ_CONTRACT } from './contract.js';
import { perSession, selection, serverInfo, sessionList, transcript } from './state.js';
import { esc, escAttr, showToast, trapFocus } from './nz_util.js';
import { sessionStream } from './session_stream.js';
import { wsm } from './ws_manager.js';
import { accessProfileChipInfo, fetchAccessProfiles, fetchCLIBackends, renderAccessProfilePicker, renderBackendPicker } from './backend_catalog.js';
import { authModalCooldown, eagerBindWorkspace, mobileEnterChat, persistPending, setActiveSessionCard, setMsgValue, shortPath, showNetworkError, stopPreviewPolling } from './utilities.js';
import { getNodeDisplayName, getNodeStatus, isMultiNode, nodeColor, projectDisplayLabel, projectDisplayPrefix, statusLabelForNode } from './session_ident.js';
import { navRebuild } from './msg_nav.js';
import { sendMessage } from './send_message.js';
import { shell } from './shell.js';

// --- Auth modal ---

async function saveToken() {
  const input = document.getElementById('token-input');
  const t = input && input.value.trim();
  if (!t) return;
  document.querySelectorAll('.modal-overlay .auth-refusal').forEach((n) => n.remove());
  try {
    const r = await fetch(NZ_CONTRACT.API.auth_login, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({token: t})
    });
    if (r.ok) {
      authModalCooldown.until = 0; // fresh session — drop any dismiss cooldown
      const overlay = document.querySelector('.modal-overlay');
      if (overlay) overlay.remove();
      wsm.disconnect();
      wsm.connect();
      shell.fetchSessions();
    } else if (r.status === 429) {
      // Rate-limited, token never compared: gate the input for Retry-After
      // (plain integer seconds) instead of inviting more 429s.
      let retryAfter = parseInt(r.headers.get('Retry-After') || '60', 10);
      if (!Number.isFinite(retryAfter) || retryAfter <= 0) retryAfter = 60;
      startLoginRetryCountdown(retryAfter);
    } else if (r.status === 400) {
      // Refused before the token compare (e.g. trusted_proxy without XFF), so the token stays.
      const reason = await r.json().then((b) => String((b && b.error) || ''), () => '');
      const note = Object.assign(document.createElement('div'), { className: 'auth-hint auth-refusal' });
      note.textContent = (reason || 'login refused (HTTP 400)').slice(0, 300);
      document.querySelector('.modal-overlay .auth-hint')?.after(note);
    } else {
      document.getElementById('token-input').value = '';
      document.getElementById('token-input').placeholder = 'invalid token — try again';
    }
  } catch(e) {
    showNetworkError('', e);
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
// inline sidebar-status countdown (via shell.updateStatusBar, so it stays out
// of the way on mobile) rather than a toast. Triggered by an
// auth_fail(Error="too many attempts") message that carries a retry_after
// hint. On expiry the gate clears and wsm.connect() fires once so the user
// doesn't have to click anything — matches the UX-P1 auto-recover spec.
//
// Idempotent: calling twice (e.g. a second in-flight reconnect that races
// through before the gate armed) clears the prior tick interval so the
// countdown reflects the freshest server directive, not a stale one.
let _wsAuthCountdownTimer = null;
function startWSAuthRetryCountdown(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) seconds = 60;
  wsm._authBlockUntil = Date.now() + seconds * 1000;
  if (_wsAuthCountdownTimer) {
    clearInterval(_wsAuthCountdownTimer);
    _wsAuthCountdownTimer = null;
  }
  // Repaint the sidebar immediately so the "鉴权过于频繁 · Ns" row appears
  // without waiting for the next 1s tick. shell.updateStatusBar reads
  // wsm._authBlockUntil directly, so we don't need to pass the remaining
  // seconds around.
  shell.updateStatusBar();
  _wsAuthCountdownTimer = setInterval(() => {
    if (Date.now() >= wsm._authBlockUntil) {
      clearInterval(_wsAuthCountdownTimer);
      _wsAuthCountdownTimer = null;
      wsm._authBlockUntil = 0;
      // Clear the existing reconnect timer so connect() fires immediately
      // rather than waiting out whatever backoff was scheduled alongside
      // the countdown. Reset backoff so post-recovery reconnect behaves
      // like a fresh page load. No toast here — the sidebar status row
      // already moved from "鉴权过于频繁" to "connecting..." which is the
      // user-visible signal.
      if (wsm.reconnectTimer) { clearTimeout(wsm.reconnectTimer); wsm.reconnectTimer = null; }
      wsm.backoff = 1000;
      shell.updateStatusBar();
      wsm.connect();
      return;
    }
    shell.updateStatusBar();
  }, 1000);
}

// renderBackendFetchFailed is the picker-slot fallback when a REMOTE node's
// backends manifest could not be fetched: a one-line notice plus a retry
// button (wired by refreshBackendPicker) instead of silently showing no
// picker as if the node had a single backend.
function renderBackendFetchFailed(node) {
  return '<span class="cp-backend-fail">' + esc(getNodeDisplayName(node)) + ' 后端清单获取失败 ' +
    '<button type="button" class="settings-syslink-btn cp-backend-retry">重试</button></span>';
}

// getSelectedAccessProfile reads the modal's access-profile <select>. Empty
// string ("" = global default) when the picker isn't rendered or the default
// option is chosen.
function getSelectedAccessProfile() {
  const el = document.getElementById('new-access-profile');
  return el && el.value ? el.value : '';
}

// accessProfileChipHtml renders the per-session access-profile chip next to the
// backend chip. Empty string when single-auth mode or global default (layout
// unchanged for deployments that don't use profiles). Shows ONLY the display
// label — never auth details (RFC §8.3 / §8.4).
function accessProfileChipHtml(profileID) {
  const info = accessProfileChipInfo(profileID);
  if (!info) return '';
  return '<span class="sc-access-profile-chip" data-access-profile="' + escAttr(profileID || '') +
    '" data-nz-bg="' + escAttr(info.color) + '" title="' + escAttr(info.tooltip) +
    '">' + esc(info.label) + '</span>';
}

function getSelectedBackend() {
  const el = document.getElementById('new-backend');
  return el && el.value ? el.value : '';
}

// backendDisplayName resolves a backend ID ("claude" / "kiro" / ...) to the
// CLI display name surfaced by /api/cli/backends ("claude-code" / "kiro").
// Used to populate cli_name on dashboard-only pending sessions BEFORE the
// server has spawned the wrapper — without this, the sidebar icon
// (cliIcon) and header label (shell.renderMainShell / updateHeaderCLI) fall
// through to defaultCLIName ("claude-code") and a kiro session shows the
// claude logomark + "claude-code v..." until the first message lands and
// the server-side SetCLIName broadcasts the correct value.
//
// Resolution order:
//   1. serverInfo.cliBackends cache (canonical: dashboard already paid for this fetch
//      to render the picker, so the lookup is free).
//   2. Hardcoded ID→display map for the brief boot window where the
//      backend list hasn't resolved yet. Mirrors profile_claude.go /
//      profile_kiro.go DisplayName.
//   3. Backend ID itself as last-resort fallback (better than empty;
//      cliIcon's `=== 'kiro'` branch still works for kiro this way).
function backendDisplayName(backendID) {
  if (!backendID) return '';
  if (serverInfo.cliBackends && Array.isArray(serverInfo.cliBackends.backends)) {
    const e = serverInfo.cliBackends.backends.find(b => b && b.id === backendID);
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
// spawned. Empty when serverInfo.cliBackends has not resolved yet (caller hides
// the version suffix).
function backendDisplayVersion(backendID) {
  if (!backendID || !serverInfo.cliBackends || !Array.isArray(serverInfo.cliBackends.backends)) return '';
  const e = serverInfo.cliBackends.backends.find(b => b && b.id === backendID);
  return (e && e.version) ? e.version : '';
}

// renderNodePicker returns an HTML fragment for a connection (node) <select>,
// or an empty string when only the local node is connected (no meaningful
// choice to offer). It took over the modal slot the agent picker used to
// occupy: the sidebar node selector was removed, so choosing which connection
// a new session lives on now happens here, at creation time. Mirrors
// renderBackendPicker's shape — single <select id="new-node"> consumed by
// getSelectedNode() at submit time — and pre-selects the current selection.node
// so the picker matches whatever the operator was last working on.
//
// 'local' is always pinned first (defensive: even if the server's nodes
// payload omits it). Remotes follow, ordered by display name.
function renderNodePicker() {
  if (!isMultiNode()) return '';
  const ids = Object.keys(sessionList.nodesData);
  if (ids.indexOf('local') === -1) ids.unshift('local');
  const sorted = ids.slice().sort((a, b) => {
    if (a === 'local' && b !== 'local') return -1;
    if (b === 'local' && a !== 'local') return 1;
    return getNodeDisplayName(a).localeCompare(getNodeDisplayName(b));
  });
  const current = selection.node || 'local';
  const options = sorted.map(id => {
    const selected = id === current ? ' selected' : '';
    const status = getNodeStatus(id);
    const label = getNodeDisplayName(id) + ' · ' + statusLabelForNode(status);
    return '<option value="' + escAttr(id) + '"' + selected + '>' + esc(label) + '</option>';
  }).join('');
  return '<div class="nz-field">' +
    '<label class="nz-field-label" for="new-node">连接</label>' +
    '<span class="picker-select-wrap"><select id="new-node" class="nz-picker-select nz-picker-select-only">' +
    options +
    '</select></span>' +
    '</div>';
}

// getSelectedNode reads the modal's connection <select>. Falls back to the
// current selection.node (then 'local') when the picker isn't rendered — i.e.
// single-connection setups where there is nothing to choose.
function getSelectedNode() {
  const el = document.getElementById('new-node');
  const v = el && el.value ? el.value : '';
  return v || selection.node || 'local';
}

// wireNodePicker attaches a change listener to the modal's #new-node <select>
// (added imperatively to stay clear of CSP's inline-handler ban). Picking a
// connection updates the global selection.node + persists it, then runs the
// caller's onChange so a palette can re-filter its project list to the newly
// chosen node. No-op when the picker isn't present (single-node setups).
function wireNodePicker(onChange) {
  const el = document.getElementById('new-node');
  if (!el) return;
  el.addEventListener('change', function () {
    selection.node = el.value || 'local';
    try { localStorage.setItem('nz_selectedNode', selection.node); } catch (_) { /* noop */ }
    if (typeof onChange === 'function') onChange();
  });
}

// wireProfileRelabel repaints the backend picker in slotId when the access
// profile changes, so its 自动 option names that profile's default_backend.
function wireProfileRelabel(slotId) {
  const el = document.getElementById('new-access-profile');
  if (el) el.addEventListener('change', () => refreshBackendPicker(slotId));
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
// connected) are unaffected: selection.node stays 'local' and the filter
// reduces to the previous local-only behaviour.
function nodeFilteredProjects() {
  if (!Array.isArray(sessionList.projectsData)) return [];
  const target = selection.node || 'local';
  return sessionList.projectsData.filter(p => (p.node || 'local') === target);
}

// Alt+N opens a new session. Cmd/Ctrl+N is left alone so the browser's
// "new window" still works.
document.addEventListener('keydown', function(e) {
  if (e.altKey && (e.key === 'n' || e.key === 'N')) {
    const tag = (e.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
    e.preventDefault();
    createNewSession();
  }
});

function createNewSession() {
  // Fetch the CURRENTLY-SELECTED node's backends (null on failure or older
  // naozhi: no picker) with the access profiles; a node or profile switch
  // inside the modal repaints the picker via refreshBackendPicker.
  Promise.all([fetchCLIBackends(selection.node), fetchAccessProfiles()]).then(([backendsData, profilesData]) => {
    // serverInfo.defaultWorkspace 来自 local stats，远程节点没有对应的 client 端字段，
    // 因此选中 remote 时不预填路径，让用户显式输入远程上的工作目录。
    const isLocal = (selection.node || 'local') === 'local';
    const ws = isLocal ? (serverInfo.defaultWorkspace || '') : '';
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
          '<div class="nz-field">' +
            '<label class="nz-field-label" for="new-workspace">工作目录</label>' +
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
          const nowLocal = (selection.node || 'local') === 'local';
          const ph = nowLocal ? (serverInfo.defaultWorkspace || '') : '';
          wsEl.placeholder = ph;
          if (!wsEl.value) wsEl.value = ph;
        }
        refreshBackendPicker('new-backend-slot');
      });
      wireProfileRelabel('new-backend-slot');
      // First-open path: a failed REMOTE manifest arrives here as null and
      // renderBackendPicker(null) painted an empty slot. Route through
      // refreshBackendPicker so the retry notice shows on open too (#2429).
      if (!backendsData && (selection.node || 'local') !== 'local') refreshBackendPicker('new-backend-slot');
      setTimeout(() => document.getElementById('new-workspace').focus(), 100);
      return;
    }

    openProjectPalette(backendsData, profilesData);
  });
}

// refreshBackendPicker re-fetches the selected node's backend manifest and
// repaints the picker in slotId, keeping the current choice (自动 included)
// when that node still has it and labelling 自动 for the picked access
// profile. The new-session flows call it on node and access-profile switches.
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
  const reqNode = selection.node;
  fetchCLIBackends(reqNode).then(backendsData => {
    // The modal may have closed while the fetch was in flight.
    if (!document.getElementById(slotId)) return;
    // Drop a stale response whose node no longer matches the selection.
    if (selection.node !== reqNode) return;
    if (!backendsData && reqNode && reqNode !== 'local') {
      slot.innerHTML = renderBackendFetchFailed(reqNode);
      const retry = slot.querySelector('.cp-backend-retry');
      if (retry) retry.addEventListener('click', () => refreshBackendPicker(slotId));
      return;
    }
    slot.innerHTML = renderBackendPicker(backendsData, { selectedId: prevChoice, profileID: getSelectedAccessProfile() });
  });
}

function openProjectPalette(backendsData, profilesData) {
  const backendPicker = renderBackendPicker(backendsData);
  const accessProfilePicker = renderAccessProfilePicker(profilesData || serverInfo.accessProfiles);
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
    ? '<div class="cmd-palette-backend nz-chip-row">' +
        (accessProfilePicker ? '<div class="nz-flex-fill">' + accessProfilePicker + '</div>' : '') +
        '<div id="cp-backend-slot" class="nz-flex-fill">' + backendPicker + '</div>' +
        (nodePicker ? '<div class="nz-flex-fill">' + nodePicker + '</div>' : '') +
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
  // scoped to selection.node (nodeFilteredProjects), so a node switch must
  // repaint it against the current query. The backend picker also repaints
  // against the newly-selected node's manifest (picker node-aware fix).
  wireNodePicker(function () {
    renderPaletteList(state, input.value);
    refreshBackendPicker('cp-backend-slot');
  });
  wireProfileRelabel('cp-backend-slot');
  // First-open path: see the no-projects modal above — a null remote
  // manifest must surface the retry notice, not an empty slot (#2429).
  if (!backendsData && (selection.node || 'local') !== 'local') refreshBackendPicker('cp-backend-slot');
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
// (shortPath: home prefix collapsed to ~, long paths truncated) so the
// returned ranges index into the same string buildProjectRow highlights.
// Matching the raw path and then painting the ranges onto the short path
// shifted every <mark> by the collapsed prefix length (and could run past
// the end of the string) - #2429. If the visible text does not match but
// the full path does (e.g. the user typed the collapsed /home/<user>
// prefix), the row still qualifies with the full-path score but no
// highlight, so the result set is never narrower than before.
function matchProjectPath(query, path) {
  const shown = fuzzyMatch(query, shortPath(path));
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
  // Palette is scoped to selection.node: switching the node selector retargets
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
    //           entries (dir_mtime 0) sort last, then original sessionList.projectsData
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
      // Final stable tiebreak: original sessionList.projectsData order (input index).
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
    ? '<span class="cp-node" data-nz-bg="' + escAttr(nodeColor(nodeId)) + '">' + esc(nodeId) + '</span>'
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
  const emojiPrefix = projectDisplayPrefix(p);
  const displayName = projectDisplayLabel(p);
  const dispHint = (displayName && displayName !== p.name)
    ? ' <span class="cp-name-alias">(' + esc(displayName) + ')</span>'
    : '';
  el.innerHTML =
    icon +
    '<div class="cp-main">' +
      '<div class="cp-name">' + (emojiPrefix ? esc(emojiPrefix) : '') +
        highlight(p.name, s.nameRanges) + dispHint + '</div>' +
      '<div class="cp-path">' + highlight(shortPath(p.path), s.pathRanges) + '</div>' +
    '</div>' + nodeBadge;
  el.addEventListener('click', () => pickPaletteProject(p));
  return el;
}

// quickRowHint is the 「快速新建」 subtitle for the selected node. Only the
// LOCAL default workspace is known client-side (stats.default_workspace);
// a remote node resolves its own default on dispatch, so name the node
// instead of echoing the local path (#2429).
function quickRowHint(node) {
  if (!node || node === 'local') return serverInfo.defaultWorkspace ? shortPath(serverInfo.defaultWorkspace) : '';
  return getNodeDisplayName(node) + ' · 默认工作区';
}

function buildQuickRow(idx) {
  const hint = quickRowHint(selection.node || 'local');
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
  // serverInfo.defaultWorkspace 仍来自 local 的 stats（接口尚未按节点返回），使用前
  // 兜底为空串，由后端 SessionDispatcher 的远程默认工作目录解析。
  const node = selection.node || 'local';
  const workspace = node === 'local' ? (serverInfo.defaultWorkspace || '') : '';
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
    ? '打开自定义工作目录：<span class="nz-accent">' + esc(query) + '</span>'
    : '打开自定义工作目录…';
  el.innerHTML =
    '<span class="cp-icon">+</span>' +
    '<div class="cp-main"><div class="cp-name nz-mute">' + label + '</div></div>';
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
  // The palette is the "New Session" entry point: a project row always starts
  // a fresh timestamp-keyed session (mode:'new'), never the folder's
  // project-stable conversation, which its sidebar card continues (#2476).
  doCreateInProject(p.path, p.name, p.node || 'local', backend, agent,
    { mode: 'new', accessProfile: accessProfile });
}

function pickPaletteCustom(initialValue) {
  // Capture the palette's backend choice before we remove the overlay. The
  // Custom Workspace modal re-renders its own copies of the pickers, so we
  // carry the pre-selection forward rather than relying on the palette's DOM
  // (which is about to be nuked). The connection choice rides on the global
  // selection.node (wireNodePicker keeps it current), so renderNodePicker below
  // pre-selects it without an explicit hand-off.
  const preselectedBackend = getSelectedBackend();
  const preselectedProfile = getSelectedAccessProfile();
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (overlay) overlay.remove();
  // 选中 remote 节点时不用 local 的 serverInfo.defaultWorkspace 占位符，避免误导用户。
  const isLocal = (selection.node || 'local') === 'local';
  const ws = isLocal ? (serverInfo.defaultWorkspace || '') : '';
  const prefill = initialValue && (initialValue.startsWith('/') || initialValue.startsWith('~')) ? initialValue : '';
  // Re-render the access-profile + backend + connection pickers inside the
  // modal and pre-select the palette's choices, so switching to Custom
  // Workspace doesn't drop any of them. The backend picker is emitted from
  // the per-node cache (serverInfo.cliBackendsByNode) for the currently-selected node,
  // falling back to the local serverInfo.cliBackends for the boot window before the
  // per-node fetch resolves; refreshBackendPicker below repaints it against
  // the authoritative manifest and on every node switch (picker node-aware
  // fix).
  const accessProfilePicker = renderAccessProfilePicker(serverInfo.accessProfiles, { selectedId: preselectedProfile });
  const seedBackends = (selection.node && selection.node !== 'local' && serverInfo.cliBackendsByNode[selection.node])
    ? serverInfo.cliBackendsByNode[selection.node].data
    : serverInfo.cliBackends;
  const picker = renderBackendPicker(seedBackends, { selectedId: preselectedBackend, profileID: preselectedProfile });
  const nodePicker = renderNodePicker();
  const modal = document.createElement('div');
  modal.className = 'modal-overlay';
  modal.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="自定义工作目录">' +
      '<h3>自定义工作目录</h3>' +
      accessProfilePicker +
      '<div id="cw-backend-slot">' + picker + '</div>' +
      nodePicker +
      '<div class="nz-field">' +
        '<label class="nz-field-label" for="new-workspace">工作目录路径</label>' +
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
      const nowLocal = (selection.node || 'local') === 'local';
      wsEl.placeholder = nowLocal ? (serverInfo.defaultWorkspace || '') : '';
    }
    refreshBackendPicker('cw-backend-slot');
  });
  wireProfileRelabel('cw-backend-slot');
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
  sessionList.sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + sessionList.sessionCounter;
  // Continue → backend-provided stable key (precise continuation); new →
  // fresh timestamp key (independent parallel session). resolveSessionKey
  // also falls back to a timestamp key when no stableKey is available.
  const key = resolveSessionKey(mode, stableKey, projectName, agent, ts);

  perSession.workspaces[key] = projectPath;
  if (nodeId && nodeId !== 'local') perSession.nodes[key] = nodeId;
  if (backend) perSession.backends[key] = backend;
  if (accessProfile) perSession.accessProfiles[key] = accessProfile;
  delete perSession.sentPicks[key]; // a continued stable key must not inherit an earlier send's
  // Durably persist the pending workspace and eagerly bind it server-side so a
  // reload-before-first-send (the proven cwd-fallback trigger) no longer drops
  // the workspace. This is the primary fix path (project palette open).
  persistPending();
  eagerBindWorkspace(key, projectPath, nodeId);

  stopPreviewPolling();
  sessionStream.unsubscribe();
  selection.key = key;
  selection.node = nodeId || 'local';
  try { localStorage.setItem('nz_selectedNode', selection.node); } catch(_) {}
  transcript.lastEventTime = 0;
  mobileEnterChat();
  setActiveSessionCard(key, selection.node);
  shell.renderMainShell();
  navRebuild();
  sessionList.lastVersion = 0;
  shell.debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

function doCreateSession() {
  const workspace = document.getElementById('new-workspace').value.trim();
  const backend = getSelectedBackend();
  const accessProfile = getSelectedAccessProfile();
  const agent = getSelectedAgent();
  // Read the connection picker directly so the choice lands even if the
  // change listener never fired (e.g. the operator never re-opened the
  // select). getSelectedNode falls back to selection.node / 'local'.
  const targetNode = getSelectedNode();
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'session') : 'session';
  document.querySelector('.modal-overlay').remove();

  sessionList.sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + sessionList.sessionCounter;
  // R110-P3 key schema (see buildDashboardSessionKey godoc): 4 segments
  // with agentID as the terminal segment so buildSessionOpts picks up the
  // right AgentOpts entry.
  const key = buildDashboardSessionKey(ts, folderName, agent);

  if (workspace) perSession.workspaces[key] = workspace;
  if (backend) perSession.backends[key] = backend;
  if (accessProfile) perSession.accessProfiles[key] = accessProfile;
  if (targetNode !== 'local') perSession.nodes[key] = targetNode;
  // Persist + eager-bind so the custom workspace survives a reload-before-send.
  persistPending();
  if (workspace) eagerBindWorkspace(key, workspace, targetNode);

  stopPreviewPolling();
  sessionStream.unsubscribe();
  selection.key = key;
  selection.node = targetNode;
  try { localStorage.setItem('nz_selectedNode', selection.node); } catch(_) {}
  transcript.lastEventTime = 0;
  mobileEnterChat();
  setActiveSessionCard(key, targetNode);
  shell.renderMainShell();
  navRebuild();
  sessionList.lastVersion = 0;
  shell.debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

// createQuickSession opens a pre-configured session with zero clicks:
// general agent, default backend, default workspace (session.cwd). No modal,
// no palette, no project picker — optimised for "I just want to ask Claude
// something fast" without deciding where it lives first. The user can still
// /cd later if they want a different workspace, or rename the session.
//
// When `initialText` is non-empty, it is dropped into the composer after
// shell.renderMainShell paints AND sendMessage is invoked — so submitQuickAsk
// ships "type in empty-state → Enter → question flies" without the user
// having to click the composer a second time.
//
// shell.renderMainShell is synchronous and writes `#msg-input` into the DOM
// immediately, but we still defer the setMsgValue + sendMessage call by
// one rAF tick so the browser has a chance to flush layout (contenteditable
// focus + selection state is finicky before paint). If `#msg-input` is
// still missing after the tick we ship text back to the caller via the
// optional `onTextStranded` callback so the caller can re-enable its own
// input and surface a toast — prevents silent message loss if a future
// shell.renderMainShell refactor becomes async or conditional.
//
// Rationale: the modal + palette are the right default for project work, but
// they add 2-3 clicks to the common "quick lookup" case. Surfacing this
// entry point — paired with the empty-state quick-ask input — lets the
// palette stay rich without penalising quick queries.
function createQuickSession(initialText, onTextStranded) {
  // Close any lingering modal/palette so repeated entry-point triggers don't
  // stack overlays (e.g. a quick-ask fired while a modal was still mounted).
  document.querySelectorAll('.modal-overlay, .cmd-palette-overlay').forEach(el => el.remove());

  const workspace = serverInfo.defaultWorkspace || '';
  const agent = 'general';
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'quick') : 'quick';

  sessionList.sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + sessionList.sessionCounter;
  const key = buildDashboardSessionKey(ts, folderName, agent);

  if (workspace) perSession.workspaces[key] = workspace;
  // Backend left unset → router falls back to the configured default.
  // Persist so a reload-before-send keeps the workspace. No eager-bind: quick
  // sessions use serverInfo.defaultWorkspace, so the override would just mirror defaultCWD.
  persistPending();

  stopPreviewPolling();
  sessionStream.unsubscribe();
  selection.key = key;
  selection.node = 'local';
  try { localStorage.setItem('nz_selectedNode', selection.node); } catch(_) {}
  transcript.lastEventTime = 0;
  mobileEnterChat();
  setActiveSessionCard(key, 'local');
  shell.renderMainShell();
  navRebuild();
  sessionList.lastVersion = 0;
  shell.debouncedFetchSessions();
  const text = (initialText || '').trim();
  // requestAnimationFrame ensures the composer DOM produced by shell.renderMainShell
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
      setMsgValue(input, text);
      // sendMessage reads from #msg-input directly — no extra threading needed.
      sendMessage();
    } else {
      input.focus();
    }
  });
}

// submitQuickAsk is the Enter-key / submit-button handler for the empty-state
// "问点什么？" composer. Reads the textarea, creates a quick session, and
// forwards the text to sendMessage() in one shot — so the user goes
// "type → Enter → see answer" with zero intermediate clicks.
function submitQuickAsk(e) {
  if (e && e.preventDefault) e.preventDefault();
  const ta = document.getElementById('quick-ask-input');
  if (!ta) return;
  const text = (ta.value || '').trim();
  if (!text) { ta.focus(); return; }
  // Disable while the session spins up so a double-Enter can't fire two
  // sessions. shell.renderMainShell synchronously replaces the empty-state DOM
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
  // already aimed at something else. The overlay check inside the timer is
  // that same rule for a mount that lands within the 50ms: an open modal owns
  // the keyboard, and taking it back drops the operator's next keystrokes —
  // the dashboard token, when the auth modal is what opened — into this
  // textarea, where Enter submits them as a session prompt.
  if (autofocus && window.matchMedia && window.matchMedia('(pointer: fine)').matches) {
    setTimeout(() => { if (!document.querySelector('.modal-overlay, .cmd-palette-overlay')) ta.focus(); }, 50);
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
  accessProfileChipHtml,
  backendDisplayName,
  backendDisplayVersion,
  createNewSession,
  doCreateInProject,
  doCreateSession,
  getSelectedNode,
  highlight,
  keyTailDisplay,
  openProjectPalette,
  pickPaletteCustom,
  renderNodePicker,
  saveToken,
  startWSAuthRetryCountdown,
  wireNodePicker,
  wireQuickAskInput,
};
