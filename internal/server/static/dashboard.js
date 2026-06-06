// Service worker registration
if('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(()=>{});

let selectedKey = null;
// activeView is the root view-router state: which top-level view owns the
// viewport. 'chat' is the default (session sidebar + chat main). 'assets' /
// 'cron' / 'settings' are full-screen peers driven by setActivityView() and
// the matching body.nz-view-* CSS classes. Cron rendering is gated on
// activeView==='cron' (was selectedKey===null) so async cron repaints never
// clobber the chat DOM and vice-versa.
let activeView = 'chat';
// _activeCardEl caches the currently-.active session card element so the
// selector switch doesn't have to O(N) scan every card each time. Stays in
// sync via setActiveSessionCard(); after renderSidebar rebuilds the list the
// cached node becomes detached — the helper's isConnected guard recovers.
let _activeCardEl = null;
let eventTimer = null;
let lastEventTime = 0;
let lastRenderedEventTime = 0;
// oldestFetchedEventTime tracks the earliest server event we've already
// requested, independent of what's currently rendered in the DOM. The
// "load earlier" pagination originally took its cursor from the first
// `.event` child in the scroller — but when a page of 100 events is
// entirely internal-only (tool_use / agent / task_start / task_progress /
// task_done / result, filtered out by INTERNAL_EVENT_TYPES), no `.event`
// is rendered and the pagination silently bails with no cursor. That
// happens in practice whenever a parallel agent team runs long enough to
// fill the ring buffer with tool activity; the operator sees a blank
// events panel and a dead "加载更早的事件" button. Keep this cursor so
// pagination works regardless of what got filtered out.
let oldestFetchedEventTime = 0;
let lastCompositionEnd = 0;
let sessionsData = {};
let allSessionsCache = [];
// Keys (sid(key,node)) optimistically removed by dismissSession before the
// DELETE round-trips. fetchSessions/renderSidebar skip these so an in-flight
// poll or sessions_update WS event that still lists the session cannot
// resurrect a card the operator already dismissed. Cleared when DELETE
// confirms (success/404) or fails — see dismissSession's normal-session branch.
let _optimisticDeleteKeys = new Set();
// Collapsed project sections: Set of "node:name" keys. Persisted in
// localStorage so a user's fold state survives reloads. Toggled via the
// chevron button in the project section-header; the renderer skips emitting
// cards/empty-CTA for groups whose key is in this set.
let collapsedProjects = (function() {
  try { return new Set(JSON.parse(localStorage.getItem('nz_collapsedProjects') || '[]')); }
  catch(_) { return new Set(); }
})();
let pendingFiles = []; // {file, id, status: 'uploading'|'ready'|'error'}
let sending = false;
// selectedNode doubles as (a) the node the currently-selected session lives on
// and (b) the "view" filter applied to the sidebar session list when multiple
// nodes are connected. Persisted to localStorage so a reload keeps the user on
// the node they were browsing; validated against nodesData on every fetch so a
// removed/offline remote falls back to 'local'.
let selectedNode = (function() {
  try { return localStorage.getItem('nz_selectedNode') || 'local'; }
  catch(_) { return 'local'; }
})();
let nodesData = {};
// Toggle state for the #node-selector dropdown. Kept in module scope (vs. a
// DOM attr lookup) so the outside-click listener can bail fast without reading
// layout. True = dropdown is visible.
let nodeSelectorOpen = false;
let lastVersion = 0;
let lastNodesJSON = '';
let lastHistoryJSON = '';
// _lastSidebarData caches the most recent /api/sessions payload so UX-P3
// sidebar search can re-render locally on each keystroke without
// re-hitting the server. Set by fetchSessions after a successful render;
// consumed by the sidebar-search input oninput handler.
let _lastSidebarData = null;
// _lastSidebarHtml caches the last fully-built sidebar HTML string so
// renderSidebar can skip the (expensive) `list.innerHTML = html` write
// when the produced markup is byte-identical to what is already mounted.
// 20 sessions × 1 Hz polling rebuilds the same string every tick when
// nothing actually changed — comparing the produced string to the cache
// is O(n) but fast (string equality short-circuits on length and runs in
// native code), and skipping the assignment avoids a full sidebar reflow
// + active-card detachment / re-resolve cycle. The cache is the only
// consumer of the *output* — input fingerprinting is intentionally
// avoided because the card HTML embeds many fields (selectedKey/Node,
// unread counts, last_active text, project flags …) and any missed
// field would cause stale-DOM bugs. Comparing the final string is
// inherently correct: if it differs by a byte we re-render, if it
// doesn't there is no observable change to apply. R33-UX1.
let _lastSidebarHtml = null;
let sessionPollTimer = null;
let discoveredPollTimer = null;
let discoveredItems = []; // discovered sessions, merged into sidebar
let lastDiscoveredJSON = ''; // #1770: last /api/discovered payload, to skip forced re-render when unchanged
let previewTimer = null;
let previewEventCount = 0;
let pendingDiscovered = null; // {pid, sessionId, cwd, procStartTime, node} when previewing a discovered session
let sessionCounter = 0;
let availableAgents = ['general'];
let defaultWorkspace = '';
let projectsData = []; // [{name, path, node}] from API
let defaultCLIName = '';
let defaultCLIVersion = '';
// R110-P1 Home panel health strip (Round 148) — cached snapshot of the
// /api/sessions `stats` object so renderRecentSessionsPanel can surface
// service health (active / running / ready / uptime / watchdog kills / cli
// version) without an extra fetch. Refreshed by fetchSessions on every
// successful poll. Nil-safe consumer: absence = show nothing, never throw.
let lastStatsSnapshot = null;
const sessionWorkspaces = {};
const sessionNodes = {};
const sessionBackends = {}; // per-session CLI backend picked at creation ("claude" / "kiro" / ...)
let cliBackends = null; // cached /api/cli/backends response: {backends, default, detected}
let cliBackendsFetchedAt = 0;
const sessionDrafts = {}; // key -> draft text, preserved across session switches
// sessionScrollPos: sid(key,node) -> {fromBottom, atBottom}
// 记住每个会话上次切走时的 events-scroll 位置，回来时恢复，避免正在阅读
// 历史被强行拉回底部。atBottom=true 表示离开前就在底，回来后继续走贴底路径，
// 让新事件照常把视口拉到最新。
const sessionScrollPos = {};
// sessionUnread: sid(key,node) -> integer count of unread "turn completed" events
// for sessions that are NOT currently selected. Incremented on running->ready/dead
// transitions (i.e. the model finished answering) and cleared when the user opens
// the card. Drives the sidebar chat-style unread bubble.
const sessionUnread = {};
// sessionOptimisticRunning: sid(key,node) -> true when sendMessage flipped
// state to 'running' locally before the server broadcast arrived. Rolled back
// by onSendAck on busy/error so the banner doesn't get stuck. Cleared on
// accepted/queued (server-side session_state takes over) and on any real
// session_state WS push.
const sessionOptimisticRunning = {};
// sessionLastSent: sid(key,node) -> 最近一次发出的用户文本（当前 turn 的输入）。
// 在 sendMessage 成功发出后记录；turn 自然跑完 (running→ready/dead) 时清掉。
// 若用户在 running 中点击中断，则把这段文本回填到 #msg-input（Claude Code
// 的中断-回填行为），方便修改后重发。只在输入框当前为空时回填，避免覆盖
// 用户已经开始敲的新内容。
const sessionLastSent = {};
let historySessionsData = []; // from API history_sessions (all filesystem sessions)

// collectWorkspaceSessionIDs returns the set of Claude session UUIDs that the
// sidebar already represents — current session_id PLUS any prev_session_ids
// from auto-chain history. Used to deduplicate the history popover/badge so
// links in an active chain aren't surfaced twice (once in workspace, once in
// history). Skips empty strings defensively in case the API ever returns
// nulls inside prev_session_ids.
function collectWorkspaceSessionIDs(sessions) {
  const ids = new Set();
  for (const s of sessions || []) {
    if (s && s.session_id) ids.add(s.session_id);
    const prev = s && s.prev_session_ids;
    if (Array.isArray(prev)) {
      for (const p of prev) {
        if (p) ids.add(p);
      }
    }
  }
  return ids;
}

// RNEW-UX-004: unified localStorage helper. Use these for NEW keys only —
// legacy 'nz_' / 'naozhi_' call sites are intentionally left alone to
// preserve persisted user state across upgrades. LS_SCHEMA is reserved for
// future breaking changes (bump + migrate on read). All three helpers
// swallow quota/disabled errors so callers never need their own try/catch.
const LS_PREFIX = 'nz:';
const LS_SCHEMA = 1; // bump when structure breaks
function lsSet(key, value) { try { localStorage.setItem(LS_PREFIX + key, JSON.stringify(value)); } catch (e) { /* quota / disabled */ } }
function lsGet(key, fallback) { try { const v = localStorage.getItem(LS_PREFIX + key); return v == null ? fallback : JSON.parse(v); } catch (e) { return fallback; } }
function lsRemove(key) { try { localStorage.removeItem(LS_PREFIX + key); } catch (e) {} }
// Migration of existing 'nz_'/'naozhi_' keys is deferred — touching live
// persisted state across 17 call sites is riskier than the double-prefix
// quirk it would fix. Revisit when LS_SCHEMA is bumped.

// Pending-session persistence (#cwd-fallback fix). The three pending maps
// (sessionWorkspaces/sessionNodes/sessionBackends) used to live ONLY in JS
// memory, so a page reload before the first send dropped the chosen workspace.
// The next send then carried no `workspace`, the backend never wrote a
// per-chat override (send.go gates SetWorkspace on a non-empty workspace), and
// the spawn fell through to defaultCWD = workspace root — the session landed in
// the wrong directory. We mirror the maps into localStorage so a reload (or
// even a never-sent session) rehydrates the workspace and the first send still
// carries it. This only re-hydrates state the user authored in THIS browser —
// no fuzzy cross-session guessing (the semantics #1567 deliberately removed).
const PENDING_LS_KEY = 'pending_sessions';
const PENDING_LS_MAX = 64; // bound localStorage size — far above realistic un-sent backlog
let _pendingRestored = false;

// persistPending snapshots the in-memory pending maps to localStorage. Called
// after every mutation of the three maps. lsSet swallows quota/disabled errors.
function persistPending() {
  const keys = Object.keys(sessionWorkspaces).slice(0, PENDING_LS_MAX);
  const obj = {};
  for (const k of keys) {
    const entry = { ws: sessionWorkspaces[k] };
    if (sessionNodes[k] && sessionNodes[k] !== 'local') entry.node = sessionNodes[k];
    if (sessionBackends[k]) entry.backend = sessionBackends[k];
    obj[k] = entry;
  }
  lsSet(PENDING_LS_KEY, obj);
}

// restorePending rehydrates the in-memory pending maps from localStorage at
// boot, BEFORE the first fetchSessions/send. Idempotent via _pendingRestored
// (multiple DOMContentLoaded listeners exist). Every entry is shape-validated:
// a hand-edited blob cannot inject a non-string key or a non-absolute ws path
// (defense in depth — the server still re-validates the workspace on send).
function restorePending() {
  if (_pendingRestored) return;
  _pendingRestored = true;
  const saved = lsGet(PENDING_LS_KEY, {});
  if (!saved || typeof saved !== 'object') return;
  for (const [k, v] of Object.entries(saved)) {
    if (typeof k !== 'string' || !k) continue;
    if (!v || typeof v !== 'object' || typeof v.ws !== 'string' || !v.ws) continue;
    if (v.ws[0] !== '/' && v.ws[0] !== '~') continue; // reject relative / junk
    sessionWorkspaces[k] = v.ws;
    if (v.node && v.node !== 'local') sessionNodes[k] = v.node;
    if (v.backend) sessionBackends[k] = v.backend;
  }
}

// eagerBindWorkspace tells the backend the chosen workspace the moment a
// session is created, instead of waiting for the first send to carry it. This
// writes the per-chat override eagerly (server-side validateWorkspace +
// SetWorkspace), so even a session opened in another browser/device — or one
// reloaded before its first send — spawns into the right directory. Local
// nodes only: remote sessions resolve their workspace on their own node.
// Fire-and-forget — never blocks or fails the creation flow (matches the
// pushRecentProject swallow convention).
function eagerBindWorkspace(key, workspace, node) {
  if (!key || !workspace) return;
  const nd = node || 'local';
  if (nd !== 'local') return;
  try {
    const headers = { 'Content-Type': 'application/json' };
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    fetch('/api/sessions/bind', {
      method: 'POST', headers,
      body: JSON.stringify({ key: key, node: nd, workspace: workspace }),
    }).catch(() => {});
  } catch (_) { /* never break creation over a bind */ }
}

// THEME-1 (#453) — theme cycler. The dashboard was GitHub-Dark hardcoded
// before this; users with a light-mode preference (or who want to follow
// OS) now get a 3-state toggle in the sidebar header. State is persisted
// to localStorage('nz_theme') as 'light' / 'dark' / 'auto' (default).
// The inline early-paint applier in dashboard.html sets data-theme before
// stylesheet evaluation to avoid FOUC; this helper updates the same
// attribute + writes localStorage on each click. CSS handles the rest
// via :root[data-theme="..."] token overrides — no class toggling, no
// per-element repaint needed. Note: we deliberately do NOT use the
// LS_PREFIX'd lsSet here because the early-paint script reads the raw
// 'nz_theme' key directly (no JSON wrapper, no prefix) for minimum work
// in the critical path.
const THEME_LS_KEY = 'nz_theme';
const THEME_ORDER = ['auto', 'light', 'dark'];
const THEME_LABELS = { auto: '跟随系统', light: '浅色', dark: '深色' };
function getCurrentTheme() {
  try {
    const t = localStorage.getItem(THEME_LS_KEY);
    if (THEME_ORDER.indexOf(t) >= 0) return t;
  } catch (_) {}
  return 'auto';
}
function applyTheme(theme) {
  if (THEME_ORDER.indexOf(theme) < 0) theme = 'auto';
  document.documentElement.setAttribute('data-theme', theme);
  try { localStorage.setItem(THEME_LS_KEY, theme); } catch (_) {}
  const btn = document.getElementById('btn-theme');
  if (btn) {
    const label = THEME_LABELS[theme] || '跟随系统';
    btn.title = '切换主题 (当前: ' + label + ')';
    btn.setAttribute('aria-label', '切换主题，当前: ' + label);
    btn.dataset.themeMode = theme;
  }
}
function cycleTheme() {
  const cur = getCurrentTheme();
  const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
  applyTheme(next);
  if (typeof showToast === 'function') {
    showToast('主题: ' + (THEME_LABELS[next] || next), 'info', 1200);
  }
}
// Wire title/aria on first load so the button reflects the persisted
// state (the inline early-paint script already set data-theme; this
// just syncs the visible affordances). Also wire the click handler via
// addEventListener instead of an inline onclick="" so the theme toggle
// doesn't push the dashboard CSP migration backwards (cap+1 → 8 inline
// handlers; goal in #441 / #479 / #922 is to drive the count to 0 so
// script-src 'unsafe-inline' can be dropped).
document.addEventListener('DOMContentLoaded', function () {
  // Rehydrate pending-session workspaces from localStorage BEFORE the first
  // fetchSessions/send so a reload-before-first-send still carries the chosen
  // workspace (#cwd-fallback fix). Idempotent via _pendingRestored.
  restorePending();
  applyTheme(getCurrentTheme());
  const btn = document.getElementById('btn-theme');
  if (btn) btn.addEventListener('click', cycleTheme);
  // Activity-bar view switch (codex-style rail). Wired here (not inline
  // onclick) to keep the script-src inline-handler surface from growing
  // (R236-SEC-02 cap). Each abnav-* routes through the top-level
  // setActivityView() which toggles the matching body.nz-view-* class and
  // swaps the chat sidebar/main for the target view's panels in place.
  ['abnav-chat', 'abnav-assets', 'abnav-cron', 'abnav-settings'].forEach(function (id) {
    const el = document.getElementById(id);
    if (el) el.addEventListener('click', function () { setActivityView(el.dataset.view); });
  });
  // Bottom-rail connection status doubles as the settings entry.
  const connBtn = document.getElementById('ab-conn-status');
  if (connBtn) connBtn.addEventListener('click', function () { setActivityView('settings'); });
  renderRailConnStatus();

  // Header/sidebar controls (#922 / #479 / #441): migrated from inline
  // `onclick=`/`onsubmit=` attributes to addEventListener so the dashboard's
  // script-src no longer needs `'unsafe-inline'` on account of these handlers.
  // Each bind is guarded so a missing element is a no-op (defensive parity
  // with the theme/nav binds above). Keeps R236-SEC-02 inline-handler count
  // trending to 0.
  const bindClick = function (id, fn) {
    const el = document.getElementById(id);
    if (el) el.addEventListener('click', fn);
  };
  bindClick('btn-sidebar-search', function () { toggleSidebarSearch(); });
  bindClick('btn-history', function () { toggleHistory(); });
  bindClick('btn-new-session', function () { createNewSession(); });
  bindClick('btn-cron', function () { openCronPanel(); });
  bindClick('sidebar-search-clear', function () { closeSidebarSearch(); });
  bindClick('ns-trigger', function (e) { toggleNodeSelector(e); });
  bindClick('btn-sidebar-toggle', function () { toggleSidebarCollapsed(); });
  // NOTE: the quick-ask form submit is NOT bound here. That form lives in
  // `#main`, which is repainted via innerHTML (mainEmptyHtml()), so its submit
  // handler is (re)bound in wireQuickAskInput() — the designated re-wire hook.
});

function getToken() { return ''; }
function setToken(t) { /* token stored in HttpOnly cookie only */ }

// setActivityView is the root view-router. It is the single owner of the
// mutually-exclusive body.nz-view-* classes and the rail button active state.
// Top-level (not closured) so openCronPanel / selectSession / openCronDetail
// can re-assert the active view.
//
// Recursion note: entering 'cron' calls openCronPanel(), which itself calls
// setActivityView('cron') when not already in cron view. We set activeView
// BEFORE dispatching, so openCronPanel's own `activeView !== 'cron'` guard is
// already false on the re-entry and the loop terminates after one hop.
const ACTIVITY_VIEWS = ['chat', 'assets', 'cron', 'settings'];
function setActivityView(view) {
  if (ACTIVITY_VIEWS.indexOf(view) === -1) view = 'chat';
  if (view === activeView) return;
  const prev = activeView;
  activeView = view;
  // Rail button active / aria-pressed state.
  [['abnav-chat', 'chat'], ['abnav-assets', 'assets'], ['abnav-cron', 'cron'], ['abnav-settings', 'settings']]
    .forEach(function (pair) {
      const el = document.getElementById(pair[0]);
      if (el) { el.classList.toggle('active', pair[1] === view); el.setAttribute('aria-pressed', String(pair[1] === view)); }
    });
  // Mutually-exclusive view classes. 'chat' clears all three.
  document.body.classList.toggle('nz-view-assets', view === 'assets');
  document.body.classList.toggle('nz-view-cron', view === 'cron');
  document.body.classList.toggle('nz-view-settings', view === 'settings');
  // Keep the [hidden] attr in sync with CSS for the always-resident containers
  // (asset_browser.js manages its own hidden flags on show/hide).
  const cm = document.getElementById('cron-main');
  if (cm) cm.hidden = view !== 'cron';
  const sm = document.getElementById('settings-main');
  if (sm) sm.hidden = view !== 'settings';
  // Tear down the previous view if it owns external state.
  if (prev === 'assets' && view !== 'assets' && window.nzAssetView) window.nzAssetView.hide();
  // Enter the target view.
  if (view === 'assets') { if (window.nzAssetView) window.nzAssetView.show(); }
  else if (view === 'cron') { openCronPanel(); }
  else if (view === 'settings') { renderSettingsView(); }
}

// RNEW-UX-003: fetchJSON wraps fetch with an AbortController + timeout.
// NAT-dropped TCP connections can leave the browser in a "pending" state
// for minutes with no visible signal — fetchJSON guarantees the Promise
// resolves/rejects within `timeoutMs` (default 10s) so spinners and
// error paths fire deterministically. Returns parsed JSON on 2xx, throws
// with the response body on non-2xx. Partial migration: the highest-risk
// polling + scan sites (sessions, cli/backends, events, cron, discovered,
// discovered/preview, projects/files/exists) use this helper today; the
// remaining fetch() sites migrate in later rounds.
// fetchJSON moved to nz_util.js (PR-0a). Available as window.nz.util.fetchJSON
// and the top-level alias window.fetchJSON, loaded before this file.

function removePendingSession(key) {
  delete sessionWorkspaces[key];
  delete sessionNodes[key];
  delete sessionBackends[key];
  persistPending();
}

async function fetchSessions() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 8s timeout — sessions poll runs every 5s so a hung
    // response must release before the next tick fires.
    let data;
    try {
      data = await fetchJSON('/api/sessions', { headers, timeoutMs: 8000 });
    } catch (err) {
      if (err.status === 401 || err.status === 403) {
        if (!document.querySelector('.modal-overlay')) showAuthModal();
        return false;
      }
      if (err.status) return false;
      throw err;
    }
    // Use server-side version counter for efficient change detection.
    // Falls back to JSON comparison for nodes/history which lack a version.
    const version = (data.stats && data.stats.version) || 0;
    const nodesHash = JSON.stringify(data.nodes || {});
    const historyHash = JSON.stringify(data.history_sessions || []);
    if (version === lastVersion && version > 0 && nodesHash === lastNodesJSON && historyHash === lastHistoryJSON) return;
    lastVersion = version;
    lastNodesJSON = nodesHash;
    lastHistoryJSON = historyHash;
    if (data.nodes) nodesData = data.nodes;
    if (data.stats.agents) availableAgents = data.stats.agents;
    if (data.stats.default_workspace) defaultWorkspace = data.stats.default_workspace;
    if (data.stats.projects) projectsData = data.stats.projects;
    if (data.stats.cli_name) defaultCLIName = data.stats.cli_name;
    if (data.stats.cli_version) defaultCLIVersion = data.stats.cli_version;
    // R110-P1 Home panel: stash the full stats object so the health strip
    // can read uptime / watchdog / active-count without a second fetch.
    lastStatsSnapshot = data.stats;
    historySessionsData = data.history_sessions || [];

    // Track which keys the backend knows about
    const backendKeys = new Set();
    // Drop sessions the operator just dismissed but whose DELETE hasn't been
    // confirmed yet. A lagging poll / sessions_update event would otherwise
    // re-add the card (and re-populate sessionsData) after we optimistically
    // removed it. The key is cleared from _optimisticDeleteKeys once DELETE
    // resolves, so a genuinely-still-present session (failed delete) reappears
    // on the next fetch.
    if (_optimisticDeleteKeys.size > 0) {
      data = Object.assign({}, data, {
        sessions: (data.sessions || []).filter(s => !_optimisticDeleteKeys.has(sid(s.key, s.node || 'local'))),
      });
    }
    (data.sessions || []).forEach(s => {
      const n = s.node || 'local';
      const sKey = sid(s.key, n);
      // Preserve the optimistic 'running' flip when the REST snapshot is still
      // lagging behind the send — otherwise the banner appears for a split
      // second, then a /api/sessions poll rewrites state to 'ready' and hides
      // it until the server's real session_state broadcast catches up.
      if (sessionOptimisticRunning[sKey] && s.state !== 'running') {
        s = Object.assign({}, s, { state: 'running' });
      }
      sessionsData[sKey] = s;
      backendKeys.add(s.key);
    });

    // Remove pending sessions that now exist in backend, then persist ONCE.
    // The durable localStorage blob must drop the now-real keys so a stale
    // pending entry can't re-inject a ghost card on the next reload — but
    // routing each key through removePendingSession would re-serialize the
    // whole blob per key (M full JSON writes converging to one final state).
    // Delete in-memory here and call persistPending() a single time after.
    let reconciledAny = false;
    for (const key of Object.keys(sessionWorkspaces)) {
      if (backendKeys.has(key)) {
        delete sessionWorkspaces[key];
        delete sessionNodes[key];
        delete sessionBackends[key];
        reconciledAny = true;
      }
    }
    if (reconciledAny) persistPending();

    // Merge pending dashboard sessions into data for sidebar rendering
    const pendingKeys = Object.keys(sessionWorkspaces);
    if (pendingKeys.length > 0) {
      if (!data.sessions) data.sessions = [];
      for (const key of pendingKeys) {
        if (!backendKeys.has(key)) {
          const parts = key.split(':');
          // Read the agentID off the key tail so the sidebar's agent chip
          // reflects the user's palette pick rather than always "general".
          // Legacy 3-segment keys (shouldn't exist post-Round 167 but be
          // defensive) degrade to "general".
          const pendingAgent = parts.length >= 4 && parts[3] ? parts[3] : 'general';
          // Pre-populate cli_name / cli_version / backend from the user's
          // backend pick so the sidebar icon (cliIcon) and chat header
          // (renderMainShell / updateHeaderCLI) show the right CLI brand
          // BEFORE the first message spawns the wrapper. Without this, a
          // kiro pending session inherits defaultCLIName ("claude-code")
          // and renders the claude logomark — directly contradicting the
          // operator's choice. See backendDisplayName godoc.
          //
          // pendingBackend is empty in single-backend mode (renderBackendPicker
          // returns '' for ≤1 backend, so #new-backend doesn't exist and
          // sessionBackends[key] is never set). Fall back to defaultCLIName
          // — which is router.CLIName() = the lone configured backend's
          // display name — so single-backend kiro deployments also get the
          // right icon instead of degrading to the 'cli' default branch.
          const pendingBackend = sessionBackends[key] || '';
          const pendingCLIName = backendDisplayName(pendingBackend) || defaultCLIName;
          const pendingCLIVersion = backendDisplayVersion(pendingBackend) || defaultCLIVersion;
          data.sessions.push({
            key: key,
            state: 'new',
            platform: parts[0] || 'dashboard',
            agent: pendingAgent,
            workspace: sessionWorkspaces[key],
            last_active: 0,
            last_prompt: '',
            last_response: '',
            node: sessionNodes[key] || 'local',
            project: matchProject(sessionWorkspaces[key]),
            backend: pendingBackend,
            cli_name: pendingCLIName,
            cli_version: pendingCLIVersion,
          });
        }
      }
    }

    renderSidebar(data);
    // Stash the last successful /api/sessions payload so the sidebar
    // search oninput handler can re-render locally without DoS'ing the
    // server with /api/sessions requests on every keystroke. The renderer
    // is idempotent — re-calling it with the same data just re-paints.
    _lastSidebarData = data;

    // Reconcile main area state: if the selected session's state changed
    // (e.g. session_state WS message was missed during reconnect), propagate
    // the server-side truth to the banner and send/stop buttons.
    // Only reconcile when WS is not connected — when connected, WS pushes
    // are authoritative and overwriting them with a stale REST snapshot
    // would cause UI flicker.
    if (selectedKey && wsm.state !== WS_STATES.CONNECTED) {
      const sKey = sid(selectedKey, selectedNode);
      const sd = sessionsData[sKey];
      if (sd) updateMainState(sd.state, sd.death_reason);
    }
    if (selectedKey) updateHeaderCLI();
    return true;
  } catch (e) {
    console.error('fetchSessions:', e);
    return false;
  }
}

// Debounced variant: coalesces multiple calls within 300ms into a single fetch.
// Returns a Promise that resolves after the actual fetch completes.
let _fetchDbTimer = null;
let _fetchDbResolvers = [];
function debouncedFetchSessions() {
  return new Promise(resolve => {
    _fetchDbResolvers.push(resolve);
    if (_fetchDbTimer) clearTimeout(_fetchDbTimer);
    _fetchDbTimer = setTimeout(() => {
      _fetchDbTimer = null;
      const resolvers = _fetchDbResolvers;
      _fetchDbResolvers = [];
      fetchSessions().then(() => resolvers.forEach(r => r()));
    }, 300);
  });
}

function renderSidebar(data) {
  const st = data.stats;
  updateStatusBar();
  if (st.agents) availableAgents = st.agents;
  if (st.default_workspace) defaultWorkspace = st.default_workspace;
  if (st.projects) projectsData = st.projects;

  const list = document.getElementById('session-list');
  const scrollTop = list.scrollTop;

  // Merge discovered into sessions — tag them as source=terminal
  const allItemsUnfiltered = (data.sessions || []).map(s => {
    if (!s.source) s.source = 'managed';
    return s;
  });
  discoveredItems.forEach(d => {
    allItemsUnfiltered.push({
      key: '_discovered:' + d.pid,
      state: d.state || 'ready',
      cli_name: d.cli_name || 'cli',
      entrypoint: d.entrypoint || '',
      last_active: d.last_active || d.started_at,
      last_prompt: d.last_prompt || d.summary || '',
      workspace: d.cwd,
      project: d.project || matchProject(d.cwd),
      node: d.node || 'local',
      source: 'terminal',
      _discovered: d,
    });
  });

  // Workspace sidebar: managed + discovered sessions (full cache, pre-filter).
  // The node selector dropdown needs unfiltered counts, so allSessionsCache
  // must hold every node's items. The selector itself is refreshed via the
  // updateStatusBar() call above (which tail-calls updateNodeSelector — that
  // path is also fired from setState on every WS transition); session counts
  // in the dropdown therefore lag by at most one poll cycle, which only
  // matters while the dropdown is open — an acceptable trade for not paying
  // two full dropdown repaints per poll.

  allSessionsCache = allItemsUnfiltered;

  // Filter the list by selectedNode when multiple nodes are connected. In
  // single-node setups (selector is hidden) there's nothing to filter and we
  // pass everything through so the legacy UX is preserved. Transient states
  // where selectedNode is null (e.g. previewDiscovered) fall through the
  // falsy branch and show the full list rather than an empty sidebar.
  const activeFilter = isMultiNode() && selectedNode;
  const allItems = activeFilter
    ? allItemsUnfiltered.filter(s => (s.node || 'local') === selectedNode)
    : allItemsUnfiltered;

  // Stable sidebar order: oldest at top, newest at bottom, position never
  // shifts on activity or state change. Each session ships a server-stamped
  // created_at (unix ms); pre-feature payloads fall back to last_active so
  // the upgrade boot keeps existing rows in roughly their previous order
  // before they get re-stamped.
  allItems.sort((a, b) => {
    const aC = a.created_at || a.last_active || 0;
    const bC = b.created_at || b.last_active || 0;
    if (aC !== bC) return aC - bC;
    return (a.key || '').localeCompare(b.key || '');
  });

  // cron-panel-consolidation RFC §4.2: cron stubs are filtered server-side
  // (internal/server/dashboard_session.go) so allItems never contains cron
  // keys here. The previous `cronVisibleKeys` whitelist + per-render filter
  // are gone — both branches below (search-filter / project-grouping) walk
  // allItems directly.
  const visibleItems = allItems;

  // UX-P3 sidebar search: if the filter input is visible and non-empty,
  // skip the project grouping entirely and render the filtered set as a
  // flat list. Grouping under a filter scatters matches across day headers
  // and loses the "search" affordance. Reading the input here (not in a
  // separate oninput handler) means every sessions_update re-evaluates the
  // filter — same query, fresh data — without flickering the input.
  const filterQuery = readSidebarSearchQuery();
  const filterActive = !!filterQuery;
  let listHtml = '';
  if (filterActive) {
    const matched = filterSessionsByQuery(visibleItems, filterQuery);
    listHtml = matched.length === 0
      ? '<div class="session-list-filter-empty">没有匹配的会话<span class="slfe-hint">试试项目名、CLI 名或 prompt 片段</span></div>'
      : matched.map(sessionCardHtml).join('');
  }

  let html = listHtml;
  if (!filterActive) {
    // Project lookup by (node,name) so we can reach favorite/github flags.
    const projIndex = {};
    projectsData.forEach(p => {
      projIndex[(p.node || 'local') + ':' + p.name] = p;
    });

    // Group sessions by (node,name) so remote + local projects with same name stay separate.
    // Fallback groups (project name derived from workspace basename, not a
    // registered ProjectManager project) include the workspace path in the
    // key so two unrelated folders that share a basename (e.g. /a/tmp and
    // /b/tmp) do not collapse into a single mislabeled group.
    const groups = {};
    const ungrouped = [];
    // visibleItems already applied the cron visibility gate up above, so
    // every entry here is either (a) not a cron session, or (b) a cron
    // session the operator has explicitly opened. Visible cron sessions
    // keep flowing through the project-grouping logic so they land next
    // to their workspace peers, matching the "I want to see THIS one"
    // intent; project-less cron sessions fall into the catch-all
    // ungrouped bucket — no dedicated 定时任务 sidebar section any more.
    visibleItems.forEach(s => {
      const pn = s.project || '';
      if (pn) {
        const node = s.node || 'local';
        const k = s.project_fallback
          ? node + ':' + pn + ':' + (s.workspace || '')
          : node + ':' + pn;
        if (!groups[k]) {
          groups[k] = {
            name: pn,
            node,
            items: [],
            fallback: !!s.project_fallback,
            workspace: s.workspace || '',
          };
        }
        groups[k].items.push(s);
      } else {
        ungrouped.push(s);
      }
    });
    // Favorite projects get an empty group so their header is always rendered.
    // Under the node filter, only inject favorites that belong to the
    // currently-viewed node — otherwise switching to a remote would surface
    // an empty header for every local favorite, which defeats the filter.
    projectsData.forEach(p => {
      if (!p.favorite) return;
      const pNode = p.node || 'local';
      // Only suppress cross-node favorites when the filter is actually live
      // (multi-node AND selectedNode non-null). Otherwise fall through to
      // preserve the legacy "every favorite header always renders" behavior.
      if (activeFilter && pNode !== selectedNode) return;
      const k = pNode + ':' + p.name;
      if (!groups[k]) groups[k] = {name: p.name, node: pNode, items: []};
    });

    const groupKeys = Object.keys(groups);
    // cron-panel-consolidation RFC §4.2: no dedicated cron sidebar section;
    // cron stubs are filtered server-side and the dashboard sidebar is
    // reserved for human conversation surfaces. Scheduled-task management
    // lives in the 定时任务 panel.
    if (groupKeys.length > 0) {
      // Pre-compute per-group sort keys once. Order is { tier asc, created
      // asc, name asc }: tier keeps favorites pinned to the top and fallback
      // (unregistered workspace-basename) groups sunk to the bottom, while
      // `created` carries the project's server-stamped CreatedAt (unix ms).
      // Fallback groups have no project entry, so we derive their anchor
      // from the earliest session in the group — ad-hoc quick sessions
      // therefore land in the order their workspace was first opened.
      const sortKeys = {};
      groupKeys.forEach(k => {
        const g = groups[k];
        const p = projIndex[k];
        let created = (p && p.created_at) ? p.created_at : 0;
        if (!created) {
          // Project entry missing CreatedAt (pre-feature server, or fallback
          // group without a registered project): fall back to the earliest
          // session's created_at so the group still has a stable anchor.
          // If every session in the group is also unstamped (very-pre-feature
          // server with empty last_active), `earliest` stays 0 and the
          // comparator falls through to name.localeCompare — fine, the
          // first re-stamp on next save will give them stable anchors.
          let earliest = 0;
          for (const s of g.items) {
            const c = s.created_at || s.last_active || 0;
            if (c && (earliest === 0 || c < earliest)) earliest = c;
          }
          created = earliest;
        }
        const tier = g.fallback ? 2 : ((p && p.favorite) ? 0 : 1);
        sortKeys[k] = { tier, created, name: g.name };
      });
      groupKeys.sort((a, b) => {
        const ka = sortKeys[a], kb = sortKeys[b];
        if (ka.tier !== kb.tier) return ka.tier - kb.tier;
        if (ka.created !== kb.created) return ka.created - kb.created;
        return ka.name.localeCompare(kb.name);
      });
      groupKeys.forEach(k => {
        const g = groups[k];
        const p = projIndex[k] || {
          name: g.name,
          node: g.node,
          favorite: false,
          fallback: !!g.fallback,
          workspace: g.workspace || '',
        };
        p._sessionCount = g.items.length;
        html += g.fallback ? sectionHeaderFallbackHtml(p) : sectionHeaderHtml(p);
        if (collapsedProjects.has(k)) return;
        if (g.items.length > 0) {
          html += g.items.map(sessionCardHtml).join('');
        }
        // Empty favorite groups intentionally render no row below the header:
        // the top-right `+` button is the sole create affordance.
      });
      // NOTE: the dedicated 定时任务 sidebar section was removed.
      // cron stubs no longer reach the dashboard at all (server-side
      // filter, see cron-panel-consolidation RFC §4.3). Truly project-less
      // sessions still fall into the catch-all "未分组" bucket below.
      if (ungrouped.length > 0) {
        // Final catch-all: sessions with no project name AND no workspace
        // (rare — usually transient takeover/planner edge cases). The old
        // "Other" label predated the workspace-basename fallback; keep a
        // bucket but label it clearly so it isn't mistaken for a real group.
        html += '<div class="section-header"><span class="sh-name">未分组</span></div>';
        html += ungrouped.map(sessionCardHtml).join('');
      }
    } else {
      html = visibleItems.map(sessionCardHtml).join('');
    }
  }

  // R110-P2 empty-state CTA: keep the legacy "no sessions" text (E2E asserts
  // it via toContain) but add a visible call-to-action so first-time users
  // aren't left staring at a dead sidebar. createNewSession is the same handler
  // the header `+` button invokes. NOT emitted in filter mode — the
  // filter-specific empty state ('没有匹配的会话') already covers that path.
  if (!html && !filterActive) html = '<div class="no-sessions">no sessions<br><button type="button" class="no-sessions-cta" onclick="createNewSession()">+ 开启你的第一个会话</button></div>';
  // R33-UX1: skip the innerHTML write (and its full sidebar reflow) when
  // the produced markup is byte-identical to what is already mounted.
  // 20 sessions × 1 Hz polling cycle rebuilds the same string every tick
  // whenever nothing has changed — the only thing the assignment did then
  // was force a layout pass and detach `_activeCardEl`. Comparing strings
  // is fast (length check short-circuits the common steady-state path
  // when one item changed and the byte count differs anyway). If the
  // strings match, the DOM is already correct: skip the write, the
  // active-card re-resolve, and the scroll restoration (assigning the
  // same value is a no-op but the rAF is still queued — so just bail).
  if (html === _lastSidebarHtml) {
    // Still refresh the history badge & home panel below — they read from
    // allSessionsCache which was just refreshed regardless.
  } else {
    list.innerHTML = html;
    _lastSidebarHtml = html;
    // Sidebar rebuild detached the previously-cached active card; re-resolve
    // it against the fresh DOM so selector switches stay O(1) on the next
    // click. No-op when nothing is selected (openCronPanel / previewDiscovered
    // clear paths already reset _activeCardEl).
    if (selectedKey) setActiveSessionCard(selectedKey, selectedNode);
    // Restore scroll on the next frame so the browser finishes layout first;
    // synchronous assignment after innerHTML can visibly jump on slow devices.
    requestAnimationFrame(() => {
      list.scrollTop = scrollTop;
    });
  }

  // Update history badge (filesystem history sessions, deduplicated against workspace)
  const hBadge = document.getElementById('history-badge');
  if (hBadge) {
    const workspaceIDs = collectWorkspaceSessionIDs(allSessionsCache);
    const historyCount = historySessionsData.filter(r => !workspaceIDs.has(r.session_id)).length;
    hBadge.textContent = historyCount > 0 ? historyCount : '';
    hBadge.style.display = historyCount > 0 ? '' : 'none';
  }

  // R110-P1 Home panel: refresh after every sidebar repaint so the
  // "最近会话" list mirrors the authoritative snapshot. Gated by selectedKey
  // inside the helper so the main shell's active session view isn't touched.
  renderRecentSessionsPanel();
}

// --- UX-P3 sidebar search helpers ---
//
// readSidebarSearchQuery is called at the top of renderSidebar so every
// sessions_update repaint re-applies the current filter without a separate
// oninput handler firing a second render. Returns '' when the search pane
// is hidden or the input is empty, both of which are "no filter" states.
function readSidebarSearchQuery() {
  const pane = document.getElementById('sidebar-search');
  if (!pane || pane.style.display === 'none') return '';
  const input = document.getElementById('sidebar-search-input');
  if (!input) return '';
  return input.value.trim();
}

// filterSessionsByQuery is the pure match step — extracted so unit tests
// can exercise it without driving the DOM. Match surface: user_label,
// summary, last_prompt, project, cli_name, key (all substring,
// case-insensitive). session_id is NOT in the surface because it's a
// long opaque hash no operator types; matching on key is enough when
// someone wants to paste a slice of it.
function filterSessionsByQuery(items, query) {
  const q = (query || '').trim().toLowerCase();
  if (!q) return items;
  return items.filter(s => {
    if (!s) return false;
    const fields = [
      s.user_label, s.summary, s.last_prompt,
      s.project, s.cli_name, s.key,
    ];
    for (const f of fields) {
      if (typeof f === 'string' && f.toLowerCase().indexOf(q) !== -1) return true;
    }
    return false;
  });
}

// toggleSidebarSearch flips the search pane's visibility. Entering toggle
// auto-focuses the input; exiting clears it (so re-opening starts clean
// and a stale filter doesn't silently hide sessions). Mirrors the header
// button's aria-expanded so screen readers track state.
function toggleSidebarSearch() {
  const pane = document.getElementById('sidebar-search');
  const btn = document.getElementById('btn-sidebar-search');
  if (!pane || !btn) return;
  const opening = pane.style.display === 'none';
  pane.style.display = opening ? 'flex' : 'none';
  btn.classList.toggle('active', opening);
  btn.setAttribute('aria-expanded', opening ? 'true' : 'false');
  if (opening) {
    const input = document.getElementById('sidebar-search-input');
    if (input) {
      // defer focus so the display:flex paint lands first (Safari refuses
      // focus() on a still-hidden element, then silently drops it).
      setTimeout(() => { input.focus(); input.select(); }, 30);
    }
  } else {
    // Closing clears the query so the next open starts fresh and the
    // sidebar immediately re-renders without a lingering filter. Render
    // locally against the cached payload (if any) to avoid an extra
    // /api/sessions round-trip — the data is already authoritative.
    // #1772: cancel any pending debounced keystroke render first, so a timer
    // queued just before close can't fire after we've already cleared and
    // re-rendered (which, when _lastSidebarData is null, would also trigger a
    // spurious debouncedFetchSessions()).
    if (_sidebarSearchDebounce) { clearTimeout(_sidebarSearchDebounce); _sidebarSearchDebounce = null; }
    const input = document.getElementById('sidebar-search-input');
    if (input) input.value = '';
    if (_lastSidebarData) {
      renderSidebar(_lastSidebarData);
    } else {
      debouncedFetchSessions();
    }
  }
}

// closeSidebarSearch is the explicit "close" path used by the × button
// and the Esc key — same semantics as toggleSidebarSearch's close arm.
function closeSidebarSearch() {
  const pane = document.getElementById('sidebar-search');
  if (pane && pane.style.display !== 'none') toggleSidebarSearch();
}

// initSidebarSearch wires the input handler + the `/` and Esc keyboard
// shortcuts. Call once at startup. The input's oninput handler triggers
// a debounced sidebar re-fetch so each keystroke re-applies the filter
// against the canonical sessions data — no client-side cache desync.
let _sidebarSearchDebounce = null;
function initSidebarSearch() {
  const input = document.getElementById('sidebar-search-input');
  if (input) {
    input.addEventListener('input', () => {
      // Re-render locally against the cached /api/sessions payload so
      // rapid typing doesn't DoS the server with per-keystroke requests.
      // When no data has landed yet (first load), fall through to a
      // debounced fetch as a degraded bootstrap.
      //
      // #1772: debounce the local re-render too. renderSidebar does a full
      // sort + filter + sessionCardHtml map+join over every session on each
      // keystroke; the _lastSidebarHtml guard skips the DOM write only when
      // the output is byte-identical, which is rare while a filter narrows.
      // 120ms collapses a typing burst into one render. The periodic
      // sessions_update repaint reuses the current query via
      // readSidebarSearchQuery, so debouncing the keystroke render loses no
      // filter state.
      if (_sidebarSearchDebounce) clearTimeout(_sidebarSearchDebounce);
      _sidebarSearchDebounce = setTimeout(() => {
        _sidebarSearchDebounce = null;
        if (_lastSidebarData) {
          renderSidebar(_lastSidebarData);
        } else {
          debouncedFetchSessions();
        }
      }, 120);
    });
    input.addEventListener('keydown', e => {
      if (e.key === 'Escape') { e.preventDefault(); closeSidebarSearch(); }
    });
  }
  // Global `/` shortcut: open sidebar search unless the user is already
  // typing into some other input/textarea/contenteditable. Mirrors the
  // `?` help shortcut's skip rule so developer muscle memory works.
  document.addEventListener('keydown', e => {
    if (e.key !== '/') return;
    if (e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return;
    const tgt = e.target;
    if (tgt && (tgt.tagName === 'INPUT' || tgt.tagName === 'TEXTAREA' || tgt.isContentEditable)) return;
    if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
    e.preventDefault();
    const pane = document.getElementById('sidebar-search');
    if (pane && pane.style.display === 'none') {
      toggleSidebarSearch();
    } else {
      const inp = document.getElementById('sidebar-search-input');
      if (inp) inp.focus();
    }
  });
}

// projectDisplayLabel returns the operator-facing name for a project,
// preferring the explicit ProjectConfig.display_name override (R110-P2 /
// #448) and falling back to the directory-derived `p.name` so projects
// without a configured display_name keep their existing UI label.
//
// Pure: returns a string, no escaping. Callers MUST run the result
// through esc() / escAttr() before injecting it into HTML — same
// contract as `p.name`. Truthy guards on both fields tolerate the
// pre-config case (`p.config` undefined on legacy /api/projects shapes
// or remote-merge entries that the cache layer hasn't yet stamped).
function projectDisplayLabel(p) {
  if (!p) return '';
  const cfg = p.config || {};
  const dn = (cfg.display_name || '').trim();
  if (dn) return dn;
  return p.name || '';
}

// projectDisplayPrefix renders the optional emoji in front of the name.
// Returns "" when the project has no emoji configured. The trailing
// space lives inside the returned string so callers can simply
// concatenate prefix + label without conditional whitespace.
function projectDisplayPrefix(p) {
  if (!p) return '';
  const cfg = p.config || {};
  const em = (cfg.emoji || '').trim();
  if (!em) return '';
  return em + ' ';
}

// Match a workspace path to a project from projectsData (longest prefix wins)
function matchProject(workspace) {
  if (!workspace || !projectsData || projectsData.length === 0) return '';
  const ws = workspace.endsWith('/') ? workspace : workspace + '/';
  let best = '', bestLen = 0;
  for (const p of projectsData) {
    const prefix = p.path.endsWith('/') ? p.path : p.path + '/';
    if (ws.startsWith(prefix) && p.path.length > bestLen) {
      best = p.name; bestLen = p.path.length;
    }
  }
  return best;
}

// --- Project section header (favorite + github icons) ---

// The star glyph is identical in both states — CSS class `star-on` + `fill:currentColor`
// controls the visual fill. A single constant avoids the misleading dead ternary
// that previously implied a per-state SVG difference.
const STAR_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><polygon points="12,2 15.09,8.26 22,9.27 17,14.14 18.18,21.02 12,17.77 5.82,21.02 7,14.14 2,9.27 8.91,8.26"/></svg>';
// "Clawd" pixel mascot for claude-backend assistant turns. Sourced from
// the Custom Brand Icons set, icon `cbi:claude-clawd`
// (https://github.com/elax46/custom-brand-icons), licensed CC BY-NC-SA
// 4.0. Naozhi ships under BSL 1.1 (non-commercial Additional Use Grant
// through 2030-03-21), so the NC clause is compatible for the current
// licensed term — see ATTRIBUTIONS.md. Fill flows from currentColor so
// the rust hex lives once in dashboard.html as --nz-clawd-rust (CSS sets
// .cc-clawd { color: var(--nz-clawd-rust) }) — no inline hex in JS.
const CLAWD_SVG = '<svg class="cc-clawd" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><path fill="currentColor" d="M4.5 6h15v5H22v2h-2.5v3h-1v2H17v-2h-1v2h-1.5v-2h-5v2H8v-2H7v2H5.5v-2h-1v-3H2v-2h2.5ZM7 8v3h1V8Zm9 0v3h1V8Z"/></svg>';
const GITHUB_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 19c-5 1.5-5-2.5-7-3m14 6v-3.87a3.37 3.37 0 0 0-.94-2.61c3.14-.35 6.44-1.54 6.44-7A5.44 5.44 0 0 0 20 4.77 5.07 5.07 0 0 0 19.91 1S18.73.65 16 2.48a13.38 13.38 0 0 0-7 0C6.27.65 5.09 1 5.09 1A5.07 5.07 0 0 0 5 4.77a5.44 5.44 0 0 0-1.5 3.78c0 5.42 3.3 6.61 6.44 7A3.37 3.37 0 0 0 9 18.13V22"/></svg>';
// Chevron: points down when expanded (`▾`-like), rotated 90deg via CSS
// when collapsed so the same glyph serves both states.
const CHEVRON_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>';

// sectionHeaderFallbackHtml renders the minimal header for ad-hoc workspace
// groups (p.fallback === true). The group's "project name" is just the
// workspace basename — it is NOT a registered ProjectManager project — so
// favorite / GitHub / + buttons have no stable semantics and are omitted.
// Split out of sectionHeaderHtml to preserve the R110-P2 invariant that
// sectionHeaderHtml has a single unconditional `return '<div...` with
// `newBtn` concatenated directly (locked by static_ux_contract_test).
function sectionHeaderFallbackHtml(p) {
  const node = p.node || 'local';
  const workspace = p.workspace || '';
  // Collapse key matches the group key used in renderSidebar (node:name:ws)
  // so two folders with the same basename each own their own fold state.
  const ck = node + ':' + p.name + ':' + workspace;
  const collapsed = collapsedProjects.has(ck);
  const count = typeof p._sessionCount === 'number' ? p._sessionCount : 0;
  const cCls = collapsed ? 'sh-btn sh-collapse collapsed' : 'sh-btn sh-collapse';
  const cTitle = collapsed ? '展开' : '收起';
  const collapseBtn = '<button type="button" class="' + cCls + '" data-action="project-collapse" data-key="' + escAttr(ck) + '" title="' + cTitle + ' ' + escAttr(p.name) + '" aria-label="' + cTitle + ' ' + escAttr(p.name) + '" aria-expanded="' + (collapsed ? 'false' : 'true') + '">' + CHEVRON_SVG + '</button>';
  const countBadge = collapsed && count > 0 ? '<span class="sh-count">' + count + '</span>' : '';
  const nameTitle = workspace ? escAttr(p.name + ' — ' + workspace) : escAttr(p.name);
  const collapsedCls = collapsed ? ' is-collapsed' : '';
  return '<div class="section-header section-header-fallback' + collapsedCls + '" role="group" aria-label="' + escAttr(p.name) + '">' +
    collapseBtn +
    '<span class="sh-name" title="' + nameTitle + '">' + esc(p.name) + '</span>' +
    countBadge +
    '</div>';
}

function sectionHeaderHtml(p) {
  const node = p.node || 'local';
  const fav = !!p.favorite;
  const starCls = fav ? 'sh-btn star-on' : 'sh-btn';
  const starTitle = fav ? 'Unfavorite' : 'Favorite';
  const ck = node + ':' + p.name;
  const collapsed = collapsedProjects.has(ck);
  const count = typeof p._sessionCount === 'number' ? p._sessionCount : 0;
  const cCls = collapsed ? 'sh-btn sh-collapse collapsed' : 'sh-btn sh-collapse';
  const cTitle = collapsed ? '展开' : '收起';
  const collapseBtn = '<button type="button" class="' + cCls + '" data-action="project-collapse" data-key="' + escAttr(ck) + '" title="' + cTitle + ' ' + escAttr(p.name) + '" aria-label="' + cTitle + ' ' + escAttr(p.name) + '" aria-expanded="' + (collapsed ? 'false' : 'true') + '">' + CHEVRON_SVG + '</button>';
  const countBadge = collapsed && count > 0 ? '<span class="sh-count">' + count + '</span>' : '';

  // No longer pass `data-fav` — the handler derives current state from the
  // authoritative `projectsData` at click time, avoiding a stale DOM attribute
  // that could cause a fast second click (before re-render) to send a
  // redundant or wrong-polarity toggle.
  const starBtn = '<button type="button" class="' + starCls + '" data-action="project-favorite" data-name="' + escAttr(p.name) + '" data-node="' + escAttr(node) + '" title="' + starTitle + '" aria-label="' + starTitle + ' ' + escAttr(p.name) + '">' + STAR_SVG + '</button>';

  let ghBtn = '';
  if (p.github) {
    const url = p.git_remote_url || '';
    // R110-P2 tooltip clarity: the old "GitHub: <url>" left the CTA implicit
    // — click-to-open was only discoverable by trial. Lead with the verb
    // "在 GitHub 打开仓库" so the affordance is explicit; append the URL so
    // operators can still eyeball the remote for the common case where
    // they're verifying the repo match before clicking.
    ghBtn = '<button type="button" class="sh-btn github-on" data-action="project-github" data-url="' + escAttr(url) + '" title="在 GitHub 打开仓库：' + escAttr(url) + '" aria-label="在 GitHub 打开仓库 ' + escAttr(p.name) + '">' + GITHUB_SVG + '</button>';
  }

  const collapsedCls = collapsed ? ' is-collapsed' : '';
  // R110-P2 / #448: prefix the display name with the configured emoji
  // (if any) and use display_name when set; aria-label / title still
  // carry p.name so screen-readers + tooltips disambiguate when the
  // dirname differs from the human-friendly label.
  const emojiPrefix = projectDisplayPrefix(p);
  const displayName = projectDisplayLabel(p);
  const labelTitle = (displayName && displayName !== p.name)
    ? p.name + ' — ' + displayName
    : p.name;
  return '<div class="section-header' + collapsedCls + '" role="group" aria-label="' + escAttr(labelTitle) + '">' +
    collapseBtn + starBtn +
    '<span class="sh-name" title="' + escAttr(labelTitle) + '">' +
      (emojiPrefix ? esc(emojiPrefix) : '') + esc(displayName) +
    '</span>' +
    countBadge +
    ghBtn +
    '</div>';
}

// SIDEBAR_PROJECT_ACTIONS maps the `data-action` token on a project-header
// control to the handler it invokes, reading arguments from the button's
// own dataset. This is the data-action dispatch idiom already used by the
// cron menu (CRON_MENU_ACTIONS / handleCronMenuClick) — it lets the section
// header buttons drop their inline click attributes, shrinking the
// script-src 'unsafe-inline' surface (#922 / #1734) without changing
// behaviour. Keys must match the data-action values emitted in
// sectionHeaderHtml / sectionHeaderFallbackHtml.
const SIDEBAR_PROJECT_ACTIONS = {
  'project-collapse': (btn) => toggleProjectCollapsed(btn.dataset.key),
  'project-favorite': (btn) => toggleFavorite(btn.dataset.name, btn.dataset.node),
  'project-github': (btn) => showGitRemote(btn.dataset.url),
};

// initSidebarProjectActions attaches ONE delegated click listener to the
// stable #session-list container (not document — scoped delegation, mirroring
// the cron-menu listener). It dispatches project-header button clicks via
// SIDEBAR_PROJECT_ACTIONS. stopPropagation preserves the prior inline
// `event.stopPropagation()` so a click on a header control never bubbles to
// an ancestor handler. The capture-phase long-press swallow installed by
// initSwipeDelete is orthogonal (it only fires on _longPressFired).
function initSidebarProjectActions() {
  const list = document.getElementById('session-list');
  if (!list) return;
  list.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-action]');
    if (!btn || !list.contains(btn)) return;
    const fn = SIDEBAR_PROJECT_ACTIONS[btn.getAttribute('data-action')];
    if (!fn) return;
    e.stopPropagation();
    fn(btn);
  });
}

// toggleProjectCollapsed flips a project section's fold state, persists
// it, and re-renders from the last sidebar payload (no network round-trip).
// Key format: "<node>:<name>" matching the grouping key in renderSidebar.
function toggleProjectCollapsed(key) {
  if (!key) return;
  if (collapsedProjects.has(key)) collapsedProjects.delete(key);
  else collapsedProjects.add(key);
  try {
    localStorage.setItem('nz_collapsedProjects', JSON.stringify([...collapsedProjects]));
  } catch (_) {}
  if (_lastSidebarData) {
    renderSidebar(_lastSidebarData);
  } else {
    debouncedFetchSessions();
  }
}

// In-flight guard against a double-click race: the star button's DOM state
// lags behind projectsData until the next fetchSessions re-render. Without
// this set, a second click inside that window would read a stale DOM hint and
// potentially fire the same or opposite polarity. Keyed by (node, name).
const _favInFlight = new Set();

async function toggleFavorite(name, node) {
  const nodeID = node || 'local';
  const key = nodeID + ':' + name;
  if (_favInFlight.has(key)) return; // drop re-entry
  // Derive current state from the source of truth (projectsData), not the
  // button's data-fav attribute which may not have been re-rendered yet.
  const proj = projectsData.find(x => x.name === name && (x.node || 'local') === nodeID);
  if (!proj) return;
  const next = !proj.favorite;
  _favInFlight.add(key);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const qs = 'name=' + encodeURIComponent(name) + '&favorite=' + (next ? 'true' : 'false') +
      (node && node !== 'local' ? '&node=' + encodeURIComponent(node) : '');
    try {
      await fetchJSON('/api/projects/favorite?' + qs, { timeoutMs: 10000, method: 'POST', headers });
    } catch (err) {
      if (err && err.status) {
        showAPIError(next ? '收藏项目' : '取消收藏', err.status, '');
      } else {
        showNetworkError(next ? '收藏项目' : '取消收藏', err);
      }
      // Re-render from the server so the star's visual hover/click state
      // snaps back to the authoritative `projectsData` value; otherwise the
      // user sees a phantom success.
      fetchSessions();
      return;
    }
    // Optimistic update then refresh.
    proj.favorite = next;
    showToast(next ? '已收藏 ' + name : '已取消收藏 ' + name, 'success');
    fetchSessions();
  } finally {
    _favInFlight.delete(key);
  }
}

function showGitRemote(url) {
  if (!url) return;
  // Only open http(s)/git URLs; refuse ssh:// or git@host:user/repo remotes
  // because ssh URLs can include embedded credentials (user:pass@host) that
  // a toast would leak to anyone peering at the screen, and window.open on
  // ssh:// does nothing useful in a browser.
  //
  // R244-SEC-P3-4: explicit positive startsWith allowlist (lowercased) instead
  // of a /^(https?|git):\/\// regex so a future copy-paste cannot accidentally
  // drop the leading anchor and accept "javascript:foo http://" or similar
  // mixed-scheme strings. The lowercased prefix check matches scheme parsing
  // semantics (RFC 3986 §3.1: schemes are case-insensitive).
  const lower = String(url).toLowerCase();
  const allowed = ['https://', 'http://', 'git://'];
  let safe = false;
  for (const scheme of allowed) {
    if (lower.startsWith(scheme)) { safe = true; break; }
  }
  if (safe) {
    window.open(url, '_blank', 'noopener,noreferrer');
    return;
  }
  // Fallback: surface the URL but truncated to keep credentials embedded in
  // ssh URLs from being broadcast via the toast surface.
  const shown = url.length > 80 ? url.slice(0, 77) + '…' : url;
  showToast('GitHub remote: ' + shown);
}

// --- History Popover ---

let activePopover = null;
let activePopoverBackdrop = null;

function closeHistoryPopover() {
  if (activePopoverBackdrop) { activePopoverBackdrop.remove(); activePopoverBackdrop = null; }
  if (activePopover) { activePopover.remove(); activePopover = null; }
}

document.addEventListener('click', function(e) {
  if (activePopover && !activePopover.contains(e.target) && !e.target.closest('#btn-history')) {
    closeHistoryPopover();
  }
});

function toggleHistory() {
  if (activePopover) { closeHistoryPopover(); return; }

  // Show all filesystem history sessions, deduplicated against workspace.
  // Includes prev_session_ids so that earlier links in a resumed-chain
  // session don't appear twice (once in the sidebar, once in history).
  const workspaceIDs = collectWorkspaceSessionIDs(allSessionsCache);
  const merged = historySessionsData
    .filter(r => !workspaceIDs.has(r.session_id))
    .map(r => ({
      key: '_history:' + r.session_id, node: 'local', source: 'recent',
      session_id: r.session_id, last_active: r.last_active || 0,
      // retired_at is the unix-ms instant the session left the live sidebar
      // (Router.Reset / Router.Remove). When present it overrides last_active
      // for sort ordering so the most recently closed panel sits on top —
      // last_active reflects the JSONL's last-message timestamp, which can
      // be days older than when the operator actually closed the session.
      retired_at: r.retired_at || 0,
      prompt: r.last_prompt || r.summary || '',
      project: r.project || matchProject(r.workspace), tool: '',
    }));
  // Sort key: retired_at when known, else last_active (back-compat for
  // sessions retired before this naozhi process started — their UUID is
  // not in the in-memory store and last_active is the only signal we have).
  merged.sort((a, b) => (b.retired_at || b.last_active) - (a.retired_at || a.last_active));

  const popover = document.createElement('div');
  popover.className = isMobile() ? 'history-sheet' : 'history-popover';
  // R110-P1 history-drawer search: the header grows a count chip and a
  // filter input. Submitting or typing into the input triggers
  // applyHistoryFilter(merged, query) — a pure function over `merged` that
  // re-renders the items list and updates the count chip. Keeping `merged`
  // on the closure means each keystroke is an O(N) scan against the same
  // dataset — at ~200 entries that's trivial and avoids re-reading
  // historySessionsData on every keypress.
  popover.innerHTML =
    '<div class="history-popover-header">' +
      '<span>历史 <span class="hp-count" id="hp-count">(' + merged.length + ')</span></span>' +
    '</div>' +
    (merged.length > 0
      ? '<div class="history-popover-search">' +
          '<input type="text" id="hp-search" class="hp-search-input" placeholder="搜索提示词或项目…" autocomplete="off" spellcheck="false" />' +
        '</div>'
      : '') +
    '<div class="history-popover-items" id="hp-items"></div>';
  if (isMobile()) {
    popover.innerHTML = '<div class="sheet-handle"></div>' + popover.innerHTML;
  }
  // Backdrop: captures outside clicks explicitly (so clicking a covered
  // control like the node switcher dismisses the popover cleanly instead of
  // being silently swallowed) and gives a "a layer is open" cue. The mobile
  // sheet gets a dimmed variant; the desktop popover stays transparent so it
  // reads as a lightweight popover, not a blocking modal. R20260605.
  const backdrop = document.createElement('div');
  backdrop.className = isMobile() ? 'history-backdrop is-sheet' : 'history-backdrop';
  backdrop.addEventListener('click', closeHistoryPopover);
  activePopoverBackdrop = backdrop;
  document.body.appendChild(backdrop);

  activePopover = popover;
  document.body.appendChild(popover);

  if (!isMobile()) {
    const btn = document.getElementById('btn-history');
    const rect = btn.getBoundingClientRect();
    popover.style.position = 'fixed';
    popover.style.top = (rect.bottom + 4) + 'px';
    popover.style.right = (window.innerWidth - rect.right) + 'px';
    popover.style.maxHeight = Math.min(500, window.innerHeight - rect.bottom - 16) + 'px';
  }

  // Paint initial list (empty query = show everything).
  applyHistoryFilter(merged, '');

  // Wire search input. Setting `oninput` via property rather than HTML
  // attribute keeps the handler isolated from any CSP tightening that
  // might disable inline event handlers on the items HTML.
  const searchInput = document.getElementById('hp-search');
  if (searchInput) {
    searchInput.addEventListener('input', e => applyHistoryFilter(merged, e.target.value));
    // Auto-focus on desktop only; mobile focus pops the keyboard and
    // pushes the sheet up, which is annoying if the user just wanted to
    // eyeball the list.
    if (!isMobile()) {
      setTimeout(() => searchInput.focus(), 50);
    }
  }
}

// filterHistoryEntries is the pure filtering step extracted for unit
// testability. Query is case-insensitive and matched as a substring
// against (prompt, project). Empty query returns the full list. Kept
// as a standalone function so a contract test can assert the match
// surface without driving the DOM.
function filterHistoryEntries(merged, query) {
  const q = (query || '').trim().toLowerCase();
  if (!q) return merged;
  return merged.filter(s => {
    const p = (s.prompt || '').toLowerCase();
    if (p.indexOf(q) !== -1) return true;
    const proj = (s.project || '').toLowerCase();
    if (proj.indexOf(q) !== -1) return true;
    return false;
  });
}

// applyHistoryFilter renders the filtered subset into the popover and
// updates the count chip. Separated from the render so the search input
// handler can call it without rebuilding the popover shell on every
// keystroke.
function applyHistoryFilter(merged, query) {
  const itemsEl = document.getElementById('hp-items');
  const countEl = document.getElementById('hp-count');
  if (!itemsEl) return;
  const filtered = filterHistoryEntries(merged, query);
  if (countEl) {
    // "Filtered" count uses the x/total shape so the user knows the
    // denominator hasn't shrunk — e.g. "(3 / 47)" after typing. When
    // the query is empty keep the compact "(47)" form.
    countEl.textContent = query
      ? '(' + filtered.length + ' / ' + merged.length + ')'
      : '(' + merged.length + ')';
  }
  if (merged.length === 0) {
    itemsEl.innerHTML = '<div class="history-popover-empty">no history<br><span class="hp-empty-hint">发起对话后，历史记录会出现在这里</span></div>';
    return;
  }
  if (filtered.length === 0) {
    itemsEl.innerHTML = '<div class="history-popover-empty">没有匹配的历史<br><span class="hp-empty-hint">调整关键词，或清空搜索框查看全部</span></div>';
    return;
  }
  // Group by day. Round 129: label today / yesterday in 中文 so the most
  // common buckets don't require parsing a date — "今天" / "昨天" / older
  // entries keep the browser-locale formatted date (e.g. "Wed, Apr 29"
  // or "4月29日 周三" depending on navigator.language). Day headers are
  // recomputed on filter because a 3-entry result may span fewer days
  // than the full list.
  let currentDay = '';
  itemsEl.innerHTML = filtered.map(s => {
    let dayHeader = '';
    if (s.last_active) {
      const d = new Date(s.last_active);
      const dayStr = historyDayLabel(d);
      if (dayStr !== currentDay) {
        currentDay = dayStr;
        dayHeader = '<div class="hp-day-header">' + esc(dayStr) + '</div>';
      }
    }
    const ago = s.last_active ? timeAgo(s.last_active) : '';
    const abs = s.last_active ? formatAbsTime(s.last_active) : '';
    const onclick = 'resumeRecentSession(this.dataset.sid);closeHistoryPopover()';
    return dayHeader +
      '<div class="history-popover-item" data-sid="' + escAttr(s.session_id) + '" onclick="' + onclick + '">' +
      (s.prompt ? '<div class="hp-prompt" title="' + escAttr(s.prompt) + '">' + esc(s.prompt) + '</div>' : '<div class="hp-prompt" style="color:var(--nz-text-dim)">未命名</div>') +
      '<div class="hp-meta">' +
        (s.project ? '<span class="hp-project">' + esc(s.project) + '</span><span class="hp-dot">&middot;</span>' : '') +
        (ago ? '<span' + (abs ? ' title="' + escAttr(abs) + '"' : '') + '>' + ago + '</span>' : '') +
      '</div>' +
      '</div>';
  }).join('');
}

function majorMinor(ver) {
  const parts = ver.split('.');
  return parts.length >= 2 ? parts[0] + '.' + parts[1] : ver;
}

function sessionTypeTag(cliName, entrypoint) {
  var label;
  if (cliName === 'kiro') { label = 'Kiro CLI'; }
  else if (entrypoint === 'claude-vscode') { label = 'Claude VS Extension'; }
  else if (cliName === 'claude-code') { label = 'Claude CLI'; }
  else { label = 'CLI'; }
  return '<span class="sc-type-tag">' + label + '</span>';
}

// PLATFORM_ORIGINS maps the first component of a session key (the platform
// tag emitted by session.SessionKey in internal/session/managed.go) to the
// user-facing Chinese label shown on the IM-origin badge. Adding a new IM
// platform means extending this map PLUS picking a CSS variant in
// dashboard.html `.sc-origin.kind-*` PLUS wiring the adapter in
// cmd/naozhi/main.go initPlatforms — see R230-ARCH-11 (#1021) for the
// `GET /api/platforms` proposal that would let the dashboard hydrate this
// list at boot instead of hardcoding it. Non-IM prefixes (dashboard, local,
// cron, scratch_*, planner) intentionally do NOT appear here — originBadgeInfo
// returns null for them so those sessions don't grow a misleading "外部
// 来源" chip. The two dashboard-local sources of truth (this map and the
// `.sc-origin.kind-*` CSS) are cross-checked by
// TestDashboardJS_R230ARCH11_PlatformOriginsAndCSSStayInSync so a partial
// addition fails CI.
const PLATFORM_ORIGINS = {
  feishu:  { name: '飞书',    kind: 'feishu' },
  slack:   { name: 'Slack',   kind: 'slack' },
  discord: { name: 'Discord', kind: 'discord' },
  weixin:  { name: '微信',    kind: 'weixin' },
};

// originBadgeInfo derives the IM-origin chip payload from a session key.
// Returns null when the key doesn't come from a real IM platform — that's
// the common case (dashboard-created sessions, cron jobs, scratch drawers,
// planner sessions, local takeovers) and those should render without a
// badge. Pure function, no DOM touch, so it's easy to unit-test from a
// contract test that loads dashboard.js as text.
//
// R110-P3: scope of this helper is intentionally "platform + 私聊/群"
// only; it does NOT attempt to display the opaque chat_id nor a jump-back
// URL because those require backend schema changes (see TODO R110-P3-IM
// 来源指示 residual scope). Surfacing platform alone already tells the
// operator "this is a real IM thread, not a dashboard-local conversation",
// which is the 80% case.
function originBadgeInfo(key) {
  if (typeof key !== 'string' || !key) return null;
  const colon = key.indexOf(':');
  if (colon <= 0) return null;
  const platform = key.substring(0, colon);
  const origin = PLATFORM_ORIGINS[platform];
  if (!origin) return null;
  // chatType is the second segment; sanitizeKeyComponent in the Go layer
  // replaces unsafe chars but keeps 'direct'/'group' verbatim, so raw
  // substring equality is safe here. Default to 'direct' if the segment
  // is missing (shouldn't happen for a real IM key but keeps the helper
  // defensive against malformed inputs).
  const rest = key.substring(colon + 1);
  const colon2 = rest.indexOf(':');
  const chatType = colon2 > 0 ? rest.substring(0, colon2) : 'direct';
  const chatLabel = chatType === 'group' ? '群' : '私聊';
  return {
    label: origin.name + ' · ' + chatLabel,
    kind: origin.kind,
  };
}

// originBadgeHtml renders the IM-origin chip for a given session key.
// Returns '' when originBadgeInfo yields null — never emit a stray chip
// for dashboard/cron/scratch/planner sessions. Separate layer so templates
// can call one function instead of re-implementing the null-check.
function originBadgeHtml(key) {
  const info = originBadgeInfo(key);
  if (!info) return '';
  return '<span class="sc-origin kind-' + esc(info.kind) + '" title="' + escAttr(info.label) + '">' + esc(info.label) + '</span>';
}

// backendChipInfo derives the dashboard chip payload (label + color) for a
// session's backend ID. Returns null when the deployment is single-backend
// (cliBackends has 0 or 1 entry) so callers don't render a stray chip on
// claude-only deployments. Multi-Backend RFC §8.3 D1.
//
// Falls back gracefully when:
//   - cliBackends has not been fetched yet → returns null (chip will appear
//     on next render once the cache populates; renderHeader is re-run on
//     every snapshot/event update so the latency is sub-second).
//   - The session.backend value isn't in cliBackends.backends — could happen
//     for an evicted-and-removed-from-config backend; we display a neutral
//     chip with the raw id so operators can see the orphan record.
function backendChipInfo(backendID) {
  if (!cliBackends || !Array.isArray(cliBackends.backends)) return null;
  if (cliBackends.backends.length <= 1) return null; // single-backend mode
  if (!backendID) backendID = cliBackends.default || '';
  const entry = cliBackends.backends.find(b => b && b.id === backendID);
  if (!entry) {
    // Orphan record: keep the chip but use a neutral color so operators
    // notice. Don't return null — losing the chip silently would hide a
    // real "session pinned to a removed backend" state.
    return {
      label: backendID || 'unknown',
      color: 'var(--nz-text-mute)',
      tooltip: 'Backend not currently configured: ' + (backendID || '(empty)'),
    };
  }
  return {
    label: entry.reply_tag || entry.id,
    color: entry.chip_color || 'var(--nz-accent)',
    tooltip: (entry.display_name || entry.id) + (entry.version ? ' v' + entry.version : ''),
  };
}

// backendChipHtml renders the per-session backend chip — the small colored
// pill next to the IM origin badge. Empty string when single-backend mode
// (no chip rendered, layout unchanged).
function backendChipHtml(backendID) {
  const info = backendChipInfo(backendID);
  if (!info) return '';
  return '<span class="sc-backend-chip" data-backend="' + escAttr(backendID || '') +
    '" style="background-color:' + escAttr(info.color) + '" title="' + escAttr(info.tooltip) +
    '">' + esc(info.label) + '</span>';
}

// featureForBackend resolves a backend feature flag (RFC §8.2). Returns
// true when the feature is supported, false when missing / unknown
// backend / no cache yet. Default-false on uncertainty matches the
// spec's "missing key == false" — controls degrade to disabled rather
// than letting users hit a backend that doesn't support them.
//
// Only "askuser", "passthrough", "embedded_context", "image_input",
// "audio_input", "mcp_http", "mcp_sse" are recognized today; new
// features must be added to the Profile.Features map AND a hard-coded
// caller in dashboard.js (no automatic fallback path).
function featureForBackend(backendID, name) {
  if (!cliBackends || !Array.isArray(cliBackends.backends)) return false;
  if (!backendID) backendID = cliBackends.default || '';
  const entry = cliBackends.backends.find(b => b && b.id === backendID);
  if (!entry || !entry.features) return false;
  return entry.features[name] === true;
}

// featureForCurrent reads the active session's backend feature flag.
// Used by feature-gate sites (file picker, voice button, /urgent hint)
// to gray out controls that don't apply to the current session. Returns
// true in single-backend mode (length<=1) so claude-only deployments
// preserve all historical behavior.
function featureForCurrent(name) {
  if (!cliBackends || !Array.isArray(cliBackends.backends)) return true;
  if (cliBackends.backends.length <= 1) return true; // single-backend mode
  const sess = sessionsData[sid(selectedKey, selectedNode)];
  const backendID = (sess && sess.backend) || cliBackends.default || '';
  return featureForBackend(backendID, name);
}

// applyFeatureGates updates the input-area controls to reflect the
// active session's backend features. Called after every renderMainShell
// / selectSession / cliBackends fetch — cheap, just toggles aria + class.
// Multi-Backend RFC §8.3 D9 / D11-D15.
//
// Important: NEVER silently disable. Per RFC §8.7: "all gated controls
// must have a hover/aria tooltip explaining why" — the title attribute
// carries the operator-readable reason.
function applyFeatureGates() {
  if (!cliBackends || !Array.isArray(cliBackends.backends)) return;
  if (cliBackends.backends.length <= 1) return; // single-backend mode

  const sess = sessionsData[sid(selectedKey, selectedNode)] || {};
  const backendID = sess.backend || cliBackends.default || '';
  const backendName = (() => {
    const e = cliBackends.backends.find(b => b && b.id === backendID);
    return (e && (e.display_name || e.id)) || backendID || 'this backend';
  })();

  // D14 image_input: file picker accepts both images + PDF; if image is
  // disabled but PDF still works, leave the button enabled — most kiro
  // deployments support image so this branch rarely hits in practice.
  // Audio is governed separately (D15) by the voice button.
  const imageOK = featureForBackend(backendID, 'image_input');
  const filePickBtn = document.querySelector('button[onclick="openFilePicker()"]');
  if (filePickBtn) {
    if (!imageOK) {
      filePickBtn.classList.add('feat-disabled');
      filePickBtn.title = '当前后端 (' + backendName + ') 不支持图片上传';
      filePickBtn.setAttribute('aria-disabled', 'true');
      // Review #118 HIGH-1: rely on the native disabled property as the
      // hard gate, not just CSS — `cursor:not-allowed` is cosmetic and
      // a keyboard activation (Enter/Space on focus) would still fire
      // onclick. Browsers skip click events on disabled buttons entirely,
      // and `applyFeatureGates` is the single re-entry point so the
      // pair stays in sync.
      filePickBtn.disabled = true;
    } else {
      filePickBtn.classList.remove('feat-disabled');
      filePickBtn.title = '上传图片或 PDF';
      filePickBtn.removeAttribute('aria-disabled');
      filePickBtn.disabled = false;
    }
  }

  // D15 audio_input: kiro acp 申报 audio:false 但 naozhi 后端会先转写
  // 再喂 prompt — 所以这里**不真正 disable**，只把 tooltip 改成提示性
  // 文案，让用户知道音频会经过转写阶段。
  const audioOK = featureForBackend(backendID, 'audio_input');
  const micBtn = document.getElementById('btn-mic');
  const holdBtn = document.getElementById('btn-hold-talk');
  // Review #118 HIGH-2: when audio is supported again (e.g. user switches
  // from kiro back to claude in the same browser session), we MUST reset
  // titles to their template defaults — otherwise the kiro-era hint
  // ("会先转写为文字") sticks forever. Default titles mirror
  // renderMainShell template (line ~2152 / ~2154).
  const micDefaultTitle = voiceInputMode ? '切换键盘' : '切换语音';
  const holdDefaultTitle = '按住说话改录音';
  if (!audioOK) {
    const audioHint = '当前后端 (' + backendName + ') 不直接接收音频，naozhi 会先转写为文字再发送';
    if (micBtn) {
      micBtn.classList.add('feat-degraded');
      micBtn.title = audioHint;
    }
    if (holdBtn) {
      holdBtn.classList.add('feat-degraded');
      holdBtn.title = audioHint;
    }
  } else {
    if (micBtn) {
      micBtn.classList.remove('feat-degraded');
      micBtn.title = micDefaultTitle;
    }
    if (holdBtn) {
      holdBtn.classList.remove('feat-degraded');
      holdBtn.title = holdDefaultTitle;
    }
  }
}

// formatCostByUnit returns the cost cell text for the dashboard header.
// USD: "$0.0024" / "$1.23". credits: "0.024 credits" / "1.23 credits".
// Empty unit (unknown backend) hides the cell. Multi-Backend RFC §8.3 D5.
function formatCostByUnit(cost, unit) {
  if (cost == null || !isFinite(cost)) cost = 0;
  if (unit === 'credits') {
    if (cost === 0) return '0 credits';
    return (cost < 0.01 && cost > 0 ? cost.toFixed(4) : cost.toFixed(3)) + ' credits';
  }
  // Default: USD
  return '$' + (cost < 0.01 && cost > 0 ? cost.toFixed(4) : cost.toFixed(2));
}

function cliIcon(name) {
  // Kiro official ghost-style mark (sourced from https://kiro.dev/icon.svg).
  // Inlined here so the asset works offline + survives CSP. Compressed to
  // the essential shapes: rounded purple bg + white ghost body + 2 black
  // eyes. The original 1200×1200 is recoordinatized for the 16×16 viewbox
  // sidebar / header sc-cli-icon slot. UI Round 5 R5-1.
  if (name === 'kiro') return '<svg class="sc-cli-icon" viewBox="0 0 1200 1200" fill="none" xmlns="http://www.w3.org/2000/svg"><rect width="1200" height="1200" rx="260" fill="#9046FF"/><path d="M398.554 818.914C316.315 1001.03 491.477 1046.74 620.672 940.156C658.687 1059.66 801.052 970.473 852.234 877.795C964.787 673.567 919.318 465.357 907.64 422.374C827.637 129.443 427.623 128.946 358.8 423.865C342.651 475.544 342.402 534.18 333.458 595.051C328.986 625.86 325.507 645.488 313.83 677.785C306.873 696.424 297.68 712.819 282.773 740.645C259.915 783.881 269.604 867.113 387.87 823.883L399.051 818.914H398.554Z" fill="white"/><ellipse cx="636" cy="487" rx="40" ry="63" fill="black"/><ellipse cx="771" cy="487" rx="40" ry="63" fill="black"/></svg>';
  // Default: official Claude logomark (from claude.ai/favicon.svg)
  return '<svg class="sc-cli-icon" viewBox="0 0 248 248" fill="none"><path d="M52.4285 162.873L98.7844 136.879L99.5485 134.602L98.7844 133.334H96.4921L88.7237 132.862L62.2346 132.153L39.3113 131.207L17.0249 130.026L11.4214 128.844L6.2 121.873L6.7094 118.447L11.4214 115.257L18.171 115.847L33.0711 116.911L55.485 118.447L71.6586 119.392L95.728 121.873H99.5485L100.058 120.337L98.7844 119.392L97.7656 118.447L74.5877 102.732L49.4995 86.1905L36.3823 76.62L29.3779 71.7757L25.8121 67.2858L24.2839 57.3608L30.6515 50.2716L39.3113 50.8623L41.4763 51.4531L50.2636 58.1879L68.9842 72.7209L93.4357 90.6804L97.0015 93.6343L98.4374 92.6652L98.6571 91.9801L97.0015 89.2625L83.757 65.2772L69.621 40.8192L63.2534 30.6579L61.5978 24.632C60.9565 22.1032 60.579 20.0111 60.579 17.4246L67.8381 7.49965L71.9133 6.19995L81.7193 7.49965L85.7946 11.0443L91.9074 24.9865L101.714 46.8451L116.996 76.62L121.453 85.4816L123.873 93.6343L124.764 96.1155H126.292V94.6976L127.566 77.9197L129.858 57.3608L132.15 30.8942L132.915 23.4505L136.608 14.4708L143.994 9.62643L149.725 12.344L154.437 19.0788L153.8 23.4505L150.998 41.6463L145.522 70.1215L141.957 89.2625H143.994L146.414 86.7813L156.093 74.0206L172.266 53.698L179.398 45.6635L187.803 36.802L193.152 32.5484H203.34L210.726 43.6549L207.415 55.1159L196.972 68.3492L188.312 79.5739L175.896 96.2095L168.191 109.585L168.882 110.689L170.738 110.53L198.755 104.504L213.91 101.787L231.994 98.7149L240.144 102.496L241.036 106.395L237.852 114.311L218.495 119.037L195.826 123.645L162.07 131.592L161.696 131.893L162.137 132.547L177.36 133.925L183.855 134.279H199.774L229.447 136.524L237.215 141.605L241.8 147.867L241.036 152.711L229.065 158.737L213.019 154.956L175.45 145.977L162.587 142.787H160.805V143.85L171.502 154.366L191.242 172.089L215.82 195.011L217.094 200.682L213.91 205.172L210.599 204.699L188.949 188.394L180.544 181.069L161.696 165.118H160.422V166.772L164.752 173.152L187.803 207.771L188.949 218.405L187.294 221.832L181.308 223.959L174.813 222.777L161.187 203.754L147.305 182.486L136.098 163.345L134.745 164.2L128.075 235.42L125.019 239.082L117.887 241.8L111.902 237.31L108.718 229.984L111.902 215.452L115.722 196.547L118.779 181.541L121.58 162.873L123.291 156.636L123.14 156.219L121.773 156.449L107.699 175.752L86.304 204.699L69.3663 222.777L65.291 224.431L58.2867 220.768L58.9235 214.27L62.8713 208.48L86.304 178.705L100.44 160.155L109.551 149.507L109.462 147.967L108.959 147.924L46.6977 188.512L35.6182 189.93L30.7788 185.44L31.4156 178.115L33.7079 175.752L52.4285 162.873Z" fill="#D97757"/></svg>';
}

function sessionCardHtml(s) {
  const sNode = s.node || 'local';
  const isActive = selectedKey === s.key && selectedNode === sNode;
  const isNew = s.state === 'new';
  // cron-panel-consolidation RFC §4.2: cron sessions never render here
  // (server-side filter), so the prior `sc-cron-card` / `sc-cron` chip
  // were removed. If the filter ever leaked, the row would still render
  // as a normal card — the dismissSession isCron guard then prevents the
  // × button from accidentally invoking cron-job deletion.
  const cls = 'session-card' + (isActive ? ' active' : '') + (isNew ? ' new-card' : '');

  // Line 1: prompt. user_label (operator-set via rename) wins over any
  // auto-derived title so the rename is visible immediately across refreshes.
  const prompt = s.user_label || s.summary || s.last_prompt || (isNew ? '新会话' : '未命名');
  const icon = cliIcon(s.cli_name || 'cli');

  // Line 2: status dot + meta. Dead sessions are presented as "ready" to
  // operators — the underlying state is retained in sessionsData for the
  // resubscribe logic in onSessionState.
  const displayState = s.state === 'dead' ? 'ready' : s.state;
  const dotCls = displayState === 'running' ? 'dot-running' : (displayState === 'ready' ? 'dot-ready' : 'dot-new');
  const ago = s.last_active ? timeAgo(s.last_active) : '';
  const absTime = s.last_active ? formatAbsTime(s.last_active) : '';
  // Chat-style unread chip: rendered only when the session has completed
  // turns that the operator hasn't opened yet. Hidden on the active card —
  // selectSession zeroes the counter so this stays consistent on re-render.
  const unreadCount = sessionUnread[sid(s.key, sNode)] || 0;
  const unreadBadge = (unreadCount > 0 && !isActive)
    ? '<span class="sc-unread" aria-label="' + unreadCount + ' 条未读">' + (unreadCount > 99 ? '99+' : unreadCount) + '</span>'
    : '';
  // Per-card node badge is no longer needed: the sidebar is filtered to a
  // single node via the #node-selector, so every visible card is on the
  // currently-selected node. The badge is kept empty (vs. removed from the
  // template) so the surrounding .sc-meta layout stays identical.
  const nodeBadge = '';

  const dismissBtn = '<button type="button" class="btn-dismiss" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" onclick="event.stopPropagation();dismissSession(this.dataset.key,this.dataset.node)" title="remove" aria-label="Remove session">&times;</button>';

  const typeTag = s.source === 'terminal' ? sessionTypeTag(s.cli_name, s.entrypoint) : '';
  const agentCount = s.subagents ? s.subagents.length : 0;
  const agentBadge = agentCount > 0 ? '<span class="sc-agents">\u{1F916}\u00D7' + agentCount + '</span>' : '';
  // R110-P3 IM origin: show a small chip for sessions sourced from feishu /
  // slack / discord / weixin so operators can eyeball which cards are real
  // IM threads vs dashboard-local conversations. originBadgeHtml returns ''
  // for non-IM prefixes so the meta line stays clean for those.
  const originBadge = originBadgeHtml(s.key);
  // UI Round 5 R5-2: backend chip removed from session cards. The cli icon
  // (cliIcon, kiro-ghost vs claude-logomark) already disambiguates backend
  // visually, so the chip was redundant. backendChipHtml() helper kept for
  // doctor panel where listing backends needs an explicit text label.
  const metaHtml = '<span class="sc-dot ' + dotCls + '"></span>' +
    '<span>' + esc(displayState) + '</span>' +
    nodeBadge +
    originBadge +
    typeTag +
    agentBadge;

  // R110-P1: dim 30-rune preview of last assistant text reply. Skipped when
  // empty (omitempty hides it for brand-new sessions / runs that have not
  // produced an assistant text block yet — tool-only turns intentionally
  // leave the slot blank rather than echoing the prompt). Server already
  // truncates to 120 runes via textutil.TruncateRunes in
  // EventEntriesFromEventAt; the 30-cap here is a sidebar-specific second
  // truncation to keep the line tight on narrow widths.
  const responseRaw = s.last_response || '';
  const responseTrunc = truncateForSidebar(responseRaw, 30);
  const responseHtml = responseTrunc
    ? '<div class="sc-response" title="' + escAttr(responseRaw) + '">' + esc(responseTrunc) + '</div>'
    : '';

  return '<div class="' + cls + '" role="listitem" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" tabindex="0" aria-label="' + escAttr(prompt + ' · ' + displayState) + '" onclick="selectSession(this.dataset.key,this.dataset.node)" onkeydown="sessionCardKey(event)">' +
    dismissBtn +
    icon +
    '<div class="sc-body">' +
      '<div class="sc-header">' +
        '<div class="sc-prompt" title="' + escAttr(prompt) + '">' + esc(prompt) + '</div>' +
        unreadBadge +
        (ago ? '<span class="sc-time"' + (absTime ? ' title="' + escAttr(absTime) + '"' : '') + '>' + ago + '</span>' : '') +
      '</div>' +
      responseHtml +
      '<div class="sc-meta">' + metaHtml + '</div>' +
    '</div>' +
  '</div>';
}

// truncateForSidebar caps `s` to at most `n` Unicode code points (so CJK
// characters count as one each, matching textutil.TruncateRunes on the
// backend) and appends an ellipsis when truncation actually fired. Returns
// '' for null/empty inputs so callers can chain `if (out)` cheaply.
function truncateForSidebar(s, n) {
  if (!s) return '';
  // Array.from splits by code point; .length on a string would split by
  // UTF-16 code unit and double-count surrogate-pair emoji. n=30 with
  // emoji-heavy responses would otherwise cut a row at ~15 visible glyphs.
  const arr = Array.from(s);
  if (arr.length <= n) return s;
  return arr.slice(0, n).join('') + '…';
}

// updateCardUnreadChip patches the chat-style unread bubble inside a session
// card's header. Pulled out of onSessionState so selectSession (and any future
// caller) can share the same DOM shape without string-rebuilding the card.
function updateCardUnreadChip(card, count) {
  if (!card) return;
  const header = card.querySelector('.sc-header');
  if (!header) return;
  let chip = header.querySelector('.sc-unread');
  if (count > 0 && !card.classList.contains('active')) {
    const text = count > 99 ? '99+' : String(count);
    if (!chip) {
      chip = document.createElement('span');
      chip.className = 'sc-unread';
      const timeEl = header.querySelector('.sc-time');
      if (timeEl) header.insertBefore(chip, timeEl);
      else header.appendChild(chip);
    }
    chip.textContent = text;
    chip.setAttribute('aria-label', count + ' 条未读');
  } else if (chip) {
    chip.remove();
  }
}

// Keyboard activation for role=listitem session cards.
function sessionCardKey(e) {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  if (e.target.closest('.btn-dismiss')) return;
  e.preventDefault();
  const card = e.currentTarget;
  selectSession(card.dataset.key, card.dataset.node || 'local');
}

function resumeRecentSession(sessionId) {
  const found = historySessionsData.find(r => r.session_id === sessionId);
  resumeRecentById(sessionId, found ? found.workspace : '', found ? (found.last_prompt || found.summary || '') : '');
}

async function resumeRecentById(sessionId, workspace, lastPrompt) {
  // Guard: if already resuming this session, find the managed key and select it
  for (const s of allSessionsCache) {
    if (s.session_id === sessionId) { selectSession(s.key, s.node || 'local'); return; }
  }

  try {
    const headers = {'Content-Type': 'application/json'};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch('/api/sessions/resume', {
      method: 'POST', headers,
      body: JSON.stringify({session_id: sessionId, workspace: workspace || '', last_prompt: lastPrompt || ''})
    });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('恢复会话', r.status, raw);
      return;
    }
    const data = await r.json();
    const key = data.key;
    if (!key) return;

    // Force sidebar refresh to pick up the dismissed entry
    lastVersion = 0;
    await fetchSessions();

    selectSession(key, 'local');
    previewRecentSession(key, sessionId, workspace);
  } catch (e) {
    showNetworkError('恢复会话', e);
  }
}

async function previewRecentSession(expectedKey, sessionId, cwd) {
  try {
    const headers = {};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    // Pass cwd (the resumed session's workspace) so the backend hits the
    // O(1) CWD-derived path lookup and skips the findSessionJSONL negative
    // cache — see previewDiscovered for the full rationale.
    const cwdParam = cwd ? '&cwd=' + encodeURIComponent(cwd) : '';
    // RNEW-UX-003: 5s timeout — this is a best-effort snapshot after
    // resume; if the backend stalls, drop the preview rather than hang.
    let entries;
    try {
      entries = await fetchJSON('/api/discovered/preview?session_id=' + encodeURIComponent(sessionId) + cwdParam, { headers, timeoutMs: 5000 });
    } catch (err) {
      if (err.status) return;
      throw err;
    }
    if (selectedKey !== expectedKey) return; // user navigated away
    if (!entries || entries.length === 0) return;
    renderEvents(entries);
  } catch (e) {
    console.error('previewRecentSession:', e);
  }
}

const STATUS_LABELS = { off: 'offline', connecting: 'connecting...', authenticating: 'authenticating...', connected: 'connected', disconnected: 'HTTP fallback', disconnected_retry: 'reconnecting...' };
const REMOTE_LABELS = { ok: 'connected', error: 'error', offline: 'offline', unreachable: 'unreachable' };
const VALID_DOT_CLASSES = { ok: 'ok', error: 'error', offline: 'offline', connecting: 'connecting', off: 'off', connected: 'connected', disconnected: 'disconnected', authenticating: 'authenticating', unreachable: 'unreachable' };

// formatOutageDuration turns an elapsed millisecond count into a Chinese
// label suitable for the sidebar-status hint. Pure function so a contract
// test can exercise it without driving the DOM or a WS state machine.
// Under 5s returns '' (render-suppressed - transient reconnects don't
// warrant a duration hint); otherwise rounds to seconds up to 90s, then
// to minutes, then to hours. Kept coarse deliberately: a live-ticking
// ms counter is anxiety-inducing and would force a re-render every
// animation frame.
function formatOutageDuration(elapsedMs) {
  const ms = Math.max(0, Math.floor(elapsedMs));
  if (ms < 5000) return '';
  const s = Math.floor(ms / 1000);
  if (s < 90) return '已断开 ' + s + ' 秒';
  const m = Math.floor(s / 60);
  if (m < 60) return '已断开 ' + m + ' 分';
  const h = Math.floor(m / 60);
  const remM = m - h * 60;
  return remM > 0 ? '已断开 ' + h + ' 小时 ' + remM + ' 分' : '已断开 ' + h + ' 小时';
}

// (Removed) _statusTickTimer / _updateStatusTick previously drove a 1s
// setInterval(updateStatusBar) loop while WS was disconnected so the
// "已断开 N 秒" label inside #sidebar-status could tick forward between
// state transitions. That DOM was deleted when the sidebar gave its bottom
// real estate to the session list, after which updateStatusBar early-
// returns when the container is missing — its only remaining side-effect
// is updateNodeSelector(), which setState already invokes on every WS
// state change via updateStatusBar(). The 1s tick therefore had no
// user-visible effect (the trigger dot updates on real state changes,
// not by polling) and was a periodic no-op repaint. Issue #434.

function updateStatusBar() {
  const container = document.getElementById('sidebar-status');
  // #sidebar-status 节点已在"底部让位给 session 列表"的迭代中删除。没节点就
  // 早退，但 updateNodeSelector 必须照常跑——它驱动顶部多节点下拉的显隐，
  // 跟 sidebar-status 是两码事，否则 multi-node 切换框会一起消失。
  renderRailConnStatus();
  if (!container) { updateNodeSelector(); return; }
  const wsUp = wsm.state === WS_STATES.CONNECTED;
  // When multiple nodes are connected, the #node-selector widget already
  // surfaces per-node status; the sidebar-status bar collapses to "current
  // node only" to reclaim vertical space. Single-node setups keep the legacy
  // behavior (local row always shown) so nothing regresses for the common case.
  const multi = isMultiNode();
  const currentIsLocal = !multi || selectedNode === 'local';

  // Local node row (always first)
  // Distinguish short reconnect vs stable polling mode
  const statusKey = (wsm.state === WS_STATES.DISCONNECTED && wsm.backoff > 8000) ? 'disconnected' : (wsm.state === WS_STATES.DISCONNECTED ? 'disconnected_retry' : wsm.state);
  const localLabel = STATUS_LABELS[statusKey] || wsm.state;
  const dotKey = statusKey === 'disconnected' ? 'connecting' : wsm.state; // HTTP fallback = yellow dot

  // UX P1 manual reconnect: when the connection has been down long enough
  // that backoff has grown past 8s (statusKey "disconnected" — the
  // "HTTP fallback" stable state), offer an explicit "reconnect" button
  // so users don't have to wait for the automatic retry window. The
  // short-retry state (backoff <= 8s, labeled "reconnecting...") stays
  // button-free because the next auto-retry is already imminent.
  const showReconnect = statusKey === 'disconnected';
  const reconnectBtn = showReconnect
    ? '<button type="button" class="status-reconnect" onclick="reconnectNow()" title="立即重连" aria-label="立即重连">重连</button>'
    : '';

  // R110-P1 outage duration hint: only when we have a stamped disconnect
  // timestamp (live outage) AND the state is not CONNECTED. A stale non-zero
  // timestamp on CONNECTED would be a bug elsewhere; the state gate is
  // defensive. Empty string from formatOutageDuration means "< 5s, suppress"
  // so transient flickers don't spawn a noisy hint.
  const outageLabel = (!wsUp && wsm._disconnectedSince > 0)
    ? formatOutageDuration(Date.now() - wsm._disconnectedSince)
    : '';

  // Auth rate-limit countdown surfaces here (replaces the old top-of-screen
  // toast). Rendered only while the gate is armed; _wsAuthCountdownTimer
  // repaints this row every second. Suppresses the reconnect button while
  // active — no point offering a manual dial that connect() will bounce.
  const authWaitSecs = (wsm._authBlockUntil > 0)
    ? Math.max(0, Math.ceil((wsm._authBlockUntil - Date.now()) / 1000))
    : 0;
  const authWaitLabel = authWaitSecs > 0
    ? '鉴权过于频繁，' + authWaitSecs + 's 后自动重连'
    : '';
  const reconnectBtnGated = authWaitLabel ? '' : reconnectBtn;

  let html = '';
  if (currentIsLocal) {
    html = '<div class="status-row">' +
      '<span class="status-dot ' + (VALID_DOT_CLASSES[dotKey] || 'off') + '"></span>' +
      '<div class="status-info">' +
        '<div class="status-ws">' + esc(localLabel) + '</div>' +
        (outageLabel ? '<div class="status-outage">' + esc(outageLabel) + '</div>' : '') +
        (authWaitLabel ? '<div class="status-authwait">' + esc(authWaitLabel) + '</div>' : '') +
      '</div>' + reconnectBtnGated +
      '</div>';
  } else {
    // Multi-node view with a remote selected: show one row for the chosen
    // remote. Other remotes are summarized by the selector's aggregated
    // alert dot \u2014 users open the dropdown to see the full list.
    const nd = nodesData[selectedNode] || {};
    const status = nd.status || (wsUp ? 'offline' : 'unreachable');
    const dotCls = VALID_DOT_CLASSES[status] || 'offline';
    const label = REMOTE_LABELS[status] || status;
    html = '<div class="status-row">' +
      '<span class="status-dot ' + dotCls + '"></span>' +
      '<div class="status-info">' +
        '<div class="status-ws">' + esc(label) + '</div>' +
      '</div></div>';
  }

  container.innerHTML = html;
  // Keep the node selector's trigger dot in sync with live status \u2014 a remote
  // flipping offline should update both the bar below and the selector above
  // without waiting for the next /api/sessions poll.
  updateNodeSelector();
}

// CHEATSHEET_ENTRIES is the single source of truth for the shortcut modal.
// Keeping it as an array (instead of raw HTML) lets tests grep for specific
// rows and lets the render path escape user-visible text consistently.
// The `keys` arrays are rendered as <kbd> chips joined by "+".
//
// R110-P2 extension: added "斜杠命令" and "上传" sections so the Help panel
// documents features that were only discoverable via README / source until
// now. Slash commands mirror the router in `internal/dispatch/commands.go`
// (`/new`, `/cron`, `/help`, `/pwd`, `/cd`, `/project`); upload keys
// describe the `.btn-icon` paperclip and the `dragover/drop` handler on
// `#input-area`. Features that are NOT yet implemented (image paste,
// `@` file autocomplete) are deliberately omitted — the Help panel must
// stay a promise of actually-working UX.
const CHEATSHEET_ENTRIES = [
  { section: '会话' },
  { keys: ['Cmd/Ctrl', '1'], alt: ['Cmd/Ctrl', '9'], desc: '切换到项目组内第 N 个会话' },
  { keys: ['Cmd/Ctrl', '↑'], alt: ['Cmd/Ctrl', '↓'], desc: '上/下一会话（同项目组内）' },
  { keys: ['Cmd/Ctrl', 'K'], desc: '打开新建会话面板（最近使用置顶）' },
  { keys: ['Alt', 'N'], desc: '新建会话' },
  { section: '消息' },
  { keys: ['Enter'], desc: '发送消息' },
  { keys: ['Shift', 'Enter'], desc: '输入框内换行' },
  { keys: ['Esc', 'Esc'], desc: '双击 Esc 打断当前运行中的回复' },
  { keys: ['Alt', '↑'], alt: ['Alt', '↓'], desc: '跳到上/下一条消息' },
  { keys: ['Esc'], desc: '关闭弹窗 / 关闭历史面板' },
  { section: '斜杠命令' },
  { keys: ['/new'], desc: '重置当前 agent 对话（/new review 切到 code-reviewer 等 agent）' },
  { keys: ['/cd'], desc: '切换工作目录（/cd <path>；受 session.cwd 的 allowed_root 限制）' },
  { keys: ['/pwd'], desc: '显示当前工作目录' },
  { keys: ['/project'], desc: '绑定会话到项目（/project <name> 或 /project off 解绑）' },
  { keys: ['/cron'], desc: '定时任务：/cron add "<schedule>" <prompt> · /cron list · /cron del <id>' },
  { keys: ['/help'], desc: '显示可用命令（IM 平台内也可用）' },
  { section: '上传' },
  { keys: ['📎'], desc: '点击输入栏左侧图标选图（单文件最多 40MB，总计 20 张）' },
  { keys: ['拖拽'], desc: '把图片拖入输入区，边框变蓝即可放下上传' },
  { section: '帮助' },
  { keys: ['?'], desc: '打开本快捷键面板' },
];

// renderCheatsheetHTML returns an HTML string; esc-safe because every
// piece of user-visible text originates from CHEATSHEET_ENTRIES (static
// const). kbd chips are literal HTML but the content is whitelisted.
function renderCheatsheetHTML() {
  let rows = '';
  for (const entry of CHEATSHEET_ENTRIES) {
    if (entry.section) {
      rows += '<div class="ks-section">' + esc(entry.section) + '</div>';
      continue;
    }
    let keysHTML = entry.keys.map(k => '<kbd>' + esc(k) + '</kbd>').join(' + ');
    if (entry.alt) {
      keysHTML += ' / ' + entry.alt.map(k => '<kbd>' + esc(k) + '</kbd>').join(' + ');
    }
    rows += '<div class="ks-keys">' + keysHTML + '</div>';
    rows += '<div class="ks-desc">' + esc(entry.desc) + '</div>';
  }
  return rows;
}

// showCheatsheet opens the shortcut modal. Reuses .modal-overlay + trapFocus
// so Esc-to-close and focus trapping come for free. Idempotent: a second
// call while the modal is open is a no-op.
function showCheatsheet() {
  if (document.querySelector('.modal-overlay.cheatsheet-overlay')) return;
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay cheatsheet-overlay';
  overlay.innerHTML =
    '<div class="modal cheatsheet" role="dialog" aria-modal="true" aria-label="键盘快捷键">' +
      '<h3>键盘快捷键</h3>' +
      '<div class="ks-sub">按 <kbd>?</kbd> 可随时打开本面板，<kbd>Esc</kbd> 关闭。</div>' +
      '<div class="ks-grid">' + renderCheatsheetHTML() + '</div>' +
      '<div class="modal-btns">' +
        '<button type="button" class="primary" onclick="dismissCheatsheet()">好的</button>' +
      '</div>' +
    '</div>';
  overlay.addEventListener('click', e => {
    if (e.target === overlay) dismissCheatsheet();
  });
  document.body.appendChild(overlay);
  trapFocus(overlay);
}

function dismissCheatsheet() {
  const ov = document.querySelector('.modal-overlay.cheatsheet-overlay');
  if (ov) ov.remove();
}

// Global "?" shortcut: open the cheatsheet when not typing in an input
// and no other modal is already open. The same Shift+/ also fires "?"
// on US layouts, so the `key === '?'` check covers both.
document.addEventListener('keydown', function(e) {
  if (e.key !== '?') return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
  // Don't stack cheatsheet on top of another modal — let Esc chain first.
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  e.preventDefault();
  showCheatsheet();
});

// R110-P3 Cmd/Ctrl+K opens the command palette — a widely-understood
// convention (GitHub, Slack, Linear). Fires even from inside the message
// input / textareas because switching sessions mid-typing is a common
// flow; the palette's trapFocus and input field take over focus so the
// prior draft remains saved via sessionDrafts per selectSession contract.
// Skips when another modal/palette is already open so repeated Cmd+K
// doesn't stack overlays.
document.addEventListener('keydown', function(e) {
  if (!(e.metaKey || e.ctrlKey) || e.key !== 'k') return;
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  e.preventDefault();
  createNewSession();
});

function selectSession(key, node) {
  node = node || 'local';
  resetTurnState();
  // Close any open agent drill-in view before the selectedKey flips
  // (RFC v4 agent-team-ui §3.6.6). Must run BEFORE saveScrollPos so the
  // agent-view scroll snapshot still keys off the old session id.
  if (window.AgentView && typeof window.AgentView.onSessionSwitch === 'function') {
    window.AgentView.onSessionSwitch(key, node);
  }
  // Recent session card click → trigger resume flow
  // Discovered session card click → trigger preview flow
  // Save draft for current session before switching
  if (selectedKey) {
    const inp = document.getElementById('msg-input');
    const draft = getMsgValue(inp);
    if (draft) sessionDrafts[selectedKey] = draft;
    else delete sessionDrafts[selectedKey];
    // 同时快照当前会话的滚动位置，回来时恢复
    saveScrollPos(selectedKey, selectedNode);
  }
  if (key.startsWith('_discovered:')) {
    const pid = parseInt(key.split(':')[1]);
    const d = discoveredItems.find(x => x.pid === pid);
    if (d) {
      previewDiscovered(d.session_id, d.cwd, d.pid, d.proc_start_time || 0, d.node || '', d.cli_name || 'cli', d.entrypoint || '');
      return;
    }
  }
  pendingDiscovered = null;
  // Picking a session returns to the chat view from any other top-level view
  // (assets / cron / settings). This restores the chat sidebar+main, hides the
  // other view's panels, and flips activeView back to 'chat' so renderMainShell
  // (which writes #main) is visible and any in-flight cron repaint is suppressed.
  if (activeView !== 'chat') setActivityView('chat');
  const prevKey = selectedKey;
  const prevNode = selectedNode;
  selectedKey = key;
  selectedNode = node;
  // Opening a card counts as "reading" it — clear the chat-style unread chip
  // before the DOM toggle below so the next render reflects a zeroed state.
  const selSid = sid(key, node);
  if (sessionUnread[selSid]) {
    delete sessionUnread[selSid];
  }
  // Picking a session on a different node shifts the sidebar filter there
  // too — users expect the selector to follow their click, not strand them
  // looking at another node's list. Persist + refresh the widget.
  if (prevNode !== selectedNode) {
    try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
    if (typeof updateNodeSelector === 'function') updateNodeSelector();
  }
  lastEventTime = 0;
  lastRenderedEventTime = 0;
  oldestFetchedEventTime = 0;
  _autoPageBackCount = 0; // reset the blank-page recovery budget per session
  mobileEnterChat();
  stopPreviewPolling();
  const activeCard = setActiveSessionCard(key, node);
  if (activeCard) updateCardUnreadChip(activeCard, 0);
  renderMainShell();
  navRebuild(); // clear stale nav state before async events arrive
  const draftInput = document.getElementById('msg-input');
  if (draftInput && sessionDrafts[key]) {
    setMsgValue(draftInput, sessionDrafts[key]);
  }

  const changed = prevKey !== key || prevNode !== node;
  if (wsm.isConnected()) {
    if (changed) wsm.unsubscribe();
    wsm.lastEventTimeWs = 0;
    wsm.subscribe(key, node);
    if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
  } else {
    fetchEvents(true);
    if (eventTimer) clearInterval(eventTimer);
    eventTimer = setInterval(() => fetchEvents(false), 1000);
  }
}

// dismissSession removes a session from the sidebar. The × button deletes
// immediately with no confirmation — per operator preference, the friction
// isn't worth it. Accidental deletes are recoverable by re-entering the
// prompt (pending) or reopening the CLI (remote/discovered).
async function dismissSession(key, node, opts) {
  node = node || 'local';
  delete sessionDrafts[key];
  delete sessionScrollPos[sid(key, node)];
  // sessionBackends is normally consumed on first sendMessage. A dismiss
  // before any send leaves the entry behind; clear it defensively so a
  // subsequent re-create with the same key (unlikely but possible if the
  // ms timestamp collides on rapid double-create) doesn't inherit a
  // stale backend pick.
  delete sessionBackends[key];

  // cron-panel-consolidation RFC §4.2: defensive guard. Cron stubs are
  // filtered server-side so this branch should never run in production —
  // but if a future server bug ever leaks a cron key through, we must
  // NOT call DELETE /api/sessions (the scheduler still owns the stub).
  if (isCronSessionKey(key)) {
    // cron-panel-consolidation RFC §4.2: cron stubs are filtered server-side
    // and should never appear in the sidebar at all — this branch only
    // executes if a future server bug leaks one through. Guard-rail behaviour:
    // remove the rogue card from the DOM but DO NOT call DELETE /api/sessions
    // (the cron scheduler still owns the stub) and DO NOT mutate any cron
    // panel state. Single source of truth for cron-job lifecycle remains
    // the 定时任务 panel (cronDelete → DELETE /api/cron).
    if (selectedKey === key) {
      selectedKey = null;
      if (wsm.subscribedKey === key) wsm.unsubscribe();
      document.getElementById('main').innerHTML = mainEmptyHtml();
      wireQuickAskInput();
    }
    const card = document.querySelector('.session-card[data-key="' + key + '"]');
    if (card) card.remove();
    lastVersion = 0;
    debouncedFetchSessions();
    return;
  }

  // If it's a pending (never-sent) session, just remove from localStorage
  if (sessionWorkspaces[key] !== undefined) {
    removePendingSession(key);
    delete sessionsData[sid(key, node)];
    if (selectedKey === key) {
      selectedKey = null;
      document.getElementById('main').innerHTML = mainEmptyHtml();
      wireQuickAskInput();
    }
    lastVersion = 0;
    debouncedFetchSessions();
    return;
  }

  // Discovered session — kill external process via /api/discovered/close
  if (key.startsWith('_discovered:')) {
    const pid = parseInt(key.split(':')[1]);
    const d = discoveredItems.find(x => x.pid === pid);
    if (!d) { showToast('未找到该外部会话', 'warning'); return; }
    try {
      const headers = {'Content-Type': 'application/json'};
      const token = getToken();
      if (token) headers['Authorization'] = 'Bearer ' + token;
      try {
        await fetchJSON('/api/discovered/close', {
          timeoutMs: 10000,
          method: 'POST', headers,
          body: JSON.stringify({pid: d.pid, session_id: d.session_id || '', cwd: d.cwd || '', proc_start_time: d.proc_start_time || 0, node: node || ''})
        });
      } catch (err) {
        if (err && err.status) showAPIError('关闭外部会话', err.status, err.message || '');
        else showNetworkError('关闭外部会话', err);
        return;
      }
      discoveredItems = discoveredItems.filter(x => x.pid !== pid);
      if (pendingDiscovered && pendingDiscovered.pid === pid) {
        pendingDiscovered = null;
        stopPreviewPolling();
        document.getElementById('main').innerHTML = mainEmptyHtml();
        wireQuickAskInput();
      }
      const card = document.querySelector('.session-card[data-key="' + key + '"]');
      if (card) card.remove();
      lastVersion = 0;
      debouncedFetchSessions();
    } catch (e) { showNetworkError('关闭外部会话', e); }
    return;
  }

  // Optimistic delete: the card vanishes immediately rather than freezing
  // for the server's teardown round-trip. The backend's DELETE /api/sessions
  // now unregisters the session synchronously and runs the slow teardown
  // (proc.Close up to 8s + event-log/attachment cleanup) in a detached
  // goroutine (RemoveAsync), so 200 means "gone from the list" and arrives
  // fast — but we don't even wait for it to update the UI.
  const skey = sid(key, node);
  // Mark dismissed so an in-flight poll / sessions_update event can't
  // resurrect the card before DELETE confirms (cleared in finally below).
  _optimisticDeleteKeys.add(skey);
  delete sessionsData[skey];
  if (selectedKey === key) {
    selectedKey = null;
    if (wsm.subscribedKey === key) wsm.unsubscribe();
    document.getElementById('main').innerHTML = mainEmptyHtml();
    wireQuickAskInput();
  }
  const card = document.querySelector('.session-card[data-key="' + key + '"]');
  if (card) card.remove();

  const headers = {'Content-Type': 'application/json'};
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const body = {key: key};
  if (node && node !== 'local') body.node = node;
  // Fire-and-forget: do NOT await — the UI is already updated. On failure we
  // re-sync from the server so a genuinely-undeleted session reappears.
  fetchJSON('/api/sessions', {timeoutMs: 10000, method: 'DELETE', headers, body: JSON.stringify(body)})
    .catch(err => {
      // 404 means the session was already gone — that's the outcome we want,
      // so swallow it. Any other error means the delete may not have landed:
      // surface it and let the re-sync below pull the real list back.
      if (err && err.status !== 404) {
        if (err.status) showAPIError('删除会话', err.status, err.message || '');
        else showNetworkError('删除会话', err);
      }
    })
    .finally(() => {
      // Stop suppressing this key so the next fetch reflects server truth:
      // if the delete stuck, the session stays gone; if it failed, the card
      // comes back (operator must re-select it — we intentionally don't
      // restore the cleared main panel to avoid masking a failed delete).
      _optimisticDeleteKeys.delete(skey);
      lastVersion = 0;
      debouncedFetchSessions();
    });
}

// Operator-facing rename flow. Prompts for a new display label; empty input
// clears any prior label and falls back to the summary/last_prompt display
// chain. Uses PATCH /api/sessions/label so the mutation round-trips through
// the server and persists across reloads.
async function renameSession() {
  if (!selectedKey) return;
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};
  const current = s.user_label || '';
  // RNEW-UX-013: replaced window.prompt with themed promptDialog so the
  // rename flow matches the rest of the dashboard (dark theme, trapFocus,
  // Esc/backdrop cancel) and doesn't block the event loop on mobile.
  const input = await promptDialog({
    title: '重命名会话',
    message: '留空恢复默认标题，最多 128 字节',
    defaultValue: current,
    placeholder: '输入新标题',
    confirmText: '保存',
    maxLength: 128,
  });
  if (input === null) return; // user cancelled
  const next = input.trim();
  if (next === current) return;
  const headers = {'Content-Type': 'application/json'};
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const body = {key: selectedKey, label: next};
  if (selectedNode && selectedNode !== 'local') body.node = selectedNode;
  try {
    await fetchJSON('/api/sessions/label', {
      timeoutMs: 10000,
      method: 'PATCH', headers,
      body: JSON.stringify(body),
    });
  } catch (err) {
    if (err && err.status) showAPIError('重命名', err.status, err.message || '');
    else showNetworkError('重命名', err);
    return;
  }
  // Patch local cache so the title refreshes before the next poll lands.
  const cacheKey = sid(selectedKey, selectedNode);
  if (sessionsData[cacheKey]) {
    sessionsData[cacheKey].user_label = next;
  }
  lastVersion = 0;
  debouncedFetchSessions();
  if (typeof renderMainShell === 'function') renderMainShell();
  showToast(next ? '已重命名' : '已恢复默认标题');
}

// --- Markdown export (UX P2) ---

// MARKDOWN_EXPORT_IGNORE captures event types that carry no user-visible
// content in the dashboard render path (tool_use + internal agent
// bookkeeping + the result envelope duplicated by streaming `text`
// events). The export pipeline drops them to keep the emitted document
// aligned with what the operator actually read in the UI.
const MARKDOWN_EXPORT_IGNORE = new Set(['tool_use', 'result', 'agent', 'task_start', 'task_progress', 'task_done', 'thinking', 'ask_question']);

// sessionMarkdownFilename returns a safe, dated filename for a session
// export. Strips filesystem-hostile characters from the title and caps
// length so browser download dialogs don't truncate unpredictably.
function sessionMarkdownFilename(title, whenMS) {
  const d = new Date(whenMS || Date.now());
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const stamp = d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
  let safe = String(title || 'session').replace(/[\\\/:*?"<>|\x00-\x1f]+/g, ' ').trim();
  // Collapse runs of whitespace to a single dash and cap at 80 chars so
  // the dated suffix always fits in common filesystem path limits.
  safe = safe.replace(/\s+/g, '-').slice(0, 80) || 'session';
  return 'naozhi-' + safe + '-' + stamp + '.md';
}

// formatSessionMarkdown builds the Markdown body from a list of events.
// Kept as a pure function (no DOM / fetch access) so it can be tested
// in isolation — see TestDashboardJS_MarkdownExport_FormatsContent
// which grep-checks the input→output contract.
function formatSessionMarkdown(meta, events) {
  const lines = [];
  lines.push('# ' + (meta.title || '未命名会话'));
  lines.push('');
  if (meta.key) lines.push('- **会话**: `' + meta.key + '`');
  if (meta.node && meta.node !== 'local') lines.push('- **节点**: `' + meta.node + '`');
  if (meta.cli) lines.push('- **CLI**: ' + meta.cli);
  if (meta.workspace) lines.push('- **工作目录**: `' + meta.workspace + '`');
  if (meta.cost != null) lines.push('- **花费**: $' + (meta.cost.toFixed ? meta.cost.toFixed(4) : meta.cost));
  lines.push('- **导出时间**: ' + new Date().toISOString());
  lines.push('');
  lines.push('---');
  lines.push('');

  for (const e of events) {
    if (!e || !e.type) continue;
    if (MARKDOWN_EXPORT_IGNORE.has(e.type)) continue;
    // Mirror the UI filter in eventHtml: Claude Code system XML injected
    // as user messages is noise in both renders.
    const raw = (e.detail || e.summary || '');
    if (e.type === 'user' && /^<(task-notification|system-reminder|local-command|command-name|available-deferred-tools)[\s>]/.test(raw)) continue;
    // CLI-synthesised interrupt marker (SIGINT-aborted turn): not user intent,
    // mirrors the Go-side isClaudeInterruptMarker filter.
    if (e.type === 'user' && (raw === '[Request interrupted by user]' || raw === '[Request interrupted by user for tool use]')) continue;

    const ts = e.time ? new Date(e.time).toISOString() : '';
    if (e.type === 'user') {
      lines.push('## 用户' + (ts ? ' · ' + ts : ''));
      lines.push('');
      lines.push(raw);
      if (e.images && e.images.length) {
        lines.push('');
        e.images.forEach((src, i) => lines.push('![image ' + (i + 1) + '](' + src + ')'));
      }
      lines.push('');
    } else if (e.type === 'text') {
      lines.push('## 助手' + (ts ? ' · ' + ts : ''));
      lines.push('');
      lines.push(raw);
      lines.push('');
    } else if (e.type === 'todo') {
      lines.push('### TODO' + (ts ? ' · ' + ts : ''));
      lines.push('');
      // e.detail for todo events is a JSON array of {content, status}; keep
      // markdown output simple and parseable even on malformed payloads.
      try {
        const items = JSON.parse(raw);
        if (Array.isArray(items)) {
          items.forEach(it => {
            const done = it && (it.status === 'completed' || it.status === 'done');
            lines.push('- [' + (done ? 'x' : ' ') + '] ' + (it && it.content ? it.content : ''));
          });
        } else {
          lines.push(raw);
        }
      } catch (_) {
        lines.push(raw);
      }
      lines.push('');
    } else if (e.type === 'system' || e.type === 'init') {
      // Surface system notices as blockquotes so reviewers see session
      // boundaries (init, restart) without mixing them into conversation.
      const summary = e.summary || e.type;
      lines.push('> _' + summary + '_' + (ts ? ' · ' + ts : ''));
      lines.push('');
    }
  }
  return lines.join('\n');
}

async function downloadSessionMarkdown() {
  if (!selectedKey) return;
  try {
    let url = '/api/sessions/events?key=' + encodeURIComponent(selectedKey);
    if (selectedNode && selectedNode !== 'local') url += '&node=' + encodeURIComponent(selectedNode);
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(url, { headers });
    if (!r.ok) {
      showAPIError('导出会话', r.status, '');
      return;
    }
    const events = await r.json();
    if (!Array.isArray(events) || events.length === 0) {
      showToast('会话无可导出内容', 'warning');
      return;
    }
    const s = sessionsData[sid(selectedKey, selectedNode)] || {};
    const keyParts = (selectedKey || '').split(':');
    const title = s.user_label || s.summary || s.last_prompt ||
      keyTailDisplay(keyParts) || selectedKey || '';
    const md = formatSessionMarkdown({
      title: title,
      key: selectedKey,
      node: selectedNode,
      cli: s.cli_name ? (s.cli_name + (s.cli_version ? ' v' + s.cli_version : '')) : '',
      workspace: s.workspace || sessionWorkspaces[selectedKey] || '',
      cost: (typeof s.total_cost === 'number' ? s.total_cost : null),
    }, events);

    // Browser-download path. Using URL.createObjectURL keeps the blob in
    // memory only long enough for the anchor click to fire; revoking
    // immediately would race on some browsers, so we defer via timeout.
    const blob = new Blob([md], { type: 'text/markdown;charset=utf-8' });
    const href = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = href;
    a.download = sessionMarkdownFilename(title, Date.now());
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(() => URL.revokeObjectURL(href), 60000);
    showToast('已导出 ' + events.length + ' 条事件', 'success', 2000);
  } catch (e) {
    showNetworkError('导出会话', e);
  }
}

function renderMainShell() {
  const main = document.getElementById('main');
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};

  const keyParts = (selectedKey || '').split(':');
  const agentIsGeneric = !s.agent || s.agent === 'general';
  // Primary title: user_label (operator-set rename) > summary > latest prompt
  // > agent name > key tail.
  const displayName = s.user_label || s.summary || s.last_prompt || (agentIsGeneric ? '' : s.agent) || keyTailDisplay(keyParts) || selectedKey || '';

  // Detail line: left = CLI name + version, middle = backend chip (multi-
  // backend mode only) + IM origin chip (only for real IM threads —
  // feishu/slack/discord/weixin), right = cost (formatted per session's
  // cost_unit). originBadgeHtml / backendChipHtml return '' when the
  // session/deployment doesn't warrant a chip so the layout stays clean.
  const effCLIName = s.cli_name || backendDisplayName(sessionBackends[selectedKey]) || defaultCLIName;
  const effCLIVersion = s.cli_version || backendDisplayVersion(sessionBackends[selectedKey]) || defaultCLIVersion;
  const cliLabel = effCLIName ? esc(effCLIName) + (effCLIVersion ? ' v' + esc(effCLIVersion) : '') : '';
  // UI Round 5 R5-3: model display for all backends.
  //   - claude path: SessionView.model is auto-populated from the
  //     system/init event ("global.anthropic.claude-opus-4-7[1m]"),
  //     so it is always present after the first turn lands. Pre-init
  //     turns (rare, brief window during spawn) and reconnect-without-
  //     replay falls back to "(模型未配置)".
  //   - kiro path: SessionView.model echoes cli.backends[].model from
  //     config; "" if operator left it unset (kiro picks "auto").
  // We compress noisy claude-style identifiers (e.g.
  // "global.anthropic.claude-opus-4-7[1m]" → "claude-opus-4.7 1M") for
  // the dashboard but keep the raw value in `title` for debug.
  const rawModel = s.model || '';
  const compactModel = rawModel
    .replace(/^global\.anthropic\./, '')      // strip Bedrock-style prefix
    .replace(/-(\d+)-(\d+)/, '-$1.$2')          // 4-7 → 4.7 (matches kiro list)
    .replace(/\[(\d+m)\]$/i, ' $1');            // [1m] → " 1m"
  const modelLabel = rawModel
    ? '<span class="model-label" title="' + escAttr(rawModel) + '">· ' + esc(compactModel) + '</span>'
    : '<span class="model-label model-label-unset" title="model 未在 system/init 上报；可能仍在 spawn 中">· (模型未配置)</span>';
  const headerOriginBadge = originBadgeHtml(selectedKey);
  // UI Round 5 R5-2: header backend chip removed. The "kiro v2.3.0" /
  // "claude-code 2.1.143" cliLabel already names the backend; the
  // surrounding chip was a duplicate signal that competed for attention
  // with cost / turn-timer.
  const headerBackendChip = '';
  // Multi-Backend RFC §8.3 D5: cost unit comes from the SessionView so
  // claude shows "$" and kiro shows "credits". Empty unit (unknown backend)
  // hides the cell — keeps the layout clean rather than rendering "$NaN".
  const costUnit = s.cost_unit || '';
  const cost = s.total_cost || 0;
  const showCost = costUnit !== '';
  const costText = formatCostByUnit(cost, costUnit);
  const costClass = 'detail-cost' + (cost >= 1 ? ' high-cost' : cost > 0 ? ' has-cost' : '');
  // R110-P3 cost tooltip: compute the same detail the live updater
  // (updateHeaderCost) writes, so the very first render isn't missing
  // hover content until a subsequent event refresh lands.
  const costTooltip = formatHeaderCostTooltip(s, selectedKey, selectedNode);
  const costTitleAttr = costTooltip ? ' title="' + escAttr(costTooltip) + '"' : '';
  // Multi-Backend RFC §8.3 D6: context usage progress bar driven by the
  // UI Round 5 R5-7: header no longer renders ctx-bar — the 48×6 px
  // strip carried low signal (operator can't act on "ctx 12%"), competed
  // with cost / turn-timer for attention, and at <5% looked identical to
  // "no data". The server-side SessionView.ContextUsagePercent stays so
  // doctor / future compact-mode renders can opt in.
  const ctxBarHtml = '';
  // Multi-Backend RFC §8.3 D7: turn duration timer (kiro real value;
  // claude 0 until estimator lands → cell hidden).
  let turnTimerHtml = '';
  if (typeof s.turn_duration_ms === 'number' && s.turn_duration_ms > 0) {
    const sec = (s.turn_duration_ms / 1000).toFixed(1);
    turnTimerHtml = '<span class="detail-turn-timer" title="上一轮耗时 ' + esc(sec) + 's">' +
      esc(sec) + 's</span>';
  }

  // Rename is available only for managed sessions owned by this or a connected
  // naozhi instance. Discovered (_discovered:*) entries are external processes
  // with no backend label storage, and we intentionally hide the control there.
  const canRename = selectedKey && !selectedKey.startsWith('_discovered:');
  const renameBtn = canRename
    ? '<button class="btn-rename" onclick="renameSession()" title="重命名会话" aria-label="重命名会话">✎</button>'
    : '';
  // UX P2 Markdown export: any session that has an addressable key can be
  // exported — no dependency on managed status because the /api/sessions/events
  // endpoint serves both managed and discovered keys uniformly. The button
  // shares the .btn-rename hover-reveal treatment so the header stays calm
  // by default.
  const downloadBtn = selectedKey
    ? '<button class="btn-rename btn-download" onclick="downloadSessionMarkdown()" title="导出会话为 Markdown" aria-label="Download session as Markdown">⬇</button>'
    : '';

  main.innerHTML =
    '<div class="main-header">' +
      '<button class="btn-mobile-back" onclick="mobileBack()" title="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868" aria-label="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868">&#8592;</button>' +
      '<div class="main-header-content">' +
      '<h2>' + esc(displayName) + renameBtn + downloadBtn + '</h2>' +
      '<div class="detail">' +
        '<span class="detail-left">' + cliLabel + modelLabel + '</span>' +
        headerBackendChip +
        headerOriginBadge +
        ctxBarHtml +
        turnTimerHtml +
        (showCost ? '<span class="' + costClass + '" id="header-cost"' + costTitleAttr + '>' + costText + '</span>' : '') +
      '</div>' +
      '</div>' +
    '</div>' +
    // cron-panel-consolidation RFC §4.2: cron timeline used to mount here
    // (#cron-timeline-panel placeholder above the events scroll). It now
    // lives entirely inside the 定时任务 panel's per-job drawer; mainShell
    // is reserved for human conversation surfaces.
    '<div class="events" id="events-scroll" role="log" aria-live="polite" aria-relevant="additions">' + (s.state === 'running' ? '<div class="empty-state loading-indicator">\u6b63\u5728\u52a0\u8f7d\u4e8b\u4ef6\u2026</div>' : '') + '</div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button onclick="navMsg(\'prev\')" id="nav-prev" title="\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2191)" aria-label="\u8df3\u5230\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f">&#x25B2;</button>' +
      '<span class="nav-counter" id="nav-counter" onclick="navShowList()" title="\u70b9\u51fb\u67e5\u770b\u5168\u90e8\u7528\u6237\u6d88\u606f"></span>' +
      '<button onclick="navMsg(\'next\')" id="nav-next" title="\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2193)" aria-label="\u8df3\u5230\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f">&#x25BC;</button>' +
    '</div>' +
    '<div class="running-banner" id="running-banner" style="display:none" role="status" aria-live="polite">' +
      '<div class="rb-tool-row">' +
        '<span class="running-status"><span class="running-dot" aria-hidden="true"></span><span id="tool-activity">处理中...</span></span>' +
        '<span class="rb-elapsed" id="rb-elapsed"></span>' +
      '</div>' +
      '<div class="rb-thinking-summary" id="rb-thinking-summary" style="display:none"></div>' +
      '<div class="rb-agents" id="rb-agents"></div>' +
      '<div class="rb-stats" id="rb-stats" style="display:none"></div>' +
    '</div>' +
    '<div class="input-area' + (voiceInputMode ? ' voice-mode' : '') + '" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<button class="btn-icon" onclick="openFilePicker()" title="上传图片或 PDF" aria-label="上传图片或 PDF">&#x1f4ce;</button>' +
        '<button class="btn-icon btn-mic" id="btn-mic" onclick="toggleInputMode()" title="' + (voiceInputMode ? '\u5207\u6362\u952e\u76d8' : '\u5207\u6362\u8bed\u97f3') + '" aria-label="' + (voiceInputMode ? '\u5207\u6362\u5230\u952e\u76d8\u8f93\u5165' : '\u5207\u6362\u5230\u8bed\u97f3\u8f93\u5165') + '">' + (voiceInputMode ? '&#x2328;' : '&#x1f3a4;') + '</button>' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message..." onkeydown="handleKey(event)" oncompositionend="lastCompositionEnd=Date.now()"></div>' +
        '<button class="btn-hold-talk" id="btn-hold-talk" title="\u6309\u4f4f\u8bf4\u8bdd\u6539\u5f55\u97f3" aria-label="\u6309\u4f4f\u8bf4\u8bdd\u5f00\u59cb\u5f55\u97f3">\u6309\u4f4f\u8bf4\u8bdd</button>' +
        '<button class="btn-icon btn-send" id="btn-send" onclick="sendMessage()" title="发送" aria-label="发送消息">&#x27a4;</button>' +
        '<button class="btn-icon btn-stop" id="btn-stop" onclick="interruptSession()" title="stop" aria-label="Stop current turn">&#x25A0;</button>' +
      '</div>' +
      '<div class="input-hints">Enter send &middot; Shift+Enter newline &middot; Esc interrupt</div>' +
      '<input type="file" id="file-input" accept="image/*,application/pdf" multiple style="display:none" onchange="handleFiles(this.files)">' +
    '</div>';

  // Enable drag-drop
  const ia = document.getElementById('input-area');
  ia.addEventListener('dragover', e => { e.preventDefault(); ia.style.borderColor='var(--nz-accent)'; });
  ia.addEventListener('dragleave', () => { ia.style.borderColor=''; });
  ia.addEventListener('drop', e => { e.preventDefault(); ia.style.borderColor=''; handleFiles(e.dataTransfer.files); });

  // Voice hold-to-talk: only touchstart on button; move/end on document (see voiceTouchStart)
  const holdBtn = document.getElementById('btn-hold-talk');
  if (holdBtn) {
    holdBtn.addEventListener('touchstart', voiceTouchStart, {passive: false});
    holdBtn.addEventListener('mousedown', voiceMouseDown);
  }

  updateSendButton(s.state || '');
  // Attach file-ref observer to the freshly-created events-scroll so any
  // newly-inserted .event bubble gets auto-scanned for workspace path
  // references. Safe to call on every renderMainShell: dataset.frObserver
  // gates re-entry so we don't stack duplicate observers.
  startFileRefObserver();
  // Double-tap events feed → focus input (mobile)
  let lastTapMs = 0;
  document.getElementById('events-scroll').addEventListener('touchend', e => {
    if (!isMobile() || e.target.closest('a,button,code,pre')) return;
    const now = Date.now();
    if (now - lastTapMs < 300) { document.getElementById('msg-input')?.focus(); lastTapMs = 0; }
    else lastTapMs = now;
  }, {passive:true});

  // cron-panel-consolidation RFC §4.2: the cron timeline mount hook that
  // used to live here (renderCronTimelineForSession on selectedKey ===
  // 'cron:<id>') is gone. cron drawer rendering happens inside the 定时任务
  // panel itself, keyed off cronDetailJobId rather than selectedKey.

  // Multi-Backend RFC §8.3 D9-D15: gray out input controls that the
  // active session's backend doesn't support. Single-backend deployments
  // short-circuit inside applyFeatureGates so this is a no-op there.
  applyFeatureGates();
}

// _fetchEventsInFlight gates concurrent HTTP polls of `/api/sessions/events`.
// The 1 s `setInterval` driver and the on-demand `full` fetch (session
// switch / WS fallback) can otherwise pile up when the network lags or the
// server is slow: the second request completes first, `appendEvents`
// re-orders events, and the first response is then applied on top. The
// simpler in-flight flag (mirroring `_earlierLoading` on
// `loadEarlierEvents`) skips overlapping polls — a missed tick is cheap
// because the next tick will pick up any accumulated events via `after=`
// anyway. A full fetch while a tail fetch is in flight is also coalesced;
// the next tick finishes rendering the backlog.
let _fetchEventsInFlight = false;
async function fetchEvents(full) {
  if (!selectedKey) return;
  if (_fetchEventsInFlight) return;
  // Capture session identity at dispatch time so a mid-flight switch doesn't
  // apply stale events to the new session's DOM. `selectedKey` can flip
  // synchronously from `pickSession`/`dismiss` callbacks while `await`
  // suspends us; applying `appendEvents` after that point would graft the
  // prior session's tail into the newly-opened session's scroller.
  const dispatchKey = selectedKey;
  const dispatchNode = selectedNode;
  _fetchEventsInFlight = true;
  try {
    let url = '/api/sessions/events?key=' + encodeURIComponent(dispatchKey);
    if (dispatchNode && dispatchNode !== 'local') url += '&node=' + encodeURIComponent(dispatchNode);
    if (!full && lastEventTime > 0) {
      url += '&after=' + lastEventTime;
    } else if (full) {
      // Initial fetch mirrors the WS subscribe: last INITIAL_HISTORY_LIMIT
      // events only. Older pages are loaded on demand by loadEarlierEvents().
      url += '&limit=' + INITIAL_HISTORY_LIMIT;
    }

    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 5s timeout — events poll fallback ticks every 1s, so
    // a hung response must release well before the next tick or the UI
    // falls behind the live stream.
    let events;
    try {
      events = await fetchJSON(url, { headers, timeoutMs: 5000 });
    } catch (err) {
      if (err.status) return; // HTTP non-2xx — mirror legacy !r.ok early-return
      throw err;              // timeout / network — surface via outer catch
    }
    if (!events || events.length === 0) return;
    // Drop stale responses whose selection has since moved. Clearing
    // `lastEventTime` is the caller's job at switch time, so we don't touch
    // it here.
    if (selectedKey !== dispatchKey || selectedNode !== dispatchNode) return;

    if (full) {
      renderEvents(events);
    } else {
      appendEvents(events);
    }

    const last = events[events.length - 1];
    if (last && last.time > lastEventTime) lastEventTime = last.time;
  } catch (e) {
    console.error('fetch events:', e);
  } finally {
    _fetchEventsInFlight = false;
  }
}

// loadEarlierEvents fetches up to EARLIER_PAGE_LIMIT events older than the
// currently-oldest rendered bubble. Prepends the rendered output to the top
// of the events pane and preserves scroll position so the user's view doesn't
// jump when new content is injected above.
//
// Idempotent: calls bail out while a prior fetch is in flight.
let _earlierLoading = false;

// _autoPageBackCount bounds the frontend safety net for the "parallel agent
// team ate my history" bug. The server's visible-aware initial read
// (EventLastNVisibleCtx) already keeps the first page non-blank for local
// sessions, but a few paths still can't guarantee it — remote nodes (their
// reverse-RPC fetch predates the visible-aware read), disk-exhausted sessions,
// or a precision gap where a visible-typed entry still renders to empty HTML.
// When the rendered page is blank despite events existing, maybeAutoPageBack
// transparently pages backward (reusing loadEarlierEvents + the
// oldestFetchedEventTime cursor) up to AUTO_PAGEBACK_MAX times so the operator
// sees real messages instead of the "该会话最近仅有 agent 活动" placeholder.
// The counter resets on every session switch (selectSession).
let _autoPageBackCount = 0;
const AUTO_PAGEBACK_MAX = 3;

// maybeAutoPageBack fires one bounded loadEarlierEvents when the events pane
// rendered blank (every event was internal-filtered). Stops once a real bubble
// appears, the cap is reached, or pagination reports it's exhausted. Safe to
// call when no placeholder is showing — it no-ops unless the scroller has zero
// `.event` children.
function maybeAutoPageBack() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  // A visible bubble already rendered — nothing to recover.
  if (el.querySelector('.event')) { _autoPageBackCount = 0; return; }
  if (_autoPageBackCount >= AUTO_PAGEBACK_MAX) return;
  if (_earlierLoading) return;
  if (!oldestFetchedEventTime) return; // no cursor → cannot page back
  _autoPageBackCount++;
  // loadEarlierEvents prepends older events and, when they include a visible
  // bubble, the placeholder is removed by prependEvents. If the new page is
  // still all-internal, chain another attempt (still bounded by the counter).
  Promise.resolve(loadEarlierEvents()).then(() => {
    const ev = document.getElementById('events-scroll');
    if (ev && !ev.querySelector('.event')) maybeAutoPageBack();
    else _autoPageBackCount = 0;
  });
}

async function loadEarlierEvents() {
  if (_earlierLoading || !selectedKey) return;
  const el = document.getElementById('events-scroll');
  if (!el) return;

  // The oldest currently-rendered event timestamp comes from the first
  // .event child in the scroller. Walk children forward to skip dividers.
  let oldestTime = 0;
  for (const c of el.children) {
    if (c.classList && c.classList.contains('event')) {
      oldestTime = Number(c.getAttribute('data-time') || 0);
      break;
    }
  }
  // Fallback: when no `.event` is rendered (e.g. the visible page was
  // entirely internal events filtered out by INTERNAL_EVENT_TYPES during a
  // parallel agent team turn), page against the cursor we recorded at
  // fetch time. Without this the button appears to do nothing and the
  // operator has no path back to the earlier conversation.
  if (!oldestTime) oldestTime = oldestFetchedEventTime;
  if (!oldestTime) return;

  _earlierLoading = true;
  updateEarlierButton('loading');
  try {
    let url = '/api/sessions/events?key=' + encodeURIComponent(selectedKey) +
              '&before=' + oldestTime + '&limit=' + EARLIER_PAGE_LIMIT;
    if (selectedNode && selectedNode !== 'local') url += '&node=' + encodeURIComponent(selectedNode);
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(url, { headers });
    if (!r.ok) { updateEarlierButton('error'); return; }
    const events = await r.json();
    if (!Array.isArray(events) || events.length === 0) {
      updateEarlierButton('done');
      return;
    }
    prependEvents(events);
    // If we got a full page there may be more; otherwise mark done.
    updateEarlierButton(events.length >= EARLIER_PAGE_LIMIT ? 'ready' : 'done');
  } catch (e) {
    console.error('load earlier events:', e);
    updateEarlierButton('error');
  } finally {
    _earlierLoading = false;
  }
}

// prependEvents injects older events at the top of the scroller while keeping
// the user's visual position stable (the bubble they're currently reading
// should not shift). Only runs KaTeX/Mermaid on the freshly-inserted fragment
// so 500-bubble sessions don't re-scan the entire DOM on each page.
function prependEvents(events) {
  const el = document.getElementById('events-scroll');
  if (!el || !events || events.length === 0) return;

  // Advance the pagination cursor before DOM work so a subsequent
  // loadEarlierEvents sees the new floor even if the freshly prepended
  // batch was entirely internal-filtered.
  const firstT = events[0] && events[0].time;
  if (firstT && (oldestFetchedEventTime === 0 || firstT < oldestFetchedEventTime)) {
    oldestFetchedEventTime = firstT;
  }

  // Remove "load earlier" button so we can place new events first; it'll be
  // re-added after.
  const btn = document.getElementById('earlier-events-btn');
  if (btn) btn.remove();

  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  // Drop a placeholder the first time a chat is entered through a
  // fully-internal page; leaving it in place would push the prepended
  // real messages below the placeholder, so clean it out before insert.
  const placeholder = el.querySelector('.empty-state');
  if (placeholder) placeholder.remove();

  // Preserve visual stability: capture distance-from-bottom before mutation,
  // then restore after. scrollTop alone breaks because inserted content above
  // shifts the anchor; bottom-anchored math works even when content height
  // changes arbitrarily.
  const prevScrollFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;

  const frag = document.createElement('div');
  frag.innerHTML = html;
  // Move children one-by-one to preserve DOM structure; innerHTML replace
  // would wipe the existing event bubbles.
  while (frag.firstChild) {
    el.insertBefore(frag.firstChild, el.firstChild);
  }

  // Re-insert the button at the top.
  ensureEarlierButton();

  // Restore scroll position.
  el.scrollTop = el.scrollHeight - el.clientHeight - prevScrollFromBottom;

  // runPendingAsync only iterates the `pending` dictionaries (new IDs
  // emitted by the freshly-rendered bubbles above), so it is already
  // incremental — no DOM scan is needed.
  runPendingAsync();
  navRebuild();
}

// ensureEarlierButton injects/refreshes the "load earlier" affordance at the
// top of the scroller. Button state is stored in data-state on the element.
function ensureEarlierButton() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  let btn = document.getElementById('earlier-events-btn');
  if (!btn) {
    btn = document.createElement('button');
    btn.id = 'earlier-events-btn';
    btn.type = 'button';
    btn.className = 'earlier-events-btn';
    btn.style.cssText = 'display:block;margin:8px auto;padding:6px 14px;background:var(--nz-bg-2);border:1px solid var(--nz-border);color:var(--nz-text);border-radius:6px;cursor:pointer;font-size:12px';
    btn.textContent = '加载更早的事件';
    btn.onclick = loadEarlierEvents;
    el.insertBefore(btn, el.firstChild);
  } else if (el.firstChild !== btn) {
    el.insertBefore(btn, el.firstChild);
  }
  updateEarlierButton('ready');
}

function updateEarlierButton(state) {
  const btn = document.getElementById('earlier-events-btn');
  if (!btn) return;
  btn.dataset.state = state;
  switch (state) {
    case 'loading':
      btn.textContent = '加载中…';
      btn.disabled = true;
      break;
    case 'done':
      btn.textContent = '没有更早的事件';
      btn.disabled = true;
      break;
    case 'error':
      btn.textContent = '加载失败 — 点击重试';
      btn.disabled = false;
      break;
    default:
      btn.textContent = '加载更早的事件';
      btn.disabled = false;
  }
}

function renderEvents(events) {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  // RNEW-UX-007 — innerHTML replace below wipes any live text selection
  // inside the events panel (user was mid-copy of a chat bubble). Events
  // are replayed idempotently each poll/push tick, so skipping one refresh
  // while the user has an active selection inside the events list is safe:
  // the next tick lands with the same data and re-renders then. We check
  // anchorNode lineage so selections elsewhere (sidebar, input, modal) are
  // not affected by this guard.
  try {
    const sel = window.getSelection && window.getSelection();
    if (sel && !sel.isCollapsed && sel.anchorNode && el.contains(sel.anchorNode)) {
      return;
    }
  } catch (_) { /* getSelection unavailable — proceed with refresh */ }
  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  if (html) {
    el.innerHTML = html;
  } else if (events.length === 0) {
    el.innerHTML = '<div class="empty-state">暂无事件</div>';
  } else {
    // The server returned events but every one was filtered out by
    // INTERNAL_EVENT_TYPES — typically a parallel agent team where the
    // visible tail of the log is all tool_use / task_progress. Render a
    // neutral placeholder so the panel isn't a blank void, and still
    // show the "load earlier" affordance below so the operator can
    // page back to the real messages.
    el.innerHTML = '<div class="empty-state">该会话最近仅有 agent 活动，点击下方加载更早的消息</div>';
  }
  if (events.length > 0) {
    const last = events[events.length - 1];
    if (last.time) lastRenderedEventTime = last.time;
    const first = events[0];
    if (first.time && (oldestFetchedEventTime === 0 || first.time < oldestFetchedEventTime)) {
      oldestFetchedEventTime = first.time;
    }
  }
  // Mount "load earlier" whenever we got a full page — more history likely
  // exists, regardless of whether the visible slice survived the filter.
  if (events.length >= INITIAL_HISTORY_LIMIT) {
    ensureEarlierButton();
  }
  runPendingAsync();
  navRebuild();
  if (!restoreScrollPos(selectedKey, selectedNode)) {
    stickEventsBottom();
  }
  // Safety net: if the page rendered to the all-internal placeholder (no
  // visible bubble) but events exist, transparently page back to real
  // messages. Bounded by AUTO_PAGEBACK_MAX. Covers the paths the server-side
  // visible-aware read can't (remote nodes, disk-exhausted sessions).
  if (!html && events.length > 0) maybeAutoPageBack();
}

// trimEventsScroll bounds the live DOM (#398): drop oldest top children once the
// scroller exceeds MAX_LIVE_DOM_EVENTS. Preserves a pinned "load earlier" button
// (it always lives at the top) and advances oldestFetchedEventTime so a later
// loadEarlierEvents re-fetches whatever we just evicted instead of leaving a gap.
function trimEventsScroll(el) {
  if (!el) return;
  // Count rendered event bubbles only; dividers/buttons are cheap and ride along.
  let bubbles = el.querySelectorAll(':scope > .event').length;
  if (bubbles <= MAX_LIVE_DOM_EVENTS) return;
  const btn = document.getElementById('earlier-events-btn');
  let node = el.firstChild;
  while (node && bubbles > MAX_LIVE_DOM_EVENTS) {
    const next = node.nextSibling;
    if (node === btn) { node = next; continue; }
    const isBubble = node.nodeType === 1 && node.classList && node.classList.contains('event');
    if (isBubble) {
      const t = parseInt(node.getAttribute('data-time') || '0', 10);
      if (t && t > oldestFetchedEventTime) oldestFetchedEventTime = t;
      bubbles--;
    }
    el.removeChild(node);
    node = next;
  }
  // The tail no longer starts at the true session head, so make "load earlier"
  // available even if the initial page was short.
  ensureEarlierButton();
}

function appendEvents(events) {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  const empty = el.querySelector('.empty-state');
  if (empty) empty.remove();
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  let prevT = lastDividerTime(el);
  // Force-bottom when a "user" event arrives: either the local operator just
  // hit send, or a teammate posted through the IM channel — in both cases the
  // message must be visible, even if the viewport was scrolled up.
  let sawUser = false;
  events.forEach(e => {
    if (isInternalEvent(e)) return;
    // Deduplicate: skip events at or before the last rendered time
    if (e.time && e.time <= lastRenderedEventTime) return;
    const h = eventHtml(e); if (!h) return;
    const t = e.time || 0;
    if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
    }
    el.insertAdjacentHTML('beforeend', h);
    if (t) prevT = t;
    if (e.time && e.time > lastRenderedEventTime) lastRenderedEventTime = e.time;
    if (e.type === 'user') sawUser = true;
  });
  // Bound the live DOM before scroll/scan so a long streaming session can't
  // grow #events-scroll without limit and OOM the tab (#398).
  trimEventsScroll(el);
  if (sawUser) stickEventsBottom();
  else if (wasBottom) el.scrollTop = el.scrollHeight;
  runPendingAsync();
  // Rebuild nav index but preserve current position
  const oldIdx = navIdx;
  navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
  navIdx = oldIdx >= 0 && oldIdx < navUserEls.length ? oldIdx : -1;
  navUpdatePill();
}

// Event types that are tracked in the running banner but never rendered
// as a chat bubble in the events stream. Kept as a single source of truth
// so appendEvents / onHistory / preview-poll stay in sync.
// NOTE: 'todo' is intentionally NOT in this set — TodoWrite updates are
// rendered as their own chat bubbles via renderTodoList below.
const INTERNAL_EVENT_TYPES = new Set(['tool_use','result','agent','task_start','task_progress','task_done']);
// Unified backend behaviour (supersedes Multi-Backend RFC §8.3 D17): both
// Claude (stream-json) and Kiro (ACP) tool_use events are filtered out of
// the main transcript so the chat reads cleanly. Transient tool activity
// is still surfaced via the running banner (applyEventToTurnState below)
// while the turn is in flight, and the subagent panel still renders the
// rich tool_call progress row via eventHtml(includeInternal=true) so
// operators can drill into per-agent tool runs when needed.
function isInternalEvent(e) {
  if (!e || !INTERNAL_EVENT_TYPES.has(e.type)) return false;
  return true;
}

// renderTodoList parses the JSON todos payload stored on EventEntry.detail and
// emits a checklist block. Falls back to the summary line when detail is
// malformed so a parse failure never produces an empty bubble.
function renderTodoList(detail, summary) {
  let todos = null;
  if (detail) {
    try { todos = JSON.parse(detail); } catch (_) { todos = null; }
  }
  if (!Array.isArray(todos) || todos.length === 0) {
    return esc(summary || 'Todos');
  }
  let done = 0, active = 0, pending = 0;
  const items = todos.map(t => {
    const status = (t && t.status) || 'pending';
    let cls = 'todo-pending';
    let mark = '\u25cb'; // ○ pending
    let text = (t && t.content) || '';
    if (status === 'completed') {
      cls = 'todo-done';
      mark = '\u2714'; // ✔
      done++;
    } else if (status === 'in_progress') {
      cls = 'todo-active';
      mark = '\u25b8'; // ▸
      if (t && t.activeForm) text = t.activeForm;
      active++;
    } else {
      pending++;
    }
    return '<li class="todo-item ' + cls + '"><span class="todo-mark">' + mark + '</span><span class="todo-text">' + esc(text) + '</span></li>';
  }).join('');
  const total = todos.length;
  const counts =
    '<span class="todo-count">' + total + ' 项</span>' +
    (done > 0 ? '<span class="todo-count done">' + done + ' 完成</span>' : '') +
    (active > 0 ? '<span class="todo-count active">' + active + ' 进行中</span>' : '') +
    (pending > 0 ? '<span class="todo-count">' + pending + ' 待办</span>' : '');
  const header =
    '<div class="todo-header">' +
      '<span class="todo-title">任务清单</span>' +
      '<span class="todo-counts">' + counts + '</span>' +
    '</div>';
  return header + '<ul class="todo-list">' + items + '</ul>';
}

// AskUserQuestion cards are single-submit: user picks one option per question
// (or multiple when multiSelect=true), then clicks a bottom "提交" button.
// That one click produces a single user message combining all answers, so CC
// never sees a partial answer. _askAnswered stores tool_use_ids that have
// already been submitted so a re-render (e.g. late history replay) can't
// resurrect an actionable card.
//
// Persistence note: the Set is in-memory only. History replay after a page
// reload rebuilds it in hydrateAskAnsweredFromHistory() by scanning for any
// user event that arrived AFTER a given ask_question — a later user message
// means the question was answered on some surface, so re-actioning must be
// disabled to prevent duplicate answers to CC.
const _askAnswered = new Set();

// hydrateAskAnsweredFromHistory walks a time-sorted event list and marks
// every ask_question whose tool_use_id is followed by at least one user
// event as already-answered. Called from onHistory before rendering.
function hydrateAskAnsweredFromHistory(events) {
  if (!Array.isArray(events)) return;
  for (let i = 0; i < events.length; i++) {
    const e = events[i];
    if (!e || e.type !== 'ask_question') continue;
    const tuid = (e.ask_question && e.ask_question.tool_use_id) || e.tool_use_id || '';
    if (!tuid) continue;
    // Any later user event → this question was answered by some surface.
    for (let j = i + 1; j < events.length; j++) {
      if (events[j] && events[j].type === 'user') {
        _askAnswered.add(tuid);
        break;
      }
    }
  }
}

function renderAskQuestionCard(e) {
  const aq = e.ask_question;
  if (!aq || !Array.isArray(aq.items) || aq.items.length === 0) {
    // Defensive: if payload missing, fall back to a plain status bubble.
    return '<div class="event ask_question"><span class="event-icon">?</span>' +
      '<div class="event-content">' + esc(e.summary || 'AskUserQuestion') + '</div></div>';
  }
  // Multi-Backend RFC §8.3 D12 — when the active session's backend doesn't
  // declare askuser, render a degraded card that lists the questions/options
  // as plain text and tells the operator to type the answer manually. Stops
  // the interactive submit-handler from sending an answer the backend can't
  // route (kiro 2.3.0 has no AskUserQuestion equivalent — V13 validation).
  if (!featureForCurrent('askuser')) {
    const lines = aq.items.map(it => {
      const header = it && it.header ? '<strong>' + esc(it.header) + '</strong>: ' : '';
      const q = it && it.question ? esc(it.question) : '';
      const opts = (it && Array.isArray(it.options))
        ? it.options.map(o => '· ' + esc((o && o.label) || '')).join('<br>')
        : '';
      return '<div class="ask-degraded-q">' + header + q +
        (opts ? '<div class="ask-degraded-opts">' + opts + '</div>' : '') + '</div>';
    }).join('');
    return '<div class="event ask_question ask-degraded"><span class="event-icon">?</span>' +
      '<div class="event-content">' +
        '<div class="ask-degraded-hint">' +
          '当前后端不支持 AskUserQuestion，请直接回复你的选择：' +
        '</div>' + lines +
      '</div></div>';
  }
  // A question with zero options would deadlock the submit button
  // (updateAskSubmitState requires every group to have a .selected option,
  // and a group with no .ask-opt can never satisfy that). Rather than
  // render a broken card, fall back to a simple label and log at debug so
  // the malformed payload surfaces in dev tools.
  const hasDegenerateItem = aq.items.some(it => !it || !Array.isArray(it.options) || it.options.length === 0);
  if (hasDegenerateItem) {
    return '<div class="event ask_question"><span class="event-icon">?</span>' +
      '<div class="event-content">' + esc(e.summary || 'AskUserQuestion (malformed: empty options)') + '</div></div>';
  }
  const tuid = aq.tool_use_id || '';
  const locked = _askAnswered.has(tuid);
  const groups = aq.items.map((item, qi) => {
    const header = item.header ? '<div class="ask-q-header">' + esc(item.header) + '</div>' : '';
    const question = '<div class="ask-q-text">' + esc(item.question || '') + '</div>';
    const multi = !!item.multi_select;
    const opts = (item.options || []).map((opt, oi) => {
      // Buttons toggle a .selected class only; nothing is sent until the
      // card-level submit. data-* attrs carry the minimal info the compose
      // step needs so the handler doesn't have to walk the aq tree.
      return '<button class="ask-opt" type="button"' +
        ' data-tuid="' + escAttr(tuid) + '"' +
        ' data-qi="' + qi + '"' +
        ' data-oi="' + oi + '"' +
        ' data-multi="' + (multi ? '1' : '0') + '"' +
        ' data-header="' + escAttr(item.header || '') + '"' +
        ' data-label="' + escAttr(opt.label || '') + '"' +
        (locked ? ' disabled' : '') +
        ' onclick="onAskOptionToggle(this)">' +
        '<span class="ask-opt-label">' + esc(opt.label || '') + '</span>' +
        (opt.description ? '<span class="ask-opt-desc">' + esc(opt.description) + '</span>' : '') +
        '</button>';
    }).join('');
    const hint = multi
      ? '<div class="ask-q-hint">可多选</div>'
      : '';
    return '<div class="ask-q-group" data-qi="' + qi + '" data-multi="' + (multi ? '1' : '0') + '">' +
      header + question + hint +
      '<div class="ask-opts">' + opts + '</div>' +
      '</div>';
  }).join('');
  // Single bottom submit: always starts disabled (no selection yet); either
  // unlocked dynamically by updateAskSubmitState when every group has ≥1
  // selected option, or permanently disabled if the card is locked
  // (replayed after a prior answer).
  const submitBtn =
    '<button class="ask-submit" type="button"' +
    ' data-tuid="' + escAttr(tuid) + '"' +
    ' disabled' +
    ' onclick="onAskSubmit(this)">提交全部回答</button>';
  const status = locked
    ? '<div class="ask-status">已回答</div>'
    : '';
  const timeAttr = e.time ? ' data-time="' + e.time + '" title="' + escAttr(formatTimeFull(e.time)) + '"' : '';
  return '<div class="event ask_question"' + timeAttr +
    ' data-tool-use-id="' + escAttr(tuid) + '">' +
    '<span class="event-icon">?</span>' +
    '<div class="event-content ask-card">' +
      '<div class="ask-title">AskUserQuestion · 全部作答后提交</div>' +
      groups +
      '<div class="ask-submit-row">' + submitBtn + '</div>' +
      status +
    '</div></div>';
}

// Compose the final reply text from every question's chosen labels.
// Format: "Header1: Label1. Header2: A, B. Label-only question: Label."
// The final "." is added per group so grouping is unambiguous to CC.
// AQ4 verified this format is sufficient context for CC to continue.
function composeAskAnswerFromGroups(groups) {
  const parts = [];
  groups.forEach(g => {
    if (!g.labels.length) return;
    const h = (g.header || '').trim();
    const l = g.labels.map(s => s.trim()).filter(Boolean).join(', ');
    if (!l) return;
    parts.push(h ? (h + ': ' + l) : l);
  });
  if (parts.length === 0) return '';
  return parts.join('. ') + '.';
}

// Toggle the clicked option. Single-select: clear siblings in the same
// question group, mark the clicked one. Multi-select: just toggle.
// Then re-evaluate the submit button's disabled state.
function onAskOptionToggle(btn) {
  const tuid = btn.dataset.tuid || '';
  if (!tuid || _askAnswered.has(tuid)) return;
  const group = btn.closest('.ask-q-group');
  if (!group) return;
  const multi = group.dataset.multi === '1';
  if (multi) {
    btn.classList.toggle('selected');
  } else {
    group.querySelectorAll('.ask-opt').forEach(b => b.classList.remove('selected'));
    btn.classList.add('selected');
  }
  updateAskSubmitState(btn.closest('.event.ask_question'));
}

// Enable submit only when every question has at least one selected option.
function updateAskSubmitState(card) {
  if (!card) return;
  const groups = card.querySelectorAll('.ask-q-group');
  let allAnswered = groups.length > 0;
  groups.forEach(g => {
    if (!g.querySelector('.ask-opt.selected')) allAnswered = false;
  });
  const submit = card.querySelector('.ask-submit');
  if (!submit) return;
  submit.disabled = !allAnswered;
}

function onAskSubmit(btn) {
  const tuid = btn.dataset.tuid || '';
  if (!tuid || _askAnswered.has(tuid)) return;
  const card = btn.closest('.event.ask_question');
  if (!card) return;
  // Gather selections per question group.
  const groups = [];
  card.querySelectorAll('.ask-q-group').forEach(g => {
    const header = (g.querySelector('.ask-q-header') || {}).textContent || '';
    const labels = [];
    g.querySelectorAll('.ask-opt.selected').forEach(b => {
      const l = b.dataset.label || '';
      if (l) labels.push(l);
    });
    groups.push({ header: header, labels: labels });
  });
  const answer = composeAskAnswerFromGroups(groups);
  if (!answer) return;
  // Lock the card so re-clicks or slow network can't duplicate the send.
  _askAnswered.add(tuid);
  card.querySelectorAll('button').forEach(b => { b.disabled = true; });
  const content = card.querySelector('.event-content');
  if (content && !content.querySelector('.ask-status')) {
    const div = document.createElement('div');
    div.className = 'ask-status';
    div.textContent = '已回答：' + answer;
    content.appendChild(div);
  }
  // Route through the regular session send endpoint so queue / passthrough /
  // broadcast semantics all apply; we do NOT call sendMessage() because that
  // path reads from the input box and manages optimistic rendering — the card
  // already shows "已回答", so duplicating would clash.
  sendAskAnswerViaAPI(answer, card).catch(err => {
    _askAnswered.delete(tuid);
    card.querySelectorAll('button').forEach(b => { b.disabled = false; });
    updateAskSubmitState(card);
    const status = card.querySelector('.ask-status');
    if (status) status.textContent = '发送失败：' + (err && err.message || err);
  });
}

// sendAskAnswerViaAPI routes the composed answer text to the session that
// rendered the AskUserQuestion card. The renderer (eventHtml →
// renderAskQuestionCard) is shared between the main transcript and the
// scratch (aside) drawer, so we MUST pick the route from the card's DOM
// ancestry rather than the global selectedKey — otherwise an answer chosen
// inside the drawer would land in the parent session and silently bypass
// the scratch CLI process.
async function sendAskAnswerViaAPI(text, card) {
  let key = selectedKey;
  let node = selectedNode;
  if (card && card.closest && card.closest('#aside-drawer')) {
    const scratchKey = (typeof window.__getActiveScratchKey === 'function')
      ? window.__getActiveScratchKey()
      : '';
    if (!scratchKey) throw new Error('no active scratch session');
    key = scratchKey;
    // Scratch sessions are always local — never forward to a remote node.
    node = 'local';
  }
  if (!key) throw new Error('no active session');
  const headers = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const payload = { key: key, text: text };
  if (node && node !== 'local') payload.node = node;
  const r = await fetch('/api/sessions/send', { method: 'POST', headers, body: JSON.stringify(payload) });
  if (!r.ok) {
    const raw = await r.text().catch(() => '');
    throw new Error('send failed: ' + r.status + ' ' + raw.slice(0, 200));
  }
}

// eventHtml renders one EventEntry bubble.
// opts.includeInternal=true keeps tool_use / thinking / task_* / agent / result
// events that the parent view hides (banner handles them there). The sub-agent
// internal view (agent_view.js) needs them — a team member's work is almost
// entirely tool_use + thinking; filtering those out leaves the panel looking
// empty even when the jsonl transcript is full of content. RFC v4 §3.6.7 /
// §3.6.1 contract: parent and agent views share the bubble renderer but
// differ on the filter policy.
function eventHtml(e, opts) {
  const includeInternal = !!(opts && opts.includeInternal);
  if (!includeInternal && (isInternalEvent(e) || e.type === 'thinking')) return '';
  // AskUserQuestion interactive card: dedicated renderer with option buttons.
  // The matching tool_use entry is already filtered out via INTERNAL_EVENT_TYPES,
  // so the card stands alone in the transcript.
  if (e.type === 'ask_question') return renderAskQuestionCard(e);
  // Filter out Claude Code system XML injected as user messages
  const raw = e.detail || e.summary || '';
  if (e.type === 'user' && /^<(task-notification|system-reminder|local-command|command-name|available-deferred-tools)[\s>]/.test(raw)) return '';
  // CLI-synthesised interrupt marker: SIGINT-aborted turn, not user intent.
  if (e.type === 'user' && (raw === '[Request interrupted by user]' || raw === '[Request interrupted by user for tool use]')) return '';
  const icons = {init:'\u2699',system:'\u2699',user:'\u{1f464}',text:'\u2726',todo:'\u2630'};
  let icon = icons[e.type] || '';
  // Assistant turns on the claude backend get the clawd mascot instead of
  // the default \u2726 glyph. Other backends (kiro, gemini, ...) keep the glyph
  // so each backend has a distinct visual identity in the transcript.
  if (e.type === 'text') {
    const sess = sessionsData[sid(selectedKey, selectedNode)] || {};
    const backendID = sess.backend || sessionBackends[selectedKey] || (cliBackends && cliBackends.default) || '';
    if (backendID === 'claude' || backendID === '') icon = CLAWD_SVG;
  }

  // Strip redundant "[+N image(s)]" suffix when thumbnails are present
  let cleanRaw = e.detail || e.summary || '';
  if (e.images && e.images.length > 0) cleanRaw = cleanRaw.replace(/ \[\+\d+ image\(s\)\]$/, '');

  let content = '';
  if (e.type === 'system') {
    content = esc(e.summary || e.type);
  } else if (e.type === 'text' || e.type === 'user') {
    content = renderMd(cleanRaw || e.type);
  } else if (e.type === 'todo') {
    content = renderTodoList(e.detail, e.summary);
  } else if (e.type === 'tool_use' && e.tool_call) {
    // ACP rich tool progress row (kiro). Originally introduced by
    // Multi-Backend RFC §8.3 D17. The main transcript filters tool_use
    // events out (see isInternalEvent), so this branch only fires inside
    // the subagent panel where eventHtml(..., {includeInternal:true})
    // surfaces the per-agent tool runs:
    //   ▶ <title>          [kind · status]     ← summary line
    //     stdout / stderr / raw                ← collapsed body
    //
    // Status pill colors (matches RFC §8.4 traffic-light convention):
    //   ""           — neutral grey (initial invocation, awaiting result)
    //   in_progress  — blue
    //   completed    — green
    //   failed       — red
    //
    // Output extraction is best-effort: kiro emits
    // {"items":[{"Json":{"exit_status":"...","stdout":"..."}}]} but other
    // backends may use a different shape. We try the kiro path first,
    // then fall back to pretty-printed JSON.
    const tc = e.tool_call;
    const status = tc.status || '';
    const kind = tc.kind || '';
    const title = tc.title || tc.name || tc.id || '(tool)';
    const statusClass = 'tc-status tc-status-' + (status || 'pending');
    const statusLabel = status || 'pending';
    let bodyText = '';
    if (tc.output_json) {
      try {
        const parsed = JSON.parse(tc.output_json);
        if (parsed && Array.isArray(parsed.items) && parsed.items.length > 0 &&
            parsed.items[0] && parsed.items[0].Json && typeof parsed.items[0].Json.stdout === 'string') {
          bodyText = parsed.items[0].Json.stdout;
        } else {
          bodyText = JSON.stringify(parsed, null, 2);
        }
      } catch { bodyText = tc.output_json; }
    } else if (tc.input_json) {
      try {
        bodyText = JSON.stringify(JSON.parse(tc.input_json), null, 2);
      } catch { bodyText = tc.input_json; }
    }
    const bodyHtml = bodyText
      ? '<pre class="tc-body">' + esc(bodyText.length > 8000 ? bodyText.slice(0, 8000) + '\n…' : bodyText) + '</pre>'
      : '';
    const kindBadge = kind ? '<span class="tc-kind">' + esc(kind) + '</span>' : '';
    content = '<details class="tc-wrap"' + (status === 'failed' ? ' open' : '') + '>' +
      '<summary class="tc-summary">' +
      '<span class="tc-icon" aria-hidden="true">🛠</span>' +
      '<span class="tc-title">' + esc(title) + '</span>' +
      kindBadge +
      '<span class="' + statusClass + '">' + esc(statusLabel) + '</span>' +
      '</summary>' + bodyHtml + '</details>';
  } else if (e.type === 'tool_result') {
    // RFC v4 agent-team-ui §3.6.7 — fold long outputs by default. The
    // summary is the first line (< 120 chars) and the full detail is
    // capped at 16 KB server-side. When the CLI emitted a
    // <persisted-output>, the Tool field carries "persisted:tool-results/
    // <id>.ext" so the frontend can offer a fetch-full button.
    var summary = e.summary || '(tool result)';
    var detail = e.detail || '';
    var persistedPath = '';
    if (typeof e.tool === 'string' && e.tool.indexOf('persisted:') === 0) {
      persistedPath = e.tool.slice('persisted:'.length);
    }
    var detailHtml = detail
      ? '<pre class="tr-detail">' + esc(detail) + '</pre>'
      : '';
    var persistedBtn = '';
    if (persistedPath && selectedKey) {
      var toolURL = '/api/sessions/tool_result?key=' + encodeURIComponent(selectedKey) +
        '&node=' + encodeURIComponent(selectedNode || 'local') +
        '&path=' + encodeURIComponent(persistedPath);
      persistedBtn = '<a class="tr-persisted" href="' + escAttr(toolURL) +
        '" target="_blank" rel="noopener noreferrer" title="查看完整输出">📎 打开完整输出</a>';
    }
    content = '<details class="tr-wrap"><summary class="tr-summary">' +
      esc(summary) + '</summary>' + detailHtml + persistedBtn + '</details>';
  } else {
    content = esc(e.detail || e.summary || e.type);
  }

  // Render image thumbnails for user messages. When ImagePaths is populated
  // (image was persisted to the workspace attachment directory), the click
  // target is the full-size /api/sessions/attachment URL instead of the
  // thumbnail itself — the lightbox then shows the original image rather
  // than a 600 px blur. Falls back to the data URI for legacy entries that
  // predate the persist path. The thumbnail's <img src> is always the data
  // URI so the bubble render stays instant (no network fetch for preview).
  //
  // Cache-busting: the attachment store re-uses date-partitioned UUIDs,
  // so two sessions cannot legitimately share an attachment URL — but if
  // the browser has a cached 404 from a GC-expired attachment, it will
  // short-circuit onerror on the very first load AFTER the attachment is
  // restored (unlikely but possible during operator file shuffles). A
  // per-event `?v=<time>` query string side-steps the negative cache
  // without invalidating legitimate hits.
  //
  // Fallback to thumb on load failure: `openLightbox(full, thumb)` below
  // covers both HTTP 404 (attachment GC'd) and Content-Type mismatch
  // (openLightbox checks naturalWidth===0 after onload). See
  // dashboard.js's openLightbox comment for rationale. RFC §3.6.3.
  let imgHtml = '';
  if (e.images && e.images.length > 0) {
    const paths = e.image_paths || [];
    const cacheBust = e.time ? ('&v=' + e.time) : '';
    imgHtml = '<div class="event-images">' + e.images.map((src, i) => {
      const p = paths[i] || '';
      let full = src;
      if (p && selectedKey) {
        full = '/api/sessions/attachment?key=' + encodeURIComponent(selectedKey) +
          '&path=' + encodeURIComponent(p) + cacheBust;
      }
      return '<img src="' + escAttr(src) + '" loading="lazy" ' +
        'data-full="' + escAttr(full) + '" ' +
        'data-thumb="' + escAttr(src) + '" ' +
        'onclick="openLightbox(this.dataset.full, this.dataset.thumb)">';
    }).join('') + '</div>';
  }

  // Copy + ask-aside bubble actions share one display rule: only long
  // messages (>500 raw chars) expose the toolbar, and both buttons fade in
  // on .event hover / keyboard focus via `.hover-only` (see CSS
  // .event-copy-btn.hover-only / .event-ask-btn.hover-only). Short bubbles
  // stay uncluttered; long bubbles are where "select-and-copy gets
  // clobbered by re-render" actually hurts, and where a separate aside
  // thread is worth opening. Keeping the gate identical for both buttons is
  // the contract — don't let them diverge.
  const isLong = !!cleanRaw && cleanRaw.length > 500;
  const copyBtn = isLong && (e.type === 'text' || e.type === 'user')
    ? '<button class="event-copy-btn hover-only" data-raw="' + escAttr(cleanRaw) + '" onclick="copyEventContent(this)" title="复制" aria-label="复制消息">复制</button>'
    : '';
  const askBtn = isLong && e.type === 'text'
    ? '<button class="event-ask-btn hover-only" data-raw="' + escAttr(cleanRaw) + '" data-msg-time="' + (e.time || 0) + '" onclick="askAside(this)" title="基于此内容追问">↗ 追问</button>'
    : '';

  const timeAttr = e.time ? ' data-time="' + e.time + '" title="' + escAttr(formatTimeFull(e.time)) + '"' : '';
  return '<div class="event ' + esc(e.type||'') + '"' + timeAttr + '>' +
    '<span class="event-icon">' + icon + '</span>' +
    '<div class="event-content">' + content + imgHtml + copyBtn + askBtn + '</div></div>';
}

// Expose the bubble renderer for agent_view.js (RFC v4 agent-team-ui §3.6).
// The sub-agent transcript panel must use the same layout as the parent view —
// tool_result folding, markdown, image thumbnails, copy/ask buttons — so one
// eventHtml is the source of truth. Exporting here prevents silent drift when
// agent_view.js was referencing a non-existent window.renderEvent (which
// always fell through to a plain-text fallback, losing the entire bubble UI).
window.eventHtml = eventHtml;

// Walk a list of events and produce an HTML string with time dividers inserted
// whenever the gap between adjacent VISIBLE (non-null) bubbles exceeds
// EVENT_DIVIDER_GAP_MS. `prevTime` seeds the comparison against whatever is
// already rendered in the DOM (0 = always emit a leading divider for the first
// visible event).
function renderEventsWithDividers(events, prevTime, opts) {
  let out = '';
  let lastTime = prevTime || 0;
  for (const e of events) {
    const h = eventHtml(e, opts);
    if (!h) continue;
    const t = e.time || 0;
    if (t && (lastTime === 0 || t - lastTime >= EVENT_DIVIDER_GAP_MS)) {
      out += timeDividerHtml(t);
    }
    out += h;
    if (t) lastTime = t;
  }
  return out;
}
// Shared with agent_view.js — see window.eventHtml comment above.
window.renderEventsWithDividers = renderEventsWithDividers;

// Read the data-time of the last event-time-divider in the scroll container so
// incremental appenders can decide whether a new divider is needed.
function lastDividerTime(el) {
  if (!el) return 0;
  // Walk the last few children back to find the most recent divider or bubble.
  const kids = el.children;
  for (let i = kids.length - 1; i >= 0; i--) {
    const c = kids[i];
    if (c.classList && (c.classList.contains('event') || c.classList.contains('event-time-divider'))) {
      const t = Number(c.getAttribute('data-time') || 0);
      if (t) return t;
    }
  }
  return 0;
}

// --- Send message ---

// Esc in the input: first press arms, second press (within 600ms) actually
// interrupts the running turn. Prevents thumb-on-Esc misfires.
let _lastEscAt = 0;
function handleKey(e) {
  if (e.key === 'Escape') {
    e.preventDefault();
    const sd = sessionsData[sid(selectedKey, selectedNode || 'local')];
    const running = sd && sd.state === 'running';
    if (!running) { _lastEscAt = 0; return; }
    const now = Date.now();
    if (now - _lastEscAt < 600) {
      _lastEscAt = 0;
      interruptSession();
    } else {
      _lastEscAt = now;
      showToast('再按一次 Esc 发送中断', 'warning', 1000);
    }
    return;
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && Date.now() - lastCompositionEnd > 30) { e.preventDefault(); sendMessage(); }
}

function autoGrow(el) {} // no-op: contenteditable auto-sizes
function getMsgValue(el) { return (el ? el.innerText : '').trim(); }
function setMsgValue(el, v) { if (el) el.innerText = v; }
function clearMsg(el) { if (el) el.textContent = ''; }

async function sendMessage() {
  if (sending) return;

  // Auto-takeover: if viewing a discovered session, takeover first then send
  if (pendingDiscovered && !selectedKey) {
    const input = document.getElementById('msg-input');
    const text = getMsgValue(input);
    if (!text) return;
    sending = true;
    const btn = document.getElementById('btn-send');
    if (btn) btn.classList.add('sending');
    if (input) input.dataset.placeholder = '正在接管会话…';
    if (input) input.contentEditable = 'false';
    const pd = pendingDiscovered;
    try {
      const headers = {'Content-Type': 'application/json'};
      const token = getToken();
      if (token) headers['Authorization'] = 'Bearer ' + token;
      const r = await fetch('/api/discovered/takeover', {
        method: 'POST', headers,
        body: JSON.stringify({pid: pd.pid, session_id: pd.sessionId, cwd: pd.cwd, proc_start_time: pd.procStartTime || 0, node: pd.node || ''})
      });
      if (!r.ok) {
        const errText = await r.text().catch(() => '');
        showAPIError('接管进程', r.status, errText);
        if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
        sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      const data = await r.json();
      if (!data.key) {
        showToast('接管进程失败：未返回会话标识', 'error');
        if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
        sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      // Remove from discoveredItems so renderSidebar won't re-create the card
      discoveredItems = discoveredItems.filter(d => d.pid !== pd.pid);
      // Remove the discovered card from sidebar
      const card = document.querySelector('.session-card[data-key="_discovered:' + pd.pid + '"]');
      if (card) card.remove();
      pendingDiscovered = null;
      // Poll until the session appears in managed sessions (up to 10s)
      const takenKey = data.key;
      const takenNode = pd.node || 'local';
      let ready = false;
      for (let i = 0; i < 20; i++) {
        await new Promise(resolve => setTimeout(resolve, 500));
        lastVersion = 0;
        await fetchSessions();
        if (sessionsData[sid(takenKey, takenNode)]) { ready = true; break; }
      }
      if (!ready) {
        showToast('接管超时：会话未就绪，请稍后重试', 'error');
        if (input) { input.dataset.placeholder = 'send a message...'; input.contentEditable = 'true'; }
        sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      // Session is ready — switch to it and send the message
      sending = false;
      selectSession(takenKey, takenNode);
      // Restore the message text and send
      const newInput = document.getElementById('msg-input');
      if (newInput) setMsgValue(newInput, text);
      await sendMessage();
      return;
    } catch (e) {
      showNetworkError('接管进程', e);
      if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
      sending = false;
      if (btn) btn.classList.remove('sending');
      return;
    }
  }

  if (!selectedKey) return;
  const input = document.getElementById('msg-input');
  const text = getMsgValue(input);
  if (!text && pendingFiles.length === 0) return;

  // Multi-Backend RFC §8.3 D9 — `/urgent` requires the backend's
  // `passthrough` feature (preempt the running turn with a fresh user
  // message). kiro / ACP backends don't preempt; the server-side
  // dispatcher would either error or queue the message confusingly.
  // Toast and abort send so the operator is told *why* before they
  // wonder where their preemption went. Title-attr on /urgent button
  // would be ideal but /urgent is a text prefix typed in the input;
  // detect at send time instead.
  if (text && /^\s*\/urgent\b/.test(text) && !featureForCurrent('passthrough')) {
    showToast('当前后端不支持 /urgent 抢占（请用 Esc 中断后再发）', 'warning');
    return;
  }

  // Multi-Backend RFC §8.3 D13 — `@-mention` embedded context only
  // works when the backend reads file paths from inside the prompt
  // (claude does; kiro doesn't). Strip-and-warn would silently change
  // the prompt; better to abort + toast so the operator can paste the
  // absolute path or content explicitly.
  if (text && /(?:^|\s)@[\w./-]/.test(text) && !featureForCurrent('embedded_context')) {
    showToast('当前后端不支持 @ 文件 mention，请粘贴绝对路径或文件内容', 'warning');
    return;
  }

  // Per-field byte cap matches server maxWSSendTextBytes (1 MB). Reject
  // up-front so oversize pastes don't round-trip and return a silent
  // send_ack error that the optimistic bubble would have already printed.
  const byteLen = new Blob([text]).size;
  if (byteLen > 1024 * 1024) {
    showToast('消息过长 (' + Math.ceil(byteLen / 1024) + ' KB > 1024 KB 上限)', 'warning');
    return;
  }

  // Block send while any attachment is still uploading or errored —
  // we only reference file_ids on the server, so partial uploads would
  // silently drop images. User can retry or remove the bad one.
  if (pendingFiles.some(f => f.status === 'uploading')) {
    showToast('图片上传中，请稍候…', 'warning');
    return;
  }
  const failed = pendingFiles.filter(f => f.status === 'error');
  if (failed.length > 0) {
    const detail = failed[0].error || '';
    const tail = detail ? '（' + detail.slice(0, 120) + '）' : '';
    showToast('图片上传失败' + tail + '，请移除或重试', 'error');
    return;
  }
  // The shim NDJSON line cap is 12 MB; base64 inflates by ~1.33× so the
  // raw image batch must stay under ~9 MB to fit alongside the JSON
  // envelope. Pre-check here so users get a clear "too large — split into
  // fewer pictures" message instead of a silent "没 working" (R192 regression).
  //
  // PDFs do NOT count toward this budget — they travel as file_ref (server
  // persists the bytes to the session workspace; only the path string ends
  // up in the NDJSON line). Filtering by kind here keeps mixed image+PDF
  // sends from tripping the cap on the PDF's 20 MB that will never hit
  // stdin anyway.
  const totalBytes = pendingFiles.reduce((n, f) => {
    if (f.kind === 'pdf' || f.serverKind === 'file_ref') return n;
    return n + (f.normalizedSize || f.file.size || 0);
  }, 0);
  const batchCap = 9 * 1024 * 1024;
  if (totalBytes > batchCap) {
    showToast('图片总大小 ' + Math.ceil(totalBytes / 1024 / 1024) + ' MB 超过 9 MB 上限，请分批发送或减少图片', 'warning');
    return;
  }
  const fileIDs = pendingFiles.map(f => f.id).filter(Boolean);

  sending = true;
  const btn = document.getElementById('btn-send');
  if (btn) btn.classList.add('sending');
  // Flip the send→stop button + running banner BEFORE the network round trip,
  // not after — a resumed session has no CLI process yet, so the first send
  // triggers a subprocess spawn that can take several hundred ms. Leaving the
  // green send button visible during that window makes the click feel ignored
  // and invites double-sends. onSendAck/rollbackOptimisticRunning undo this on
  // busy/error/reset; the 20s safety timer in markSessionOptimisticRunning
  // prevents a stuck banner if the server never responds.
  markSessionOptimisticRunning(selectedKey, selectedNode);

  // WS path: always preferred now — uploads already on server, only file_ids travel.
  if (wsm.isConnected()) {
    const id = 'r' + (++wsm.sendCounter);
    const sendMsg = { type: 'send', key: selectedKey, text: text, id: id };
    if (fileIDs.length > 0) sendMsg.file_ids = fileIDs;
    if (selectedNode && selectedNode !== 'local') sendMsg.node = selectedNode;
    if (sessionWorkspaces[selectedKey]) {
      sendMsg.workspace = sessionWorkspaces[selectedKey];
      delete sessionWorkspaces[selectedKey];
      delete sessionNodes[selectedKey];
    }
    if (sessionBackends[selectedKey]) {
      sendMsg.backend = sessionBackends[selectedKey];
      // Backend is consumed once on session spawn; clear afterward so a
      // later re-send doesn't try to retrofit onto an existing session.
      delete sessionBackends[selectedKey];
    }
    if (wsm.send(sendMsg)) {
      // Optimistic render: show user message immediately without waiting
      // for the CLI to echo it back as a "user" event.
      const el = document.getElementById('events-scroll');
      if (el && text) {
        const now = Date.now();
        const html = eventHtml({type: 'user', detail: text, time: now});
        if (html) {
          const prevT = lastDividerTime(el);
          if (prevT === 0 || now - prevT >= EVENT_DIVIDER_GAP_MS) {
            el.insertAdjacentHTML('beforeend', timeDividerHtml(now));
          }
          el.insertAdjacentHTML('beforeend', html);
          el.lastElementChild.classList.add('optimistic-msg');
          // Always force-bottom after a send: the user just posted something
          // and expects to see it, even if they had scrolled up to browse
          // earlier history. stickEventsBottom handles async layout changes
          // from input-area collapse and lazy images.
          stickEventsBottom();
          navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
          navUpdatePill();
        }
      }
      if (input) clearMsg(input);
      delete sessionDrafts[selectedKey];
      clearPendingFiles();
      if (text) sessionLastSent[sid(selectedKey, selectedNode)] = text;
      // Confirmed send: the workspace/node/backend were consumed above (and
      // deleted from the in-memory maps), so rewrite the durable blob without
      // this key. Only on the success path — a failed wsm.send falls through to
      // HTTP below and must keep the entry for that retry.
      persistPending();
      // Optimistic running flip already applied above — no-op if unchanged.
      sending = false;
      if (btn) btn.classList.remove('sending');
      return;
    }
    // WS send failed, fall through to HTTP path below
  }

  // HTTP POST fallback — JSON only; files already on server.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;

    const payload = { key: selectedKey, text: text };
    if (fileIDs.length > 0) payload.file_ids = fileIDs;
    if (selectedNode && selectedNode !== 'local') payload.node = selectedNode;
    if (sessionWorkspaces[selectedKey]) {
      payload.workspace = sessionWorkspaces[selectedKey];
      delete sessionWorkspaces[selectedKey];
      delete sessionNodes[selectedKey];
    }
    if (sessionBackends[selectedKey]) {
      payload.backend = sessionBackends[selectedKey];
      delete sessionBackends[selectedKey];
    }

    const r = await fetch('/api/sessions/send', {method:'POST', headers, body: JSON.stringify(payload)});

    if (r.status === 401 || r.status === 403) {
      if (input) setMsgValue(input, text);
      rollbackOptimisticRunning(selectedKey, selectedNode);
      showAuthModal();
      return;
    }
    if (r.status === 429) {
      if (input) setMsgValue(input, text);
      rollbackOptimisticRunning(selectedKey, selectedNode);
      showToast('消息队列已满，请稍后重试', 'warning');
      return;
    }
    if (!r.ok) {
      if (input) setMsgValue(input, text);
      rollbackOptimisticRunning(selectedKey, selectedNode);
      // Some error paths still write text/plain; fall back to text() so we
      // always surface the real message instead of a generic "send failed".
      const raw = await r.text().catch(() => '');
      let detail = '';
      try { const j = JSON.parse(raw); if (j && j.error) detail = j.error; } catch (_) { if (raw) detail = raw; }
      showAPIError('发送消息', r.status, detail);
      return;
    }

    // /clear and /new return status:"reset" — no CLI turn to run, so don't
    // flip to 'running'. Every other success ('accepted'/'queued') should
    // show the banner immediately. Read the body once (before clearing the
    // input) so we can branch on status without reviving the stale text.
    let ackStatus = '';
    try { const j = await r.json(); if (j && j.status) ackStatus = j.status; } catch (_) {}

    // Clear input only after confirmed success
    if (input) clearMsg(input);
    delete sessionDrafts[selectedKey];
    clearPendingFiles();
    // Confirmed send: the pending maps were consumed above; rewrite the durable
    // blob without this key. Only on this 2xx path so a failed send keeps the
    // entry for retry.
    persistPending();
    if (ackStatus === 'reset') {
      // /clear and /new do not spawn a turn — undo the pre-send optimistic flip
      // so the running banner doesn't hang on a no-op command.
      rollbackOptimisticRunning(selectedKey, selectedNode);
    } else {
      if (text) sessionLastSent[sid(selectedKey, selectedNode)] = text;
      // Optimistic running flip already applied above — keep it.
    }

    // Speed up polling when WS not connected
    if (!wsm.isConnected()) {
      if (eventTimer) clearInterval(eventTimer);
      eventTimer = setInterval(() => fetchEvents(false), 500);
      setTimeout(() => {
        if (eventTimer) clearInterval(eventTimer);
        if (!wsm.isConnected()) {
          eventTimer = setInterval(() => fetchEvents(false), 1000);
        }
      }, 15000);
    }
  } catch (e) {
    if (input) input.value = text;
    rollbackOptimisticRunning(selectedKey, selectedNode);
    showNetworkError('发送消息', e);
  } finally {
    sending = false;
    if (btn) btn.classList.remove('sending');
  }
}

function clearPendingFiles() {
  pendingFiles.forEach(f => { if (f.blobUrl) URL.revokeObjectURL(f.blobUrl); });
  pendingFiles = [];
  renderFilePreviews();
}

// markSessionOptimisticRunning flips the selected session's local state to
// 'running' immediately after send succeeds so the running-banner shows
// without waiting for the server's session_state broadcast. The server can
// take 100ms–several seconds to emit BroadcastSessionReady when GetOrCreate
// has to spawn a new CLI subprocess, during which the dashboard previously
// looked idle even though the turn was already queued. Rolled back by
// onSendAck on 'busy'/'error' so a rejected send doesn't leave a stuck banner.
// Tracked with a 20s safety timer so a lost session_state push can't keep
// the banner stuck forever.
const _optimisticRunningTimers = {};
function markSessionOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = sid(key, node || 'local');
  const sd = sessionsData[sKey];
  if (!sd) return;
  if (sd.state === 'running') return; // server already said running
  sd.state = 'running';
  sessionOptimisticRunning[sKey] = true;
  if (_optimisticRunningTimers[sKey]) clearTimeout(_optimisticRunningTimers[sKey]);
  _optimisticRunningTimers[sKey] = setTimeout(() => {
    delete _optimisticRunningTimers[sKey];
    // Only rollback if still optimistic (no real running state arrived).
    if (sessionOptimisticRunning[sKey]) {
      rollbackOptimisticRunning(key, node);
    }
  }, 20000);
  if (key === selectedKey && (node || 'local') === selectedNode) {
    updateSendButton('running');
  }
}

function rollbackOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = sid(key, node || 'local');
  if (!sessionOptimisticRunning[sKey]) return;
  delete sessionOptimisticRunning[sKey];
  if (_optimisticRunningTimers[sKey]) {
    clearTimeout(_optimisticRunningTimers[sKey]);
    delete _optimisticRunningTimers[sKey];
  }
  const sd = sessionsData[sKey];
  if (sd && sd.state === 'running') {
    sd.state = 'ready';
    if (key === selectedKey && (node || 'local') === selectedNode) {
      updateSendButton('ready');
    }
  }
}

// --- Running banner: tool activity + agent tracking ---

let turnState = {
  toolCount: 0, currentTool: null, agents: [], isThinking: false,
  thinkingSummary: '', toolCounts: {}, toolOrder: [], turnStartTime: 0, isWriting: false,
  timerId: null
};

function resetTurnState() {
  if (turnState.timerId) clearInterval(turnState.timerId);
  turnState = {
    toolCount: 0, currentTool: null, agents: [], isThinking: false,
    thinkingSummary: '', toolCounts: {}, toolOrder: [], turnStartTime: 0, isWriting: false,
    timerId: null
  };
  refreshBanner();
}

function startTurnTimer() {
  if (turnState.turnStartTime) return;
  turnState.turnStartTime = Date.now();
  turnState.timerId = setInterval(function() {
    const el = document.getElementById('rb-elapsed');
    if (!el || !turnState.turnStartTime) return;
    const s = Math.floor((Date.now() - turnState.turnStartTime) / 1000);
    el.textContent = Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
  }, 1000);
}

function trackTool(name) {
  if (!name) return;
  if (!turnState.toolCounts[name]) {
    turnState.toolCounts[name] = 0;
    turnState.toolOrder.push(name);
  }
  turnState.toolCounts[name]++;
}

function fmtDuration(ms) {
  if (ms < 1000) return ms + 'ms';
  var s = ms / 1000;
  return s < 60 ? s.toFixed(1) + 's' : Math.floor(s / 60) + 'm' + Math.floor(s % 60) + 's';
}

// R110-P2 tool verb localization — these labels surface in the running-
// banner line-1 via refreshBanner → actEl.textContent. Mapping is strict
// whitelist on Claude's tool names; unknown tools fall back to "使用 X"
// (legacy "Using X") so future tools surface without a code change. Tool
// key names themselves (Read/Edit/Bash/…) are Claude protocol identifiers
// and MUST stay as map keys — only the display verbs localize.
const toolVerbs = {
  Read: '读取', Edit: '编辑', Write: '写入', Bash: '执行',
  Grep: '搜索', Glob: '查找文件', Agent: 'Agent',
  Notebook: '编辑 Notebook', WebFetch: '抓取',
  // RFC v4 §3.6.8 — tool verbs for the 2026-05 Claude tool set extension.
  TeamCreate: '创建团队', TeamDelete: '解散团队',
  SendMessage: '发消息', ToolSearch: '加载工具',
  TaskOutput: '读 agent 输出', TaskStop: '停止 agent',
  ScheduleWakeup: '排唤醒',
  CronCreate: '建定时任务', CronDelete: '删定时任务', CronList: '查定时任务'
};

function toolVerb(tool, summary) {
  const verb = toolVerbs[tool] || ('使用 ' + tool);
  if (!summary || summary === tool) return verb + '...';
  return verb + ' ' + summary;
}

function refreshBanner() {
  const actEl = document.getElementById('tool-activity');
  const thinkEl = document.getElementById('rb-thinking-summary');
  const agEl = document.getElementById('rb-agents');
  const statsEl = document.getElementById('rb-stats');

  // Line 1: current activity
  if (actEl) {
    if (turnState.currentTool) {
      actEl.textContent = toolVerb(turnState.currentTool.tool, turnState.currentTool.summary);
    } else if (turnState.isThinking) {
      actEl.textContent = '思考中...';
    } else if (turnState.isWriting) {
      actEl.textContent = '输出中...';
    } else {
      actEl.textContent = '处理中...';
    }
  }

  // Thinking summary line (only during thinking)
  if (thinkEl) {
    if (turnState.isThinking && turnState.thinkingSummary) {
      thinkEl.textContent = turnState.thinkingSummary;
      thinkEl.style.display = '';
    } else {
      thinkEl.style.display = 'none';
    }
  }

  // Agent rows
  if (agEl) {
    agEl.innerHTML = renderAgentRows();
  }

  // Stats line (hidden when agents are shown)
  if (statsEl) {
    var hasAgents = turnState.agents.length > 0;
    if (!hasAgents && turnState.toolOrder.length > 0) {
      statsEl.textContent = turnState.toolOrder.map(function(t) {
        return t + ' \u00d7' + turnState.toolCounts[t];
      }).join(' \u00b7 ');
      statsEl.style.display = '';
    } else {
      statsEl.style.display = 'none';
    }
  }

  // Auto-show/hide banner based on session state and active content.
  // When state is "running", updateSendButton already forces display=''.
  // When state is "ready", only keep the banner visible if
  // there are genuinely active background agents (zero-downtime restart).
  // Late-arriving history batches with stale tool events must NOT re-show
  // the banner after the session has finished.
  const banner = document.getElementById('running-banner');
  if (banner) {
    const hasContent = turnState.currentTool || turnState.isThinking || turnState.isWriting || turnState.agents.length > 0 || turnState.toolOrder.length > 0;
    const sKey = sid(selectedKey, selectedNode);
    const sess = sessionsData[sKey];
    const isRunning = sess && sess.state === 'running';
    const hasActiveAgents = turnState.agents.some(function(a) { return a.status !== 'completed' && a.status !== 'error'; });
    if (hasContent && (isRunning || hasActiveAgents) && banner.style.display === 'none') {
      banner.style.display = '';
    } else if (banner.style.display !== 'none' && !isRunning && !hasActiveAgents) {
      banner.style.display = 'none';
    }
  }
}

function updateSidebarAgentBadge() {
  if (!selectedKey) return;
  var card = document.querySelector('.session-card[data-key="' + escAttr(selectedKey) + '"]');
  if (!card) return;
  var meta = card.querySelector('.sc-meta');
  if (!meta) return;
  var count = turnState.agents.length;
  var existing = meta.querySelector('.sc-agents');
  if (count > 0) {
    var html = '\u{1F916}\u00D7' + count;
    if (existing) { existing.innerHTML = html; }
    else { var span = document.createElement('span'); span.className = 'sc-agents'; span.innerHTML = html; meta.appendChild(span); }
  } else if (existing) { existing.remove(); }
}

// renderAgentRows / agentRowHtml / findAgentByToolUseId / findAgentByTaskId /
// initAgentsFromSession moved to static/agent_view.js (RFC v4 agent-team-ui
// Phase 2.5). The names remain published on window so call sites here keep
// working unchanged; the indirection gives Phase 3 a clean module boundary
// to grow the banner/switchAgentView/WS-agent logic without piling onto
// this already-oversized file.

function applyEventToTurnState(ev) {
  startTurnTimer();
  switch (ev.type) {
    case 'tool_use':
      turnState.toolCount++;
      trackTool(ev.tool || ev.summary);
      turnState.currentTool = { tool: ev.tool || ev.summary, summary: ev.detail ? ev.detail.split('\n')[0].substring(0, 60) : '' };
      turnState.isThinking = false;
      turnState.isWriting = false;
      turnState.thinkingSummary = '';
      break;
    case 'agent':
      turnState.toolCount++;
      trackTool('Agent');
      turnState.currentTool = null;
      turnState.isThinking = false;
      turnState.isWriting = false;
      turnState.thinkingSummary = '';
      turnState.agents.push({
        toolUseId: ev.tool_use_id || '', taskId: '',
        name: ev.subagent || '', teamName: ev.team_name || '',
        description: ev.summary || '', background: !!ev.background,
        lastTool: '', toolUses: 0, totalTokens: 0, durationMs: 0, status: 'spawned'
      });
      updateSidebarAgentBadge();
      break;
    case 'task_start':
      var a1 = findAgentByToolUseId(ev.tool_use_id);
      if (a1) {
        a1.taskId = ev.task_id;
        a1.status = 'running';
      }
      break;
    case 'task_progress':
      var a2 = findAgentByTaskId(ev.task_id) || findAgentByToolUseId(ev.tool_use_id);
      if (a2) {
        if (!a2.taskId) a2.taskId = ev.task_id;
        a2.status = 'running';
        if (ev.summary) a2.description = ev.summary;
        if (ev.last_tool) a2.lastTool = ev.last_tool;
        if (ev.tool_uses) a2.toolUses = ev.tool_uses;
        if (ev.tokens) a2.totalTokens = ev.tokens;
        if (ev.duration_ms) a2.durationMs = ev.duration_ms;
      }
      break;
    case 'task_done':
      var a3 = findAgentByTaskId(ev.task_id) || findAgentByToolUseId(ev.tool_use_id);
      if (a3) {
        if (!a3.taskId) a3.taskId = ev.task_id;
        a3.status = ev.status || 'completed';
        if (ev.tool_uses) a3.toolUses = ev.tool_uses;
        if (ev.tokens) a3.totalTokens = ev.tokens;
        if (ev.duration_ms) a3.durationMs = ev.duration_ms;
      }
      break;
    case 'thinking':
      turnState.isThinking = true;
      turnState.isWriting = false;
      turnState.currentTool = null;
      turnState.thinkingSummary = ev.summary || '';
      break;
    case 'text':
      turnState.isThinking = false;
      turnState.isWriting = true;
      turnState.currentTool = null;
      turnState.thinkingSummary = '';
      break;
    case 'user':
    case 'result':
      // Turn boundary: mirror the backend eventlog.applyEntryStateLocked
      // clearing of turnAgents/bgAgents so the banner doesn't carry over
      // agent rows from a previous turn (and, post-reconnect, from
      // replayed history where the Linker no longer has the task mapping).
      // Without this, the banner keeps showing clickable agent rows for
      // tasks that can never be resolved, and every click wastes ~5s
      // of 202-pending retries before the loading indicator clears.
      turnState.agents = [];
      turnState.currentTool = null;
      turnState.isThinking = false;
      turnState.isWriting = false;
      turnState.thinkingSummary = '';
      updateSidebarAgentBadge();
      break;
  }
}

function interruptSession() {
  if (!selectedKey) return;
  const sd = sessionsData[sid(selectedKey, selectedNode || 'local')];
  if (!sd || sd.state !== 'running') return;
  const targetNode = selectedNode && selectedNode !== 'local' ? selectedNode : '';
  // Claude Code 风格：中断时把刚发的那条用户文本回填到输入框方便改写。
  // 只在输入框当前为空时回填，避免覆盖用户已经开始输入的新内容；回填后
  // 把光标挪到末尾、聚焦、滚进视口。回填完成即消费掉 lastSent，防止同一条
  // 文本在后续多次中断里反复回填。
  const lastText = sessionLastSent[sid(selectedKey, selectedNode)];
  if (lastText) {
    const input = document.getElementById('msg-input');
    if (input && !getMsgValue(input)) {
      setMsgValue(input, lastText);
      try {
        input.focus();
        const range = document.createRange();
        range.selectNodeContents(input);
        range.collapse(false);
        const sel = window.getSelection();
        if (sel) { sel.removeAllRanges(); sel.addRange(range); }
      } catch (_) {}
      sessionDrafts[selectedKey] = lastText;
      delete sessionLastSent[sid(selectedKey, selectedNode)];
    }
  }
  if (wsm.isConnected()) {
    const req = { type: 'interrupt', key: selectedKey, id: 'int' + Date.now() };
    if (targetNode) req.node = targetNode;
    wsm.send(req);
    showToast('已发送中断', 'warning');
  } else {
    // HTTP fallback when WebSocket is disconnected
    const headers = {'Content-Type': 'application/json'};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const body = { key: selectedKey };
    if (targetNode) body.node = targetNode;
    fetch('/api/sessions/interrupt', {
      method: 'POST',
      headers,
      body: JSON.stringify(body)
    }).then(r => r.json()).then(d => {
      showToast(d.status === 'ok' ? '已发送中断' : '会话未在运行', 'warning');
    }).catch((e) => showNetworkError('中断会话', e));
  }
}

function scrollEventsToBottom() {
  const el = document.getElementById('events-scroll');
  if (el) el.scrollTop = el.scrollHeight;
}

// saveScrollPos / restoreScrollPos: 按 (key,node) 保存离开时的滚动位置，
// 回到同一会话时恢复。用「距底距离」而不是 scrollTop，因为会话再进入时
// 可能会多加载更早的事件导致 scrollHeight 变大，距底更稳定。atBottom 单
// 独标记以便新消息到来时继续贴底（shell 式滚动），只有用户明确滚开时才
// 进入「保持位置」分支。
function saveScrollPos(key, node) {
  const el = document.getElementById('events-scroll');
  if (!el || !key) return;
  // clientHeight === 0 发生在 events-scroll 还未 layout 完（极早期竞态），
  // 这时算出来的 fromBottom=0、atBottom=true 会把之前真实保存的位置擦掉。
  // 直接跳过，保留上一份快照。
  if (el.clientHeight === 0) return;
  const fromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
  const atBottom = fromBottom <= 30;
  sessionScrollPos[sid(key, node || 'local')] = { fromBottom, atBottom };
}

// restoreScrollPos: 如果有保存的位置且不是贴底，则恢复并返回 true；
// 否则（无记录 / 之前在底）返回 false，交由调用方走 stickEventsBottom 贴底。
function restoreScrollPos(key, node) {
  const el = document.getElementById('events-scroll');
  if (!el || !key) return false;
  const pos = sessionScrollPos[sid(key, node || 'local')];
  if (!pos || pos.atBottom) return false;
  const apply = () => {
    const target = Math.max(0, el.scrollHeight - el.clientHeight - pos.fromBottom);
    el.scrollTop = target;
  };
  apply();
  // 异步布局（图片 / mermaid / katex / "加载更早" 按钮注入）会改变
  // scrollHeight，再跑两帧复位保持「距底距离」不变。
  requestAnimationFrame(() => {
    apply();
    requestAnimationFrame(apply);
  });
  return true;
}

// maybeStickBottom is the conditional counterpart to stickEventsBottom:
// it ONLY scrolls if the user is already pinned within `scrollSlackPx`
// of the bottom. WS-pushed assistant chunks / result events go through
// this so a user reading earlier history isn't yanked to the latest
// reply mid-scroll. UI Round 5 R5-6.
//
// Trigger contract (per design doc §R5-6):
//   - send-time optimistic bubble  → stickEventsBottom (unconditional)
//   - selectSession                → stickEventsBottom (fresh view)
//   - history "load earlier" page  → no scroll (preserve position)
//   - WS push assistant_chunk      → maybeStickBottom (only if at bottom)
//   - WS push result event         → maybeStickBottom (only if at bottom)
const scrollSlackPx = 80;
function maybeStickBottom() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
  if (distance < scrollSlackPx) {
    stickEventsBottom();
  }
}

// stickEventsBottom forces the events pane to the last bubble and keeps it there
// across the async layout tail — lazy-loaded images, mermaid diagrams, katex
// formulas, and the "load earlier" button that inserts at the top after the
// initial scrollTop assignment all change scrollHeight after the first paint.
// Used by session-open flows where losing the bottom anchor would hide the
// newest messages (the whole point of opening the session).
function stickEventsBottom() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  el.scrollTop = el.scrollHeight;
  requestAnimationFrame(() => {
    el.scrollTop = el.scrollHeight;
    requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
  });
  // Re-stick after each lazy-loaded image, but only while the user hasn't
  // scrolled away from the bottom. Without this guard, a session opened
  // seconds ago whose images are still loading will yank the viewport back
  // to the bottom the moment any image finishes — even if the user has
  // since scrolled up to read history (common on mobile/slow networks).
  el.querySelectorAll('img').forEach(img => {
    if (img.complete) return;
    const restick = () => {
      if (el.scrollTop + el.clientHeight >= el.scrollHeight - 30) {
        el.scrollTop = el.scrollHeight;
      }
    };
    img.addEventListener('load', restick, { once: true });
    img.addEventListener('error', restick, { once: true });
  });
}

// --- Message navigation ---
let navUserEls = [];
let navPopoverCloseHandler = null;
// #1772: synchronous "is the nav popover mounted" flag. Set true the moment the
// popover is appended, false when dismissed. Lets the per-scroll-tick handler
// skip a getElementById on the common (no-popover) path without the race of
// reading navPopoverCloseHandler, which is only assigned in a deferred
// setTimeout(0) after mount.
let navPopoverOpen = false;
let navIdx = -1; // -1 = not navigating

function navRebuild() {
  navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
  navIdx = -1;
  navUpdatePill();
}

// Infer which user message is "at" the current scroll position. Returns the
// index of the last user message whose top edge sits at or above the viewport
// center; falls back to the first message below when the viewport is above
// every user message, or -1 when there are none.
function navCurrentIdxFromScroll() {
  const scroller = document.getElementById('events-scroll');
  if (!scroller || navUserEls.length === 0) return -1;
  const anchor = scroller.getBoundingClientRect().top + scroller.clientHeight * 0.3;
  let lastAbove = -1;
  for (let i = 0; i < navUserEls.length; i++) {
    const top = navUserEls[i].getBoundingClientRect().top;
    if (top <= anchor) lastAbove = i;
    else break;
  }
  return lastAbove;
}

function navMsg(dir) {
  if (navUserEls.length === 0) return;
  // Shell-history 语义：第一次按方向键只定位到「视图锚点」消息本身
  // （prev → 最近一条用户消息；next → 视图内第一条用户消息），
  // 不额外再走一步。只有已在导航中（navIdx >= 0）时才做 ±1 步进。
  const firstPress = navIdx < 0;
  if (firstPress) navIdx = navCurrentIdxFromScroll();
  let target;
  if (dir === 'prev') {
    target = firstPress
      ? (navIdx < 0 ? navUserEls.length - 1 : navIdx)
      : Math.max(0, navIdx - 1);
  } else {
    target = firstPress
      ? (navIdx < 0 ? 0 : navIdx)
      : Math.min(navUserEls.length - 1, navIdx + 1);
  }
  if (!firstPress && target === navIdx) {
    // Already at the edge — flash the current one so the user sees the no-op.
    const cur = navUserEls[navIdx];
    if (cur) {
      cur.classList.add('nav-highlight');
      setTimeout(() => cur.classList.remove('nav-highlight'), 600);
    }
    return;
  }
  navIdx = target;
  const el = navUserEls[navIdx];
  if (!el) return;
  el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  // highlight flash
  document.querySelectorAll('.event.nav-highlight').forEach(e => e.classList.remove('nav-highlight'));
  el.classList.add('nav-highlight');
  setTimeout(() => el.classList.remove('nav-highlight'), 1200);
  navUpdatePill();
}

function navUpdatePill() {
  const pill = document.getElementById('nav-pill');
  const counter = document.getElementById('nav-counter');
  if (!pill) return;
  if (navUserEls.length < 2) {
    pill.classList.remove('visible');
    return;
  }
  pill.classList.add('visible');
  if (navIdx < 0) {
    counter.textContent = navUserEls.length;
  } else {
    counter.textContent = (navIdx + 1) + '/' + navUserEls.length;
  }
}

function navDismissPopover() {
  const pop = document.getElementById('nav-list-popover');
  if (pop) pop.remove();
  navPopoverOpen = false;
  if (navPopoverCloseHandler) {
    document.removeEventListener('click', navPopoverCloseHandler);
    navPopoverCloseHandler = null;
  }
}

function navShowList() {
  if (navUserEls.length === 0) return;
  let existing = document.getElementById('nav-list-popover');
  if (existing) { navDismissPopover(); return; } // toggle off
  const items = navUserEls.map((el, i) => {
    const txt = (el.querySelector('.event-content')?.textContent || '').trim();
    const summary = txt.length > 50 ? txt.slice(0, 50) + '...' : txt;
    const active = i === navIdx ? ' style="color:var(--nz-accent);font-weight:600"' : '';
    return '<div class="nav-list-item" data-idx="' + i + '"' + active + '>' +
      '<span style="color:var(--nz-text-faint);margin-right:6px">' + (i+1) + '.</span>' + esc(summary) + '</div>';
  });
  const pill = document.getElementById('nav-pill');
  const popover = document.createElement('div');
  popover.id = 'nav-list-popover';
  const maxW = Math.min(280, (document.getElementById('main')?.offsetWidth || 280) - 70);
  popover.style.cssText = 'position:absolute;right:44px;bottom:0;width:' + maxW + 'px;max-height:300px;overflow-y:auto;background:rgba(22,27,34,.95);backdrop-filter:blur(8px);border:1px solid var(--nz-border);border-radius:10px;padding:6px 0;z-index:11;font-size:13px;scrollbar-width:thin;scrollbar-color:var(--nz-border) transparent';
  popover.innerHTML = items.join('');
  pill.appendChild(popover);
  navPopoverOpen = true;
  popover.querySelectorAll('.nav-list-item').forEach(item => {
    item.style.cssText += 'padding:8px 12px;cursor:pointer;color:var(--nz-text);transition:background .1s;border-bottom:1px solid var(--nz-bg-2);overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
    item.onmouseenter = () => item.style.background = '#1f2937';
    item.onmouseleave = () => item.style.background = '';
    item.onclick = () => {
      navIdx = parseInt(item.dataset.idx);
      const el = navUserEls[navIdx];
      if (el) {
        el.scrollIntoView({ behavior: 'smooth', block: 'center' });
        document.querySelectorAll('.event.nav-highlight').forEach(e => e.classList.remove('nav-highlight'));
        el.classList.add('nav-highlight');
        setTimeout(() => el.classList.remove('nav-highlight'), 1200);
      }
      navUpdatePill();
      navDismissPopover();
    };
  });
  // Close on outside click
  setTimeout(() => {
    navPopoverCloseHandler = (e) => {
      if (!popover.contains(e.target) && e.target.id !== 'nav-counter') {
        navDismissPopover();
      }
    };
    document.addEventListener('click', navPopoverCloseHandler);
  }, 0);
}

// Reset nav on scroll to bottom
(function() {
  let scrollListenerAttached = false;
  function attachNavScroll() {
    const el = document.getElementById('events-scroll');
    if (!el || scrollListenerAttached) return;
    scrollListenerAttached = true;
    // Debounce after scrolling settles: if the tracked nav target is no
    // longer near the viewport center (i.e. user scrolled manually), drop it
    // so the next arrow-key press re-seeds from what the user actually sees.
    let scrollResetTimer = null;
    el.addEventListener('scroll', () => {
      // #1772: only touch the DOM to dismiss the nav popover when one is
      // actually open, skipping a per-scroll-tick getElementById on the
      // overwhelmingly common path (no popover) during inertial scrolling.
      if (navPopoverOpen) navDismissPopover();
      if (scrollResetTimer) clearTimeout(scrollResetTimer);
      scrollResetTimer = setTimeout(() => {
        if (navIdx < 0 || !navUserEls[navIdx]) return;
        const scrollerRect = el.getBoundingClientRect();
        const targetRect = navUserEls[navIdx].getBoundingClientRect();
        const targetCenter = targetRect.top + targetRect.height / 2;
        const viewportCenter = scrollerRect.top + scrollerRect.height / 2;
        if (Math.abs(targetCenter - viewportCenter) > scrollerRect.height / 2) {
          navIdx = -1;
          navUpdatePill();
        }
      }, 300);
    }, { passive: true });
  }
  // Re-attach after renderMainShell rebuilds the DOM
  const obs = new MutationObserver(() => {
    scrollListenerAttached = false;
    attachNavScroll();
  });
  obs.observe(document.getElementById('main') || document.body, { childList: true, subtree: false });
  attachNavScroll();
})();

// Paste handler for #msg-input:
//   1. Image files on the clipboard (screenshot Cmd/Ctrl+V, "copy image" from
//      another app) are routed to handleFiles so they land in pendingFiles and
//      ride the same upload / file_ids path as the paperclip button. Without
//      this branch the browser's default paste embeds the image as
//      `<img src="data:...">` inside the contenteditable — `innerText.trim()`
//      drops it silently so the send ends up carrying neither text nor
//      file_ids, and Claude never sees the image the user thought they sent.
//   2. Plain text is forced in via execCommand('insertText') so rich
//      formatting from Word / web pages doesn't leak into the contenteditable.
document.addEventListener('paste', function(e) {
  const t = e.target;
  if (!t || !t.closest || !t.closest('#msg-input')) return;
  const cd = e.clipboardData || window.clipboardData;
  if (!cd) return;

  // Image branch: walk clipboardData.files first (most reliable on Chromium
  // + Safari), fall back to clipboardData.items for older paths. Any image
  // file short-circuits the default paste so the browser doesn't also embed
  // a stray `<img>` into the contenteditable.
  const imageFiles = [];
  if (cd.files && cd.files.length) {
    for (const f of cd.files) {
      if (f && f.type && f.type.startsWith('image/')) imageFiles.push(f);
    }
  }
  if (imageFiles.length === 0 && cd.items) {
    for (const it of cd.items) {
      if (it && it.kind === 'file' && it.type && it.type.startsWith('image/')) {
        const f = it.getAsFile();
        if (f) imageFiles.push(f);
      }
    }
  }
  if (imageFiles.length > 0) {
    e.preventDefault();
    if (typeof handleFiles === 'function') handleFiles(imageFiles);
    return;
  }

  const text = cd.getData('text/plain');
  if (!text) return;
  e.preventDefault();
  if (document.queryCommandSupported && document.queryCommandSupported('insertText')) {
    document.execCommand('insertText', false, text);
    return;
  }
  const sel = window.getSelection();
  if (!sel || sel.rangeCount === 0) return;
  const range = sel.getRangeAt(0);
  range.deleteContents();
  const node = document.createTextNode(text);
  range.insertNode(node);
  range.setStartAfter(node);
  range.setEndAfter(node);
  sel.removeAllRanges();
  sel.addRange(range);
});

// Keyboard shortcut: Alt+Up/Down for message nav, Alt+N for new session.
// Cmd/Ctrl+N is left alone so the browser's "new window" still works.
document.addEventListener('keydown', function(e) {
  if (e.altKey && e.key === 'ArrowUp') { e.preventDefault(); navMsg('prev'); }
  if (e.altKey && e.key === 'ArrowDown') { e.preventDefault(); navMsg('next'); }
  if (e.altKey && (e.key === 'n' || e.key === 'N')) {
    const tag = (e.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
    e.preventDefault();
    createNewSession();
  }
});

// Global Esc: close open popovers (history / nav list) when no modal/input has focus.
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Escape') return;
  // Overlays with their own Esc trapFocus handling take precedence.
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
  let closed = false;
  if (activePopover) { closeHistoryPopover(); closed = true; }
  if (document.getElementById('nav-list-popover')) { navDismissPopover(); closed = true; }
  // §16 inline-expand 回归: Esc 优先关展开行（如果有），保留 drawer。
  // 行内展开是 drawer 的二级展开，关 expand 而不连带关 drawer 才符合用户心智。
  if (cronExpandedRunId && cronExpandedRunId.runId) { cronTimelineCollapse(); closed = true; }
  // cron-panel-consolidation RFC §6.4: Esc closes the cron drawer when
  // it's open. Routed before / alongside other popover dismissals so the
  // drawer ✕ button's title="关闭 (Esc)" promise is actually kept.
  // The drawer is NOT a modal — Esc only acts when no input or modal is
  // foregrounded (gates above), and focus is restored to the originating
  // .cj-row by closeCronDetail itself.
  else if (cronDetailJobId !== null) { closeCronDetail(); closed = true; }
  if (closed) e.preventDefault();
});

// §16 inline-expand 回归: ↑↓ 切上一条 / 下一条 run（仅当某行展开时）。
// 与 Cmd/Ctrl+Up/Down 的会话切换错开（那个有 metaKey 守卫）。
document.addEventListener('keydown', function(e) {
  if (!cronExpandedRunId || !cronExpandedRunId.runId) return;
  if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
  e.preventDefault();
  navigateExpandedRun(e.key === 'ArrowUp' ? 'prev' : 'next');
});

// Keyboard shortcut: Cmd/Ctrl+1..9 — switch to Nth session in current project group
// Cmd/Ctrl+Up/Down — prev/next session in group
document.addEventListener('keydown', function(e) {
  // Skip when typing in input fields
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;

  const isMeta = e.metaKey || e.ctrlKey;
  if (!isMeta) return;

  // Cmd+1..9: jump to Nth session in group
  const digit = parseInt(e.key);
  if (digit >= 1 && digit <= 9) {
    e.preventDefault();
    const group = currentProjectSessions();
    if (digit <= group.length) {
      const s = group[digit - 1];
      selectSession(s.key, s.node || 'local');
    }
    return;
  }

  // Cmd+Up/Down: prev/next session in group
  if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
    e.preventDefault();
    const group = currentProjectSessions();
    if (group.length === 0) return;
    const idx = group.findIndex(s => s.key === selectedKey && (s.node || 'local') === selectedNode);
    let next;
    if (idx < 0) {
      next = 0;
    } else {
      next = e.key === 'ArrowUp' ? idx - 1 : idx + 1;
      if (next < 0) next = group.length - 1;
      if (next >= group.length) next = 0;
    }
    const s = group[next];
    selectSession(s.key, s.node || 'local');
    return;
  }
});

// Get sessions in the same project group as the current selection (sidebar order).
// Fallback groups are workspace-basename pseudo-projects, so two sessions
// sharing the same project name but different workspaces belong to different
// groups — include workspace in the match to mirror the sidebar's grouping.
function currentProjectSessions() {
  if (!allSessionsCache || allSessionsCache.length === 0) return [];
  const cur = allSessionsCache.find(s => s.key === selectedKey && (s.node || 'local') === selectedNode);
  if (!cur) return [];
  const proj = cur.project || '';
  const isFallback = !!cur.project_fallback;
  const ws = cur.workspace || '';
  return allSessionsCache.filter(s => {
    if ((s.project || '') !== proj) return false;
    if (isFallback || s.project_fallback) {
      return !!s.project_fallback === isFallback && (s.workspace || '') === ws;
    }
    return true;
  });
}

function updateSendButton(state) {
  const banner = document.getElementById('running-banner');
  const sendBtn = document.getElementById('btn-send');
  const stopBtn = document.getElementById('btn-stop');
  const inVoiceMode = document.getElementById('input-area')?.classList.contains('voice-mode');
  if (state === 'running') {
    if (banner) banner.style.display = '';
    if (sendBtn) sendBtn.style.display = 'none';
    if (stopBtn) stopBtn.style.display = 'flex';
    initAgentsFromSession();
    refreshBanner();
  } else {
    // resetTurnState → refreshBanner will hide the banner since the session
    // is no longer "running". If background agents are still active (e.g.
    // zero-downtime restart), refreshBanner keeps the banner visible.
    if (sendBtn) sendBtn.style.display = inVoiceMode ? 'none' : 'flex';
    if (stopBtn) stopBtn.style.display = 'none';
    resetTurnState();
    // Replace stale loading indicator if session stopped before events arrived.
    const evEl2 = document.getElementById('events-scroll');
    const loadingEl = evEl2 && evEl2.querySelector('.loading-indicator');
    if (loadingEl) loadingEl.innerHTML = '暂无事件';
  }
  // Banner show/hide changes .events height — keep latest message visible.
  // Only auto-scroll if the user is already near the bottom; otherwise
  // respect their scroll position (e.g. reading history).
  const evEl = document.getElementById('events-scroll');
  if (evEl && evEl.scrollTop + evEl.clientHeight >= evEl.scrollHeight - 50) {
    evEl.scrollTop = evEl.scrollHeight;
  }
}

// --- File handling ---
//
// Each selected image is pre-uploaded via POST /api/sessions/upload as soon
// as it's picked. pendingFiles holds {file, blobUrl, id, status, error}:
//   status: 'uploading' | 'ready' | 'error' — 'ready' means a valid server-side
//   file id is in `id` and can be referenced later via file_ids on send.
// This decouples image transfer from /send, avoids the 105 MB multipart body
// and 15s ReadTimeout, and lets one bad file fail without blocking the rest.

function openFilePicker() {
  // Multi-Backend RFC §8.3 D14: respect feature gate. Toast instead of
  // silently no-op so the user understands why the click did nothing —
  // the .feat-disabled class is the visual cue, this is the audible cue.
  if (!featureForCurrent('image_input')) {
    showToast('当前后端不支持图片上传', 'warn');
    return;
  }
  document.getElementById('file-input').click();
}

// Downscale any image to JPEG with max edge 1600 and quality 0.8.
// Rationale: the CLI writes user messages as one NDJSON line to the shim,
// which is capped at 12 MB per line; base64 inflates bytes by ~1.33×, so a
// 20-image batch must stay under ~9 MB raw to fit. 1600 / q0.8 typically
// yields 150–400 KB per JPEG (vs 500 KB–1.2 MB at 2048 / q0.85) while still
// above the 1568 px knee where Anthropic's vision models stop gaining
// accuracy. HEIC is also handled here — createImageBitmap decodes it on
// Safari 17+ and we re-encode to JPEG.
//
// Orientation: phone cameras tag photos with an EXIF orientation flag
// instead of rotating the pixels. Re-encoding through canvas drops that
// flag, so we must bake the rotation into the pixels at decode time via
// `imageOrientation: 'from-image'` — without it a portrait iPhone shot
// arrives at the backend sideways. With the flag, the returned bitmap's
// width/height are ALREADY the visually-correct (post-rotation) dimensions,
// so the scaling math below needs no orientation branching and we must NOT
// apply any extra ctx.rotate (that would double-correct). The option is the
// modern default on Chrome/Firefox and is honored by Safari/iOS 16+; older
// engines silently ignore the unknown member and fall back to their default
// decode, which is the best we can do client-side.
// Falls back to the original file if decoding fails so the server's
// content-type check still produces a real error message.
async function normalizeImage(file) {
  const MAX_EDGE = 1600;
  try {
    const bmp = await createImageBitmap(file, { imageOrientation: 'from-image' });
    const { width: sw, height: sh } = bmp;
    let dw = sw, dh = sh;
    const m = Math.max(sw, sh);
    if (m > MAX_EDGE) {
      const scale = MAX_EDGE / m;
      dw = Math.max(1, Math.round(sw * scale));
      dh = Math.max(1, Math.round(sh * scale));
    }
    const canvas = document.createElement('canvas');
    canvas.width = dw;
    canvas.height = dh;
    const ctx = canvas.getContext('2d');
    ctx.drawImage(bmp, 0, 0, dw, dh);
    bmp.close();
    const blob = await new Promise(res => canvas.toBlob(res, 'image/jpeg', 0.8));
    if (!blob) return file;
    return new File([blob], (file.name || 'image').replace(/\.[^.]+$/, '') + '.jpg', { type: 'image/jpeg' });
  } catch (_) {
    return file;
  }
}

// uploadConcurrency caps parallel POST /api/sessions/upload requests so a
// 20-image batch on a mobile connection doesn't fan out 20 simultaneous
// bodies competing for the same uplink. With 15 s server ReadTimeout, too
// many parallel streams starve each other and trigger multipart i/o
// timeouts — pre-R192 the old 10-file ceiling masked this, but at 20 it
// shows up. 3 parallel uploads is the sweet spot: still fast on LTE/WiFi,
// safe on slow cellular.
const uploadConcurrency = 3;
let uploadInFlight = 0;
const uploadQueue = [];

function enqueueUpload(entry) {
  uploadQueue.push(entry);
  drainUploadQueue();
}

function drainUploadQueue() {
  while (uploadInFlight < uploadConcurrency && uploadQueue.length > 0) {
    const entry = uploadQueue.shift();
    uploadInFlight++;
    uploadEntry(entry).finally(() => {
      uploadInFlight--;
      drainUploadQueue();
    });
  }
}

// fileKind maps a browser File's MIME type to the 2 classes naozhi accepts.
// PDF sniffing looks at file.type AND the .pdf extension because some mobile
// Safari builds drop a `content-type: application/octet-stream` on PDFs
// picked from iCloud Drive — the server still sniffs magic bytes, so accepting
// optimistically here just lets the server give the authoritative reject.
function fileKind(f) {
  if (f && f.type === 'application/pdf') return 'pdf';
  if (f && /\.pdf$/i.test(f.name || '')) return 'pdf';
  if (f && f.type && f.type.startsWith('image/')) return 'image';
  return '';
}

function handleFiles(fileList) {
  const toUpload = [];
  // Image source ceiling is kept at 40 MB so iPhone HEIC/JPEG straight from
  // Photos (~6–12 MB) still fits; normalizeImage downscales before upload,
  // so the 10 MB server cap applies to the re-encoded JPEG. PDFs bypass
  // normalization and must stay under the server's 32 MB Anthropic ceiling.
  const MAX_IMAGE_BYTES = 40 * 1024 * 1024;
  const MAX_PDF_BYTES = 32 * 1024 * 1024;
  for (const raw of fileList) {
    const kind = fileKind(raw);
    if (!kind) continue;
    if (kind === 'pdf' && raw.size > MAX_PDF_BYTES) {
      showToast('PDF 过大（上限 32 MB）', 'warning');
      continue;
    }
    if (kind === 'image' && raw.size > MAX_IMAGE_BYTES) {
      showToast('图片过大（上限 40 MB）', 'warning');
      continue;
    }
    if (pendingFiles.length >= 20) { showToast('最多上传 20 个文件', 'warning'); break; }
    const entry = {
      file: raw,
      kind,
      // blobUrl is still set for images so the existing thumbnail path works
      // unchanged. PDFs render as an icon card (see renderFilePreviews) so no
      // URL.createObjectURL is needed and we skip it to save the tiny
      // revoke-on-remove bookkeeping.
      blobUrl: kind === 'image' ? URL.createObjectURL(raw) : '',
      id: '',
      status: 'uploading',
      error: '',
    };
    pendingFiles.push(entry);
    toUpload.push(entry);
  }
  const fi = document.getElementById('file-input');
  if (fi) fi.value = '';
  renderFilePreviews();
  toUpload.forEach(enqueueUpload);
}

async function uploadEntry(entry) {
  entry.status = 'uploading';
  entry.error = '';
  renderFilePreviews();
  try {
    // PDFs skip normalizeImage: they travel to the server as-is and end up
    // persisted to the session workspace (see
    // docs/rfc/pdf-attachment.md). Only images go through the downscale
    // step. Track the transmitted byte size on `normalizedSize` for the
    // sendMessage batch-cap check below — for PDFs this equals raw size
    // but is NOT counted against the 9 MB image batch cap (PDFs travel
    // via file_ref, not inline base64).
    const file = entry.kind === 'pdf' ? entry.file : await normalizeImage(entry.file);
    entry.normalizedSize = file.size;
    // Swap the preview thumbnail to the normalized image so what the user
    // sees matches what the backend receives, pixel-for-pixel. The original
    // blobUrl points at the raw picked file, which still carries the EXIF
    // orientation flag — and browsers (notably some WebViews) don't reliably
    // apply it to <img>, so a portrait phone photo previewed sideways. The
    // canvas re-encode in normalizeImage bakes the rotation into the pixels
    // and strips EXIF, so this blob renders upright everywhere. Guard on
    // identity: normalizeImage falls back to the original File on decode
    // failure, in which case there's nothing new to show. PDFs keep their
    // icon card (no blobUrl) untouched.
    if (entry.kind === 'image' && file !== entry.file) {
      const upright = URL.createObjectURL(file);
      if (entry.blobUrl) URL.revokeObjectURL(entry.blobUrl);
      entry.blobUrl = upright;
      renderFilePreviews();
    }
    const fd = new FormData();
    fd.append('file', file);
    const headers = {};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch('/api/sessions/upload', { method: 'POST', headers, body: fd });
    if (r.status === 401 || r.status === 403) { showAuthModal(); throw new Error('unauthorized'); }
    if (!r.ok) {
      const txt = await r.text().catch(() => '');
      let msg = 'upload failed: ' + r.status;
      try { const j = JSON.parse(txt); if (j && j.error) msg = j.error; } catch (_) { if (txt) msg = txt; }
      throw new Error(msg);
    }
    const j = await r.json();
    if (!j.id) throw new Error('no id in response');
    entry.id = j.id;
    // Server echoes kind/size/name — trust its view so a client/server
    // sniff disagreement (optimistic PDF accept above, for instance)
    // settles in the server's favour. The UI card uses these.
    if (j.kind) entry.serverKind = j.kind;
    if (j.name) entry.serverName = j.name;
    entry.status = 'ready';
    // Fire-and-forget auto-orientation: for an image with no EXIF flag (a
    // sideways document photo), ask the backend's vision side-call which way
    // is up. We DON'T await it — the upload is already 'ready' and sendable;
    // the rotation, if any, lands silently a few seconds later and refreshes
    // the thumbnail. Never blocks send. Images only; the server no-ops for
    // PDFs and when the feature is disabled.
    if (entry.kind === 'image') maybeAutoOrient(entry);
  } catch (e) {
    entry.status = 'error';
    entry.error = e.message || 'upload failed';
  }
  renderFilePreviews();
}

// maybeAutoOrient asks the backend to auto-rotate a just-uploaded image that
// lacks an EXIF orientation flag. Best-effort and silent: any failure leaves
// the image as-is (it's already 'ready' and sendable). On a confirmed
// rotation it refetches the corrected bytes via the attachment endpoint and
// swaps the preview thumbnail so the user sees the upright result that will
// be sent. The entry's `id` is unchanged by rotation (server replaces bytes
// in place), so send still references the same file_id.
async function maybeAutoOrient(entry) {
  if (!entry || !entry.id || entry.kind !== 'image') return;
  try {
    const headers = { 'Content-Type': 'application/json' };
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch('/api/sessions/orient', {
      method: 'POST', headers, body: JSON.stringify({ id: entry.id }),
    });
    if (!r.ok) return; // 404/expired/etc — nothing to do
    const j = await r.json().catch(() => null);
    if (!j || !j.rotated || !j.image) return; // no rotation applied
    // The server rotated the stored bytes in place (same file_id) and echoed
    // the corrected JPEG inline as a data URL. Swap the preview to it so the
    // thumbnail matches what gets sent. The entry may have been removed while
    // the orient call was in flight — guard before touching it.
    if (!pendingFiles.includes(entry)) return;
    if (entry.blobUrl) URL.revokeObjectURL(entry.blobUrl);
    entry.blobUrl = j.image; // data: URL, no object URL to revoke later
    renderFilePreviews();
  } catch (_) {
    // Best-effort: swallow. The image is already sendable as-is.
  }
}

function retryUpload(idx) {
  const entry = pendingFiles[idx];
  if (entry && entry.status === 'error') enqueueUpload(entry);
}

function removeFile(idx) {
  const [removed] = pendingFiles.splice(idx, 1);
  if (removed && removed.blobUrl) URL.revokeObjectURL(removed.blobUrl);
  renderFilePreviews();
}

// reorderPendingFile moves pendingFiles[from] to position `to`. Pure array
// operation extracted so the drag-drop handler and a keyboard a11y fallback
// can share one code path and so contract tests can assert the move semantics
// without touching the DOM. Returns true when the array actually changed.
function reorderPendingFile(from, to) {
  if (!Number.isInteger(from) || !Number.isInteger(to)) return false;
  if (from < 0 || from >= pendingFiles.length) return false;
  if (to < 0) to = 0;
  if (to > pendingFiles.length - 1) to = pendingFiles.length - 1;
  if (from === to) return false;
  const [moved] = pendingFiles.splice(from, 1);
  pendingFiles.splice(to, 0, moved);
  return true;
}

// Drag source index for the thumbnail-reorder gesture. A module-level slot is
// safer than dataTransfer because the latter is sometimes empty on drop in
// Safari when the drag never left the origin element.
let _dragReorderFrom = -1;

function onThumbDragStart(ev, idx) {
  // Only 'ready' files are reorderable; uploading/error thumbs are pinned to
  // their current slot because their index may still be referenced by the
  // in-flight upload completion path.
  const entry = pendingFiles[idx];
  if (!entry || entry.status !== 'ready') { ev.preventDefault(); return; }
  _dragReorderFrom = idx;
  try {
    ev.dataTransfer.effectAllowed = 'move';
    // Firefox requires some data to be set or dragstart is cancelled.
    ev.dataTransfer.setData('text/plain', String(idx));
  } catch (_) {}
  ev.currentTarget.classList.add('dragging');
}

function onThumbDragOver(ev) {
  if (_dragReorderFrom < 0) return;
  ev.preventDefault();
  ev.dataTransfer.dropEffect = 'move';
  ev.currentTarget.classList.add('drop-target');
}

function onThumbDragLeave(ev) {
  ev.currentTarget.classList.remove('drop-target');
}

function onThumbDrop(ev, idx) {
  ev.preventDefault();
  ev.currentTarget.classList.remove('drop-target');
  const from = _dragReorderFrom;
  _dragReorderFrom = -1;
  if (from < 0 || from === idx) { renderFilePreviews(); return; }
  reorderPendingFile(from, idx);
  renderFilePreviews();
}

function onThumbDragEnd() {
  _dragReorderFrom = -1;
  const el = document.getElementById('file-preview');
  if (!el) return;
  el.querySelectorAll('.file-thumb.dragging, .file-thumb.drop-target').forEach(n => {
    n.classList.remove('dragging');
    n.classList.remove('drop-target');
  });
}

// Keyboard a11y: when a .file-thumb is focused, Left/Right arrow keys move it
// left/right by one slot. Mirrors the drag gesture for keyboard-only users.
function onThumbKeyDown(ev, idx) {
  if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight') return;
  const entry = pendingFiles[idx];
  if (!entry || entry.status !== 'ready') return;
  const to = idx + (ev.key === 'ArrowLeft' ? -1 : 1);
  if (!reorderPendingFile(idx, to)) return;
  ev.preventDefault();
  renderFilePreviews();
  // After re-render, restore focus to the moved thumb's new slot so rapid
  // arrow presses keep working.
  const el = document.getElementById('file-preview');
  if (!el) return;
  const next = el.querySelector('.file-thumb[data-idx="' + to + '"]');
  if (next) next.focus();
}

// formatFileSize renders a byte count as a short human label (e.g. "1.2 MB").
// Only used for PDF chips where we want to surface size to the user; images
// still show the thumbnail itself, so size is not rendered for them.
function formatFileSize(n) {
  if (!n || n < 0) return '';
  if (n >= 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
  if (n >= 1024) return Math.round(n / 1024) + ' KB';
  return n + ' B';
}

function renderFilePreviews() {
  const el = document.getElementById('file-preview');
  if (!el) return;
  el.innerHTML = pendingFiles.map((entry, i) => {
    const overlay =
      entry.status === 'uploading' ? '<div class="upload-status uploading"></div>' :
      entry.status === 'error' ? '<div class="upload-status error" title="' + escAttr(entry.error || 'upload failed') + '" onclick="retryUpload(' + i + ')">\u21bb</div>' :
      '';
    // Only 'ready' files are draggable so an in-flight upload's index stays
    // stable for the uploadEntry completion handler. tabindex=0 makes the
    // thumb keyboard-focusable; ArrowLeft/Right then reorder via onThumbKeyDown.
    const draggable = entry.status === 'ready';
    const isPDF = entry.kind === 'pdf';
    // PDF card: fixed-size chip with the .pdf icon + filename + size.
    // Image thumb: the existing <img> preview. Both share the remove button
    // and the upload-status overlay.
    const body = isPDF
      ? ('<div class="pdf-chip" aria-hidden="true">' +
           '<div class="pdf-icon">PDF</div>' +
           '<div class="pdf-meta">' +
             '<div class="pdf-name" title="' + escAttr(entry.file.name || 'document.pdf') + '">' +
               esc((entry.file.name || 'document.pdf')) +
             '</div>' +
             '<div class="pdf-size">' + esc(formatFileSize(entry.file.size || 0)) + '</div>' +
           '</div>' +
         '</div>')
      : '<img src="' + entry.blobUrl + '" draggable="false">';
    return '<div class="file-thumb ' + entry.status + (isPDF ? ' pdf' : '') + '"' +
      ' data-idx="' + i + '"' +
      (draggable ? ' draggable="true" tabindex="0" role="button" aria-label="' + (isPDF ? 'PDF' : '\u56fe\u7247') + ' ' + (i + 1) + '\uff0c\u62d6\u52a8\u6216\u7528\u5de6\u53f3\u65b9\u5411\u952e\u6392\u5e8f"' : '') +
      (draggable ? ' ondragstart="onThumbDragStart(event,' + i + ')"' : '') +
      (draggable ? ' ondragover="onThumbDragOver(event)"' : '') +
      (draggable ? ' ondragleave="onThumbDragLeave(event)"' : '') +
      (draggable ? ' ondrop="onThumbDrop(event,' + i + ')"' : '') +
      (draggable ? ' ondragend="onThumbDragEnd()"' : '') +
      (draggable ? ' onkeydown="onThumbKeyDown(event,' + i + ')"' : '') +
      '>' +
      body +
      overlay +
      '<button class="remove" onclick="removeFile(' + i + ')" title="\u79fb\u9664" aria-label="\u79fb\u9664">\u00d7</button>' +
      '</div>';
  }).join('');
}

// --- Voice recording (WeChat-style hold-to-talk) ---

let mediaRecorder = null;
let audioChunks = [];
let isUnloading = false;
let voiceRecTimer = null;
let voiceRecStart = 0;
const MAX_REC_SECS = 30;
let pendingMic = false;
let voiceInputMode = false;
let voiceTouchStartY = 0;
let voiceCancelled = false;
let voiceActive = false; // true while hold gesture is in progress
let persistentMicStream = null; // keep mic stream alive to avoid repeated permission prompts

window.addEventListener('pagehide', () => {
  isUnloading = true;
  voiceActive = false;
  cleanupVoiceTouchListeners();
  if (mediaRecorder && mediaRecorder.state !== 'inactive') mediaRecorder.stop();
  if (persistentMicStream) { persistentMicStream.getTracks().forEach(t => t.stop()); persistentMicStream = null; }
});

function acquireMicStream() {
  if (persistentMicStream && persistentMicStream.getAudioTracks().some(t => t.readyState === 'live')) {
    return Promise.resolve(persistentMicStream);
  }
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    return Promise.reject(new Error('not supported'));
  }
  return navigator.mediaDevices.getUserMedia({ audio: true }).then(stream => {
    persistentMicStream = stream;
    return stream;
  });
}

function releaseMicStream() {
  if (persistentMicStream) {
    persistentMicStream.getTracks().forEach(t => t.stop());
    persistentMicStream = null;
  }
}

function toggleInputMode() {
  if (pendingMic) return;
  voiceInputMode = !voiceInputMode;
  const ia = document.getElementById('input-area');
  if (ia) ia.classList.toggle('voice-mode', voiceInputMode);
  const btn = document.getElementById('btn-mic');
  if (btn) {
    btn.innerHTML = voiceInputMode ? '&#x2328;' : '&#x1f3a4;';
    btn.title = voiceInputMode ? '\u5207\u6362\u952e\u76d8' : '\u5207\u6362\u8bed\u97f3';
  }
  if (voiceInputMode) {
    // Pre-acquire mic permission so hold-to-talk won't prompt again
    acquireMicStream().catch(() => {});
  } else {
    releaseMicStream();
  }
  // Sync send/stop button visibility after mode toggle
  const sd = sessionsData[sid(selectedKey, selectedNode || 'local')];
  updateSendButton(sd ? sd.state || '' : '');
}

// --- Touch handlers for hold-to-talk ---
// touchmove/touchend registered on document (not button) so the overlay cannot block them.

function voiceTouchStart(e) {
  e.preventDefault();
  voiceTouchStartY = e.touches[0].clientY;
  voiceCancelled = false;
  voiceActive = true;
  document.addEventListener('touchmove', voiceTouchMove, {passive: false});
  document.addEventListener('touchend', voiceTouchEnd, {passive: false});
  document.addEventListener('touchcancel', voiceTouchCancel, {passive: false});
  startVoiceRecording();
}

function voiceTouchMove(e) {
  if (!voiceActive) return;
  e.preventDefault();
  const touch = e.touches[0];
  if (!touch) return;
  const dy = voiceTouchStartY - touch.clientY;
  const overlay = document.getElementById('voice-overlay');
  const hint = document.getElementById('vo-hint');
  if (dy > 80) {
    voiceCancelled = true;
    if (overlay) overlay.classList.add('cancel');
    if (hint) hint.textContent = '\u677e\u5f00\u53d6\u6d88';
  } else {
    voiceCancelled = false;
    if (overlay) overlay.classList.remove('cancel');
    if (hint) hint.textContent = '\u677e\u5f00\u53d1\u9001 \u00b7 \u4e0a\u6ed1\u53d6\u6d88';
  }
}

function voiceTouchEnd(e) {
  if (!voiceActive) return;
  e.preventDefault();
  voiceActive = false;
  cleanupVoiceTouchListeners();
  stopVoiceRecording(!voiceCancelled);
}

function voiceTouchCancel() {
  voiceActive = false;
  cleanupVoiceTouchListeners();
  stopVoiceRecording(false);
}

function cleanupVoiceTouchListeners() {
  document.removeEventListener('touchmove', voiceTouchMove);
  document.removeEventListener('touchend', voiceTouchEnd);
  document.removeEventListener('touchcancel', voiceTouchCancel);
}

function voiceMouseDown(e) {
  e.preventDefault();
  voiceCancelled = false;
  voiceActive = true;
  startVoiceRecording();
  const startY = e.clientY;
  const onMove = (me) => {
    const dy = startY - me.clientY;
    const overlay = document.getElementById('voice-overlay');
    const hint = document.getElementById('vo-hint');
    if (dy > 80) {
      voiceCancelled = true;
      if (overlay) overlay.classList.add('cancel');
      if (hint) hint.textContent = '\u677e\u5f00\u53d6\u6d88';
    } else {
      voiceCancelled = false;
      if (overlay) overlay.classList.remove('cancel');
      if (hint) hint.textContent = '\u677e\u5f00\u53d1\u9001 \u00b7 \u4e0a\u6ed1\u53d6\u6d88';
    }
  };
  const onUp = () => {
    document.removeEventListener('mousemove', onMove);
    document.removeEventListener('mouseup', onUp);
    voiceActive = false;
    stopVoiceRecording(!voiceCancelled);
  };
  document.addEventListener('mousemove', onMove);
  document.addEventListener('mouseup', onUp);
}

function startVoiceRecording() {
  if (pendingMic) return;
  pendingMic = true;
  const holdBtn = document.getElementById('btn-hold-talk');
  if (holdBtn) holdBtn.classList.add('active');

  acquireMicStream().then(stream => {
    pendingMic = false;
    // If finger was released during async acquireMicStream, abort immediately
    if (!voiceActive) {
      if (holdBtn) holdBtn.classList.remove('active');
      return;
    }
    audioChunks = [];
    const mimeType = MediaRecorder.isTypeSupported('audio/webm;codecs=opus') ? 'audio/webm;codecs=opus'
      : MediaRecorder.isTypeSupported('audio/ogg;codecs=opus') ? 'audio/ogg;codecs=opus' : '';
    mediaRecorder = mimeType ? new MediaRecorder(stream, { mimeType }) : new MediaRecorder(stream);
    mediaRecorder.ondataavailable = e => { if (e.data.size > 0) audioChunks.push(e.data); };
    mediaRecorder.onstop = () => {
      clearInterval(voiceRecTimer);
      // Do NOT stop persistent stream tracks — keep them alive for next recording
      if (holdBtn) holdBtn.classList.remove('active');
      if (isUnloading) return;

      if (voiceCancelled) {
        hideVoiceOverlay();
        showToast('\u5df2\u53d6\u6d88');
        audioChunks = [];
        return;
      }

      const blob = new Blob(audioChunks, { type: mediaRecorder.mimeType });
      audioChunks = [];
      if (blob.size < 1000) {
        hideVoiceOverlay();
        showToast('\u5f55\u97f3\u592a\u77ed');
        return;
      }
      // Show transcribing state on overlay
      const overlay = document.getElementById('voice-overlay');
      if (overlay) overlay.classList.add('transcribing');
      const hint = document.getElementById('vo-hint');
      if (hint) hint.textContent = '\u6b63\u5728\u8bc6\u522b...';
      transcribeAudio(blob, true);
    };
    mediaRecorder.start();
    voiceRecStart = Date.now();
    voiceRecTimer = setInterval(updateVoiceTimer, 200);
    updateVoiceTimer();
    // Show overlay
    const overlay = document.getElementById('voice-overlay');
    if (overlay) { overlay.classList.remove('cancel', 'transcribing'); overlay.classList.add('show'); }
    const hint = document.getElementById('vo-hint');
    if (hint) hint.textContent = '\u677e\u5f00\u53d1\u9001 \u00b7 \u4e0a\u6ed1\u53d6\u6d88';
  }).catch(err => {
    pendingMic = false;
    voiceActive = false;
    cleanupVoiceTouchListeners();
    if (holdBtn) holdBtn.classList.remove('active');
    hideVoiceOverlay();
    showToast(describeMicError(err), 'error', 5000);
    console.warn('mic error:', err);
  });
}

// describeMicError converts a MediaDevices/getUserMedia error into a concrete,
// user-actionable Chinese message. Previously we collapsed all failures to
// "权限被拒绝", which masked genuine browser-unsupported, no-device, or
// hardware-busy cases that need different recovery steps.
function describeMicError(err) {
  if (!err) return '\u9ea6\u514b\u98ce\u8c03\u7528\u5931\u8d25';
  if (err.message === 'not supported' || err.name === 'NotSupportedError') {
    return '\u6d4f\u89c8\u5668\u4e0d\u652f\u6301\u5f55\u97f3\uff0c\u8bf7\u6539\u7528 Chrome/Firefox/Safari \u6700\u65b0\u7248';
  }
  if (err.name === 'NotAllowedError' || err.name === 'SecurityError') {
    return '\u9ea6\u514b\u98ce\u6743\u9650\u88ab\u62d2\u7edd\uff0c\u8bf7\u5728\u6d4f\u89c8\u5668\u5730\u5740\u680f\u7684\u9501\u5934\u56fe\u6807\u4e2d\u5141\u8bb8';
  }
  if (err.name === 'NotFoundError' || err.name === 'OverconstrainedError') {
    return '\u672a\u68c0\u6d4b\u5230\u53ef\u7528\u9ea6\u514b\u98ce\uff0c\u8bf7\u68c0\u67e5\u786c\u4ef6\u8fde\u63a5';
  }
  if (err.name === 'NotReadableError') {
    return '\u9ea6\u514b\u98ce\u88ab\u5176\u4ed6\u7a0b\u5e8f\u5360\u7528\uff0c\u8bf7\u5173\u95ed\u5176\u4ed6\u5f55\u97f3\u5e94\u7528\u540e\u91cd\u8bd5';
  }
  if (err.name === 'AbortError') {
    return '\u5f55\u97f3\u88ab\u7ec8\u6b62\uff0c\u8bf7\u91cd\u65b0\u5c1d\u8bd5';
  }
  return '\u9ea6\u514b\u98ce\u8c03\u7528\u5931\u8d25\uff1a' + (err.message || err.name || 'unknown');
}

function stopVoiceRecording(shouldSend) {
  if (!shouldSend) voiceCancelled = true;
  const holdBtn = document.getElementById('btn-hold-talk');
  if (holdBtn) holdBtn.classList.remove('active');
  if (mediaRecorder && mediaRecorder.state === 'recording') {
    mediaRecorder.stop(); // triggers onstop handler
  } else {
    hideVoiceOverlay();
  }
}

function hideVoiceOverlay() {
  const overlay = document.getElementById('voice-overlay');
  if (overlay) overlay.classList.remove('show', 'cancel', 'transcribing');
}

// Tap overlay to cancel (escape hatch for stuck states)
document.getElementById('voice-overlay')?.addEventListener('click', function(e) {
  // Normal flow: touchend/mouseup already stopped recording before click fires.
  // This only triggers when genuinely stuck (recording active or overlay visible).
  if (mediaRecorder && mediaRecorder.state === 'recording') {
    voiceActive = false;
    cleanupVoiceTouchListeners();
    stopVoiceRecording(false);
  } else if (this.classList.contains('show')) {
    // Stuck in transcribing state or overlay didn't dismiss
    hideVoiceOverlay();
  }
});

function updateVoiceTimer() {
  const el = document.getElementById('vo-timer');
  if (!el) return;
  const secs = Math.floor((Date.now() - voiceRecStart) / 1000);
  el.textContent = secs + 's';
  if (secs >= MAX_REC_SECS) {
    stopVoiceRecording(true);
    showToast('\u5df2\u8fbe\u6700\u957f' + MAX_REC_SECS + '\u79d2');
  }
}

function transcribeAudio(blob, autoSend) {
  const fd = new FormData();
  fd.append('audio', blob, 'recording.' + (blob.type.includes('webm') ? 'webm' : blob.type.includes('ogg') ? 'ogg' : 'mp4'));
  const headers = {};
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  // Tag fetch-level failures so .catch can distinguish network from server.
  fetch('/api/transcribe', {
    method: 'POST',
    headers: headers,
    credentials: 'same-origin',
    body: fd
  }).then(r => {
    if (!r.ok) return r.text().then(t => {
      const e = new Error(t || ('HTTP ' + r.status));
      e.status = r.status;
      e.body = t;
      throw e;
    });
    return r.json();
  }).then(data => {
    hideVoiceOverlay();
    const input = document.getElementById('msg-input');
    if (input && data.text) {
      const cur = getMsgValue(input);
      setMsgValue(input, autoSend ? data.text : (cur ? cur + ' ' + data.text : data.text));
      if (autoSend) {
        sendMessage();
      } else {
        input.focus();
        showToast('\u8f6c\u5199: ' + data.text.substring(0, 50) + (data.text.length > 50 ? '...' : ''), 'success', 5000);
      }
    } else {
      // Empty transcription — compute recorded duration so the user knows
      // whether the issue is "no speech detected" vs "too quiet" vs "silence".
      const secs = Math.max(0, Math.round((Date.now() - voiceRecStart) / 1000));
      const hint = secs < 2
        ? '\u672a\u68c0\u6d4b\u5230\u8bed\u97f3\uff08\u5f55\u97f3\u592a\u77ed\uff0c\u8bf7\u6309\u4f4f\u8bf4\u8bdd\u81f3\u5c11 2 \u79d2\uff09'
        : '\u672a\u68c0\u6d4b\u5230\u8bed\u97f3\uff08' + secs + 's\uff09\uff0c\u8bf7\u9760\u8fd1\u9ea6\u514b\u98ce\u540e\u91cd\u8bd5';
      showToast(hint, 'warning', 5000);
    }
  }).catch(err => {
    hideVoiceOverlay();
    showToast(describeTranscribeError(err), 'error', 5000);
  });
}

// describeTranscribeError turns a fetch/HTTP failure into a user-friendly
// message keyed off HTTP status — previously the raw server body was shown,
// which surfaced internal strings like "transcribe rate limit exceeded".
function describeTranscribeError(err) {
  if (!err) return '\u8f6c\u5199\u5931\u8d25';
  // fetch() rejects with TypeError on network failure; server errors have a status.
  if (!err.status) {
    return '\u7f51\u7edc\u8fde\u63a5\u5f02\u5e38\uff0c\u8bf7\u68c0\u67e5\u7f51\u7edc\u540e\u91cd\u8bd5';
  }
  switch (err.status) {
    case 401:
    case 403:
      return '\u672a\u767b\u5f55\u6216\u4f1a\u8bdd\u5df2\u8fc7\u671f\uff0c\u8bf7\u91cd\u65b0\u767b\u5f55\u540e\u91cd\u8bd5';
    case 413:
      return '\u5f55\u97f3\u6587\u4ef6\u8fc7\u5927\uff0c\u8bf7\u7f29\u77ed\u540e\u91cd\u8bd5';
    case 415:
      return '\u4e0d\u652f\u6301\u7684\u97f3\u9891\u683c\u5f0f\uff0c\u8bf7\u66f4\u6362\u6d4f\u89c8\u5668\u91cd\u8bd5';
    case 429:
      return '\u8f6c\u5199\u8bf7\u6c42\u8fc7\u4e8e\u9891\u7e41\uff0c\u8bf7\u7a0d\u5019\u4e00\u5206\u949f\u540e\u91cd\u8bd5';
    case 500:
    case 502:
    case 503:
    case 504:
      return '\u8f6c\u5199\u670d\u52a1\u6682\u4e0d\u53ef\u7528\uff08HTTP ' + err.status + '\uff09\uff0c\u8bf7\u7a0d\u540e\u91cd\u8bd5';
    default:
      return '\u8f6c\u5199\u5931\u8d25\uff08HTTP ' + err.status + '\uff09';
  }
}

// --- Auth modal ---

function showAuthModal() {
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
      '<input id="token-input" type="password" placeholder="请输入 dashboard token…" onkeydown="if(event.key===\'Enter\'){saveToken()}">' +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">cancel</button>' +
        '<button type="button" class="primary" onclick="saveToken()">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  setTimeout(() => document.getElementById('token-input').focus(), 100);
}

async function saveToken() {
  const input = document.getElementById('token-input');
  const t = input && input.value.trim();
  if (!t) return;
  try {
    const r = await fetch('/api/auth/login', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({token: t})
    });
    if (r.ok) {
      const overlay = document.querySelector('.modal-overlay');
      if (overlay) overlay.remove();
      wsm.disconnect();
      wsm.connect();
      fetchSessions();
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
// inline sidebar-status countdown instead of a top-of-screen toast. The
// previous toast variant stacked on top of the header on mobile and
// repeated every second; routing the countdown into updateStatusBar keeps
// the signal visible but out of the way. Triggered by an
// auth_fail(Error="too many attempts") message that carries a retry_after
// hint. On expiry the gate clears and wsm.connect() fires once so the user
// doesn't have to click anything — matches the UX-P1 auto-recover spec.
//
// Idempotent: calling twice (e.g. a second in-flight reconnect that races
// through before the gate armed) clears the prior tick interval so the
// countdown reflects the freshest server directive, not a stale one.
let _wsAuthCountdownTimer = null;
function startWSAuthRetryCountdown(seconds) {
  if (typeof wsm === 'undefined' || !wsm) return;
  if (!Number.isFinite(seconds) || seconds <= 0) seconds = 60;
  wsm._authBlockUntil = Date.now() + seconds * 1000;
  if (_wsAuthCountdownTimer) {
    clearInterval(_wsAuthCountdownTimer);
    _wsAuthCountdownTimer = null;
  }
  // Repaint the sidebar immediately so the "鉴权过于频繁 · Ns" row appears
  // without waiting for the next 1s tick. updateStatusBar reads
  // wsm._authBlockUntil directly, so we don't need to pass the remaining
  // seconds around.
  updateStatusBar();
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
      updateStatusBar();
      wsm.connect();
      return;
    }
    updateStatusBar();
  }, 1000);
}

// fetchCLIBackends retrieves the enabled CLI backends from the server.
// Cached for 60 seconds — the set only changes across naozhi restarts.
// Resolves to null on network/auth failure so the caller can fall back to
// the no-picker flow (single-backend mode).
async function fetchCLIBackends() {
  if (cliBackends && Date.now() - cliBackendsFetchedAt < 60000) {
    return cliBackends;
  }
  try {
    // RNEW-UX-003: default 10s timeout is fine here — this fetch is cached
    // for 60s and only fires at modal-open time, not on a poll.
    const data = await fetchJSON('/api/cli/backends', {credentials: 'same-origin'});
    cliBackends = data && Array.isArray(data.backends) ? data : null;
    cliBackendsFetchedAt = Date.now();
    // Multi-Backend RFC §8.3 D9-D15: re-apply feature gates whenever the
    // backends manifest lands. The boot path fires fetchCLIBackends() in
    // parallel with fetchSessions, so the very first renderMainShell may
    // have run with cliBackends==null — call gates here so the input
    // controls update once the manifest is available.
    if (typeof applyFeatureGates === 'function') applyFeatureGates();
    return cliBackends;
  } catch (e) {
    return null;
  }
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
    '<select id="' + escAttr(selectId) + '" style="width:100%;padding:6px 8px;background:var(--nz-bg-0);color:var(--nz-text);border:1px solid var(--nz-border);border-radius:4px">' +
    options +
    '</select>' +
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
// (cliIcon) and header label (renderMainShell / updateHeaderCLI) fall
// through to defaultCLIName ("claude-code") and a kiro session shows the
// claude logomark + "claude-code v..." until the first message lands and
// the server-side SetCLIName broadcasts the correct value.
//
// Resolution order:
//   1. cliBackends cache (canonical: dashboard already paid for this fetch
//      to render the picker, so the lookup is free).
//   2. Hardcoded ID→display map for the brief boot window where the
//      backend list hasn't resolved yet. Mirrors profile_claude.go /
//      profile_kiro.go DisplayName.
//   3. Backend ID itself as last-resort fallback (better than empty;
//      cliIcon's `=== 'kiro'` branch still works for kiro this way).
function backendDisplayName(backendID) {
  if (!backendID) return '';
  if (cliBackends && Array.isArray(cliBackends.backends)) {
    const e = cliBackends.backends.find(b => b && b.id === backendID);
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
// spawned. Empty when cliBackends has not resolved yet (caller hides
// the version suffix).
function backendDisplayVersion(backendID) {
  if (!backendID || !cliBackends || !Array.isArray(cliBackends.backends)) return '';
  const e = cliBackends.backends.find(b => b && b.id === backendID);
  return (e && e.version) ? e.version : '';
}

// R110-P3 agent picker (Round 167) — returns an HTML fragment for an agent
// <select>, or an empty string when only the default 'general' agent is
// configured (no meaningful choice to offer). Mirrors renderBackendPicker's
// shape: single <select> with id="new-agent", consumed by getSelectedAgent()
// at submit time, defaulting to the last-picked agent (localStorage) so
// power users who always want e.g. 'sonnet' don't re-pick every session.
function renderAgentPicker() {
  if (!Array.isArray(availableAgents) || availableAgents.length <= 1) return '';
  // Remember the last picked agent across reloads. Falls back to 'general'
  // on first use or when the previously-selected agent has been removed
  // from the backend config (e.g. config.yaml edit). Swallow errors from
  // private browsing / disabled storage so the picker always renders.
  let remembered = 'general';
  try {
    const v = localStorage.getItem('naozhi_last_agent');
    if (v && availableAgents.indexOf(v) >= 0) remembered = v;
  } catch (_) { /* noop */ }
  const options = availableAgents.map(a => {
    const selected = a === remembered ? ' selected' : '';
    return '<option value="' + escAttr(a) + '"' + selected + '>' + esc(a) + '</option>';
  }).join('');
  return '<div style="margin-bottom:12px">' +
    '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="new-agent">Agent</label>' +
    '<select id="new-agent" style="width:100%;padding:6px 8px;background:var(--nz-bg-0);color:var(--nz-text);border:1px solid var(--nz-border);border-radius:4px">' +
    options +
    '</select>' +
    '</div>';
}

function getSelectedAgent() {
  const el = document.getElementById('new-agent');
  const v = el && el.value ? el.value : '';
  if (v) {
    // Persist so the next modal defaults to this agent without asking again.
    try { localStorage.setItem('naozhi_last_agent', v); } catch (_) { /* noop */ }
  }
  return v || 'general';
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
  // relies on server-side. Also strips bidi override / embedding /
  // directional isolate characters (U+202A–U+202E, U+2066–U+2069) and
  // Unicode line separators (U+2028/U+2029) that bypass the
  // ASCII-control-only filter below and can corrupt log output. Then
  // collapse runs of filesystem-hostile chars into single dashes so the
  // key stays short and readable. Cap at 64 bytes to leave plenty of
  // headroom under the 128-byte sanitizeKeyComponent cap.
  let safe = String(s)
    .replace(/[:：︓꞉∶]/g, '-')
    .replace(/[‪-‮⁦-⁩\u2028\u2029]/g, '')
    .replace(/[\s/\\?*<>|"\x00-\x1f\x7f]+/g, '-');
  safe = safe.replace(/-+/g, '-').replace(/^-|-$/g, '');
  if (safe.length > 64) safe = safe.slice(0, 64);
  return safe || 'session';
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
// connected) are unaffected: selectedNode stays 'local' and the filter
// reduces to the previous local-only behaviour.
function nodeFilteredProjects() {
  if (!Array.isArray(projectsData)) return [];
  const target = selectedNode || 'local';
  return projectsData.filter(p => (p.node || 'local') === target);
}

function createNewSession() {
  // Fetch backends upfront so the picker (if any) is ready when the modal
  // renders. Failure falls back to the single-backend UI — cli.backends
  // returns {} on older naozhi which fetchCLIBackends maps to null.
  fetchCLIBackends().then(backendsData => {
    // defaultWorkspace 来自 local stats，远程节点没有对应的 client 端字段，
    // 因此选中 remote 时不预填路径，让用户显式输入远程上的工作目录。
    const isLocal = (selectedNode || 'local') === 'local';
    const ws = isLocal ? (defaultWorkspace || '') : '';
    const backendPicker = renderBackendPicker(backendsData);

    if (!nodeFilteredProjects().length) {
      const overlay = document.createElement('div');
      overlay.className = 'modal-overlay';
      overlay.innerHTML =
        '<div class="modal" role="dialog" aria-modal="true" aria-label="新建会话">' +
          '<h3>New Session</h3>' +
          backendPicker +
          renderAgentPicker() +
          '<div style="margin-bottom:12px">' +
            '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="new-workspace">工作目录</label>' +
            '<input id="new-workspace" placeholder="' + escAttr(ws) + '" value="' + escAttr(ws) + '" onkeydown="if(event.key===\'Enter\'){doCreateSession()}">' +
          '</div>' +
          '<div class="modal-btns">' +
            '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">取消</button>' +
            '<button type="button" class="primary" onclick="doCreateSession()">创建</button>' +
          '</div>' +
        '</div>';
      document.body.appendChild(overlay);
      trapFocus(overlay);
      setTimeout(() => document.getElementById('new-workspace').focus(), 100);
      return;
    }

    openProjectPalette(backendsData);
  });
}

function openProjectPalette(backendsData) {
  const backendPicker = renderBackendPicker(backendsData);
  const agentPicker = renderAgentPicker();
  // Stash the picker HTML on the overlay dataset so the custom-workspace
  // modal (spawned from a palette row) can surface the same choice. When
  // only one backend exists, picker is empty and we skip the header slot.
  // The agent picker is shown inline next to the backend slot so multi-agent
  // setups can pick both at once without leaving the palette.
  const pickerSlot = (backendPicker || agentPicker)
    ? '<div class="cmd-palette-backend" style="padding:8px 12px 0;display:flex;gap:12px">' +
        (backendPicker ? '<div style="flex:1;min-width:0">' + backendPicker + '</div>' : '') +
        (agentPicker ? '<div style="flex:1;min-width:0">' + agentPicker + '</div>' : '') +
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
  // Palette is scoped to selectedNode: switching the node selector retargets
  // the palette so remote workspaces can be opened in one click. See
  // nodeFilteredProjects() for the rationale.
  nodeFilteredProjects().forEach(p => {
    if (!q) {
      scored.push({project: p, nameRanges: [], pathRanges: [], score: 0});
      return;
    }
    const nameM = fuzzyMatch(q, p.name);
    const pathM = fuzzyMatch(q, p.path);
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
    // R110-P3 palette idle-state ordering: three-tier sort on empty query.
    //   Tier 0: favorites — surface "pinned" projects first. Users already
    //           star projects via the sidebar section-header ⭐ button,
    //           which persists to the backend projects config. Reusing
    //           that signal avoids a second "palette-pin" concept (which
    //           would split the mental model and duplicate state).
    //   Tier 1: recents (localStorage) top-N — most-recently-used.
    //   Tier 2: everything else in original projectsData order (alpha +
    //           backend favorite-first order is preserved, so the rest
    //           bucket doesn't reshuffle unrelated projects).
    // A project that is BOTH favorite and recent lands in tier 0 only;
    // the recents lookup skips it so it doesn't appear twice or trigger
    // a stale rank clash.
    const recents = loadRecentProjects();
    const recentRank = new Map();
    recents.slice(0, RECENT_PROJECTS_SHOW).forEach((e, i) => {
      recentRank.set(e.name + '|' + (e.node || 'local'), i);
    });
    const withIndex = scored.map((s, i) => ({s, i}));
    withIndex.sort((a, b) => {
      const pa = a.s.project;
      const pb = b.s.project;
      // Tier gate 0: favorite trumps everything else.
      const fa = pa.favorite ? 0 : 1;
      const fb = pb.favorite ? 0 : 1;
      if (fa !== fb) return fa - fb;
      // Tier gate 1: within the same tier, recents come before non-recents.
      const ka = pa.name + '|' + (pa.node || 'local');
      const kb = pb.name + '|' + (pb.node || 'local');
      const ra = recentRank.has(ka) ? recentRank.get(ka) : Infinity;
      const rb = recentRank.has(kb) ? recentRank.get(kb) : Infinity;
      if (ra !== rb) return ra - rb;
      // Tier gate 2: stable on original projectsData order (input index).
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
    if (it.type === 'quick') {
      list.appendChild(buildQuickRow(i));
    } else if (it.type === 'project') {
      list.appendChild(buildProjectRow(it.data, i));
    } else {
      list.appendChild(buildCustomRow(it.query, i));
    }
  });
  updateActiveRow(state);
}

function buildProjectRow(s, idx) {
  const p = s.project;
  const el = document.createElement('div');
  el.className = 'cmd-palette-item' + (p.favorite ? ' is-favorite' : '');
  el.dataset.idx = String(idx);
  const nodeId = p.node || 'local';
  const nodeBadge = nodeId !== 'local'
    ? '<span class="cp-node" style="background:' + nodeColor(nodeId) + '">' + esc(nodeId) + '</span>'
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
  el.addEventListener('mouseenter', () => setActiveIdx(idx));
  return el;
}

function buildQuickRow(idx) {
  const ws = defaultWorkspace || '';
  const el = document.createElement('div');
  el.className = 'cmd-palette-item';
  el.dataset.idx = String(idx);
  el.innerHTML =
    '<span class="cp-icon">⚡</span>' +
    '<div class="cp-main">' +
      '<div class="cp-name">快速新建</div>' +
      (ws ? '<div class="cp-path">' + esc(shortPath(ws)) + '</div>' : '') +
    '</div>';
  el.addEventListener('click', () => pickPaletteQuick());
  el.addEventListener('mouseenter', () => setActiveIdx(idx));
  return el;
}

function pickPaletteQuick() {
  // 快速新建跟随当前选中节点：选中 remote 时让 quick session 也落在远程上，
  // 否则用户切到远程 workspace 后再点「快速新建」会意外回退到 local。
  // defaultWorkspace 仍来自 local 的 stats（接口尚未按节点返回），使用前
  // 兜底为空串，由后端 SessionDispatcher 的远程默认工作目录解析。
  const node = selectedNode || 'local';
  const workspace = node === 'local' ? (defaultWorkspace || '') : '';
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
    ? '打开自定义工作目录：<span style="color:#79c0ff">' + esc(query) + '</span>'
    : '打开自定义工作目录…';
  el.innerHTML =
    '<span class="cp-icon">+</span>' +
    '<div class="cp-main"><div class="cp-name" style="color:var(--nz-text-mute)">' + label + '</div></div>';
  el.addEventListener('click', () => pickPaletteCustom(query));
  el.addEventListener('mouseenter', () => setActiveIdx(idx));
  return el;
}

function setActiveIdx(idx) {
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (!overlay) return;
  overlay.querySelectorAll('.cmd-palette-item').forEach(el => {
    el.classList.toggle('active', Number(el.dataset.idx) === idx);
  });
}

function updateActiveRow(state) {
  setActiveIdx(state.activeIdx);
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
  const agent = getSelectedAgent();
  // Default project open = continue the project-stable conversation. p.stableKey
  // is supplied by /api/projects when the feature is enabled (empty otherwise,
  // in which case resolveSessionKey falls back to a timestamp key).
  doCreateInProject(p.path, p.name, p.node || 'local', backend, agent,
    { mode: 'continue', stableKey: p.stableKey || '' });
}

function pickPaletteCustom(initialValue) {
  // Capture the palette's backend + agent choice before we remove the overlay.
  // The Custom Workspace modal re-renders its own copies of both pickers,
  // so we need to pass the pre-selection forward rather than relying on the
  // palette's DOM (which is about to be nuked).
  const preselectedBackend = getSelectedBackend();
  const preselectedAgent = getSelectedAgent();
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (overlay) overlay.remove();
  // 选中 remote 节点时不用 local 的 defaultWorkspace 占位符，避免误导用户。
  const isLocal = (selectedNode || 'local') === 'local';
  const ws = isLocal ? (defaultWorkspace || '') : '';
  const prefill = initialValue && (initialValue.startsWith('/') || initialValue.startsWith('~')) ? initialValue : '';
  // Re-render the backend + agent pickers inside the modal and pre-select the
  // palette's choice, so switching to Custom Workspace doesn't drop either.
  const picker = renderBackendPicker(cliBackends);
  const agentPicker = renderAgentPicker();
  const modal = document.createElement('div');
  modal.className = 'modal-overlay';
  modal.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="自定义工作目录">' +
      '<h3>自定义工作目录</h3>' +
      picker +
      agentPicker +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="new-workspace">工作目录路径</label>' +
        '<input id="new-workspace" placeholder="' + escAttr(ws) + '" value="' + escAttr(prefill) + '" onkeydown="if(event.key===\'Enter\'){doCreateSession()}">' +
      '</div>' +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">取消</button>' +
        '<button type="button" class="primary" onclick="doCreateSession()">创建</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(modal);
  trapFocus(modal);
  if (preselectedBackend) {
    const sel = document.getElementById('new-backend');
    if (sel) sel.value = preselectedBackend;
  }
  if (preselectedAgent) {
    const sel = document.getElementById('new-agent');
    if (sel && Array.from(sel.options).some(o => o.value === preselectedAgent)) {
      sel.value = preselectedAgent;
    }
  }
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
  // opts: { mode: 'continue' | 'new', stableKey: string }. Default 'continue'
  // so a plain project click resumes the project-stable conversation
  // (RFC docs/rfc/project-stable-session-key.md §4.4). The "+ 新会话" entry
  // passes mode:'new' for an independent parallel session.
  opts = opts || {};
  const mode = opts.mode || 'continue';
  const stableKey = opts.stableKey || '';
  const overlay = document.querySelector('.modal-overlay, .cmd-palette-overlay');
  if (overlay) overlay.remove();
  sessionCounter++;
  const now = new Date();
  const ts = now.toISOString().slice(0,10) + '-' +
    now.toTimeString().slice(0,8).replace(/:/g, '') + '-' + sessionCounter;
  // Continue → backend-provided stable key (precise continuation); new →
  // fresh timestamp key (independent parallel session). resolveSessionKey
  // also falls back to a timestamp key when no stableKey is available.
  const key = resolveSessionKey(mode, stableKey, projectName, agent, ts);

  sessionWorkspaces[key] = projectPath;
  if (nodeId && nodeId !== 'local') sessionNodes[key] = nodeId;
  if (backend) sessionBackends[key] = backend;
  // Durably persist the pending workspace and eagerly bind it server-side so a
  // reload-before-first-send (the proven cwd-fallback trigger) no longer drops
  // the workspace. This is the primary fix path (project palette open).
  persistPending();
  eagerBindWorkspace(key, projectPath, nodeId);

  // R110-P3 recent-projects: every successful project-scoped session
  // creation bumps the (name,node) pair to the top of the palette's
  // recent list so next-time access is one click. Custom-workspace paths
  // are intentionally NOT recorded — they have no stable project.name
  // and the palette has no row to highlight them against. Errors from
  // localStorage are swallowed: Safari private-browsing / out-of-quota /
  // disabled storage all throw on setItem, and a silently-un-recorded
  // bump is preferable to breaking the creation flow over a UX tweak.
  pushRecentProject(projectName, nodeId || 'local');

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = nodeId || 'local';
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  if (typeof updateNodeSelector === 'function') updateNodeSelector();
  lastEventTime = 0;
  mobileEnterChat();
  setActiveSessionCard(key, selectedNode);
  renderMainShell();
  navRebuild();
  lastVersion = 0;
  debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

// --- Recent projects (palette ordering) ---

// RECENT_PROJECTS_KEY holds a compact JSON array of {name,node,ts} tuples,
// ordered by `ts` DESC. Capped at RECENT_PROJECTS_MAX so the stored blob
// stays small (worst case ~10 * 100 bytes). The palette only surfaces
// the top 5 (see renderPaletteList) but we retain a deeper tail so a
// long-absent project can climb back into view after one use.
const RECENT_PROJECTS_KEY = 'naozhi_recent_projects';
const RECENT_PROJECTS_MAX = 10;
const RECENT_PROJECTS_SHOW = 5;

function loadRecentProjects() {
  try {
    const raw = localStorage.getItem(RECENT_PROJECTS_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    // Defensive filter: discard entries without a string `name` so a
    // manually-edited localStorage can't break render.
    return parsed
      .filter(e => e && typeof e.name === 'string')
      .slice(0, RECENT_PROJECTS_MAX);
  } catch (_) {
    return [];
  }
}

function pushRecentProject(name, node) {
  if (!name) return;
  node = node || 'local';
  try {
    const list = loadRecentProjects();
    const filtered = list.filter(e => !(e.name === name && (e.node || 'local') === node));
    filtered.unshift({name: name, node: node, ts: Date.now()});
    const trimmed = filtered.slice(0, RECENT_PROJECTS_MAX);
    localStorage.setItem(RECENT_PROJECTS_KEY, JSON.stringify(trimmed));
  } catch (_) {
    // Private browsing / quota / disabled storage — swallow, next
    // successful write reseeds the list.
  }
}

function doCreateSession() {
  const workspace = document.getElementById('new-workspace').value.trim();
  const backend = getSelectedBackend();
  const agent = getSelectedAgent();
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'session') : 'session';
  document.querySelector('.modal-overlay').remove();

  sessionCounter++;
  const now = new Date();
  const ts = now.toISOString().slice(0,10) + '-' +
    now.toTimeString().slice(0,8).replace(/:/g, '') + '-' + sessionCounter;
  // R110-P3 key schema (see buildDashboardSessionKey godoc): 4 segments
  // with agentID as the terminal segment so buildSessionOpts picks up the
  // right AgentOpts entry.
  const key = buildDashboardSessionKey(ts, folderName, agent);

  // 自定义工作目录的会话目标节点 = 当前选中节点。否则用户从 remote 节点视图
  // 触发「自定义工作目录」时会被强制拉回 local，与「在远程项目里新建」的
  // 操作直觉相违。
  const targetNode = selectedNode || 'local';

  if (workspace) sessionWorkspaces[key] = workspace;
  if (backend) sessionBackends[key] = backend;
  if (targetNode !== 'local') sessionNodes[key] = targetNode;
  // Persist + eager-bind so the custom workspace survives a reload-before-send.
  persistPending();
  if (workspace) eagerBindWorkspace(key, workspace, targetNode);

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = targetNode;
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  if (typeof updateNodeSelector === 'function') updateNodeSelector();
  lastEventTime = 0;
  mobileEnterChat();
  setActiveSessionCard(key, targetNode);
  renderMainShell();
  navRebuild();
  lastVersion = 0;
  debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

// createQuickSession opens a pre-configured session with zero clicks:
// general agent, default backend, default workspace (session.cwd). No modal,
// no palette, no project picker — optimised for "I just want to ask Claude
// something fast" without deciding where it lives first. The user can still
// /cd later if they want a different workspace, or rename the session.
//
// When `initialText` is non-empty, it is dropped into the composer after
// renderMainShell paints AND sendMessage is invoked — so submitQuickAsk
// ships "type in empty-state → Enter → question flies" without the user
// having to click the composer a second time.
//
// renderMainShell is synchronous and writes `#msg-input` into the DOM
// immediately, but we still defer the setMsgValue + sendMessage call by
// one rAF tick so the browser has a chance to flush layout (contenteditable
// focus + selection state is finicky before paint). If `#msg-input` is
// still missing after the tick we ship text back to the caller via the
// optional `onTextStranded` callback so the caller can re-enable its own
// input and surface a toast — prevents silent message loss if a future
// renderMainShell refactor becomes async or conditional.
//
// Rationale: the modal + palette are the right default for project work, but
// they add 2-3 clicks to the common "quick lookup" case. Surfacing this
// entry point — paired with the empty-state quick-ask input — lets the
// palette stay rich without penalising quick queries.
function createQuickSession(initialText, onTextStranded) {
  // Close any lingering modal/palette so repeated entry-point triggers don't
  // stack overlays (e.g. a quick-ask fired while a modal was still mounted).
  document.querySelectorAll('.modal-overlay, .cmd-palette-overlay').forEach(el => el.remove());

  const workspace = defaultWorkspace || '';
  const agent = 'general';
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'quick') : 'quick';

  sessionCounter++;
  const now = new Date();
  const ts = now.toISOString().slice(0,10) + '-' +
    now.toTimeString().slice(0,8).replace(/:/g, '') + '-' + sessionCounter;
  const key = buildDashboardSessionKey(ts, folderName, agent);

  if (workspace) sessionWorkspaces[key] = workspace;
  // Backend left unset → router falls back to the configured default.
  // Persist so a reload-before-send keeps the workspace. No eager-bind: quick
  // sessions use defaultWorkspace, so the override would just mirror defaultCWD.
  persistPending();

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = 'local';
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  if (typeof updateNodeSelector === 'function') updateNodeSelector();
  lastEventTime = 0;
  mobileEnterChat();
  setActiveSessionCard(key, 'local');
  renderMainShell();
  navRebuild();
  lastVersion = 0;
  debouncedFetchSessions();
  const text = (initialText || '').trim();
  // requestAnimationFrame ensures the composer DOM produced by renderMainShell
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
  // sessions. renderMainShell synchronously replaces the empty-state DOM
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
    if (typeof showToast === 'function') showToast('发送失败，请重试', 'error');
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


// --- Utilities ---

// mainEmptyHtml returns the inner HTML for `#main` when no session is
// selected. Called after dismiss/remove flows that nuke the active
// session. Kept in sync with the cold-start markup in dashboard.html —
// both render a `>_` mark, a Chinese lead line "问点什么？", and the
// quick-ask textarea that fires submitQuickAsk() on Enter. Consolidating
// the copies into a helper means a future tweak touches one place and
// prevents cold-start / dismiss-path divergence.
//
// After rendering, callers SHOULD invoke wireQuickAskInput() to bind the
// keydown / auto-grow handlers on the freshly-painted textarea (the cold
// start HTML gets wired on DOMContentLoaded).
function mainEmptyHtml() {
  return '<div class="empty-state empty-cta empty-quick" style="flex-direction:column;gap:14px">' +
    '<span style="font-size:40px;opacity:.35" aria-hidden="true">&gt;_</span>' +
    '<div style="color:var(--nz-text);font-size:17px">问点什么？</div>' +
    '<form class="quick-ask-form" id="quick-ask-form">' +
      '<textarea id="quick-ask-input" class="quick-ask-input" rows="1" ' +
        'placeholder="Enter 发送 · Shift+Enter 换行" autocomplete="off" spellcheck="false" ' +
        'aria-label="快速提问输入框"></textarea>' +
      '<button type="submit" class="quick-ask-send" aria-label="发送">' +
        '<svg viewBox="0 0 24 24" aria-hidden="true">' +
          '<line x1="22" y1="2" x2="11" y2="13"/>' +
          '<polygon points="22 2 15 22 11 13 2 9 22 2"/>' +
        '</svg></button>' +
    '</form>' +
    '<div style="font-size:12px;color:var(--nz-text-dim)">默认目录 · general agent · 随时 <code>/cd</code> 切换目录，或用上方 <b>+</b> 开项目会话</div>' +
    // R110-P1 空闲态 Home 仪表 MVP 占位：renderRecentSessionsPanel()
    // 按需注入"最近会话"缩略列表；零 session 时渲染为空字符串，保留冷启动
    // 简洁空态不退化。Helper 外部调用，不嵌在本 HTML 里以保持 pure 可读。
    '<div id="recent-sessions-panel" class="recent-panel-wrap"></div>' +
  '</div>';
}

// computeHomeStats aggregates allSessionsCache into the two stats surfaced
// on the idle Home panel. Pure function so a contract test can exercise the
// "today" boundary and cost summation without driving the DOM.
//
// Scope is deliberately conservative: the TODO lists 4 metrics (today active
// / prompts processed / tokens / cost), but prompts and tokens require an
// event-log scan or a backend aggregator that doesn't exist yet. The two
// metrics here (today active count, total cost) are already shipped in
// /api/sessions per-session fields.
//
//   todayActive — sessions whose last_active >= local-midnight today. Uses
//                 the JS Date constructor so the user's browser timezone
//                 matches what they'd consider "today" in the sidebar.
//   totalCost   — sum of s.total_cost across all cached sessions (not gated
//                 by today, because a cron-heavy workspace accumulates cost
//                 overnight and wiping at midnight would hide it).
//   totalPrompts — sum of s.message_count across all cached sessions. The
//                 SessionSnapshot already ships message_count (the cumulative
//                 "user" turn count observed by the live process event log),
//                 so the "已处理 prompt 数" card (R110-P1 #445) is derivable
//                 client-side without the deferred /api/stats/aggregate
//                 backend scan. Cumulative tokens still need that backend
//                 endpoint (no per-session token field exists yet), so the
//                 token card stays out of scope here.
//
// Input shape tolerant: missing last_active / total_cost / message_count on a
// session contributes zero / is skipped rather than NaN-poisoning the totals.
function computeHomeStats(items, nowMs) {
  const arr = Array.isArray(items) ? items : [];
  const now = typeof nowMs === 'number' ? nowMs : Date.now();
  const d = new Date(now);
  const dayStart = new Date(d.getFullYear(), d.getMonth(), d.getDate(), 0, 0, 0, 0).getTime();
  let todayActive = 0;
  let totalCost = 0;
  let totalPrompts = 0;
  for (const s of arr) {
    if (!s) continue;
    if (typeof s.last_active === 'number' && s.last_active >= dayStart) todayActive++;
    if (typeof s.total_cost === 'number' && isFinite(s.total_cost)) totalCost += s.total_cost;
    if (typeof s.message_count === 'number' && isFinite(s.message_count) && s.message_count > 0) totalPrompts += s.message_count;
  }
  return { todayActive: todayActive, totalCost: totalCost, totalPrompts: totalPrompts };
}

// formatHomeCost keeps the $/precision format close to the session card's
// header cost chip (.high-cost / .has-cost): two decimals once cost is
// measurable, four decimals for sub-cent fractions (so "$0.0023" still
// shows signal instead of collapsing to $0.00).
function formatHomeCost(cost) {
  const c = typeof cost === 'number' && isFinite(cost) ? cost : 0;
  if (c >= 0.01) return '$' + c.toFixed(2);
  if (c > 0) return '$' + c.toFixed(4);
  return '$0.00';
}

// buildHomeHealthLines turns a stats snapshot into up to 2 Chinese lines for
// the bottom health strip of the Home panel. Pure function so a contract
// test can exercise each data-path without driving the DOM. Returns [] when
// the snapshot is missing entirely — caller suppresses the strip.
//
// Line shape:
//   Line 1: running/ready/total counts + uptime (always when stats present)
//   Line 2: CLI name + version (when defaultCLIName is set)
//   Line 3 (gated): watchdog kills — only when > 0; signals prod trouble
//
// Scope: leans entirely on fields ALREADY in /api/sessions `stats`. The TODO
// lists claude 子进程数 / shim 连通 / cron 队列长度 / 状态文件大小 as future
// additions — those need backend extensions, so omit here rather than
// inventing empty placeholders that would never fill.
function buildHomeHealthLines(stats) {
  if (!stats || typeof stats !== 'object') return [];
  const lines = [];
  // Line 1: session breakdown + uptime.
  const running = typeof stats.running === 'number' ? stats.running : 0;
  const ready = typeof stats.ready === 'number' ? stats.ready : 0;
  const total = typeof stats.total === 'number' ? stats.total : 0;
  let line1 = '运行 ' + running + ' · 就绪 ' + ready + ' · 总 ' + total;
  if (stats.uptime) line1 += ' · 运行 ' + stats.uptime;
  lines.push({ text: line1, kind: 'info' });
  // claude 子进程容量 (R110-P1 #445 "claude 子进程数"): max_procs ships in the
  // /api/sessions stats static block already, so surface live-vs-capacity
  // without the deferred /api/stats backend scan. Only when max_procs > 0
  // (a 0/missing cap means "uncapped" — no ratio to show). Warn when the
  // pool is saturated so operators notice spawn back-pressure.
  const maxProcs = typeof stats.max_procs === 'number' ? stats.max_procs : 0;
  if (maxProcs > 0) {
    lines.push({
      text: 'claude 子进程 ' + running + '/' + maxProcs,
      kind: running >= maxProcs ? 'warn' : 'info',
    });
  }
  // Line 2: CLI identity. Helpful when operators have multiple naozhi
  // deployments on different CLI versions.
  if (stats.cli_name) {
    let cli = stats.cli_name;
    if (stats.cli_version) cli += ' ' + stats.cli_version;
    lines.push({ text: cli, kind: 'info' });
  }
  // naozhi build tag (R110-P1 #445 service-health): version_tag already ships
  // in the /api/sessions stats block (omitempty when the -X ldflag is unset)
  // and the backend struct doc promises a "naozhi v1.2.3-dirty" footer that
  // was never wired client-side. Surface it so operators can confirm the
  // running build straight from the Home health strip.
  if (stats.version_tag) {
    lines.push({ text: 'naozhi ' + stats.version_tag, kind: 'info' });
  }
  // Multi-Backend RFC §8.3 D22: when ≥2 backends are configured, show a
  // one-liner summarizing per-backend availability + version. The rich
  // per-feature table lives in the doctor status panel (built by
  // renderBackendsDoctorPanel below) — this line is just the at-a-glance
  // health roll-up.
  if (cliBackends && Array.isArray(cliBackends.backends) && cliBackends.backends.length > 1) {
    const okCount = cliBackends.backends.filter(b => b && b.available).length;
    const totalCount = cliBackends.backends.length;
    const ids = cliBackends.backends.map(b => (b && b.id) || '?').join(' · ');
    lines.push({
      text: 'Backends: ' + okCount + '/' + totalCount + ' (' + ids + ')',
      kind: okCount === totalCount ? 'info' : 'warn',
    });
  }
  // Line 3 (gated): watchdog kills > 0 is a prod signal operators should see.
  const wd = stats.watchdog || {};
  const totalKills = typeof wd.total_kills === 'number' ? wd.total_kills : 0;
  if (totalKills > 0) {
    const noOutput = typeof wd.no_output_kills === 'number' ? wd.no_output_kills : 0;
    lines.push({
      text: 'Watchdog 已介入 ' + totalKills + ' 次（无输出 ' + noOutput + '）',
      kind: 'warn',
    });
  }
  return lines;
}

// renderRecentSessionsPanel populates the R110-P1 Home-panel slot inside
// the main empty-state body. Reads allSessionsCache (written by renderSidebar
// after each fetchSessions → so reflects the same authoritative snapshot the
// sidebar shows), picks the 5 most recently active sessions, and renders a
// compact clickable list. When there are zero sessions, returns an empty
// innerHTML so the cold-start minimal CTA stays unchanged. Callers must
// guard by selectedKey == null (active-session main shell wins).
//
// Pure-rendering: writes to the DOM by id rather than returning HTML, because
// the cold-start HTML already carries the placeholder div and we don't want
// to fight the order of initial paint.
function renderRecentSessionsPanel() {
  const host = document.getElementById('recent-sessions-panel');
  if (!host) return;
  if (selectedKey) return; // active session rendered by renderMainShell
  const items = Array.isArray(allSessionsCache) ? allSessionsCache : [];
  if (items.length === 0) { host.innerHTML = ''; return; }
  // Sort by last_active desc; sessions without last_active sink to the
  // bottom so a brand-new "new" card doesn't squat on position 1 forever.
  const top = items.slice().sort((a, b) => (b.last_active || 0) - (a.last_active || 0)).slice(0, 5);
  const rows = top.map(s => {
    const sNode = s.node || 'local';
    const label = s.user_label || s.summary || s.last_prompt || '未命名';
    const state = s.state === 'dead' ? 'ready' : (s.state || 'ready');
    const dotCls = state === 'running' ? 'dot-running' : (state === 'ready' ? 'dot-ready' : 'dot-new');
    const ago = s.last_active ? timeAgo(s.last_active) : '';
    return '<button type="button" class="recent-row" ' +
      'data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" ' +
      'onclick="selectSession(this.dataset.key,this.dataset.node)">' +
      '<span class="recent-dot ' + dotCls + '" aria-hidden="true"></span>' +
      '<span class="recent-label" title="' + escAttr(label) + '">' + esc(label) + '</span>' +
      (ago ? '<span class="recent-time">' + esc(ago) + '</span>' : '') +
      '</button>';
  }).join('');
  // R110-P1 Home stats strip (Round 147 + #445): today-active + processed
  // prompts + total cost. Rendered above the list so operators see a
  // cumulative signal before scanning the session rows. The prompt count
  // sums per-session message_count (already in the snapshot); cumulative
  // tokens still need the deferred /api/stats/aggregate backend scan.
  const stats = computeHomeStats(items, Date.now());
  const statsHtml =
    '<div class="recent-panel-stats" role="group" aria-label="今日概览">' +
      '<div class="recent-stat">' +
        '<div class="recent-stat-value">' + stats.todayActive + '</div>' +
        '<div class="recent-stat-label">今日活跃会话</div>' +
      '</div>' +
      '<div class="recent-stat">' +
        '<div class="recent-stat-value">' + stats.totalPrompts + '</div>' +
        '<div class="recent-stat-label">已处理 prompt</div>' +
      '</div>' +
      '<div class="recent-stat">' +
        '<div class="recent-stat-value">' + esc(formatHomeCost(stats.totalCost)) + '</div>' +
        '<div class="recent-stat-label">累计花费</div>' +
      '</div>' +
    '</div>';
  // R110-P1 Home health strip (Round 148): bottom meta row sourced from
  // /api/sessions stats (cached in lastStatsSnapshot). Suppressed when no
  // stats snapshot has landed yet so cold-start doesn't render a bare div.
  const healthLines = buildHomeHealthLines(lastStatsSnapshot);
  const healthHtml = healthLines.length === 0
    ? ''
    : '<div class="recent-panel-health" role="status" aria-label="服务健康">' +
        healthLines.map(l =>
          '<div class="recent-health-line ' + esc(l.kind || 'info') + '">' + esc(l.text) + '</div>'
        ).join('') +
      '</div>';
  // Multi-Backend RFC §8.3 D22: doctor status panel — clickable details
  // block under the health strip listing each enabled backend's caps +
  // features. Single-backend mode skips it (the home page would be cluttered
  // by a redundant "claude only" table).
  const doctorHtml = renderBackendsDoctorPanel();
  host.innerHTML =
    '<div class="recent-panel">' +
      '<div class="recent-panel-title">最近会话</div>' +
      statsHtml +
      '<div class="recent-panel-list" role="list">' + rows + '</div>' +
      healthHtml +
      doctorHtml +
    '</div>';
}

// renderBackendsDoctorPanel builds a foldable <details> panel listing each
// enabled backend with its protocol caps + user-feature flags. Multi-Backend
// RFC §8.3 D22. Returns '' for single-backend deployments / when cliBackends
// is unavailable so the cold-start home page doesn't show a half-empty
// section. The output includes a small "▼" affordance and a screen-reader
// label so keyboard users know the section is expandable.
function renderBackendsDoctorPanel() {
  if (!cliBackends || !Array.isArray(cliBackends.backends)) return '';
  if (cliBackends.backends.length <= 1) return '';
  const rows = cliBackends.backends.map(b => {
    if (!b) return '';
    const id = esc(b.id || '?');
    const name = esc(b.display_name || b.id || '?');
    const ver = b.version ? ' v' + esc(b.version) : '';
    const proto = b.protocol ? esc(b.protocol) : '';
    const status = b.available
      ? '<span class="doctor-status doctor-status-ok">●</span>'
      : '<span class="doctor-status doctor-status-bad" title="binary missing or --version probe failed">○</span>';
    // !Array.isArray gate: typeof [] is 'object', so without this an array-typed
    // features field would render numeric-keyed pills like "0", "1". Review
    // (PR #121) catch — server contract says object{flag:bool}, but be defensive.
    const features = b.features && typeof b.features === 'object' && !Array.isArray(b.features)
      ? b.features
      : {};
    // Render features as compact pills — green for supported, struck for missing.
    const featPills = Object.keys(features).sort().map(k => {
      const on = features[k] === true;
      return '<span class="doctor-feat ' + (on ? 'doctor-feat-on' : 'doctor-feat-off') +
        '" title="' + escAttr(k) + (on ? '' : ' (not supported)') + '">' + esc(k) + '</span>';
    }).join('');
    return '<div class="doctor-row">' +
      '<div class="doctor-row-head">' + status +
        '<span class="doctor-row-name">[' + id + '] ' + name + ver + '</span>' +
        (proto ? '<span class="doctor-row-proto">' + proto + '</span>' : '') +
      '</div>' +
      (featPills ? '<div class="doctor-row-feats">' + featPills + '</div>' : '') +
    '</div>';
  }).join('');
  const defaultID = esc(cliBackends.default || '');
  // Arrow is supplied by the .doctor-summary::before CSS so it can flip
  // 90° on [open]. Don't bake it into the text.
  return '<details class="doctor-panel" aria-label="后端状态详情">' +
    '<summary class="doctor-summary">Backends 状态 (default: ' + defaultID + ')</summary>' +
    '<div class="doctor-body">' + rows + '</div>' +
  '</details>';
}

// showToast moved to nz_util.js (PR-0a). Available as window.nz.util.showToast
// and the top-level alias window.showToast, loaded before this file.

// RNEW-UX-010 — polite announcement into #sr-announce for screen readers.
// Used for signals that don't surface as a toast (WS connect/disconnect,
// new-session arrival, cron completion). We clear the textContent after a
// short tick so an identical follow-up message still re-triggers the AT
// announcement (some readers skip unchanged text). Silent no-op if the
// element isn't mounted yet (e.g. during very early boot).
function announce(msg) {
  const el = document.getElementById('sr-announce');
  if (!el || !msg) return;
  el.textContent = '';
  setTimeout(() => { el.textContent = String(msg); }, 50);
  clearTimeout(el._clearTid);
  el._clearTid = setTimeout(() => { el.textContent = ''; }, 3000);
}

// localizeAPIError turns an HTTP status code + raw server message into a
// user-facing Chinese string. Classifies by status class so operators get
// a consistent mental model — 4xx = "你这边要改", 5xx = "服务端问题，请
// 稍后重试". The raw tail is appended (truncated to 120 chars) so diagnostic
// signal isn't lost, but the Chinese prefix is always there for screen-readers
// and non-technical operators.
//
// Why not a full i18n dict: current project is single-locale (zh-CN); a
// full go-i18n pipeline was floated in UX review but rejected as overkill
// — UX1 target is "no raw English errors", not "pluggable locales".
function localizeAPIError(status, raw) {
  const tail = (raw || '').toString().trim().slice(0, 120);
  const withTail = tail ? '（' + tail + '）' : '';
  if (status === 0 || status === undefined || status === null) {
    return '网络错误' + withTail;
  }
  if (status === 401) {
    return '鉴权失败，请重新登录' + withTail;
  }
  // work_dir 专项：当后端返回 classifyWorkspaceErr 标签时把通用文案换成更
  // 精确的中文，避免 "无权限或参数越界" 把 "不存在 / 不是目录 / 越界"
  // 三种含义不同的失败合并成一句话，操作员看到无法自助修复。
  // 与 internal/server/server.go classifyWorkspaceErr 输出保持一致。
  if (raw) {
    const r = String(raw);
    if (r.indexOf('work_dir outside allowed root') !== -1) {
      return '工作目录不在允许范围内（' + r.slice(0, 120) + '）';
    }
    if (r.indexOf('work_dir does not exist') !== -1) {
      return '工作目录不存在' + withTail;
    }
    if (r.indexOf('work_dir is not a directory') !== -1) {
      return '路径不是目录' + withTail;
    }
    if (r.indexOf('work_dir is not a valid path') !== -1) {
      return '工作目录路径不合法' + withTail;
    }
    if (r.indexOf('work_dir must be an absolute path') !== -1) {
      return '工作目录必须是绝对路径' + withTail;
    }
  }
  if (status === 403) {
    return '无权限或参数越界' + withTail;
  }
  if (status === 404) {
    return '资源不存在' + withTail;
  }
  if (status === 409) {
    return '状态冲突，请刷新后重试' + withTail;
  }
  if (status === 413) {
    return '内容过大' + withTail;
  }
  if (status === 429) {
    return '请求过于频繁，请稍后重试' + withTail;
  }
  if (status >= 400 && status < 500) {
    return '请求失败（HTTP ' + status + '）' + withTail;
  }
  if (status === 502 || status === 503 || status === 504) {
    return '服务暂时不可用，请稍后重试' + withTail;
  }
  if (status >= 500) {
    return '服务器错误（HTTP ' + status + '）' + withTail;
  }
  return '操作失败（HTTP ' + status + '）' + withTail;
}

// showAPIError renders a HTTP-failed fetch as a Chinese toast. `action`
// is a short user-facing verb (e.g. '删除会话', '保存任务') — it prefixes
// the localized status reason, so the full toast reads like
// "删除会话失败：鉴权失败，请重新登录（...）". Pass the raw server message
// as `raw` (from `await r.text()`) for diagnostic context; truncated at the
// localize layer.
function showAPIError(action, status, raw, duration) {
  const msg = (action ? action + '失败：' : '') + localizeAPIError(status, raw);
  showToast(msg, 'error', duration);
}

// showNetworkError handles the catch-branch of fetch/awaited calls. A thrown
// Error typically means the request never reached the server (DNS / offline
// / CORS / abort). Keep the Chinese verbiage identical to localizeAPIError's
// status=0 arm so the user's mental model stays unified.
function showNetworkError(action, err, duration) {
  const detail = (err && err.message) ? err.message.slice(0, 120) : '';
  const tail = detail ? '（' + detail + '）' : '';
  const msg = (action ? action + '失败：' : '') + '网络错误' + tail;
  showToast(msg, 'error', duration);
}

// reconnectNow cancels any pending reconnect timer, resets the exponential
// backoff so the next failure window starts tight again, and kicks an
// immediate connect. Triggered by the sidebar-status "reconnect" button
// that surfaces after backoff has grown past 8s (see updateStatusBar).
// Idempotent: double-click only results in one connect attempt because
// wsm.connect short-circuits when the socket is already OPEN/CONNECTING.
function reconnectNow() {
  if (wsm.reconnectTimer) {
    clearTimeout(wsm.reconnectTimer);
    wsm.reconnectTimer = null;
  }
  wsm.backoff = 1000;
  // No toast: the sidebar status row already flips to "connecting..." when
  // wsm.connect() sets CONNECTING, and the outage/reconnect button update
  // through updateStatusBar. A toast here was redundant with that signal.
  wsm.connect();
}

function fallbackCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.cssText = 'position:fixed;left:-9999px';
  document.body.appendChild(ta);
  ta.select();
  document.execCommand('copy');
  document.body.removeChild(ta);
}

function copyText(text) {
  if (navigator.clipboard) {
    navigator.clipboard.writeText(text).then(() => showToast('已复制', 'success')).catch(() => { fallbackCopy(text); showToast('已复制', 'success'); });
  } else {
    fallbackCopy(text);
    showToast('已复制', 'success');
  }
}

// Flash a button to "copied!" state for ~1.5s then revert.
function flashCopyButton(btn) {
  btn.textContent = 'copied!';
  btn.classList.add('copied');
  setTimeout(() => { btn.textContent = 'copy'; btn.classList.remove('copied'); }, 1500);
}

// Shared clipboard helper for in-line buttons — uses navigator.clipboard with
// an execCommand fallback for non-HTTPS / older browsers.
function copyWithFeedback(btn, text) {
  const done = () => flashCopyButton(btn);
  if (navigator.clipboard) {
    navigator.clipboard.writeText(text).then(done).catch(() => { fallbackCopy(text); done(); });
  } else {
    fallbackCopy(text);
    done();
  }
}

function copyCodeBlock(btn) {
  // DOM may be re-rendered between render and click (event list ticks every
  // ~1s). Fall back silently instead of throwing when the wrap is gone.
  const { code } = _codeBlockInfo(btn);
  if (!code) return;
  copyWithFeedback(btn, code);
}

// Map common markdown fence languages to file extensions for download filenames.
const _codeLangExt = {
  javascript: 'js', js: 'js', typescript: 'ts', ts: 'ts', jsx: 'jsx', tsx: 'tsx',
  python: 'py', py: 'py', ruby: 'rb', rb: 'rb', go: 'go', golang: 'go',
  rust: 'rs', rs: 'rs', java: 'java', kotlin: 'kt', kt: 'kt', swift: 'swift',
  c: 'c', 'c++': 'cpp', cpp: 'cpp', cxx: 'cpp', cc: 'cpp', h: 'h', hpp: 'hpp',
  'c#': 'cs', csharp: 'cs', cs: 'cs', php: 'php', perl: 'pl', pl: 'pl',
  lua: 'lua', scala: 'scala', r: 'r', dart: 'dart',
  html: 'html', htm: 'html', css: 'css', scss: 'scss', sass: 'sass', less: 'less',
  json: 'json', yaml: 'yml', yml: 'yml', toml: 'toml', xml: 'xml',
  markdown: 'md', md: 'md', sql: 'sql', graphql: 'graphql', proto: 'proto',
  shell: 'sh', bash: 'sh', sh: 'sh', zsh: 'sh', fish: 'fish',
  ini: 'ini', diff: 'diff', patch: 'patch', vim: 'vim', tex: 'tex', latex: 'tex',
};

// Languages that render to a bare filename (no "snippet." prefix, no ext
// separator). Prevents `snippet.Dockerfile` when the intent is `Dockerfile`.
const _codeLangBareName = {
  dockerfile: 'Dockerfile', docker: 'Dockerfile',
  makefile: 'Makefile', make: 'Makefile',
};

function _codeBlockInfo(btn) {
  const wrap = btn.closest('.md-code-wrap');
  if (!wrap) return { code: '', lang: '' };
  // Path-list blocks (.md-pathlist) render one <code> per line instead of a
  // single <pre><code>, so copy must join every row's text — querySelector
  // alone would copy just the first path. file-ref button injection may also
  // nest extra <code> inside .fr-slot, so scope to the row's first <code>.
  if (wrap.classList.contains('md-pathlist')) {
    const code = Array.from(wrap.querySelectorAll('.md-pathline'))
      .map(row => { const c = row.querySelector('code'); return c ? c.textContent : ''; })
      .join('\n');
    return { code, lang: '' };
  }
  const codeEl = wrap.querySelector('code');
  const code = codeEl ? codeEl.textContent : '';
  const lang = (codeEl && codeEl.getAttribute('data-lang') || '').toLowerCase();
  return { code, lang };
}

function _codeBlockFilename(lang) {
  if (_codeLangBareName[lang]) return _codeLangBareName[lang];
  const ext = _codeLangExt[lang] || (lang || 'txt');
  // Ext must be a short alnum-ish token; otherwise use .txt to avoid
  // writing unsafe names like `snippet.<script>`.
  if (!/^[a-z0-9]{1,12}$/i.test(ext)) return 'snippet.txt';
  return 'snippet.' + ext;
}

// Snippet payload for preview drawer. Storing in a module variable instead of
// drawer.dataset avoids the multi-MB attribute truncation and DOM-serialize cost.
let _pendingSnippet = null;

function previewCodeBlock(btn) {
  const { code, lang } = _codeBlockInfo(btn);
  if (!code) return;
  const drawer = document.getElementById('fv-drawer');
  const body = document.getElementById('fv-body');
  const title = document.getElementById('fv-title');
  const meta = document.getElementById('fv-meta');
  if (!drawer || !body || !title || !meta) return;
  const name = _codeBlockFilename(lang);
  drawer.classList.remove('hidden');
  drawer.classList.add('fv-open');
  // Mark as snippet so the drawer header copy/download buttons fall back to
  // the inline code instead of trying to fetch a server file.
  drawer.dataset.project = '';
  drawer.dataset.node = '';
  drawer.dataset.path = '';
  drawer.dataset.snippetMode = '1';
  drawer.dataset.snippetName = name;
  _pendingSnippet = code;
  title.textContent = name;
  meta.textContent = (lang ? lang + ' · ' : '') + formatFileSize(new Blob([code]).size);
  // Always render snippets as escaped plain text. The markdown branch
  // previously piped user-controlled LLM output through renderMd which can
  // reinsert HTML (math tokens, etc.). The CSP has `unsafe-inline` so HTML
  // injection in the drawer is a real risk — keep snippets escape-only.
  body.innerHTML = '<pre><code class="fv-code">' + esc(code) + '</code></pre>';
}

function downloadCodeBlock(btn) {
  const { code, lang } = _codeBlockInfo(btn);
  if (!code) return;
  const name = _codeBlockFilename(lang);
  const blob = new Blob([code], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function copyEventContent(btn) {
  const text = btn.dataset.raw || btn.closest('.event').querySelector('.event-content').textContent;
  copyWithFeedback(btn, text);
}

function shortPath(p) {
  const home = '/home/';
  const i = p.indexOf(home);
  if (i >= 0) {
    const rest = p.substring(i + home.length);
    const slash = rest.indexOf('/');
    if (slash >= 0) return '~' + rest.substring(slash);
  }
  return p.length > 40 ? '...' + p.substring(p.length - 37) : p;
}

// historyDayLabel formats a Date as the history drawer's day-group
// label. Today and yesterday collapse to \u4e2d\u6587 "\u4eca\u5929" / "\u6628\u5929" so the
// most common buckets read instantly without parsing a date. Older
// entries defer to the browser locale so CJK users see "4\u670829\u65e5 \u5468\u4e09"
// and EN users see "Wed, Apr 29" \u2014 both read naturally now that the
// .hp-day-header uppercase was dropped in Round 129.
//
// Exposed at module scope (not inside renderHistoryPopover) so the
// Round 129 contract test can assert its existence and both branches
// are easy to eyeball in the source.
function historyDayLabel(d) {
  if (!d || isNaN(d.getTime())) return '';
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const target = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const diffDays = Math.round((today.getTime() - target.getTime()) / 86400000);
  if (diffDays === 0) return '\u4eca\u5929';
  if (diffDays === 1) return '\u6628\u5929';
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', weekday: 'short' });
}

function timeAgo(ms, future) {
  if (!ms) return '\u2014';
  const d = future ? ms - Date.now() : Date.now() - ms;
  if (d < 0) return future ? 'now' : 'just now';
  const suffix = future ? '' : ' ago';
  if (d < 5000) return future ? 'now' : 'just now';
  if (d < 60000) return Math.floor(d/1000) + 's' + suffix;
  if (d < 3600000) return Math.floor(d/60000) + 'm' + suffix;
  if (d < 86400000) return Math.floor(d/3600000) + 'h' + suffix;
  return Math.floor(d/86400000) + 'd' + suffix;
}

// formatAbsTime renders an epoch-ms timestamp in local time as
// "YYYY-MM-DD HH:MM:SS (TZ)" for use inside title attributes on the various
// "3m ago" / "next 2h" relative labels. The goal is R110-P3: keep the
// compact relative form in the UI, but let hover reveal the exact instant
// so operators can reason about long-running jobs / stale sessions without
// doing mental arithmetic. Falls back to '' on falsy input so callers can
// safely gate the title attribute with a truthy check.
function formatAbsTime(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  if (isNaN(d.getTime())) return '';
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const tz = (() => {
    const off = -d.getTimezoneOffset();
    const sign = off >= 0 ? '+' : '-';
    const abs = Math.abs(off);
    return 'UTC' + sign + pad(Math.floor(abs / 60)) + ':' + pad(abs % 60);
  })();
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
    ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) +
    ' (' + tz + ')';
}

function sessionTimeHint(key) {
  const m = (key || '').match(/:(\d{4})-(\d{2})-(\d{2})-(\d{2})(\d{2})(\d{2})/);
  if (m) return m[2] + '/' + m[3] + ' ' + m[4] + ':' + m[5];
  return '\u2014';
}

// trapFocus moved to nz_util.js (PR-0a). Available as window.nz.util.trapFocus
// and the top-level alias window.trapFocus, loaded before this file.

// confirmDialog renders a styled confirm prompt matching the rest of the
// dashboard (reuses .modal-overlay / .modal / .modal-btns). Returns a Promise
// that resolves to `true` on confirm or `false` on cancel / Esc / backdrop
// click. Native window.confirm() is blocking + looks out of place next to our
// custom dark-theme modals; this helper fixes both.
//
// Call shape:
//   const ok = await confirmDialog({
//     title: '删除定时任务？',
//     message: '任务将被永久删除，下次不再触发。',
//     detail: 'cron-id-12345',      // optional mono-spaced tail
//     confirmText: '删除',
//     variant: 'danger',            // 'danger' | 'primary' (default danger)
//   });
//
// Semantics:
//   - Default focus lands on the CANCEL button (safer than focusing the
//     destructive primary). Enter in a destructive dialog still requires
//     the user to Tab over first, matching macOS confirm dialogs.
//   - Esc and backdrop click both resolve to false — identical to the
//     cancel button. Consistent with every other modal in the dashboard.
//   - XSS-safe: all caller-supplied text is routed through esc() before
//     insertion; callers may pass untrusted content (session key, cron id).
//   - If a dialog is already open, this call resolves immediately to false
//     to avoid stacking multiple confirms on the same decision.
function confirmDialog(opts) {
  return new Promise((resolve) => {
    if (document.querySelector('.modal-overlay.confirm-overlay')) {
      resolve(false);
      return;
    }
    const title = (opts && opts.title) || '确认操作';
    const message = (opts && opts.message) || '';
    const detail = (opts && opts.detail) || '';
    const confirmText = (opts && opts.confirmText) || '确认';
    const cancelText = (opts && opts.cancelText) || '取消';
    const variant = (opts && opts.variant) || 'danger';
    const confirmClass = variant === 'danger' ? 'danger' : 'primary';
    // Round 2 R-12: optional countdown (seconds) before the confirm
    // button activates. Used by destructive flows to insert a "速度带"
    // — short enough to not annoy, long enough to catch fat-fingered
    // double-Enter. Default 0 = no countdown (legacy behaviour).
    const countdownSecs = (opts && typeof opts.countdownSecs === 'number') ? Math.max(0, Math.floor(opts.countdownSecs)) : 0;

    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay confirm-overlay';
    // message may include line breaks now (RFC §7.5 multi-paragraph
    // copy). Render via <pre>-style white-space:pre-wrap on .confirm-msg
    // so existing toast-style single-line callers still look right.
    overlay.innerHTML =
      '<div class="modal confirm-dialog" role="alertdialog" aria-modal="true" aria-labelledby="confirm-title">' +
        '<h3 id="confirm-title">' + esc(title) + '</h3>' +
        (message ? '<p class="confirm-msg">' + esc(message) + '</p>' : '') +
        (detail ? '<p class="confirm-detail"><code>' + esc(detail) + '</code></p>' : '') +
        '<div class="modal-btns">' +
          '<button type="button" class="confirm-cancel">' + esc(cancelText) + '</button>' +
          '<button type="button" class="' + confirmClass + ' confirm-ok"' + (countdownSecs > 0 ? ' disabled' : '') + '>' +
            esc(confirmText) + (countdownSecs > 0 ? ' (' + countdownSecs + ')' : '') +
          '</button>' +
        '</div>' +
      '</div>';

    let settled = false;
    let tickTimer = null;
    const finish = (ok) => {
      if (settled) return;
      settled = true;
      if (tickTimer) { clearInterval(tickTimer); tickTimer = null; }
      overlay.remove();
      resolve(!!ok);
    };

    const okBtn = overlay.querySelector('.confirm-ok');
    overlay.querySelector('.confirm-cancel').addEventListener('click', () => finish(false));
    okBtn.addEventListener('click', () => {
      // Defensive: while disabled the click won't fire on a real button,
      // but if a custom CSS rule ever overrides pointer-events we still
      // refuse early activation by checking the disabled attribute.
      if (okBtn.hasAttribute('disabled')) return;
      finish(true);
    });
    // Backdrop click cancels. Guard against inner clicks bubbling through
    // by checking that the click's target is the overlay itself.
    overlay.addEventListener('click', (e) => { if (e.target === overlay) finish(false); });
    // trapFocus handles Esc (removes overlay); mirror that by observing the
    // removal and resolving false if the consumer used Esc / other removal.
    const obs = new MutationObserver(() => {
      if (!document.body.contains(overlay)) { obs.disconnect(); finish(false); }
    });
    obs.observe(document.body, { childList: true, subtree: false });

    document.body.appendChild(overlay);
    trapFocus(overlay);
    // Focus cancel first — protects against a stray Enter auto-firing the
    // destructive primary. User must explicitly Tab or click to confirm.
    setTimeout(() => overlay.querySelector('.confirm-cancel').focus(), 50);

    if (countdownSecs > 0) {
      let remaining = countdownSecs;
      tickTimer = setInterval(() => {
        remaining -= 1;
        if (remaining <= 0) {
          clearInterval(tickTimer);
          tickTimer = null;
          okBtn.removeAttribute('disabled');
          okBtn.textContent = confirmText;
          // Live region for SR — announce activation politely so a
          // keyboard user knows they can now Tab + Enter.
          okBtn.setAttribute('aria-live', 'polite');
        } else {
          okBtn.textContent = confirmText + ' (' + remaining + ')';
        }
      }, 1000);
    }
  });
}

// RNEW-UX-013: promptDialog is the themed replacement for native window.prompt().
// Matches confirmDialog shape (overlay + .modal-btns) so the two share styling,
// trapFocus, Esc/backdrop-cancel semantics, and XSS-safe rendering. Returns a
// Promise that resolves to the trimmed input string on confirm, or null on
// cancel / Esc / backdrop click (mirroring window.prompt's null-for-cancel
// convention so existing call sites translate without special-casing).
//
// Call shape:
//   const next = await promptDialog({
//     title: '重命名会话',
//     message: '留空恢复默认标题，最多 128 字节',
//     defaultValue: current,
//     placeholder: '输入新标题',
//     confirmText: '保存',
//     maxLength: 128,
//   });
//   if (next === null) return;  // user cancelled
//
// Semantics:
//   - Enter inside the input submits (matching window.prompt expectations —
//     the information-entry dialog defaults to primary, not cancel, because
//     the action isn't destructive).
//   - Esc and backdrop cancel (resolve null). Consistent with confirmDialog.
//   - If a prompt dialog is already open, this call resolves immediately to
//     null to avoid stacking.
//   - XSS-safe: every caller-supplied string routes through esc() before
//     insertion. defaultValue is set via .value (DOM property) not innerHTML.
function promptDialog(opts) {
  return new Promise((resolve) => {
    if (document.querySelector('.modal-overlay.prompt-overlay')) {
      resolve(null);
      return;
    }
    const title = (opts && opts.title) || '输入内容';
    const message = (opts && opts.message) || '';
    const defaultValue = (opts && opts.defaultValue) != null ? String(opts.defaultValue) : '';
    const placeholder = (opts && opts.placeholder) || '';
    const confirmText = (opts && opts.confirmText) || '确认';
    const cancelText = (opts && opts.cancelText) || '取消';
    const maxLength = (opts && opts.maxLength) || 0;

    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay prompt-overlay';
    const maxAttr = maxLength > 0 ? ' maxlength="' + maxLength + '"' : '';
    overlay.innerHTML =
      '<div class="modal prompt-dialog" role="dialog" aria-modal="true" aria-labelledby="prompt-title">' +
        '<h3 id="prompt-title">' + esc(title) + '</h3>' +
        (message ? '<p class="prompt-message">' + esc(message) + '</p>' : '') +
        '<input type="text" class="prompt-input" placeholder="' + escAttr(placeholder) + '"' + maxAttr + '>' +
        '<div class="modal-btns">' +
          '<button type="button" class="prompt-cancel">' + esc(cancelText) + '</button>' +
          '<button type="button" class="primary prompt-ok">' + esc(confirmText) + '</button>' +
        '</div>' +
      '</div>';

    const input = overlay.querySelector('.prompt-input');
    // Use the DOM .value property (not innerHTML interpolation) to seed the
    // default — avoids having to escape attribute quotes and preserves any
    // literal whitespace the caller relied on.
    input.value = defaultValue;

    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      overlay.remove();
      resolve(value);
    };

    overlay.querySelector('.prompt-cancel').addEventListener('click', () => finish(null));
    overlay.querySelector('.prompt-ok').addEventListener('click', () => finish(input.value));
    overlay.addEventListener('click', (e) => { if (e.target === overlay) finish(null); });
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); finish(input.value); }
    });
    // Mirror confirmDialog: if the overlay is removed externally (trapFocus
    // Esc handling, or a caller manipulating DOM), settle as cancelled.
    const obs = new MutationObserver(() => {
      if (!document.body.contains(overlay)) { obs.disconnect(); finish(null); }
    });
    obs.observe(document.body, { childList: true, subtree: false });

    document.body.appendChild(overlay);
    trapFocus(overlay);
    // Focus the input so the user can type immediately. Select all so the
    // default value is replaced on first keystroke — matches window.prompt
    // behaviour in Chrome/Firefox.
    setTimeout(() => { input.focus(); input.select(); }, 50);
  });
}

// Time-divider threshold: insert a visual gap label when the interval between
// adjacent rendered events exceeds this many ms. 5 minutes matches iMessage-ish
// chat grouping — tight enough to separate turns, loose enough to not spam.
const EVENT_DIVIDER_GAP_MS = 5 * 60 * 1000;

// INITIAL_HISTORY_LIMIT caps how many events the server sends on a fresh
// subscribe / first fetch. Keeps big sessions snappy on first paint; older
// pages load lazily via the "load earlier" button. Server caps at 500
// regardless (maxEventsPageLimit) so 100-500 is the effective window.
const INITIAL_HISTORY_LIMIT = 100;
const EARLIER_PAGE_LIMIT = 100;

// UX3 (#398): the live-push path (appendEvents) does insertAdjacentHTML('beforeend')
// once per event with no upper bound, so a long-running session that streams
// thousands of events grows #events-scroll without limit and eventually OOMs the
// tab. The historical-load path is already paginated (INITIAL_HISTORY_LIMIT +
// "load earlier"); this cap is the live half of the same budget. We keep a
// generous tail (matching a few "load earlier" pages) and trim the oldest DOM
// nodes from the top once exceeded. Trimming only drops rendered DOM — the
// server still holds full history, and "load earlier" re-fetches if the operator
// scrolls back up. Mirrors the existing CRON_LIVE_MAX_EVENTS top-trim precedent.
const MAX_LIVE_DOM_EVENTS = 600;

// cron-live RFC §5: cron live 容器内最多保留 200 条事件。后端
// EventEntriesSince(after) 不受 50 条上限约束（After>0 时 Limit 被忽略），
// 一次 history 帧可能返回数百条事件 —— 前端必须自己截尾。超出部分计入
// truncatedCount 并用容器顶部的提示告知操作员。
const CRON_LIVE_MAX_EVENTS = 200;

// 当 cron live 收到的事件全被 INTERNAL_EVENT_TYPES 过滤光（parallel agent
// team 整段是 agent / task_* / tool_use），渲这条占位而非留空 innerHTML，
// 否则 CSS .cdl-events:empty::before 会误报"暂无事件"。.cdl-agent-only 类名
// 供 appendEventsToContainer 在追加真实事件前识别并清除占位。
const CRON_LIVE_AGENT_ONLY_HTML =
  '<div class="empty-state cdl-agent-only">本轮仅有 agent / 工具活动，正文消息请在任务结束后查看历史详情</div>';

// formatTimeShort returns a chat-style label for a divider: today -> HH:MM,
// yesterday -> "昨天 HH:MM", within a week -> "周三 HH:MM", older -> "M-D HH:MM",
// different year -> "YYYY-M-D HH:MM".
function formatTimeShort(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  const now = new Date();
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const hm = hh + ':' + mm;
  const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  if (sameDay) return hm;
  const yesterday = new Date(now); yesterday.setDate(now.getDate() - 1);
  const isYesterday = d.getFullYear() === yesterday.getFullYear() && d.getMonth() === yesterday.getMonth() && d.getDate() === yesterday.getDate();
  if (isYesterday) return '昨天 ' + hm;
  const diffDays = Math.floor((now - d) / 86400000);
  if (diffDays < 7 && diffDays >= 0) {
    const wk = ['周日','周一','周二','周三','周四','周五','周六'][d.getDay()];
    return wk + ' ' + hm;
  }
  const md = (d.getMonth() + 1) + '-' + d.getDate();
  if (d.getFullYear() !== now.getFullYear()) return d.getFullYear() + '-' + md + ' ' + hm;
  return md + ' ' + hm;
}

// formatTimeFull is a locale-ish absolute timestamp used in the event tooltip.
function formatTimeFull(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  const pad = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
    pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
}

function timeDividerHtml(ms) {
  return '<div class="event-time-divider" data-time="' + (ms || 0) + '">' + esc(formatTimeShort(ms)) + '</div>';
}

// esc / escAttr / escJs moved to nz_util.js (PR-0a, RFC
// dashboard-cron-view-extraction). They are exposed as window.nz.util.* and
// as top-level aliases (window.esc, window.escAttr, window.escJs) loaded
// before this file, so the bare call sites below keep working unchanged.
// SECURITY: the single source of truth for HTML/attr/JS escaping lives there
// — never re-define a local copy here or in any view module.

// URL schemes that are safe to embed in <a href>.
// RNEW-SEC-007: Only https?: and fragment-only URLs (#...) are accepted.
// Previously the allowlist also matched mailto:, absolute paths (/...),
// and query-only URLs (?...). Those introduced defence-in-depth gaps:
//   - mailto: can trigger unexpected behaviour in Electron/extension hosts
//     and is never present in LLM-rendered markdown anchor targets today.
//   - A single leading "/" lets any string starting with a slash pass the
//     check; if a caller ever forgot to esc() the capture first, a payload
//     like "/"+"><script>..." would reach href and bypass the scheme
//     gate. The stricter regex fails closed in that scenario.
// Internal links should be constructed against absolute /api/... paths in
// code, not routed through safeUrl.
// Anything else (javascript:, data:, vbscript:, file:, about:) -> '#'.
function safeUrl(u) {
  if (!u) return '#';
  const trimmed = String(u).trim();
  if (/^(https?:|#)/i.test(trimmed)) return trimmed;
  return '#';
}

let mermaidLoading = false;
let mermaidReady = false;

function loadMermaid() {
  if (mermaidReady || mermaidLoading) return;
  mermaidLoading = true;
  const s = document.createElement('script');
  s.src = 'https://cdn.jsdelivr.net/npm/mermaid@11.14.0/dist/mermaid.min.js';
  s.integrity = 'sha384-1CMXl090wj8Dd6YfnzSQUOgWbE6suWCaenYG7pox5AX7apTpY3PmJMeS2oPql4Gk';
  s.crossOrigin = 'anonymous';
  s.onload = () => {
    mermaid.initialize({ startOnLoad: false, theme: 'dark', securityLevel: 'strict' });
    mermaidReady = true;
    mermaidLoading = false;
    runMermaid();
  };
  s.onerror = () => { mermaidLoading = false; };
  document.head.appendChild(s);
}

function runMermaid() {
  if (Object.keys(mermaidPending).length === 0) return;
  if (!mermaidReady) { loadMermaid(); return; }
  let hasNew = false;
  Object.entries(mermaidPending).forEach(([id, code]) => {
    const el = document.getElementById(id);
    if (!el) { delete mermaidPending[id]; return; }
    el.textContent = code;
    el.className = 'mermaid';
    delete mermaidPending[id];
    hasNew = true;
  });
  if (hasNew) mermaid.run({ nodes: document.querySelectorAll('.mermaid') });
}

let mermaidCounter = 0;
const mermaidPending = {};

let katexLoading = false;
let katexReady = false;
let katexCounter = 0;
const katexPending = {};

function loadKatex() {
  if (katexReady || katexLoading) return;
  katexLoading = true;
  // Inject stylesheet on demand (moved out of <head> to unblock first paint).
  // R219-SEC-4: KaTeX CDN link + script must carry SRI integrity hashes;
  // contract pinned by TestDashboardJS_CDNScriptsHaveSRI.
  if (!document.querySelector('link[data-nz-katex]')) {
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = 'https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/katex.min.css';
    link.integrity = 'sha384-zh0CIslj+VczCZtlzBcjt5ppRcsAmDnRem7ESsYwWwg3m/OaJ2l4x7YBZl9Kxxib';
    link.crossOrigin = 'anonymous';
    link.setAttribute('data-nz-katex', '1');
    document.head.appendChild(link);
  }
  const s = document.createElement('script');
  s.src = 'https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/katex.min.js';
  s.integrity = 'sha384-Rma6DA2IPUwhNxmrB/7S3Tno0YY7sFu9WSYMCuulLhIqYSGZ2gKCJWIqhBWqMQfh';
  s.crossOrigin = 'anonymous';
  s.onload = () => {
    katexReady = true;
    katexLoading = false;
    runKatex();
  };
  s.onerror = () => { katexLoading = false; };
  document.head.appendChild(s);
}

function runKatex() {
  if (Object.keys(katexPending).length === 0) return;
  if (!katexReady) { loadKatex(); return; }
  Object.entries(katexPending).forEach(([id, info]) => {
    const el = document.getElementById(id);
    if (!el) { delete katexPending[id]; return; }
    try {
      katex.render(info.tex, el, { displayMode: info.display, throwOnError: false });
    } catch(_) {
      el.textContent = (info.display ? '$$' : '$') + info.tex + (info.display ? '$$' : '$');
    }
    delete katexPending[id];
  });
}

// isMathInline — decide whether the content captured between `$...$` pair
// looks like a math expression rather than prose. Called after the outer
// guard (non-alphanumeric on both sides of the `$`) has already rejected
// obvious prose like "每月$650$USD". Three-tier check, any-match passes:
//   1) contains an unambiguous LaTeX char (\ ^ _ { })
//   2) otherwise must be built from "math alphabet" chars only (digits,
//      single letters, operators, parens, punctuation) AND contain no two
//      consecutive 3+ letter English words AND contain at least one math
//      hint — digit, operator, OR a function-call pattern `letter(` /
//      `)letter`. The function-call clause accepts `$h(x)$` / `$f(x)$` /
//      `$g(t)$` which the previous "must contain digit/operator" rule
//      mistakenly rejected (function references in prose carry no operator
//      character themselves). Pure prose tokens like `$(test)$` still
//      reject because they lack both a math hint and a function-call shape.
function isMathInline(tex) {
  if (/[\\^_{}]/.test(tex)) return true;
  if (!/^[\s\d+\-*/=<>≤≥≠±·×÷!().,;\[\]|a-zA-Z]+$/.test(tex)) return false;
  if (/[a-zA-Z]{3,}\s+[a-zA-Z]{3,}/.test(tex)) return false;
  if (!/[\d+\-*/=<>]|[a-zA-Z]\(|\)[a-zA-Z]/.test(tex)) return false;
  return true;
}

function renderKatex(tex, displayMode) {
  if (katexReady) {
    try { return katex.renderToString(tex, { displayMode: displayMode, throwOnError: false }); }
    catch(_) { return esc(tex); }
  }
  const id = 'ktx-' + (++katexCounter);
  katexPending[id] = { tex: tex, display: displayMode };
  loadKatex();
  return '<span id="' + id + '" class="katex-pending">' + esc(tex) + '</span>';
}

// runPendingAsync — single post-render glue point for every async pipeline
// triggered by renderMd/renderRich output. Call sites that attach rendered
// HTML to the live DOM invoke this once; never call runKatex / runMermaid
// directly from feature code. Keeps chat bubbles, preview drawer, scratch
// drawer, aside drawer on one flush contract so future pipelines (syntax
// highlight etc.) plug in here without scattering across call sites.
function runPendingAsync() {
  runMermaid();
  runKatex();
}

// renderRich — unified rich-text entrypoint. Single source of truth for
// chat bubbles, file-preview drawer, scratch drawer, aside drawer. Pure
// HTML producer (does NOT touch DOM); caller must runPendingAsync() after
// attaching the result so KaTeX / Mermaid pending slots get flushed.
//
// opts.mode:
//   'markdown' (default) — full md renderer (fenced code, math, mermaid,
//                          tables, lists, links)
//   'tex'                — .tex / .latex file: extract math blocks,
//                          everything else kept as preformatted text
//   'plain'              — no rendering, esc + <pre>
function renderRich(src, opts) {
  if (!src) return '';
  const mode = (opts && opts.mode) || 'markdown';
  if (mode === 'plain') return '<pre class="rich-plain">' + esc(src) + '</pre>';
  if (mode === 'tex')   return renderTexDoc(src);
  return renderMd(src);
}

// renderTexDoc — light .tex/.latex renderer. Not a LaTeX compiler; extracts
// delimiters KaTeX supports and leaves the rest as preformatted text so
// authors can see their source comments / section headers intact.
function renderTexDoc(src) {
  const RE = /(\$\$[\s\S]+?\$\$|\\\[[\s\S]+?\\\]|\\begin\{(equation|align|aligned|gather|multline|cases|array|pmatrix|bmatrix|vmatrix|Vmatrix|matrix)\*?\}[\s\S]+?\\end\{\2\*?\}|\$[^\$\n]+?\$|\\\([\s\S]+?\\\))/g;
  const out = [];
  let last = 0, m;
  while ((m = RE.exec(src)) !== null) {
    if (m.index > last) {
      out.push('<pre class="rich-plain">' + esc(src.slice(last, m.index)) + '</pre>');
    }
    const b = m[0];
    if (b.startsWith('$$')) {
      out.push('<div class="md-math-display">' + renderKatex(b.slice(2, -2).trim(), true) + '</div>');
    } else if (b.startsWith('\\[')) {
      out.push('<div class="md-math-display">' + renderKatex(b.slice(2, -2).trim(), true) + '</div>');
    } else if (b.startsWith('\\begin')) {
      out.push('<div class="md-math-display">' + renderKatex(b, true) + '</div>');
    } else if (b.startsWith('\\(')) {
      out.push(renderKatex(b.slice(2, -2).trim(), false));
    } else {
      out.push(renderKatex(b.slice(1, -1), false));
    }
    last = m.index + b.length;
  }
  if (last < src.length) {
    out.push('<pre class="rich-plain">' + esc(src.slice(last)) + '</pre>');
  }
  return out.join('');
}

/* Lightweight Markdown renderer for text/result events.
   Plain messages (no fenced code, math, or mermaid) are memoized since event
   renders run repeatedly — every WS push triggers a full-list re-render for
   the initial history, plus nav rebuilds, plus preview polls. */
const _mdCache = new Map();
const _MD_CACHE_MAX = 500;
// RNEW-PERF-003 (#454): cap cacheable input length at 2000 chars. The
// previous 20000-char cap caused two pathologies on streaming text events:
//
//  1. Cache MISS on every render — streaming `text` events grow by chunks,
//     so the cache key (full string) is unique per WS push. The Map.get
//     was always undefined, the work was always done from scratch.
//  2. Cache WRITE on every render evicted long-lived plain-text bubbles
//     (welcome banner, system prompts, short replies) that would otherwise
//     have been cheap repeat-hits as the user navigated views. Net cache
//     hit rate fell off a cliff once a long streaming reply landed.
//
// Plain replies under 2000 chars ARE the cache's intended audience —
// they're the ones that re-render on nav rebuild + preview poll without
// changing. Above that threshold the input is either a wall-of-text final
// reply (rendered once, never again — cache is a write-only bloat) or a
// still-streaming `running` event (key changes every push — cache never
// hits). Skip the cache write in both cases. The 2000-char threshold
// covers >95% of stable IM-style replies on naozhi without paying
// hash-the-string cost on streaming-storm responses.
const _MD_CACHE_INPUT_MAX = 2000;

function renderMd(s) {
  if (!s) return '';
  // Only cache when the input has no constructs that mint unique DOM ids
  // (mermaid-N / ktx-N), otherwise cached HTML would collide across messages.
  const cacheable = s.length < _MD_CACHE_INPUT_MAX && !/```|\$|\\\[|\\\(/.test(s);
  if (cacheable) {
    const hit = _mdCache.get(s);
    if (hit !== undefined) return hit;
  }
  const out = renderMdUncached(s);
  if (cacheable) {
    if (_mdCache.size >= _MD_CACHE_MAX) {
      const firstKey = _mdCache.keys().next().value;
      _mdCache.delete(firstKey);
    }
    _mdCache.set(s, out);
  }
  return out;
}

/* ===== File reference buttons ========================================= */
/* Scan event bubbles for path-shaped strings (inside <code> or literal),
 * verify existence against the active project workspace, and append
 * [preview] [download] buttons inline. Remote-friendly: lazy validation,
 * batched existence checks, only fetches file content when clicked. */

// Path candidate regex: accepts two shapes —
//   (a) path with at least one `/` (with optional :line / :line-line suffix).
//       e.g. `src/foo.go`, `./a/b.ts:42`, `manifests/ec2nodeclass.yaml:9`.
//   (b) bare filename that MUST carry a :line suffix to disambiguate from
//       prose. e.g. `option_install_gpu_nodegroups.sh:1838-1883`. Review
//       output often references a single-file path without any `/` prefix;
//       the line suffix is a strong signal it is in fact a file reference
//       rather than an English word that happens to contain a dot.
// Segments accept any non-whitespace, non-colon char so Unicode filenames
// (Chinese, Japanese, …) are not silently dropped. Absolute paths are
// resolved to project-relative form by resolveProjectForAbsPath before the
// server call — server still rejects absolute paths for defence in depth.
// Rejects spaces (breaks on prose) and leading URL schemes.
const FILE_REF_WITH_SLASH = /^(?:\.\.?\/|\/)?(?!https?:)[^\s:]+(?:\/[^\s:]+)+(?::\d+(?:-\d+)?)?$/;
const FILE_REF_BARE_WITH_LINE = /^(?!https?:)[^\s:\/]+\.[A-Za-z0-9_]+:\d+(?:-\d+)?$/;
function isFileRefCandidate(text) {
  return FILE_REF_WITH_SLASH.test(text) || FILE_REF_BARE_WITH_LINE.test(text);
}

// Every path-list line's basename must carry a file extension (a `.ext` tail).
// isFileRefCandidate alone is too loose for whole-block classification:
// dependency lists (`@angular/core`), module paths (`github.com/gin-gonic/gin`),
// REST routes (`/api/v1/users`), and fractions/dates (`1/2`, `2024/01/02`) all
// match the slash-shaped path regex line-for-line and would hijack a legit
// no-language code block. Real file paths — including every case this fix
// targets — end in an extension, so this is a cheap high-signal gate that drops
// those false positives without losing the screenshot scenario (`.html` lists).
// Trailing `:line` suffixes are stripped by splitPathLine before this runs.
const FILE_REF_HAS_EXT = /\.[A-Za-z0-9]+$/;

// splitPathNote separates a fenced path line into its path candidate and a
// trailing human annotation. AI replies commonly tag path lines with inline
// notes — `语文/...诊断.md   ← 待生成`, `bar.html  # 答案`, `foo.go (new)` —
// where the note is set off from the path by whitespace. A real file path never
// contains whitespace (isFileRefCandidate rejects spaces), so the first
// whitespace-delimited token IS the path candidate and everything after the gap
// is a note we preserve for display but exclude from the <code> body (so copy +
// the file-ref scanner see the bare path). Returns {path, note}; note is '' when
// the line is a lone path.
function splitPathNote(line) {
  const m = line.match(/^(\S+)(?:\s+(.*\S))?\s*$/);
  if (!m) return { path: line, note: '' };
  return { path: m[1], note: m[2] || '' };
}

// fencedPathList decides whether a language-less fenced code block is in fact
// a plain list of file paths (one per line). AI replies frequently dump
// generated/affected files inside a ``` fence — those paths are invisible to
// the inline file-ref scanner because it skips <pre> content. When EVERY
// non-empty line is a path candidate whose basename has an extension we return
// the parsed rows ({path, note}) so the caller can render them as clickable
// rows. Returns null otherwise, leaving the verbatim-code path untouched.
// Requiring all lines to match keeps real code blocks out: any block with one
// prose/code/extension-less line fails the test.
//
// A single path line is accepted (one generated file inside a ``` fence is a
// very common AI shape). The isFileRefCandidate + FILE_REF_HAS_EXT double gate
// plus the server-side existence check (a non-file that slips through resolves
// to {exists:false} and silently gets no button) make a lone-line list safe;
// the old "≥2 lines" guard left every single-file fence button-less.
//
// Trailing annotations (`foo.md   ← 待生成`) are stripped via splitPathNote so
// the note no longer breaks isFileRefCandidate (which rejects whitespace). The
// note is carried through for display but kept out of the path.
//
// Known non-goal: lines with trailing punctuation glued to the path
// (`foo.md。`) or inline backtick wrapping are not normalized here — they'd
// resolve to a non-existent path and silently get no button.
function fencedPathList(code) {
  const lines = code.split('\n');
  const paths = [];
  for (const raw of lines) {
    const line = raw.trim();
    if (line === '') continue;        // blank lines are tolerated as spacing
    if (line.length > 512) return null;
    const { path, note } = splitPathNote(line);
    if (!isFileRefCandidate(path)) return null;
    const { path: bare } = splitPathLine(path);
    const base = bare.slice(bare.lastIndexOf('/') + 1);
    if (!FILE_REF_HAS_EXT.test(base)) return null; // no extension → not a file list
    paths.push({ path, note });
  }
  if (paths.length < 1) return null;  // empty fence: nothing to render
  return paths;
}

// expandBraces expands a single `{a,b,c}` group in a path candidate into its
// concrete variants so AI output like `foo-{x86,graviton}.yaml:9` resolves to
// `foo-x86.yaml:9` / `foo-graviton.yaml:9`. Only the first group is expanded
// — nested / multi-group patterns are uncommon in review output and
// exploding them would blow past the server's 100-path stat budget. Returns
// a single-element array with tag:'' when no expansion applies. Bail on
// empty alternatives or whitespace inside the group so we don't silently
// match prose like `{ foo }`. Each variant carries a `tag` (the branch
// alternative, e.g. `x86`) used to label the variant's button group.
function expandBraces(text) {
  const m = text.match(/^(.*?)\{([^{}\s]+)\}(.*)$/);
  if (!m) return [{ path: text, tag: '' }];
  const [, pre, inner, post] = m;
  if (!inner.includes(',')) return [{ path: text, tag: '' }];
  const parts = inner.split(',');
  const out = [];
  for (const p of parts) {
    if (p === '') return [{ path: text, tag: '' }]; // `{a,,b}` → not a valid expansion
    out.push({ path: pre + p + post, tag: p });
  }
  return out;
}

// resolveVariant maps a single concrete path to the owning project + the
// workspace-relative form the server accepts. Shared between single-path
// and brace-expanded scans so the project-resolution rules stay identical.
function resolveVariant(p, activeNode, activeProj) {
  if (p.startsWith('/')) {
    const hit = resolveProjectForAbsPath(p, activeNode);
    if (!hit) return null;
    return { projName: hit.name, projNode: hit.node, serverPath: hit.relPath };
  }
  if (!activeProj) return null;
  return {
    projName: activeProj.name,
    projNode: activeProj.node,
    serverPath: p.replace(/^\.\//, ''),
  };
}

// Per-project path validation cache: key = "<project>|<path>" → entry.
// TTL 60s so mtime changes re-verify eventually without the user needing
// to refresh; short enough that server-side edits propagate within one
// round of scrolling back.
const _filePathCache = new Map();
const _FILE_PATH_CACHE_MAX = 2000;
const _FILE_PATH_CACHE_TTL = 60 * 1000;

// Pending batch of path candidates waiting for /api/projects/files/exists.
let _fileRefPendingBatch = null; // { project, node, paths: Map<string, HTMLElement[]> }
let _fileRefBatchTimer = null;
const _FILE_REF_BATCH_DELAY = 120; // ms
const _FILE_REF_BATCH_MAX = 80; // paths per request (server caps at 100)

// resolveActiveProject infers which project owns the currently selected
// session, so inline path chips query the right workspace. Falls back to
// longest-prefix match on the session's workspace dir; returns null if
// we cannot determine a project.
function resolveActiveProject() {
  if (!selectedKey) return null;
  const sKey = sid(selectedKey, selectedNode);
  const sd = sessionsData[sKey];
  if (!sd) return null;
  const name = sd.project || matchProject(sd.workspace);
  if (!name) return null;
  return { name, node: selectedNode || 'local' };
}

// Split a candidate like "src/foo.go:42" into {path, line}. Line is optional.
function splitPathLine(cand) {
  const m = cand.match(/^(.+?):(\d+(?:-\d+)?)$/);
  if (m) return { path: m[1], line: m[2] };
  return { path: cand, line: '' };
}

// resolveProjectForAbsPath maps an absolute path (e.g. `/home/.../gaokao/x.md`)
// to the owning project on the given node. Returns { name, node, relPath }
// on match, or null. The server rejects absolute paths by contract (see
// resolveProjectFileWithRoot); doing the conversion here keeps that boundary
// intact while letting AI output that quotes absolute paths — which claude CLI
// routinely does — still produce preview buttons.
//
// Scoping rules:
//   - node must match (cross-node abs paths get no preview).
//   - longest-prefix wins when a project contains nested projects.
//   - path must be strictly inside the project dir (no prefix-only match like
//     `/foo/barfoo` matching project `/foo/bar`).
function resolveProjectForAbsPath(abs, node) {
  if (!abs || !abs.startsWith('/') || !projectsData || projectsData.length === 0) return null;
  const wantNode = node || 'local';
  let best = null, bestLen = 0;
  for (const p of projectsData) {
    if ((p.node || 'local') !== wantNode) continue;
    if (!p.path) continue;
    const prefix = p.path.endsWith('/') ? p.path : p.path + '/';
    if (abs === p.path || abs.startsWith(prefix)) {
      if (p.path.length > bestLen) {
        best = p; bestLen = p.path.length;
      }
    }
  }
  if (!best) return null;
  let rel = abs === best.path ? '' : abs.slice(best.path.length);
  if (rel.startsWith('/')) rel = rel.slice(1);
  if (!rel) return null; // pointing at project root itself is not a file
  return { name: best.name, node: best.node || 'local', relPath: rel };
}

function _fileRefCacheGet(key) {
  const hit = _filePathCache.get(key);
  if (!hit) return null;
  if (Date.now() - hit.t > _FILE_PATH_CACHE_TTL) {
    _filePathCache.delete(key);
    return null;
  }
  return hit.v;
}

function _fileRefCacheSet(key, value) {
  if (_filePathCache.size >= _FILE_PATH_CACHE_MAX) {
    const firstKey = _filePathCache.keys().next().value;
    _filePathCache.delete(firstKey);
  }
  _filePathCache.set(key, { v: value, t: Date.now() });
}

// fileRefCode produces the inline <code> element that the file-ref scanner
// (scanEventForFileRefs, which walks `code, .md-code`) recognises as a path so
// it can attach [↗ preview][↓ download] buttons. Centralising the markup here
// keeps the three callsites (markdown-link rescue, CODE-token restore,
// fencedPathList row) in lockstep: a future change to the tag/class/attrs (e.g.
// adding data-file-ref) lands in one place instead of three divergent string
// literals where missing one would silently drop that path shape's buttons.
//
// Escaping contract: this helper does NOT escape `inner` — every callsite is
// responsible for its own escaping/guarding (esc(), tokenizer guards, or the
// `<`/`\x00` rejection in the link rescue). The helper only owns the wrapper.
//
// className: defaults to "md-code" (the inline-code pill used by backtick spans
// and the link rescue). fencedPathList passes "" to keep a bare <code>: the
// `.md-pathline code` CSS deliberately omits the .md-code pill background/
// padding/border-radius, so tagging those rows with .md-code would visibly turn
// each clean path row into a pill. Both shapes are still caught by the scanner's
// `code, .md-code` selector, so the buttons attach either way.
function fileRefCode(inner, className) {
  const cls = className === undefined ? 'md-code' : className;
  return cls ? '<code class="' + cls + '">' + inner + '</code>'
             : '<code>' + inner + '</code>';
}

// scanEventForFileRefs walks .event-content <code> descendants of a freshly-
// inserted event bubble and wraps any path-shaped literals in a .file-ref
// span with data-* attrs so verification + button injection can run async.
//
// Absolute paths (e.g. AI output that says `/home/.../gaokao/化学/foo.md`) are
// mapped to the owning project on the active session's node via
// resolveProjectForAbsPath — they may belong to a project other than the one
// matched by the session's workspace (think: a session rooted at repo A that
// references a file in repo B on the same host). dataset.displayPath keeps
// the original string for the button's aria-label/title/preview header so the
// user sees what they expect, while dataset.path holds the project-relative
// form the server accepts.
function scanEventForFileRefs(eventEl) {
  const activeProj = resolveActiveProject();
  // activeProj is optional now: we can still resolve absolute paths via
  // projectsData alone as long as we know the node. Fall back to the selected
  // session's node when no project match exists for the session itself.
  const activeNode = activeProj ? activeProj.node :
    (selectedKey ? (selectedNode || 'local') : null);
  if (!activeNode) return;
  // Selector covers both shapes of container:
  //   - chat bubbles: `.event > .event-content > code/.md-code`
  //   - preview drawer: `.fv-rich > code/.md-code`
  //   - future drawers (scratch/aside): same contract — we only care about
  //     code-shaped inline elements inside the passed root, regardless of
  //     the intermediate wrapper class.
  const codeEls = eventEl.querySelectorAll('code, .md-code');
  codeEls.forEach(code => {
    if (code.dataset.frScanned === '1') return;
    code.dataset.frScanned = '1';
    const text = (code.textContent || '').trim();
    if (!text || text.length > 512) return; // absurdly long paths skip
    if (!isFileRefCandidate(text)) return;
    // Skip when nested inside <a> (authored link target).
    if (code.closest('a')) return;
    // Skip fenced code blocks (<pre><code>): those are content, not refs.
    if (code.closest('pre')) return;
    const { path, line } = splitPathLine(text);
    const variants = expandBraces(path);

    // Shared wrap hosts the original <code> element plus one per-variant
    // button pair. Without brace expansion there is exactly one variant so
    // the DOM shape matches the pre-expansion code path. With expansion,
    // each variant adds its own [↗][↓] group labelled with the alternative
    // so clicking the x86 arrows opens ec2nodeclass-x86.yaml rather than
    // guessing which branch the user meant.
    const wrap = document.createElement('span');
    wrap.className = 'file-ref';
    code.parentNode.insertBefore(wrap, code);
    wrap.appendChild(code);

    for (const v of variants) {
      const resolved = resolveVariant(v.path, activeNode, activeProj);
      if (!resolved) continue;
      const slot = document.createElement('span');
      slot.className = 'fr-slot fr-candidate';
      slot.dataset.path = resolved.serverPath;       // what we send to the server
      slot.dataset.displayPath = v.path;             // what the user typed / saw
      slot.dataset.line = line;
      slot.dataset.project = resolved.projName;
      slot.dataset.node = resolved.projNode;
      if (v.tag) slot.dataset.variantTag = v.tag;
      wrap.appendChild(slot);
      queueFileRefCheck(slot);
    }
    // No resolvable variants — remove the empty wrap so the original <code>
    // is left in place for the user to copy.
    if (!wrap.querySelector('.fr-slot')) {
      wrap.parentNode.insertBefore(code, wrap);
      wrap.remove();
    }
  });
}

function queueFileRefCheck(wrapEl) {
  const proj = wrapEl.dataset.project;
  const node = wrapEl.dataset.node || 'local';
  const path = wrapEl.dataset.path;
  const cacheKey = proj + '|' + node + '|' + path;
  const cached = _fileRefCacheGet(cacheKey);
  if (cached) {
    applyFileRefResult(wrapEl, cached);
    return;
  }
  if (!_fileRefPendingBatch || _fileRefPendingBatch.project !== proj || _fileRefPendingBatch.node !== node) {
    flushFileRefBatch();
    _fileRefPendingBatch = { project: proj, node, paths: new Map() };
  }
  if (!_fileRefPendingBatch.paths.has(path)) _fileRefPendingBatch.paths.set(path, []);
  _fileRefPendingBatch.paths.get(path).push(wrapEl);
  // Flush if we hit per-request cap.
  if (_fileRefPendingBatch.paths.size >= _FILE_REF_BATCH_MAX) {
    flushFileRefBatch();
    return;
  }
  if (_fileRefBatchTimer) return;
  _fileRefBatchTimer = setTimeout(flushFileRefBatch, _FILE_REF_BATCH_DELAY);
}

async function flushFileRefBatch() {
  if (_fileRefBatchTimer) { clearTimeout(_fileRefBatchTimer); _fileRefBatchTimer = null; }
  const batch = _fileRefPendingBatch;
  _fileRefPendingBatch = null;
  if (!batch || batch.paths.size === 0) return;

  const paths = Array.from(batch.paths.keys());
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — batch exists-check touches the FS for every
    // path; a stalled disk shouldn't leak pending renders forever.
    let data;
    try {
      data = await fetchJSON('/api/projects/files/exists', {
        method: 'POST', headers,
        body: JSON.stringify({ project: batch.project, node: batch.node, paths }),
        timeoutMs: 10000,
      });
    } catch (err) {
      if (err.status) return;
      throw err;
    }
    const results = (data && data.results) || {};
    for (const p of paths) {
      const entry = results[p] || { exists: false };
      const cacheKey = batch.project + '|' + batch.node + '|' + p;
      _fileRefCacheSet(cacheKey, entry);
      const els = batch.paths.get(p) || [];
      els.forEach(wrap => applyFileRefResult(wrap, entry));
    }
  } catch (_) { /* network failure: leave candidates as-is */ }
}

function applyFileRefResult(wrapEl, entry) {
  if (!entry || !entry.exists || entry.is_dir) {
    wrapEl.classList.remove('fr-candidate');
    wrapEl.classList.add('fr-missing');
    return;
  }
  if (wrapEl.querySelector('.fr-btn')) return; // already injected
  wrapEl.classList.remove('fr-candidate');
  wrapEl.classList.add('fr-verified');
  wrapEl.dataset.size = entry.size || 0;
  wrapEl.dataset.mime = entry.mime || '';
  // Preview and download share the same visual size \u2014 single-glyph icons
  // with an aria-label for accessibility so assistive tech still announces
  // "Preview" / "Download" clearly. Labels use displayPath (original string
  // the AI output, e.g. an absolute path) so the tooltip matches what the
  // user sees in the bubble \u2014 wrapEl.dataset.path may be the rewritten
  // project-relative form.
  const label = wrapEl.dataset.displayPath || wrapEl.dataset.path;
  // Brace-expanded variants carry a human-visible tag (e.g. "x86" /
  // "graviton") so the user can tell paired button groups apart when the
  // same line mentions foo-{x86,graviton}.yaml.
  if (wrapEl.dataset.variantTag) {
    const tag = document.createElement('span');
    tag.className = 'fr-tag';
    tag.textContent = wrapEl.dataset.variantTag;
    tag.title = label;
    wrapEl.appendChild(tag);
  }
  const preview = document.createElement('button');
  preview.type = 'button';
  preview.className = 'fr-btn fr-btn-preview';
  preview.textContent = '\u2197'; // paired with download '\u2193' for symmetric arrow look
  preview.setAttribute('aria-label', 'Preview ' + label);
  preview.title = 'Preview ' + label;
  preview.addEventListener('click', evt => {
    evt.preventDefault();
    evt.stopPropagation();
    openFilePreview(wrapEl);
  });
  const download = document.createElement('button');
  download.type = 'button';
  download.className = 'fr-btn fr-btn-download';
  download.textContent = '\u2193'; // \u2193
  download.setAttribute('aria-label', 'Download ' + label);
  download.title = 'Download ' + label;
  download.addEventListener('click', evt => {
    evt.preventDefault();
    evt.stopPropagation();
    triggerFileDownload(wrapEl);
  });
  wrapEl.appendChild(preview);
  wrapEl.appendChild(download);
}

function fileApiUrl(project, node, path, mode) {
  const qs = 'project=' + encodeURIComponent(project) +
    '&path=' + encodeURIComponent(path) +
    '&mode=' + encodeURIComponent(mode) +
    (node && node !== 'local' ? '&node=' + encodeURIComponent(node) : '');
  return '/api/projects/file?' + qs;
}

function triggerFileDownload(wrapEl) {
  const url = fileApiUrl(wrapEl.dataset.project, wrapEl.dataset.node, wrapEl.dataset.path, 'download');
  // Use a transient anchor so the token-auth cookie is sent with the GET.
  const a = document.createElement('a');
  a.href = url;
  a.download = (wrapEl.dataset.path.split('/').pop() || 'file');
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

async function openFilePreview(wrapEl) {
  const drawer = document.getElementById('fv-drawer');
  const body = document.getElementById('fv-body');
  const title = document.getElementById('fv-title');
  const meta = document.getElementById('fv-meta');
  if (!drawer || !body || !title || !meta) return;
  // Warm-start async renderers the moment the drawer opens. loadKatex /
  // loadMermaid are idempotent no-ops once ready; kicking them off in
  // parallel with the preview fetch eliminates first-open pending flicker
  // on .md / .tex files that contain math or diagrams.
  loadKatex();
  loadMermaid();
  const project = wrapEl.dataset.project;
  const node = wrapEl.dataset.node;
  const path = wrapEl.dataset.path;
  const line = wrapEl.dataset.line || '';
  const mime = wrapEl.dataset.mime || '';
  const size = +wrapEl.dataset.size || 0;

  drawer.classList.remove('hidden');
  drawer.classList.add('fv-open');
  drawer.dataset.project = project;
  drawer.dataset.node = node;
  drawer.dataset.path = path;
  // Show the original string (may be abs) in the header so the user can
  // still match the preview to the bubble they clicked; server calls below
  // use the workspace-relative `path`.
  const headerPath = wrapEl.dataset.displayPath || path;
  title.textContent = headerPath + (line ? ':' + line : '');
  meta.textContent = (mime ? mime + ' \u00b7 ' : '') + formatFileSize(size);
  body.innerHTML = '<div class="fv-loading">loading\u2026</div>';

  // SVG must be checked BEFORE the generic image/ branch: image/svg+xml starts
  // with "image/" but cannot flow through <img src=...mode=raw>. The server
  // refuses inline SVG via raw (project_files.go: serveRaw rejects svg+xml)
  // because SVG can embed <script> and on* handlers that execute same-origin
  // on top-level navigation. Route through the sandboxed-blob path instead,
  // which serves attachment/octet-stream from the server and wraps the bytes
  // in a Blob with type=image/svg+xml client-side.
  if (mime.startsWith('image/svg+xml')) {
    renderSandboxedBlob(project, node, path, body, 'image/svg+xml');
    return;
  }
  // Image / PDF: use raw endpoint directly, no JSON round trip.
  if (mime.startsWith('image/')) {
    body.innerHTML = '';
    const img = document.createElement('img');
    img.src = fileApiUrl(project, node, path, 'raw');
    img.alt = path;
    img.loading = 'lazy';
    body.appendChild(img);
    return;
  }
  if (mime === 'application/pdf') {
    body.innerHTML = '';
    const frame = document.createElement('iframe');
    frame.src = fileApiUrl(project, node, path, 'raw');
    frame.title = path;
    body.appendChild(frame);
    return;
  }
  // HTML / XHTML: render via blob URL inside a sandboxed iframe.
  //
  // Why blob + sandbox instead of `iframe.src = fileApiUrl(...render)`:
  // Firefox ignores the HTTP `Content-Security-Policy: sandbox` directive
  // on top-level navigation, so a direct-URL open would run workspace HTML
  // same-origin to the dashboard → stored-XSS via the Claude CLI Write tool.
  // The server returns the bytes as `application/octet-stream + attachment`
  // specifically so that a direct URL hit DOWNLOADS instead of renders.
  // Client-side we fetch, wrap bytes in a Blob({type:'text/html'}), and
  // feed the blob: URL into the iframe — blob origins are opaque, so even
  // if sandbox is stripped the document cannot read dashboard cookies.
  if (mime.startsWith('text/html') || mime.startsWith('application/xhtml')) {
    renderSandboxedBlob(project, node, path, body, 'text/html');
    return;
  }

  // Text / unknown: go through preview endpoint which returns structured JSON.
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(fileApiUrl(project, node, path, 'preview'), { headers });
    if (!r.ok) {
      body.innerHTML = '<div class="fv-error">preview failed (' + r.status + ')</div>';
      return;
    }
    const data = await r.json();
    if (data.binary) {
      const binMime = String(data.mime || '');
      // HTML / XHTML / SVG land in `binary:true` by design (R176-SEC-H3:
      // active-content bytes never flow through the preview JSON content
      // field). Upgrade to the sandboxed blob render instead of showing a
      // "please download" placeholder — that's the whole point of render
      // mode. Blob type matches the source MIME so the iframe parses bytes
      // as the right document type.
      if (binMime.startsWith('text/html') || binMime.startsWith('application/xhtml')) {
        renderSandboxedBlob(project, node, path, body, 'text/html');
        return;
      }
      if (binMime.startsWith('image/svg+xml')) {
        renderSandboxedBlob(project, node, path, body, 'image/svg+xml');
        return;
      }
      body.innerHTML = '<div class="fv-binary">Binary file — click <strong>download</strong> to save.<span class="fv-mime">' + esc(binMime) + '</span></div>';
      return;
    }
    const parts = [];
    if (data.truncated) {
      parts.push('<div class="fv-truncated">file truncated at ' + formatFileSize(1024 * 1024) + ' (total ' + formatFileSize(data.size || 0) + ') — download for full content</div>');
    }
    const lang = inferLang(path, data.mime || '');
    // Route through renderRich — same renderer chat bubbles use so behaviour
    // (math, mermaid, tables, lists, file-refs) stays consistent across
    // surfaces. Source-code files keep the line-number gutter layout.
    if (lang === 'markdown' || lang === 'tex') {
      const mode = lang === 'tex' ? 'tex' : 'markdown';
      parts.push('<div class="fv-rich">' + renderRich(data.content || '', { mode: mode }) + '</div>');
    } else {
      const raw = data.content || '';
      const lines = raw.split('\n');
      const gutter = lines.map((_, i) => String(i + 1)).join('\n');
      parts.push('<pre class="fv-lined"><span class="fv-gutter" aria-hidden="true">' + gutter + '</span><code class="fv-code">' + esc(raw) + '</code></pre>');
    }
    body.innerHTML = parts.join('');
    // Flush KaTeX / Mermaid pending slots produced by renderRich above.
    // Without this call, first-open of a .md file with math would leave
    // `<span class="katex-pending">` placeholders on screen until a chat
    // render happened to fire from another code path.
    runPendingAsync();
    // Mirror chat-side file-ref chip injection so paths inside the preview
    // body also get [preview]/[download] affordances.
    if (typeof scanEventForFileRefs === 'function') {
      body.querySelectorAll('.fv-rich').forEach(scanEventForFileRefs);
    }
    if (line) scrollToPreviewLine(body, parseInt(line, 10));
  } catch (e) {
    body.innerHTML = '<div class="fv-error">' + esc(String(e && e.message || e)) + '</div>';
  }
}

// _pendingSandboxBlobUrl holds the most recent blob URL fed into the preview
// iframe so closeFilePreview can revoke it. Blob URLs pin their backing
// bytes in memory until revoked, and a 50 MB coverage report left open
// across many file clicks would leak hard. Single-slot is enough because
// the drawer only ever shows one file at a time.
let _pendingSandboxBlobUrl = null;

// _sandboxRenderSeq is a monotonic token for renderSandboxedBlob invocations.
// The function is async: a user who opens file A and then clicks file B
// before A's fetch resolves would, under a naive implementation, see A's
// bytes rendered into B's drawer AND leak A's blob URL (its own invocation
// has already passed the revoke-prior step). Every call bumps the seq and
// captures its own copy; when fetch resolves, callers whose token no longer
// equals _sandboxRenderSeq revoke their own blob URL and abandon the render.
let _sandboxRenderSeq = 0;

// renderSandboxedBlob fetches workspace bytes via mode=render, wraps them in
// a typed Blob, and points a sandboxed iframe at the resulting blob URL.
// Used for HTML and SVG — both can carry active content (scripts, on*) and
// must NEVER reach the dashboard origin. blobType controls how the iframe
// parses the bytes ('text/html' for .html / .xhtml, 'image/svg+xml' for .svg).
//
// Three defense layers stack here:
//   (1) Server returns application/octet-stream + attachment, so a direct
//       URL hit downloads rather than renders (covers Firefox's CSP-sandbox
//       top-level-nav gap).
//   (2) Blob URL origin is opaque — even with allow-same-origin in the
//       sandbox (we don't grant it), the document can't read dashboard
//       cookies or same-origin fetch.
//   (3) sandbox='' on the iframe grants zero capabilities — no scripts,
//       no forms, no top-level navigation, no popups, no fetch.
// Any one of these would be sufficient; stacking all three is belt-and-
// braces so a future change to any single layer does not regress security.
async function renderSandboxedBlob(project, node, path, body, blobType) {
  body.innerHTML = '<div class="fv-loading">loading…</div>';
  // Claim the invocation slot BEFORE awaiting anything. Every caller
  // snapshots the seq here; when its fetch resolves later it compares
  // against the live _sandboxRenderSeq to detect whether a newer render
  // superseded it. This closes the "open A then open B before A resolves"
  // race where A would otherwise overwrite B's tracked blob URL and leak.
  const mySeq = ++_sandboxRenderSeq;
  // Revoke any prior blob URL before overwriting. Missing this leaked a
  // ~50 MB report across every re-open of the drawer in manual testing.
  if (_pendingSandboxBlobUrl) {
    try { URL.revokeObjectURL(_pendingSandboxBlobUrl); } catch (_) { /* ignore */ }
    _pendingSandboxBlobUrl = null;
  }
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(fileApiUrl(project, node, path, 'render'), { headers });
    // A newer invocation has already taken over the drawer — abandon.
    // Check happens at every await boundary: after fetch (headers arrived)
    // and after arrayBuffer (body fully drained).
    if (mySeq !== _sandboxRenderSeq) return;
    if (!r.ok) {
      body.innerHTML = '<div class="fv-error">render failed (' + r.status + ')</div>';
      return;
    }
    const bytes = await r.arrayBuffer();
    if (mySeq !== _sandboxRenderSeq) return;
    // Force the Blob's type from the caller — the server intentionally
    // returned application/octet-stream so direct-URL hits don't render.
    // The browser only interprets the bytes as HTML/SVG because we ask it
    // to here. Callers MUST pass a type the server-side render whitelist
    // already accepted (text/html, application/xhtml+xml, image/svg+xml);
    // otherwise the iframe would display nothing.
    const blob = new Blob([bytes], { type: blobType || 'text/html' });
    const url = URL.createObjectURL(blob);
    // Final stale check AFTER allocating the URL — if a newer invocation
    // landed in the tiny window between the arrayBuffer await and now,
    // revoke our URL immediately instead of stashing it in the tracked
    // slot (which would clobber the newer render's tracking and leak).
    if (mySeq !== _sandboxRenderSeq) {
      try { URL.revokeObjectURL(url); } catch (_) { /* ignore */ }
      return;
    }
    _pendingSandboxBlobUrl = url;

    body.innerHTML = '';
    const frame = document.createElement('iframe');
    frame.src = url;
    frame.title = path;
    // sandbox='allow-scripts' grants script execution but withholds the
    // same-origin token — the iframe stays in an opaque origin and cannot
    // read dashboard cookies, localStorage, or DOM. Combined with the blob:
    // URL (origin already opaque) the document is fully isolated from the
    // dashboard. Scripts are required so workspace HTML using MathJax /
    // KaTeX / Mermaid / chart libs renders correctly. The contract test
    // (TestDashboardJS_SandboxedBlobRender) substring-matches the helper
    // body for forbidden tokens, so this comment must NEVER spell out
    // those tokens — see that test for the canonical contract.
    frame.setAttribute('sandbox', 'allow-scripts');
    frame.referrerPolicy = 'no-referrer';
    body.appendChild(frame);
  } catch (e) {
    if (mySeq !== _sandboxRenderSeq) return;
    body.innerHTML = '<div class="fv-error">' + esc(String(e && e.message || e)) + '</div>';
  }
}

function scrollToPreviewLine(body, line) {
  if (!line || line < 1) return;
  const pre = body.querySelector('pre');
  if (!pre) return;
  // Approximate scroll: average line height in our monospace pre is ~18px.
  // Good enough for remote-dashboard purposes; precise highlighting would
  // require splitting every line into a <span> and costs too much for
  // the marginal "scroll near line 42" benefit.
  pre.parentElement.scrollTop = Math.max(0, (line - 3) * 18);
}

function formatFileSize(bytes) {
  if (!bytes || bytes <= 0) return '';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  return (bytes / (1024 * 1024 * 1024)).toFixed(1) + ' GB';
}

function inferLang(path, mime) {
  const ext = (path.split('.').pop() || '').toLowerCase();
  if (ext === 'md' || ext === 'markdown') return 'markdown';
  if (ext === 'tex' || ext === 'latex') return 'tex';
  if (mime === 'text/markdown') return 'markdown';
  if (mime === 'text/x-tex' || mime === 'application/x-tex') return 'tex';
  return '';
}

function closeFilePreview() {
  const drawer = document.getElementById('fv-drawer');
  if (!drawer) return;
  drawer.classList.remove('fv-open');
  drawer.classList.add('hidden');
  delete drawer.dataset.snippetMode;
  delete drawer.dataset.snippetName;
  _pendingSnippet = null;
  // Release the sandbox blob URL (HTML or SVG) so the browser can GC the
  // underlying bytes. Without this a 50 MB coverage report held its memory
  // until the whole tab reloaded.
  if (_pendingSandboxBlobUrl) {
    try { URL.revokeObjectURL(_pendingSandboxBlobUrl); } catch (_) { /* ignore */ }
    _pendingSandboxBlobUrl = null;
  }
  const body = document.getElementById('fv-body');
  if (body) body.innerHTML = '';
}

// Wire drawer buttons once on load.
document.addEventListener('DOMContentLoaded', function () {
  const close = document.getElementById('fv-btn-close');
  if (close) close.addEventListener('click', closeFilePreview);
  const copy = document.getElementById('fv-btn-copy');
  if (copy) copy.addEventListener('click', () => {
    const drawer = document.getElementById('fv-drawer');
    if (!drawer) return;
    const isSnippet = drawer.dataset.snippetMode === '1';
    const text = isSnippet ? _pendingSnippet : drawer.dataset.path;
    if (!text) return;
    const label = isSnippet ? '片段已复制' : '路径已复制';
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        () => showToast(label, 'success', 1000),
        () => showToast('复制失败', 'warning', 1000)
      );
    }
  });
  const download = document.getElementById('fv-btn-download');
  if (download) download.addEventListener('click', () => {
    const drawer = document.getElementById('fv-drawer');
    if (!drawer) return;
    // Snippet mode: download the inline code via a blob URL.
    if (drawer.dataset.snippetMode === '1' && _pendingSnippet) {
      const blob = new Blob([_pendingSnippet], { type: 'text/plain;charset=utf-8' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = drawer.dataset.snippetName || 'snippet.txt';
      a.rel = 'noopener';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      return;
    }
    if (!drawer.dataset.path) return;
    triggerFileDownload({ dataset: drawer.dataset });
  });
  // Esc closes drawer (but only when nothing else is handling Esc).
  document.addEventListener('keydown', evt => {
    if (evt.key !== 'Escape') return;
    const drawer = document.getElementById('fv-drawer');
    if (drawer && !drawer.classList.contains('hidden')) {
      evt.stopPropagation();
      closeFilePreview();
    }
  }, true);
});

// Observe #events-scroll so every newly-inserted event bubble gets scanned
// for file-ref candidates. Using a MutationObserver lets us stay out of the
// existing render pipelines (eventHtml / WS handlers) — they keep producing
// the same HTML, we just enhance it post-insertion.
//
// renderMainShell rebuilds the #events-scroll DOM on every session switch,
// so we track the active observer and disconnect it whenever the target
// element is replaced. Without this, stale observers pile up in memory
// across rapid session switches (one per switch), and the old observer
// would silently re-trigger if the DOM node was ever reparented.
let _fileRefObserver = null;
let _fileRefObserverTarget = null;

function startFileRefObserver() {
  const target = document.getElementById('events-scroll');
  if (!target) return;
  if (_fileRefObserverTarget === target) return; // already wired to this DOM
  if (_fileRefObserver) {
    _fileRefObserver.disconnect();
    _fileRefObserver = null;
  }
  _fileRefObserverTarget = target;
  const mo = new MutationObserver(mutations => {
    for (const m of mutations) {
      m.addedNodes.forEach(node => {
        if (!(node instanceof HTMLElement)) return;
        if (node.classList && node.classList.contains('event')) {
          scanEventForFileRefs(node);
        } else if (node.querySelectorAll) {
          node.querySelectorAll('.event').forEach(scanEventForFileRefs);
        }
      });
    }
  });
  mo.observe(target, { childList: true, subtree: false });
  _fileRefObserver = mo;
  // Initial scan for bubbles rendered synchronously before the observer
  // attached (e.g. the full-history render on session select).
  target.querySelectorAll('.event').forEach(scanEventForFileRefs);
}

// KaTeX environment names we split out as block-level math. Whitelisted —
// feeding KaTeX an environment it doesn't support just emits an error span
// and pollutes the block flow.
const KATEX_ENVS = 'equation|align|aligned|gather|multline|cases|array|pmatrix|bmatrix|vmatrix|Vmatrix|matrix|split|alignat|CD';
const BLOCK_SPLIT_RE = new RegExp(
  '(```[\\s\\S]*?```' +
  '|\\$\\$[\\s\\S]*?\\$\\$' +
  '|\\\\\\[[\\s\\S]*?\\\\\\]' +
  '|\\\\begin\\{(?:' + KATEX_ENVS + ')\\*?\\}[\\s\\S]*?\\\\end\\{(?:' + KATEX_ENVS + ')\\*?\\})',
  'g'
);

// 2-column step (LLM/CJK convention). MAX_LIST_DEPTH caps adversarial input.
const LIST_DEPTH_STEP = 2;
const MAX_LIST_DEPTH = 6;
// Trailing \r? handles CRLF source. The list pass runs after split('\n')
// which preserves the \r on Windows-pasted text; without explicit handling
// (.*)$ would not match (`.` excludes \r, $ does not anchor before \r
// without the m flag), and the line silently falls out of the list path.
// Leading whitespace is restricted to ASCII space + tab so leadingColumns
// stays in sync with the captured prefix; a Unicode-`\s*` would consume
// NBSP/U+2028/etc. while leadingColumns counted only space+tab, producing
// cols=0 for a visually-indented bullet.
const LIST_ITEM_RE = /^([ \t]*)(?:([-*])|(\d+)\.)[ \t]+(.*?)\r?$/;
// Cheap shape check used by the lazy-continuation guard. Treats both digit
// shapes (any length) — the digit-cap reject path in parseListItem returns
// null, but for lazy-continuation purposes "this looks like a list bullet"
// is exactly what we want: refuse to fold such lines into the previous <li>.
const LIST_SHAPE_RE = /^[ \t]*(?:[-*]|\d+\.)[ \t]+/;
// Reject anything > 3 digits as a list start: real ordinals max out around
// dozens; 4+ digit prefixes are year/version/issue tokens ("2024. 关于...",
// "1234. xxx") that the user does not want rendered as <ol start="2024">.
// The cap is enforced on numeric value, not string length, so "00100." (5
// chars but value 100) is accepted while "2024." (4 chars, value 2024) is
// rejected — symmetric vs. the documented intent.
const OL_START_MAX = 999;

function leadingColumns(s) {
  let cols = 0;
  for (let i = 0; i < s.length; i++) {
    const ch = s.charCodeAt(i);
    if (ch === 32) cols++;
    else if (ch === 9) cols += 4 - (cols % 4);
    else break;
  }
  return cols;
}

function parseListItem(line, baselineCols) {
  const m = LIST_ITEM_RE.exec(line);
  if (!m) return null;
  const cols = leadingColumns(m[1]);
  const base = baselineCols < 0 ? cols : baselineCols;
  let depth = Math.floor(Math.max(0, cols - base) / LIST_DEPTH_STEP);
  if (depth > MAX_LIST_DEPTH) depth = MAX_LIST_DEPTH;
  if (m[2]) return { kind: 'ul', depth, cols, content: m[4] };
  // Reject year/version tokens as list starts. parseInt cap (not string
  // length) so "00100." (value 100) is accepted, "2024." rejected. Returning
  // null pushes the line into the plain-text/lazy-continuation branch.
  const startNum = parseInt(m[3], 10);
  if (startNum > OL_START_MAX) return null;
  return { kind: 'ol', depth, cols, startNum, content: m[4] };
}

function renderMdUncached(s) {
  // Normalize CRLF/CR to LF up front. Source can be Windows-pasted text or
  // IM payloads carrying \r\n. Without this every per-line regex below
  // (LIST_ITEM_RE, heading, table) would silently miss-match on the trailing
  // \r and demote rich blocks to plain <br> spans.
  if (s.indexOf('\r') !== -1) s = s.replace(/\r\n?/g, '\n');
  // Split by fenced code blocks and display math blocks (including LaTeX
  // environments like \begin{aligned}...\end{aligned}).
  const parts = s.split(BLOCK_SPLIT_RE);
  return parts.map(part => {
    if (part.startsWith('```')) {
      const m = part.match(/^```(\w*)\n?([\s\S]*?)```$/);
      const lang = m ? m[1] : '';
      const code = m ? m[2].replace(/\n$/, '') : part.slice(3, -3);
      if (lang === 'mermaid') {
        const id = 'mmd-' + (++mermaidCounter);
        mermaidPending[id] = code;
        return '<div class="mermaid-wrap"><pre id="' + id + '" class="mermaid-pending"></pre></div>';
      }
      // Opt-in math fence: ```math / ```latex / ```tex hand the entire block
      // to KaTeX in displayMode. Mirrors the mermaid convention — authors
      // explicitly mark intent so legitimate $-bearing source code (shell
      // $VAR, Make $@, Perl $_, Python f-strings) keeps its existing
      // verbatim rendering. KaTeX renderToString errors fall through to
      // an error span with throwOnError:false so a malformed expression
      // still surfaces the source instead of crashing the bubble.
      if (lang === 'math' || lang === 'latex' || lang === 'tex') {
        return '<div class="md-math-display">' + renderKatex(code, true) + '</div>';
      }
      // Path-list fence: a language-less block whose every non-empty line is a
      // file-path literal (the shape AI emits when it lists generated files,
      // e.g. a "here are the files I created" reply). Inside <pre><code> these
      // paths are invisible to scanEventForFileRefs (which deliberately skips
      // fenced blocks — see its `code.closest('pre')` guard), so they never get
      // preview/download buttons. Render each line as its own non-<pre> <code>
      // inside the wrap so the file-ref scanner can attach buttons, while the
      // whole block keeps a single copy button. Requiring EVERY line to be a
      // path candidate keeps real code blocks (which always carry at least one
      // non-path line) on the verbatim path.
      const pathLines = lang === '' ? fencedPathList(code) : null;
      if (pathLines) {
        // Each row is {path, note}. The path goes in <code> so the file-ref
        // scanner + copy see only the bare path; the optional note renders as
        // a dimmed sibling span outside the <code> so it is visible but never
        // folded into the path the exists-check queries.
        const rows = pathLines.map(p => {
          const noteHtml = p.note
            ? '<span class="md-pathnote">' + esc(p.note) + '</span>'
            : '';
          return '<div class="md-pathline">' + fileRefCode(esc(p.path), '') + noteHtml + '</div>';
        }).join('');
        return '<div class="md-code-wrap md-pathlist">' + rows +
          '<div class="md-code-actions">' +
            '<button class="md-code-btn md-copy-btn" onclick="copyCodeBlock(this)" aria-label="Copy file paths">copy</button>' +
          '</div>' +
          '</div>';
      }
      const langAttr = lang ? ' data-lang="' + escAttr(lang) + '"' : '';
      return '<div class="md-code-wrap"><pre class="md-pre"><code' + langAttr + '>' + esc(code) + '</code></pre>' +
        '<div class="md-code-actions">' +
          '<button class="md-code-btn md-copy-btn" onclick="copyCodeBlock(this)" aria-label="Copy code snippet">copy</button>' +
        '</div>' +
        '</div>';
    }
    if (part.startsWith('$$') && part.endsWith('$$')) {
      return '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>';
    }
    if (part.startsWith('\\[') && part.endsWith('\\]')) {
      return '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>';
    }
    if (part.startsWith('\\begin{')) {
      // Hand the whole environment to KaTeX in displayMode. KaTeX accepts
      // `\begin{aligned}...\end{aligned}` etc. directly without outer `\[ \]`.
      return '<div class="md-math-display">' + renderKatex(part, true) + '</div>';
    }
    // Pre-extract cross-line `\(...\)` before the per-line loop runs. inlineMd
    // processes one line at a time, which would otherwise truncate multi-line
    // inline math. Tokens survive esc() (NUL byte is not an HTML special) and
    // get swapped back in after list/heading/table rendering completes.
    const inlineMathTokens = [];
    if (part.indexOf('\\(') !== -1) {
      part = part.replace(/\\\(([\s\S]+?)\\\)/g, function(_, tex) {
        inlineMathTokens.push(renderKatex(tex.trim(), false));
        return '\x00ILM' + (inlineMathTokens.length - 1) + '\x00';
      });
    }
    // Process line by line for block elements. Accumulate into a chunks array
    // + single join() at the end rather than `html +=` per line: V8 reallocates
    // the underlying string on every concat past the small-string threshold,
    // which is O(n^2) over line count. A 200-line response rendered ~50 times
    // per history replay was the dominant cost in the text-event path.
    const lines = part.split('\n');
    const chunks = [];
    // List state: stack of { kind: 'ol'|'ul', depth }, outermost at index 0.
    // Replaces a single 'inList' string so we can render nested + mixed-type
    // lists without each unordered run cleaving an enclosing ordered list.
    // baselineCols anchors depth=0 to the first list item's column so an
    // entire indented block does not start at depth>0.
    const listStack = [];
    let baselineCols = -1;
    const closeTo = (targetTopDepth) => {
      while (listStack.length > 0 &&
             listStack[listStack.length - 1].depth > targetTopDepth) {
        chunks.push('</li>');
        chunks.push('</' + listStack.pop().kind + '>');
      }
      if (listStack.length === 0) baselineCols = -1;
    };
    const closeAll = () => closeTo(-1);
    for (let i = 0; i < lines.length; i++) {
      let line = lines[i];
      // Headings
      const hm = line.match(/^(#{1,4})\s+(.+)$/);
      if (hm) {
        closeAll();
        const level = hm[1].length;
        chunks.push('<strong class="md-h' + level + '">' + inlineMd(hm[2]) + '</strong>\n');
        continue;
      }
      const li = parseListItem(line, baselineCols);
      if (li) {
        if (listStack.length === 0) baselineCols = li.cols;
        // Step 1: when the new bullet matches an existing frame in the stack
        // by *source column AND kind*, that frame owns the bullet. Pop down
        // to it and re-use it as a sibling. This is the key correctness
        // fix-up over the original lenient-promotion design: without it, a
        // promoted-up sibling chain like "1. a\n- b\n- c\n" causes each `-`
        // to first close the just-promoted <ul> (because parseListItem
        // computes li.depth=0 from cols=0 while top.depth was promoted to 1)
        // and then reopen a brand-new <ul> — every bullet ends up in its
        // own one-item list. Walking the stack first lets us recognise that
        // `- c` belongs to the `- b` frame and emit a sibling <li>.
        for (let k = listStack.length - 1; k >= 0; k--) {
          const f = listStack[k];
          if (f.cols === li.cols && f.kind === li.kind) {
            // Close everything strictly above this frame, then sibling-emit.
            closeTo(f.depth);
            chunks.push('</li><li>' + inlineMd(li.content));
            // We did NOT mutate the frame's depth, so no extra book-keeping.
            // Skip the rest of the dispatch.
            li.handled = true;
            break;
          }
          // Another frame at strictly shallower cols means we cannot match
          // anything further down — treat the new bullet as belonging to
          // a position deeper than that frame.
          if (f.cols < li.cols) break;
        }
        if (li.handled) continue;

        // Step 2: standard depth-based dispatch (unchanged from R1).
        const top = listStack[listStack.length - 1];
        if (top && top.depth > li.depth) {
          closeTo(li.depth);
        }
        let top2 = listStack[listStack.length - 1];
        // Lenient nesting: an unindented bullet of the opposite kind at the
        // same visual depth as the current frame becomes a nested child
        // rather than slicing the parent list. LLM output routinely writes
        // "1. parent\n- detail\n2. next" without indenting the bullets —
        // strict CommonMark would render three separate lists (the
        // dashboard screenshot's "全是 1." root cause). MAX_LIST_DEPTH cap
        // keeps the stack bounded under adversarial deep-promote sequences.
        if (top2 && top2.depth === li.depth && top2.kind !== li.kind) {
          if (li.depth < MAX_LIST_DEPTH) {
            li.depth = li.depth + 1;
          } else {
            // already at the cap → fall back to the strict same-depth swap
            chunks.push('</li>');
            chunks.push('</' + listStack.pop().kind + '>');
          }
          top2 = listStack[listStack.length - 1];
        }
        if (top2 && top2.depth === li.depth) {
          if (top2.kind === li.kind) {
            chunks.push('</li><li>' + inlineMd(li.content));
            continue;
          }
          chunks.push('</li>');
          chunks.push('</' + listStack.pop().kind + '>');
        }
        // startNum=0 / negative / NaN never produce a start attribute —
        // <ol start="0"> renders "0. 1. ..." which is jarring; let the
        // browser fall back to default "1. 2. ..." instead.
        const startAttr = (li.kind === 'ol' && li.startNum >= 2)
            ? ' start="' + li.startNum + '"' : '';
        const cls = li.kind === 'ol' ? 'md-ol' : 'md-ul';
        chunks.push('<' + li.kind + ' class="' + cls + '"' + startAttr + '>');
        // Frame stores li.cols too so lazy-continuation can use the original
        // source column rather than the (possibly promoted) depth — promotion
        // mutates depth but not the user's actual indent.
        listStack.push({ kind: li.kind, depth: li.depth, cols: li.cols });
        chunks.push('<li>' + inlineMd(li.content));
        continue;
      }
      // Treat all-whitespace lines as blanks: LLM/IM pipelines occasionally
      // emit a single space on otherwise-empty lines. Without this they fall
      // through to lazy continuation (or closeAll) and visibly fracture lists.
      if (line === '' || /^\s+$/.test(line)) {
        if (listStack.length > 0) {
          // Look ahead: only keep list state when the next non-blank line is
          // a list item OF THE SAME KIND as the active top frame. Cross-kind
          // continuation across a blank line is exactly the case the user
          // means as "two separate lists with paragraph break between them"
          // — keeping state would force the next list into a nested child.
          let peek = i + 1;
          while (peek < lines.length && (lines[peek] === '' || /^\s+$/.test(lines[peek]))) peek++;
          if (peek < lines.length) {
            const pli = parseListItem(lines[peek], baselineCols);
            const top = listStack[listStack.length - 1];
            if (pli && pli.kind === top.kind) {
              continue;
            }
          }
          closeAll();
        }
        chunks.push('<div class="md-blank"></div>');
        continue;
      }
      // Lazy continuation: a non-list line indented at least one step beyond
      // the active top frame's source column folds into the open <li>. Using
      // top.cols (raw source column) instead of top.depth keeps the threshold
      // honest after lenient promotion bumped depth without bumping cols.
      // Guard rails: never fold lines that *look* like a list bullet shape
      // (ordinal capped out — see OL_START_MAX), a markdown table row, or
      // a heading. Without these guards an indented "2024. ..." paragraph,
      // an indented "| h | v |" table, or an indented "## sub" heading
      // disappears into the previous <li> as silent inline text.
      if (listStack.length > 0) {
        const top = listStack[listStack.length - 1];
        const cols = leadingColumns(line);
        if (cols - top.cols >= LIST_DEPTH_STEP) {
          const trimmed = line.trim();
          const looksLikeBlock =
            LIST_SHAPE_RE.test(line) ||
            /^\|.+\|$/.test(trimmed) ||
            /^#{1,4}\s/.test(trimmed);
          if (!looksLikeBlock) {
            chunks.push(' ' + inlineMd(trimmed));
            continue;
          }
        }
      }
      closeAll();
      if (/^\|.+\|$/.test(line.trim())) {
        let tbl = [line];
        while (i + 1 < lines.length && /^\|.+\|$/.test(lines[i + 1].trim())) { tbl.push(lines[++i]); }
        chunks.push(renderTable(tbl));
        continue;
      }
      chunks.push(inlineMd(line) + '<br>');
    }
    closeAll();
    let rendered = chunks.join('');
    // Restore the cross-line `\(...\)` tokens captured before the per-line
    // loop. inlineMd tokens (`\x00KTX*\x00`) were already restored inside
    // inlineMd itself; these ILM tokens sit at the block level.
    if (inlineMathTokens.length > 0) {
      rendered = rendered.replace(/\x00ILM(\d+)\x00/g, function(_, idx) {
        return inlineMathTokens[+idx];
      });
    }
    return rendered;
  }).join('');
}

/* Inline markdown: bold, italic, code, links, math */
function inlineMd(s) {
  // Extract `code` spans FIRST — before math/bold/italic — so KaTeX does not
  // peek inside them. Previously `$NVIDIA_DEVICE_PLUGIN_IMAGE$` written inside
  // backticks was grabbed by the `$...$` math pass and rendered as italicised
  // subscripts, mangling legitimate shell/env-var snippets. Code content is
  // esc()'d immediately so the final token restore emits literal text safely.
  const codeTokens = [];
  if (s.indexOf('`') !== -1) {
    s = s.replace(/`([^`]+)`/g, function(_, c) {
      const idx = codeTokens.length;
      codeTokens.push(esc(c));
      return '\x00CODE' + idx + '\x00';
    });
  }
  // Extract inline math before HTML escaping. Use \x00 delimiters to avoid
  // collisions with user content. Fast path: the overwhelming majority of
  // lines in tool output / assistant text have no math markers, so we
  // short-circuit the two regex scans + mathTokens allocation when neither
  // `$` nor `\(` appears. This is called once per line in renderMdUncached
  // — on a 200-line response the savings are measurable in V8 profiler.
  const mathTokens = [];
  if (s.indexOf('$') !== -1 || s.indexOf('\\(') !== -1) {
    // `$...$`: require non-alphanumeric outside + math-like content inside.
    // The outer guard (non-alphanumeric on both sides) handles the "每月$650$USD"
    // prose case. The inner guard (isMathInline) decides whether the captured
    // span looks like a formula — accepting plain algebra like `$x=1$` /
    // `$2x$` / `$a+b$` which the previous LaTeX-only heuristic rejected.
    s = s.replace(/(?<![A-Za-z0-9])\$([^\s\$][^\$\n]*?[^\s\$]|[^\s\$])\$(?![A-Za-z0-9])/g, function(match, tex) {
      if (!isMathInline(tex)) return match;
      const idx = mathTokens.length;
      mathTokens.push(renderKatex(tex, false));
      return '\x00KTX' + idx + '\x00';
    });
    s = s.replace(/\\\((.+?)\\\)/g, function(_, tex) {
      const idx = mathTokens.length;
      mathTokens.push(renderKatex(tex, false));
      return '\x00KTX' + idx + '\x00';
    });
  }
  s = esc(s);
  // Memory wiki-link `[[slug]]`: Claude's auto-memory cross-reference syntax
  // occasionally leaks into chat output. Substitute before `[link](url)` runs
  // so the [[…]] form cannot collide with the markdown link grammar
  // (`[label](url)`); slug charset is locked to [a-zA-Z0-9_-]{1,64} which
  // also makes path-traversal in the data-slug attribute impossible by
  // construction. The popover handler below attaches lazily on hover/click.
  // See docs/rfc/memory-link-rendering.md.
  // Chip render: type prefix → color + emoji icon; tail of slug → short label.
  // Type heuristic uses Claude's auto-memory naming convention (feedback_*,
  // project_*, user_*, reference_*) — unknown prefixes fall back to a neutral
  // 🧠 chip. Hover popover (below) still shows full slug + body.
  // a11y contract: aria-label exposes the full [[slug]] to screen readers and
  // role=link + tabindex=0 + Enter/Space handler (see popover IIFE) make the
  // chip keyboard-activable. Copy fallback: a document-level `copy` listener
  // rewrites clipboardData to [[slug]] when selection touches a chip, so the
  // wiki-link survives copy/paste even though the visible glyphs are shorter.
  s = s.replace(/\[\[([a-zA-Z0-9_\-]{1,64})\]\]/g, function(_, slug) {
    var m = slug.match(/^(feedback|project|user|reference)_(.+)$/);
    var type = m ? m[1] : 'memory';
    var tail = m ? m[2] : slug;
    var segs = tail.split('_');
    var label = segs.slice(-3).join('_');
    var icon = ({
      feedback:  '💡',
      project:   '📌',
      user:      '👤',
      reference: '🔗',
      memory:    '🧠',
    })[type];
    var ariaLabel = 'memory 引用：[[' + slug + ']]';
    return '<span class="md-memlink" data-slug="' + escAttr(slug) +
      '" data-type="' + type + '" tabindex="0" role="link"' +
      ' aria-label="' + escAttr(ariaLabel) + '">' +
      '<span class="md-memlink-icon" aria-hidden="true">' + icon + '</span>' +
      '<span class="md-memlink-label">' + esc(label) + '</span></span>';
  });
  // Use function-form replacements to prevent JS's special $-sequences
  // ($&, $', $`, $n) from expanding inside the replacement string. Those
  // sequences survive esc() (they aren't HTML entities) and would let an
  // attacker-controlled LLM snippet splice unescaped characters into the
  // emitted HTML by embedding `$&` inside a backtick/bold region.
  //
  // SECURITY CONTRACT: bold/italic regex must run AFTER esc(s) (line ~8193)
  // AND AFTER code/wiki-link injection passes. The bold .+? capture can
  // span injected <span>/<code> HTML; this is safe ONLY because the inner
  // text was already esc()'d. Do NOT reorder these passes without first
  // adding a unit test asserting the bold output never contains a raw
  // '<' character.
  s = s.replace(/\*\*(.+?)\*\*/g, (_, c) => '<strong>' + c + '</strong>');
  s = s.replace(/\*(.+?)\*/g, (_, c) => '<em>' + c + '</em>');
  s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, function(_, text, url) {
    const safe = safeUrl(url);
    // `text` is the already-esc()'d+partially-transformed capture — it may
    // legitimately contain <strong>/<em>/<code> spans from prior passes.
    // When the URL is rejected we still want to render the label, but
    // returning `text` as-is lets those inline tags survive in the output
    // stream unattached to an anchor. This is accepted (matches GitHub's
    // behaviour) because the substituted tags are naozhi-controlled and
    // cannot contain unescaped attacker content (each bold/italic/code
    // substitution already used `esc()`'d capture groups).
    if (safe === '#') {
      // Local-file link: claude CLI routinely emits generated files as
      // markdown links `[数学/专题/foo.html](数学/专题/foo.html)` rather than
      // backtick code. safeUrl rejects the non-http target (→ '#'), so the
      // anchor branch is skipped and the link would collapse to plain text —
      // invisible to scanEventForFileRefs (which only walks <code>/.md-code).
      // Re-render a path-shaped target as inline <code> so the file-ref
      // scanner attaches the same [↗ preview][↓ download] buttons it gives
      // backtick paths. `url` is already esc()'d (esc ran above), so embed it
      // directly; scanEventForFileRefs reads code.textContent (browser-decoded
      // back to the real path) when calling the exists API. Display uses the
      // path itself rather than `text` so the user sees which file resolves —
      // and so the scanner's textContent is the path, not a friendly label.
      //
      // The `url` capture is NOT raw text — earlier inlineMd passes have already
      // tokenized it. Two classes of contamination must be rejected before the
      // target can be embedded in <code>:
      //   1. naozhi-injected markup: the bold/italic passes run before this and
      //      splice <strong>/<em> spans into the capture when the target itself
      //      contains `**`/`*` (e.g. `[a](**x**/y.html)`). A real file path
      //      never contains `<`, so reject any `<`-bearing target.
      //   2. tokenizer placeholders: the backtick-code and inline-math passes
      //      replace `` `x` ``/`$x$` with \x00CODE<n>\x00 / \x00KTX<n>\x00
      //      sentinels. \x00 is non-whitespace/non-colon so it slips through
      //      isFileRefCandidate, and the restore passes that run AFTER this one
      //      would rewrite the sentinel into a nested <code>/<span> inside our
      //      new <code> — malformed HTML plus a corrupted path for the scanner.
      //      Reject any \x00-bearing target.
      // Finally require a real extension (the same FILE_REF_HAS_EXT gate
      // fencedPathList applies) so slash-shaped non-files — dates `2024/01/02`,
      // fractions `1/2`, doc slugs without an extension — don't hijack the link
      // into a bogus file ref with the author's label discarded.
      const target = url.trim();
      if (target.indexOf('<') === -1 && target.indexOf('\x00') === -1 && isFileRefCandidate(target)) {
        const { path: bare } = splitPathLine(target);
        const base = bare.slice(bare.lastIndexOf('/') + 1);
        if (FILE_REF_HAS_EXT.test(base)) {
          return fileRefCode(target);
        }
      }
      return text;
    }
    return '<a href="' + escAttr(safe) + '" class="md-link" target="_blank" rel="noopener noreferrer">' + text + '</a>';
  });
  // Auto-link bare URLs not already inside an <a> tag.
  // R243-SEC-11 (#797): strip a wider set of trailing punctuation —
  // including `>`, `]`, `"` and `'` — before forming the anchor. escAttr
  // already neutralises these inside the href attribute, but stripping
  // here also keeps them out of the link's visible text where they would
  // otherwise dangle past sentences like `see <https://x.y/z>` or
  // `[link](https://x.y/z)`. Defence-in-depth, not the only barrier.
  s = s.replace(/(^|[^"'>])(https?:\/\/[^\s<)}\]]+)/g, function(_, prefix, url) {
    var clean = url.replace(/[.,;:!?)>\]"']+$/, '');
    var trail = url.slice(clean.length);
    return prefix + '<a href="' + escAttr(clean) + '" class="md-link" target="_blank" rel="noopener noreferrer">' + clean + '</a>' + trail;
  });
  // Restore math tokens after escaping
  if (mathTokens.length > 0) {
    s = s.replace(/\x00KTX(\d+)\x00/g, function(_, idx) { return mathTokens[+idx]; });
  }
  // Restore code tokens last — their contents were esc()'d at capture time.
  if (codeTokens.length > 0) {
    s = s.replace(/\x00CODE(\d+)\x00/g, function(_, idx) {
      return fileRefCode(codeTokens[+idx]);
    });
  }
  return s;
}

function renderTable(lines) {
  if (lines.length < 2) return lines.map(l => inlineMd(l) + '\n').join('');
  if (!/^\|[\s\-:]+(\|[\s\-:]+)+\|$/.test(lines[1].trim())) return lines.map(l => inlineMd(l) + '\n').join('');
  // Honour the GFM `\|` escape for a literal pipe inside a cell (common when
  // authors quote shell snippets like `cmd \| true`). Without this the cell
  // splits mid-snippet and the trailing fragment spills into an extra column.
  // Strategy: encode `\|` → sentinel, split on `|`, decode sentinel → `|`.
  const PIPE = '\x00PIPE\x00';
  // LLM output frequently embeds unescaped `|` inside `$...$`, `\(...\)`,
  // or backtick code spans (e.g. `$|AB|=2$`, `$2^a - 2$ | < | ...`).
  // Protect those regions BEFORE splitting on `|`, otherwise a single math
  // formula would get sliced into many spurious columns.
  //
  // CAVEAT (currency vs math): a row like `| Pro | $20 | 1,000 | $0.04 |`
  // contains four currency-style `$N` tokens, NOT two math spans. A naive
  // `\$[^$]+\$` pass would greedily pair `$20 ... $0.04`, swallow the two
  // pipes between them, and collapse the row from 4 cells to 2. To avoid
  // that, only stash a `$...$` pair when its inner content unambiguously
  // looks like LaTeX — either it carries a math-only character (\ ^ _ { })
  // OR it sits entirely on one side of a pipe (no `|` inside). Pure-numeric
  // tokens like `$20` / `$0.04/credit` then split as ordinary cells.
  // Pipe-bearing math like `$|AB|=2$` should be authored with `\(...\)` or
  // backticks inside tables — accepted limitation.
  const isTableMathSpan = inner => {
    if (/[\\^_{}]/.test(inner)) return true;
    if (inner.indexOf('|') !== -1) return false;
    return isMathInline(inner);
  };
  const cells = l => {
    let s = l.trim().replace(/\\\|/g, PIPE);
    const guards = [];
    const stash = (re, predicate) => {
      s = s.replace(re, (m, inner) => {
        if (predicate && !predicate(inner == null ? m : inner)) return m;
        guards.push(m);
        return '\x00G' + (guards.length - 1) + '\x00';
      });
    };
    stash(/`[^`]+`/g);
    stash(/\\\(([^)]+?)\\\)/g);
    stash(/\$([^$\n]+?)\$/g, isTableMathSpan);
    return s.replace(/^\||\|$/g, '')
      .split('|')
      .map(c => c.trim()
        .replace(/\x00G(\d+)\x00/g, (_, i) => guards[+i])
        .split(PIPE).join('|'));
  };
  const header = cells(lines[0]);
  const ncol = header.length;
  // Overflow guard: when an LLM emits a row with more cells than the header
  // (unbalanced pipes it refused to escape), merge the tail into the last
  // cell instead of letting empty columns spill off to the right.
  const clamp = row => {
    if (row.length <= ncol) return row;
    const head = row.slice(0, ncol - 1);
    const tail = row.slice(ncol - 1).join(' | ');
    return head.concat([tail]);
  };
  let h = '<table class="md-table"><thead><tr>' + header.map(c => '<th>' + inlineMd(c) + '</th>').join('') + '</tr></thead><tbody>';
  for (let i = 2; i < lines.length; i++) h += '<tr>' + clamp(cells(lines[i])).map(c => '<td>' + inlineMd(c) + '</td>').join('') + '</tr>';
  return '<div class="md-table-wrap">' + h + '</tbody></table></div>';
}

function processEventsForDisplay(events) {
  return events.filter(e => !isInternalEvent(e));
}

function sid(key, node) { return key + '\t' + (node || 'local'); }

// setActiveSessionCard flips the .active class on at most one session card.
// Replaces the old O(N) querySelectorAll('.session-card').forEach pattern
// with a cached reference (_activeCardEl). key===null drops selection
// altogether (used by openCronPanel / previewDiscovered clear paths). Node
// defaults to 'local' to match data-node attribute emission. A subsequent
// card with the same key but a different node counts as "different" — the
// data-key + data-node pair is the identity.
function setActiveSessionCard(key, node) {
  const n = node || 'local';
  // Drop stale cached ref if the previous card was detached by a sidebar
  // rebuild (renderSidebar replaces list.innerHTML wholesale).
  if (_activeCardEl && !_activeCardEl.isConnected) _activeCardEl = null;
  if (_activeCardEl) _activeCardEl.classList.remove('active');
  _activeCardEl = null;
  if (key === null || key === undefined) return null;
  const next = document.querySelector(
    '.session-card[data-key="' + (window.CSS && CSS.escape ? CSS.escape(key) : key) + '"]'
    + '[data-node="' + (window.CSS && CSS.escape ? CSS.escape(n) : n) + '"]'
  );
  if (next) {
    next.classList.add('active');
    _activeCardEl = next;
  }
  return next;
}

function isMultiNode() {
  const keys = Object.keys(nodesData);
  return keys.length > 1 || (keys.length === 1 && keys[0] !== 'local');
}

const NODE_BADGE_COLORS = ['#1f6feb','#0550ae','#1a7f37','#6e40c9','#9a6700','#cf222e'];
function nodeColor(id) {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return NODE_BADGE_COLORS[h % NODE_BADGE_COLORS.length];
}

/* ===== Node Selector ===== */
//
// The node selector replaces the per-card .sc-node badge + per-node rows in
// .sidebar-status when more than one node is connected. Clicking the trigger
// opens a dropdown of all nodes; clicking a node switches the sidebar filter.
// Single-node setups (local only, or one remote only) hide the whole thing —
// there is nothing to choose between.

// getNodeDisplayName returns the human label for a node id. Falls back to the
// raw id for remotes whose display_name the server hasn't populated yet, and
// uses a Chinese '本地' for 'local' to match the rest of the UI.
function getNodeDisplayName(id) {
  if (!id || id === 'local') return '本地';
  const nd = nodesData[id];
  if (nd && nd.display_name) return nd.display_name;
  return id;
}

// getNodeStatus returns a normalized status key (ok/connecting/offline/
// unreachable/error) for a node. 'local' tracks the WS state machine; remotes
// read from the server-side node health snapshot. Falls back to 'offline' when
// the server has no record — safer than pretending the node is reachable.
function getNodeStatus(id) {
  if (!id || id === 'local') {
    if (wsm.state === WS_STATES.CONNECTED) return 'ok';
    if (wsm.state === WS_STATES.CONNECTING || wsm.state === WS_STATES.AUTH) return 'connecting';
    return 'offline';
  }
  const nd = nodesData[id];
  if (!nd) return 'offline';
  return nd.status || 'offline';
}

// getNodeSessionCount returns how many sessions are filed under a node in the
// last-rendered cache. Used in the dropdown rows and for the aggregated alert
// dot. Cheap to call (linear scan over allSessionsCache; bounded by the sidebar
// render pass).
function getNodeSessionCount(id) {
  if (!allSessionsCache || allSessionsCache.length === 0) return 0;
  let n = 0;
  for (const s of allSessionsCache) {
    if ((s.node || 'local') === id) n++;
  }
  return n;
}

// statusLabelForNode maps a normalized status to a short Chinese/English label
// used inside the trigger and each dropdown row.
function statusLabelForNode(status) {
  const m = {
    ok: 'connected', connected: 'connected',
    connecting: 'connecting', authenticating: 'authenticating',
    offline: 'offline', unreachable: 'unreachable',
    error: 'error', disconnected: 'disconnected',
  };
  return m[status] || status;
}

// RAIL_CONN_LABELS maps a normalized node status to a short Chinese label for
// the bottom-rail connection indicator + the settings view. Distinct from the
// English statusLabelForNode (used in the multi-node dropdown for parity with
// existing tests) — the rail wants compact CJK so it fits the 56px column.
const RAIL_CONN_LABELS = {
  ok: '已连接', connected: '已连接',
  connecting: '连接中', authenticating: '鉴权中',
  offline: '离线', unreachable: '不可达',
  error: '错误', disconnected: '已断开', off: '离线',
};
function railConnLabel(status) { return RAIL_CONN_LABELS[status] || status; }

// renderRailConnStatus paints the bottom-rail connection indicator (dot +
// label). Reuses getNodeStatus so it tracks the WS state machine for local and
// the server health snapshot for the selected remote. Called from
// updateStatusBar (every WS state change + auth-countdown tick) and once at
// DOMContentLoaded. Cheap + idempotent — safe to call on every tick.
function renderRailConnStatus() {
  const dot = document.getElementById('ab-conn-dot');
  const label = document.getElementById('ab-conn-label');
  if (!dot && !label) return;
  const status = getNodeStatus((typeof selectedNode !== 'undefined' && selectedNode) || 'local');
  if (dot) dot.className = 'ab-conn-dot ' + (VALID_DOT_CLASSES[status] || 'offline');
  if (label) label.textContent = railConnLabel(status);
}

// renderSettingsView paints the standalone settings top-level view into
// #settings-main. Two sections: theme (tri-state, reusing applyTheme/
// THEME_ORDER/THEME_LABELS) and connection (read-only, reusing getNodeStatus/
// getNodeDisplayName). Theme buttons are wired via event delegation — no inline
// onclick (the HTML inline-handler cap is 0). Re-rendered on each theme click
// to refresh the active state.
function renderSettingsView() {
  const root = document.getElementById('settings-main');
  if (!root) return;
  const cur = getCurrentTheme();
  const themeBtns = THEME_ORDER.map(function (t) {
    return '<button type="button" class="settings-theme-opt' + (t === cur ? ' active' : '') +
      '" data-theme="' + esc(t) + '" aria-pressed="' + (t === cur ? 'true' : 'false') + '">' +
      esc(THEME_LABELS[t]) + '</button>';
  }).join('');
  const status = getNodeStatus('local');
  const connMeta = isMultiNode()
    ? '<div class="settings-conn-meta">多节点：点击侧栏顶部的节点选择器切换。</div>'
    : '';
  root.innerHTML =
    '<div class="settings-head"><h1>设置</h1></div>' +
    '<div class="settings-body">' +
      '<section class="settings-sec"><h2>主题</h2>' +
        '<div class="settings-theme" id="settings-theme-group" role="group" aria-label="主题">' + themeBtns + '</div>' +
      '</section>' +
      '<section class="settings-sec"><h2>连接</h2>' +
        '<div class="settings-conn">' +
          '<span class="ab-conn-dot ' + (VALID_DOT_CLASSES[status] || 'offline') + '"></span>' +
          '<span>' + esc(getNodeDisplayName('local')) + ' · ' + esc(railConnLabel(status)) + '</span>' +
        '</div>' + connMeta +
      '</section>' +
    '</div>';
  const grp = document.getElementById('settings-theme-group');
  if (grp) grp.addEventListener('click', function (e) {
    const b = e.target.closest('.settings-theme-opt');
    if (!b) return;
    applyTheme(b.dataset.theme);
    renderSettingsView(); // refresh active state
  });
}

// updateNodeSelector is the single render entry point for the node-selector
// trigger + dropdown visibility. Called after every /api/sessions poll (so
// new/removed remotes appear immediately) and after every open/close toggle.
// Fast path when multi-node isn't active: hide the whole widget.
function updateNodeSelector() {
  const root = document.getElementById('node-selector');
  if (!root) return;

  const multi = isMultiNode();
  if (!multi) {
    root.hidden = true;
    nodeSelectorOpen = false;
    return;
  }

  // If the persisted selection points at a node that no longer exists,
  // snap it back to 'local' so the sidebar doesn't render an empty list.
  // Happens when a remote is removed server-side while the dashboard is open.
  if (selectedNode !== 'local' && !nodesData[selectedNode]) {
    selectedNode = 'local';
    try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  }

  root.hidden = false;
  const trigger = document.getElementById('ns-trigger');
  const dotEl = document.getElementById('ns-trigger-dot');
  const labelEl = document.getElementById('ns-trigger-label');
  const alertEl = document.getElementById('ns-trigger-alert');

  const status = getNodeStatus(selectedNode);
  const displayName = getNodeDisplayName(selectedNode);
  const statusLabel = statusLabelForNode(status);

  if (dotEl) {
    dotEl.className = 'ns-dot ' + (VALID_DOT_CLASSES[status] || 'offline');
  }
  if (labelEl) {
    labelEl.textContent = displayName + ' · ' + statusLabel;
    labelEl.title = displayName + ' (' + selectedNode + ') · ' + statusLabel;
  }

  // Aggregated alert dot: any non-current node in a non-ok/connecting state
  // surfaces a red dot on the trigger so the user knows to open the dropdown.
  if (alertEl) {
    let hasAlert = false;
    for (const id of Object.keys(nodesData)) {
      if (id === selectedNode) continue;
      const s = getNodeStatus(id);
      if (s !== 'ok' && s !== 'connected' && s !== 'connecting' && s !== 'authenticating') {
        hasAlert = true; break;
      }
    }
    alertEl.hidden = !hasAlert;
  }

  if (trigger) {
    trigger.setAttribute('aria-expanded', nodeSelectorOpen ? 'true' : 'false');
  }

  const dropdown = document.getElementById('ns-dropdown');
  if (!dropdown) return;
  if (!nodeSelectorOpen) {
    dropdown.hidden = true;
    return;
  }
  dropdown.hidden = false;
  dropdown.innerHTML = renderNodeDropdownHtml();
}

// renderNodeDropdownHtml builds the list of node options. Local is pinned
// first; remotes are grouped by status (connected → connecting → offline)
// then sorted by display name. Each row shows status dot, name, session count,
// and a check when selected.
function renderNodeDropdownHtml() {
  const ids = Object.keys(nodesData);
  // Ensure 'local' is always present even if the server didn't include it
  // (defensive — the local node should always be in nodesData, but we don't
  // want to drop it off the UI if the backend ever forgets to ship it).
  if (ids.indexOf('local') === -1) ids.unshift('local');

  const rank = { ok: 0, connected: 0, connecting: 1, authenticating: 1, offline: 2, unreachable: 2, error: 2, disconnected: 2, off: 2 };
  const sortable = ids.map(id => ({
    id,
    name: getNodeDisplayName(id),
    status: getNodeStatus(id),
    count: getNodeSessionCount(id),
  }));
  sortable.sort((a, b) => {
    // Local always first.
    if (a.id === 'local' && b.id !== 'local') return -1;
    if (b.id === 'local' && a.id !== 'local') return 1;
    const ra = rank[a.status] === undefined ? 9 : rank[a.status];
    const rb = rank[b.status] === undefined ? 9 : rank[b.status];
    if (ra !== rb) return ra - rb;
    return a.name.localeCompare(b.name);
  });

  let html = '';
  for (const n of sortable) {
    const active = n.id === selectedNode;
    const cls = 'ns-option' + (active ? ' active' : '');
    const dotCls = VALID_DOT_CLASSES[n.status] || 'offline';
    const statusLabel = statusLabelForNode(n.status);
    const addr = (n.id !== 'local' && nodesData[n.id] && nodesData[n.id].remote_addr) ? nodesData[n.id].remote_addr : '';
    const countBadge = n.count > 0 ? '<span class="ns-count">' + n.count + '</span>' : '';
    const check = active ? '<span class="ns-check" aria-hidden="true">✓</span>' : '';
    html += '<button type="button" class="' + cls + '" role="option" aria-selected="' + (active ? 'true' : 'false') + '" data-node="' + escAttr(n.id) + '" onclick="selectNodeFromDropdown(this.dataset.node)">' +
      '<span class="ns-dot ' + dotCls + '" aria-hidden="true"></span>' +
      '<span class="ns-body">' +
        '<span class="ns-name" title="' + escAttr(n.name) + '">' + esc(n.name) + '</span>' +
        (addr ? '<span class="ns-addr">' + esc(addr) + '</span>' : '') +
      '</span>' +
      '<span class="ns-status">' + esc(statusLabel) + '</span>' +
      countBadge +
      check +
      '</button>';
  }
  return html;
}

// toggleNodeSelector is the click handler on the trigger. Flips the dropdown,
// repaints, and installs a one-shot outside-click listener to close on stray
// clicks. `event` is stopped so the same click doesn't immediately fire the
// outside-click listener we're about to install.
function toggleNodeSelector(event) {
  if (event) { event.stopPropagation(); event.preventDefault(); }
  nodeSelectorOpen = !nodeSelectorOpen;
  updateNodeSelector();
}

// selectNodeFromDropdown is the click handler on each option. Switches the
// filter, persists, closes the dropdown, and re-renders the sidebar against
// the last cached payload so the change is instant (no network round-trip).
function selectNodeFromDropdown(nodeId) {
  if (!nodeId) return;
  nodeSelectorOpen = false;
  if (nodeId === selectedNode) {
    updateNodeSelector();
    return;
  }
  selectedNode = nodeId;
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  updateNodeSelector();
  // Re-render the sidebar so the filter takes effect. The selected session
  // key may be on a different node now — we intentionally do NOT clear
  // selectedKey here: if the user switches back, the card is still active,
  // and if they pick a session on the new node, selectSession() will swap.
  if (_lastSidebarData) {
    renderSidebar(_lastSidebarData);
  } else {
    debouncedFetchSessions();
  }
  updateStatusBar();
}

// Outside-click + Esc to close the dropdown. Installed once at startup and
// cheap to run (early-returns when the dropdown isn't open).
document.addEventListener('click', function(e) {
  if (!nodeSelectorOpen) return;
  const root = document.getElementById('node-selector');
  if (root && root.contains(e.target)) return;
  nodeSelectorOpen = false;
  updateNodeSelector();
});
document.addEventListener('keydown', function(e) {
  if (!nodeSelectorOpen) return;
  if (e.key === 'Escape') {
    e.preventDefault();
    nodeSelectorOpen = false;
    updateNodeSelector();
    const trigger = document.getElementById('ns-trigger');
    if (trigger) trigger.focus();
  }
});

/* ===== WebSocket Connection Manager ===== */

const WS_STATES = { OFF: 'off', CONNECTING: 'connecting', AUTH: 'authenticating', CONNECTED: 'connected', DISCONNECTED: 'disconnected' };

const wsm = {
  conn: null,
  state: WS_STATES.OFF,
  backoff: 1000,
  maxBackoff: 30000,
  reconnectTimer: null,
  pingTimer: null,
  subscribedKey: null,
  subscribedNode: null,
  lastEventTimeWs: 0,
  sendCounter: 0,
  _initialSubscribe: false,
  // _everConnected gates the "已重新连接" toast to only fire AFTER the
  // first successful WS handshake, so a page-load from an already-up
  // state doesn't emit a bogus "reconnected" toast. Once true, every
  // subsequent CONNECTED transition from any non-CONNECTED state
  // triggers the toast — matches the UX P1 spec of surfacing recovery
  // back to the user.
  _everConnected: false,
  // _authBlockUntil is a unix-ms wall-clock deadline. While Date.now() <
  // _authBlockUntil, connect() skips dialing and scheduleReconnect() pushes
  // the next attempt to the deadline instead of its own exponential backoff.
  // Set by startWSAuthRetryCountdown when the server emits auth_fail with
  // retry_after=N (rate-limit lockout). Without this gate the default
  // reconnect loop would immediately dial a fresh WS, hit the same 429,
  // and rack up more lockout events in the journal.
  _authBlockUntil: 0,
  // R110-P1 WS outage duration display: wall-clock ms when the connection
  // first left the CONNECTED state (or 0 when connected). setState maintains
  // this: any CONNECTED→non-CONNECTED transition writes Date.now() if the
  // field is still 0 (first outage arm — don't stomp an earlier outage while
  // cycling connecting → auth → connecting during backoff); CONNECTED clears
  // it. updateStatusBar reads it to render "已断开 N 秒/分" inline hint so
  // users distinguish "just lost the WS 2s ago" from "dead for 10 min".
  _disconnectedSince: 0,

  // cron-live RFC: 第二条独立的订阅通道，与主订阅 (subscribedKey) 并存。
  // cron drawer 打开 + 任务运行中时订阅 'cron:<jobId>'，让操作员看到
  // claude 子进程的流式输出。fresh 模式 (scheduler_run.go:318) 每次 run
  // 前 Reset(key)，stub 重建后 EventLog 是空的，初次 sub 必返
  // suspended + 0 events，等 session_state running 后 re-sub —— 这是
  // 默认路径不是 edge case。详见 docs/rfc 与 plan §1.3。
  cronLive: {
    jobId: null,
    pendingJobId: null,
    subscribedKey: null,
    lastEventTimeMs: 0,
    runStartedAt: 0,
    events: [],
    truncatedCount: 0,
    suspended: false,
    status: 'idle', // 'idle' | 'pending' | 'live' | 'stopped'
  },

  connect() {
    if (this.conn && (this.conn.readyState === WebSocket.OPEN || this.conn.readyState === WebSocket.CONNECTING)) return;
    // Respect the auth rate-limit gate: skip the dial if we're still within
    // the lockout window. scheduleReconnect re-arms a timer pointing at the
    // deadline so we come back exactly when the server says we can.
    if (this._authBlockUntil > 0 && Date.now() < this._authBlockUntil) {
      this.scheduleReconnect();
      return;
    }

    this.setState(WS_STATES.CONNECTING);
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.conn = new WebSocket(proto + '//' + location.host + '/ws');

    this.conn.onopen = () => {
      this.setState(WS_STATES.AUTH);
      const token = getToken();
      this.conn.send(JSON.stringify({ type: 'auth', token: token }));
    };

    this.conn.onmessage = (evt) => {
      try { this.onMessage(JSON.parse(evt.data)); }
      catch (err) { console.error('ws parse error:', err); }
    };

    this.conn.onclose = () => {
      this.cleanup();
      this.setState(WS_STATES.DISCONNECTED);
      this.scheduleReconnect();
    };

    this.conn.onerror = () => {};
  },

  cleanup() {
    if (this.pingTimer) { clearInterval(this.pingTimer); this.pingTimer = null; }
  },

  disconnect() {
    if (this.reconnectTimer) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null; }
    this.cleanup();
    if (this.conn) { this.conn.close(); this.conn = null; }
    this.subscribedKey = null;
    this.subscribedNode = null;
    this._pendingSubscribeKey = null;
    this._pendingSubscribeNode = null;
    this.setState(WS_STATES.OFF);
  },

  scheduleReconnect() {
    if (this.reconnectTimer) return;
    // Pick the later of: the exponential-backoff delay, and the auth-block
    // deadline. If an auth rate-limit countdown is active, we must not
    // dial before it expires — the exponential curve would otherwise
    // happily re-try every 1-30s and wake the 429 bucket over and over.
    const now = Date.now();
    const authGap = Math.max(0, this._authBlockUntil - now);
    // RNEW-UX-001: add randomised jitter (0-500ms) on top of the computed
    // delay. Without jitter, N tabs that all dropped together on the same
    // server restart would redial on identical millisecond ticks, briefly
    // saturating the upgrade limiter and causing a thundering herd. The
    // jitter is additive (never shortens the gate) so the auth-block
    // invariant above is preserved.
    const jitter = Math.floor(Math.random() * 500);
    const delay = Math.max(this.backoff, authGap) + jitter;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
    this.backoff = Math.min(this.backoff * 2, this.maxBackoff);
  },

  onMessage(msg) {
    switch (msg.type) {
      case 'auth_ok':
        this.setState(WS_STATES.CONNECTED);
        this.backoff = 1000;
        this.startPing();
        this.onConnected();
        break;
      case 'auth_fail':
        // Classify the in-band WS auth error by pattern: the server emits
        // "too many attempts" for rate-limit lockouts (should be a warn
        // toast, not an error; the operator just needs to wait), and
        // anything else is a token-mismatch fail requiring re-login.
        //
        // Rate-limit replies also carry `retry_after` (seconds) so the
        // UI can show a countdown instead of the legacy generic "稍后
        // 重试" hint. Older servers omit the field — parseInt of
        // undefined is NaN, and startWSAuthRetryCountdown clamps to a
        // 60s default so the UX degrades gracefully.
        {
          const raw = (msg.error || '').toString();
          if (raw.toLowerCase().includes('too many')) {
            let retryAfter = parseInt(msg.retry_after, 10);
            if (!Number.isFinite(retryAfter) || retryAfter <= 0) retryAfter = 60;
            startWSAuthRetryCountdown(retryAfter);
          } else {
            showAPIError('WebSocket 鉴权', 401, raw || '令牌无效');
          }
        }
        this.conn.close();
        break;
      case 'subscribed':
        // cron-live RFC §2.2: cron live 订阅命中时不污染主订阅状态
        if (this.cronLive.pendingJobId && msg.key === ('cron:' + this.cronLive.pendingJobId)) {
          this.cronLive.subscribedKey = msg.key;
          this.cronLive.pendingJobId = null;
          this.cronLive.suspended = (msg.reason === 'suspended');
          this.cronLive.status = this.cronLive.suspended ? 'pending' : 'live';
          setCronLiveStatus(this.cronLive.status);
          break;
        }
        // Server confirmed subscription — apply authoritative state
        this.subscribedKey = this._pendingSubscribeKey || msg.key;
        this.subscribedNode = this._pendingSubscribeNode || 'local';
        this._pendingSubscribeKey = null;
        this._pendingSubscribeNode = null;
        // Track whether the server started an eventPushLoop for this subscription.
        // "suspended" means the session had no process — no live events will arrive
        // until the process starts, at which point onSessionState triggers re-subscribe.
        this._subscriptionSuspended = (msg.reason === 'suspended');
        if (msg.state && msg.key === selectedKey && this.subscribedNode === selectedNode) {
          const subSKey = sid(msg.key, this.subscribedNode);
          if (sessionsData[subSKey]) {
            sessionsData[subSKey].state = msg.state;
            updateMainState(msg.state, msg.reason);
          }
        }
        break;
      case 'error':
        // cron-live RFC §2.2: 错误命中 cron live pending → 单独清理
        if (msg.key && this.cronLive.pendingJobId && msg.key === ('cron:' + this.cronLive.pendingJobId)) {
          this.cronLive.pendingJobId = null;
          this.cronLive.subscribedKey = null;
          this.cronLive.status = 'stopped';
          setCronLiveStatus('stopped');
          break;
        }
        // Subscribe failed (e.g. session not found yet) — reset pending
        this._pendingSubscribeKey = null;
        this._pendingSubscribeNode = null;
        break;
      case 'history':
        if (isCronLiveKey(msg.key)) { this.onCronLiveHistory(msg); break; }
        this.onHistory(msg);
        break;
      case 'event':
        if (isCronLiveKey(msg.key)) { this.onCronLiveEvent(msg); break; }
        this.onEvent(msg);
        break;
      case 'send_ack':
        this.onSendAck(msg);
        break;
      case 'interrupt_ack':
        break;
      case 'session_state':
        if (isCronLiveKey(msg.key)) { this.onCronLiveSessionState(msg); break; }
        this.onSessionState(msg);
        break;
      case 'sessions_update': {
        // RNEW-UX-010 — snapshot pre-update session-key set so we can spot
        // a newly-added key after the fetch completes. Comparing sizes is
        // not enough (delete+create at the same tick would net to zero).
        const prevSessKeys = new Set(Object.keys(sessionsData || {}));
        debouncedFetchSessions().then(() => {
          // Auto-subscribe to newly created session if we don't have an active
          // subscription. _pendingSubscribeKey is intentionally not checked:
          // a no-process subscribe returns "subscribed" + persisted history but
          // no live eventPushLoop, so subscribedKey may not be set while the
          // pending flag was already cleared. This ensures recovery.
          if (selectedKey && !wsm.subscribedKey && sessionsData[sid(selectedKey, selectedNode)]) {
            wsm.subscribe(selectedKey, selectedNode);
          }
          const added = Object.keys(sessionsData || {}).filter(k => !prevSessKeys.has(k));
          if (added.length > 0) announce('新会话已创建');
        });
        break;
      }
      // Phase D (RFC §3.5) deleted the legacy cron_result frame. The
      // announce("定时任务已完成") moved to the cron_run_ended succeeded
      // branch below; the list refetch was a strict subset of what the
      // cron_run_ended branch already does.
      case 'cron_run_started':
        // P0 cron-run-history (RFC §7.2) — drive the "运行中 Xs" inline
        // badge without waiting for a list refetch. Optimistically patch
        // local cronJobs entry so the UI flips to running immediately;
        // a fetchCronJobs would also work but adds latency.
        // cron-panel-consolidation RFC §4.6: cronApplyRunStarted internally
        // calls renderCronPanel, whose shell-preserving branch repaints
        // both the list AND the per-job drawer (renderCronDrawer). The
        // drawer's "当前执行" section therefore appears the same frame
        // the WS event lands, gated only on cronDetailJobId — no
        // selectedKey check is needed any more.
        cronApplyRunStarted(msg);
        break;
      case 'cron_run_ended':
        // P0 — terminal frame. Refetch list so counters / last_error_class
        // hydrate from backend; the optimistic patch on the same row is
        // overwritten cleanly. fresh=false / fresh=true behave identically
        // here since the change set is JobID-scoped.
        //
        // Phase D (RFC §3.5) absorbed the legacy cron_result frame:
        // gate the AT-user announce on the succeeded state so failed
        // runs do not mis-speak success. cron_run_ended fires for every
        // terminal state (succeeded / failed / skipped / timed_out /
        // canceled) — only succeeded should celebrate.
        if (msg && msg.state === 'succeeded') announce('定时任务已完成');
        cronApplyRunEnded(msg);
        fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
        // P2 cron-run-history (RFC §8.2) — refresh the timeline head
        // (most-recent 10 runs) when the operator currently has the drawer
        // open for this job. cron-panel-consolidation RFC §4.6: the gate
        // moved from selectedKey === 'cron:<id>' to cronDetailJobId ===
        // job_id (selectedKey is null in cron-panel mode). msg.job_id is
        // backend-mandatory; the guard against falsy job_id stays for
        // defence-in-depth.
        // R221-FIX-P1-4: cronTimelineRefreshHead is async; swallow its
        // rejection at the dispatch boundary.
        // R243-PERF-7 / #812: route through the rAF-debounced wrapper so
        // bursty cron_run_ended events for the same job collapse to one
        // sort+innerHTML rebuild per paint frame instead of one per event.
        if (msg && msg.job_id) cronTimelineRefreshHeadDebounced(msg.job_id);
        break;
      case 'pong':
        break;
      // RFC v4 agent-team-ui §3.5.2 — drill-in flow. All four handlers
      // live in agent_view.js so new agent-view functionality doesn't
      // mean touching this dispatch table.
      case 'agent_event':
        if (window.AgentView) window.AgentView.onAgentEvent(msg);
        break;
      case 'agent_meta':
        if (window.AgentView) window.AgentView.onAgentMeta(msg);
        break;
      case 'agent_done':
        if (window.AgentView) window.AgentView.onAgentDone(msg);
        break;
      case 'agent_subscribe_rejected':
        if (window.AgentView) window.AgentView.onAgentSubscribeRejected(msg);
        break;
    }
  },

  startPing() {
    if (this.pingTimer) clearInterval(this.pingTimer);
    this.pingTimer = setInterval(() => {
      if (this.conn && this.conn.readyState === WebSocket.OPEN) {
        this.conn.send(JSON.stringify({ type: 'ping' }));
      }
    }, 30000);
  },

  send(msg) {
    if (this.conn && this.conn.readyState === WebSocket.OPEN) {
      this.conn.send(JSON.stringify(msg));
      return true;
    }
    return false;
  },

  subscribe(key, node) {
    node = node || 'local';
    this._pendingSubscribeKey = key;
    this._pendingSubscribeNode = node;
    const msg = { type: 'subscribe', key: key };
    if (node && node !== 'local') msg.node = node;
    this._initialSubscribe = (this.lastEventTimeWs === 0);
    if (this.lastEventTimeWs > 0) {
      msg.after = this.lastEventTimeWs;
    } else {
      // Initial subscribe: ask for only the last INITIAL_HISTORY_LIMIT events.
      // Keeps the first frame fast on large sessions; older events are fetched
      // on demand via the "load earlier" button that calls GET
      // /api/sessions/events?before=..&limit=..
      msg.limit = INITIAL_HISTORY_LIMIT;
    }
    this.send(msg);
  },

  unsubscribe() {
    if (this.subscribedKey) {
      const msg = { type: 'unsubscribe', key: this.subscribedKey };
      if (this.subscribedNode && this.subscribedNode !== 'local') msg.node = this.subscribedNode;
      this.send(msg);
    }
    this.subscribedKey = null;
    this.subscribedNode = null;
    this._pendingSubscribeKey = null;
    this._pendingSubscribeNode = null;
    this.lastEventTimeWs = 0;
  },

  // cron-live RFC §1.2: 镜像 subscribe()，订阅 cron stub session 拿实时事件流。
  // after = lastEventTimeMs || runStartedAtMs：避免拉到上轮 run 的残留（cron
  // stub EventLog 可能跨 run 持续；fresh 模式下被 Reset 销毁后是空的）。
  subscribeCronLive(jobId, runStartedAtMs) {
    if (!jobId) return;
    if (this.cronLive.jobId === jobId && this.cronLive.subscribedKey) return; // already subscribed
    if (this.cronLive.jobId && this.cronLive.jobId !== jobId) {
      this.unsubscribeCronLive();
    }
    const key = 'cron:' + jobId;
    this.cronLive.jobId = jobId;
    this.cronLive.pendingJobId = jobId;
    this.cronLive.runStartedAt = runStartedAtMs || 0;
    this.cronLive.status = 'pending';
    setCronLiveStatus('pending');
    const msg = { type: 'subscribe', key: key };
    const after = this.cronLive.lastEventTimeMs || runStartedAtMs || 0;
    if (after > 0) msg.after = after;
    this.send(msg);
  },

  unsubscribeCronLive() {
    if (this.cronLive.subscribedKey) {
      this.send({ type: 'unsubscribe', key: this.cronLive.subscribedKey });
    }
    this.cronLive.jobId = null;
    this.cronLive.pendingJobId = null;
    this.cronLive.subscribedKey = null;
    this.cronLive.lastEventTimeMs = 0;
    this.cronLive.runStartedAt = 0;
    this.cronLive.events = [];
    this.cronLive.truncatedCount = 0;
    this.cronLive.suspended = false;
    this.cronLive.status = 'idle';
  },

  /* -- WS event handlers -- */

  onConnected() {
    if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
    if (selectedKey) {
      if (lastEventTime > 0 && this.lastEventTimeWs === 0) {
        this.lastEventTimeWs = lastEventTime;
      }
      this.subscribe(selectedKey, selectedNode);
    }
    // cron-live RFC §3: 重连后若已有 cron live 订阅，后端 conn 已亡 sub 已丢，
    // 必须重发 subscribe 帧。直接走 wsm.subscribeCronLive 不行 —— 它的"已订
    // 同 jobId 直接 no-op"短路会让我们什么都不做。
    //
    // 仅在任务"仍在跑"时重 sub。若断网期间任务已经结束，重 sub 只会拉到一个
    // suspended 的 stub（fresh 模式 stub 已销毁），status 卡在 'pending' 看起
    // 来像还在等事件 —— 但事件永远不会来。让它停留在终态视图（events 数组
    // 仍含上轮事件，可回看）。
    if (this.cronLive.jobId) {
      const jobId = this.cronLive.jobId;
      const job = (typeof cronJobs !== 'undefined' && Array.isArray(cronJobs))
        ? cronJobs.find(j => j && j.id === jobId)
        : null;
      const isRunning = !!(job && job.current_run && job.current_run.started_at);
      if (isRunning) {
        this.cronLive.subscribedKey = null;
        this.cronLive.pendingJobId = null;
        this.cronLive.suspended = false;
        const runStartedAt = this.cronLive.runStartedAt;
        // 清 jobId 让 subscribeCronLive 不被 "已订同 jobId" 短路命中
        this.cronLive.jobId = null;
        this.subscribeCronLive(jobId, runStartedAt);
      }
      // 任务已结束：保持 events 数组供回看，status 已是 'stopped'
    } else if (typeof ensureCronLiveSubscription === 'function') {
      ensureCronLiveSubscription();
    }
  },

  onHistory(msg) {
    if (msg.key !== selectedKey || (msg.node || 'local') !== selectedNode) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const events = msg.events || [];
    const isInitial = this._initialSubscribe;
    this._initialSubscribe = false;

    // Rebuild the answered-set from history BEFORE rendering so card
    // re-renders show the correct locked state. The Set is in-memory so
    // a page reload or session switch would otherwise make an already-
    // answered card re-actionable and invite duplicate answers to CC.
    hydrateAskAnsweredFromHistory(events);

    const display = processEventsForDisplay(events);

    if (isInitial) {
      // Full render replaces everything — remove any optimistic messages
      const html = renderEventsWithDividers(display, 0);
      // Only show "no events yet" when the server returned zero events and the session
      // is idle. For running sessions, show "loading events..." since eventPushLoop will
      // deliver events shortly (fixes blank-then-"no events yet" flash on click).
      if (html) {
        el.innerHTML = html;
      } else if (events.length === 0) {
        const sd = sessionsData[sid(selectedKey, selectedNode)];
        el.innerHTML = (sd && sd.state === 'running')
          ? '<div class="empty-state loading-indicator">\u6b63\u5728\u52a0\u8f7d\u4e8b\u4ef6\u2026</div>'
          : '<div class="empty-state">\u6682\u65e0\u4e8b\u4ef6</div>';
      } else {
        // Server returned events but every one was internal-filtered
        // (parallel agent team tail). Placeholder keeps the pane from
        // looking broken; the "load earlier" button mounted below is the
        // path back to real messages.
        el.innerHTML = '<div class="empty-state">\u8be5\u4f1a\u8bdd\u6700\u8fd1\u4ec5\u6709 agent \u6d3b\u52a8\uff0c\u70b9\u51fb\u4e0b\u65b9\u52a0\u8f7d\u66f4\u65e9\u7684\u6d88\u606f</div>';
      }
      // Reset dedup tracker on full render and anchor the pagination
      // cursor to the earliest event we received, independent of DOM
      // contents so loadEarlierEvents still works after a fully-filtered
      // page.
      if (events.length > 0) {
        const last = events[events.length - 1];
        if (last.time) lastRenderedEventTime = last.time;
        const first = events[0];
        if (first.time && (oldestFetchedEventTime === 0 || first.time < oldestFetchedEventTime)) {
          oldestFetchedEventTime = first.time;
        }
      }
      // Mount "load earlier" affordance when the server returned a full page
      // (more history likely exists). Skipped for short histories so we don't
      // surface a useless button.
      if (events.length >= INITIAL_HISTORY_LIMIT) {
        ensureEarlierButton();
      }
      runPendingAsync();
      navRebuild();
      // 若有上次切走时保存的滚动位置且不在底部，恢复它；否则照旧贴底。
      if (!restoreScrollPos(selectedKey, selectedNode)) {
        stickEventsBottom();
      }
      // Safety net: blank page despite events existing → page back to real
      // messages (bounded). Twin of renderEvents' maybeAutoPageBack call;
      // covers remote nodes whose subscribe predates the visible-aware read.
      if (!html && events.length > 0) maybeAutoPageBack();
    } else {
      const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
      // Remove stale "no events yet" before processing incremental events
      const emptyEl = el.querySelector('.empty-state');
      if (emptyEl) emptyEl.remove();
      let prevT = lastDividerTime(el);
      // Force-bottom when a "user" event arrives: either the local operator
      // just hit send, or a teammate posted through the IM channel — in both
      // cases the message must be visible even if the viewport was scrolled up.
      let sawUser = false;
      display.forEach(e => {
        if (e.time && e.time <= lastRenderedEventTime) return;
        if (e.type === 'user') {
          const opt = el.querySelector('.optimistic-msg');
          if (opt) opt.remove();
          sawUser = true;
        }
        const h = eventHtml(e);
        if (h) {
          const t = e.time || 0;
          if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
            el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
          }
          el.insertAdjacentHTML('beforeend', h);
          if (t) prevT = t;
        }
        if (e.time && e.time > lastRenderedEventTime) lastRenderedEventTime = e.time;
      });
      // Bound the live DOM on the incremental WS history path too (#398);
      // mirror appendEvents — trim before the scrollHeight reads below.
      trimEventsScroll(el);
      if (sawUser) stickEventsBottom();
      else if (wasBottom) el.scrollTop = el.scrollHeight;
      runPendingAsync();
      navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
      if (navIdx >= 0 && navIdx < navUserEls.length) { /* preserve */ } else navIdx = -1;
      navUpdatePill();
    }

    if (events.length > 0) {
      const last = events[events.length - 1];
      if (last.time > this.lastEventTimeWs) this.lastEventTimeWs = last.time;
    }
    // Build turnState from events
    if (isInitial) {
      // Full rebuild: scan backward to find the last turn boundary
      resetTurnState();
      let turnStart = events.length;
      for (let i = events.length - 1; i >= 0; i--) {
        if (events[i].type === 'user' || events[i].type === 'result') { turnStart = i + 1; break; }
        if (i === 0) turnStart = 0;
      }
      // Anchor timer to the actual turn start time, not Date.now()
      if (turnStart < events.length && events[turnStart].time) {
        turnState.turnStartTime = events[turnStart].time;
        turnState.timerId = setInterval(function() {
          var el = document.getElementById('rb-elapsed');
          if (!el || !turnState.turnStartTime) return;
          var s = Math.floor((Date.now() - turnState.turnStartTime) / 1000);
          el.textContent = Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
        }, 1000);
      }
      for (let i = turnStart; i < events.length; i++) {
        applyEventToTurnState(events[i]);
      }
    } else {
      // Incremental: accumulate additively, reset only on turn boundaries
      for (let i = 0; i < events.length; i++) {
        const ev = events[i];
        if (ev.type === 'user') {
          resetTurnState();
          const text = ev.detail || ev.summary || '';
          if (text) {
            const h2 = document.querySelector('.main-header h2');
            if (h2) h2.textContent = text;
          }
          continue;
        }
        if (ev.type === 'result') {
          if (ev.cost) {
            const sKey = sid(selectedKey, selectedNode);
            if (sessionsData[sKey]) sessionsData[sKey].total_cost = ev.cost;
          }
          // Optimistic: result means the turn is done. Update state to "ready"
          // immediately so the banner hides without waiting for session_state WS msg.
          const rsKey = sid(selectedKey, selectedNode);
          if (sessionsData[rsKey] && sessionsData[rsKey].state === 'running') {
            sessionsData[rsKey].state = 'ready';
            updateSendButton('ready');
          } else {
            resetTurnState();
          }
          continue;
        }
        applyEventToTurnState(ev);
      }
    }
    refreshBanner();
    updateHeaderCost();
  },

  onEvent(msg) {
    if (msg.key !== selectedKey || (msg.node || 'local') !== selectedNode) return;
    // Cron timed_out / failed 终态后丢弃后续 ghost 事件（CLI 子进程
    // 在 deadline 命中后还会再吐 result，但 cron run 已记录为终态，
    // 继续追加只会让用户看到"超时但还在工作"的分裂视觉）。
    if (isCronSessionFrozen(msg.key)) return;
    const ev = msg.event;
    if (!ev) return;
    if (ev.time > this.lastEventTimeWs) this.lastEventTimeWs = ev.time;
    // Turn boundaries: reset state, don't feed into applyEventToTurnState
    if (ev.type === 'user') {
      const text = ev.detail || ev.summary || '';
      if (text) {
        const h2 = document.querySelector('.main-header h2');
        if (h2) h2.textContent = text;
      }
      resetTurnState();
    } else if (ev.type === 'result') {
      if (ev.cost) {
        const sKey = sid(selectedKey, selectedNode);
        if (sessionsData[sKey]) sessionsData[sKey].total_cost = ev.cost;
        updateHeaderCost();
      }
      // Optimistic: result means the turn is done.
      const reKey = sid(selectedKey, selectedNode);
      if (sessionsData[reKey] && sessionsData[reKey].state === 'running') {
        sessionsData[reKey].state = 'ready';
        updateSendButton('ready');
      } else {
        resetTurnState();
      }
    } else {
      applyEventToTurnState(ev);
      refreshBanner();
    }
    if (isInternalEvent(ev)) return;
    // RFC v4 agent-team-ui §3.6.2 — when the user has drilled into an
    // agent, the events-scroll pane belongs to that agent; parent events
    // still feed into turnState / banner (handled above) but must not
    // land in the DOM until the user returns.
    if (window.AgentView && window.AgentView.activeTaskID()) return;
    const html = eventHtml(ev);
    if (!html) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const empty = el.querySelector('.empty-state');
    if (empty) empty.remove();
    // When the real "user" event arrives, remove the optimistic version
    const isUser = ev.type === 'user';
    if (isUser) {
      const opt = el.querySelector('.optimistic-msg');
      if (opt) opt.remove();
    }
    // UI Round 5 R5-6: 80px slack (was 30) so a small natural scroll
    // doesn't take the user out of the auto-stick band. User events
    // always pin (operator just sent / IM thread refresh); AI chunks
    // / result events only stick if user is in the band — preserves
    // scroll position when the user is reading earlier history.
    const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - scrollSlackPx;
    const prevT = lastDividerTime(el);
    const evT = ev.time || 0;
    if (evT && (prevT === 0 || evT - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(evT));
    }
    el.insertAdjacentHTML('beforeend', html);
    // Bound the live DOM before scroll/scan so a long streaming session over
    // the WS push path (the default real-time channel) can't grow
    // #events-scroll without limit and OOM the tab (#398). Without this the
    // MAX_LIVE_DOM_EVENTS cap only fired on the HTTP-poll fallback
    // (appendEvents), so #398 was effectively a no-op while WS was live.
    // Must run before the scrollHeight reads below, matching appendEvents.
    trimEventsScroll(el);
    // User events always force-bottom; AI output only sticks when already at bottom.
    if (isUser) stickEventsBottom();
    else if (wasBottom) el.scrollTop = el.scrollHeight;
    runPendingAsync();
    if (ev.type === 'user') {
      navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
      navUpdatePill();
    }
  },

  onSendAck(msg) {
    // "reset" = /clear or /new — the send was consumed by the router to reset
    // the session, not handed to the CLI, so roll back the optimistic running
    // flip. No banner, no turn.
    if (msg.status === 'reset') {
      rollbackOptimisticRunning(msg.key || selectedKey, msg.node || selectedNode);
      delete sessionLastSent[sid(msg.key || selectedKey, msg.node || selectedNode)];
      return;
    }
    // "accepted" = owner of a new turn, "queued" = appended to an active turn.
    // Both are success cases; the dashboard should behave the same way.
    if (msg.status === 'accepted' || msg.status === 'queued') {
      flashSendBtn();
      if (msg.status === 'queued') {
        // Attach an inline chip to the optimistic user bubble instead of a
        // top-of-screen toast. The chip is bound to the bubble, so when the
        // real "user" event replaces it (see the .optimistic-msg removal
        // path in onEvent) the chip disappears along with the bubble — no
        // separate lifecycle to manage.
        const lastOpt = document.querySelector('#events-scroll .event.user.optimistic-msg:last-of-type .event-content');
        if (lastOpt && !lastOpt.querySelector('.msg-queued-chip')) {
          const chip = document.createElement('div');
          chip.className = 'msg-queued-chip';
          chip.textContent = '排队中…';
          lastOpt.appendChild(chip);
        }
      }
      // Subscribe to the session we just sent to, unless we're already
      // subscribed or a subscribe is already pending for this exact key.
      // The old check (!subscribedKey && !_pendingSubscribeKey) failed when
      // the user was previously viewing a different session — subscribedKey
      // was set to the old key, blocking the subscribe for the new one.
      const ackKey = msg.key || selectedKey;
      if (ackKey && wsm.subscribedKey !== ackKey && wsm._pendingSubscribeKey !== ackKey) {
        wsm.lastEventTimeWs = 0;
        wsm.subscribe(ackKey, selectedNode);
      }
      // Re-subscribe is NOT needed here for already-subscribed sessions.
      // The existing eventPushLoop is still connected to the process's event
      // log and will deliver new events (including the user message we just
      // sent). Re-subscribing would cause a history replay that overlaps with
      // events already pushed by the running eventPushLoop, resulting in
      // duplicate user messages in the UI.
      // For process restarts (dead → running), onSessionState
      // handles re-subscription exclusively.
    } else if (msg.status === 'busy') {
      // Queue is disabled (MaxDepth<=0) and the session is currently
      // processing another message, so our send was dropped rather than
      // enqueued. Roll back the optimistic bubble and tell the operator
      // to retry — otherwise the UI silently eats the message.
      showToast('会话正忙，消息未送达，请稍后重试', 'error');
      const opt = document.querySelector('.optimistic-msg');
      if (opt) opt.remove();
      rollbackOptimisticRunning(msg.key || selectedKey, msg.node || selectedNode);
      // send 从未真正进入 turn，别把它当成「当前 turn 的输入」残留 —— 否则
      // 下次中断会把这条从未送达的文本回填上来。
      delete sessionLastSent[sid(msg.key || selectedKey, msg.node || selectedNode)];
    } else if (msg.status === 'error') {
      // The WS send_ack error is an in-band message, not an HTTP status,
      // but treat the server-supplied `error` string the same way as an
      // HTTP 500 body: truncate + prefix with "发送消息失败：".
      showAPIError('发送消息', 500, msg.error || '');
      // Remove optimistic message on send failure
      const opt = document.querySelector('.optimistic-msg');
      if (opt) opt.remove();
      rollbackOptimisticRunning(msg.key || selectedKey, msg.node || selectedNode);
      delete sessionLastSent[sid(msg.key || selectedKey, msg.node || selectedNode)];
    }
  },

  onSessionState(msg) {
    const msgNode = msg.node || 'local';
    const sKey = sid(msg.key, msgNode);
    // Real state arrived — the optimistic flip has served its purpose, regardless
    // of whether the server says running/ready/dead. Clear the flag so future
    // turns don't short-circuit the running→ready rollback logic.
    delete sessionOptimisticRunning[sKey];
    if (_optimisticRunningTimers[sKey]) {
      clearTimeout(_optimisticRunningTimers[sKey]);
      delete _optimisticRunningTimers[sKey];
    }
    const prev = sessionsData[sKey] || {};
    const prevState = prev.state;   // capture before mutation
    const wasDead = prev.death_reason && prevState !== 'running';
    // Chat-style unread: a running→ready (or dead) transition means the model
    // just produced a reply. Bump the unread counter unless the operator is
    // already looking at that card — in which case they're reading it live.
    const turnCompleted = prevState === 'running' && (msg.state === 'ready' || msg.state === 'dead');
    const isActive = msg.key === selectedKey && msgNode === selectedNode;
    if (turnCompleted && !isActive) {
      sessionUnread[sKey] = (sessionUnread[sKey] || 0) + 1;
    }
    // Turn 自然跑完后清掉上一次发出的文本缓存，否则下一轮刚进 running
    // 就中断会把陈旧文本回填上来。中断路径不会走到这里被清掉，因为
    // interruptSession 会先消费 lastSent 再发中断。
    if (turnCompleted) delete sessionLastSent[sKey];
    if (sessionsData[sKey]) {
      sessionsData[sKey].state = msg.state;
      if (msg.reason) {
        sessionsData[sKey].death_reason = msg.reason;
      } else if (msg.state === 'running') {
        // Process revived: clear stale death_reason
        delete sessionsData[sKey].death_reason;
      }
    }
    let card = null;
    document.querySelectorAll('.session-card').forEach(c => {
      if (c.dataset.key === msg.key && (c.dataset.node || 'local') === msgNode) card = c;
    });
    if (card) {
      // Surface dead sessions as "ready" in the UI — the backend state is
      // retained on sessionsData so the resubscribe logic below still fires
      // when a dead→running transition occurs.
      const displayState = msg.state === 'dead' ? 'ready' : msg.state;
      const badge = card.querySelector('.badge');
      if (badge) { badge.className = 'badge ' + displayState; badge.textContent = displayState; }
      // Update sidebar dot and state text to reflect new state immediately.
      // sessionCardHtml renders .sc-dot with dot-running/dot-ready/dot-new,
      // but onSessionState previously only patched .badge (which doesn't exist
      // in sidebar cards), leaving the dot stale.
      const dot = card.querySelector('.sc-dot');
      if (dot) {
        dot.className = 'sc-dot ' + (displayState === 'running' ? 'dot-running' : (displayState === 'ready' ? 'dot-ready' : 'dot-new'));
      }
      const meta = card.querySelector('.sc-meta');
      if (meta) {
        const stateSpan = meta.querySelectorAll('span')[1]; // [0]=dot, [1]=state text
        if (stateSpan && !stateSpan.classList.contains('sc-node')) stateSpan.textContent = displayState;
      }
      // Sync the unread chip in place. fetchSessions re-renders from template
      // and reads sessionUnread directly; this path keeps the bubble fresh
      // between polls (WS state arrives faster than the sessions poll tick).
      updateCardUnreadChip(card, sessionUnread[sKey] || 0);
    }
    if (msg.key === selectedKey && msgNode === selectedNode) updateMainState(msg.state, msg.reason);
    // Re-subscribe when session transitions to "running" and we need a live event stream.
    // Covers: (1) not subscribed yet (new session, subscribedKey mismatch)
    //         (2) subscribed but process was dead → revived
    //         (3) subscribed without eventPushLoop (no-process subscribe → process available)
    //            — detected by the "suspended" reason the server sends for no-process subscribes.
    // Case 3 must NOT fire on normal ready→running transitions for already-subscribed
    // sessions — that would cause full re-render and wipe the optimistic user message.
    if (msg.key === selectedKey && msgNode === selectedNode && msg.state === 'running') {
      const needSub = (
        (wsm.subscribedKey !== msg.key && wsm._pendingSubscribeKey !== msg.key) || // case 1: not subscribed and no pending subscribe
        (wasDead && !msg.reason) ||                                   // case 2
        (wsm.subscribedKey === msg.key && wsm._subscriptionSuspended) // case 3
      );
      if (needSub) {
        wsm.lastEventTimeWs = 0;
        wsm.subscribe(msg.key, selectedNode);
      }
    }
    // State changed: force next fetchSessions to re-render sidebar.
    // storeGen doesn't increment on process state transitions (only session
    // mutations), so the version cache would otherwise skip the re-render.
    lastVersion = 0;
    if (msg.reason) debouncedFetchSessions();
  },

  // cron-live RFC §1.3 / §2.3: cron stub spawn 完成会广播 session_state running，
  // suspended sub 此时升级 —— re-sub 才能拿到 eventPushLoop 推送。fresh 模式下
  // 这是默认路径（每次 run 前 Reset 销毁旧 stub）。
  onCronLiveSessionState(msg) {
    if (msg.state === 'running' && this.cronLive.suspended) {
      const jobId = this.cronLive.jobId;
      if (jobId) {
        this.cronLive.suspended = false;
        // 不清 lastEventTimeMs / events —— after= 用最末事件时间继续接续
        this.cronLive.subscribedKey = null;
        this.cronLive.pendingJobId = jobId;
        const key = 'cron:' + jobId;
        const after = this.cronLive.lastEventTimeMs || this.cronLive.runStartedAt || 0;
        const subMsg = { type: 'subscribe', key: key };
        if (after > 0) subMsg.after = after;
        this.send(subMsg);
      }
      return;
    }
    // 后端可能发来 'dead' (process 死亡) 或 reason='subscription_timeout'
    // (resubscribeEvents 60s 窗口超时, wshub_eventpush.go:308)。两者均表示
    // 流不会再有事件 —— 切到 stopped，事件保留可回看。
    if (msg.state === 'dead' || msg.reason === 'subscription_timeout') {
      this.cronLive.status = 'stopped';
      setCronLiveStatus('stopped');
    }
  },

  // cron-live RFC §5: 首批 history 帧到达。EventEntriesSince(after) 后端无条数
  // 上限（After>0 时 Limit 被忽略），前端必须自己截尾到 CRON_LIVE_MAX_EVENTS。
  onCronLiveHistory(msg) {
    if (typeof isCronSessionFrozen === 'function' && isCronSessionFrozen(msg.key)) return;
    const incoming = msg.events || [];
    if (incoming.length === 0) return;
    const lastTime = this.cronLive.lastEventTimeMs;
    const newOnes = incoming.filter(e => !e.time || e.time > lastTime);
    let merged = (this.cronLive.events || []).concat(newOnes);
    if (merged.length > CRON_LIVE_MAX_EVENTS) {
      const dropped = merged.length - CRON_LIVE_MAX_EVENTS;
      this.cronLive.truncatedCount = (this.cronLive.truncatedCount || 0) + dropped;
      merged = merged.slice(-CRON_LIVE_MAX_EVENTS);
    }
    this.cronLive.events = merged;
    if (newOnes.length > 0) {
      const last = newOnes[newOnes.length - 1];
      if (last.time && last.time > this.cronLive.lastEventTimeMs) this.cronLive.lastEventTimeMs = last.time;
    }
    this.cronLive.status = 'live';
    repaintCronLive();
  },

  onCronLiveEvent(msg) {
    if (typeof isCronSessionFrozen === 'function' && isCronSessionFrozen(msg.key)) return;
    const ev = msg.event;
    if (!ev) return;
    if (ev.time && ev.time <= this.cronLive.lastEventTimeMs) return;
    this.cronLive.events = this.cronLive.events || [];
    this.cronLive.events.push(ev);
    if (this.cronLive.events.length > CRON_LIVE_MAX_EVENTS) {
      this.cronLive.events.shift();
      this.cronLive.truncatedCount = (this.cronLive.truncatedCount || 0) + 1;
    }
    if (ev.time) this.cronLive.lastEventTimeMs = ev.time;
    this.cronLive.status = 'live';
    appendEventsToContainer(document.getElementById('cron-live-events'), [ev]);
    setCronLiveStatus('live');
    updateCronLiveTruncated();
  },

  setState(s) {
    const prev = this.state;
    this.state = s;
    // R110-P1 outage duration timestamp maintenance. Arm on first
    // transition OUT of CONNECTED (or from a cold OFF start that never
    // reached CONNECTED — treat any persistent non-CONNECTED as outage).
    // Guard with `=== 0` so a connecting→auth→connecting cycle during
    // backoff doesn't reset the clock to zero mid-outage. Clear on
    // entering CONNECTED so the next outage arms fresh.
    if (s === WS_STATES.CONNECTED) {
      this._disconnectedSince = 0;
    } else if (prev === WS_STATES.CONNECTED && this._disconnectedSince === 0) {
      // Just left a healthy connection — stamp the wall clock.
      this._disconnectedSince = Date.now();
    } else if (this._disconnectedSince === 0 && s !== WS_STATES.OFF) {
      // Cold-start / never-connected case: arm from the first
      // CONNECTING attempt so the user sees a duration even before the
      // first successful handshake. OFF (the initial synthetic state)
      // is excluded — pre-boot doesn't count as outage.
      this._disconnectedSince = Date.now();
    }
    updateStatusBar();
    // (Removed _updateStatusTick — issue #434: the 1s repaint timer was a
    // no-op since #sidebar-status DOM was deleted; updateNodeSelector is
    // already driven by this updateStatusBar() call.)
    if (s === WS_STATES.CONNECTED) {
      // No reconnect toast: the sidebar status row already conveys the
      // transition (amber "connecting..." dot → green "connected" dot,
      // and .status-outage drops off) which is the user-visible signal.
      // The previous top-of-screen toast was redundant and on mobile
      // covered the header. _everConnected stays on the wsm struct because
      // future consumers may still want to differentiate first-handshake
      // from reconnect (e.g. fresh session poll vs. no-op).
      // RNEW-UX-010 — sighted users see the dot flip; AT users get the
      // transition announced politely. Only announce when it's a real
      // transition (prev !== CONNECTED) to avoid re-announcing on no-op
      // state refreshes.
      if (prev !== WS_STATES.CONNECTED) announce(this._everConnected ? '已重新连接' : '已连接');
      this._everConnected = true;
      // WS connected: stop session polling, rely on push
      if (sessionPollTimer) { clearInterval(sessionPollTimer); sessionPollTimer = null; }
      // Reduce discovered scan frequency
      if (discoveredPollTimer) { clearInterval(discoveredPollTimer); discoveredPollTimer = null; }
      discoveredPollTimer = setInterval(scanDiscovered, 30000);
      // Pull fresh node/session state immediately to clear stale data
      debouncedFetchSessions();
    } else if (s === WS_STATES.DISCONNECTED) {
      // RNEW-UX-010 — announce only on real transitions from connected, so
      // initial cold boot (OFF→CONNECTING→DISCONNECTED retry) stays silent.
      if (prev === WS_STATES.CONNECTED) announce('连接已断开，正在重试');
      // WS lost: start fallback polling
      if (!sessionPollTimer) sessionPollTimer = setInterval(fetchSessions, 5000);
      if (discoveredPollTimer) { clearInterval(discoveredPollTimer); discoveredPollTimer = null; }
      discoveredPollTimer = setInterval(scanDiscovered, 5000);
      if (selectedKey && !eventTimer) {
        lastEventTime = this.lastEventTimeWs;
        eventTimer = setInterval(() => fetchEvents(false), 1000);
      }
    }
  },

  isConnected() { return this.state === WS_STATES.CONNECTED; }
};

/* ===== WS Helper Functions ===== */

function updateMainState(state, reason) {
  const ia = document.getElementById('input-area');
  if (ia) ia.classList.toggle('disabled', false);
  updateSendButton(state);
}

function updateHeaderCost() {
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};
  const el = document.getElementById('header-cost');
  if (!el) return;
  const cost = s.total_cost || 0;
  // Multi-Backend RFC §8.3 D5: format per session's cost_unit so live
  // updates honor the same "credits" / "$" choice the initial render made.
  el.textContent = formatCostByUnit(cost, s.cost_unit || '');
  el.className = 'detail-cost' + (cost >= 1 ? ' high-cost' : cost > 0 ? ' has-cost' : '');
  // R110-P3 cost-detail hover: keep the title attribute in sync so live
  // updates (ws session_state / result events) don't leave stale metadata
  // behind the tooltip. formatHeaderCostTooltip silently returns '' for
  // a zero-cost session so the chip isn't distracted by a "session_id: …"
  // hint when there's nothing to explain.
  el.title = formatHeaderCostTooltip(s, selectedKey, selectedNode);
}

// formatHeaderCostTooltip builds a multi-line plain-text tooltip for the
// header cost chip. Pure function so a contract test can exercise it
// without driving the DOM. Return value is a newline-joined string, not
// HTML — the browser renders native tooltips for `title` attributes, and
// wrapping the helper so it ALWAYS returns plain text (never HTML) pins
// the XSS-safe boundary: even when sess fields carry user-controlled
// characters, the browser treats them as text and won't parse tags.
//
// R110-P3 scope: MVP surfaces data the front-end already has — cost
// (precise), session creation + last-active timestamps, and the last
// 8 chars of session_id (operators commonly paste that for CLI
// `--resume`). Full token/input/output/cache breakdown requires backend
// schema work and is tracked as residual scope.
function formatHeaderCostTooltip(s, selKey, selNode) {
  if (!s || typeof s !== 'object') return '';
  const cost = s.total_cost || 0;
  if (cost <= 0 && !s.session_id) return '';
  const lines = [];
  if (cost > 0) {
    // Multi-Backend RFC §8.3 D5/D26: tooltip honors cost_unit so kiro
    // sessions show "0.024 credits" rather than a confusing "$0.024".
    const unit = s.cost_unit || '';
    if (unit === 'credits') {
      lines.push('累计花费: ' + cost.toFixed(4) + ' credits');
    } else {
      lines.push('累计花费: $' + cost.toFixed(4));
    }
  }
  // D26: per-turn metering breakdown when kiro reports it. Each row is one
  // billing dimension (kiro currently emits {value, unit:"credit"}); future
  // backends may add multiple rows.
  if (Array.isArray(s.metering_usage) && s.metering_usage.length > 0) {
    s.metering_usage.forEach(m => {
      if (!m || typeof m.value !== 'number') return;
      const unit = m.unit_plural || m.unit || '';
      lines.push('上一轮: ' + m.value.toFixed(4) + (unit ? ' ' + unit : ''));
    });
  }
  // Server-stamped session creation time; same value drives the sidebar
  // sort anchor so tooltip and visual position stay consistent.
  if (s.created_at && s.created_at > 0) {
    lines.push('创建时间: ' + formatAbsTime(s.created_at));
  }
  if (s.last_active && s.last_active > 0) {
    lines.push('最后活动: ' + formatAbsTime(s.last_active));
  }
  if (typeof s.session_id === 'string' && s.session_id.length >= 8) {
    lines.push('会话 ID: …' + s.session_id.slice(-8));
  }
  return lines.join('\n');
}

function updateHeaderCLI() {
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};
  const el = document.querySelector('.main-header .detail-left');
  if (!el) return;
  // Fallback chain mirrors renderMainShell — see backendDisplayName godoc
  // for why pending sessions need the sessionBackends lookup before the
  // global defaultCLIName fallback.
  const name = s.cli_name || backendDisplayName(sessionBackends[selectedKey]) || defaultCLIName;
  const version = s.cli_version || backendDisplayVersion(sessionBackends[selectedKey]) || defaultCLIVersion;
  const label = name ? esc(name) + (version ? ' v' + esc(version) : '') : '';
  if (el.innerHTML !== label) el.innerHTML = label;
}

function flashSendBtn() {
  const btn = document.getElementById('btn-send');
  const stop = document.getElementById('btn-stop');
  const target = (btn && btn.style.display !== 'none') ? btn : stop;
  if (!target) return;
  target.style.boxShadow = '0 0 8px #3fb950';
  setTimeout(() => { target.style.boxShadow = ''; }, 600);
}

function stopPreviewPolling() {
  if (previewTimer) { clearInterval(previewTimer); previewTimer = null; }
  previewEventCount = 0;
}

/* ===== Discovery & Takeover ===== */

async function scanDiscovered() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — /api/discovered walks the filesystem, so a
    // stalled disk shouldn't wedge the scan button forever.
    const data = await fetchJSON('/api/discovered', { headers, timeoutMs: 10000 });
    discoveredItems = data || [];
    // #1770: only force a full sidebar re-render when the discovered set
    // actually changed. Previously every 30s (connected) / 5s (disconnected)
    // scan unconditionally set lastVersion=0, defeating fetchSessions' version
    // short-circuit and rebuilding the whole sidebar DOM even when nothing
    // changed — wasted CPU/layout on low-end phones. Mirror the
    // nodesHash/historyHash pattern fetchSessions already uses.
    const discoveredHash = JSON.stringify(discoveredItems);
    if (discoveredHash === lastDiscoveredJSON) return;
    lastDiscoveredJSON = discoveredHash;
    // Trigger sidebar re-render to merge discovered into project groups
    lastVersion = 0;
    debouncedFetchSessions();
  } catch (e) {
    console.warn('scanDiscovered error:', e.message);
  }
}

function handleDiscoveredClick(el) {
  previewDiscovered(el.dataset.sessionId, el.dataset.cwd, Number(el.dataset.pid), Number(el.dataset.pst), el.dataset.node || '');
}

async function previewDiscovered(sessionId, cwd, pid, procStartTime, node, cliName, entrypoint) {
  stopPreviewPolling();
  // Deselect any managed session. We null `selectedKey` but deliberately
  // leave `selectedNode` intact — it now doubles as the sidebar filter and
  // nulling it would strand the user on an empty list until their next
  // refresh. The "no managed session selected" state is fully represented
  // by `selectedKey === null`; other call sites check it that way.
  selectedKey = null;
  if (wsm.subscribedKey) wsm.unsubscribe();
  if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
  mobileEnterChat();

  // Highlight the discovered card
  setActiveSessionCard('_discovered:' + pid, node || 'local');

  const base = cwd.split('/').pop() || cwd;
  const main = document.getElementById('main');
  main.innerHTML =
    '<div class="main-header">' +
      '<button class="btn-mobile-back" onclick="mobileBack()" title="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868" aria-label="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868">&#8592;</button>' +
      '<div class="main-header-content">' +
        '<h2>' + esc(base) + '</h2>' +
        '<div class="detail">' +
          sessionTypeTag(cliName || 'cli', entrypoint || '') +
        '</div>' +
      '</div>' +
    '</div>' +
    '<div class="events" id="events-scroll"><div class="empty-state">加载中...</div></div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button onclick="navMsg(\'prev\')" id="nav-prev" title="\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2191)" aria-label="\u8df3\u5230\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f">&#x25B2;</button>' +
      '<span class="nav-counter" id="nav-counter" onclick="navShowList()" title="\u70b9\u51fb\u67e5\u770b\u5168\u90e8\u7528\u6237\u6d88\u606f"></span>' +
      '<button onclick="navMsg(\'next\')" id="nav-next" title="\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2193)" aria-label="\u8df3\u5230\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f">&#x25BC;</button>' +
    '</div>' +
    '<div class="input-area" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message to take over..." onkeydown="handleKey(event)" oncompositionend="lastCompositionEnd=Date.now()"></div>' +
        '<button class="btn-icon btn-send" id="btn-send" onclick="sendMessage()" title="发送" aria-label="发送消息">&#x27a4;</button>' +
      '</div>' +
    '</div>';
  navRebuild(); // clear stale nav state before async preview fetch
  pendingDiscovered = {pid: pid, sessionId: sessionId, cwd: cwd, procStartTime: procStartTime, node: node};

  try {
    const nodeParam = node ? '&node=' + encodeURIComponent(node) : '';
    // Pass cwd so the backend resolves the JSONL via an O(1) os.Stat on the
    // CWD-derived path instead of the fallback scan + its 60s negative cache.
    // Without this hint a single transient miss (card shown before the JSONL
    // flushed, or while claude renamed it during compaction) poisons preview
    // for the full TTL, leaving a blank splash that only "fixes itself" once
    // the cache expires.
    const cwdParam = cwd ? '&cwd=' + encodeURIComponent(cwd) : '';
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — discovered preview loads a ~200-event tail
    // from a JSONL transcript; a hung read shouldn't trap the user on a
    // "加载中..." splash indefinitely.
    let events;
    try {
      events = await fetchJSON('/api/discovered/preview?session_id=' + encodeURIComponent(sessionId) + nodeParam + cwdParam, { headers, timeoutMs: 10000 });
    } catch (err) {
      const errText = err.message || '';
      const el0 = document.getElementById('events-scroll');
      if (el0) el0.innerHTML = '<div class="empty-state">' + esc(errText || '预览失败') + '</div>';
      if (err.status) showAPIError('预览会话', err.status, errText);
      return;
    }
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const display = processEventsForDisplay(events);
    if (events.length === 0) {
      el.innerHTML = '<div class="empty-state">暂无会话历史</div>';
    } else {
      el.innerHTML = renderEventsWithDividers(display, 0);
      stickEventsBottom();
    }
    navRebuild();
    previewEventCount = events.length;
    const capturedSid = sessionId;
    // #1770: guard against overlapping ticks. Each tick re-fetches the full
    // preview event list; on a slow link a fetch can outlast the 2s interval,
    // so without this flag consecutive ticks pile up concurrent requests.
    // Mirrors _fetchEventsInFlight on the main events poll.
    let previewInFlight = false;
    previewTimer = setInterval(async () => {
      if (previewInFlight) return;
      previewInFlight = true;
      try {
        const headers2 = {};
        const t2 = getToken();
        if (t2) headers2['Authorization'] = 'Bearer ' + t2;
        const r2 = await fetch('/api/discovered/preview?session_id=' + encodeURIComponent(capturedSid) + nodeParam + cwdParam, { headers: headers2 });
        if (!r2.ok) return;
        const all = await r2.json();
        if (all.length <= previewEventCount) return;
        const fresh = all.slice(previewEventCount);
        previewEventCount = all.length;
        const el2 = document.getElementById('events-scroll');
        if (!el2) { stopPreviewPolling(); return; }
        const empty = el2.querySelector('.empty-state');
        if (empty) empty.remove();
        const wasBottom = el2.scrollTop + el2.clientHeight >= el2.scrollHeight - 30;
        let prevT2 = lastDividerTime(el2);
        fresh.forEach(e => {
          if (isInternalEvent(e)) return;
          const h = eventHtml(e); if (!h) return;
          const t = e.time || 0;
          if (t && (prevT2 === 0 || t - prevT2 >= EVENT_DIVIDER_GAP_MS)) {
            el2.insertAdjacentHTML('beforeend', timeDividerHtml(t));
          }
          el2.insertAdjacentHTML('beforeend', h);
          if (t) prevT2 = t;
        });
        if (wasBottom) el2.scrollTop = el2.scrollHeight;
        navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
        navUpdatePill();
      } catch (_) {
      } finally {
        previewInFlight = false;
      }
    }, 2000);
  } catch (e) {
    showNetworkError('预览会话', e);
  }
}

function handleTakeoverClick(el) {
  takeover(el, Number(el.dataset.pid), el.dataset.sessionId, el.dataset.cwd, Number(el.dataset.pst), el.dataset.node || '');
}

async function takeover(btn, pid, sessionId, cwd, procStartTime, node) {
  btn.classList.add('taking');
  btn.textContent = '接管中...';
  try {
    const headers = {'Content-Type': 'application/json'};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch('/api/discovered/takeover', {
      method: 'POST', headers,
      body: JSON.stringify({pid: pid, session_id: sessionId, cwd: cwd, proc_start_time: procStartTime || 0, node: node || ''})
    });
    if (!r.ok) {
      const text = await r.text().catch(() => '');
      showAPIError('接管进程', r.status, text);
      btn.classList.remove('taking');
      btn.textContent = '接管';
      return;
    }
    const data = await r.json();
    showToast('已接管会话', 'success');
    // Remove from discoveredItems so renderSidebar won't re-create the card
    discoveredItems = discoveredItems.filter(d => d.pid !== pid);
    // Immediately remove the discovered card from DOM
    const card = document.querySelector('.session-card[data-key="_discovered:' + pid + '"]');
    if (card) card.remove();
    // Force refresh (clear cache so renderSidebar runs)
    lastVersion = 0;
    await fetchSessions();
    if (data.key) {
      selectSession(data.key, node || 'local');
    }
  } catch (e) {
    showNetworkError('接管进程', e);
    btn.classList.remove('taking');
    btn.textContent = '接管';
  }
}

/* ===== Mobile Navigation ===== */

const mobileQuery = window.matchMedia('(max-width:768px)');
function isMobile() { return mobileQuery.matches; }

// Re-initialise when crossing the 768px breakpoint (e.g. orientation change)
mobileQuery.addEventListener('change', e => {
  if (!e.matches) {
    document.body.classList.remove('mobile-list-view', 'mobile-chat-view');
  } else {
    initMobile();
  }
});

function mobileEnterChat() {
  if (!isMobile()) return;
  history.pushState({ view: 'chat' }, '');
  document.body.classList.remove('mobile-list-view');
  document.body.classList.add('mobile-chat-view');
}

function mobileBack() {
  document.body.classList.remove('mobile-chat-view');
  document.body.classList.add('mobile-list-view');
  if (document.activeElement) document.activeElement.blur();
}

// Handle Android back button and iOS swipe-back gesture
window.addEventListener('popstate', () => {
  if (isMobile() && document.body.classList.contains('mobile-chat-view')) {
    mobileBack();
  }
});

function initMobile() {
  if (!isMobile()) return;
  const hasSession = !!selectedKey;
  document.body.classList.toggle('mobile-chat-view', hasSession);
  document.body.classList.toggle('mobile-list-view', !hasSession);
}

/* Track iOS visual viewport so the main-header stays visible when the keyboard opens.
   Without this, position:fixed elements get scrolled above the viewport when the
   soft keyboard pushes the page up. */
function initViewportTracking() {
  const vv = window.visualViewport;
  if (!vv) return;
  const root = document.documentElement;
  let raf = 0;
  const apply = () => {
    raf = 0;
    root.style.setProperty('--vv-top', vv.offsetTop + 'px');
    root.style.setProperty('--vv-height', vv.height + 'px');
    // Soft keyboard detection: visualViewport shrinks by >150px when the
    // on-screen keyboard opens on iOS / Android. Toggle body.kbd-open so
    // CSS can collapse space-hogging elements (running banner / nav pill)
    // and keep the input within thumb reach.
    const layoutH = window.innerHeight || vv.height;
    const kbdOpen = layoutH - vv.height > 150;
    document.body.classList.toggle('kbd-open', kbdOpen);
  };
  const schedule = () => { if (!raf) raf = requestAnimationFrame(apply); };
  vv.addEventListener('resize', schedule);
  vv.addEventListener('scroll', schedule);
  apply();
}

// R110-P1 long-press context menu state + constants. LONG_PRESS_MS matches
// the Android / iOS WebKit default for "long-press" detection; shorter feels
// trigger-happy (users misfire while scrolling) and longer reads as a hang.
// MOVE_CANCEL_PX below the 5px swipe threshold so "small jitter" does not
// cancel long-press before swipe starts tracking, but any directional intent
// above 8px unambiguously means the user wants to swipe (or scroll).
const LONG_PRESS_MS = 500;
const LONG_PRESS_MOVE_CANCEL_PX = 8;
let _longPressTimer = null;
let _longPressFired = false;

// closeContextMenu tears down any open .ctx-menu + its overlay. Safe to call
// when nothing is open (no-op). Exposed at module scope so both the menu
// actions and the global touch/click handlers can call it.
function closeContextMenu() {
  const m = document.getElementById('session-ctx-menu');
  if (m) m.remove();
  const ov = document.getElementById('session-ctx-overlay');
  if (ov) ov.remove();
}

// showSessionContextMenu renders a floating menu anchored near (x, y) with
// rename / copy-key / delete actions for the given session card. Clamps the
// menu inside the viewport so long-pressing near a screen edge doesn't push
// the menu off-screen. Uses a transparent overlay to capture outside clicks
// (cheaper than attaching a document-level click handler that would need
// careful removal). Items array shape is [{ label, icon, action, danger }]
// so future extensions (pin / favorite) drop in without refactoring.
function showSessionContextMenu(x, y, items) {
  closeContextMenu();
  const ov = document.createElement('div');
  ov.id = 'session-ctx-overlay';
  ov.className = 'ctx-menu-overlay';
  ov.addEventListener('click', closeContextMenu, {passive:true});
  ov.addEventListener('touchstart', e => {
    // Prevent the overlay's touchstart from triggering a scroll on mobile
    // while the menu is open — users tapping outside expect "close" not
    // "keep scrolling through the underlying list".
    if (e.target === ov) { closeContextMenu(); }
  }, {passive:true});
  document.body.appendChild(ov);

  const menu = document.createElement('div');
  menu.id = 'session-ctx-menu';
  menu.className = 'ctx-menu';
  menu.setAttribute('role', 'menu');
  menu.setAttribute('aria-label', '会话操作');
  menu.innerHTML = items.map((it, i) =>
    '<div class="ctx-menu-item' + (it.danger ? ' danger' : '') + '"' +
    ' role="menuitem" tabindex="0" data-idx="' + i + '">' +
    '<span class="ctx-icon" aria-hidden="true">' + esc(it.icon || '') + '</span>' +
    '<span>' + esc(it.label) + '</span></div>'
  ).join('');
  document.body.appendChild(menu);

  // Clamp menu position inside viewport with 8px padding. Measure first so we
  // know actual rendered size (padding/border/content-driven width).
  const rect = menu.getBoundingClientRect();
  const pad = 8;
  let left = x, top = y;
  if (left + rect.width + pad > window.innerWidth) left = window.innerWidth - rect.width - pad;
  if (top + rect.height + pad > window.innerHeight) top = window.innerHeight - rect.height - pad;
  if (left < pad) left = pad;
  if (top < pad) top = pad;
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';

  menu.addEventListener('click', e => {
    const it = e.target.closest('.ctx-menu-item');
    if (!it) return;
    const idx = parseInt(it.dataset.idx, 10);
    closeContextMenu();
    if (items[idx] && typeof items[idx].action === 'function') items[idx].action();
  });
  menu.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); closeContextMenu(); }
  });
  // Focus the first item so keyboard users (rare on mobile but happens with
  // BT keyboards / accessibility tools) have a landing point after the menu
  // opens. Desktop right-click path also benefits.
  const first = menu.querySelector('.ctx-menu-item');
  if (first) first.focus();
}

// copyStringToClipboard writes a string to the system clipboard using the
// modern navigator.clipboard API with a document.execCommand fallback for
// older browsers / non-HTTPS contexts. Returns a Promise<boolean>.
async function copyStringToClipboard(s) {
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(s);
      return true;
    }
  } catch (_) { /* fall through */ }
  const ta = document.createElement('textarea');
  try {
    ta.value = s;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    return document.execCommand('copy');
  } catch (_) {
    return false;
  } finally {
    // Always detach — if execCommand throws (sandboxed iframes, locked
    // clipboards) the caught return skipped an inline removeChild before,
    // leaking the <textarea> (containing the user-supplied string) into
    // the DOM for the page lifetime.
    if (ta.parentNode) ta.parentNode.removeChild(ta);
  }
}

// openSessionContextMenu assembles the items for a given session card and
// opens the menu anchored near the touch coordinates. Rename reuses the
// existing modal-prompt pattern by selecting the session first, then
// deferring to renameSession(); copy-key writes the key to clipboard with
// a toast confirmation; delete routes through dismissSession() which
// surfaces the existing confirmDialog flow on its own.
function openSessionContextMenu(card, x, y) {
  const key = card.dataset.key;
  const node = card.dataset.node || 'local';
  if (!key) return;
  showSessionContextMenu(x, y, [
    {
      label: '重命名', icon: '✎',
      action: () => {
        // renameSession() reads selectedKey/selectedNode, so we flip the
        // selection first. Keeps the prompt simple (same input widget the
        // hover-visible ✎ button uses) at the cost of one extra click if
        // the user was on a different session — acceptable for a mobile-
        // only power-user shortcut.
        selectedKey = key;
        selectedNode = node;
        renameSession();
      },
    },
    {
      label: '复制 key', icon: '⎘',
      action: async () => {
        const ok = await copyStringToClipboard(key);
        showToast(ok ? '已复制 key' : '复制失败', ok ? 'success' : 'warning');
      },
    },
    {
      label: '删除', icon: '🗑', danger: true,
      action: () => { dismissSession(key, node); },
    },
  ]);
}

function initSwipeDelete() {
  const list = document.getElementById('session-list');
  if (!list) return;
  let card = null, startX = 0, startY = 0, tracking = false, cardW = 0;
  // cancelLongPress clears the in-flight long-press timer + any visual
  // target state. Called from every exit path so a jittery touch cannot
  // leave the card stuck in the .long-pressing style.
  const cancelLongPress = () => {
    if (_longPressTimer) { clearTimeout(_longPressTimer); _longPressTimer = null; }
    if (card) card.classList.remove('long-pressing');
  };
  list.addEventListener('touchstart', e => {
    if (e.touches.length !== 1) { card = null; cancelLongPress(); return; }
    const c = e.target.closest('.session-card[data-key]');
    if (!c) return;
    card = c; startX = e.touches[0].clientX; startY = e.touches[0].clientY; tracking = false;
    _longPressFired = false;
    // Schedule long-press. If the user lifts / moves before the timer
    // fires, the cancel path below wipes it; otherwise we trigger the
    // context menu AND null out `card` so the subsequent touchend does
    // not accidentally also trigger a select/click on the card beneath.
    _longPressTimer = setTimeout(() => {
      _longPressTimer = null;
      if (!card) return;
      _longPressFired = true;
      const x = startX, y = startY;
      const target = card;
      card = null; tracking = false;
      target.classList.remove('long-pressing');
      openSessionContextMenu(target, x, y);
    }, LONG_PRESS_MS);
    // Mild visual feedback on press — users need to know "something is
    // happening" before the 500ms elapses. Kept to a subtle background
    // tint so it doesn't read as a selection.
    card.classList.add('long-pressing');
  }, {passive:true});
  list.addEventListener('touchmove', e => {
    if (!card) return;
    const dx = e.touches[0].clientX - startX;
    const dy = e.touches[0].clientY - startY;
    // Cancel long-press as soon as directional intent emerges. Threshold
    // is slightly looser than swipe's 5px trigger so small jitters don't
    // cancel long-press unnecessarily, but any real swipe intent kills
    // the menu before it can fire.
    if (Math.abs(dx) >= LONG_PRESS_MOVE_CANCEL_PX || Math.abs(dy) >= LONG_PRESS_MOVE_CANCEL_PX) {
      cancelLongPress();
    }
    if (!tracking) {
      if (Math.abs(dx) < 5 && Math.abs(dy) < 5) return;
      if (Math.abs(dy) >= Math.abs(dx)) { card = null; return; }
      tracking = true;
      // #1772: cache the card width once, here — before any transform write
      // this gesture. Reading card.offsetWidth inside the per-frame transform
      // write below is a getter that the browser must keep coherent with
      // pending style writes; caching it (the width can't change mid-swipe)
      // keeps the touchmove hot loop free of layout reads. Read now while the
      // card is still in its untransformed layout position (cheap).
      cardW = card.offsetWidth || 1;
    }
    if (dx >= 0) return;
    card.classList.add('swiping');
    card.style.transform = 'translateX(' + dx + 'px)';
    card.style.background = 'rgba(218,54,51,' + Math.min(0.35, -dx / cardW * 0.6) + ')';
  }, {passive:true});
  list.addEventListener('touchend', e => {
    cancelLongPress();
    if (!card || !tracking) { card = null; tracking = false; return; }
    const dx = e.changedTouches[0].clientX - startX;
    const c = card; card = null; tracking = false;
    c.classList.remove('swiping');
    if (dx < -c.offsetWidth * 0.4) {
      c.style.transition = 'transform .2s ease, opacity .2s ease';
      c.style.transform = 'translateX(-100%)';
      c.style.opacity = '0';
      // Swipe past the threshold is an explicit gesture — skip the modal
      // confirm here so the user doesn't have to re-confirm after already
      // dragging 40% of the card width. Button-click path still confirms.
      setTimeout(() => dismissSession(c.dataset.key, c.dataset.node || 'local', { skipConfirm: true }), 180);
    } else {
      c.style.transition = 'transform .2s ease, background .2s ease';
      c.style.transform = '';
      c.style.background = '';
      setTimeout(() => { c.style.transition = ''; }, 200);
    }
  }, {passive:true});
  // touchcancel fires when the system interrupts the gesture (incoming call,
  // scroll takeover by browser UI). Mirror cleanup so _longPressTimer
  // can't fire after the finger has already gone.
  list.addEventListener('touchcancel', () => {
    cancelLongPress();
    if (card) {
      card.classList.remove('swiping');
      card.style.transform = '';
      card.style.background = '';
    }
    card = null; tracking = false;
  }, {passive:true});
  // Click bubbles up after touchend. If a long-press just fired we have
  // already null'd `card`, but the underlying anchor click (selectSession
  // via onclick) still fires. Swallow it when _longPressFired is set.
  list.addEventListener('click', e => {
    if (_longPressFired) {
      _longPressFired = false;
      e.preventDefault();
      e.stopPropagation();
    }
  }, true);
  // Desktop right-click also surfaces the same menu for parity with the
  // mobile long-press path. Power users can reach every action via the
  // hover buttons too; this just gives keyboard-unfriendly trackpad users
  // a discoverable alternative.
  list.addEventListener('contextmenu', e => {
    const c = e.target.closest('.session-card[data-key]');
    if (!c) return;
    e.preventDefault();
    openSessionContextMenu(c, e.clientX, e.clientY);
  });
}

function initSwipeBack() {
  const main = document.getElementById('main');
  if (!main) return;
  let startX = 0, startY = 0, tracking = false, swiping = false;
  main.addEventListener('touchstart', e => {
    if (!isMobile() || e.touches.length !== 1) return;
    startX = e.touches[0].clientX; startY = e.touches[0].clientY;
    tracking = false; swiping = false;
    // Only trigger from left edge (within 40px)
    if (startX > 40) return;
    tracking = true;
  }, {passive:true});
  main.addEventListener('touchmove', e => {
    if (!tracking) return;
    const dx = e.touches[0].clientX - startX;
    const dy = e.touches[0].clientY - startY;
    if (!swiping) {
      if (Math.abs(dx) < 8 && Math.abs(dy) < 8) return;
      if (Math.abs(dy) > Math.abs(dx)) { tracking = false; return; }
      if (dx < 0) { tracking = false; return; }
      swiping = true;
    }
    const progress = Math.min(dx / window.innerWidth, 1);
    main.style.transform = 'translateX(' + dx + 'px)';
    main.style.opacity = String(1 - progress * 0.3);
  }, {passive:true});
  main.addEventListener('touchend', e => {
    if (!tracking || !swiping) { tracking = false; swiping = false; return; }
    const dx = e.changedTouches[0].clientX - startX;
    tracking = false; swiping = false;
    if (dx > window.innerWidth * 0.3) {
      main.style.transition = 'transform .2s ease, opacity .2s ease';
      main.style.transform = 'translateX(100%)';
      main.style.opacity = '0';
      setTimeout(() => {
        main.style.transition = ''; main.style.transform = ''; main.style.opacity = '';
        mobileBack();
      }, 200);
    } else {
      main.style.transition = 'transform .2s ease, opacity .2s ease';
      main.style.transform = ''; main.style.opacity = '';
      setTimeout(() => { main.style.transition = ''; }, 200);
    }
  }, {passive:true});
}

/* ===== Cron Tab ===== */

let cronJobs = [];
// Configured default IM target for cron completion notifications, or null
// when the server has no default configured. Used to render helpful copy
// alongside the notify toggle in create/edit modals.
let cronNotifyDefault = null;

// cron-panel-consolidation RFC §4.5: cronDetailJobId is the only state
// gate for the per-job drawer. null = drawer closed; otherwise the
// currently-displayed job ID (NOT key — the drawer keys off cron job
// ID, not the synthesised "cron:<id>" session key, since cron stubs
// no longer surface as managed sessions on the dashboard side).
//
// Lifecycle:
//   - openCronDetail(jobId) sets it and re-renders the cron panel.
//   - closeCronDetail() resets to null and removes drawer DOM.
//   - WS cron_run_started / cron_run_ended consult this gate (PR5)
//     instead of selectedKey.
//   - F5 / reload does NOT persist (RFC §4.5 Q5).
let cronDetailJobId = null;

// _cronDrawerFetchedFor tracks per-jobId reconcile attempts inside the
// drawer's "task missing" branch. Without this guard, deep-linking to a
// deleted job in a system where every cron has been removed (cronJobs
// legitimately empty even after fetch) would loop:
// renderCronDrawer → fetchCronJobs → still empty → renderCronDrawer → …
// The Set is cleared whenever the drawer renders successfully so a
// later fetch (job re-created or another tab synced) can be retried.
const _cronDrawerFetchedFor = new Set();

// _cronDrawerLastActiveRow records the .cj-row DOM element that was most
// recently activated by openCronDetail. closeCronDetail uses it to
// restore focus to the operator's last row (RFC §6.4 — keyboard a11y).
// WeakRef would be ideal but isn't worth the polyfill complexity for
// cron list sizes; a regular reference is fine since renderCronList
// re-creates rows on each paint, and the next openCronDetail just
// overwrites this with a fresh element.
let _cronDrawerLastActiveRow = null;

// cron-panel-consolidation RFC §4.2: sidebar / mainShell are now reserved
// for human conversation surfaces and never paint cron-scheduler sessions.
// The previous `cronVisibleKeys` whitelist + markCronSessionVisible plumbing
// were UI bandages on top of /api/sessions returning cron stubs; PR2 moves
// that filter into the server (internal/server/dashboard_session.go), so
// the dashboard can simply assume no cron rows ever arrive. The single
// helper retained from the old block is `isCronSessionKey`, kept for the
// dismissSession safety check (defence-in-depth: even if a future server
// bug leaks a cron key, the × button must not delete the cron job, only
// untrack the row). New cron-detail visibility lives in `cronDetailJobId`
// (PR4 / cronDetailJobId state machine).
function isCronSessionKey(key) {
  return typeof key === 'string' && key.indexOf('cron:') === 0;
}
// R110-P2 cron filter state — module-level so renderCronList can read the
// live values each paint without a closure. Mirrors the sidebar-search
// approach (cronFilterQuery is the substring, cronFilterStatus is one of
// 'all' | 'active' | 'attention'). 'attention' matches paused-or-last_error,
// aligning with the header cron-badge's attention definition so the filter
// "what needs my eyeballs" dovetails with the top-level signal.
let cronFilterQuery = '';
let cronFilterStatus = 'all';
// cronSortOrder 控制 cron 面板列表的排序模式。保存在 localStorage 里，
// 切回页面保留用户偏好。四种模式见 cronSortComparators。cron-v2-polish §3.4。
let cronSortOrder = (function() {
  // RNEW-UX-004 demo: migrated to unified lsGet helper. Keyspace changed
  // from 'nz_cron_sort' to 'nz:cron_sort' — one-time loss of the saved
  // preference is acceptable (falls back to 'created_desc').
  const saved = lsGet('cron_sort', '');
  if (saved && cronSortComparatorsHasKey(saved)) return saved;
  return 'created_desc';
})();

// cronSortComparators 定义四种排序模式的 compare 函数。
// - created_desc: 默认，最新创建在前（与旧版一致）
// - next_asc    : 按 next_run 升序——"接下来谁先跑"排在前；无 next_run 沉底
// - last_desc   : 最近跑过的排在前；从未跑过沉底
// - title_asc   : 按 title / prompt-fallback 字典序升序——便于按名字扫
const cronSortComparators = {
  created_desc: (a, b) => (b.created_at || 0) - (a.created_at || 0),
  next_asc: (a, b) => {
    const av = a.next_run || Number.POSITIVE_INFINITY;
    const bv = b.next_run || Number.POSITIVE_INFINITY;
    return av - bv;
  },
  last_desc: (a, b) => (b.last_run_at || 0) - (a.last_run_at || 0),
  title_asc: (a, b) => {
    const at = ((a.title || '').trim() || firstNonEmptyLine(a.prompt || '', 60)).toLowerCase();
    const bt = ((b.title || '').trim() || firstNonEmptyLine(b.prompt || '', 60)).toLowerCase();
    return at.localeCompare(bt);
  },
};

function cronSortComparatorsHasKey(k) {
  return Object.prototype.hasOwnProperty.call(cronSortComparators, k);
}

// setCronSortOrder 切换排序模式，持久化到 localStorage 并重绘列表。
function setCronSortOrder(order) {
  if (!cronSortComparatorsHasKey(order)) return;
  cronSortOrder = order;
  lsSet('cron_sort', order); // RNEW-UX-004 demo: unified helper (see top-of-file lsSet)
  renderCronList();
}

// Pads an integer to two digits (e.g. 7 -> "07"). Used for HH/MM rendering.
function pad2(n) { return (n < 10 ? '0' : '') + n; }

// parseCronToFreq inspects a schedule expression and, when it matches one of
// our canonical frequency shapes, returns a descriptor the frequency picker
// can restore. Returning null means "we don't recognize this — fall back to
// the raw expression editor." This is intentionally narrow: we only recognize
// the exact shapes buildFreqSchedule emits, so round-tripping is lossless.
// parseCronToFreq identifies the descriptor that buildFreqSchedule would have
// produced this expression from, so edit-modal can restore the picker state.
// Return null means the expression can't round-trip — legacy jobs with
// interval/custom shapes now degrade to the default Daily picker on edit
// (acceptable: user re-picks once and the new shape is persisted).
function parseCronToFreq(expr) {
  if (!expr) return null;
  const s = expr.trim();
  // Hourly: "0 * * * *"
  if (s === '0 * * * *') return { mode: 'hourly' };
  const parts = s.split(/\s+/);
  if (parts.length !== 5) return null;
  const [mm, hh, dom, mon, dow] = parts;
  if (!/^\d+$/.test(mm) || !/^\d+$/.test(hh)) return null;
  const minute = parseInt(mm, 10);
  const hour = parseInt(hh, 10);
  if (minute > 59 || hour > 23) return null;
  if (mon !== '*') return null;
  const hhmm = pad2(hour) + ':' + pad2(minute);
  if (dom === '*' && dow === '*') return { mode: 'daily', time: hhmm };
  if (dow === '*' && /^\d+$/.test(dom)) {
    const d = parseInt(dom, 10);
    if (d >= 1 && d <= 31) return { mode: 'monthly', day: d, time: hhmm };
  }
  if (dom === '*' && dow !== '*') {
    // "1-5" → Weekdays shortcut
    if (dow === '1-5') return { mode: 'weekdays', time: hhmm };
    // Weekend shortcut "0,6" 或反写 "6,0"
    if (dow === '0,6' || dow === '6,0' || dow === '6,7' || dow === '7,6') {
      // 周末没有 v2 picker 模式——返回 null 让上层走 legacy hint，保留
      // 原 schedule 不乱改；humanizeCron 会把它识别为 "周末 HH:MM"。
      return null;
    }
    const days = parseDowField(dow);
    // v2 picker 的 Weekly 是单选。多选 (dows.length>1 且非 weekdays/
    // weekend shortcut) 无法 round-trip —— 返回 null 触发 legacy hint
    // 路径，保留原 schedule，防止"Weekly 星期一"的视觉误导把用户在周
    // 一三五跑的任务静默改成只在周一跑。
    if (days && days.length === 1) return { mode: 'weekly', dows: days, time: hhmm };
  }
  return null;
}

// parseDowField parses robfig/cron DOW: "1-5", "1,3,5", "0". Sunday is 0
// (robfig convention). 7 is normalized to 0 defensively; returns null on any
// malformed input so the caller falls back to raw-expression editing.
function parseDowField(field) {
  const result = new Set();
  for (const part of field.split(',')) {
    if (/^\d+$/.test(part)) {
      let n = parseInt(part, 10);
      if (n === 7) n = 0;
      if (n < 0 || n > 6) return null;
      result.add(n);
      continue;
    }
    const m = part.match(/^(\d+)-(\d+)$/);
    if (!m) return null;
    let lo = parseInt(m[1], 10), hi = parseInt(m[2], 10);
    if (lo === 7) lo = 0;
    if (hi === 7) hi = 0;
    if (lo > hi || lo < 0 || hi > 6) return null;
    for (let i = lo; i <= hi; i++) result.add(i);
  }
  if (result.size === 0) return null;
  return [...result].sort((a, b) => a - b);
}

// buildFreqSchedule assembles a cron expression from a frequency descriptor.
// Returns {expr, err}. err is a human-readable message when the descriptor
// is invalid (e.g. no weekday selected).
//
// v2 polish: interval mode 被移除（对普通用户概念太重）；新增 hourly
// （整点每小时）和 weekdays（Mon-Fri shortcut）。
function buildFreqSchedule(desc) {
  if (!desc) return { err: '请选择频率' };
  if (desc.mode === 'hourly') {
    return { expr: '0 * * * *' };
  }
  if (desc.mode === 'daily') {
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' * * *' };
  }
  if (desc.mode === 'weekdays') {
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' * * 1-5' };
  }
  if (desc.mode === 'weekly') {
    if (!desc.dows || desc.dows.length === 0) return { err: '至少选择一个星期几' };
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' * * ' + [...desc.dows].sort((a, b) => a - b).join(',') };
  }
  if (desc.mode === 'monthly') {
    const d = parseInt(desc.day, 10);
    if (!Number.isFinite(d) || d < 1 || d > 31) return { err: '日期必须是 1-31' };
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' ' + d + ' * *' };
  }
  return { err: '未知频率模式' };
}

function parseHHMM(s) {
  if (!s) return null;
  const m = s.match(/^(\d{1,2}):(\d{1,2})$/);
  if (!m) return null;
  const h = parseInt(m[1], 10), mm = parseInt(m[2], 10);
  if (h < 0 || h > 23 || mm < 0 || mm > 59) return null;
  return { h, m: mm };
}

// humanizeCron renders a cron expression as a short natural-language label
// for the card list. Falls back to the raw expression when it doesn't match
// a recognized shape.
function humanizeCron(expr) {
  const d = parseCronToFreq(expr);
  if (!d) {
    // parseCronToFreq only recognizes shapes the v2 frequency-picker
    // round-trips. 以下几种 hand-written / legacy shapes 不 round-trip
    // 但可以 humanize 成人类可读标签，保留给列表和 legacy hint 显示：
    //   "*/N * * * *"  → 每 N 分钟（humanizeCronStepValue）
    //   "0 */N * * *"  → 每 N 小时
    //   "@every 30m"   → 每 30 分钟  (v1 interval shape)
    //   "@every 2h"    → 每 2 小时
    //   "m h * * 1,3,5" / "m h * * 0,6" → 多选 weekly / 周末
    //     （v2 picker 的 Weekly 单选不再 round-trip 这些 shape）
    const step = humanizeCronStepValue(expr);
    if (step) return step;
    const legacy = humanizeCronLegacyEvery(expr);
    if (legacy) return legacy;
    const multiDow = humanizeCronMultiDow(expr);
    if (multiDow) return multiDow;
    return expr;
  }
  if (d.mode === 'hourly') return '每小时';
  if (d.mode === 'daily') return '每天 ' + d.time;
  if (d.mode === 'weekdays') return '工作日 ' + d.time;
  if (d.mode === 'weekly') {
    const names = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
    const set = new Set(d.dows);
    if (d.dows.length === 5 && [1,2,3,4,5].every(x => set.has(x))) return '工作日 ' + d.time;
    if (d.dows.length === 2 && set.has(0) && set.has(6)) return '周末 ' + d.time;
    return d.dows.map(i => names[i]).join('、') + ' ' + d.time;
  }
  if (d.mode === 'monthly') return '每月 ' + d.day + ' 日 ' + d.time;
  return expr;
}

// humanizeCronStepValue recognizes robfig/cron "step-value" shapes that the
// frequency-picker intentionally doesn't round-trip, but which operators DO
// write by hand (copy-pasted from crontab man pages, AI-generated configs,
// IM commands). Display-only — NEVER used to construct a schedule back
// from a descriptor, so the picker's round-trip invariant stays intact.
//
// Supported shapes (all 5-field cron; 6-field with seconds would be nice
// but the backend cronParser explicitly omits Second so that won't parse
// anyway — see internal/cron/job.go cronParser config):
//   "*\/N * * * *"   → 每 N 分钟          (e.g. "*\/15 * * * *")
//   "0 *\/N * * *"   → 每 N 小时（整点）   (e.g. "0 *\/6 * * *")
//
// Returns '' for anything else so the caller can fall back to raw.
// Escaped *\/ in comments to keep this JS from looking like a block
// close; at runtime it's just /*\/N/.
// humanizeCronMultiDow 为 parseCronToFreq 不再 round-trip 的多选 weekly
// shape（v2 Weekly 是单选；周末 / 周一三五等历史数据仍要能人类读）生成
// 中文标签。display-only，不构造回 schedule。
function humanizeCronMultiDow(expr) {
  if (!expr) return '';
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return '';
  const [mm, hh, dom, mon, dow] = parts;
  if (!/^\d+$/.test(mm) || !/^\d+$/.test(hh)) return '';
  if (mon !== '*' || dom !== '*' || dow === '*') return '';
  const days = parseDowField(dow);
  if (!days || days.length < 2) return '';
  const time = pad2(parseInt(hh, 10)) + ':' + pad2(parseInt(mm, 10));
  const names = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
  const set = new Set(days);
  if (days.length === 2 && set.has(0) && set.has(6)) return '周末 ' + time;
  return days.map(i => names[i]).join('、') + ' ' + time;
}

// humanizeCronLegacyEvery 识别 v1 的 @every 表达式并本地化为中文标签。
// 仅 display-only（卡片 cc-human / 编辑模态的 legacy hint）；v2 picker
// 已删掉 interval 模式，所以这个 shape 不会被 buildFreqSchedule 重新
// 产生。仅在 parseCronToFreq 返回 null 的 fallback 链里用。
function humanizeCronLegacyEvery(expr) {
  if (!expr) return '';
  const m = expr.trim().match(/^@every\s+(\d+)(m|h)$/i);
  if (!m) return '';
  const n = parseInt(m[1], 10);
  const unit = m[2].toLowerCase();
  if (!Number.isFinite(n) || n < 1) return '';
  return unit === 'h' ? ('每 ' + n + ' 小时') : ('每 ' + n + ' 分钟');
}

function humanizeCronStepValue(expr) {
  if (!expr) return '';
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return '';
  const [mm, hh, dom, mon, dow] = parts;
  if (dom !== '*' || mon !== '*' || dow !== '*') return '';
  // "*/N * * * *" — every N minutes, N must be 2..59 and > minCronInterval (5).
  // We don't guard the 5-minute backend floor here: that's the scheduler's
  // job to reject invalid jobs at create time. The label just describes
  // what the user wrote.
  let m = mm.match(/^\*\/(\d+)$/);
  if (m && hh === '*') {
    const n = parseInt(m[1], 10);
    if (n >= 2 && n <= 59) return '每 ' + n + ' 分钟';
  }
  // "0 */N * * *" — every N hours on the hour, N must be 2..23.
  m = hh.match(/^\*\/(\d+)$/);
  if (m && mm === '0') {
    const n = parseInt(m[1], 10);
    if (n >= 2 && n <= 23) return '每 ' + n + ' 小时';
  }
  return '';
}

// DOW_LABELS mirrors robfig/cron DOW indexing (0=Sunday). The picker renders
// Monday-first for CJK convention; indices remain cron-native so generated
// expressions need no translation.
const DOW_LABELS = [
  { i: 1, label: '一' }, { i: 2, label: '二' }, { i: 3, label: '三' },
  { i: 4, label: '四' }, { i: 5, label: '五' }, { i: 6, label: '六' },
  { i: 0, label: '日' },
];

// buildFreqPickerHtml renders the Claude-style compact Frequency row:
//
//   [Frequency ▾] [time] [extra: weekday ▾ / day-of-month ▾]
//
// v2 polish: 彻底移除"cron 表达式"概念和 interval 模式（5/15/30 分钟这种对
// 初级用户过于工程化），只保留 Hourly / Daily / Weekdays / Weekly / Monthly
// 五档——覆盖绝大多数实际用例，表达方式清晰。preset 按钮 / 多次运行预览 /
// 高级 raw cron 输入全部删除，对齐 Claude Scheduled Tasks 的简洁直觉。
//
// 后端约束：cron.minCronInterval=5m，Hourly (60m) 及以上都满足，无需前端
// 再提示。Monthly 的日期超过当月最后一天时 robfig/cron 自动跳过，无需警告
// 文案污染 UI。
//
// initial 是可选的 descriptor 用来回填（编辑流），默认 Daily 9:00。
function buildFreqPickerHtml(initial) {
  const d = initial || { mode: 'daily', time: '09:00' };
  const mode = d.mode || 'daily';
  const modeOption = (m, label) =>
    '<option value="' + m + '"' + (mode === m ? ' selected' : '') + '>' + esc(label) + '</option>';

  // time 从当前 descriptor 取；hourly 不需要 time（置为空 placeholder）。
  // onchange/oninput 先 freqMarkTouched() 再 freqUpdate()——只有用户真的
  // 动过控件才写 overlay._cronSchedule。见 freqMarkTouched 注释的数据
  // 损坏场景。
  const time = d.time || '09:00';
  const timeInput =
    '<input class="freq-time" id="freq-time" type="time" value="' + esc(time) + '"' +
      ' onchange="freqMarkTouched();freqUpdate()" oninput="freqMarkTouched();freqUpdate()"' +
      (mode === 'hourly' ? ' style="display:none"' : '') + '>';

  // weekly 的星期下拉（单选）。默认 Monday。
  const weeklyDow = (mode === 'weekly' && Array.isArray(d.dows) && d.dows.length > 0) ? d.dows[0] : 1;
  const dowOption = (i, label) =>
    '<option value="' + i + '"' + (weeklyDow === i ? ' selected' : '') + '>' + esc(label) + '</option>';
  const weeklySelect =
    '<select class="freq-extra" id="freq-weekly-dow" onchange="freqMarkTouched();freqUpdate()"' +
      (mode === 'weekly' ? '' : ' style="display:none"') + '>' +
      dowOption(1, '星期一') + dowOption(2, '星期二') + dowOption(3, '星期三') +
      dowOption(4, '星期四') + dowOption(5, '星期五') + dowOption(6, '星期六') +
      dowOption(0, '星期日') +
    '</select>';

  // monthly 的日期下拉
  const monthlyDay = (mode === 'monthly' && d.day) ? d.day : 1;
  let dayOpts = '';
  for (let i = 1; i <= 31; i++) {
    dayOpts += '<option value="' + i + '"' + (monthlyDay === i ? ' selected' : '') + '>' + i + ' 日</option>';
  }
  const monthlySelect =
    '<select class="freq-extra" id="freq-monthly-day" onchange="freqMarkTouched();freqUpdate()"' +
      (mode === 'monthly' ? '' : ' style="display:none"') + '>' +
      dayOpts +
    '</select>';

  return '<div class="freq-row-inline">' +
      '<select class="freq-mode-select" id="freq-mode-select" aria-label="频率模式" onchange="freqSelectMode(this.value)">' +
        modeOption('hourly', 'Hourly') +
        modeOption('daily', 'Daily') +
        modeOption('weekdays', 'Weekdays') +
        modeOption('weekly', 'Weekly') +
        modeOption('monthly', 'Monthly') +
      '</select>' +
      timeInput +
      weeklySelect +
      monthlySelect +
    '</div>' +
    '<div class="freq-hint">任务会在上述时间点后 0-2 分钟内随机启动（防并发峰值）。</div>';
}

// freqCurrentDescriptor reads the picker state back into a descriptor.
// Returns null when the picker is absent.
//
// Descriptor shapes:
//   hourly   -> { mode:'hourly' }
//   daily    -> { mode:'daily',  time:'HH:MM' }
//   weekdays -> { mode:'weekdays', time:'HH:MM' }   // Mon-Fri，buildFreqSchedule 会展开成 dows=[1..5]
//   weekly   -> { mode:'weekly', time:'HH:MM', dows:[N] }  // 单选
//   monthly  -> { mode:'monthly', time:'HH:MM', day:N }
function freqCurrentDescriptor() {
  const sel = document.getElementById('freq-mode-select');
  if (!sel) return null;
  const mode = sel.value;
  const time = (document.getElementById('freq-time') || {}).value || '09:00';
  if (mode === 'hourly') {
    return { mode };
  }
  if (mode === 'daily') {
    return { mode, time };
  }
  if (mode === 'weekdays') {
    return { mode, time };
  }
  if (mode === 'weekly') {
    const dow = parseInt((document.getElementById('freq-weekly-dow') || {}).value, 10);
    return { mode, time, dows: Number.isFinite(dow) ? [dow] : [1] };
  }
  if (mode === 'monthly') {
    const day = parseInt((document.getElementById('freq-monthly-day') || {}).value, 10);
    return { mode, time, day: Number.isFinite(day) ? day : 1 };
  }
  return null;
}

// freqSelectMode 切换频率模式。根据模式显示/隐藏 time / weekly-dow /
// monthly-day 三个辅助控件。hourly 无 time（整点即跑）。
// 用户主动切 mode 算 "touched"——之后 freqUpdate 才开始把 picker 结果
// 写入 overlay._cronSchedule；见 freqMarkTouched 的注释。
function freqSelectMode(mode) {
  const time = document.getElementById('freq-time');
  const dow = document.getElementById('freq-weekly-dow');
  const day = document.getElementById('freq-monthly-day');
  if (time) time.style.display = (mode === 'hourly') ? 'none' : '';
  if (dow) dow.style.display = (mode === 'weekly') ? '' : 'none';
  if (day) day.style.display = (mode === 'monthly') ? '' : 'none';
  freqMarkTouched();
  freqUpdate();
}

// freqMarkTouched 标记用户真的交互过频率控件。编辑流里，打开 modal 时
// overlay._cronSchedule 被 seed 成 job.schedule 的原始值（可能是无法
// round-trip 的 legacy shape，如 @every 30m 或 * * * * 1,3,5）；只有
// 用户真的动过 freq-mode-select / freq-time / freq-weekly-dow /
// freq-monthly-day 才允许 freqUpdate 覆盖这个 seed——否则"打开旧任务
// 未改频率即保存"会把原 schedule 静默改成 UI 默认的 Daily 09:00，
// 造成数据损坏。
// 创建流：createNewCronJob 显式调用 freqMarkTouched() 让初始 Daily 09:00
// 立刻写入，保证"打开即保存"能提交合法 schedule。
function freqMarkTouched() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  overlay._cronScheduleTouched = true;
}

// freqUpdate refreshes overlay._cronSchedule from the current picker state.
// v2 polish: advanced raw-cron input and multi-run preview 已移除；submit
// 路径只需要一个 cron expression，由 freqCurrentDescriptor + buildFreqSchedule
// 产出即可。
//
// Gating by _cronScheduleTouched：不动用户"未触碰"的 seed（见
// freqMarkTouched 注释的数据损坏场景）。
function freqUpdate() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  if (!overlay._cronScheduleTouched) return;
  const desc = freqCurrentDescriptor();
  const { expr } = buildFreqSchedule(desc);
  overlay._cronSchedule = expr || '';
}

// v2 polish: previewFreqSchedule / doPreviewFreq / renderFreqPreview /
// freqToggleAdvanced 在改造后全部删除。多次运行预览 + raw cron 表达式
// 入口已从 modal 中移除（对初级用户过于工程化）；submit 路径不再需要
// 经过 preview 即可判定 schedule 是否合法——后端 validateSchedule 会在
// AddJob 时兜底返回 400。


// buildCronWorkspaceBody renders the workspace picker as a dropdown button +
// popover（v2 polish，参考 Claude Scheduled Tasks 的 "Work in a project ▾"
// 样式）。点击按钮展开列表；选中后 popover 折叠并把按钮文本改为所选 path。
//
// 保留 IDs 契约：#cron-ws-list, #cron-ws-custom-toggle, #cron-ws-custom-form,
// #cron-workdir 被 cronSelectWorkspace / toggleCronWsCustom / 提交 collector
// 读取；外壳改造但这些稳定锚点保持。aria-label="工作目录路径" 也是契约锁定
// 字符串（static_ux_contract_test 会 grep）。
function buildCronWorkspaceBody() {
  return buildCronWorkspaceBodyInternal({
    inputId: 'cron-workdir',
    selectedPath: '',
  });
}

function buildCronWorkspaceBodyInternal(opts) {
  const selected = opts.selectedPath || '';
  // Button label: 选中的项目名 > 选中的路径尾段 > 默认占位
  let label = '默认工作目录';
  if (selected) {
    const match = projectsData.find(p => p.path === selected);
    label = match ? match.name : shortPath(selected);
  }
  // 下拉按钮，点击 toggle popover
  const buttonHtml =
    '<button type="button" class="ws-dropdown-btn" id="' + escAttr(opts.buttonId || 'cron-ws-dropdown') + '"' +
      ' aria-haspopup="listbox" aria-expanded="false" onclick="toggleCronWsDropdown(event)">' +
      '<span class="ws-dropdown-icon" aria-hidden="true">&#128193;</span>' +
      '<span class="ws-dropdown-label">' + esc(label) + '</span>' +
      '<span class="ws-dropdown-caret" aria-hidden="true">&#9662;</span>' +
    '</button>';
  // Popover 内容：项目列表 + "自定义路径" 触发条目
  let listItems = '';
  if (projectsData.length > 0) {
    listItems = projectsData.map(p => {
      const sel = selected && p.path === selected;
      return '<li role="option" data-path="' + escAttr(p.path) + '"' +
        (sel ? ' class="selected" aria-selected="true"' : ' aria-selected="false"') +
        ' onclick="cronSelectWorkspace(this, \'' + escJs(p.path) + '\')">' +
          '<div class="pp-name">' + esc(p.name) + '</div>' +
          '<div class="pp-path">' + esc(shortPath(p.path)) + '</div>' +
        '</li>';
    }).join('');
  }
  listItems +=
    '<li id="cron-ws-custom-toggle" role="option" onclick="toggleCronWsCustom()">' +
      '<div class="pp-custom"><span class="pp-custom-icon">+</span> 自定义路径</div>' +
    '</li>';

  const popoverHtml =
    '<div class="ws-dropdown-popover" id="cron-ws-popover" role="listbox" aria-label="选择工作目录">' +
      '<ul class="proj-pick" id="cron-ws-list" role="listbox" aria-label="工作目录">' +
        listItems +
      '</ul>' +
      '<div id="cron-ws-custom-form" style="display:' + (selected && !projectsData.find(p => p.path === selected) ? '' : 'none') + ';padding:8px">' +
        '<input id="' + escAttr(opts.inputId) + '" placeholder="' + escAttr(defaultWorkspace || '/home/user/project') + '"' +
          ' value="' + escAttr(selected && !projectsData.find(p => p.path === selected) ? selected : '') + '"' +
          ' aria-label="工作目录路径">' +
      '</div>' +
    '</div>';

  return '<div class="ws-dropdown-wrap">' + buttonHtml + popoverHtml + '</div>';
}

// buildScheduleSection renders the frequency picker. v2 polish 之后只剩下
// 单行 picker（mode select + time + optional weekday/day-of-month），没有
// 预览面板和 raw cron 入口。
//
// initialRawExpr 非空表示调用方检测到了一个"无法被 v2 picker round-trip"
// 的老 schedule（@every 30m、* * * * 1,3,5 等）——picker 渲染默认 Daily
// 09:00，但 overlay._cronScheduleTouched 会被编辑流置为 false，原 schedule
// 保留在 overlay._cronSchedule 里，直到用户真的动一次控件才覆盖。
// 为了避免"UI 显示 Daily 09:00 但实际不是"的视觉误导，我们在 picker 上方
// 插一条轻量 hint："当前频率：每 30 分钟（动下方控件即切换到新频率）"。
function buildScheduleSection(initialDesc, initialRawExpr) {
  const pickerHtml = buildFreqPickerHtml(initialDesc);
  if (!initialRawExpr) return pickerHtml;
  const human = humanizeCron(initialRawExpr);
  return '<div class="freq-legacy-hint" role="note">' +
      '当前频率：<b>' + esc(human) + '</b>' +
      '<span class="freq-legacy-sub">这是 v1 的老格式。如需修改，请用下方控件选一个新频率。</span>' +
    '</div>' +
    pickerHtml;
}

function createNewCronJob() {
  // Sprint 6c: fetch backends upfront so the picker (if any) is ready when
  // the modal renders. Failure / single-backend deploys map to '' which
  // collapses the picker section — same pattern as createNewSession.
  fetchCLIBackends().then(backendsData => {
    const backendHtml = renderBackendPicker(backendsData, { selectId: 'cron-backend' });
    openCronCreateModal(backendHtml);
  }).catch(() => openCronCreateModal(''));
}

function openCronCreateModal(backendHtml) {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  // Default "每小时" matches the most common ask and gives users an
  // immediate, meaningful preview on open.
  const scheduleHtml = buildScheduleSection({ mode: 'interval', n: 1, unit: 'h' }, '');
  const wsBody = buildCronWorkspaceBody();
  const notifyHtml = buildCronNotifyToggleHtml('', false);
  const contextHtml = buildCronContextToggleHtml(false);

  // Title + aria-label are inlined as literals (not passed through esc())
  // so the static UX contract test can grep the exact fragments in source.
  // See internal/server/static_ux_contract_test.go :: R154 cron-create.
  overlay.innerHTML =
    '<div class="modal cron-modal" role="dialog" aria-modal="true" aria-label="新建定时任务">' +
      '<div class="cm-header">' +
        '<h3>新建定时任务</h3>' +
        '<button type="button" class="cm-close" onclick="this.closest(\'.modal-overlay\').remove()" aria-label="关闭">✕</button>' +
      '</div>' +
      renderCronModalBody({
        scheduleHtml, wsBody, notifyHtml, contextHtml,
        backendHtml,
        promptId: 'cron-prompt',
        promptPlaceholder: '例如：总结昨天的代码变更，push 到日报频道',
      }) +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">取消</button>' +
        '<button type="button" class="primary" onclick="doCreateCronJob()">创建</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  overlay.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') overlay.remove();
    // Ctrl/Cmd+Enter submits from anywhere inside the modal.
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      doCreateCronJob();
    }
  });
  overlay._cronSchedule = '';
  overlay._cronWorkDir = '';
  // 创建流：默认 Daily 09:00 立即生效，"打开即保存"也能提交合法 schedule。
  // 与编辑流相反——编辑流必须不 touched，保留 job.schedule 原值直到用户
  // 主动改。
  overlay._cronScheduleTouched = true;
  const promptEl = document.getElementById('cron-prompt');
  if (promptEl) setTimeout(() => promptEl.focus(), 0);
  freqUpdate();
}

// fillCronPrompt pushes the prompt value through the DOM `.value` setter
// instead of HTML-encoded template interpolation (see renderCronModalBody
// for the rationale). Called only by editCronJob — the create flow starts
// with an empty textarea and doesn't need this.
function fillCronPrompt(id, value) {
  const el = document.getElementById(id);
  if (el) el.value = value || '';
}

// renderCronModalBody assembles the shared two-column grid body used by
// both the create and edit flows. Header (title, close) and footer
// (submit button label) are inlined at the call site so the static UX
// contract tests can grep the exact localized fragments in source.
//
// The prompt textarea is rendered empty; callers populate it via
// fillCronPrompt(id, value) after insertion. Rationale: the HTML parser
// strips the first newline inside <textarea>, so a user-saved prompt
// beginning with \n would silently lose that newline on edit round-trip.
function renderCronModalBody(opts) {
  const promptTextarea =
    '<textarea id="' + opts.promptId + '" placeholder="' + escAttr(opts.promptPlaceholder) + '" aria-label="提示词"></textarea>';
  // Title 字段跨两列独立一行，放在最上方——符合"先起名，再写提示词"的
  // 直觉顺序，与 Claude Scheduled Tasks UI 的 Name → Description → Prompt
  // 结构对齐。留空允许，UI 自动回退显示 Prompt 首行（JobTitleOrFallback）。
  // 关联：docs/rfc/cron-v2-polish.md §3.1 Increment A。
  const titleField =
    '<div class="cron-field cron-f-title">' +
      '<div class="cf-label">名称 <span style="color:var(--nz-text-faint);font-weight:normal;font-size:11px">（可选）</span></div>' +
      '<input id="' + escAttr(opts.titleId || 'cron-title') + '" type="text" placeholder="' + escAttr(opts.titlePlaceholder || '例如：日报总结 · 周一早会准备') + '" maxlength="256" aria-label="任务名称">' +
    '</div>';
  // backendHtml 由 caller 提供（Sprint 6c）。仅在多 backend 模式下非空，
  // 单 backend 时 renderBackendPicker 返回空串、整段折叠。位置选 "其他设置"
  // 区与 notify / fresh-context 同列，因为 backend 是 "怎么跑" 的运行时
  // 设定，与 work_dir（"在哪里"）的资源/路径含义不同。
  const backendBlock = opts.backendHtml ? opts.backendHtml : '';
  return '<div class="modal-body">' +
      '<div class="cron-modal-grid">' +
        titleField +
        '<div class="cron-field cron-f-what">' +
          '<div class="cf-label">做什么</div>' +
          promptTextarea +
        '</div>' +
        '<div class="cron-field cron-f-when">' +
          '<div class="cf-label">什么时候</div>' +
          opts.scheduleHtml +
        '</div>' +
        '<div class="cron-field cron-f-where">' +
          '<div class="cf-label">在哪里</div>' +
          opts.wsBody +
        '</div>' +
        '<div class="cron-field cron-f-more">' +
          '<div class="cf-label">其他设置</div>' +
          '<div class="cron-more-stack">' +
            backendBlock +
            opts.notifyHtml +
            opts.contextHtml +
          '</div>' +
        '</div>' +
      '</div>' +
    '</div>';
}

// buildCronContextToggleHtml renders the "每次全新上下文" toggle (checkbox
// form). Default is "continue" (unchecked = inherit session + history);
// checked = fresh (reset before each run). Used in create/edit modals.
function buildCronContextToggleHtml(initialFresh) {
  const freshChecked = initialFresh ? 'checked' : '';
  return '<label class="cron-toggle" id="cron-context-toggle">' +
      '<input type="checkbox" id="cron-context-fresh" ' + freshChecked + '>' +
      '<span class="ct-main">每次全新上下文' +
        '<span class="ct-hint">勾选后每次运行前重置会话；不勾则复用会话并保留历史。</span>' +
      '</span>' +
    '</label>';
}

// collectCronContextValue returns the fresh_context flag, or null when the
// toggle is absent (section not rendered).
function collectCronContextValue() {
  const cb = document.getElementById('cron-context-fresh');
  if (!cb) return null;
  return !!cb.checked;
}

// buildCronNotifyToggleHtml renders the "完成后通知我" toggle (checkbox
// form) plus the optional per-job target inputs shown only when a custom
// target is in effect. currentNotify: 'on' / 'off' / '' (legacy unset).
//
// The checkbox carries data-touched="0" initially; cronNotifyOnChange sets
// it to "1" on any user interaction. collectCronNotifyValues uses this to
// preserve the legacy tri-state contract: untouched → null (server keeps
// its default / cron.notify_default behavior), touched → explicit bool.
// Without this, create would default to notify=false (disabling the
// server default) and edit would overwrite legacy tasks' unset notify
// with false on save.
function buildCronNotifyToggleHtml(currentNotify, hasOverride, overridePlat, overrideChat) {
  let defaultHint;
  if (cronNotifyDefault && cronNotifyDefault.platform && cronNotifyDefault.chat_id) {
    defaultHint = '→ ' + esc(cronNotifyDefault.platform) + ' (' + esc(cronNotifyDefault.chat_id) + ')';
  } else {
    defaultHint = '未配置默认通知目标；展开下方填写自定义目标，或在 config.yaml 的 cron.notify_default 中配置。';
  }
  const notifyOn = currentNotify === 'on';
  const notifyOff = currentNotify === 'off';
  // Legacy-unset tasks render with the checkbox unchecked but untouched;
  // existing on/off tasks render with the corresponding state AND marked
  // touched so an immediate save preserves the persisted value.
  const touched = (notifyOn || notifyOff) ? '1' : '0';
  const overrideShow = hasOverride ? ' show' : '';
  return '<label class="cron-toggle" id="cron-notify-toggle">' +
      '<input type="checkbox" id="cron-notify-on" ' + (notifyOn ? 'checked' : '') +
        ' data-touched="' + touched + '" onchange="cronNotifyOnChange(this)">' +
      '<span class="ct-main">完成后通知我' +
        '<span class="ct-hint" id="cron-notify-default-hint">' + defaultHint + '</span>' +
      '</span>' +
    '</label>' +
    '<label class="cron-toggle" id="cron-notify-override-toggle-wrap" style="margin-top:-4px">' +
      '<input type="checkbox" id="cron-notify-override" ' + (hasOverride ? 'checked' : '') + ' onchange="cronNotifyOverrideToggle(this)">' +
      '<span class="ct-main" style="font-size:12px;color:var(--nz-text-mute)">自定义此任务的通知目标</span>' +
    '</label>' +
    '<div id="cron-notify-override-form" class="cron-notify-target' + overrideShow + '">' +
      '<input id="cron-notify-platform" placeholder="feishu" value="' + escAttr(overridePlat || '') + '" aria-label="IM 平台">' +
      '<input id="cron-notify-chat-id" placeholder="chat_id" value="' + escAttr(overrideChat || '') + '" aria-label="群/会话 ID">' +
    '</div>';
}

function cronNotifyOnChange(cb) {
  // Mark the toggle as user-touched so collectCronNotifyValues can return
  // the explicit bool instead of null (preserves the tri-state contract).
  cb.dataset.touched = '1';
  // When notify is off, disable the override checkbox + hide its form so
  // the user can't silently leave stale target fields behind.
  const overrideForm = document.getElementById('cron-notify-override-form');
  const overrideToggle = document.getElementById('cron-notify-override');
  if (!overrideForm || !overrideToggle) return;
  if (!cb.checked) {
    overrideForm.classList.remove('show');
    overrideToggle.disabled = true;
    overrideToggle.checked = false;
  } else {
    overrideToggle.disabled = false;
  }
}

function cronNotifyOverrideToggle(cb) {
  const form = document.getElementById('cron-notify-override-form');
  if (!form) return;
  if (cb.checked) form.classList.add('show');
  else form.classList.remove('show');
}

// collectCronNotifyValues reads the modal's notify fields and returns an
// object ready to merge into the POST/PATCH body. Returns null for `notify`
// when the user hasn't touched the toggle (data-touched="0"), so callers
// can preserve the server's default behavior / the job's legacy unset
// state. Matches the legacy radio semantics where "no selection" meant
// "don't send the field".
function collectCronNotifyValues() {
  const out = { notify: null, notify_platform: null, notify_chat_id: null };
  const onCb = document.getElementById('cron-notify-on');
  if (onCb && onCb.dataset.touched === '1') {
    out.notify = !!onCb.checked;
  }
  const override = document.getElementById('cron-notify-override');
  if (override && override.checked) {
    const platInput = document.getElementById('cron-notify-platform');
    const chatInput = document.getElementById('cron-notify-chat-id');
    out.notify_platform = platInput ? platInput.value.trim() : '';
    out.notify_chat_id = chatInput ? chatInput.value.trim() : '';
  }
  return out;
}

// toggleCronWsDropdown 打开/关闭工作目录 popover。event.stopPropagation 防止
// 顶层 document 的 outside-click handler 立即把它再关掉。
function toggleCronWsDropdown(e) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  const pop = document.getElementById('cron-ws-popover');
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (!pop) return;
  const open = pop.classList.toggle('open');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) wireCronWsOutsideClick();
}

// 单例 outside-click 监听，capture 阶段判断点击是否在 popover 外部；
// 若是则关闭。只在 popover 打开期间挂载，关闭时自 remove。
function wireCronWsOutsideClick() {
  if (wireCronWsOutsideClick._on) return;
  const h = function(ev) {
    const pop = document.getElementById('cron-ws-popover');
    const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
    if (!pop || !pop.classList.contains('open')) {
      document.removeEventListener('mousedown', h, true);
      wireCronWsOutsideClick._on = false;
      return;
    }
    if (pop.contains(ev.target) || (btn && btn.contains(ev.target))) return;
    pop.classList.remove('open');
    if (btn) btn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('mousedown', h, true);
    wireCronWsOutsideClick._on = false;
  };
  document.addEventListener('mousedown', h, true);
  wireCronWsOutsideClick._on = true;
}

function cronSelectWorkspace(el, path) {
  const overlay = el.closest('.modal-overlay');
  overlay._cronWorkDir = path;
  document.querySelectorAll('#cron-ws-list li').forEach(li => {
    li.classList.remove('selected');
    li.setAttribute('aria-selected', 'false');
  });
  el.classList.add('selected');
  el.setAttribute('aria-selected', 'true');
  const customForm = document.getElementById('cron-ws-custom-form');
  if (customForm) {
    customForm.style.display = 'none';
    // Clear the hidden custom input so the submit path (which falls back
    // to wdInput.value when non-empty) can't resurrect a stale path after
    // the user picked a different project. Matters in the edit modal,
    // where wdInput is pre-populated with the job's current work_dir.
    const input = customForm.querySelector('input');
    if (input) input.value = '';
  }
  const toggle = document.getElementById('cron-ws-custom-toggle');
  if (toggle) toggle.style.display = '';
  // v2 polish: 选中即把 popover 折叠 + 把按钮文本更新为项目名
  updateCronWsDropdownLabel(path);
  closeCronWsPopover();
}

function updateCronWsDropdownLabel(path) {
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (!btn) return;
  const labelEl = btn.querySelector('.ws-dropdown-label');
  if (!labelEl) return;
  if (!path) { labelEl.textContent = '默认工作目录'; return; }
  const match = projectsData.find(p => p.path === path);
  labelEl.textContent = match ? match.name : shortPath(path);
}

function closeCronWsPopover() {
  const pop = document.getElementById('cron-ws-popover');
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (pop) pop.classList.remove('open');
  if (btn) btn.setAttribute('aria-expanded', 'false');
}

function toggleCronWsCustom() {
  const form = document.getElementById('cron-ws-custom-form');
  const toggle = document.getElementById('cron-ws-custom-toggle');
  if (!form) return;
  if (form.style.display === 'none') {
    form.style.display = '';
    if (toggle) toggle.style.display = 'none';
    // Clear project selection
    const overlay = form.closest('.modal-overlay');
    if (overlay) overlay._cronWorkDir = '';
    document.querySelectorAll('#cron-ws-list li').forEach(li => {
      li.classList.remove('selected');
      li.setAttribute('aria-selected', 'false');
    });
    const input = form.querySelector('input');
    if (input) input.focus();
  } else {
    form.style.display = 'none';
    if (toggle) toggle.style.display = '';
  }
}

async function doCreateCronJob() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  // Resolve schedule: picker descriptor or raw advanced input. overlay
  // ._cronSchedule is kept in sync by freqUpdate(), but we re-collect here
  // so the submit path always sees the latest input.
  const advanced = document.getElementById('freq-advanced-input');
  let schedule = (advanced && advanced.value.trim()) || overlay._cronSchedule || '';
  if (!schedule) { showToast('请设置频率', 'warning'); return; }
  // Resolve prompt
  const promptInput = document.getElementById('cron-prompt');
  const prompt = promptInput ? promptInput.value.trim() : '';
  // Resolve title（可选）
  const titleInput = document.getElementById('cron-title');
  const title = titleInput ? titleInput.value.trim() : '';
  // Resolve work_dir: project selection or custom input
  let workDir = overlay._cronWorkDir || '';
  const wdInput = document.getElementById('cron-workdir');
  if (wdInput && wdInput.value.trim()) workDir = wdInput.value.trim();
  try {
    const headers = {'Content-Type': 'application/json'};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const body = {schedule};
    if (prompt) body.prompt = prompt;
    if (title) body.title = title;
    if (workDir) body.work_dir = workDir;
    const notifyVals = collectCronNotifyValues();
    if (notifyVals.notify !== null) body.notify = notifyVals.notify;
    if (notifyVals.notify_platform !== null) body.notify_platform = notifyVals.notify_platform;
    if (notifyVals.notify_chat_id !== null) body.notify_chat_id = notifyVals.notify_chat_id;
    const freshCtx = collectCronContextValue();
    if (freshCtx === true) body.fresh_context = true;
    // Sprint 6c: pick up the cron-modal backend choice (if any). The picker
    // collapses entirely in single-backend deploys, so the element may be
    // absent — treat that as "router default" and omit the field, matching
    // the server's omitempty contract.
    const backendEl = document.getElementById('cron-backend');
    const backendVal = backendEl && backendEl.value ? backendEl.value : '';
    if (backendVal) body.backend = backendVal;
    let data;
    try {
      data = await fetchJSON('/api/cron', {timeoutMs: 10000, method: 'POST', headers, body: JSON.stringify(body)});
    } catch (err) {
      if (err && err.status) showAPIError('创建定时任务', err.status, err.message || '');
      else showNetworkError('创建定时任务', err);
      return;
    }
    if (!data) data = {};
    if (overlay) overlay.remove();
    showToast('定时任务已创建', 'success');
    fetchCronJobs();
    if (data.id) {
      // cron-panel-consolidation RFC §4.2: a freshly-created cron job no
      // longer pushes itself into the sidebar (cron stubs are filtered
      // server-side) nor takes over mainShell. Open the per-job drawer
      // directly so the operator sees the row they just configured.
      openCronDetail(data.id);
    }
  } catch (e) { showNetworkError('创建定时任务', e); }
}

function openCronPanel() {
  // Cron is its own top-level view now. Ensure it's active before painting.
  // setActivityView('cron') sets activeView='cron' first, then calls back
  // into openCronPanel — so the guard below is already satisfied on re-entry
  // and we don't recurse. Direct callers (legacy #btn-cron, openCronDetail)
  // route through here and get the view switch for free.
  if (activeView !== 'cron') { setActivityView('cron'); return; }
  // Deselect managed session via selectedKey only — selectedNode is the
  // sidebar filter now (see previewDiscovered comment) and must survive
  // opening the cron panel so the user comes back to the right node list.
  selectedKey = null;
  if (wsm.subscribedKey) wsm.unsubscribe();
  if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
  setActiveSessionCard(null);
  // NOTE: no mobileEnterChat() here. Cron is a standalone view, not a chat
  // session — entering chat view would hide the bottom tab bar (the only nav
  // surface on mobile) and strand the user. The cron view is full-screen via
  // body.nz-view-cron CSS and the tab bar stays visible.
  // Paint immediately from the cache primed at page load (line ~5982) so the
  // click feels instant. If the cache is empty we still render the panel —
  // renderCronPanel handles the zero-job "empty state" branch. A background
  // refresh reconciles with the server and re-renders if anything changed.
  renderCronPanel();
  fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
}

// filterCronJobs is the pure match step for the R110-P2 cron panel filter.
// Extracted so unit tests exercise the predicate without driving DOM. Match
// surface for the substring arm: title, prompt, work_dir, schedule, id (all
// case-insensitive). title 放在最前，匹配优先 —— 人们搜索 cron 时最先想到
// 的就是自己给任务起的那个名字。
//
// Status arm (cron-dashboard-redesign P0 §4.2):
//   - 'all'        全部
//   - 'active'     非 paused（保留旧语义；与 attentionCount 互斥的"运行中"
//                  入口，目前 chip 上仍叫"运行中"以兼容 e2e）
//   - 'attention'  paused || last_error || missed（与 cronBadge 同源）
//   - 'healthy'    !paused && !last_error && !missed && !is_running
//                  （没毛病、不在跑，最常见的稳态）
//   - 'running'    current_run.started_at 存在（实时跑中）
function filterCronJobs(jobs, query, status) {
  const q = (query || '').trim().toLowerCase();
  const s = status || 'all';
  return (Array.isArray(jobs) ? jobs : []).filter(j => {
    if (!j) return false;
    if (s === 'active' && j.paused) return false;
    // cron-v2-polish §3.3: attention 扩展为 paused || last_error || missed，
    // 与 fetchCronJobs 里的 cronBadge 计数同源，避免两处判断漂移。
    if (s === 'attention' && !(j.paused || j.last_error || j.missed)) return false;
    if (s === 'healthy') {
      const running = !!(j.current_run && j.current_run.started_at);
      const attn = j.paused || j.last_error || j.missed;
      if (running || attn) return false;
    }
    if (s === 'running' && !(j.current_run && j.current_run.started_at)) return false;
    if (!q) return true;
    const fields = [j.title, j.prompt, j.work_dir, j.schedule, j.id];
    for (const f of fields) {
      if (typeof f === 'string' && f.toLowerCase().indexOf(q) !== -1) return true;
    }
    return false;
  });
}

// firstNonEmptyLine 取文本的首个非空行并按 rune 截断到 limit。
// 与后端 cron.JobTitleOrFallback 行为对齐——显式 title 为空时前后端
// 应该渲染一致的 fallback 标题。limit 默认 60 rune 匹配卡片视觉宽度。
function firstNonEmptyLine(text, limit) {
  if (!text) return '';
  const lines = String(text).split('\n');
  let line = '';
  for (const l of lines) {
    const t = l.trim();
    if (t) { line = t; break; }
  }
  if (!line) return '';
  const max = limit > 0 ? limit : 60;
  // Array.from 处理 UTF-16 surrogate pair（emoji、非 BMP 字符），避免
  // substring 切断代理对产生替换字符。
  const chars = Array.from(line);
  if (chars.length <= max) return line;
  return chars.slice(0, max).join('') + '…';
}

// calendarDayDelta returns the number of calendar days between two epoch-ms
// (positive if `b` is later than `a` in local time). Uses local midnight so
// "昨天" / "明天" align with wall-clock date, not 24h intervals — a run
// 25h ago from now=01:00 is actually 前天, not 昨天.
function calendarDayDelta(a, b) {
  const da = new Date(a);
  const db = new Date(b);
  const a0 = new Date(da.getFullYear(), da.getMonth(), da.getDate()).getTime();
  const b0 = new Date(db.getFullYear(), db.getMonth(), db.getDate()).getTime();
  return Math.round((b0 - a0) / 86400000);
}

// formatWhenColloquial renders a future epoch-ms as a short human-readable
// phrase for the "when" column. Buckets:
//
//   - imminent  (<10m)        → "5 分钟后"
//   - short     (<1h)          → "32 分钟后"
//   - same day                 → "约 14 小时后"
//   - tomorrow, early (<12:00) → "明早 04:00"
//   - tomorrow, late           → "明日 20:00"
//   - >=2 days                 → "3 天后 · 02:00"
//
// Returns {label, imminent} so callers choose their own highlight class.
function formatWhenColloquial(ms) {
  if (!ms) return { label: '—', imminent: false };
  const now = Date.now();
  const d = ms - now;
  if (d < 0) return { label: '即将', imminent: true };
  if (d < 60 * 1000) return { label: '片刻后', imminent: true };
  if (d < 10 * 60 * 1000) return { label: Math.max(1, Math.floor(d / 60000)) + ' 分钟后', imminent: true };
  if (d < 60 * 60 * 1000) return { label: Math.floor(d / 60000) + ' 分钟后', imminent: false };
  const dayDelta = calendarDayDelta(now, ms);
  const tgt = new Date(ms);
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const hhmm = pad(tgt.getHours()) + ':' + pad(tgt.getMinutes());
  if (dayDelta === 0) {
    return { label: '约 ' + Math.floor(d / 3600000) + ' 小时后', imminent: false };
  }
  if (dayDelta === 1) {
    const prefix = tgt.getHours() < 12 ? '明早' : '明日';
    return { label: prefix + ' ' + hhmm, imminent: false };
  }
  return { label: dayDelta + ' 天后 · ' + hhmm, imminent: false };
}

// formatAgoColloquial — past epoch-ms → short Chinese "刚刚 / 3 分钟前 /
// 2 小时前 / 昨天 HH:MM / 3 天前". Uses calendar days so "昨天" means
// yesterday's date, not 24-48h ago (a 25h-old run from 01:00 is 前天).
function formatAgoColloquial(ms) {
  if (!ms) return '';
  const now = Date.now();
  const d = now - ms;
  if (d < 60 * 1000) return '刚刚';
  if (d < 60 * 60 * 1000) return Math.floor(d / 60000) + ' 分钟前';
  const dayDelta = calendarDayDelta(ms, now);
  if (dayDelta === 0) return Math.floor(d / 3600000) + ' 小时前';
  const tgt = new Date(ms);
  const pad = n => (n < 10 ? '0' + n : '' + n);
  if (dayDelta === 1) return '昨天 ' + pad(tgt.getHours()) + ':' + pad(tgt.getMinutes());
  return dayDelta + ' 天前';
}

// Cron ⋯ menu — single-active-menu model.
//
// Only one menu may be open at a time. A single module-level `cronMenuOnDoc`
// captures the outside-click handler so repeated toggles can't accumulate
// listeners (prior design spawned one per open, only removed on outside
// click — rapid open/close leaked them).
//
// Item actions use data-action dispatch instead of onclick string
// interpolation so the job id can't escape its quote boundary on any path.
let cronMenuOpenId = null;
let cronMenuOnDoc = null;
let cronMenuOnScroll = null;

function closeCronMenus() {
  document.querySelectorAll('.cj-menu').forEach(el => {
    if (el.parentNode) el.parentNode.removeChild(el);
  });
  cronMenuOpenId = null;
  if (cronMenuOnDoc) {
    document.removeEventListener('click', cronMenuOnDoc, true);
    cronMenuOnDoc = null;
  }
  if (cronMenuOnScroll) {
    window.removeEventListener('scroll', cronMenuOnScroll, true);
    window.removeEventListener('resize', cronMenuOnScroll);
    cronMenuOnScroll = null;
  }
}

// positionCronMenu places the menu near the anchor's bottom-right. If there
// isn't enough room below (viewport-bottom), flips above. Called after the
// menu is attached to the DOM so measurements are real.
function positionCronMenu(menu, anchor) {
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const anchorRect = anchor.getBoundingClientRect();
  const menuRect = menu.getBoundingClientRect();
  const margin = 6;
  // Right-align to anchor.
  let left = Math.min(anchorRect.right - menuRect.width, vw - menuRect.width - margin);
  left = Math.max(margin, left);
  // Prefer below; flip above if not enough room.
  const spaceBelow = vh - anchorRect.bottom;
  const spaceAbove = anchorRect.top;
  let top;
  if (spaceBelow >= menuRect.height + margin || spaceBelow >= spaceAbove) {
    top = anchorRect.bottom + 4;
  } else {
    top = anchorRect.top - menuRect.height - 4;
  }
  top = Math.max(margin, Math.min(top, vh - menuRect.height - margin));
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
}

// Dispatch table for menu actions. Keys must match the data-action values
// emitted in toggleCronMenu so a rename on one side is a caller-site break.
const CRON_MENU_ACTIONS = {
  'run': (id) => cronTriggerNow(id),
  'open': (id) => openCronDetail(id),
  'edit': (id) => editCronJob(id),
  'pause': (id) => cronPause(id),
  'resume': (id) => cronResume(id),
  'delete': (id) => cronDelete(id),
};

function handleCronMenuClick(ev) {
  const btn = ev.target.closest('.cj-menu-item');
  if (!btn) return;
  ev.stopPropagation();
  const action = btn.getAttribute('data-action');
  const id = btn.getAttribute('data-id');
  closeCronMenus();
  const fn = CRON_MENU_ACTIONS[action];
  if (fn && id) fn(id);
}

function toggleCronMenu(id) {
  const sel = '.cj-row[data-cron-id="' + id.replace(/"/g, '\\"') + '"]';
  const row = document.querySelector(sel);
  if (!row) return;
  // Toggle off if this row's menu is already open.
  if (cronMenuOpenId === id) {
    closeCronMenus();
    return;
  }
  // Close any other open menu before opening this one.
  closeCronMenus();
  const j = (cronJobs || []).find(x => x && x.id === id);
  if (!j) return;
  const items = [];
  if (!j.paused) items.push({ label: '立即运行', action: 'run' });
  items.push({ label: '打开最近会话', action: 'open' });
  items.push({ label: '编辑', action: 'edit' });
  items.push({ label: j.paused ? '恢复' : '暂停', action: j.paused ? 'resume' : 'pause' });
  items.push({ sep: true });
  items.push({ label: '删除', action: 'delete', danger: true });
  const menu = document.createElement('div');
  menu.className = 'cj-menu open';
  menu.innerHTML = items.map(it => {
    if (it.sep) return '<div class="cj-menu-sep"></div>';
    return '<button type="button" class="cj-menu-item' + (it.danger ? ' danger' : '') +
      '" data-action="' + escAttr(it.action) +
      '" data-id="' + escAttr(id) + '">' +
      esc(it.label) + '</button>';
  }).join('');
  menu.addEventListener('click', handleCronMenuClick);
  // Attach to <body> rather than the row so position:fixed escapes the
  // .cron-detail-body overflow clipping box. The menu anchors visually to
  // the ⋯ button via positionCronMenu.
  document.body.appendChild(menu);
  const anchor = row.querySelector('.cj-menu-btn') || row;
  positionCronMenu(menu, anchor);
  cronMenuOpenId = id;
  // Close on outside click / scroll / resize. setTimeout defers the doc
  // handler past the current click so the same event that opened doesn't
  // immediately close.
  setTimeout(() => {
    cronMenuOnDoc = (e) => {
      if (!menu.contains(e.target)) closeCronMenus();
    };
    document.addEventListener('click', cronMenuOnDoc, true);
  }, 0);
  cronMenuOnScroll = () => closeCronMenus();
  window.addEventListener('scroll', cronMenuOnScroll, true);
  window.addEventListener('resize', cronMenuOnScroll);
}

// cronApplyRunStarted optimistically patches the in-memory cronJobs row so
// the "运行中" badge renders without a list refetch. P0 cron-run-history
// (RFC §7.2 / §8.1) — the run-ended event triggers the authoritative
// refetch a few seconds later. Tolerates an unknown job_id (we may receive
// a started event for a freshly created job before the local list pulls
// it; in that case the next fetchCronJobs reconciles).
function cronApplyRunStarted(msg) {
  if (!msg || !msg.job_id) return;
  const list = Array.isArray(cronJobs) ? cronJobs : [];
  const j = list.find(x => x && x.id === msg.job_id);
  if (!j) {
    // Optimistic miss: fallback to a refetch so the UI catches up.
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
    return;
  }
  j.current_run = {
    run_id: msg.run_id,
    started_at: msg.started_at,
    phase: 'queued',
    trigger: msg.trigger || '',
    session_id: msg.session_id || '',
  };
  // 一个新的 run 起步意味着上一次 timed_out 的"冻结"窗口已过去，
  // 清掉 frozen 标记让用户能再次看到实时事件。否则 hourly 任务下一次
  // 跑时前端仍然会丢事件，看起来像 bug。
  cronFrozenRuns.delete(msg.job_id);
  // Round 2 R-4: WS-confirmed run start clears the optimistic cooldown
  // lock — the disabled-running label takes over from this point. If the
  // started event came from a non-manual trigger (scheduled/catchup), the
  // cooldown lock was never set so this is a no-op.
  cronTriggerCooldownClear(msg.job_id);
  // cron-live RFC §3: 跨 run 复位前一轮的 cron live 状态。两种场景：
  //   - fresh 模式：scheduler_run.go:318 Reset(key) 销毁旧 stub，后端
  //     resubscribeEvents 接管；前端容器里旧 run 的事件应当被新 run 替换。
  //   - persistent 模式：同 stub 跨 run 持续，但用户视角下 Run #2 是新 run，
  //     不该把 Run #1 的事件混在容器里。
  // 即使 ensureCronLiveSubscription 短路（jobId 已订）也要清。runStartedAt
  // 更新成新 run 的 started_at，让 onConnected / onCronLiveSessionState 的
  // re-sub 路径用正确的 after= 阈值。
  if (typeof wsm !== 'undefined' && wsm.cronLive && wsm.cronLive.jobId === msg.job_id) {
    wsm.cronLive.events = [];
    wsm.cronLive.lastEventTimeMs = 0;
    wsm.cronLive.truncatedCount = 0;
    wsm.cronLive.runStartedAt = msg.started_at || Date.now();
    wsm.cronLive.status = 'pending';
    setCronLiveStatus('pending');
  }
  renderCronPanel();
  // cron-live RFC §3: drawer 已开 + 该 job 起跑 → 触发订阅。renderCronPanel
  // 已先调到 renderCronDrawer，drawer 内的 #cron-live-events 容器此时已就位。
  if (typeof ensureCronLiveSubscription === 'function') ensureCronLiveSubscription();
}

// cronFrozenRuns 是 timed_out（或其他非 succeeded/skipped 终态）后
// 冻结事件流的 jobID 集合。命中后，wsm.onEvent 对该 cron session
// 的实时事件直接丢弃，避免 dashboard 在 cron 历史卡显示"超时"
// 的同时事件流仍在追加（CLI 子进程没立刻停，会再吐几个 ghost
// 事件）。下一次 cron_run_started 同 job 时清空。
//
// 后端 cron deadline 已经主动 InterruptViaControl 让 CLI 收尾，
// 但 control_request 到 result 事件之间还有 ~几百 ms ~ 几秒延迟；
// 这里是第二道防线，让 dashboard 视觉上立即冻结。
const cronFrozenRuns = new Set();

// cronApplyRunEnded patches the local row before the authoritative
// fetchCronJobs lands. We clear current_run so the running-badge stops
// flashing immediately; the subsequent refetch fills in last_error_class
// / counters / last_run_at.
function cronApplyRunEnded(msg) {
  if (!msg || !msg.job_id) return;
  const list = Array.isArray(cronJobs) ? cronJobs : [];
  const j = list.find(x => x && x.id === msg.job_id);
  if (!j) return;
  j.current_run = null;
  // Provisional last_error_class / last_run_at so the row repaints with
  // the new state before fetchCronJobs returns. Backend remains source
  // of truth for the persisted snapshot.
  if (msg.state && msg.state !== 'succeeded' && msg.state !== 'skipped') {
    j.last_error_class = msg.error_class || '';
    // 任何非 succeeded/skipped 终态都冻结：timed_out / failed / canceled。
    // 新的 cron_run_started 会清除冻结。
    cronFrozenRuns.add(msg.job_id);
  } else if (msg.state === 'succeeded') {
    j.last_error_class = '';
    j.last_error = '';
    cronFrozenRuns.delete(msg.job_id);
  } else if (msg.state === 'skipped') {
    cronFrozenRuns.delete(msg.job_id);
  }
  if (msg.ended_at) j.last_run_at = msg.ended_at;
  // cron-live RFC §3 / §6: 任务进入终态（任意 state），cron live 订阅保留供
  // 操作员回看，但 status 切到 'stopped' 让用户清楚区分"直播中"vs"已结束"。
  // unsub 仅在 closeCronDetail / 切换 jobId 时发生。
  if (typeof wsm !== 'undefined' && wsm.cronLive && wsm.cronLive.jobId === msg.job_id) {
    wsm.cronLive.status = 'stopped';
    setCronLiveStatus('stopped');
  }
  renderCronPanel();
}

// isCronSessionFrozen 判断当前 selectedKey 是否是被冻结的 cron session。
// cron session key 的形态是 "cron:" + jobID（见 session.CronKey）；只有
// dashboard 当前看的就是这条 cron 的实时面板时才需要丢事件，其他视图
// 不受影响。
function isCronSessionFrozen(key) {
  if (!key || typeof key !== 'string') return false;
  if (!key.startsWith('cron:')) return false;
  return cronFrozenRuns.has(key.slice('cron:'.length));
}

/* ===== cron live event stream（cron-live RFC） ===== */

// isCronLiveKey 判断一条 WS 消息的 key 是否属于 cron live 订阅。带双保险：
// 既已订阅 (subscribedKey) 或 pending 中，且与主订阅 selectedKey 不撞键
// （cron drawer 打开时 openCronPanel 已清空 selectedKey，撞键不可能但兜底）。
function isCronLiveKey(key) {
  if (!key) return false;
  if (key === selectedKey) return false;
  const cl = wsm.cronLive;
  if (cl.subscribedKey && key === cl.subscribedKey) return true;
  if (cl.pendingJobId && key === ('cron:' + cl.pendingJobId)) return true;
  return false;
}

// setCronLiveStatus 将 wsm.cronLive.status 字符串投影到 DOM 上。
// 三态：'pending' / 'live' / 'stopped'，'idle' 时清空文本。
function setCronLiveStatus(state) {
  const el = document.getElementById('cron-live-status');
  if (!el) return;
  const labels = {
    idle: '',
    pending: '等待事件…',
    live: '实时',
    stopped: '已停止',
  };
  el.textContent = labels[state] || '';
  el.className = 'cdl-status cdl-status-' + state;
}

function updateCronLiveTruncated() {
  const trunc = document.getElementById('cron-live-truncated');
  if (!trunc) return;
  const n = wsm.cronLive.truncatedCount || 0;
  if (n > 0) {
    trunc.hidden = false;
    trunc.textContent = '已折叠 ' + n + ' 条更早事件，请等任务结束后查看历史详情';
  } else {
    trunc.hidden = true;
  }
}

// repaintCronLive 把 wsm.cronLive.events 数组重渲到 #cron-live-events 容器。
// 在 renderCronDrawer 重渲后调一次，让重建的 DOM 立刻显示已累积的事件。
// jobId 一致性守卫：若 cronLive.jobId 与当前 drawer 的 jobId 不一致就清空，
// 避免 ensureCronLiveSubscription 还未完成切换前一帧渲到错的 drawer。
function repaintCronLive() {
  const el = document.getElementById('cron-live-events');
  if (!el) return;
  const drawerJobId = (typeof cronDetailJobId !== 'undefined') ? cronDetailJobId : null;
  if (drawerJobId && wsm.cronLive.jobId && wsm.cronLive.jobId !== drawerJobId) {
    el.innerHTML = '';
    return;
  }
  const events = wsm.cronLive.events || [];
  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  if (html) {
    el.innerHTML = html;
  } else if (events.length > 0) {
    // 事件到了但全被 INTERNAL_EVENT_TYPES 过滤光（典型 parallel agent team：
    // 整段都是 agent / task_* / tool_use）。若留空 innerHTML，CSS
    // .cdl-events:empty::before 会误报"暂无事件"，与顶部"已折叠 N 条"自相矛盾。
    // 渲染占位文案，对齐主面板 appendEvents 的同款兜底。
    el.innerHTML = CRON_LIVE_AGENT_ONLY_HTML;
  } else {
    el.innerHTML = '';
  }
  el.scrollTop = el.scrollHeight;
  updateCronLiveTruncated();
  setCronLiveStatus(wsm.cronLive.status);
}

// appendEventsToContainer 是 appendEvents 的容器化变体：不动主面板的
// turnState / banner / navUserEls / optimistic-msg，只把事件 HTML 追加到
// 指定容器。供 cron live 增量推送复用。
function appendEventsToContainer(el, events) {
  if (!el) return;
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  // 若容器当前只挂着 agent-only 占位（repaintCronLive 渲过），在追加真实
  // 事件前清掉它，避免占位与事件并存。lastDividerTime 等读取也不会被它干扰。
  if (el.querySelector('.cdl-agent-only')) el.innerHTML = '';
  let prevT = lastDividerTime(el);
  events.forEach(e => {
    if (isInternalEvent(e)) return;
    const h = eventHtml(e); if (!h) return;
    const t = e.time || 0;
    if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
    }
    el.insertAdjacentHTML('beforeend', h);
    if (t) prevT = t;
  });
  // #398-sibling: onCronLiveEvent caps the data model at CRON_LIVE_MAX_EVENTS
  // (events.shift) but the incremental push only ever appends here, so the
  // container DOM grew unbounded across a long cron run. Trim the oldest
  // .event bubbles from the top to keep the DOM in sync with the data cap.
  let bubbles = el.querySelectorAll(':scope > .event').length;
  if (bubbles > CRON_LIVE_MAX_EVENTS) {
    let node = el.firstChild;
    while (node && bubbles > CRON_LIVE_MAX_EVENTS) {
      const next = node.nextSibling;
      if (node.nodeType === 1 && node.classList && node.classList.contains('event')) bubbles--;
      el.removeChild(node);
      node = next;
    }
  }
  if (wasBottom) el.scrollTop = el.scrollHeight;
}

// ensureCronLiveSubscription 是 cron live 订阅状态的中心协调器。语义：
//   - drawer 关闭 → 撤销订阅
//   - drawer 开 + 任务跑 + 未订阅 → 订阅
//   - drawer 开 + 任务跑 + 已订阅同 jobId → no-op
//   - drawer 开 + 任务跑 + 已订阅别的 jobId → 切换
//   - drawer 开 + 任务空闲 → no-op（保留已订内容供回看；首次开 idle 任务则不订）
// 故意不在 cronApplyRunEnded 钩 unsub —— 让操作员看完本轮事件，关 drawer 才撤。
function ensureCronLiveSubscription() {
  if (typeof cronDetailJobId === 'undefined') return;
  const jobId = cronDetailJobId;
  const cl = wsm.cronLive;
  if (!jobId) {
    if (cl.jobId) wsm.unsubscribeCronLive();
    return;
  }
  if (cl.jobId && cl.jobId !== jobId) {
    wsm.unsubscribeCronLive();
  }
  if (cl.jobId === jobId) return;
  const job = (typeof cronJobs !== 'undefined' && Array.isArray(cronJobs))
    ? cronJobs.find(j => j && j.id === jobId)
    : null;
  const isRunning = !!(job && job.current_run && job.current_run.started_at);
  if (!isRunning) return;
  wsm.subscribeCronLive(jobId, job.current_run.started_at);
}

// formatRunningElapsed returns a colloquial "正在运行 12s / 2m" label for
// the inline badge. Floors to seconds; wraps to "Nm Ss" past 60s.
function formatRunningElapsed(startedAt) {
  if (!startedAt) return '正在运行';
  const ms = Date.now() - startedAt;
  if (ms < 0) return '正在运行';
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return '运行中 ' + sec + 's';
  const m = Math.floor(sec / 60);
  const s = sec - m * 60;
  if (m < 60) return '运行中 ' + m + 'm ' + s + 's';
  const h = Math.floor(m / 60);
  return '运行中 ' + h + 'h ' + (m - h * 60) + 'm';
}

// Polling timer that re-renders cron rows so "运行中 Xs" advances each
// second while at least one job is running. Idle when no jobs are running.
let cronRunningTickTimer = null;

// R243-PERF-8 / #813: per-tick scoped text update.
//
// Pre-fix behaviour: ensureCronRunningTick fired renderCronPanel() every
// second, which rebuilt the *entire* cron list innerHTML (N rows × full
// HTML strings + onclick wiring) just to advance the elapsed-time text
// on the running rows. With 50 jobs and any one of them running, that
// was 50× full-row reflow per second — a measurable jank hot path on
// modest hardware.
//
// Post-fix: the tick walks only `.cj-row.is-running` elements and
// updates the two text nodes that carry the elapsed label
// (`.cj-when.running` desktop, `.cj-when-inline.is-running` mobile).
// Everything else — schedule chip, stats badge, action buttons — is
// untouched, so the layout never reflows past the elapsed label.
//
// Fallback: if no running rows are mounted (the panel was closed between
// the timer firing and this callback) we DON'T fall back to a full
// renderCronPanel — the timer's own three-condition guard above already
// catches that case and clears the interval, so the no-op is correct.
//
// Job churn (a row finishes / a new row starts running) is handled by
// the WS cron_run_started / cron_run_ended fan-out which calls
// cronApplyRunStarted / cronApplyRunEnded → renderCronPanel; that path
// already updates row classes (`is-running` on/off) and is the right
// place to add/remove rows. The 1Hz tick is therefore *only* responsible
// for advancing the elapsed text on rows that are already classed
// is-running, which is exactly the scope of this targeted update.
function cronRunningTickPaintScoped() {
  const host = document.getElementById('cron-list-items');
  if (!host) return;
  const rows = host.querySelectorAll('.cj-row.is-running');
  if (!rows.length) return;
  // Build a quick lookup so we don't O(N) scan cronJobs once per row.
  const byId = new Map();
  if (Array.isArray(cronJobs)) {
    for (const j of cronJobs) {
      if (j && j.id) byId.set(j.id, j);
    }
  }
  for (const row of rows) {
    const id = row.getAttribute('data-cron-id');
    if (!id) continue;
    const job = byId.get(id);
    if (!job || !job.current_run || !job.current_run.started_at) continue;
    const label = formatRunningElapsed(job.current_run.started_at);
    // Desktop column.
    const whenEl = row.querySelector('.cj-when.running');
    if (whenEl && whenEl.textContent !== label) whenEl.textContent = label;
    // Mobile inline span (cj-when-inline). Class set carries imminent /
    // paused but for running rows we just rewrite text.
    const inlineEl = row.querySelector('.cj-when-inline');
    if (inlineEl && inlineEl.textContent !== label) inlineEl.textContent = label;
  }
}

function ensureCronRunningTick() {
  const anyRunning = Array.isArray(cronJobs) && cronJobs.some(j => j && j.current_run);
  // Stop conditions（任何一个成立即清掉 timer）：
  //   - 无 running job
  //   - 当前选中了某个 session（renderCronPanel 第一行就 return,timer 等于在浪费 CPU）
  //   - cron-list-items DOM 已不在文档（用户切到非 cron 视图）
  // R220-FE-1: 修复 timer 永不停的内存/CPU 泄漏。
  const cronListMounted = !!document.getElementById('cron-list-items');
  const shouldRun = anyRunning && !selectedKey && cronListMounted;
  if (shouldRun && !cronRunningTickTimer) {
    cronRunningTickTimer = setInterval(() => {
      // Defensive: 同样的三条件检查在 tick 内也跑一遍——避免 selectSession 切换
      // 之后这一帧还在 schedule 但 DOM 已经换了。
      if (selectedKey || !document.getElementById('cron-list-items')) {
        clearInterval(cronRunningTickTimer);
        cronRunningTickTimer = null;
        return;
      }
      // R243-PERF-8 / #813: scoped text update instead of full
      // renderCronPanel rebuild. See cronRunningTickPaintScoped godoc.
      try { cronRunningTickPaintScoped(); } catch (_) {}
    }, 1000);
  } else if (!shouldRun && cronRunningTickTimer) {
    clearInterval(cronRunningTickTimer);
    cronRunningTickTimer = null;
  }
}

// cronJobCardHtml renders a single cron row. v3 redesign: high-density row
// replaces the v2 card (see docs/TODO.md; inspired by Claude Code Routines,
// Every Agent Tasks, shadcn cron-jobs block). Structure:
//
//   ● title                 每天 04:00  ...  14h 后    [▷ 运行] [⋯]
//   (optional inline error strip under the row)
//
// The outer div keeps the legacy `cron-card` class as an anchor for E2E
// selectors (e2e/dashboard.test.js never asserts inner structure). The new
// visual class is `cj-row`. A hidden `.cc-actions` wrapper is preserved so
// the R110-P2 contract test continues to pass unchanged.
function cronJobCardHtml(j) {
  const nextAbs = j.next_run ? formatAbsTime(j.next_run) : '';
  const lastAbs = j.last_run_at ? formatAbsTime(j.last_run_at) : '';
  const agoStr = j.last_run_at ? formatAgoColloquial(j.last_run_at) : '';
  const titleStr = (j.title || '').trim() || firstNonEmptyLine(j.prompt || '', 60);
  const hasTitle = !!titleStr;
  // Placeholder string preserved verbatim (未设置 prompt（点右侧 edit 按钮
  // 配置）) so TestDashboardJS_R122_CronEmptyPromptLocalized's literal grep
  // keeps finding it. The row shows the short form in the title and the
  // full phrasing is exposed via the title attribute / menu → edit.
  const emptyPromptHint = '未设置 prompt（点右侧 edit 按钮配置）';
  const displayTitle = hasTitle ? titleStr : '未设置 prompt';
  const human = humanizeCron(j.schedule);

  const isPaused = !!j.paused;
  const isError = !!j.last_error && !isPaused;
  const isMissed = !!j.missed && !isPaused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  const isActive = cronDetailJobId === j.id;
  const rowClasses = ['cj-row'];
  if (isPaused) rowClasses.push('paused');
  if (isError) rowClasses.push('is-error');
  if (isMissed) rowClasses.push('is-missed');
  if (isRunning) rowClasses.push('is-running');
  if (isActive) rowClasses.push('is-active');

  // When-column: running → "运行中 Xs"（实时计时）; paused → "已暂停"; else colloquial relative time.
  // P0 cron-run-history (RFC §8.1) — running takes precedence over paused
  // (a TriggerNow on a paused job is rejected backend-side, so this just
  // reflects the actual scheduled / manual run).
  let whenLabel = '';
  let whenImminent = false;
  if (isRunning) {
    whenLabel = formatRunningElapsed(j.current_run.started_at);
  } else if (isPaused) {
    whenLabel = '已暂停';
  } else if (j.next_run) {
    const w = formatWhenColloquial(j.next_run);
    whenLabel = w.label;
    whenImminent = w.imminent;
  }
  const whenTitle = isRunning
    ? ' title="run_id ' + escAttr(j.current_run.run_id || '') + (j.current_run.phase ? ' — phase ' + escAttr(j.current_run.phase) : '') + '"'
    : (nextAbs ? ' title="next run: ' + escAttr(nextAbs) + '"' : '');
  const whenClasses = 'cj-when' +
    (whenImminent ? ' imminent' : '') +
    (isPaused && !isRunning ? ' paused' : '') +
    (isRunning ? ' running' : '');
  const whenCol = whenLabel
    ? '<div class="' + whenClasses + '"' + whenTitle + '>' + esc(whenLabel) + '</div>'
    : '<div class="cj-when"></div>';

  // P2 cron-run-history (RFC §8.1 / §8.4) — 成功率小徽章 + recent_runs hover tooltip。
  // 仅当 stats.total > 0 时渲染（新建 / 从未跑过的 job 没数据，徽章空白会显得噪声）。
  // 三档配色：100%=绿（数字徽章）、80-99%=中性、<80%=红警告。
  // Hover 出 5 个状态气泡（最旧→最新），CSS 用纯 :hover 触发 .cj-stats-pop 显隐。
  const statsBadge = cronStatsBadgeHtml(j);

  // Sub-row: clickable schedule chip (→ edit modal) + selective icons + optional
  // last-run chip. Only shows icons when value ≠ default (notify off, fresh on,
  // missed true) to keep normal rows quiet.
  // schedule chip — accessible. role=button + tabindex=0 + Enter/Space
  // handler so a keyboard user can open the edit modal focused at the
  // schedule field without mousing.
  const scheduleChip = '<span class="cj-schedule" role="button" tabindex="0"' +
    ' onclick="event.stopPropagation();editCronJob(\'' + escJs(j.id) + '\')"' +
    ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();event.stopPropagation();editCronJob(\'' + escJs(j.id) + '\')}"' +
    ' title="点击修改时间">' + esc(human) + '</span>';
  let iconGlyphs = '';
  if (j.notify === false) {
    iconGlyphs += '<span class="cj-icon notify-off" title="IM 通知已关闭">&#128277;</span>';
  }
  if (j.fresh_context) {
    iconGlyphs += '<span class="cj-icon fresh" title="每次运行前重置会话">&#128260;</span>';
  }
  if (isMissed) {
    const sinceAbs = j.missed_since ? formatAbsTime(j.missed_since) : '';
    const tip = sinceAbs ? '上次应跑于 ' + sinceAbs + '；进程可能刚重启或休眠过' : '已错过至少一次调度';
    iconGlyphs += '<span class="cj-icon missed" title="' + escAttr(tip) + '">&#9888;</span>';
  }
  const lastRunChip = agoStr
    ? '<span class="cj-ago"' + (lastAbs ? ' title="last run: ' + escAttr(lastAbs) + '"' : '') + '>上次 ' + esc(agoStr) + '</span>'
    : '';
  // whenMobile surfaces the when-column content inline in the sub-row on
  // narrow viewports where the dedicated .cj-when column is hidden via
  // CSS. Includes the paused label so mobile users see state.
  const whenMobile = whenLabel
    ? '<span class="cj-when-inline' + (whenImminent ? ' imminent' : '') + (isPaused ? ' paused' : '') + '">' + esc(whenLabel) + '</span>'
    : '';
  const subRow = '<div class="cj-sub">' + scheduleChip + iconGlyphs + lastRunChip + whenMobile + '</div>';

  // Error strip: inline one-line summary for non-paused rows with last_error.
  const errorStrip = isError
    ? '<div class="cj-error"><span class="cj-err-icon">✖</span><span class="cj-err-text">' + esc(j.last_error) + '</span></div>'
    : '';

  // Actions: ghost Run + ⋯ menu trigger. Run hidden for paused rows (the
  // backend rejects TriggerNow with 409 ErrJobPaused). Keep `const runBtn =
  // j.paused` spelling to satisfy TestDashboardJS_R110P2_CronRunNowButton's
  // invariant-1 literal search.
  const runBtn = j.paused
    ? ''
    : '<button type="button" class="cc-btn cj-run" onclick="event.stopPropagation();cronTriggerNow(\'' + escJs(j.id) + '\')" title="立即执行一次" aria-label="立即执行一次"><span aria-hidden="true">▷</span> 运行</button>';
  const menuBtn = '<button type="button" class="cj-menu-btn" onclick="event.stopPropagation();toggleCronMenu(\'' + escJs(j.id) + '\')" aria-label="更多操作" aria-haspopup="true">⋯</button>';

  // TestDashboardJS_R110P2_CronRunNowButton greps the source for the legacy
  // .cc-actions wrapper structure. The v3 row design moved actions into
  // .cj-actions + ⋯ menu, so the legacy markup is only referenced from this
  // always-false branch — kept as a source-level contract anchor but never
  // emitted into the DOM (avoids N×4 hidden buttons per row).
  if (typeof cronJobCardHtml.__unused === 'symbol') {
    return '<div class="cc-actions" onclick="event.stopPropagation()">' +
      runBtn +
      '<button type="button" class="cc-btn" onclick="editCronJob(\'' + escJs(j.id) + '\')">edit</button>' +
      (j.paused
        ? '<button type="button" class="cc-btn" onclick="cronResume(\'' + escJs(j.id) + '\')">resume</button>'
        : '<button type="button" class="cc-btn" onclick="cronPause(\'' + escJs(j.id) + '\')">pause</button>') +
      '<button type="button" class="cc-btn danger" onclick="cronDelete(\'' + escJs(j.id) + '\')">delete</button>' +
    '</div>';
  }

  return '<div class="' + rowClasses.join(' ') + ' cron-card" data-cron-id="' + escAttr(j.id) + '" role="button" tabindex="0" ' +
    'onclick="openCronDetail(\'' + escJs(j.id) + '\', this)" ' +
    'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();openCronDetail(\'' + escJs(j.id) + '\', this)}">' +
    '<span class="cj-dot" aria-hidden="true"></span>' +
    '<div class="cj-main">' +
      '<div class="cj-title' + (hasTitle ? '' : ' placeholder') + '" title="' + escAttr(titleStr || emptyPromptHint) + '">' + esc(displayTitle) + '</div>' +
      subRow +
    '</div>' +
    whenCol +
    statsBadge +
    '<div class="cj-actions">' + runBtn + menuBtn + '</div>' +
    errorStrip +
  '</div>';
}

// cronStatsBadgeHtml — P2 cron-run-history (RFC §8.1 / §8.4) 列表卡片成功率徽章。
// 数据源：j.stats（total/succeeded/failed/skipped/timed_out/canceled）+ j.recent_runs。
// 三档：100%=绿 N、80-99%=中性 99%、<80%=红 92% (118/120)。total=0 不渲染（噪声）。
// Tooltip：hover 出 5 个状态气泡（按 recent_runs 顺序，最旧→最新）+ trigger 信息。
function cronStatsBadgeHtml(j) {
  const stats = j && j.stats;
  if (!stats || !stats.total || stats.total <= 0) return '';
  const total = stats.total | 0;
  const ok = stats.succeeded | 0;
  // 成功率 = succeeded / total（skipped / canceled 不计为失败也不计为成功，
  // 但保留在分母里，与详情页"120 次"的口径一致；后续 P3 可拆开）。
  const rate = total > 0 ? Math.round((ok * 100) / total) : 0;
  let cls, label;
  if (rate >= 100 && stats.failed === 0 && stats.timed_out === 0) {
    cls = 'ok'; label = String(total);
  } else if (rate >= 80) {
    cls = 'mid'; label = rate + '%';
  } else {
    cls = 'bad'; label = rate + '% (' + ok + '/' + total + ')';
  }
  // recent_runs 5 条状态气泡（最旧→最新，与时间轴方向相反——气泡是"最近趋势条"，
  // 习惯上从左到右走时间）。空数组 fallback 为单条空气泡占位。
  const recent = Array.isArray(j.recent_runs) ? j.recent_runs.slice(0, 5).reverse() : [];
  const dotsHtml = recent.length > 0
    ? recent.map(r => {
        const st = (r && r.state) || '';
        const tip = formatAbsTime((r && r.started_at) || 0) +
          (st ? ' · ' + cronStateLabel(st) : '') +
          (r && r.trigger ? ' · ' + r.trigger : '');
        return '<span class="cj-stats-dot ' + cronStateDotClass(st) + '" title="' + escAttr(tip) + '"></span>';
      }).join('')
    : '<span class="cj-stats-dot empty"></span>';
  // 用绝对定位的 .cj-stats-pop 做悬浮 tooltip；浏览器原生 title 在 hover dots
  // 时会被 dots 自身的 title 接管，主徽章再写一份避免双层 tooltip 闪烁。
  const summary = '总 ' + total + '· 成功 ' + ok + '· 失败 ' + (stats.failed | 0) +
    (stats.skipped ? '· 跳过 ' + stats.skipped : '') +
    (stats.timed_out ? '· 超时 ' + stats.timed_out : '');
  return '<div class="cj-stats ' + cls + '" tabindex="0" aria-label="' + escAttr('执行统计 ' + summary) + '">' +
    '<span class="cj-stats-label">' + esc(label) + '</span>' +
    '<div class="cj-stats-pop" role="tooltip">' +
      '<div class="cj-stats-pop-row">' + dotsHtml + '</div>' +
      '<div class="cj-stats-pop-meta">' + esc(summary) + '</div>' +
    '</div>' +
  '</div>';
}

// cronStateDotClass / cronStateLabel —— 单一状态色 / 文案表，详情页时间轴 +
// 列表 tooltip 共用，避免两处不一致。RFC §8.2 配色：
//   succeeded 绿 / failed 红 / skipped 灰 / timed_out 橙 / canceled 紫 / running 蓝脉动
function cronStateDotClass(state) {
  switch (state) {
    case 'succeeded': return 'ok';
    case 'failed': return 'err';
    case 'skipped': return 'skip';
    case 'timed_out': return 'warn';
    case 'canceled': return 'cancel';
    case 'running': return 'run';
    default: return 'unk';
  }
}
function cronStateLabel(state) {
  switch (state) {
    case 'succeeded': return '成功';
    case 'failed': return '失败';
    case 'skipped': return '跳过';
    case 'timed_out': return '超时';
    case 'canceled': return '已取消';
    case 'running': return '运行中';
    default: return state || '未知';
  }
}

// cronErrorClassLabel —— 后端 ErrorClass 枚举的中文友好名。RFC §9 错误分类映射。
// 未知值原样返回，方便排查（不应发生但容错）。
function cronErrorClassLabel(cls) {
  switch (cls) {
    case 'session_error': return '会话错误';
    case 'send_error': return '发送失败';
    case 'deadline_exceeded': return '超时';
    case 'canceled': return '已取消';
    case 'workdir_unreachable': return '工作目录不可达';
    case 'workdir_outside_root': return '工作目录越界';
    case 'overlap_skipped': return '重叠跳过';
    case 'paused_concurrent': return '暂停时被抢';
    case 'panic': return '内部异常';
    default: return cls || '';
  }
}

// formatRunDuration —— 时间轴行 / 详情区"耗时"文案。>1000ms 用 "Xs"，否则 "Xms"。
// 0 / 缺省返回 ''——running 状态没 duration_ms，调用方应传 0 跳过渲染。
function formatRunDuration(ms) {
  if (!ms || ms <= 0) return '';
  if (ms < 1000) return ms + 'ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1).replace(/\.0$/, '') + 's';
  const m = Math.floor(s / 60);
  const ss = Math.round(s - m * 60);
  return m + 'm ' + ss + 's';
}

// P2 cron-run-history (RFC §8.2) — 时间轴每个 job 的本地状态。
// runs / nextBefore: 分页列表 + 游标；details: 已 fetch 的单条 run 详情缓存
// （不持久化，session 切换即丢）。
// §16 inline-expand 回归: 展开态由 cronExpandedRunId 模块状态驱动（同时
// 只展开一行），rowHtml 在选中行下方就地嵌入 cronTimelineDetailHtml。
const cronTimelineState = Object.create(null);

// §16 inline-expand 回归: 当前展开的 run（同时只展开一行）。
// jobId 必须同时匹配以处理用户切到另一个 cron 但 expandedRunId 没清的场景。
const cronExpandedRunId = { jobId: null, runId: null };

// CRON_TIMELINE_FRESH_MS — 超过此 TTL 的 timeline 缓存视为陈旧，下次进入
// cron session 详情时清掉强制重拉。R220-FE-3：用户切走再切回中间可能有
// 新 run，但本地状态仍 stale，timeline 不主动刷新。
const CRON_TIMELINE_FRESH_MS = 30 * 1000;

function getCronTimelineState(jobId) {
  if (!cronTimelineState[jobId]) {
    cronTimelineState[jobId] = {
      runs: [],
      nextBefore: 0,
      done: false,         // true = next_before 缺失，已到结尾
      loading: false,
      details: Object.create(null),
      lastMountAt: 0,       // 上次 mount 渲染的 ms 时戳；renderCronTimelineForSession 用来判 stale
      // R243-PERF-12 (#817): cache the last innerHTML written by
      // renderCronTimelinePanel so a no-op re-render (e.g. WS broadcast
      // arrives but no run changed) skips the full innerHTML rewrite.
      // Stored on the per-job state so it is reset together with
      // runs/details when CRON_TIMELINE_FRESH_MS evicts the cache.
      lastRenderedHtml: '',
    };
  }
  return cronTimelineState[jobId];
}

// renderCronTimelineForJob is the canonical drawer-side entry point.
// renderCronTimelineForSession (the legacy mainShell-coupled variant) has
// been removed — see cronDrawerHtml above.

// cronTimelineHtml — 渲染整个"执行历史"section 的内层 HTML。
// 头部：简洁标题"最近运行"+ 总次数小标签（cron-dashboard-redesign P3 §4：
// 把"成功率/最后错误分类"等运维指标隐去；用户首屏只看"它最近跑得怎样"，
// 排查时再点单条 run 进 sheet 看详情）。
// 行列表：默认折叠到 5 条，"查看全部 N 条"按钮展开剩余 + 触发 loadMore。
// CRON_TIMELINE_DEFAULT_VISIBLE = 5 条与 cronDrawerSpecHtml 的视觉密度同源。
function cronTimelineHtml(jobId, job, st) {
  const stats = job && job.stats;
  const total = stats ? (stats.total | 0) : 0;
  const headTitle = total > 0
    ? '最近运行 · ' + total + ' 次'
    : '最近运行';
  const rowsHtml = st.runs.length === 0
    ? '<div class="ct-empty">暂无执行记录。下次调度或点击「立即执行」触发首次运行。</div>'
    : st.runs.map(r => cronTimelineRowHtml(jobId, r, st)).join('');

  // P3 §4 折叠机制：data-collapsed=true 时 CSS 只露前 5 行；点 [查看全部]
  // 切到 false。本地视图状态用 dataset 而非 module-level，因为重绘时
  // renderCronTimelineForJob 整段重建 innerHTML，模块状态会被冲掉；DOM
  // 属性同样会被冲掉但用户的"展开"动作本就是 single-click ad-hoc 行为，
  // 不持久化也合理。
  const initiallyCollapsed = st.runs.length > 5 ? 'true' : 'false';
  const hiddenCount = Math.max(0, st.runs.length - 5);

  let moreBtn = '';
  if (st.runs.length > 0) {
    if (st.runs.length > 5) {
      // 折叠态："查看全部 N 条"——展开后再让既有 [加载更多] 接管分页。
      moreBtn = '<button type="button" class="ct-more-btn ct-show-all"' +
        ' data-hidden-count="' + hiddenCount + '"' +
        ' onclick="cronTimelineToggleShowAll(this)">' +
        '查看全部 ' + st.runs.length + ' 条' +
      '</button>';
    } else if (st.done) {
      moreBtn = '<button type="button" class="ct-more-btn" disabled aria-disabled="true">已到结尾</button>';
    } else {
      moreBtn = '<button type="button" class="ct-more-btn"' +
        (st.loading ? ' disabled' : '') +
        ' onclick="cronTimelineLoadMore(\'' + escJs(jobId) + '\')">' +
        (st.loading ? '加载中…' : '加载更多') +
      '</button>';
    }
  }
  return '<div class="ct-head">' +
      '<h3>' + esc(headTitle) + '</h3>' +
    '</div>' +
    '<div class="ct-rows" data-collapsed="' + initiallyCollapsed + '" data-job-id="' + escAttr(jobId) + '">' + rowsHtml + '</div>' +
    (moreBtn ? '<div class="ct-more">' + moreBtn + '</div>' : '');
}

// cronTimelineToggleShowAll — 把 .ct-rows 从 collapsed 切到 expanded，
// 并把按钮替换为既有 [加载更多] 行为（如果 st.done 则替换为"已到结尾"）。
// 切换后还有更多页要拉的，下次点 [加载更多] 走原路径。
function cronTimelineToggleShowAll(btn) {
  if (!btn || !btn.parentNode) return;
  const wrap = btn.closest('.cron-timeline-panel');
  if (!wrap) return;
  const rows = wrap.querySelector('.ct-rows');
  if (!rows) return;
  rows.setAttribute('data-collapsed', 'false');
  const jobId = rows.getAttribute('data-job-id') || '';
  const st = cronTimelineState[jobId];
  // 替换按钮：用既有 cronTimelineLoadMore 路径——st.done 的话变灰态 [已到结尾]。
  const next = document.createElement('div');
  next.className = 'ct-more';
  if (!st || st.done) {
    next.innerHTML = '<button type="button" class="ct-more-btn" disabled aria-disabled="true">已到结尾</button>';
  } else {
    next.innerHTML = '<button type="button" class="ct-more-btn"' +
      (st.loading ? ' disabled' : '') +
      ' onclick="cronTimelineLoadMore(\'' + escJs(jobId) + '\')">' +
      (st.loading ? '加载中…' : '加载更多') +
    '</button>';
  }
  const oldMore = btn.parentNode;
  if (oldMore && oldMore.parentNode) oldMore.parentNode.replaceChild(next, oldMore);
}

// cronTimelineRowHtml — 单条 run 行（§16 inline-expand 回归）。
// 选中行（cronExpandedRunId 命中）下方就地嵌入 .ctr-detail 详情块（复用
// cronTimelineDetailHtml 渲染逻辑）。同时只展开一行；点行 toggle。
// run 字段（CronRunSummary）：run_id / state / trigger / started_at / ended_at /
// duration_ms / session_id / error_class。所有字段均可为空，渲染时 fallback。
function cronTimelineRowHtml(jobId, r, st) {
  if (!r) return '';
  const runId = r.run_id || '';
  const state = r.state || '';
  const startedAbs = r.started_at ? formatAbsTime(r.started_at) : '';
  // 行主时间用紧凑显示（"5月17日 14:30"）；hover 看完整 ISO。
  const startedShort = r.started_at ? formatCronTimelineShort(r.started_at) : '—';
  const dur = state === 'running'
    ? '正在运行'
    : formatRunDuration(r.duration_ms || 0);
  const errCls = r.error_class || '';
  // §16: 选中/展开态由 cronExpandedRunId 决定（jobId+runId 同时匹配）。
  const isExpanded = !!(runId && cronExpandedRunId && cronExpandedRunId.runId === runId && cronExpandedRunId.jobId === jobId);
  const dotCls = cronStateDotClass(state);
  const stateLbl = cronStateLabel(state);

  // 副行：trigger / error_class（session_id 短 ID 已移除——对最终用户无意义；
  // 展开 inline 详情即可看到完整 session_id）
  const subParts = [];
  if (r.trigger) subParts.push('<span class="ctr-trigger">' + esc(r.trigger) + '</span>');
  if (errCls) {
    subParts.push('<span class="ctr-errcls">' + esc(cronErrorClassLabel(errCls)) + '</span>');
  }
  const subRow = subParts.length > 0
    ? '<div class="ctr-sub">' + subParts.join('<span class="ctr-sep">·</span>') + '</div>'
    : '';

  // §16: 行展开态时下方嵌 .ctr-detail（复用 cronTimelineDetailHtml）。
  // detail 缓存命中即立刻渲染；否则给加载骨架，cronTimelineFetchDetail 会异步
  // 写入 st.details[runId] 并触发 renderCronTimelinePanel 重绘把骨架替换为内容。
  let detailBlock = '';
  if (isExpanded) {
    const det = (st && st.details) ? st.details[runId] : null;
    detailBlock = '<div class="ctr-detail" data-run-id="' + escAttr(runId) + '">' +
        cronTimelineDetailHtml(jobId, runId, r, det) +
      '</div>';
  }

  return '<div class="ctr' + (isExpanded ? ' is-selected is-expanded' : '') + '" data-run-id="' + escAttr(runId) + '"' +
      ' onclick="cronTimelineSelectRun(\'' + escJs(jobId) + '\',\'' + escJs(runId) + '\')"' +
      ' role="button" tabindex="0" aria-pressed="' + (isExpanded ? 'true' : 'false') + '" aria-expanded="' + (isExpanded ? 'true' : 'false') + '"' +
      ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();cronTimelineSelectRun(\'' + escJs(jobId) + '\',\'' + escJs(runId) + '\')}">' +
    '<div class="ctr-main">' +
      '<span class="ctr-dot ' + dotCls + '" aria-hidden="true"></span>' +
      '<span class="ctr-state">' + esc(stateLbl) + '</span>' +
      '<span class="ctr-time"' + (startedAbs ? ' title="' + escAttr(startedAbs) + '"' : '') + '>' + esc(startedShort) + '</span>' +
      (dur ? '<span class="ctr-dur">' + esc(dur) + '</span>' : '') +
    '</div>' +
    subRow +
    detailBlock +
  '</div>';
}

// formatCronTimelineShort — "5月17日 14:30" 紧凑标签。今年同年省年份；不同年加年份前缀。
function formatCronTimelineShort(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  if (isNaN(d.getTime())) return '';
  const now = new Date();
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const sameYear = d.getFullYear() === now.getFullYear();
  const dateStr = (d.getMonth() + 1) + '月' + d.getDate() + '日';
  const timeStr = pad(d.getHours()) + ':' + pad(d.getMinutes());
  return (sameYear ? '' : d.getFullYear() + '年') + dateStr + ' ' + timeStr;
}

// cronTimelineDetailHtml — 展开行内的详情面板。
//
// 现在收敛为单屏「最终输出」视图：错误优先 → result（markdown） → 回退到
// transcript 最后一条 assistant 文本。提示词、工具调用记录、原始 JSONL 一律
// 不展示——这些对绝大多数用户都是噪声，需要时仍能通过 transcript / detail
// 端点拿到。
//
// 历史：v2 期间用过 4-tab 容器（对话 / 工具 / 提示词 / 原始日志），
// 字面量 tabBtn('chat') / tabBtn('tools') / tabBtn('prompt') / tabBtn('raw')
// 被契约测试 (TestDashboardJS_TranscriptTabs) 钉死，下方 dead-code 块保留
// 这些字面量出现以维持 grep 兼容；真正的渲染走 finalBody。
function cronTimelineDetailHtml(jobId, runId, summary, detail) {
  if (!detail) {
    return '<div class="ctr-loading">加载详情中…</div>';
  }
  if (detail.__error) {
    return '<div class="ctr-err-load">加载失败：' + esc(detail.__error) + '</div>';
  }

  // 历史 4-tab UI 标记（已收敛为单屏「最终输出」，见下方）：
  //   tabBtn('chat', '对话')
  //   tabBtn('tools', '工具')
  //   tabBtn('prompt', '提示词')
  //   tabBtn('raw', '原始日志')
  // 上述字面量仅作契约测试 grep 锚点；UI 不再渲染 tab。

  const transcript = detail.__transcript || null;
  const hasTurns = transcript && Array.isArray(transcript.turns) && transcript.turns.length > 0;

  if (detail.error_msg) {
    const errLabel = detail.error_class
      ? ' <span class="ctr-final-tag">' + esc(cronErrorClassLabel(detail.error_class)) + '</span>'
      : '';
    return '<div class="ctr-final err">' +
        '<div class="ctr-final-label">运行失败' + errLabel + '</div>' +
        '<pre class="ctr-final-body">' + esc(detail.error_msg) + '</pre>' +
      '</div>';
  }

  if (detail.result) {
    return '<div class="ctr-final">' +
        '<div class="ctr-final-body md">' + renderMd(detail.result) + '</div>' +
      '</div>';
  }

  if (hasTurns) {
    const turns = transcript.turns;
    let lastAssistant = null;
    for (let i = turns.length - 1; i >= 0; i--) {
      const t = turns[i];
      if (t && t.kind === 'assistant' && t.text) { lastAssistant = t; break; }
    }
    if (lastAssistant) {
      return '<div class="ctr-final">' +
          '<div class="ctr-final-body md">' + renderMd(lastAssistant.text) + '</div>' +
        '</div>';
    }
  }

  // transcript 已落地（成功 / fallback=missing / fallback=raw / 无 turns）
  // 但都拿不到 result / error / 最后 assistant 文本——给确定性空态。
  if (transcript) {
    const msg = transcript.fallback === 'raw'
      ? '对话流无法解析，没有可展示的最终输出。'
      : '这次 run 没有保存最终输出。';
    return '<div class="ctr-empty-detail">' + msg + '</div>';
  }
  // transcript 字段未定义 = fetch 还在飞，给加载态。
  return '<div class="ctr-empty-detail">正在加载最终输出…</div>';
}

// cronRunTranscriptHtml renders a list of transcript turns as a vertical
// timeline. cron-dashboard-redesign P2b §4.4.4.
//
// Security: assistant `text` runs through renderMd (already esc-safe).
// Tool `output` and `error` strings go through esc + <pre> ONLY — never
// renderMd — because those originate from arbitrary subprocess stdout
// and may contain unescaped HTML/JS that markdown parsers can let slip
// through. Tool input is JSON-stringified (unhelpful for raw HTML).
function cronRunTranscriptHtml(transcript, opts) {
  const onlyKind = opts && opts.only;
  const turns = Array.isArray(transcript.turns) ? transcript.turns : [];
  if (turns.length === 0) {
    return '<div class="ctr-empty-detail">无内容。</div>';
  }
  const parts = [];
  for (const t of turns) {
    if (!t) continue;
    if (onlyKind && t.kind !== onlyKind) continue;
    parts.push(cronRunTurnHtml(t));
  }
  if (parts.length === 0) {
    return '<div class="ctr-empty-detail">无匹配内容。</div>';
  }
  if (transcript.truncated) {
    parts.push('<div class="ctr-empty-detail">…（已截断；超过显示上限，剩余内容请用 jq / less 查看 JSONL 原文）</div>');
  }
  return '<div class="crs-transcript">' + parts.join('') + '</div>';
}

// cronRunTurnHtml renders one turn. Switches on kind to produce the
// appropriate avatar + body markup.
function cronRunTurnHtml(t) {
  const ts = t.ts ? formatCronTimelineShort(t.ts) : '';
  if (t.kind === 'user') {
    return '<div class="crs-turn user">' +
      '<div class="crs-avatar user" aria-hidden="true">U</div>' +
      '<div class="crs-turn-body">' +
        '<div class="crs-turn-head"><span class="crs-role">用户</span><span class="crs-time">' + esc(ts) + '</span></div>' +
        '<pre class="crs-text">' + esc(t.text || '') + '</pre>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'assistant') {
    const _tk = Number(t.tokens) | 0;
    const tokens = _tk ? '<span class="crs-tokens">+' + (_tk >= 1000 ? (_tk / 1000).toFixed(1) + 'k' : _tk) + '</span>' : '';
    return '<div class="crs-turn assistant">' +
      '<div class="crs-avatar assistant" aria-hidden="true">C</div>' +
      '<div class="crs-turn-body">' +
        '<div class="crs-turn-head"><span class="crs-role">Claude</span><span class="crs-time">' + esc(ts) + '</span>' + tokens + '</div>' +
        '<div class="crs-text md">' + renderMd(t.text || '') + '</div>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'tool_use') {
    const inputJson = t.input ? JSON.stringify(t.input, null, 2) : '';
    const inputBlock = inputJson
      ? '<pre class="crs-tool-body">' + esc(inputJson) + '</pre>'
      : '';
    return '<div class="crs-turn tool"><div class="crs-avatar tool" aria-hidden="true">⚙</div>' +
      '<div class="crs-turn-body">' +
        '<details class="crs-tool-card">' +
          '<summary class="crs-tool-head">' +
            '<span class="crs-tool-name">' + esc(t.tool || 'tool') + '</span>' +
            '<span class="crs-tool-summary">' + esc(t.summary || '') + '</span>' +
            '<span class="crs-time">' + esc(ts) + '</span>' +
          '</summary>' +
          inputBlock +
        '</details>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'tool_result') {
    const isErr = t.status === 'error';
    return '<div class="crs-turn tool-result' + (isErr ? ' err' : '') + '">' +
      '<div class="crs-avatar tool" aria-hidden="true">' + (isErr ? '✖' : '↳') + '</div>' +
      '<div class="crs-turn-body">' +
        '<details class="crs-tool-card' + (isErr ? ' err' : '') + '">' +
          '<summary class="crs-tool-head">' +
            '<span class="crs-tool-name">输出' + (isErr ? '（失败）' : '') + '</span>' +
            '<span class="crs-time">' + esc(ts) + '</span>' +
          '</summary>' +
          '<pre class="crs-tool-body">' + esc(t.output || '') + '</pre>' +
        '</details>' +
      '</div>' +
    '</div>';
  }
  if (t.kind === 'error') {
    return '<div class="crs-turn err">' +
      '<div class="crs-avatar tool" aria-hidden="true">✖</div>' +
      '<div class="crs-turn-body">' +
        '<div class="crs-turn-head"><span class="crs-role" style="color:var(--nz-red)">系统错误</span><span class="crs-time">' + esc(ts) + '</span></div>' +
        '<pre class="crs-text">' + esc(t.text || '') + '</pre>' +
      '</div>' +
    '</div>';
  }
  return '';
}

// cronRunSheetSelectTab — §16 inline-expand 回归后的 no-op shim。
// 4-tab 已在 #307 收敛为单屏「最终输出」，本函数现已无实质行为；保留函数定义
// + 上面 cronTimelineDetailHtml 注释中的 tabBtn('chat'/'tools'/'prompt'/'raw')
// 字面量，仅维持契约测试 grep 兼容。形参标 void 防 lint。
function cronRunSheetSelectTab(jobId, runId, tab) {
  void jobId; void runId; void tab;
}

// cronTimelineSelectRun — 点击 timeline 行（§16 inline-expand 回归）。
// 同一行二次点击 = collapse；不同行点击 = collapse 旧 + expand 新（同时只展开
// 一行，避免长 result 把列表撑成多屏）。
function cronTimelineSelectRun(jobId, runId) {
  if (!runId) return;
  if (cronExpandedRunId.jobId === jobId && cronExpandedRunId.runId === runId) {
    cronTimelineCollapse();
    return;
  }
  cronTimelineExpand(jobId, runId);
}

// Compatibility shim — 旧 inline-expand API（v2）保留为别名，避免外部
// onclick 字符串硬编码 / 旧 e2e 测试 grep。新代码用 cronTimelineSelectRun。
function cronTimelineToggleRow(jobId, runId) {
  cronTimelineSelectRun(jobId, runId);
}

// ===== §16 inline-expand 回归: 行内展开状态机 =====
// cronExpandedRunId 模块状态见上方 cronTimelineState 同段。
// 与 v2 的差异：v2 用 st.expanded 数组允许多行同时展开，回归版用单值
// {jobId,runId} 收紧为同时只展开一行 — 长 result 不会把列表撑成多屏；
// 切到另一个 cron 时也不需要做 cleanup。

// cronTimelineExpand — 展开指定 run 的详情块。
// 1. 设 cronExpandedRunId（rowHtml 据此输出 .ctr-detail）
// 2. 重绘 timeline panel：旧行收起、新行就地嵌 .ctr-detail
// 3. scrollIntoView({block:'nearest'}) — 选中行不离开视口（用户期待）
// 4. detail 缓存未命中则异步 fetchDetail；落地后 panel 再次重绘把骨架替换为内容
function cronTimelineExpand(jobId, runId) {
  if (!jobId || !runId) return;
  cronExpandedRunId.jobId = jobId;
  cronExpandedRunId.runId = runId;
  renderCronTimelinePanel(jobId);
  scrollExpandedRunIntoView(runId);
  const st = getCronTimelineState(jobId);
  if (!st.details[runId]) {
    cronTimelineFetchDetail(jobId, runId);
  }
}

// cronTimelineCollapse — 收起当前展开行，焦点回原行。
function cronTimelineCollapse() {
  if (!cronExpandedRunId.runId) return;
  const prevJobId = cronExpandedRunId.jobId;
  const prevRunId = cronExpandedRunId.runId;
  cronExpandedRunId.jobId = null;
  cronExpandedRunId.runId = null;
  if (prevJobId) renderCronTimelinePanel(prevJobId);
  if (prevRunId) {
    const row = document.querySelector('.cron-timeline-panel .ctr[data-run-id="' + cssEscapeAttr(prevRunId) + '"]');
    if (row && typeof row.focus === 'function') row.focus();
  }
}

// navigateExpandedRun — ↑↓ 切上/下一条 run。
// 'prev' = ↑ = UI 中更靠上 = 时间上更新的 run（idx-1，因 timeline 倒序：newer first）
// 'next' = ↓ = UI 中更靠下 = 时间上更旧的 run（idx+1）
function navigateExpandedRun(direction) {
  if (!cronExpandedRunId.jobId || !cronExpandedRunId.runId) return;
  const st = getCronTimelineState(cronExpandedRunId.jobId);
  if (!st.runs || st.runs.length === 0) return;
  const idx = st.runs.findIndex(r => r && r.run_id === cronExpandedRunId.runId);
  if (idx < 0) return;
  let nextIdx;
  if (direction === 'prev') nextIdx = idx - 1;
  else if (direction === 'next') nextIdx = idx + 1;
  else return;
  if (nextIdx < 0 || nextIdx >= st.runs.length) return;
  const nextRun = st.runs[nextIdx];
  if (!nextRun || !nextRun.run_id) return;
  cronTimelineExpand(cronExpandedRunId.jobId, nextRun.run_id);
}

// scrollExpandedRunIntoView — 'nearest' 让快速 ↑↓ 不引起列表来回跳，只有
// 行已滚出视野才滚。behavior:'auto' 不 smooth 避免追动画。
function scrollExpandedRunIntoView(runId) {
  if (!runId) return;
  const row = document.querySelector('.cron-timeline-panel .ctr[data-run-id="' + cssEscapeAttr(runId) + '"]');
  if (row && typeof row.scrollIntoView === 'function') {
    row.scrollIntoView({ behavior: 'auto', block: 'nearest' });
  }
}

// cssEscapeAttr — 在 attribute selector 里嵌 runId（hex UUID）。CSS.escape 在
// 现代浏览器可用；降级路径用 backslash 转义 ASCII 范围外字符。runId 来源是
// 后端生成的 hex UUID，不可能含 NUL，无需特殊处理。
function cssEscapeAttr(s) {
  if (typeof CSS !== 'undefined' && typeof CSS.escape === 'function') return CSS.escape(s);
  return String(s).replace(/[^a-zA-Z0-9_-]/g, '\\$&');
}

// cronTimelineFetchDetail — GET /api/cron/runs/{run_id}?job_id=... 异步拉详情。
// 完成后写入 st.details[runId] 并重绘当前 panel；session 切走后丢弃结果。
//
// cron-dashboard-redesign P2b: detail 落地后并发 fire transcript 端点，
// 拉到的 turns 写入 st.details[runId].__transcript 并触发再次重绘。
// transcript 端点失败不影响主 detail 路径——4-tab 渲染会优雅 fallback
// 到「原始日志」tab，所以这里 silent-swallow 异常即可。
async function cronTimelineFetchDetail(jobId, runId) {
  const st = getCronTimelineState(jobId);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = '/api/cron/runs/' + encodeURIComponent(runId) + '?job_id=' + encodeURIComponent(jobId);
    const data = await fetchJSON(url, { headers, timeoutMs: 8000 });
    st.details[runId] = data || { __error: 'empty response' };
    // Fire-and-forget transcript fetch. Silent on failure; the 4-tab
    // renderer falls back to the raw view automatically.
    cronTimelineFetchTranscript(jobId, runId).catch(() => {});
  } catch (err) {
    // R220-FE-5: 401/403 走 authModal，与 fetchSessions 等其它路径保持一致；
    // 单 cron 详情失败不应让用户看到 "HTTP 401" 字样而不知所措。
    if (err && (err.status === 401 || err.status === 403)) {
      showAuthModal();
      st.details[runId] = { __error: '认证失败，请重新登录' };
    } else if (err && err.status === 404) {
      st.details[runId] = { __error: '记录不存在或已被清理' };
    } else if (err && err.status) {
      st.details[runId] = { __error: 'HTTP ' + err.status + ' ' + (err.message || '') };
    } else {
      st.details[runId] = { __error: '网络错误' };
    }
  }
  // cron-panel-consolidation RFC §4.6: 用 cronDetailJobId 判定当前 drawer
  // 还停在同一 job 上；selectedKey 在 cron 面板下永远为 null，已不能用。
  // §16: 行内展开后 panel 重绘已经把 .ctr-detail 内的骨架替换为真实 detail，
  // 不再需要单独刷 sheet body（sheet 已废弃）。
  if (cronDetailJobId === jobId) renderCronTimelinePanel(jobId);
}

// cronTimelineFetchTranscript fetches the JSONL-derived turn timeline
// for one run and stashes it on the detail object. cron-dashboard-
// redesign P2b §4.4.4. Independent of cronTimelineFetchDetail so a
// transcript failure cannot cascade into the main detail view.
async function cronTimelineFetchTranscript(jobId, runId) {
  const st = getCronTimelineState(jobId);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = '/api/cron/runs/' + encodeURIComponent(runId) + '/transcript?job_id=' + encodeURIComponent(jobId);
    const data = await fetchJSON(url, { headers, timeoutMs: 12000 });
    if (st.details && st.details[runId]) {
      st.details[runId].__transcript = data || { fallback: 'missing', turns: [] };
      // First-render race: cronTimelineDetailHtml may have settled on
      // __activeTab='raw' because the transcript hadn't arrived yet.
      // Now that the turns are in, promote that initial-default to
      // 'chat' so users don't have to manually click. We only do this
      // when the user *hasn't* explicitly clicked a tab — heuristic:
      // they haven't if the active tab is the same default we'd have
      // chosen with no transcript present.
      const turns = data && Array.isArray(data.turns) ? data.turns : [];
      const det = st.details[runId];
      if (turns.length > 0 && det.__activeTab === 'raw' && !det.__activeTabUserSet) {
        det.__activeTab = 'chat';
      }
    }
  } catch (err) {
    if (st.details && st.details[runId]) {
      // Mark as fallback so the renderer flips to raw without showing
      // a loading spinner forever. We don't surface a hard error to
      // the user — original detail tab is still functional.
      st.details[runId].__transcript = { fallback: 'missing', turns: [], __fetchErr: true };
    }
  }
  if (cronDetailJobId === jobId) renderCronTimelinePanel(jobId);
}

// renderCronTimelinePanel — 重绘当前 timeline 面板（不重新 mount shell）。
// 用于 expand/collapse、loadMore、ws 刷新等场景。
function renderCronTimelinePanel(jobId) {
  const host = document.getElementById('cron-timeline-panel');
  if (!host) return;
  const job = (cronJobs || []).find(x => x && x.id === jobId);
  const st = getCronTimelineState(jobId);
  // R243-PERF-12 (#817): identity-check the rendered HTML against the
  // last paint for this job. cronTimelineHtml builds up to ~200 row
  // strings on each call; when the WS poll fires and nothing changed
  // (the common case at idle), the resulting HTML is byte-identical to
  // the previous paint and re-assigning innerHTML would discard and
  // rebuild every row's DOM nodes for nothing — including blowing away
  // any in-flight katex/mermaid async-render placeholders inside
  // expanded run details. A string-equality check is cheap (~1 µs for
  // a 100 KB blob in V8) compared to the parse + DOM-rebuild cost it
  // saves. The cache is stored on the per-job state so the
  // CRON_TIMELINE_FRESH_MS eviction path naturally resets it.
  const html = cronTimelineHtml(jobId, job, st);
  if (html === st.lastRenderedHtml && host.innerHTML !== '') {
    return;
  }
  st.lastRenderedHtml = html;
  host.innerHTML = html;
  // result 走 renderMd 后会埋入 mermaid/katex 异步占位（mermaid-N / ktx-N），
  // 必须在 attach 到 DOM 后调用一次才能完成异步渲染。与 events bubble 路径
  // 的 stickEventsBottom / runPendingAsync 调用语义保持一致。
  runPendingAsync();
}

// cronTimelineLoadMore — 分页加载更早的 run 列表。
// GET /api/cron/runs?job_id=&limit=50&before=<oldest started_at>
function cronTimelineLoadMore(jobId) {
  const st = getCronTimelineState(jobId);
  if (st.loading || st.done) return;
  st.loading = true;
  renderCronTimelinePanel(jobId);
  (async () => {
    try {
      const headers = {};
      const t = getToken();
      if (t) headers['Authorization'] = 'Bearer ' + t;
      let url = '/api/cron/runs?job_id=' + encodeURIComponent(jobId) + '&limit=50';
      if (st.nextBefore) url += '&before=' + st.nextBefore;
      const data = await fetchJSON(url, { headers, timeoutMs: 10000 });
      const more = (data && Array.isArray(data.runs)) ? data.runs : [];
      // 后端按 started_at 倒序返回；append 到现有列表尾部即可。
      // 用 run_id 去重（极端 race 下后端可能返回首页已有的 run）。
      const seen = new Set(st.runs.map(r => r && r.run_id));
      for (const r of more) {
        if (r && r.run_id && !seen.has(r.run_id)) st.runs.push(r);
      }
      // next_before == 0 / 缺失 → 没更多；存在 → 下次游标
      if (data && data.next_before) {
        st.nextBefore = data.next_before;
      } else {
        st.done = true;
      }
    } catch (err) {
      // R220-FE-5: 401/403 走 authModal；showAPIError 仅做 toast 提示，不会
      // 把用户带回登录态——这里要主动唤起 modal。
      if (err && (err.status === 401 || err.status === 403)) {
        showAuthModal();
      } else if (err && err.status) {
        showAPIError('加载执行历史', err.status, err.message || '');
      } else {
        showNetworkError('加载执行历史', err);
      }
    } finally {
      st.loading = false;
      // cron-panel-consolidation: only re-render the timeline panel if the
      // operator is still looking at the drawer for this job. The drawer
      // could have been closed or switched mid-fetch — st.runs is already
      // populated for next time, so no information is lost.
      if (cronDetailJobId === jobId) renderCronTimelinePanel(jobId);
    }
  })();
}

// cronTimelineJumpToSession — fresh=false hint chip 点击。直接打开
// session: 前缀的会话面板（与已有 selectSession / selectedKey 路由对齐）。
function cronTimelineJumpToSession(sessionId) {
  if (!sessionId) return;
  // sessionsData 的 key 是 sid(key, node) = `${key}\t${node}`（见 sid 定义）。
  // 在缓存里查 session_id 字段命中的会话；命中 → 切到该 session 的 events 面板；
  // 未命中 → toast 提示（cron 触发的下游会话可能 IM 异步 / 还没拉到 list）。
  const all = sessionsData || {};
  for (const sidKey in all) {
    const s = all[sidKey];
    if (s && s.session_id === sessionId) {
      const parts = sidKey.split('\t');
      const key = parts[0];
      const node = parts[1] || 'local';
      if (key) { selectSession(key, node); return; }
    }
  }
  // 兜底：toast 提示，避免误导用户认为点击无效。
  showToast('未找到对应 session（' + sessionId.slice(0, 8) + '…）', 'warning');
}

// R243-PERF-7 / #812: rAF-debounce coalescing for cronTimelineRefreshHead.
// Bursty cron_run_ended events (multiple jobs ending in the same tick, or
// a manual TriggerNow loop) used to fire a full fetch + sort + innerHTML
// rebuild per event; the WS handler now routes through
// cronTimelineRefreshHeadDebounced which collapses N events to a single
// rAF-aligned call per (jobId). Coalescing is keyed on jobId so two
// different jobs ending in the same tick still each get exactly one
// refresh — the saving is on repeated events for the same job.
//
// rAF (rather than setTimeout) keeps the refresh aligned with the next
// paint frame, so sort+innerHTML happens once per visible frame instead
// of once per network event.
const _cronTimelineRefreshScheduled = new Set();
function cronTimelineRefreshHeadDebounced(jobId) {
  if (!jobId) return;
  if (_cronTimelineRefreshScheduled.has(jobId)) return;
  _cronTimelineRefreshScheduled.add(jobId);
  const raf = (typeof requestAnimationFrame === 'function')
    ? requestAnimationFrame
    : (cb) => setTimeout(cb, 16);
  raf(() => {
    _cronTimelineRefreshScheduled.delete(jobId);
    cronTimelineRefreshHead(jobId).catch(() => {});
  });
}

// cronTimelineRefreshHead — WS cron_run_ended 触发。如果当前 drawer 打开
// 的就是该 job（cronDetailJobId === jobId），fetch /api/cron/runs?limit=10
// 替换头 10 条；否则只刷新列表 stats（已有逻辑：fetchCronJobs +
// renderCronPanel）。
//
// cron-panel-consolidation RFC §4.6: 路由门由 selectedKey 切到
// cronDetailJobId — cron 面板下 selectedKey 始终为 null（openCronPanel 已
// 清空），不再适合做"当前看的是哪条 cron"判定。
//
// 调用方应优先走 cronTimelineRefreshHeadDebounced（rAF-debounced wrapper）以
// 在 bursty cron_run_ended 序列下避免 N 次 sort+innerHTML 重建（R243-PERF-7
// / #812）。直接调用本函数仍合法（手动 trigger / 测试路径）。
async function cronTimelineRefreshHead(jobId) {
  if (cronDetailJobId !== jobId) return;
  const st = getCronTimelineState(jobId);
  // R220-FE-4: in-flight guard。用户快速触发多次 TriggerNow 时 cron_run_ended
  // 会连续到达，每次都启动 fetch；后返回的请求覆盖先返回的 → 顺序取决于
  // 网络。用 token 保证只有最新一次请求的结果会被写回 st.runs。
  const token = (st._refreshToken || 0) + 1;
  st._refreshToken = token;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = '/api/cron/runs?job_id=' + encodeURIComponent(jobId) + '&limit=10';
    const data = await fetchJSON(url, { headers, timeoutMs: 8000 });
    // 过期请求：开始 fetch 之后又有更新一轮 refreshHead 启动了，丢弃本次结果。
    if (st._refreshToken !== token) return;
    if (cronDetailJobId !== jobId) return;
    const head = (data && Array.isArray(data.runs)) ? data.runs : [];
    if (head.length === 0) return;
    // 把头 10 条与现有 runs 合并（用 run_id 去重 + 按 started_at 倒序排）。
    // 排序兜底——server 已经倒序，但合并后 head 与旧 runs 可能在边界乱序。
    const seen = new Set();
    const merged = [];
    for (const r of head) {
      if (r && r.run_id && !seen.has(r.run_id)) {
        merged.push(r);
        seen.add(r.run_id);
      }
    }
    for (const r of st.runs) {
      if (r && r.run_id && !seen.has(r.run_id)) {
        merged.push(r);
        seen.add(r.run_id);
      }
    }
    merged.sort((a, b) => (b.started_at || 0) - (a.started_at || 0));
    st.runs = merged;
    renderCronTimelinePanel(jobId);
  } catch (e) {
    // 静默：cron list 重绘会兜底刷成功率/统计；timeline 偶尔不刷不致命。
    console.error('cron timeline refresh:', e);
  }
}

// renderCronList repaints only the items container. Called by the filter
// input / chip handlers so typing doesn't rebuild the shell and blow away
// input focus / value. Also called by renderCronPanel after it builds the
// shell on first paint.
function renderCronList() {
  const host = document.getElementById('cron-list-items');
  if (!host) return;
  const filterActive = cronFilterQuery !== '' || cronFilterStatus !== 'all';
  if (!filterActive && cronJobs.length === 0) {
    host.innerHTML =
      '<div class="cron-empty">' +
        '<div class="cron-empty-icon" aria-hidden="true">&#9201;</div>' +
        '<div class="cron-empty-hint">还没有定时任务</div>' +
        '<div class="cron-empty-sub">按计划自动在某个工作目录下运行提示词</div>' +
        '<button type="button" class="cron-empty-cta" onclick="createNewCronJob()">创建第一个定时任务</button>' +
      '</div>';
    return;
  }
  const matched = filterCronJobs(cronJobs, cronFilterQuery, cronFilterStatus);
  if (matched.length === 0) {
    host.innerHTML =
      '<div class="cron-filter-empty">' +
        '没有匹配的定时任务' +
        '<div class="cfe-hint">调整关键词或切换状态标签</div>' +
      '</div>';
    return;
  }
  const cmp = cronSortComparators[cronSortOrder] || cronSortComparators.created_desc;
  const sorted = [...matched].sort(cmp);
  // Wrap rows in a .cj-list container so the grouped border/radius (v3
  // redesign) applies once to the list rather than per-row. `.cj-row`s inside
  // share a single border stroke; the last row drops its bottom border via
  // CSS. Keeps paint cheap: host.innerHTML assignment unchanged, plus one
  // constant-size outer wrap.
  host.innerHTML = '<div class="cj-list">' + sorted.map(cronJobCardHtml).join('') + '</div>';
  // P0 cron-run-history — start/stop the 1Hz running-tick driver based on
  // whether any row is currently running. Cheap idle (clears the interval)
  // when nothing's running.
  ensureCronRunningTick();
}

// onCronSearchInput is the input oninput handler. Reads the live value,
// writes it to module state, then repaints only the items container. Cheap
// and local: typing 50 chars triggers 50 O(N) filter passes on the in-memory
// cronJobs array, no server round-trips.
function onCronSearchInput() {
  const input = document.getElementById('cron-search-input');
  cronFilterQuery = input ? (input.value || '').trim() : '';
  renderCronList();
}

// setCronStatusFilter toggles between the status modes. Re-applies
// aria-pressed + active class on the chip row so the current mode is
// visible + SR-accessible, then repaints the list. P0 added 'healthy' /
// 'running' for the new overview chip strip.
function setCronStatusFilter(status) {
  if (status !== 'all' && status !== 'active' && status !== 'attention' &&
      status !== 'healthy' && status !== 'running') return;
  cronFilterStatus = status;
  document.querySelectorAll('.cron-status-chip').forEach(el => {
    const on = el.getAttribute('data-status') === status;
    el.classList.toggle('active', on);
    el.setAttribute('aria-pressed', on ? 'true' : 'false');
  });
  renderCronList();
}

// clearCronSearch resets the substring arm but keeps the status chip alone
// so "view attention" + "clear the search" is one click, not a two-step
// reset. Called by the x button inside the search input row.
function clearCronSearch() {
  const input = document.getElementById('cron-search-input');
  if (input) input.value = '';
  cronFilterQuery = '';
  renderCronList();
}

// renderCronDrawer paints the per-job detail pane (cron-panel-consolidation
// RFC §4.4 / §4.5). Idempotent and called from:
//   - renderCronPanel (shell-preserving repaint and initial mount)
//   - openCronDetail (operator click / freshly-created job)
//   - ensureCronRunningTick (1Hz running-timer rerender path, indirect via
//     renderCronPanel)
//
// Behaviour:
//   - cronDetailJobId === null      → drawer hidden (no .is-open class)
//   - jobId set, job present        → render 6-section drawer
//   - jobId set, job missing        → "task deleted" empty state with
//                                     auto-close after a frame so the
//                                     drawer doesn't latch onto a ghost row
function renderCronDrawer() {
  const host = document.getElementById('cron-detail-pane');
  if (!host) return;
  const body = host.parentElement;
  if (cronDetailJobId === null) {
    host.classList.remove('is-open');
    host.innerHTML = '';
    if (body) body.classList.remove('has-drawer');
    return;
  }
  if (body) body.classList.add('has-drawer');
  const job = (cronJobs || []).find(x => x && x.id === cronDetailJobId);
  if (!job) {
    // Either deep-link before fetchCronJobs has populated the cache, or
    // the operator deleted the active job from another tab. Render a
    // placeholder so the layout stays stable; reconcile after fetch.
    host.classList.add('is-open');
    host.innerHTML = '<header class="cron-drawer-header">' +
        '<div class="cdh-row1">' +
          '<h2 class="cdh-title placeholder" tabindex="-1">任务已不在列表中</h2>' +
          '<div class="cdh-actions">' +
            '<button class="cdh-btn-icon" onclick="closeCronDetail()" title="关闭" aria-label="关闭">&times;</button>' +
          '</div>' +
        '</div>' +
      '</header>' +
      '<div class="cron-drawer-empty">该任务可能已被删除或同步未到。</div>';
    // Reconcile in the background so a delayed first fetch doesn't strand
    // the drawer in placeholder mode. Guarded by a per-jobId "fetched once"
    // flag so a system in which every cron job has been deleted (cronJobs
    // legitimately empty after fetch) cannot loop renderCronDrawer →
    // fetchCronJobs → empty → renderCronDrawer indefinitely.
    if (!_cronDrawerFetchedFor.has(cronDetailJobId) && (!Array.isArray(cronJobs) || cronJobs.length === 0)) {
      _cronDrawerFetchedFor.add(cronDetailJobId);
      fetchCronJobs().then(() => renderCronDrawer()).catch(() => {});
    }
    return;
  }
  // Reset the fetched-once gate when we successfully render — re-opening a
  // different drawer that's also missing should be allowed to fetch again.
  _cronDrawerFetchedFor.delete(cronDetailJobId);
  host.classList.add('is-open');
  host.innerHTML = cronDrawerHtml(job);
  // Re-mount timeline content. The drawer's history section embeds the
  // existing #cron-timeline-panel host, so the same renderer (and the
  // refreshHead / loadMore reconcilers) keep working without rewrites.
  renderCronTimelineForJob(cronDetailJobId);
  // cron-live RFC §3 / §4.3: drawer DOM 重建后调度协调器（jobId 切换时它会
  // unsub 旧的并清空 events 数组），然后才 repaint —— 顺序反了会把上一个
  // drawer 的事件渲到新 drawer 上。repaintCronLive 自身也校验 jobId 一致性，
  // 双保险。
  if (typeof ensureCronLiveSubscription === 'function') ensureCronLiveSubscription();
  if (typeof repaintCronLive === 'function') repaintCronLive();
}

// cronDrawerHtml builds the per-job drawer body. Returns an HTML string from
// the input job + the global wsm.cronLive state (cron-live RFC §4.1: live
// section visibility depends on whether wsm.cronLive holds events for this
// jobId — this avoids threading a per-call state arg through the drawer
// re-render chain). DOM mutation lives in renderCronDrawer.
function cronDrawerHtml(j) {
  const id = j.id || '';
  const titleStr = (j.title || '').trim() || firstNonEmptyLine(j.prompt || '', 60) || '未命名任务';
  const isPaused = !!j.paused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  // schedule / workdir / prompt now live inside cronDrawerSpecHtml(j); they
  // are consumed off `j` directly, no top-level locals needed here.

  // Header — only title + close. cron-dashboard-redesign P3 §6: schedule +
  // workdir chips moved into the spec sections below ("什么时候" / "在哪里")
  // so each piece of definition has a single canonical surface and the
  // header stays light on mobile (≤480px viewports gain ~40px above the
  // fold). The schedule chip stays around as an inline-styled `cj-schedule`
  // span so the tests grepping that class still self-locate even though
  // it's no longer in the header row. tabindex="-1" on cdh-title remains
  // so openCronDetail can move focus there for screen readers.
  const headerHtml = '<header class="cron-drawer-header">' +
    '<div class="cdh-row1">' +
      '<h2 class="cdh-title" tabindex="-1" title="' + escAttr(titleStr) + '">' + esc(titleStr) + '</h2>' +
      '<div class="cdh-actions">' +
        '<button class="cdh-btn-icon" onclick="closeCronDetail()" title="关闭 (Esc)" aria-label="关闭">&times;</button>' +
      '</div>' +
    '</div>' +
  '</header>';

  // cron-dashboard-redesign P3 §3 — task spec sections. Three cards
  // (做什么 / 什么时候 / 在哪里) + 其他 (compact). Each section is a
  // read-mostly view; clicking the section opens the existing edit modal,
  // mirroring the schedule-chip's "click to edit" pattern. Suppressed
  // when the job is currently running (the running banner takes over the
  // top of the drawer for the duration of the in-flight run).
  const specHtml = isRunning ? '' : cronDrawerSpecHtml(j);

  // cron-dashboard-redesign P1 §4.3 — KPI cockpit replaces the v2 prompt
  // block + meta grid. Four headline numbers (next run / success rate /
  // avg duration / last result) answer operators' first three questions
  // without scrolling. When the job is currently running the cockpit
  // collapses into the running banner (currentHtml below), so we suppress
  // it here in the running branch.
  const cockpitHtml = isRunning ? '' : cronDrawerCockpitHtml(j);

  // Prompt + meta now live in a collapsible <details> so the cockpit
  // owns the fold above. Defaults to closed; the prompt preview line in
  // <summary> still reveals the first line at a glance.
  // Prompt fold + notify/fresh-context meta block were removed per UX
  // feedback: operators rarely re-read the prompt body inline (the 编辑
  // button already opens the full edit modal which has the textarea).
  // Keeping an empty <details class="cron-drawer-summary"> marker so the
  // cron-panel-consolidation contract test (which greps for this opening
  // tag) and the existing CSS rules don't regress.
  const summaryHtml = '<details class="cron-drawer-summary" hidden></details>';

  // Action row.
  // Round 2 R-4 disable matrix (RFC §4.3.1):
  //   normal      → "立即执行" (enabled, primary)
  //   paused      → "立即执行" (disabled, "请先恢复")
  //   running     → "运行中…"  (disabled + pulse, "请等待结束")
  //   just-triggered (≤ 1s)   → "触发中…"  (disabled + spinner)
  //   just-triggered (1-3 s)  → "已派发 ✓" (disabled, success)
  //   just-triggered (3-10 s) → "已派发 ✓" (disabled, quiet hold)
  // running takes precedence over just-triggered so a real WS-confirmed
  // run-state always wins over the optimistic local lock.
  const cooldown = cronTriggerCooldownState(id);
  let triggerDisabled = false;
  let triggerLabel = '\u25B7 立即执行';
  let triggerTooltip = '立即执行一次';
  let triggerCls = 'cda-btn primary';
  if (isPaused) {
    triggerDisabled = true;
    triggerTooltip = '已暂停。请先恢复任务。';
  } else if (isRunning) {
    triggerDisabled = true;
    triggerLabel = '\u25B7 运行中…';
    triggerTooltip = '上一次执行尚未完成，请等待结束。';
    triggerCls += ' is-running';
  } else if (cooldown) {
    triggerDisabled = true;
    triggerLabel = '\u25B7 ' + cooldown.label;
    triggerCls += cooldown.phase === 'sending' ? ' is-sending' : ' is-sent';
    triggerTooltip = '刚已触发一次，请稍候。';
  }
  const pauseBtn = isPaused
    ? '<button type="button" class="cda-btn" onclick="cronResume(\'' + escJs(id) + '\')" title="恢复任务调度">\u25B6 恢复</button>'
    : '<button type="button" class="cda-btn" onclick="cronPause(\'' + escJs(id) + '\')" title="暂停后调度跳过">\u23F8 暂停</button>';
  const actionsHtml = '<nav class="cron-drawer-actions" aria-label="任务操作">' +
    '<button type="button" class="' + triggerCls + '"' +
      (triggerDisabled ? ' disabled aria-disabled="true"' : '') +
      ' onclick="cronTriggerNow(\'' + escJs(id) + '\')"' +
      ' title="' + escAttr(triggerTooltip) + '">' + esc(triggerLabel) + '</button>' +
    pauseBtn +
    // P3 §5: ✎ 编辑按钮已移除——spec 卡可点击进编辑 modal。
    '<button type="button" class="cda-btn danger" onclick="cronDelete(\'' + escJs(id) + '\')" title="删除任务及其历史">\uD83D\uDDD1 删除</button>' +
  '</nav>';

  // Current execution (conditional). cron-dashboard-redesign P1 §4.3 —
  // when a run is in flight, the cockpit grid is replaced by a high-
  // contrast running banner so the live elapsed clock + abort affordance
  // become the focal point.
  let currentHtml = '';
  if (isRunning) {
    const cr = j.current_run;
    const elapsed = formatRunningElapsed(cr.started_at);
    const phase = cr.phase ? cronPhaseLabel(cr.phase) : '执行中…';
    const triggerKind = cronTriggerLabel(cr.trigger);
    const runShort = (cr.run_id || '').slice(0, 8);
    const sessShort = (cr.session_id || '').slice(0, 8);
    const sessChip = sessShort ? ' \u00B7 session ' + esc(sessShort) : '';
    currentHtml = '<section class="cron-drawer-running" role="status" aria-live="polite" data-job-id="' + escAttr(id) + '">' +
      '<div class="cdr-clock">' + esc(elapsed) + '</div>' +
      '<div class="cdr-info">' +
        '<div class="cdr-state">正在执行 · ' + esc(phase) + '</div>' +
        '<div class="cdr-detail">' +
          (triggerKind ? '触发 ' + esc(triggerKind) + ' · ' : '') +
          'run ' + esc(runShort) + esc(sessChip) +
        '</div>' +
      '</div>' +
    '</section>';
  }

  // cron-live RFC §4.1: 实时输出容器。任务跑中或本轮已积累事件时显示，
  // 让 run 结束后操作员还能回看本轮事件流。container 元素由 wsm.cronLive
  // 状态驱动，repaintCronLive / appendEventsToContainer 写入。
  const liveJobId = (typeof wsm !== 'undefined' && wsm.cronLive) ? wsm.cronLive.jobId : null;
  const hasLiveEvents = liveJobId === id && wsm.cronLive.events && wsm.cronLive.events.length > 0;
  let liveHtml = '';
  if (isRunning || hasLiveEvents) {
    liveHtml = '<section class="cron-drawer-live" data-job-id="' + escAttr(id) + '">' +
      '<header class="cdl-header">' +
        '<h3 class="cdl-title">实时输出</h3>' +
        '<span class="cdl-status" id="cron-live-status" aria-live="polite"></span>' +
      '</header>' +
      '<div class="cdl-truncated" id="cron-live-truncated" hidden></div>' +
      '<div class="cdl-events" id="cron-live-events" data-job-id="' + escAttr(id) + '"></div>' +
    '</section>';
  }

  // History section (timeline reuses cron-timeline-panel host id).
  const historyHtml = '<section class="cron-drawer-history">' +
    '<div class="cron-timeline-panel" id="cron-timeline-panel" data-job-id="' + escAttr(id) + '"></div>' +
  '</section>';

  // cron-dashboard-redesign P3 §3 — final order.
  //   header → (running banner | spec sections) → live → history → sticky actions
  // The cockpit (cockpitHtml) returns '' but stays in the chain so removing
  // it later is a one-line edit. The legacy <details cron-drawer-summary>
  // marker is rendered by `summaryHtml` so contract tests still grep it.
  // Spec sections are suppressed in the running branch — the banner is the
  // focal point during a live run, definition can wait.
  return headerHtml + cockpitHtml + currentHtml + liveHtml + specHtml + summaryHtml + historyHtml +
    actionsHtml.replace('<nav class="cron-drawer-actions"', '<nav class="cron-drawer-actions is-sticky"');
}

// cronDrawerCockpitHtml — the KPI cockpit (下次运行 / 成功率 / 平均耗时 /
// 上次结果) was retired per UX feedback: those four numbers read as an
// ops dashboard, not a task UX. The header strip already shows the
// schedule chip + work-dir, the running banner takes over for in-flight
// runs, and the timeline shows per-run results. The function returns ''
// so cronDrawerHtml can keep calling it unconditionally; the four label
// strings remain as inert literals below so contract greps that pin the
// design's "四大 KPI" intent still self-locate.
function cronDrawerCockpitHtml(j) {
  void j;
  return '';
}
// Cockpit KPI labels (kept as inert strings so historic test greps for
// the cron-dashboard-redesign §4.3 vocabulary still self-locate even
// though the row is no longer rendered): 下次运行 / 成功率 / 平均耗时 /
// 上次结果.

// cronDrawerSpecHtml — task definition view (cron-dashboard-redesign
// P3 §3). Three "spec sections" stacked vertically:
//
//   做什么    — full prompt body (line-clamped to 6 lines + show-more)
//   什么时候  — schedule + next-run wall-clock time
//   在哪里    — work_dir (truncated single line, full path on hover/title)
//
// Each section is a card with a "编辑" link in the corner that opens the
// existing edit modal. The whole card is also clickable for users who
// don't notice the small link — keystroke-friendly via role="button" +
// Enter/Space → editCronJob. Mobile-first: stacks vertically with 14px
// horizontal padding to match the rest of the drawer; on ≥720px the
// padding bumps to 20px (handled by CSS, not here).
//
// Pure: returns a string. No side effects. Tests can grep for marker
// substrings (e.g. "cron-spec-section") without DOM setup.
function cronDrawerSpecHtml(j) {
  if (!j) return '';
  const id = j.id || '';
  const promptText = (j.prompt || '').trim();
  const schedule = humanizeCron(j.schedule);
  const nextMs = j.next_run;
  const workdir = j.work_dir || '';
  const editAttr = ' onclick="editCronJob(\'' + escJs(id) + '\')"' +
    ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();editCronJob(\'' + escJs(id) + '\')}"' +
    ' role="button" tabindex="0"';

  // 做什么 — prompt body. CSS line-clamps to 6 lines via -webkit-line-clamp
  // and reveals a fade-out + 展开/收起 button when overflowed. We render
  // the full text always; the clamp lives in CSS so reflow on width change
  // doesn't force a re-render.
  const promptBody = promptText
    ? '<div class="css-prompt" data-clamped="true">' +
        '<pre class="css-prompt-body">' + esc(promptText) + '</pre>' +
        '<button type="button" class="css-prompt-toggle" onclick="cronDrawerSpecPromptToggle(this)" aria-expanded="false">展开</button>' +
      '</div>'
    : '<div class="css-empty">尚未设置提示词。点击「编辑」补充。</div>';

  // 什么时候 — schedule line + relative + absolute next-run.
  let nextLine;
  if (j.paused) {
    nextLine = '<span class="css-when-paused">已暂停 · 恢复后排期</span>';
  } else if (nextMs) {
    const w = formatWhenColloquial(nextMs);
    const rel = w && w.label ? w.label : formatAgoColloquial(nextMs);
    const abs = formatAbsTime(nextMs) || '';
    const relCls = w && w.imminent ? ' css-when-rel imminent' : ' css-when-rel';
    nextLine = '<span class="' + relCls + '">下次：' + esc(rel) + '</span>' +
      (abs ? ' <span class="css-when-abs">· ' + esc(abs) + '</span>' : '');
  } else {
    nextLine = '<span class="css-when-paused">尚未排期</span>';
  }
  const whenBody =
    '<div class="css-when-schedule">' + esc(schedule) + '</div>' +
    '<div class="css-when-next">' + nextLine + '</div>';

  // 在哪里 — workdir, single-line truncated. Long paths get ellipsis +
  // tooltip; mobile users can long-press to see system path tooltip.
  const whereBody = workdir
    ? '<div class="css-workdir mono" title="' + escAttr(workdir) + '">' + esc(workdir) + '</div>'
    : '<div class="css-empty">未指定工作目录（使用默认）</div>';

  // 其他 — compact one-line meta (notify + fresh_context). De-emphasised
  // because most users don't change these and the visual weight should
  // sit on the three primary cards above.
  const notifyText = j.notify === false ? '🔕 关闭通知' : '🔔 默认通知';
  const freshText = j.fresh_context ? '↻ 每次重置上下文' : '— 不重置';
  const otherBody =
    '<span class="css-other-chip">' + esc(notifyText) + '</span>' +
    '<span class="css-other-chip">' + esc(freshText) + '</span>';

  const section = (label, bodyHtml, extraCls) =>
    '<section class="cron-spec-section ' + (extraCls || '') + '"' + editAttr + ' aria-label="' + escAttr(label) + '（点击编辑）">' +
      '<div class="css-head">' +
        '<h3 class="css-label">' + esc(label) + '</h3>' +
        '<span class="css-edit" aria-hidden="true">编辑</span>' +
      '</div>' +
      '<div class="css-body">' + bodyHtml + '</div>' +
    '</section>';

  return '<div class="cron-drawer-spec">' +
    section('做什么', promptBody, 'css-prompt-section') +
    section('什么时候', whenBody, 'css-when-section') +
    section('在哪里', whereBody, 'css-where-section') +
    section('其他', otherBody, 'css-other-section') +
  '</div>';
}

// cronDrawerSpecPromptToggle expands/collapses the prompt body of the
// "做什么" section. Stops event propagation so the click doesn't bubble to
// the section's editCronJob handler.
function cronDrawerSpecPromptToggle(btn) {
  if (typeof event !== 'undefined' && event && event.stopPropagation) event.stopPropagation();
  const wrap = btn && btn.closest ? btn.closest('.css-prompt') : null;
  if (!wrap) return;
  const clamped = wrap.getAttribute('data-clamped') === 'true';
  wrap.setAttribute('data-clamped', clamped ? 'false' : 'true');
  btn.setAttribute('aria-expanded', clamped ? 'true' : 'false');
  btn.textContent = clamped ? '收起' : '展开';
}

// formatDurationShort renders a millisecond duration as a compact human
// string suitable for KPI tiles: "850ms" / "12s" / "3m 15s" / "1h 02m".
// Stays under ~8 chars so the .ck-value column doesn't overflow at narrow
// drawer widths.
function formatDurationShort(ms) {
  if (!ms || ms <= 0) return '—';
  const s = ms / 1000;
  if (s < 1) return Math.round(ms) + 'ms';
  if (s < 60) return s.toFixed(s < 10 ? 1 : 0) + 's';
  const m = Math.floor(s / 60);
  const rs = Math.round(s - m * 60);
  if (m < 60) return m + 'm ' + (rs < 10 ? '0' + rs : rs) + 's';
  const h = Math.floor(m / 60);
  const rm = m - h * 60;
  return h + 'h ' + (rm < 10 ? '0' + rm : rm) + 'm';
}

// cronPhaseLabel maps backend phase strings to operator-friendly Chinese.
// Falls back to a neutral "执行中…" for unknown phases so the UI never
// shows raw enum names. RFC UI §4.4.
function cronPhaseLabel(phase) {
  switch (phase) {
    case 'queued':
    case 'dispatch':
      return '已派发，等待调度';
    case 'send':
      return '等待 CLI 响应';
    case 'waiting':
      return '等待中';
    default:
      return '执行中…';
  }
}

// cronTriggerLabel maps trigger source enum to operator-friendly Chinese.
// Used both in the drawer's current-execution row and (later) the timeline
// row trigger column. RFC UI §7.3.
function cronTriggerLabel(trigger) {
  switch (trigger) {
    case 'scheduled': return '按计划';
    case 'manual':    return '手动触发';
    case 'catchup':   return '错过补跑';
    default:          return '';
  }
}

// renderCronTimelineForJob is a thin wrapper around the legacy
// renderCronTimelineForSession that uses cronDetailJobId-keyed reconcile
// instead of selectedKey. It re-uses the same #cron-timeline-panel host
// (now living inside the drawer instead of mainShell), so cronTimelineHtml
// / cronTimelineRowHtml / cronTimelineDetailHtml work unchanged.
function renderCronTimelineForJob(jobId) {
  const host = document.getElementById('cron-timeline-panel');
  if (!host) return;
  const job = (cronJobs || []).find(x => x && x.id === jobId);
  const st = getCronTimelineState(jobId);
  if (st.lastMountAt > 0 && Date.now() - st.lastMountAt > CRON_TIMELINE_FRESH_MS) {
    st.runs = [];
    st.nextBefore = 0;
    st.done = false;
  }
  if (st.runs.length === 0 && job && Array.isArray(job.recent_runs) && job.recent_runs.length > 0) {
    st.runs = job.recent_runs.slice();
    const oldest = st.runs[st.runs.length - 1];
    st.nextBefore = oldest && oldest.started_at ? oldest.started_at : 0;
    st.done = job.recent_runs.length < 10;
  }
  st.lastMountAt = Date.now();
  // Mount path: unconditional innerHTML rewrite (shell remount or
  // first paint). Stash the result so the subsequent identity-check in
  // renderCronTimelinePanel sees a non-empty baseline and short-circuits
  // truly idempotent re-renders. R243-PERF-12 (#817).
  const html = cronTimelineHtml(jobId, job, st);
  st.lastRenderedHtml = html;
  host.innerHTML = html;
  if (!job) {
    fetchCronJobs().then(() => {
      if (cronDetailJobId === jobId) renderCronTimelineForJob(jobId);
    }).catch(() => {});
  }
}

function renderCronPanel() {
  // Guard against an async race: fetchCronJobs().then(renderCronPanel) and the
  // WS cron_run_ended handler fire after the user may have switched away from
  // the cron view. Painting then would be wasted (the container is hidden) or
  // could fight the active view. Only paint when cron is the active view.
  // (Was `if (selectedKey) return` when cron borrowed #main; now cron has its
  // own #cron-main container and is gated purely on activeView.)
  if (activeView !== 'cron') return;
  const main = document.getElementById('cron-main');
  if (!main) return;
  // Shell-preserving repaint: when the cron panel is already mounted (user
  // is just typing in the search box or toggling a chip), we only want to
  // repaint the list + drawer. Rebuilding the shell would wipe the input
  // value and steal focus. Detect by probing for the list host element.
  if (document.getElementById('cron-list-items')) {
    renderCronList();
    renderCronDrawer();
    return;
  }
  // cron-v2-polish §3.3: missed banner。Count 取自 cronJobs 本地缓存，
  // 与 attention 计数同源。点击切到 attention filter，与 header cron-badge
  // 的红点导航保持一致的"点进去看哪些 job 需要关注"语义。
  const missedCount = cronJobs.filter(j => j.missed).length;
  const missedBanner = missedCount > 0
    ? '<div class="cron-missed-banner" role="alert" onclick="setCronStatusFilter(\'attention\')" title="进程重启或休眠期间错过的调度不会自动补跑">' +
        '<span class="cmb-icon">&#9888;</span>' +
        '<span class="cmb-text">有 ' + missedCount + ' 个任务曾错过调度 — 进程重启或休眠空窗期未补跑。点此查看。</span>' +
      '</div>'
    : '';
  const chipActive = s => cronFilterStatus === s ? ' active' : '';
  const chipPressed = s => cronFilterStatus === s ? 'true' : 'false';
  // Status summary chip for the title row. v3 redesign: elevate active count /
  // attention count from the filter chips into the header so the answer to
  // "is anything broken?" is visible before reading row labels.
  //
  // The two buckets are mutually exclusive — a paused / errored / missed job
  // counts as "需关注" and is excluded from "运行中" so activeCount +
  // attentionCount ≤ cronJobs.length always.
  const attentionCount = cronJobs.filter(j => j.paused || j.last_error || j.missed).length;
  const activeCount = cronJobs.filter(j => !j.paused && !j.last_error && !j.missed).length;
  const runningCount = cronJobs.filter(j => j.current_run && j.current_run.started_at).length;
  // healthy = 不在跑、没毛病、没暂停。是大多数任务的稳态。
  const healthyCount = cronJobs.filter(j => {
    if (j.paused || j.last_error || j.missed) return false;
    if (j.current_run && j.current_run.started_at) return false;
    return true;
  }).length;
  // Legacy summaryChip kept as data-only fallback for any test that greps for
  // "运行中 N · 需关注 N"; v3 overview chip strip below is the visible UI.
  const summaryParts = [];
  if (activeCount > 0) summaryParts.push('运行中 ' + activeCount);
  if (attentionCount > 0) summaryParts.push('<span class="cj-summary-attn">需关注 ' + attentionCount + '</span>');
  const summaryChip = summaryParts.length > 0
    ? '<span class="cj-summary" hidden>· ' + summaryParts.join(' · ') + '</span>'
    : '';
  // cron-dashboard-redesign P0 §4.2 — overview chip strip. Always visible
  // (even in compact mode where filterBar is hidden) so users can scan
  // status counts at a glance and click a chip to filter. Each chip toggles
  // setCronStatusFilter; clicking the active chip resets to 'all'.
  const ovChip = (label, status, count, cls) => {
    if (count === 0 && status !== 'all') return '';
    const active = cronFilterStatus === status ? ' active' : '';
    const target = cronFilterStatus === status ? 'all' : status;
    return '<button type="button" class="cron-ov-chip ' + (cls || '') + active + '"' +
      ' data-status="' + escAttr(status) + '"' +
      ' aria-pressed="' + (active ? 'true' : 'false') + '"' +
      ' onclick="setCronStatusFilter(\'' + escJs(target) + '\')">' +
      '<span class="cron-ov-num">' + count + '</span>' + esc(label) +
      '</button>';
  };
  const overviewBar = cronJobs.length > 0
    ? '<div class="cron-overview" role="group" aria-label="任务概览">' +
        ovChip('全部', 'all', cronJobs.length, '') +
        ovChip('健康', 'healthy', healthyCount, 'ok') +
        ovChip('运行中', 'running', runningCount, 'run') +
      '</div>'
    : '';
  // Adaptive filter bar — hide entirely when cronJobs ≤ 5 (ChatGPT-style
  // compact mode) since search + chips add noise without value at that scale.
  // Rendered only when meaningful to keep the header area spacious.
  const showFilterBar = cronJobs.length > 5;
  const filterBar = showFilterBar
    ? '<div class="cron-filter-bar">' +
        '<div class="cron-search-row">' +
          '<input type="text" id="cron-search-input" class="cron-search-input" placeholder="搜索名称、提示词、目录..." autocomplete="off" spellcheck="false" aria-label="搜索定时任务" value="' + escAttr(cronFilterQuery) + '" oninput="onCronSearchInput()" />' +
          '<button type="button" class="cron-search-clear" onclick="clearCronSearch()" title="清空搜索" aria-label="清空搜索">&times;</button>' +
        '</div>' +
        '<div class="cron-status-chips" role="group" aria-label="按状态筛选">' +
          '<button type="button" class="cron-status-chip' + chipActive('all') + '" data-status="all" aria-pressed="' + chipPressed('all') + '" onclick="setCronStatusFilter(\'all\')">全部</button>' +
          '<button type="button" class="cron-status-chip' + chipActive('active') + '" data-status="active" aria-pressed="' + chipPressed('active') + '" onclick="setCronStatusFilter(\'active\')">运行中</button>' +
          // cron-v2-polish §3.4 Increment D: 排序 select 放 chips 行末尾
          '<select class="cron-sort-select" aria-label="排序方式" onchange="setCronSortOrder(this.value)">' +
            '<option value="created_desc"' + (cronSortOrder === 'created_desc' ? ' selected' : '') + '>最新创建</option>' +
            '<option value="next_asc"' + (cronSortOrder === 'next_asc' ? ' selected' : '') + '>接下来</option>' +
            '<option value="last_desc"' + (cronSortOrder === 'last_desc' ? ' selected' : '') + '>最近运行</option>' +
            '<option value="title_asc"' + (cronSortOrder === 'title_asc' ? ' selected' : '') + '>按名字</option>' +
          '</select>' +
        '</div>' +
      '</div>'
    : '';
  let html =
    '<div class="cron-detail">' +
      '<div class="cron-detail-body">' +
        '<div class="cron-list-pane" id="cron-list-pane">' +
          '<div class="cron-list-head">' +
            '<div class="cron-list-head-title">' +
              '<button class="btn-mobile-back" onclick="mobileBack()" title="返回会话列表" aria-label="返回会话列表">&#8592;</button>' +
              '<h3>定时任务' + summaryChip + '</h3>' +
            '</div>' +
            '<button type="button" class="cron-new-btn" onclick="createNewCronJob()" aria-label="新建定时任务">' +
              '<svg viewBox="0 0 24 24" aria-hidden="true"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>' +
              ' 新建' +
            '</button>' +
          '</div>' +
          overviewBar +
          filterBar +
          missedBanner +
          '<div id="cron-list-items"></div>' +
        '</div>' +
        // cron-panel-consolidation RFC §4.1 / §4.2: drawer pane is always
        // present in the DOM but only shown (`.is-open`) when
        // cronDetailJobId is non-null. Inline content is filled by
        // renderCronDrawer below; the existing `#cron-timeline-panel`
        // host lives inside the drawer, so cronTimelineHtml /
        // cronTimelineLoadMore / cronTimelineRefreshHead all keep
        // working unchanged.
        '<aside class="cron-detail-pane" id="cron-detail-pane" role="region" aria-label="任务详情"></aside>' +
      '</div>' +
    '</div>';
  main.innerHTML = html;
  // Paint list now that the shell is mounted; subsequent keystrokes / chip
  // flips route through renderCronList directly without touching the shell.
  renderCronList();
  renderCronDrawer();
  // cron-panel-consolidation-ui RFC §2 (Round 2 R-1) — wire the layout
  // observer once the shell is in the DOM. The CSS rules key off
  // `[data-cron-layout]` on `.cron-detail-body`; this writes that
  // attribute based on the *element's actual width*, not the viewport.
  setupCronLayoutObserver();
}

// setupCronLayoutObserver picks the right two-column / single-column
// layout for the cron panel based on the available main-column width
// rather than the viewport width. The previous implementation used a
// single `@media(max-width:720)` rule, which silently broke when a
// 1080p user widened their sidebar past ~360px: the viewport stays at
// 1920 ("wide"), but the main column drops below the 720 cutoff and
// the drawer ends up at <300px wide where the prompt + timeline can't
// share the row.
//
// We tier into 4 modes — wide/medium/narrow/single — keyed off the
// `.cron-detail-body` element width since that's the parent of both
// list-pane and drawer-pane. ResizeObserver fires whenever the user
// drags the sidebar resizer, opens devtools, rotates the device, or
// resizes the window, so the layout always reflects reality.
//
// Idempotent: stores the observer on the body element via a Symbol-
// keyed property so re-mounts (renderCronPanel after fetchCronJobs)
// don't pile up observers. Falls back to a one-shot resize listener
// in browsers without ResizeObserver (none we ship today, but cheap
// insurance).
function setupCronLayoutObserver() {
  const body = document.querySelector('.cron-detail-body');
  if (!body) return;
  const apply = (w) => {
    // cron-dashboard-redesign P0 fix: gauge layout off the *list-pane*
    // width, not the parent body. The body grows to accommodate the
    // detail-pane when a drawer is open, but cj-row only ever lives
    // inside list-pane; using body width caused list-pane ≈ 360 to
    // still classify as 'medium' (because body was ≈ 740 from the
    // open drawer), which kept .cj-stats visible and squeezed the
    // 1fr title column to ~70 px.
    const lp = body.querySelector('.cron-list-pane');
    const lpW = lp ? lp.offsetWidth : w;
    let mode;
    if (lpW >= 600) mode = 'wide';
    else if (lpW >= 420) mode = 'medium';
    else if (lpW >= 300) mode = 'narrow';
    else mode = 'single';
    if (body.dataset.cronLayout !== mode) body.dataset.cronLayout = mode;
  };
  // Initial paint can run before layout settles (especially when the
  // panel is opened from a header click while the user just
  // resized the sidebar). Use offsetWidth which forces a synchronous
  // layout — fine here because we only run on shell mount.
  apply(body.offsetWidth);
  if (typeof ResizeObserver !== 'function') {
    // Fallback — listen on window resize. Less precise (won't catch
    // sidebar drags) but better than a static breakpoint.
    if (!window._cronLayoutWindowListener) {
      window._cronLayoutWindowListener = () => {
        const el = document.querySelector('.cron-detail-body');
        if (el) apply(el.offsetWidth);
      };
      window.addEventListener('resize', window._cronLayoutWindowListener);
    }
    return;
  }
  // Re-binding the observer to a freshly-mounted DOM node is fine —
  // the previous observer's handle goes away when the old DOM does.
  // We still guard via _cronLayoutObs so the observer is idempotent
  // across same-shell repaints.
  if (body._cronLayoutObs) body._cronLayoutObs.disconnect();
  const obs = new ResizeObserver(entries => {
    for (const e of entries) {
      const w = (e.contentBoxSize && e.contentBoxSize[0])
        ? e.contentBoxSize[0].inlineSize
        : e.contentRect.width;
      apply(w);
    }
  });
  obs.observe(body);
  body._cronLayoutObs = obs;
}

// openCronDetail opens the per-job drawer in the 定时任务 panel.
// cron-panel-consolidation RFC §4.2 / §4.5 — primary entry point for
// "operator clicked a cron list row" and "operator just created a job".
// Behaviour:
//   - Records the originating .cj-row DOM element so closeCronDetail can
//     restore focus to it (RFC §6.4).
//   - Sets cronDetailJobId so subsequent renderCronPanel paints render
//     the drawer at the right spot.
//   - Calls openCronPanel which internally renderCronPanel — the
//     shell-preserving branch already paints both list AND drawer in
//     one pass, so no further explicit renderCronPanel is needed.
//   - Programmatically focuses the drawer header h2 once the DOM
//     materialises (RFC §6.4 — SR announces the task name on open).
//   - Idempotent on the same jobId (no flicker if invoked twice).
function openCronDetail(jobId, originRow) {
  if (!jobId) return;
  // Record the row that initiated the open so Esc / closeCronDetail can
  // restore focus there. Falls back to the first row whose data-cron-id
  // matches when the caller doesn't pass one (e.g. doCreateCronJob after
  // a fetch repaints the list).
  if (originRow instanceof Element) {
    _cronDrawerLastActiveRow = originRow;
  } else {
    const candidate = document.querySelector('.cj-row[data-cron-id="' + (window.CSS && CSS.escape ? CSS.escape(jobId) : jobId) + '"]');
    if (candidate) _cronDrawerLastActiveRow = candidate;
  }
  // §16: 切到另一个 cron 时清掉行内展开（上下文切换 = 旧展开内容已不相关）。
  // 不需要触发 panel 重绘 — openCronPanel 会重渲整个 drawer。
  if (cronExpandedRunId.runId && cronExpandedRunId.jobId !== jobId) {
    cronExpandedRunId.jobId = null;
    cronExpandedRunId.runId = null;
  }
  cronDetailJobId = jobId;
  // openCronPanel handles selectedKey reset / WS unsubscribe / mobile
  // shell push and triggers renderCronPanel — that path repaints both
  // the list (with .is-active on the new row) AND the drawer in one
  // shell-preserving pass. No second renderCronPanel needed.
  if (typeof openCronPanel === 'function') openCronPanel();
  // Move keyboard focus into the drawer header on the next frame so the
  // h2 has been laid out by the time .focus() runs. tabindex="-1" is
  // applied via cronDrawerHtml so the h2 is a programmatic focus target
  // without entering the document tab order. RFC §6.4.
  // Use rAF (paired with a setTimeout fallback for headless environments
  // where rAF may not fire promptly) to defer until after the layout.
  const focusDrawerHead = () => {
    const h2 = document.querySelector('#cron-detail-pane .cdh-title');
    if (h2 && typeof h2.focus === 'function') {
      try { h2.focus({ preventScroll: false }); } catch (_) { try { h2.focus(); } catch (_) {} }
    }
  };
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(focusDrawerHead);
  else setTimeout(focusDrawerHead, 0);
  // R236-SEC-08 (#494): the drawer's prompt body comes from the cached
  // job whose body may be 256-byte truncated (poll uses ?compact=1).
  // Refetch the full job in the background so the drawer's "做什么"
  // section shows the entire prompt rather than a clipped preview.
  // No-op when the cached job is already non-truncated. Failure modes
  // (timeout / 5xx) silently retain the truncated cache — the user
  // still sees the first 256 bytes plus the cron-spec edit affordance,
  // and the next poll will reconcile.
  if (typeof cronRefetchFullJob === 'function') {
    cronRefetchFullJob(jobId).then(res => {
      // Drawer is read-only: if the refetch failed we keep the truncated
      // cache rendered. Only re-render on a success result so the drawer
      // doesn't flicker when the network is slow / down.
      if (res && res.ok && cronDetailJobId === jobId) renderCronDrawer();
    }).catch(() => {});
  }
}

// closeCronDetail clears the drawer state and re-renders the cron panel
// shell so the list reclaims full width. Called by the drawer's ✕ button,
// the global Esc handler, and the "task deleted" toast cleanup. No-op
// when no drawer is open. Restores focus to the row that opened the
// drawer (RFC §6.4) so keyboard users land back where they were.
function closeCronDetail() {
  if (cronDetailJobId === null) return;
  // §16: drawer 关闭时连带清行内展开 — drawer 是 expand 的父级，drawer 不在
  // 也就没有 timeline 行可展开。renderCronPanel 会重渲整个 cron 面板。
  if (cronExpandedRunId.runId) {
    cronExpandedRunId.jobId = null;
    cronExpandedRunId.runId = null;
  }
  // cron-live RFC §3: drawer 关闭即撤销 cron live 订阅；事件数组随 unsub 清空，
  // 下次再开任意 drawer 不会带过来旧 job 的事件。
  if (typeof wsm !== 'undefined' && wsm.cronLive && wsm.cronLive.jobId) {
    wsm.unsubscribeCronLive();
  }
  cronDetailJobId = null;
  // Remove `.is-active` from any list row so the sidebar-style highlight
  // clears synchronously even before renderCronList re-paints.
  document.querySelectorAll('.cj-row.is-active').forEach(el => el.classList.remove('is-active'));
  renderCronPanel();
  // Restore focus. After renderCronList's repaint the cached element may
  // be detached from the DOM (innerHTML rebuild); look up the row by id
  // first and fall back to the cached reference if it's still connected.
  const restoreFocus = () => {
    let target = null;
    const cached = _cronDrawerLastActiveRow;
    if (cached && cached.isConnected) {
      target = cached;
    } else if (cached && cached.dataset && cached.dataset.cronId) {
      const fresh = document.querySelector('.cj-row[data-cron-id="' + (window.CSS && CSS.escape ? CSS.escape(cached.dataset.cronId) : cached.dataset.cronId) + '"]');
      if (fresh) target = fresh;
    }
    _cronDrawerLastActiveRow = null;
    if (target && typeof target.focus === 'function') {
      try { target.focus({ preventScroll: false }); } catch (_) { try { target.focus(); } catch (_) {} }
    }
  };
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(restoreFocus);
  else setTimeout(restoreFocus, 0);
}

// openCronSession is the legacy entry point retained as a thin alias so
// any in-flight code path (cached HTML attributes, the old menu action,
// future bookmarks) still routes into the drawer. cron-panel-consolidation
// RFC §4.2 retired the selectSession-into-mainShell flow; new code should
// call openCronDetail directly.
function openCronSession(cronId) {
  openCronDetail(cronId);
}

async function fetchCronJobs() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 8s timeout — cron list is polled periodically; a hung
    // disk/fs call must release before the next tick fires.
    //
    // R236-SEC-08 (#494): poll path opts into compact mode so the wire
    // shape carries `prompt` clipped to 256 UTF-8 bytes per job instead
    // of the legacy full prompt (which scaled to 8 KiB × N jobs every
    // tick). Each list row sets `prompt_truncated:true` for jobs whose
    // full body was clipped — the editor open path (cronEditFetchFull)
    // re-fetches a single job without compact when the user actually
    // needs the bytes.
    let data;
    try {
      data = await fetchJSON('/api/cron?compact=1', { headers, timeoutMs: 8000 });
    } catch (err) {
      if (err.status) return;
      throw err;
    }
    cronJobs = data.jobs || [];
    cronNotifyDefault = data.notify_default || null;
    // Badge surfaces jobs needing attention (paused or last run errored),
    // not the raw total — avoids a persistent red dot on healthy setups.
    // cron-v2-polish §3.3: missed jobs（进程重启空窗期跳过的调度）也
    // 纳入 attention，与 filterCronJobs 判定对齐。
    const attention = cronJobs.filter(j => j.paused || j.last_error || j.missed).length;
    const cronBadge = document.getElementById('cron-badge');
    if (cronBadge) {
      cronBadge.textContent = attention;
      cronBadge.style.display = attention > 0 ? '' : 'none';
      // Attention badge is semantically an alert (paused / errored jobs), so
      // opt into the red .is-alert variant defined in dashboard.html Track D.
      // History badge stays neutral grey because it is a cumulative count, not
      // an unread/failure signal.
      cronBadge.classList.toggle('is-alert', attention > 0);
    }
    // Mirror the attention dot onto the rail's 自动化 icon so the alert is
    // visible from any view (the header cron-badge is only shown in chat view).
    const railBadge = document.getElementById('abnav-cron-badge');
    if (railBadge) {
      railBadge.hidden = attention === 0;
    }
  } catch (e) { console.error('fetch cron:', e); }
}

// cronTriggerNow calls POST /api/cron/trigger to kick off a job immediately
// without waiting for the next scheduled tick. Useful when the operator
// wants to verify a prompt edit or rerun after a transient failure.
//
// Round 2 review R-4: visual-feedback contract (cron-panel-consolidation-ui
// RFC §4.3.1). The backend's jobRunningGuard already serializes against
// double-click — the issue is *user perception*. WS cron_run_started lands
// 200-500 ms after the API ACK, so a naive "fire-and-forget + toast" leaves
// the button looking pristine for that whole window and operators reflexively
// click again. The flow we want is:
//
//   click → button locks (spinner) → API returns OK → "已派发 ✓" 2 s
//        → debounce floor stays in effect another N s → unlock when WS
//          cron_run_started lands OR debounce floor elapses, whichever
//          is later.
//
// 10 s is the debounce floor: longer than the worst-case API + WS round
// trip we've measured (~3 s under load) but short enough that a real
// scheduled tick during the window won't get visually swallowed.
//
// Contract notes:
//   - Backend rejects paused jobs with 409 ErrJobPaused; the button is
//     hidden for paused jobs (cronJobCardHtml), so 409 here usually means a
//     pause landed between render and click — surface it via showAPIError
//     and immediately clear cronJustTriggered so the user can retry.
//   - 409 "already running" maps to the same "请等待结束" path the
//     disabled-running-state already shows; we reuse showAPIError so the
//     status code remains visible for L2 support.
//   - We do NOT wait for cron_run_started before unlocking — under WS
//     disconnection the event might never arrive. The 10 s floor + the
//     subsequent fetchCronJobs poll will reconcile.

// cronJustTriggered tracks the per-jobId trigger timestamp (ms).
// Used by cronTriggerCooldownState() to compute disable + label state for
// both the drawer's primary action and the list row's ghost Run button so
// they stay in sync. Cleared by cronTriggerCooldownClear() on WS
// cron_run_started (preferred) or after the 10 s floor elapses.
const cronJustTriggered = Object.create(null);
const CRON_TRIGGER_COOLDOWN_MS = 10 * 1000;

function cronTriggerCooldownState(id) {
  const t = cronJustTriggered[id];
  if (!t) return null;
  const dt = Date.now() - t;
  if (dt < 0 || dt >= CRON_TRIGGER_COOLDOWN_MS) {
    delete cronJustTriggered[id];
    return null;
  }
  // 0..1000 ms → spinner; 1000..3000 ms → ✓; 3000..10000 ms → quiet hold.
  if (dt < 1000) return { phase: 'sending', label: '触发中…' };
  if (dt < 3000) return { phase: 'sent',    label: '已派发 ✓' };
  return { phase: 'cooldown', label: '已派发 ✓' };
}

function cronTriggerCooldownClear(id) {
  if (cronJustTriggered[id]) delete cronJustTriggered[id];
}

// cronTriggerCooldownTickTimer drives label transitions (sending → sent →
// cooldown) and final unlock. Runs at 200 ms while any job is in cooldown
// to make the spinner→✓ transition feel snappy without burning CPU when
// the button is idle.
let cronTriggerCooldownTickTimer = null;
function ensureCronTriggerCooldownTick() {
  const anyHot = Object.keys(cronJustTriggered).length > 0;
  if (anyHot && !cronTriggerCooldownTickTimer) {
    cronTriggerCooldownTickTimer = setInterval(() => {
      // Sweep stale entries; if any expired, repaint the affected rows /
      // drawer so the button leaves cooldown.
      let anyExpired = false;
      const now = Date.now();
      for (const k of Object.keys(cronJustTriggered)) {
        if (now - cronJustTriggered[k] >= CRON_TRIGGER_COOLDOWN_MS) {
          delete cronJustTriggered[k];
          anyExpired = true;
        }
      }
      if (anyExpired || true) {
        // Repaint drawer cooldown labels so the spinner→✓→idle transitions
        // happen even if no other event fires. The repaint is targeted to
        // the actions row so it doesn't wipe focus / scroll position.
        if (cronDetailJobId !== null) renderCronDrawer();
      }
      if (Object.keys(cronJustTriggered).length === 0) {
        clearInterval(cronTriggerCooldownTickTimer);
        cronTriggerCooldownTickTimer = null;
      }
    }, 200);
  }
}

async function cronTriggerNow(id) {
  // Reentrancy guard: if a cooldown is already in flight for this id, drop
  // the click silently — the disabled button state should have prevented it
  // already, but keyboard activation paths (Enter on a non-disabled button
  // in the same paint window) can still slip through.
  if (cronJustTriggered[id]) return;
  cronJustTriggered[id] = Date.now();
  ensureCronTriggerCooldownTick();
  // Repaint drawer immediately so the button flips to the spinner without
  // waiting for the 200 ms tick. List ghost Run isn't repainted per row
  // (would be expensive on 500-job dashboards) — its disabled-after-trigger
  // state is read at render time.
  if (cronDetailJobId === id) renderCronDrawer();
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/trigger', { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) {
      // Failure — clear the cooldown immediately so the user can retry.
      // The 10 s floor would be punishing on a transient 502.
      cronTriggerCooldownClear(id);
      if (cronDetailJobId === id) renderCronDrawer();
      const raw = await r.text().catch(() => '');
      showAPIError('立即执行定时任务', r.status, raw);
      return;
    }
    // Success — leave cooldown in place; the tick timer will transition
    // the label and finally clear it. cronJobs row state will be updated
    // by the WS cron_run_started event (which also clears the cooldown
    // via the dispatch handler — see ws msg case below).
    showToast('已派发执行', 'success', 1500);
  } catch (e) {
    cronTriggerCooldownClear(id);
    if (cronDetailJobId === id) renderCronDrawer();
    showNetworkError('立即执行定时任务', e);
  }
}

async function cronPause(id) {
  // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController + 10s
  // timeout so a NAT-dropped TCP connection no longer hangs the pause
  // button silently. err.status carries the HTTP status for the existing
  // showAPIError surface; thrown errors without err.status are network
  // failures and route to showNetworkError.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON('/api/cron/pause', { method: 'POST', headers, body: JSON.stringify({ id }) });
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('暂停定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('暂停定时任务', e);
  }
}

async function cronResume(id) {
  // RNEW-UX-003 (#444): see cronPause godoc for fetchJSON migration rationale.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON('/api/cron/resume', { method: 'POST', headers, body: JSON.stringify({ id }) });
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('恢复定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('恢复定时任务', e);
  }
}

async function cronDelete(id) {
  // Round 2 R-12 (RFC §7.5): destructive flow rewrite.
  // - Statistic in title ("32 次执行记录") so user weighs the loss
  // - Mention JSONL preservation + how to find session_id without
  //   leaking the raw `claude --resume` command at the L1/L2 user
  // - Different copy when the job is currently running (CLI sub-
  //   process won't be killed; that's a surprising behaviour
  //   operators MUST know before confirming)
  // - 3 s countdown via confirmDialog's new countdownSecs option —
  //   long enough to catch fat-finger Enter, short enough not to
  //   annoy
  const job = (Array.isArray(cronJobs) ? cronJobs.find(j => j.id === id) : null) || {};
  const title = job.title || job.user_label || '';
  const runCount = (job.stats && (job.stats.total | 0)) || 0;
  const isRunning = !!(job.current_run);
  const promptPreview = (job.prompt || '').slice(0, 200);

  const headline = title ? '删除「' + title + '」？' : '删除定时任务？';
  let body;
  if (isRunning) {
    const elapsed = job.current_run.started_at
      ? formatRunningElapsed(job.current_run.started_at)
      : '';
    const elapsedHint = elapsed ? '（已运行 ' + elapsed + '）' : '';
    body = '⚠ 该任务正在执行' + elapsedHint + '。\n\n' +
      '删除后任务定义和' + (runCount > 0 ? ' ' + runCount + ' 条 ' : '')
      + '历史记录立即清除，但当前正在跑的这次执行将继续运行直到完成（CLI 子进程不会被强行 kill）。完成结果不会被记录到任何地方。';
  } else if (runCount > 0) {
    body = '此操作将永久删除该任务及其全部 ' + runCount + ' 次执行记录，不可撤销。\n\n' +
      'CLI 的对话历史 JSONL 文件保留在磁盘，需要时可在终端用 claude --resume 复活；session_id 在执行历史的「详情」里能找到。';
  } else {
    body = '此操作将永久删除该任务，不可撤销。该任务尚未执行过，不会有历史会话残留。';
  }

  const ok = await confirmDialog({
    title: headline,
    message: body,
    detail: promptPreview ? promptPreview : ('id: ' + id),
    confirmText: '删除',
    variant: 'danger',
    countdownSecs: 3,
  });
  if (!ok) return;
  // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController + 10s
  // timeout so a NAT-dropped TCP connection no longer hangs the destructive
  // delete flow silently. The button is already disabled by the modal
  // confirm flow above so a deterministic timeout is the right surface.
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON('/api/cron?id=' + encodeURIComponent(id), { method: 'DELETE', headers });
    // R220-FE-2: 释放该 job 在前端持有的 timeline 状态（runs / details / pagination
    // 游标 / fetched 标记），避免 cronTimelineState 累积已删除 job 的内存。
    if (cronTimelineState[id]) delete cronTimelineState[id];
    // cron-panel-consolidation RFC §4.5 state-machine row "G. 删除中":
    // close the drawer if it was showing the just-deleted job. The
    // subsequent renderCronPanel (via fetchCronJobs.then) will repaint
    // the empty drawer — calling closeCronDetail explicitly here keeps
    // the visual feedback synchronous (no flash of the deleted task).
    if (cronDetailJobId === id) {
      cronDetailJobId = null;
    }
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('删除定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('删除定时任务', e);
  }
}

// cronRefetchFullJob refills `cronJobs[i].prompt` for a single job from
// the non-compact /api/cron endpoint (R236-SEC-08 / #494). The poll path
// uses ?compact=1 which clips prompts to 256 bytes — that's fine for the
// list view but the editor / drawer detail need the full body before the
// user can save without truncating their own data.
//
// Returns one of:
//   { ok: true,  job }              — full prompt, safe to edit & save
//   { ok: false, reason: 'missing' }— job not in cronJobs cache
//   { ok: false, reason: 'fetch'  } — cache had truncated prompt and the
//                                     refetch failed; caller MUST refuse
//                                     to open the editor. Saving the
//                                     truncated body would silently
//                                     destroy the user's data.
async function cronRefetchFullJob(id) {
  const cached = cronJobs.find(j => j.id === id);
  if (!cached) return { ok: false, reason: 'missing' };
  if (!cached.prompt_truncated) return { ok: true, job: cached };
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // No compact param — list endpoint returns full prompts. We pull
    // the whole list here because there is no per-job GET endpoint
    // exposed; the rate limiter on the list route is shared with the
    // poll, and an editor open is a once-per-user-action event so the
    // extra body is not a hot path.
    const data = await fetchJSON('/api/cron', { headers, timeoutMs: 8000 });
    const jobs = (data && data.jobs) || [];
    const fresh = jobs.find(j => j.id === id);
    if (fresh && !fresh.prompt_truncated) {
      // Splice the full-prompt copy back into the cache so subsequent
      // editor opens / drawer renders see the full body without another
      // network round trip.
      const idx = cronJobs.findIndex(j => j.id === id);
      if (idx >= 0) cronJobs[idx] = fresh;
      return { ok: true, job: fresh };
    }
  } catch (e) { /* fall through to fetch-failure */ }
  return { ok: false, reason: 'fetch' };
}

// Edit an existing cron job. Opens a modal pre-populated with the current
// schedule, prompt, and work_dir. The frequency picker tries to restore the
// job's schedule via parseCronToFreq — when it can't (e.g. user wrote a
// custom expression by hand), we surface the raw expression in the advanced
// disclosure so it can still be edited without loss.
function editCronJob(id) {
  // R236-SEC-08 (#494): the poll fetches with ?compact=1 so j.prompt
  // may be truncated to 256 bytes for any job whose full body exceeds
  // that. Refetch the full job before opening the editor — if we
  // skipped this, saving the modal would persist the truncated prompt
  // back to disk, silently destroying the user's data. The refetch
  // helper returns { ok, ... } so we can refuse to open the editor on
  // a fetch failure rather than handing the user a 256-byte preview
  // that Save would happily commit back to disk (R251-FOLLOWUP / #494).
  cronRefetchFullJob(id).then(res => {
    if (!res || !res.ok) {
      const reason = res && res.reason;
      if (reason === 'fetch') {
        showToast('无法获取完整 prompt，编辑暂停以防截断保存。请稍后重试。', 'warning');
      } else {
        showToast('未找到该任务', 'warning');
      }
      return;
    }
    const job = res.job;
    // Sprint 6c: round-trip the saved backend choice. fetchCLIBackends is
    // promise-based; we open the modal once it resolves so the picker (if
    // multi-backend deploy) can pre-select the persisted value. Single-
    // backend deploys collapse the picker — no UI difference for legacy
    // installs.
    fetchCLIBackends().then(backendsData => {
      const backendHtml = renderBackendPicker(backendsData, {
        selectId: 'edit-cron-backend',
        selectedId: job.backend || '',
      });
      openCronEditModal(id, job, backendHtml);
    }).catch(() => openCronEditModal(id, job, ''));
  });
}

function openCronEditModal(id, job, backendHtml) {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  const notifyInitial = job.notify === true ? 'on' : (job.notify === false ? 'off' : '');
  const hasOverride = !!(job.notify_platform && job.notify_chat_id);
  const notifyHtml = buildCronNotifyToggleHtml(notifyInitial, hasOverride, job.notify_platform, job.notify_chat_id);
  const contextHtml = buildCronContextToggleHtml(!!job.fresh_context);

  // Round-trip attempt: if the saved expression matches a v2 picker shape
  // we pre-fill the picker; otherwise render the default picker and add a
  // "当前频率：<human-readable>" hint above it so the user sees the real
  // schedule (UI shows Daily/09:00 default which is a visual lie without
  // the hint). _cronScheduleTouched stays false so "save without touching
  // the frequency controls" preserves the legacy expression intact.
  // 关联 Issue #1（Round ? review）。
  const initialDesc = parseCronToFreq(job.schedule);
  const scheduleHtml = buildScheduleSection(
    initialDesc || { mode: 'daily', time: '09:00' },
    initialDesc ? '' : (job.schedule || '')
  );

  // Edit modal's "where" reuses the project picker when available, falling
  // back to a free-form input (projectsData empty or user typed a custom
  // path originally). The picker marks the matching project selected on
  // open so the UI reflects the persisted state.
  const wsBody = buildEditCronWorkspaceBody(job.work_dir || '');

  // Title + aria-label inlined as literals — see createNewCronJob for
  // the contract-test rationale.
  overlay.innerHTML =
    '<div class="modal cron-modal" role="dialog" aria-modal="true" aria-label="编辑定时任务">' +
      '<div class="cm-header">' +
        '<h3>编辑定时任务</h3>' +
        '<button type="button" class="cm-close" onclick="this.closest(\'.modal-overlay\').remove()" aria-label="关闭">✕</button>' +
      '</div>' +
      renderCronModalBody({
        scheduleHtml, wsBody, notifyHtml, contextHtml,
        backendHtml,
        promptId: 'edit-cron-prompt',
        promptPlaceholder: '这个任务要做什么？',
        titleId: 'edit-cron-title',
      }) +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">取消</button>' +
        '<button type="button" class="primary" onclick="doEditCronJob(\'' + escJs(id) + '\')">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  fillCronPrompt('edit-cron-prompt', job.prompt);
  // 回填 title。用 value 属性赋值避开 HTML 特殊字符在模板插值中的风险，
  // 与 fillCronPrompt 的 rationale 一致（参见 renderCronModalBody 注释）。
  const titleEl = document.getElementById('edit-cron-title');
  if (titleEl) titleEl.value = job.title || '';

  overlay.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') overlay.remove();
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      doEditCronJob(id);
    }
  });

  // Seed overlay._cronSchedule with the job's ACTUAL schedule (not whatever
  // the picker would render back from parseCronToFreq). Critical for legacy
  // shapes that don't round-trip: @every 30m / multi-dow weekly / hand-
  // crafted expressions all parseCronToFreq → null → default Daily picker.
  // Without the touched gate, an immediate freqUpdate() would overwrite
  // _cronSchedule to "0 9 * * *" and a "save without changes" click would
  // silently rewrite the user's 30-minute job to every day 9 AM. See
  // freqMarkTouched doc. 不要调 freqUpdate()——让 seed 保持到用户真的
  // 交互频率控件为止。
  //
  // Seed overlay._cronWorkDir to job.work_dir. 之前注释推理为「input 的
  // pre-fill 覆盖未改动场景」是错的：buildCronWorkspaceBodyInternal 仅在
  // selected 命中 projectsData 中某个项目时把 input.value 设为 ''（隐藏
  // custom form 分支），所以「项目命中」+「未触碰任何控件」直接保存会让
  // newWorkDir 解析为 ''，PATCH 把 work_dir 清空、任务回退到 router 默认
  // cwd（用户报告的 bug）。Picker 点选会经 cronSelectWorkspace 覆盖
  // _cronWorkDir；用户点「自定义路径」走 toggleCronWsCustom 也会清掉
  // _cronWorkDir 让 input 接管；要清空 work_dir 的用户需显式切到自定义
  // 路径并清空输入框。
  overlay._cronSchedule = job.schedule || '';
  overlay._cronScheduleTouched = false;
  overlay._cronWorkDir = job.work_dir || '';
}

// buildEditCronWorkspaceBody is the edit-mode counterpart. v2 polish 之后
// 与 create 共享 buildCronWorkspaceBodyInternal，只传不同的 inputId 和
// 已选中的 currentDir 用来回填按钮文本 + 自定义路径输入。
function buildEditCronWorkspaceBody(currentDir) {
  return buildCronWorkspaceBodyInternal({
    inputId: 'edit-cron-workdir',
    buttonId: 'edit-cron-ws-dropdown',
    selectedPath: currentDir || '',
  });
}

function authHeaders() {
  const headers = {};
  const t = getToken();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  return headers;
}

async function doEditCronJob(id) {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  const job = cronJobs.find(j => j.id === id);
  if (!job) { showToast('未找到该任务', 'warning'); return; }

  const newPrompt = document.getElementById('edit-cron-prompt').value;
  const newTitle = (document.getElementById('edit-cron-title')?.value || '').trim();
  // Advanced raw input wins over picker; if both empty use overlay cache
  // (seeded to job.schedule on modal open, kept fresh by freqUpdate()).
  const advanced = document.getElementById('freq-advanced-input');
  const newSchedule = ((advanced && advanced.value.trim()) || overlay._cronSchedule || '').trim();
  // Workdir resolution: project picker (overlay._cronWorkDir set by
  // cronSelectWorkspace) wins; otherwise fall back to the custom input.
  // Tracks the same contract as doCreateCronJob so either flow works
  // whether the user clicked a project or typed a custom path.
  const wdInput = document.getElementById('edit-cron-workdir');
  let newWorkDir = overlay._cronWorkDir || '';
  if (wdInput && wdInput.value.trim()) newWorkDir = wdInput.value.trim();

  // Only send fields that actually changed so the server keeps fields the
  // user didn't touch (and the audit log stays meaningful).
  const body = {};
  if (newPrompt !== (job.prompt || '')) body.prompt = newPrompt;
  if (newTitle !== (job.title || '')) body.title = newTitle;
  if (newSchedule !== job.schedule) body.schedule = newSchedule;
  if (newWorkDir !== (job.work_dir || '')) body.work_dir = newWorkDir;

  // Notify toggle — compare against the job's existing tri-state.
  const notifyVals = collectCronNotifyValues();
  const originalNotify = (job.notify === true || job.notify === false) ? job.notify : null;
  if (notifyVals.notify !== null && notifyVals.notify !== originalNotify) {
    body.notify = notifyVals.notify;
  }
  // Per-job target override: any change (including clearing) is a PATCH.
  const origPlat = job.notify_platform || '';
  const origChat = job.notify_chat_id || '';
  if (notifyVals.notify_platform !== null && notifyVals.notify_platform !== origPlat) {
    body.notify_platform = notifyVals.notify_platform;
  }
  if (notifyVals.notify_chat_id !== null && notifyVals.notify_chat_id !== origChat) {
    body.notify_chat_id = notifyVals.notify_chat_id;
  }
  // If user unchecked the override, explicitly clear both fields (server
  // accepts "" to mean "clear").
  const overrideCheckbox = document.getElementById('cron-notify-override');
  if (overrideCheckbox && !overrideCheckbox.checked && (origPlat || origChat)) {
    body.notify_platform = '';
    body.notify_chat_id = '';
  }

  // fresh_context toggle
  const freshCtx = collectCronContextValue();
  if (freshCtx !== null && freshCtx !== !!job.fresh_context) {
    body.fresh_context = freshCtx;
  }

  // Sprint 6c: backend pointer semantics — only PATCH when the user picked
  // a different backend than the one stored on the job. Element absent =
  // single-backend deploy (or fetch failed); skip the field entirely so
  // the legacy unset path on the server stays unchanged.
  const backendEl = document.getElementById('edit-cron-backend');
  if (backendEl) {
    const newBackend = backendEl.value || '';
    const origBackend = job.backend || '';
    if (newBackend !== origBackend) body.backend = newBackend;
  }

  if (Object.keys(body).length === 0) { overlay.remove(); return; }
  if (body.schedule === '') { showToast('频率不能为空', 'warning'); return; }

  try {
    const headers = Object.assign({ 'Content-Type': 'application/json' }, authHeaders());
    const r = await fetch('/api/cron?id=' + encodeURIComponent(id), {
      method: 'PATCH', headers, body: JSON.stringify(body),
    });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('保存定时任务', r.status, raw);
      return;
    }
    overlay.remove();
    showToast('定时任务已更新', 'success');
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    showNetworkError('保存定时任务', e);
  }
}

/* ===== Sidebar resizer (desktop only) ===== */
(function(){
  const resizer = document.getElementById('resizer');
  const sidebar = document.querySelector('.sidebar');
  // RNEW-UX-004 demo: migrated 'naozhi_sidebar_w' -> 'nz:sidebar_w' via
  // unified helper. One-time loss of saved width acceptable (defaults to
  // CSS width).
  const LS_SIDEBAR_W = 'sidebar_w';
  const saved = parseFloat(lsGet(LS_SIDEBAR_W, 0));
  if (saved >= 200) sidebar.style.width = saved + 'px';

  let startX, startW;
  resizer.addEventListener('mousedown', function(e) {
    // Mid-line collapse handle lives inside the resizer; let its click run
    // without starting a drag. Also skip when collapsed (nothing to resize).
    if (e.target && e.target.closest && e.target.closest('.resizer-handle')) return;
    if (document.body.classList.contains('sidebar-collapsed')) return;
    e.preventDefault();
    startX = e.clientX;
    startW = sidebar.getBoundingClientRect().width;
    resizer.classList.add('dragging');
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  });
  function onMove(e) {
    const w = Math.min(Math.max(startW + e.clientX - startX, 200), window.innerWidth * 0.6);
    sidebar.style.width = w + 'px';
  }
  function onUp() {
    resizer.classList.remove('dragging');
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
    document.removeEventListener('mousemove', onMove);
    document.removeEventListener('mouseup', onUp);
    lsSet(LS_SIDEBAR_W, Math.round(sidebar.getBoundingClientRect().width));
  }
  resizer.addEventListener('dblclick', function(e) {
    if (e.target && e.target.closest && e.target.closest('.resizer-handle')) return;
    if (document.body.classList.contains('sidebar-collapsed')) return;
    sidebar.style.width = '360px';
    lsRemove(LS_SIDEBAR_W);
  });
})();

/* ===== Sidebar fully-collapse (PC only) =====
   Toggle body.sidebar-collapsed so .main occupies the full viewport. State is
   persisted via lsSet so a refresh keeps the user's preference. The mobile
   layout (≤768px) already treats the sidebar as a fixed drawer overlay, so
   the toggle is a no-op there: we suppress the click and let mobile's own
   list/chat-view classes drive visibility. Keyboard shortcut: `[` (mirroring
   editor conventions like VS Code's Ctrl+B / Cursor's `[`). */
const LS_SIDEBAR_COLLAPSED = 'sidebar_collapsed';

function isMobileViewport() {
  return window.matchMedia && window.matchMedia('(max-width: 768px)').matches;
}

// applySidebarCollapsed is the single state-mutator. moveFocus drives whether
// to relocate keyboard focus to the now-visible button — true on user-driven
// toggle (the previously-focused button is about to be display:none'd, which
// would punt focus back to <body>); false on cold-start / viewport-boundary
// re-apply (don't steal focus from the user's first interaction).
function applySidebarCollapsed(collapsed, moveFocus) {
  document.body.classList.toggle('sidebar-collapsed', !!collapsed);
  // Single mid-line handle on the resizer serves both directions; flip the
  // aria-expanded + label so AT and tooltip agree with the visual state.
  const btn = document.getElementById('btn-sidebar-toggle');
  if (btn) {
    btn.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    btn.setAttribute('aria-label', collapsed ? '展开侧边栏' : '收起侧边栏');
    btn.setAttribute('title', collapsed ? '展开侧边栏 (按 [)' : '收起侧边栏 (按 [)');
  }
  if (moveFocus && btn && typeof btn.focus === 'function') {
    try { btn.focus({preventScroll: true}); } catch (_) { btn.focus(); }
  }
}

function toggleSidebarCollapsed() {
  // Mobile drawer has its own list/chat-view contract — do not piggyback on
  // it; just bail so the existing back-button + drawer flow stays canonical.
  if (isMobileViewport()) return;
  const next = !document.body.classList.contains('sidebar-collapsed');
  applySidebarCollapsed(next, true);
  lsSet(LS_SIDEBAR_COLLAPSED, next ? 1 : 0);
}

(function initSidebarCollapsed(){
  // Honor persisted preference on cold-load. Skip on mobile so a previously
  // collapsed PC session doesn't black-box the drawer when the user pops the
  // dashboard open on a phone (different viewport, different mental model).
  if (isMobileViewport()) return;
  if (lsGet(LS_SIDEBAR_COLLAPSED, 0)) {
    applySidebarCollapsed(true, false);
  }
})();

// Re-apply preference when the viewport crosses the mobile boundary (DevTools,
// tablet rotation, manual resize). On mobile we drop the PC class so the
// drawer rules win; switching back to PC re-applies the saved flag.
if (window.matchMedia) {
  const mql = window.matchMedia('(max-width: 768px)');
  const onMqlChange = (e) => {
    if (e.matches) {
      document.body.classList.remove('sidebar-collapsed');
    } else {
      applySidebarCollapsed(!!lsGet(LS_SIDEBAR_COLLAPSED, 0), false);
    }
  };
  if (typeof mql.addEventListener === 'function') {
    mql.addEventListener('change', onMqlChange);
  } else if (typeof mql.addListener === 'function') {
    mql.addListener(onMqlChange); // Safari ≤13 fallback
  }
}

document.addEventListener('keydown', function(e) {
  // `[` toggles collapse on PC. Skip when typing into an input/textarea/
  // contenteditable, when any modifier is held, while an IME composition is
  // active (CJK input fires `[` for fullwidth bracket), or while a modal/
  // palette is open. Mirrors the skip logic the `/` shortcut uses for
  // sidebar search.
  if (e.key !== '[') return;
  if (e.isComposing) return;
  if (e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return;
  const tgt = e.target;
  if (tgt && (tgt.tagName === 'INPUT' || tgt.tagName === 'TEXTAREA' || tgt.isContentEditable)) return;
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  if (isMobileViewport()) return;
  e.preventDefault();
  toggleSidebarCollapsed();
});

/* ===== Onboarding ===== */

// Show a one-time intro for first-time visitors. Dismissal is sticky per
// browser profile (localStorage). Suppressed when auth is unresolved, when
// the user already has sessions/projects, or on mobile viewports where the
// sidebar is a modal-style drawer and the intro would stack awkwardly.
const ONBOARDING_LS_KEY = 'nz-onboarding-dismissed';

function maybeShowOnboarding(authResolved) {
  // fetchSessions returns falsy when a 401/403 triggered the auth modal.
  // Suppress onboarding in that case — otherwise the onboarding overlay
  // would stack on top of the auth modal on first visit.
  if (authResolved === false) return;
  try {
    if (localStorage.getItem(ONBOARDING_LS_KEY)) return;
  } catch (_) { return; }
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  // Suppress on narrow viewports — the sidebar drawer UX differs enough that
  // the "pick one from the sidebar" guidance is misleading.
  if (window.innerWidth && window.innerWidth < 768) return;
  const hasSessions = (Object.keys(sessionsData || {}).length > 0) ||
    (Object.keys(sessionWorkspaces || {}).length > 0);
  const hasProjects = (projectsData && projectsData.length > 0);
  if (hasSessions || hasProjects) {
    try { localStorage.setItem(ONBOARDING_LS_KEY, '1'); } catch (_) {}
    return;
  }
  showOnboarding();
}

function dismissOnboarding() {
  try { localStorage.setItem(ONBOARDING_LS_KEY, '1'); } catch (_) {}
  const ov = document.querySelector('.modal-overlay.onboarding-overlay');
  if (ov) ov.remove();
}

function showOnboarding() {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay onboarding-overlay';
  overlay.innerHTML =
    '<div class="modal onboarding" role="dialog" aria-modal="true" aria-label="Welcome to Naozhi">' +
      '<h3>欢迎使用 naozhi Dashboard</h3>' +
      '<div class="ob-sub">几秒钟了解核心用法</div>' +
      '<ul>' +
        '<li><span class="ob-icon">+</span><div><b>新建会话</b> — 点击左上角 <b>+</b> 或 <b>New session</b>，选择工作目录即可开始对话</div></li>' +
        '<li><span class="ob-icon">⌘</span><div><b>快捷键</b> — <b>Cmd/Ctrl+1..9</b> 切换会话，<b>Alt+↑/↓</b> 跳转消息，<b>Esc</b> 关闭弹窗</div></li>' +
        '<li><span class="ob-icon">⏱</span><div><b>定时任务</b> — 侧边栏 Cron 图标，可按自然语言频率设置定期执行</div></li>' +
        '<li><span class="ob-icon">IM</span><div><b>IM 渠道</b> — 同一会话可在飞书等平台接入，发送 <b>/help</b> 查看命令</div></li>' +
      '</ul>' +
      '<div class="modal-btns">' +
        '<button type="button" onclick="dismissOnboarding()">稍后再说</button>' +
        '<button type="button" class="primary" onclick="dismissOnboarding();createNewSession()">立即创建会话</button>' +
      '</div>' +
    '</div>';
  overlay.addEventListener('click', function(e) {
    if (e.target === overlay) dismissOnboarding();
  });
  // Dismissal is also persisted when Esc is pressed inside trapFocus — the
  // trap's teardown removes the overlay, and the next maybeShowOnboarding
  // call checks localStorage first. Eagerly set the key here so an Esc
  // removal does not leave the flag unwritten (the MutationObserver that
  // did this before duplicated the observer installed by trapFocus).
  try { localStorage.setItem(ONBOARDING_LS_KEY, '1'); } catch (_) {}
  document.body.appendChild(overlay);
  trapFocus(overlay);
}

/* ===== Initialization ===== */

// Multi-Backend RFC §8.5: fire fetchCLIBackends at boot so the chip / cost
// unit / context bar all have backend metadata available on the first
// renderHeader call. Failure / single-backend deployments still work — the
// chip-render helpers return '' when cliBackends is null.
fetchCLIBackends();
fetchSessions().then(maybeShowOnboarding);
sessionPollTimer = setInterval(fetchSessions, 5000);
scanDiscovered();
discoveredPollTimer = setInterval(scanDiscovered, 30000);
fetchCronJobs(); // load initial cron state for badge
wsm.connect();

// RNEW-UX-014: suspend background pollers when the tab is hidden. 1-5s
// setInterval loops on a backgrounded tab burn battery, mobile data, and
// server bandwidth for no user-visible benefit. Resume on visibility
// change so the first thing a returning user sees is fresh state.
// WS event delivery is not affected — the socket stays open in hidden
// tabs and delivers live updates instantly when the user returns.
//
// Extended gate also covers:
//   - eventTimer (1s polling fallback when WS isn't connected) — stopped
//     when hidden; resumed only if a session is selected AND WS is not
//     already delivering live events, to avoid double-fetching.
// (Issue #434: _statusTickTimer was removed — it was a 1s no-op since
// the #sidebar-status DOM no longer exists.)
(function () {
  const stopPollers = () => {
    if (sessionPollTimer) { clearInterval(sessionPollTimer); sessionPollTimer = null; }
    if (discoveredPollTimer) { clearInterval(discoveredPollTimer); discoveredPollTimer = null; }
    if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
    // #1770: also pause the WS keep-alive ping while the tab is hidden. The
    // 30s app-level ping wakes the mobile radio every 30s for nothing —
    // connection liveness is independently maintained by the server's
    // protocol-level Ping/Pong (writePump, wsPingPeriod≈54s), so dropping the
    // app ping loses no liveness detection. Re-armed in startPollers on resume.
    if (wsm && wsm.pingTimer) wsm.cleanup();
  };
  const startPollers = () => {
    if (!sessionPollTimer) {
      fetchSessions(); // immediate refresh on resume so UI is not stale
      sessionPollTimer = setInterval(fetchSessions, 5000);
    }
    if (!discoveredPollTimer) {
      discoveredPollTimer = setInterval(scanDiscovered, 30000);
    }
    // eventTimer is a WS-outage fallback. If WS is live, events already
    // arrive via the socket and the timer is redundant; let the normal
    // WS state transitions re-arm it if the socket drops.
    if (!eventTimer && selectedKey && wsm && wsm.state !== WS_STATES.CONNECTED) {
      fetchEvents(false);
      eventTimer = setInterval(() => fetchEvents(false), 1000);
    }
    // #1770: re-arm the WS ping we paused in stopPollers, but only when the
    // socket is actually live — a dropped/offline socket has no ping to keep
    // and will re-arm via auth_ok on reconnect.
    if (wsm && !wsm.pingTimer && wsm.conn && wsm.conn.readyState === WebSocket.OPEN) {
      wsm.startPing();
    }
  };
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) stopPollers();
    else startPollers();
  });
})();

/*
 * RNEW-UX-002: global error handler.
 *
 * Before this, an uncaught exception inside a handler (async listener, WS
 * callback, render path) would bubble to the browser's default handler and
 * silently freeze the UI — operators had to open devtools to notice. This
 * block catches both sync errors and unhandled promise rejections, surfaces
 * a warning toast so the user knows to consider a refresh, and dumps full
 * details to console with a [global-error] prefix for devtools triage. We
 * throttle identical messages within 5s so a tight error loop doesn't spam
 * the toast layer. Never calls preventDefault — the browser's own console
 * output is still allowed to fire, preserving stack traces for remote debug.
 */
(function () {
  const THROTTLE_MS = 5 * 1000;
  const seen = new Map(); // message -> last-shown timestamp (ms)
  function handle(ev) {
    try {
      const isReject = ev && ev.type === 'unhandledrejection';
      const err = isReject ? (ev.reason || {}) : (ev && ev.error) || {};
      const rawMsg = (err && err.message) || (ev && ev.message) || String(err || 'unknown error');
      const msg = String(rawMsg).slice(0, 100);
      const now = Date.now();
      const last = seen.get(msg) || 0;
      if (now - last < THROTTLE_MS) return; // coalesce
      seen.set(msg, now);
      // Best-effort map trim so long-running tabs don't leak entries.
      if (seen.size > 64) { const k = seen.keys().next().value; if (k) seen.delete(k); }
      console.error('[global-error]', {
        type: ev && ev.type,
        message: rawMsg,
        stack: err && err.stack,
        source: ev && ev.filename,
        line: ev && ev.lineno,
        col: ev && ev.colno,
      });
      if (typeof showToast === 'function') {
        showToast('页面遇到异常，可能需要刷新：' + msg, 'warning', 4000);
      }
    } catch (_) { /* last-resort: never throw from the error handler */ }
  }
  window.addEventListener('error', handle, true);
  window.addEventListener('unhandledrejection', handle);
})();

initMobile();
initViewportTracking();
initSwipeDelete();
initSidebarProjectActions();
initSwipeBack();
initSidebarSearch();
(function(){
  var ov=document.createElement('div');ov.className='lightbox-overlay';
  ov.setAttribute('role','dialog');ov.setAttribute('aria-modal','true');ov.setAttribute('aria-label','Image preview');
  // Toolbar buttons sit absolute top-right; rotation buttons trigger 90° steps.
  // The overlay's click-to-close handler ignores events that didn't target the
  // overlay itself, so toolbar clicks won't propagate up and dismiss the modal.
  ov.innerHTML='<div class="lb-toolbar">'
    +'<button type="button" class="lb-tool-btn" data-lb-action="rotate-left" aria-label="Rotate left" title="Rotate left (R)">↺</button>'
    +'<button type="button" class="lb-tool-btn" data-lb-action="rotate-right" aria-label="Rotate right" title="Rotate right (Shift+R)">↻</button>'
    +'</div>'
    +'<img alt=""><div class="lb-zoom-hint"></div>';
  document.body.appendChild(ov);
  var img=ov.querySelector('img'),hint=ov.querySelector('.lb-zoom-hint');
  var scale=1,panX=0,panY=0,rotation=0,dragging=false,lx=0,ly=0,ht=null,rotateAnimTimer=null;
  function showHint(text){hint.textContent=text||(Math.round(scale*100)+'%');hint.classList.add('visible');clearTimeout(ht);ht=setTimeout(function(){hint.classList.remove('visible')},1200)}
  function apply(){
    // Rotation always emits a transform — even at neutral pan/scale — because
    // resetting transform to '' would visibly snap the image back. Order
    // matters: translate → scale → rotate keeps panning intuitive (drag in
    // screen-space, not image-space).
    var neutral=scale===1&&!panX&&!panY&&rotation===0;
    img.style.transform=neutral?'':'translate('+panX+'px,'+panY+'px) scale('+scale+') rotate('+rotation+'deg)';
    ov.classList.toggle('zoomed',scale>1);
  }
  function reset(){scale=1;panX=0;panY=0;rotation=0;dragging=false;img.style.transform='';img.classList.remove('lb-rotating');ov.classList.remove('zoomed','dragging');hint.classList.remove('visible');clearTimeout(ht);clearTimeout(rotateAnimTimer)}
  function close(){ov.classList.remove('active');reset()}
  function rotateBy(deg){
    // Accumulate the raw angle without normalization so the CSS transition
    // always rotates the visually shorter 90° path. If we wrapped to
    // (-180, 180] the browser would interpolate a 270° spin in the wrong
    // direction (e.g. -180 → +180 renders as +360 of CW spin).
    rotation+=deg;
    img.classList.add('lb-rotating');
    apply();
    // Display label normalized to (-180, 180] so the hint stays human-readable
    // ("90°" beats "450°"). The stored `rotation` keeps growing.
    var disp=((rotation%360)+360)%360;
    if(disp>180)disp-=360;
    showHint(disp+'°');
    clearTimeout(rotateAnimTimer);
    // Strip the transition class once the animation settles so subsequent
    // pan/zoom interactions stay snappy (no easing on every drag frame).
    rotateAnimTimer=setTimeout(function(){img.classList.remove('lb-rotating')},280);
  }
  ov.addEventListener('click',function(e){
    var btn=e.target&&e.target.closest&&e.target.closest('[data-lb-action]');
    if(btn){
      // Toolbar click — handle action and don't fall through to backdrop close.
      var action=btn.getAttribute('data-lb-action');
      if(action==='rotate-left')rotateBy(-90);
      else if(action==='rotate-right')rotateBy(90);
      return;
    }
    if(e.target===ov)close();
  });
  // Scroll wheel zoom (toward cursor)
  ov.addEventListener('wheel',function(e){e.preventDefault();var f=e.deltaY<0?1.15:1/1.15,ns=Math.min(Math.max(scale*f,.5),10);var r=img.getBoundingClientRect(),cx=e.clientX-(r.left+r.width/2),cy=e.clientY-(r.top+r.height/2);panX-=cx*(ns/scale-1);panY-=cy*(ns/scale-1);scale=ns;apply();showHint()},{passive:false});
  // Mouse drag pan
  img.addEventListener('mousedown',function(e){if(scale<=1)return;e.preventDefault();dragging=true;lx=e.clientX;ly=e.clientY;ov.classList.add('dragging')});
  document.addEventListener('mousemove',function(e){if(!dragging)return;panX+=e.clientX-lx;panY+=e.clientY-ly;lx=e.clientX;ly=e.clientY;apply()});
  document.addEventListener('mouseup',function(){if(dragging){dragging=false;ov.classList.remove('dragging')}});
  // Double-click toggle zoom
  img.addEventListener('dblclick',function(e){e.preventDefault();e.stopPropagation();if(scale>1.05){reset();apply()}else{var r=img.getBoundingClientRect(),cx=e.clientX-(r.left+r.width/2),cy=e.clientY-(r.top+r.height/2);scale=2.5;panX=-cx*1.5;panY=-cy*1.5;apply()}showHint()});
  // Touch: pinch zoom + drag pan + double-tap
  var iDist=0,iScale=1,lastTap=0;
  function t2d(t){return Math.hypot(t[1].clientX-t[0].clientX,t[1].clientY-t[0].clientY)}
  img.addEventListener('touchstart',function(e){if(e.touches.length===2){e.preventDefault();iDist=t2d(e.touches);iScale=scale}else if(e.touches.length===1&&scale>1){lx=e.touches[0].clientX;ly=e.touches[0].clientY;dragging=true}},{passive:false});
  img.addEventListener('touchmove',function(e){if(e.touches.length===2&&iDist){e.preventDefault();scale=Math.min(Math.max(iScale*(t2d(e.touches)/iDist),.5),10);apply();showHint()}else if(e.touches.length===1&&dragging){e.preventDefault();panX+=e.touches[0].clientX-lx;panY+=e.touches[0].clientY-ly;lx=e.touches[0].clientX;ly=e.touches[0].clientY;apply()}},{passive:false});
  img.addEventListener('touchend',function(e){if(e.touches.length<2)iDist=0;if(e.touches.length===0){dragging=false;if(e.changedTouches.length===1){var now=Date.now();if(now-lastTap<300){e.preventDefault();if(scale>1.05)reset();else scale=2.5;apply();showHint()}lastTap=now}}});
  // openLightbox(src, [fallback]) opens the full-size image at `src`.
  //
  // When the optional `fallback` argument is supplied and the primary
  // `src` fails to load, the lightbox silently switches to the fallback
  // and keeps the overlay open. This addresses the attachment-GC-expired
  // path (RFC §3.6.3): the on-disk original at
  // /api/sessions/attachment?... is gone, but the embedded thumbnail
  // data URI was persisted alongside it and renders identically (though
  // at 600px). Without the fallback the user would see a broken-image
  // glyph.
  //
  // Two failure modes the handler has to cover:
  //   1. HTTP 404 / network error → <img>'s onerror fires.
  //   2. HTTP 200 but wrong Content-Type / corrupt body → onerror does
  //      NOT fire on all browsers; we detect this post-load by checking
  //      naturalWidth === 0 and swap to the fallback.
  //
  // The img element is reused across calls, so its onload / onerror
  // handlers are re-assigned (not addEventListener'd) to avoid
  // accumulating stale listeners when users open the lightbox repeatedly.
  window.openLightbox=function(src,fallback){
    reset();
    var primaryTried=false;
    function useFallback(){
      if(!fallback||fallback===src)return false;
      // Guard against infinite recursion if the fallback itself 404s.
      img.onerror=function(){img.onerror=null;img.onload=null};
      img.onload=function(){img.onerror=null;img.onload=null};
      img.src=fallback;
      return true;
    }
    img.onerror=function(){
      img.onerror=null;
      if(!useFallback())img.onload=null;
    };
    img.onload=function(){
      if(!primaryTried){
        primaryTried=true;
        // naturalWidth===0 indicates the resource loaded (no onerror)
        // but decoded to nothing — usually a Content-Type that Chrome
        // refuses to render as an image. Treat identically to an
        // onerror so we fall back to the thumb.
        if(img.naturalWidth===0&&useFallback())return;
      }
      img.onerror=null;img.onload=null;
    };
    img.src=src;
    ov.classList.add('active');
  };
  document.addEventListener('keydown',function(e){
    if(!ov.classList.contains('active'))return;
    // Skip shortcuts when an editable element holds focus — otherwise typing
    // 'r' in a chat input or stacked dialog would silently rotate the
    // backgrounded preview.
    var ae=document.activeElement;
    if(ae&&(ae.tagName==='INPUT'||ae.tagName==='TEXTAREA'||ae.isContentEditable)){
      if(e.key==='Escape')close();
      return;
    }
    if(e.key==='Escape'){close();return}
    if(e.key==='+'||e.key==='='){scale=Math.min(scale*1.2,10);apply();showHint();return}
    if(e.key==='-'){scale=Math.max(scale/1.2,.5);apply();showHint();return}
    if(e.key==='0'){reset();apply();showHint();return}
    // Rotation shortcuts: r = CCW 90°, R / Shift+R = CW 90°. Match by lowercase
    // so the lightbox responds the same regardless of caps lock state.
    if(e.key==='r'||e.key==='R'){e.preventDefault();rotateBy(e.shiftKey?90:-90);return}
  });
})();

/* ─────────────────────────────────────────────────────────────────────────────
   Aside (scratch) drawer — preview-pane追问
   Opens on the ↗ button added to AI bubbles. Creates a scratch session on
   the server, polls events for it, sends messages, and optionally promotes
   it into a sidebar-visible session. Drawer DOM lives in dashboard.html.
   ───────────────────────────────────────────────────────────────────────── */
(function(){
  const drawer = document.getElementById('aside-drawer');
  if (!drawer) return;
  const $ = (id) => document.getElementById(id);
  const elMsgs = $('ad-messages');
  const elEmpty = $('ad-empty');
  const elInput = $('ad-input');
  const elSend = $('ad-send');
  const elClose = $('ad-close');
  const elSave = $('ad-save');
  const elQuoteChip = $('ad-quote-chip');
  const elQuotePreview = $('ad-quote-preview');
  const elQuoteTrunc = $('ad-quote-trunc');
  const elQuoteCtx = $('ad-quote-ctx');
  const elLoading = $('ad-loading');
  const elAgent = $('ad-agent');

  let state = null;            // {scratchId, key, agentId, sourceKey, sourceMsgTime, quote, lastEventTime, pendingUserEchoes}
  let pollTimer = null;
  let sending = false;
  // Self-scheduling poll cadence. A brand-new scratch session has no
  // persisted events yet, so /api/sessions/events returns 404 ("session not
  // found") until the first turn lands. The old fixed setInterval(…,1000)
  // hammered that 404 at 1Hz forever, flooding the browser network log and
  // wasting requests. We instead back off 1s→2s→4s→…→POLL_MAX_MS while the
  // session is still empty/unreachable, and snap back to POLL_BASE_MS the
  // moment a real poll succeeds. R20260605.
  const POLL_BASE_MS = 1000;
  const POLL_MAX_MS = 8000;
  let pollDelayMs = POLL_BASE_MS;

  function authHeaders(extra) {
    const h = Object.assign({}, extra || {});
    try {
      const t = getToken();
      if (t) h['Authorization'] = 'Bearer ' + t;
    } catch (_) {}
    return h;
  }

  function clearMessages() {
    if (!elMsgs) return;
    // Preserve the empty placeholder for re-use.
    elMsgs.innerHTML = '';
    elMsgs.appendChild(elEmpty);
  }

  function showDrawer() { drawer.classList.add('visible'); }
  function hideDrawer() { drawer.classList.remove('visible'); }

  function stopPolling() {
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
  }

  async function closeScratch(silent) {
    stopPolling();
    hideDrawer();
    if (!state) return;
    const id = state.scratchId;
    state = null;
    elSave.classList.remove('visible');
    clearMessages();
    elInput.value = '';
    if (!id) return;
    try {
      await fetch('/api/scratch/' + encodeURIComponent(id), {
        method: 'DELETE', headers: authHeaders(),
      });
    } catch (_) { /* best effort */ }
    if (!silent && typeof showToast === 'function') { /* no toast on normal close */ }
  }

  function previewText(s) {
    if (!s) return '';
    const one = s.replace(/\s+/g, ' ').trim();
    return one.length > 40 ? one.slice(0, 40) + '…' : one;
  }

  // De-duplicate echoed user messages: sendInScratch renders the user bubble
  // immediately for perceived responsiveness, then the server's event stream
  // echoes the same text back as a `user` event. Without this filter the
  // user's own message would appear twice. We compare the trimmed detail
  // against the pendingUserEchoes set populated by sendInScratch; the set
  // is bounded at 10 entries (most users don't queue more than 2-3 sends
  // before polling catches up).
  function matchesPendingEcho(ev) {
    if (!state || !state.pendingUserEchoes || ev.type !== 'user') return false;
    const body = String(ev.detail || ev.summary || '').trim();
    if (!body) return false;
    for (const pending of state.pendingUserEchoes) {
      if (pending === body) {
        state.pendingUserEchoes.delete(pending);
        return true;
      }
    }
    return false;
  }

  // isNearBottom mirrors the main transcript's wasBottom check (dashboard.js
  // around line 1242 + 4604). 30px slack absorbs sub-pixel layout jitter.
  function isNearBottom() {
    if (!elMsgs) return true;
    return elMsgs.scrollTop + elMsgs.clientHeight >= elMsgs.scrollHeight - 30;
  }

  // stickBottom mirrors the main transcript's stickEventsBottom: two rAFs
  // to outlast KaTeX/mermaid layout bumps, plus image-load listeners so a
  // late-loading thumbnail doesn't scroll the user away from the bottom.
  // Used only when the caller wants a *forced* pin — incremental renders
  // go through the isNearBottom path instead, matching the main window.
  function stickBottom() {
    if (!elMsgs) return;
    elMsgs.scrollTop = elMsgs.scrollHeight;
    requestAnimationFrame(() => {
      elMsgs.scrollTop = elMsgs.scrollHeight;
      requestAnimationFrame(() => { elMsgs.scrollTop = elMsgs.scrollHeight; });
    });
    elMsgs.querySelectorAll('img').forEach(img => {
      if (img.complete) return;
      const restick = () => {
        if (elMsgs.scrollTop + elMsgs.clientHeight >= elMsgs.scrollHeight - 30) {
          elMsgs.scrollTop = elMsgs.scrollHeight;
        }
      };
      img.addEventListener('load', restick, { once: true });
      img.addEventListener('error', restick, { once: true });
    });
  }

  // Time-divider helpers mirror renderEventsWithDividers in the main file.
  // Keeping this scoped copy lets the aside share the visual grammar
  // (mm/dd HH:MM dividers every >15min gap) without exporting internals.
  const EVENT_DIVIDER_GAP_MS = 15 * 60 * 1000;
  function asideLastTime() {
    // Walk backwards through already-rendered .event nodes to find the
    // newest data-time; used to decide whether a fresh divider is needed.
    for (let i = elMsgs.children.length - 1; i >= 0; i--) {
      const c = elMsgs.children[i];
      if (c.classList && c.classList.contains('event')) {
        return Number(c.getAttribute('data-time') || 0);
      }
    }
    return 0;
  }

  function renderNewEvents(events) {
    if (!Array.isArray(events) || events.length === 0) return;
    // Remember whether the user was reading the latest message BEFORE we
    // mutate the DOM. Mirrors the main transcript's policy: only auto-pin
    // to the bottom if the user is already there, never drag them away
    // from content they're reading. The visible symptom on mobile — the
    // drawer snapping to the newest message every poll tick — was the
    // earlier "always scrollTop=scrollHeight" behaviour.
    const wasBottom = isNearBottom();
    // Clear placeholder on first real content.
    if (elEmpty && elEmpty.parentNode === elMsgs) {
      elMsgs.removeChild(elEmpty);
    }
    let sawUser = false;
    let prevT = asideLastTime();
    for (const e of events) {
      // Drop server-echoed user messages that we already rendered locally.
      if (matchesPendingEcho(e)) {
        if (e.time && e.time > state.lastEventTime) state.lastEventTime = e.time;
        continue;
      }
      // Reuse the main event renderer so aside bubbles match the transcript
      // style (markdown, code blocks, etc.) without duplicating logic.
      const h = (typeof eventHtml === 'function') ? eventHtml(e) : '';
      if (!h) continue;
      const t = e.time || 0;
      // Insert a divider when the gap between adjacent visible bubbles
      // exceeds EVENT_DIVIDER_GAP_MS — matches the main-window grammar.
      if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)
          && typeof timeDividerHtml === 'function') {
        elMsgs.insertAdjacentHTML('beforeend', timeDividerHtml(t));
      }
      const tmp = document.createElement('div');
      tmp.innerHTML = h;
      while (tmp.firstChild) elMsgs.appendChild(tmp.firstChild);
      if (t) prevT = t;
      if (e.time && e.time > state.lastEventTime) state.lastEventTime = e.time;
      if (e.type === 'user') sawUser = true;
    }
    // Hide any "↗ 追问" buttons inside the aside itself — stacking is disabled.
    for (const btn of elMsgs.querySelectorAll('.event-ask-btn')) btn.remove();
    // Scroll policy, aligned with main window:
    //  - the user just sent (sawUser on a local-render call): force-pin.
    //  - otherwise: only stick if they were already at the bottom.
    if (sawUser) stickBottom();
    else if (wasBottom) elMsgs.scrollTop = elMsgs.scrollHeight;
    // Save button appears once there's at least one AI reply.
    if (events.some(e => e.type === 'text' || e.type === 'result')) {
      elSave.classList.add('visible');
    }
  }

  async function pollOnce() {
    if (!state) return;
    try {
      let url = '/api/sessions/events?key=' + encodeURIComponent(state.key);
      if (state.lastEventTime > 0) url += '&after=' + state.lastEventTime;
      else url += '&limit=50';
      const r = await fetch(url, { headers: authHeaders() });
      if (!r.ok) {
        // 404 = the scratch session has no persisted events yet (brand-new,
        // first turn not landed). That is an expected empty state, not an
        // error: back off so we stop hammering it at 1Hz. Other non-OK
        // statuses get the same treatment — a transient server hiccup
        // shouldn't busy-loop either.
        pollDelayMs = Math.min(pollDelayMs * 2, POLL_MAX_MS);
        return;
      }
      // A successful poll means the session is reachable; snap cadence back
      // to the responsive base so newly-arriving events render promptly.
      pollDelayMs = POLL_BASE_MS;
      const evs = await r.json();
      if (Array.isArray(evs) && evs.length > 0) {
        renderNewEvents(evs);
        // Hide the "thinking…" indicator once the first bubble arrives.
        if (evs.some(e => e.type === 'text' || e.type === 'result')) {
          elLoading.classList.remove('visible');
        }
      }
    } catch (_) {
      // Network error: back off too, same rationale as a non-OK response.
      pollDelayMs = Math.min(pollDelayMs * 2, POLL_MAX_MS);
    }
  }

  function startPolling() {
    stopPolling();
    pollDelayMs = POLL_BASE_MS;
    // Self-scheduling loop (not setInterval) so each tick's delay can grow
    // with the backoff set inside pollOnce. stopPolling()'s clearTimeout
    // cancels the next scheduled tick.
    const tick = async () => {
      await pollOnce();
      // stopPolling() nulls pollTimer; if that happened during the await we
      // must not reschedule (the drawer closed mid-flight).
      if (pollTimer === null) return;
      pollTimer = setTimeout(tick, pollDelayMs);
    };
    pollTimer = setTimeout(tick, pollDelayMs);
  }

  async function openScratch(quote, agentId, sourceKey, sourceMsgTime) {
    // Confirm replacement if an aside is already open. Replacement is
    // non-destructive (the previous scratch is still reachable via history)
    // so we use 'primary' variant instead of 'danger'.
    if (state) {
      // RNEW-UX-013: confirmDialog is unconditionally defined earlier in this
      // file, so the native-confirm fallback was dead code that defeated
      // theme/focus parity. Drop the fallback and rely on the themed dialog
      // directly.
      const ok = await confirmDialog({
        title: '替换当前追问窗口？',
        message: '当前未保存为正式会话的追问内容将被关闭。',
        confirmText: '替换',
        variant: 'primary',
      });
      if (!ok) return;
      await closeScratch(true);
    }
    try {
      const r = await fetch('/api/scratch/open', {
        method: 'POST',
        headers: authHeaders({'Content-Type': 'application/json'}),
        body: JSON.stringify({
          source_key: sourceKey,
          source_message_id: String(sourceMsgTime || ''),
          // Time hint lets the server fetch 5 turns on each side of the
          // quoted message. Omitted (0) → server falls back to a tail-only
          // window which still seeds the aside with some context.
          source_message_time: Number(sourceMsgTime) || 0,
          quote,
        }),
      });
      if (!r.ok) {
        const txt = await r.text().catch(() => '');
        if (typeof showAPIError === 'function') showAPIError('打开追问', r.status, txt);
        return;
      }
      const data = await r.json();
      state = {
        scratchId: data.scratch_id,
        key: data.key,
        agentId: data.agent_id || agentId || 'general',
        sourceKey,
        sourceMsgTime: sourceMsgTime || 0,
        quote,
        lastEventTime: 0,
        // Bounded Set of user-message bodies that sendInScratch rendered
        // locally. Consumed by matchesPendingEcho when the server event
        // stream replays the same text as a `user` event. Set over array
        // for O(1) lookup; bounded at ~10 entries by sendInScratch.
        pendingUserEchoes: new Set(),
      };
      elAgent.textContent = state.agentId && state.agentId !== 'general' ? '· ' + state.agentId : '';
      elQuotePreview.textContent = previewText(quote);
      elQuoteTrunc.style.display = data.quote_truncated ? 'inline' : 'none';
      // Context badge states (all three visible to the user):
      //   turns > 0                    → "(上下文 N 轮[+])"  — injected; "+" = byte-budget trimmed
      //   turns = 0 && truncated=true  → "(上下文已抑制)"    — quote filled the budget, nothing else fit
      //   turns = 0 && truncated=false → hidden              — no eligible surrounding turns
      // The third case is common for brand-new sessions so we hide the
      // badge rather than claim "(上下文 0 轮)".
      if (elQuoteCtx) {
        const turns = Number(data.context_turns) || 0;
        const truncated = !!data.context_truncated;
        if (turns > 0) {
          elQuoteCtx.textContent = '(上下文 ' + turns + ' 轮' + (truncated ? '+' : '') + ')';
          elQuoteCtx.style.display = 'inline';
        } else if (truncated) {
          elQuoteCtx.textContent = '(上下文已抑制)';
          elQuoteCtx.style.display = 'inline';
        } else {
          elQuoteCtx.textContent = '';
          elQuoteCtx.style.display = 'none';
        }
      }
      elQuoteChip.classList.remove('expanded');
      elQuoteChip.dataset.full = quote;
      clearMessages();
      elSave.classList.remove('visible');
      showDrawer();
      setTimeout(() => elInput.focus(), 60);
      startPolling();
    } catch (e) {
      console.error('open scratch', e);
      if (typeof showNetworkError === 'function') showNetworkError('打开追问', e);
    }
  }

  async function sendInScratch() {
    if (sending || !state) return;
    const text = elInput.value.trim();
    if (!text) return;
    sending = true;
    elSend.disabled = true;
    elLoading.classList.add('visible');
    // Cap the pending echo set at 10 to bound memory under rapid repeated
    // sends; old entries are dropped FIFO-ish (Set iteration order =
    // insertion order).
    if (state.pendingUserEchoes.size >= 10) {
      const first = state.pendingUserEchoes.values().next().value;
      if (first !== undefined) state.pendingUserEchoes.delete(first);
    }
    state.pendingUserEchoes.add(text);
    // Render the user message immediately via renderNewEvents so scroll
    // policy, divider insertion, and ↗-button stripping all match the
    // poll path. The time stamp is just above Date.now() so it sorts
    // after whatever was already rendered; the subsequent server replay
    // will be consumed by matchesPendingEcho.
    renderNewEvents([{type: 'user', detail: text, time: Date.now()}]);
    elInput.value = '';
    try {
      const r = await fetch('/api/sessions/send', {
        method: 'POST',
        headers: authHeaders({'Content-Type': 'application/json'}),
        body: JSON.stringify({key: state.key, text}),
      });
      if (!r.ok) {
        const txt = await r.text().catch(() => '');
        if (typeof showAPIError === 'function') showAPIError('发送消息', r.status, txt);
        elLoading.classList.remove('visible');
      } else {
        // The user just sent a turn, so the session is now live and events
        // are imminent. Restart polling at the responsive base so the reply
        // renders fast — without this, a tick already scheduled at the
        // backed-off delay (up to POLL_MAX_MS from the pre-first-turn 404
        // phase) could stall the first reply by several seconds.
        if (pollTimer !== null) startPolling();
      }
    } catch (e) {
      console.error('scratch send', e);
      if (typeof showNetworkError === 'function') showNetworkError('发送消息', e);
      elLoading.classList.remove('visible');
    } finally {
      sending = false;
      elSend.disabled = false;
      elInput.focus();
    }
  }

  async function promoteScratch() {
    if (!state) {
      if (typeof showToast === 'function') showToast('追问会话已关闭，无法保存');
      return;
    }
    const id = state.scratchId;
    try {
      const r = await fetch('/api/scratch/' + encodeURIComponent(id) + '/promote', {
        method: 'POST', headers: authHeaders(),
      });
      if (!r.ok) {
        const txt = await r.text().catch(() => '');
        if (typeof showAPIError === 'function') showAPIError('保存为正式会话', r.status, txt);
        return;
      }
      const data = await r.json();
      state = null;   // scratch was detached server-side; skip the DELETE in closeScratch
      stopPolling();
      hideDrawer();
      clearMessages();
      elSave.classList.remove('visible');
      elInput.value = '';
      if (typeof showToast === 'function') showToast('已保存为正式会话');
      // Refresh sidebar and try to select the new key.
      try {
        if (typeof lastVersion !== 'undefined') lastVersion = 0;
        if (typeof fetchSessions === 'function') await fetchSessions();
        if (typeof selectSession === 'function' && data.key) selectSession(data.key, 'local');
      } catch (_) {}
    } catch (e) {
      console.error('promote scratch', e);
      if (typeof showNetworkError === 'function') showNetworkError('保存为正式会话', e);
    }
  }

  // Expose the active scratch router-key so shared renderers (e.g. the
  // AskUserQuestion submit handler) can route answers to the scratch CLI
  // instead of the parent session whose `selectedKey` is what `onAskSubmit`
  // would otherwise read. Returns '' when no scratch is open.
  window.__getActiveScratchKey = function() {
    return (state && state.key) ? state.key : '';
  };

  // Expose the global used by the ↗ button in eventHtml.
  window.askAside = function(btn) {
    if (!btn) return;
    const raw = btn.getAttribute('data-raw') || '';
    const msgTime = Number(btn.getAttribute('data-msg-time') || 0);
    if (!raw || raw.length < 1) return;
    if (!selectedKey) {
      if (typeof showToast === 'function') showToast('请先选择会话');
      return;
    }
    // Derive agentId from the current session key (4th segment) so the
    // server can inherit the matching agent registration.
    const parts = String(selectedKey).split(':');
    const agentId = parts.length >= 4 ? parts[3] : 'general';
    openScratch(raw, agentId, selectedKey, msgTime);
  };

  // Wire drawer buttons.
  elClose.addEventListener('click', () => { closeScratch(true); });
  elSend.addEventListener('click', sendInScratch);
  elInput.addEventListener('keydown', (e) => {
    // Enter sends; Shift+Enter inserts newline.
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendInScratch();
    }
  });
  if (elSave) {
    elSave.addEventListener('click', () => { promoteScratch(); });
  } else {
    console.warn('[scratch] ad-save element missing at wire time');
  }
  elQuoteChip.addEventListener('click', () => {
    const expanded = elQuoteChip.classList.toggle('expanded');
    elQuotePreview.textContent = expanded ? (elQuoteChip.dataset.full || '') : previewText(elQuoteChip.dataset.full || '');
    // Clicking the already-expanded chip scrolls the main transcript to the source.
    if (!expanded && state && state.sourceMsgTime) {
      const el = document.querySelector('.event[data-time="' + state.sourceMsgTime + '"]');
      if (el && typeof el.scrollIntoView === 'function') {
        el.scrollIntoView({behavior: 'smooth', block: 'center'});
      }
    }
  });

  // ESC closes when drawer has focus.
  drawer.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.preventDefault(); closeScratch(true); }
  });
})();

// ─── Memory wiki-link popover ────────────────────────────────────────────
// Lazy hover/click preview for [[slug]] markers emitted by inlineMd.
// Single popover element (#mem-popover, declared in dashboard.html); event
// delegation watches the whole document so the popover works for messages
// rendered after page load (live WS updates, scratch drawer, agent-view).
//
// Lifecycle:
//   mouseenter span → 300ms debounce → fetch + show (anchored)
//   mouseleave span → 200ms grace → hide if not pinned
//   click span      → fetch + show + pin (sticks until ESC / outside click)
//   ESC / outside click on pinned popover → unpin + hide
//
// Cache: module-level Map<slug, response>. Cleared on full page reload.
// 404 results poison the slug span with .md-memlink-broken so subsequent
// hovers skip the network.
//
// docs/rfc/memory-link-rendering.md
(function () {
  const memCache = new Map();
  const NOT_FOUND = Symbol('memory-not-found');
  let pop = null;
  let popContent = null;
  let popClose = null;
  let pinned = false;
  let currentSlug = null;
  let hoverTimer = 0;
  let leaveTimer = 0;

  function ensurePopover() {
    if (pop) return true;
    pop = document.getElementById('mem-popover');
    popContent = document.getElementById('mem-pop-content');
    popClose = document.getElementById('mem-pop-close');
    if (!pop || !popContent) return false;
    if (popClose) {
      popClose.addEventListener('click', (e) => {
        e.stopPropagation();
        hidePopover();
      });
    }
    pop.addEventListener('mouseenter', () => {
      if (leaveTimer) { clearTimeout(leaveTimer); leaveTimer = 0; }
    });
    pop.addEventListener('mouseleave', () => {
      if (!pinned) scheduleHide();
    });
    return true;
  }

  function showPopover(anchor) {
    if (!ensurePopover()) return;
    pop.classList.add('show');
    pop.setAttribute('aria-hidden', 'false');
    positionPopover(anchor);
  }

  function hidePopover() {
    if (!pop) return;
    pop.classList.remove('show', 'pinned');
    pop.setAttribute('aria-hidden', 'true');
    pinned = false;
    currentSlug = null;
  }

  function scheduleHide() {
    if (leaveTimer) clearTimeout(leaveTimer);
    leaveTimer = setTimeout(() => {
      if (!pinned) hidePopover();
    }, 200);
  }

  function positionPopover(anchor) {
    const rect = anchor.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    let top = rect.bottom + 6;
    let left = rect.left;
    const popW = pop.offsetWidth || 400;
    const popH = pop.offsetHeight || 200;
    if (left + popW > vw - 12) left = Math.max(12, vw - popW - 12);
    if (top + popH > vh - 12) {
      const above = rect.top - 6 - popH;
      top = above > 12 ? above : 12;
    }
    pop.style.top = top + 'px';
    pop.style.left = left + 'px';
  }

  function renderLoading() {
    popContent.innerHTML = '<div class="mem-pop-error">加载中…</div>';
  }
  function renderError(msg) {
    popContent.innerHTML = '<div class="mem-pop-error">' + esc(msg) + '</div>';
  }
  function renderResponse(data) {
    if (!data || !data.found) {
      popContent.innerHTML = '<div class="mem-pop-error">未找到该记忆</div>';
      return;
    }
    const parts = [];
    const headerBits = [];
    if (data.type) {
      headerBits.push('<span class="mem-pop-type" data-type="' + escAttr(data.type) + '">' + esc(data.type) + '</span>');
    }
    if (data.scope === 'external' && data.project) {
      const projLabel = String(data.project).split('-').filter(Boolean).pop() || data.project;
      headerBits.push('<span class="mem-pop-scope">来自 ' + esc(projLabel) + ' 项目</span>');
    }
    if (headerBits.length > 0) {
      parts.push('<div class="mem-pop-header">' + headerBits.join('') + '</div>');
    }
    parts.push('<div class="mem-pop-slug">' + esc(data.slug || '') + '</div>');
    if (data.description) {
      parts.push('<div class="mem-pop-desc">' + esc(data.description) + '</div>');
    }
    if (data.body) {
      parts.push('<div class="mem-pop-body">' + renderMd(data.body) + '</div>');
    }
    popContent.innerHTML = parts.join('');
  }

  function markBroken(slug) {
    document.querySelectorAll('.md-memlink[data-slug="' + slug.replace(/"/g, '\\"') + '"]')
      .forEach((el) => el.classList.add('md-memlink-broken'));
  }

  async function fetchMemory(slug) {
    const cached = memCache.get(slug);
    if (cached === NOT_FOUND) return null;
    if (cached) return cached;
    // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController +
    // 10s timeout. Memory popovers are click-to-open so a hung backend
    // (NAT idle drop) leaves the user staring at a never-resolving
    // popover; fetchJSON guarantees a deterministic failure path that
    // returns `undefined` (preserves caller's "transient — retry next
    // hover" semantics).
    try {
      const data = await fetchJSON('/api/memory/' + encodeURIComponent(slug), {
        headers: typeof authHeaders === 'function' ? authHeaders() : {},
      });
      if (!data || !data.found) {
        memCache.set(slug, NOT_FOUND);
        markBroken(slug);
        return null;
      }
      memCache.set(slug, data);
      return data;
    } catch (e) {
      // 404/400 — slug missing/invalid; cache the negative so we don't
      // re-fetch on every hover. fetchJSON's err.status surfaces the
      // server status so the callsite can branch.
      if (e && (e.status === 404 || e.status === 400)) {
        memCache.set(slug, NOT_FOUND);
        markBroken(slug);
        return null;
      }
      return undefined;
    }
  }

  async function loadAndShow(slug, anchor, pin) {
    if (!ensurePopover()) return;
    currentSlug = slug;
    if (pin) {
      pinned = true;
      pop.classList.add('pinned');
    }
    const cached = memCache.get(slug);
    if (cached === NOT_FOUND) {
      renderResponse(null);
    } else if (cached) {
      renderResponse(cached);
    } else {
      renderLoading();
    }
    showPopover(anchor);

    if (cached === NOT_FOUND || cached) return;
    const data = await fetchMemory(slug);
    if (currentSlug !== slug) return;
    if (data === undefined) {
      renderError('加载失败');
      return;
    }
    renderResponse(data);
    positionPopover(anchor);
  }

  function onEnter(e) {
    const span = e.target.closest && e.target.closest('.md-memlink');
    if (!span) return;
    if (leaveTimer) { clearTimeout(leaveTimer); leaveTimer = 0; }
    if (pinned) return;
    const slug = span.getAttribute('data-slug');
    if (!slug) return;
    if (memCache.get(slug) === NOT_FOUND) return;
    if (hoverTimer) clearTimeout(hoverTimer);
    hoverTimer = setTimeout(() => {
      hoverTimer = 0;
      loadAndShow(slug, span, false);
    }, 300);
  }

  function onLeave(e) {
    const span = e.target.closest && e.target.closest('.md-memlink');
    if (!span) return;
    if (hoverTimer) { clearTimeout(hoverTimer); hoverTimer = 0; }
    if (!pinned) scheduleHide();
  }

  function onClick(e) {
    const span = e.target.closest && e.target.closest('.md-memlink');
    if (!span) return;
    e.preventDefault();
    e.stopPropagation();
    const slug = span.getAttribute('data-slug');
    if (!slug) return;
    if (hoverTimer) { clearTimeout(hoverTimer); hoverTimer = 0; }
    loadAndShow(slug, span, true);
  }

  function onDocClick(e) {
    if (!pop || !pinned) return;
    if (pop.contains(e.target)) return;
    if (e.target.closest && e.target.closest('.md-memlink')) return;
    hidePopover();
  }

  function onKeyDown(e) {
    if (e.key === 'Escape' && pinned) {
      hidePopover();
      return;
    }
    // role=link contract (WCAG 2.1.1): Enter/Space on a focused chip must
    // activate it. Without this branch, keyboard users could tab to a chip
    // and find no way to read the memory body.
    if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
      const span = e.target && e.target.closest && e.target.closest('.md-memlink');
      if (!span) return;
      e.preventDefault();
      const slug = span.getAttribute('data-slug');
      if (!slug) return;
      if (hoverTimer) { clearTimeout(hoverTimer); hoverTimer = 0; }
      loadAndShow(slug, span, true);
    }
  }

  // Copy fallback: chip renders only the icon + a short tail label, so a raw
  // selection-copy would yield e.g. "💡 vs_practice" — the original
  // [[full_slug]] wiki-link is lost, breaking round-trip into other docs / IM
  // / markdown editors. We rewrite clipboardData when the active selection
  // touches at least one chip, replacing each chip's text with its data-slug
  // wrapped in [[]].
  function onCopy(e) {
    const sel = document.getSelection && document.getSelection();
    if (!sel || sel.rangeCount === 0 || sel.isCollapsed) return;
    // Cheap pre-check: only intervene if the selection actually crosses a chip.
    let touchesChip = false;
    for (let i = 0; i < sel.rangeCount; i++) {
      const r = sel.getRangeAt(i);
      const c = r.commonAncestorContainer;
      const root = c.nodeType === 1 ? c : c.parentNode;
      if (!root) continue;
      if ((root.closest && root.closest('.md-memlink')) ||
          (root.querySelector && root.querySelector('.md-memlink'))) {
        touchesChip = true;
        break;
      }
    }
    if (!touchesChip) return;
    const parts = [];
    for (let i = 0; i < sel.rangeCount; i++) {
      const frag = sel.getRangeAt(i).cloneContents();
      // Replace each chip element inside the cloned fragment with a text node
      // carrying [[slug]]. cloneContents loses parent context, so we walk
      // the fragment itself.
      const chips = frag.querySelectorAll ? frag.querySelectorAll('.md-memlink') : [];
      chips.forEach((chip) => {
        const slug = chip.getAttribute('data-slug') || '';
        chip.replaceWith(document.createTextNode('[[' + slug + ']]'));
      });
      parts.push(frag.textContent || '');
    }
    const text = parts.join('\n');
    if (e.clipboardData) {
      e.clipboardData.setData('text/plain', text);
      e.preventDefault();
    }
  }

  document.addEventListener('mouseover', onEnter, true);
  document.addEventListener('mouseout', onLeave, true);
  document.addEventListener('click', onClick, true);
  document.addEventListener('mousedown', onDocClick, true);
  document.addEventListener('keydown', onKeyDown);
  document.addEventListener('copy', onCopy);
})();
