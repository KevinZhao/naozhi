import { esc, escAttr, fetchJSON, showToast, trapFocus, nzState, nzBus, nzViews, nzTest, registerActions  , isCronSessionKey } from './nz_util.js';
import {
  BLOCK_SPLIT_RE,
  LIST_ITEM_RE,
  LIST_SHAPE_RE,
  MAX_LIST_DEPTH,
  _mdCache,
  isMathDisplay,
  isMathInline,
  katexPending,
  katexReady,
  configureRenderMd,
  loadKatex,
  loadMermaid,
  parseListItem,
  renderKatex,
  renderMd,
  renderRich,
  renderTable,
  runPendingAsync,
} from './render_md.js';
import { configureSelfUpdate } from './self_update.js';
import {
  configureSessionHeader,
  fetchSessionRuns,
  gitChipHtml,
  gitStateCache,
  setHeaderEffortChip,
  setHeaderGitChip,
  setHeaderOverlayDriftChip,
  setHeaderSpawnDiagChip,
} from './session_header.js';
import {
  awaitPendingOrients,
  configureComposerFiles,
  handleFiles,
  onThumbDragEnd,
  onThumbDragLeave,
  onThumbDragOver,
  onThumbDragStart,
  onThumbDrop,
  onThumbKeyDown,
  openFilePicker,
  removeFile,
  renderFilePreviews,
  retryUpload,
} from './composer_files.js';
import {
  collapseSidebarForDrawer,
  configureMobileNav,
  initMobile,
  initSwipeBack,
  initSwipeDelete,
  initViewportTracking,
  isMobile,
  mobileBack,
  mobileEnterChat,
  restoreSidebarAfterDrawer,
  toggleSidebarCollapsed,
} from './mobile_nav.js';import {
  configureVoice,
  escCloseVoiceOverlay,
  toggleInputMode,
  voiceInputMode,
  voiceMouseDown,
  voiceTouchStart,
} from './voice.js';
import {
  configureSplitView,
  nzAnyDrawerOpen,
  nzSplitBringToFront,
  nzSplitEnter,
  nzSplitExit,
} from './split_view.js';
import {
  configureSystemView,
  deselectNodeSession,
  fetchSystemDaemons,
  openSystemPanel,
  reconcileSelectedNode,
  renderSystemView,
  stopSystemPoll,
} from './system_view.js';
import {
  applyEventToTurnState,
  configureRunningBanner,
  interruptSession,
  paintTurnElapsed,
  refreshBanner,
  resetTurnState,
  resetTurnStateForUserEcho,
  restoreScrollPos,
  saveScrollPos,
  scrollSlackPx,
  startTurnTimer,
  stickEventsBottom,
  turnState,
} from './running_banner.js';
import {
  FILE_REF_HAS_EXT,
  closeFilePreview,
  configureFileRefs,
  fencedPathList,
  fileRefCode,
  formatFileSize,
  isFileRefCandidate,
  isMultiNode,
  nodeColor,
  processEventsForDisplay,
  regroupAvatars,
  setActiveSessionCard,
  sid,
  splitPathLine,
  startFileRefObserver,
} from './file_refs.js';
import {
  AVATAR_GROUP_GAP_MS,
  CRON_LIVE_MAX_EVENTS,
  EARLIER_PAGE_LIMIT,
  EVENT_DIVIDER_GAP_MS,
  INITIAL_HISTORY_LIMIT,
  MAX_LIVE_DOM_EVENTS,
  announce,
  configureUtilities,
  confirmDialog,
  copyCodeBlock,
  copyEventContent,
  decodeEscEntities,
  formatAbsTime,
  formatTimeFull,
  historyDayLabel,
  mainEmptyHtml,
  promptDialog,
  reconnectNow,
  refreshCostSummary,
  renderRecentSessionsPanel,
  renderServiceOverviewHtml,
  safeUrl,
  shortPath,
  showAPIError,
  showNetworkError,
  startSidebarTimeTick,
  stopSidebarTimeTick,
  timeAgo,
  timeDividerHtml,
} from './utilities.js';
// Service worker registration
if('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(()=>{});

let selectedKey = null;
// #2431: last (session, state) pushed through updateSendButton — the main-area
// state that is actually on screen, whichever path (WS push, optimistic flip,
// renderMainShell, REST reconcile) applied it. The fetchSessions reconcile
// compares against this so a 5 s fallback poll only re-applies a state that
// has actually changed. Cleared on session switch.
let _lastAppliedMainState = null;
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
// costSummaryCache is the ledger's last-30-day unit-bucketed total from
// /api/cost/summary (null until the first fetch lands); the 服务概览 花费 card
// prefers it over the live-session sum, which forgets deleted sessions and cron.
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
let lastVersion = 0;
let lastNodesJSON = '';
let lastHistoryJSON = '';
// _lastSidebarData caches the most recent /api/sessions payload so the
// sidebar can re-render locally without re-hitting the server. Set by
// fetchSessions after a successful render.
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
// _previewGen is bumped on every previewDiscovered() entry. The awaited
// preview fetch and the 2s poll tick compare their captured generation
// against it so a stale call can't render into (or start a second interval
// for) a card the operator has since clicked away from.
let _previewGen = 0;
let pendingDiscovered = null; // {pid, sessionId, cwd, procStartTime, node} when previewing a discovered session
let sessionCounter = 0;
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
const sessionAccessProfiles = {}; // per-session access profile picked at creation ("" = global default)
// Header-chip picks made on a session that has no server entry yet (created,
// no message sent). The server parks them and applies them on first spawn;
// this mirror lets the chips show the pick meanwhile. Dropped on promotion.
const sessionPendingTuning = {}; // key -> { model, effort }
let cliBackends = null; // cached LOCAL /api/cli/backends response: {backends, default, detected}
let cliBackendsFetchedAt = 0;
// Per-node backend manifest cache for the node-aware new-session picker.
// Keyed by node id ('local' or a remote node id). Values: {data, at}. The
// global cliBackends above stays LOCAL-only — every chip / feature-gate /
// cost-unit consumer reads the local manifest — while the picker resolves
// the manifest for whichever node the "New Session" modal targets, so a
// remote node's backends + default drive the picker (picker node-aware fix).
const cliBackendsByNode = {};
let accessProfiles = null; // cached /api/access-profiles response: {profiles, default}
let accessProfilesFetchedAt = 0;
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
// sessionOptimisticPrevState: sid(key,node) -> 乐观翻转成 'running' 之前，服务端
// 最后报告的真实状态。onSessionState 判 dead→running 重订阅时必须用它：翻转发生
// 在网络往返之前，所以对每一次本页发起的 send，服务端真正的 running 广播到达时
// sessionsData[sKey].state 恒为 'running'，直接读它会把"进程被回收后从本页发消息"
// 这个最常见的失联场景判成普通 ready→running。与 sessionOptimisticRunning 同生
// 同灭。
const sessionOptimisticPrevState = {};
// sessionLastSent: sid(key,node) -> 最近一次发出的用户文本（当前 turn 的输入）。
// 在 sendMessage 成功发出后记录；turn 自然跑完 (running→ready/dead) 时清掉。
// 若用户在 running 中点击中断，则把这段文本回填到 #msg-input（Claude Code
// 的中断-回填行为），方便修改后重发。只在输入框当前为空时回填，避免覆盖
// 用户已经开始敲的新内容。
const sessionLastSent = {};
// httpSendPending: sids this tab has an HTTP send in flight for (added before
// the request leaves, cleared on sync rejection / send_error / the key's next
// ready|dead state). onSendError gates on it so a send_error fanned out to
// every subscriber of the key is acted on only by the tab that sent — and,
// unlike sessionLastSent (which carries interrupt re-fill semantics and is
// only set when there is text), it also covers image-only sends, the main
// HTTP-send case.
const httpSendPending = new Set();
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
    if (sessionAccessProfiles[k]) entry.access_profile = sessionAccessProfiles[k];
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
    if (typeof v.access_profile === 'string' && v.access_profile) sessionAccessProfiles[k] = v.access_profile;
  }
}

// eagerBindWorkspace tells the backend the chosen workspace the moment a
// session is created, instead of waiting for the first send to carry it. This
// writes the per-chat override eagerly (server-side validateWorkspace +
// SetWorkspace), so even a session opened in another browser/device — or one
// reloaded before its first send — spawns into the right directory. Local
// nodes only: remote sessions resolve their workspace on their own node.
// Fire-and-forget — never blocks or fails the creation flow: localStorage /
// network errors are swallowed rather than aborting session creation.
function eagerBindWorkspace(key, workspace, node) {
  if (!key || !workspace) return;
  const nd = node || 'local';
  if (nd !== 'local') return;
  try {
    const headers = { 'Content-Type': 'application/json' };
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    fetch(NZ_CONTRACT.API.sessions_bind, {
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
// applyTheme sets data-theme + writes the localStorage early-paint cache, and
// when persist is true also pushes the choice to the server (PUT /api/settings)
// so it survives a browser/device change or cache clear. localStorage stays
// the first-paint source of truth (the inline applier in dashboard.html reads
// it before any JS) — the server copy is reconciled in on load by syncThemeFromServer.
// persist defaults false so the load-time reconcile and the initial
// applyTheme(getCurrentTheme()) don't echo back to the server.
function applyTheme(theme, persist) {
  if (THEME_ORDER.indexOf(theme) < 0) theme = 'auto';
  document.documentElement.setAttribute('data-theme', theme);
  try { localStorage.setItem(THEME_LS_KEY, theme); } catch (_) {}
  syncThemeColorMeta();
  if (persist) persistTheme(theme);
}
// persistTheme writes the theme to the server, best-effort: a failure leaves
// the localStorage copy intact (still applied locally) and just surfaces a
// toast, since the change isn't lost — only un-synced to other devices.
function persistTheme(theme) {
  const headers = { 'Content-Type': 'application/json' };
  const t = getToken();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  fetchJSON(NZ_CONTRACT.API.settings, { method: 'PUT', headers, body: JSON.stringify({ theme: theme }), timeoutMs: 10000 })
    .catch(function (err) {
      showToast('主题已应用，但未能保存到服务器', 'error');
    });
}
// syncThemeFromServer pulls the server-persisted theme on load and reconciles
// it with the localStorage cache. The server is the cross-device source of
// truth, so a differing server value wins and is re-applied (without
// re-persisting). On error/no-store we keep whatever localStorage gave us.
function syncThemeFromServer() {
  const headers = {};
  const t = getToken();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  fetchJSON(NZ_CONTRACT.API.settings, { headers, timeoutMs: 8000 })
    .then(function (s) {
      const srv = s && s.theme;
      if (THEME_ORDER.indexOf(srv) < 0) return;       // unknown/empty → keep local
      if (srv === getCurrentTheme()) return;            // already in sync
      applyTheme(srv);                                  // server wins; persist=false
      if (activeView === 'settings') renderSettingsView(); // refresh active pill if open
    })
    .catch(function () { /* offline / pre-persist server: keep localStorage */ });
}
// Keep the PWA/browser chrome (the standalone title bar, mobile status bar)
// in sync with the active theme. The manifest's static theme_color only
// covers first paint; once the page is live the resolved --nz-bg-0 is the
// source of truth, so a dark bar no longer bleeds through in light mode.
// Reads computed style after the data-theme attribute is set so 'auto' picks
// up the OS preference too.
function syncThemeColorMeta() {
  try {
    const bg = getComputedStyle(document.documentElement)
      .getPropertyValue('--nz-bg-0').trim();
    if (!bg) return;
    let meta = document.querySelector('meta[name="theme-color"]');
    if (!meta) {
      meta = document.createElement('meta');
      meta.setAttribute('name', 'theme-color');
      document.head.appendChild(meta);
    }
    meta.setAttribute('content', bg);
  } catch (_) {}
}
// In 'auto' mode the resolved bg follows the OS scheme, so re-sync when the
// system preference flips while the page is open.
try {
  if (window.matchMedia) {
    const _themeMql = window.matchMedia('(prefers-color-scheme: light)');
    const _onSchemeChange = function () {
      if (getCurrentTheme() === 'auto') syncThemeColorMeta();
    };
    if (_themeMql.addEventListener) _themeMql.addEventListener('change', _onSchemeChange);
    else if (_themeMql.addListener) _themeMql.addListener(_onSchemeChange);
  }
} catch (_) {}
document.addEventListener('DOMContentLoaded', function () {
  // Rehydrate pending-session workspaces from localStorage BEFORE the first
  // fetchSessions/send so a reload-before-first-send still carries the chosen
  // workspace (#cwd-fallback fix). Idempotent via _pendingRestored.
  restorePending();
  applyTheme(getCurrentTheme());
  // Reconcile with the server-persisted theme (cross-device source of truth).
  // Runs after the localStorage-based applyTheme above so first paint is
  // instant and a differing server value re-applies once it resolves.
  syncThemeFromServer();
  // Activity-bar view switch (codex-style rail). Wired here (not inline
  // onclick) to keep the script-src inline-handler surface from growing
  // (R236-SEC-02 cap). Each abnav-* routes through the top-level
  // setActivityView() which toggles the matching body.nz-view-* class and
  // swaps the chat sidebar/main for the target view's panels in place.
  ['abnav-chat', 'abnav-assets', 'abnav-files', 'abnav-cron', 'abnav-system', 'abnav-settings'].forEach(function (id) {
    const el = document.getElementById(id);
    if (el) el.addEventListener('click', function () { setActivityView(el.dataset.view); });
  });
  // Header/sidebar controls (#922 / #479 / #441): migrated from inline click /
  // submit attributes to addEventListener so the dashboard's script-src no
  // longer needs `'unsafe-inline'` on account of these handlers. (Prose avoids
  // the literal token the CSP ratchet counts — see generatedOnclickCap.)
  // Each bind is guarded so a missing element is a no-op (defensive parity
  // with the theme/nav binds above). Keeps R236-SEC-02 inline-handler count
  // trending to 0.
  const bindClick = function (id, fn) {
    const el = document.getElementById(id);
    if (el) el.addEventListener('click', fn);
  };
  bindClick('btn-history', function () { toggleHistory(); });
  bindClick('btn-new-session', function () { createNewSession(); });
  bindClick('btn-sidebar-toggle', function () { toggleSidebarCollapsed(); });
  // NOTE: the quick-ask form submit is NOT bound here. That form lives in
  // `#main`, which is repainted via innerHTML (mainEmptyHtml()), so its submit
  // handler is (re)bound in wireQuickAskInput() — the designated re-wire hook.
});

function getToken() { return ''; }
// authHeaders builds the Authorization header set for fetch calls. Moved
// here from cron_view (#2557 PR-E1): it wraps getToken, which lives in this
// file, and both views consume it.
function authHeaders() {
  const headers = {};
  const t = getToken();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  return headers;
}

// setActivityView is the root view-router. It is the single owner of the
// mutually-exclusive body.nz-view-* classes and the rail button active state.
// Top-level (not closured) so openCronPanel / selectSession / openCronDetail
// can re-assert the active view.
//
// Recursion note: entering 'cron' calls openCronPanel(), which itself calls
// setActivityView('cron') when not already in cron view. We set activeView
// BEFORE dispatching, so openCronPanel's own `activeView !== 'cron'` guard is
// already false on the re-entry and the loop terminates after one hop.
const ACTIVITY_VIEWS = ['chat', 'assets', 'files', 'cron', 'system', 'settings'];
function setActivityView(view) {
  if (ACTIVITY_VIEWS.indexOf(view) === -1) view = 'chat';
  if (view === activeView) return;
  const prev = activeView;
  activeView = view;
  // Rail button active / aria-pressed state.
  [['abnav-chat', 'chat'], ['abnav-assets', 'assets'], ['abnav-files', 'files'], ['abnav-cron', 'cron'], ['abnav-system', 'system'], ['abnav-settings', 'settings']]
    .forEach(function (pair) {
      const el = document.getElementById(pair[0]);
      if (el) { el.classList.toggle('active', pair[1] === view); el.setAttribute('aria-pressed', String(pair[1] === view)); }
    });
  // Mutually-exclusive view classes. 'chat' clears all of them.
  document.body.classList.toggle('nz-view-assets', view === 'assets');
  document.body.classList.toggle('nz-view-files', view === 'files');
  document.body.classList.toggle('nz-view-cron', view === 'cron');
  document.body.classList.toggle('nz-view-system', view === 'system');
  document.body.classList.toggle('nz-view-settings', view === 'settings');
  // Keep the [hidden] attr in sync with CSS for the always-resident containers
  // (asset_browser.js manages its own hidden flags on show/hide).
  const cm = document.getElementById('cron-main');
  if (cm) cm.hidden = view !== 'cron';
  const sm = document.getElementById('settings-main');
  if (sm) sm.hidden = view !== 'settings';
  const sysm = document.getElementById('system-main');
  if (sysm) sysm.hidden = view !== 'system';
  // Tear down the previous view if it owns external state.
  if (prev === 'assets' && view !== 'assets' && nzViews.asset) nzViews.asset.hide();
  if (prev === 'files' && view !== 'files' && nzViews.files) nzViews.files.hide();
  if (prev === 'system' && view !== 'system') stopSystemPoll();
  // Leaving chat: close any docked preview / 追问 drawer. They are position:fixed
  // siblings of .container with no nz-view-* hide rule, so without this they
  // float over the assets/cron/settings view and leave the split padding
  // reserved. Both close paths run nzSplitExit, clearing nz-split-open.
  if (prev === 'chat' && view !== 'chat') {
    closeFilePreview();
    if (closeScratchDrawer) closeScratchDrawer();
  }
  // Enter the target view.
  if (view === 'assets') { if (nzViews.asset) nzViews.asset.show(); }
  else if (view === 'files') { if (nzViews.files) nzViews.files.show(); }
  else if (view === 'cron') { emitCron('cron:open-panel'); }
  else if (view === 'system') { openSystemPanel(); }
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
  delete sessionAccessProfiles[key];
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
      data = await fetchJSON(NZ_CONTRACT.API.sessions, { headers, timeoutMs: 8000 });
    } catch (err) {
      if (err.status === 401 || err.status === 403) {
        showAuthModal({ auto: true }); // background poll: respects de-dupe + cooldown
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
    // The effort tier changes at turn boundaries, which do NOT advance
    // stats.version (storeGen only moves on session add/remove/rename/reset).
    // renderMainShell doesn't run on turn completion either, so this poll is
    // the only path by which a new tier reaches the screen — and it has to
    // happen BEFORE the version short-circuit below, which is exactly where an
    // earlier revision of this code sat and silently never fired.
    // docs/rfc/kiro-effort-visibility.md §5.1 / R1b
    if (selectedKey) setHeaderEffortChip(data.sessions);
    if (selectedKey) setHeaderSpawnDiagChip(data.sessions);
    if (selectedKey) setHeaderOverlayDriftChip(data.sessions);
    // #2431: under WS-fallback polling this REST poll is the ONLY state source,
    // and process state transitions (running↔ready, last_response, sc-time)
    // never advance stats.version — storeGen moves on add/remove/rename/reset
    // only, and with the socket down no session_state push zeroes lastVersion.
    // The version short-circuit would therefore freeze the sidebar and the
    // "WS disconnected → always reconcile" block further down never ran. Skip
    // the gate while disconnected; renderSidebar is idempotent so the 5 s
    // repaint is the intended fallback cost.
    const wsConnected = wsm.state === WS_STATES.CONNECTED;
    if (wsConnected && version === lastVersion && version > 0 && nodesHash === lastNodesJSON && historyHash === lastHistoryJSON) return;
    lastVersion = version;
    lastNodesJSON = nodesHash;
    lastHistoryJSON = historyHash;
    if (data.nodes) nodesData = data.nodes;
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
    // #2431: map (not forEach) so the optimistic 'running' copy below lands in
    // the payload handed to renderSidebar / stashed as _lastSidebarData — the
    // original objects still say 'ready', so a re-render from the cached
    // payload (toggleProjectCollapsed, sidebar search) painted the card idle
    // while the main banner showed running.
    const sessions = (data.sessions || []).map(s => {
      const n = s.node || 'local';
      const sKey = sid(s.key, n);
      // Preserve the optimistic 'running' flip when the REST snapshot is still
      // lagging behind the send — otherwise the banner appears for a split
      // second, then a /api/sessions poll rewrites state to 'ready' and hides
      // it until the server's real session_state broadcast catches up.
      if (sessionOptimisticRunning[sKey] && s.state !== 'running') {
        s = Object.assign({}, s, { state: 'running' });
      }
      // A workspace change (/cd, or the first snapshot after spawn resolved
      // the real cwd) invalidates the cached git state — the new directory can
      // be a different repo, a different worktree, or no repo at all.
      const prevWS = sessionsData[sKey] && sessionsData[sKey].workspace;
      if (prevWS !== undefined && prevWS !== s.workspace) {
        invalidateGitState(s.key, n);
      }
      sessionsData[sKey] = s;
      backendKeys.add(s.key);
      return s;
    });
    data = Object.assign({}, data, { sessions });

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
        delete sessionAccessProfiles[key];
        delete sessionPendingTuning[key];
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
          // defaultCLIVersion is the DEFAULT backend's live version, tracked
          // from each session's system/init frame and refreshed from stats
          // every poll; backendDisplayVersion() reads the /api/cli/backends
          // manifest cached up to 60s client-side. For a pending session on the
          // default backend, prefer the live value so a host claude upgrade
          // under a long-lived naozhi doesn't make the just-created card flash
          // the pre-upgrade version during the manifest's stale window. A
          // non-default backend (e.g. kiro) has no live source here, so keep
          // its manifest value — preferring defaultCLIVersion would mislabel it
          // with the default backend's version. R20260613-pending-version.
          const isDefaultBackend = !pendingBackend || (cliBackends !== null && pendingBackend === cliBackends.default);
          const pendingCLIVersion = isDefaultBackend
            ? (defaultCLIVersion || backendDisplayVersion(pendingBackend))
            : (backendDisplayVersion(pendingBackend) || defaultCLIVersion);
          // #2431: mirror the server's project/project_fallback shape so an
          // unregistered workspace groups under its basename from the first
          // paint instead of sitting in 未分组 until the first send promotes it.
          const pendingWS = sessionWorkspaces[key];
          const pendingProject = matchProject(pendingWS);
          const pendingFallback = pendingProject ? '' : workspaceFallbackName(pendingWS);
          data.sessions.push({
            key: key,
            state: 'new',
            platform: parts[0] || 'dashboard',
            agent: pendingAgent,
            workspace: pendingWS,
            // Stamp the pending card with "now" so the sidebar sort (oldest
            // top, newest bottom — see renderSidebar) lands it at the bottom
            // immediately. Leaving created_at/last_active at 0 sorts it to the
            // TOP, then the real backend session (with a real created_at) snaps
            // it to the bottom — the card visibly jumps. The server-stamped
            // created_at on the real session is ~equal, so no jump on promote.
            created_at: Date.now(),
            last_active: 0,
            last_prompt: '',
            last_response: '',
            node: sessionNodes[key] || 'local',
            project: pendingProject || pendingFallback,
            project_fallback: !!pendingFallback,
            backend: pendingBackend,
            access_profile: sessionAccessProfiles[key] || '',
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
    // (e.g. session_state WS message was missed), propagate the server-side
    // truth to the banner and send/stop buttons.
    //
    // When WS is disconnected, REST is the only state source — always reconcile.
    // When WS is connected, reconcile ONLY the "finished" direction: if the
    // REST snapshot says the turn is over (state !== 'running') and we're not
    // inside the optimistic-running window, then a terminal WS signal (the
    // 'result' event and/or the 'ready' session_state broadcast) was dropped on
    // the still-open connection — without this fallback the "处理中..." banner
    // stays stuck until the operator switches sessions or reconnects. We never
    // reconcile toward 'running' over a live WS: that's the push side's job, and
    // a lagging REST snapshot would flicker the banner (the very reason the
    // optimistic-running flip at line 469-475 exists).
    if (selectedKey) {
      const sKey = sid(selectedKey, selectedNode);
      const sd = sessionsData[sKey];
      if (sd && (!wsConnected || (sd.state !== 'running' && !sessionOptimisticRunning[sKey]))) {
        // #2431: updateSendButton is not idempotent ('running' re-seeds agent
        // rows from the REST snapshot; 'ready' resets turn state + loading
        // indicator + scroll) and this runs every 5 s under fallback — only
        // re-apply when REST differs from what the main area last applied.
        const applied = _lastAppliedMainState;
        if (!(applied && applied.key === sKey && applied.state === sd.state)) {
          updateMainState(sd.state, sd.death_reason);
        }
      } else if (sd && wsConnected && sd.state === 'running') {
        // Self-heal a DROPPED 'running' session_state push. renderSidebar above
        // always paints the card from the REST snapshot, so the sidebar shows
        // this selected session as running — but the right-side banner is only
        // ever flipped to running by the WS push (the block above refuses to
        // reconcile toward running to avoid banner flicker). If that push was
        // lost, the sidebar says running while the banner stays idle — the one
        // left/right desync the push-only rule leaves open. Reconcile toward
        // running ONLY when the banner is still fully hidden: an already-visible
        // banner (live activity, or zero-downtime background agents) is left
        // untouched, so this can't flicker a banner that's correctly showing.
        const banner = document.getElementById('running-banner');
        if (banner && banner.style.display === 'none') {
          updateMainState('running', sd.death_reason);
        }
      }
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
      key: discoveredKey(d.pid, d.node),
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
  allSessionsCache = allItemsUnfiltered;

  // The sidebar lists every connected node's sessions together — the old
  // per-node filter (driven by the sidebar node selector) was removed when
  // that selector moved into the New Session modal. Each card carries a
  // .sc-node badge (sessionCardHtml) so operators can still tell which
  // connection a session lives on. selectedNode now only tracks which node
  // the *currently-open* session lives on (for dispatch / header), not a
  // sidebar visibility filter, so nothing here is hidden.
  const allItems = allItemsUnfiltered;

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
  // are gone — the project-grouping branch below walks allItems directly.
  const visibleItems = allItems;

  let html = '';
  {
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
    // The sidebar lists every connected node's sessions together (the per-node
    // filter was removed in #2180 when the node selector moved into the New
    // Session modal), so every favorite's header renders regardless of which
    // node it lives on — matching the unfiltered session list above.
    projectsData.forEach(p => {
      if (!p.favorite) return;
      const pNode = p.node || 'local';
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
  // the header `+` button invokes.
  if (!html) html = '<div class="no-sessions">no sessions<br><button type="button" class="no-sessions-cta" data-action="session-new">+ 开启你的第一个会话</button></div>';
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

  // History badge (ui-polish-light-theme D10): the always-on total count
  // ("84") had no action value — it's an archive size, not an unread count —
  // and pulled the eye on every load. The count still shows inside the
  // popover header ("历史（84）"); the button badge stays hidden unless a
  // future alert-grade state (.is-alert/.is-warn) needs it.
  const hBadge = document.getElementById('history-badge');
  if (hBadge) hBadge.style.display = 'none';

  // R110-P1 Home panel: refresh after every sidebar repaint so the
  // "最近会话" list mirrors the authoritative snapshot. Gated by selectedKey
  // inside the helper so the main shell's active session view isn't touched.
  renderRecentSessionsPanel();
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
// workspaceFallbackName mirrors internal/dashboard/session/handlers.go's
// workspaceFallbackName: the folder basename used as a sidebar group label
// when the workspace is not a registered project. '' for empty, "/" and ".".
function workspaceFallbackName(ws) {
  if (!ws) return '';
  const trimmed = ws.replace(/\/+$/, '');
  if (!trimmed) return '';
  const base = trimmed.slice(trimmed.lastIndexOf('/') + 1);
  return (!base || base === '.') ? '' : base;
}

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

// ICONS — single source of truth for the non-SVG glyph set (#2027). Before
// this map the dashboard carried four parallel icon notations (SVG consts,
// `&#x..;` / `&#..;` HTML entities, bare Unicode glyphs like `✎ ⎘`, and raw
// `\u{..}` escapes) with the same semantic icon spelled differently at each
// call site. Centralising here means one icon → one definition → one notation.
//
// Notation rule: literal Unicode glyph characters (one form, no mixing decimal
// `&#128277;` with hex `&#x2328;`, no mixing `\u{1f464}` lowercase with
// `\u{1F916}` uppercase). Literal glyphs are the only notation that renders
// correctly in BOTH consumption contexts used here: dropped raw into innerHTML
// / template strings, and passed through esc() (which would HTML-escape an
// entity's `&` into `&amp;` and show it verbatim). SVG-backed icons
// (star/github/chevron/clawd) keep their dedicated *_SVG consts above.
const ICONS = {
  close:    '×', // dismiss / close affordance
  back:     '←', // mobile back
  navUp:    '▲', // previous user message
  navDown:  '▼', // next user message
  edit:     '✎', // rename / edit
  copy:     '⎘', // copy key
  trash:    '🗑', // delete
  attach:   '📎', // attach file
  mic:      '🎤', // voice input
  keyboard: '⌨', // keyboard input
  send:     '➤', // send message
  stop:     '■', // interrupt turn
  download: '⬇', // download
  downArrow:'↓', // file-row download (thinner, paired with ↗)
  preview:  '↗', // preview / ask-aside
  gear:     '⚙', // init / system event
  user:     '&gt;_', // user event — brand ">_" terminal prompt mark (rust mono, see .event.user .event-icon)
  spark:    '✦', // assistant text event (non-claude backends)
  todo:     '☰', // todo event
  robot:    '🤖', // subagent badge / agent count
  galleryPrev:  '‹', // lightbox previous image
  galleryNext:  '›', // lightbox next image
  zoomOut:      '−', // lightbox zoom out
  zoomIn:       '+', // lightbox zoom in
  rotateLeft:   '↺', // lightbox rotate left
  rotateRight:  '↻', // lightbox rotate right
};

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

  // Project settings gear (RFC project-access-profile §8.1). Opens the
  // right-side settings drawer for this project. Remote projects are edited on
  // their own node's dashboard, so the gear is local-only (the PUT
  // /api/projects/config remote proxy exists but the drawer's live pickers read
  // the LOCAL backend/access-profile registries).
  let gearBtn = '';
  if (node === 'local') {
    gearBtn = '<button type="button" class="sh-btn" data-action="project-settings" data-name="' + escAttr(p.name) + '" title="项目设置：' + escAttr(p.name) + '" aria-label="项目设置 ' + escAttr(p.name) + '">' + ICONS.gear + '</button>';
  }

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
    gearBtn +
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
// #1980 PR-2: the project-header buttons' scoped #session-list delegation
// (SIDEBAR_PROJECT_ACTIONS / initSidebarProjectActions) merged into the
// global nz.actions registry at the tail of this file — closest() single
// dispatch subsumes the old stopPropagation, and the capture-phase
// long-press swallow from initSwipeDelete still runs first (capture).

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
      await fetchJSON(NZ_CONTRACT.API.projects_favorite + '?' + qs, { timeoutMs: 10000, method: 'POST', headers });
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

// openProjectSettings opens the per-project settings modal (RFC
// project-access-profile §8.1). It reads GET /api/projects/config for the
// current values, renders editable fields (display name / emoji / access
// profile / backend / planner model + prompt), and writes back via PUT. The
// access-profile + backend pickers reuse the same registries the new-session
// modal consumes, so a project can be pinned to an auth chain / backend without
// hand-editing project.yaml. Local projects only (see gear-button gating).
async function openProjectSettings(name) {
  if (!name) return;
  // Load the three inputs in parallel: current config + the two registries the
  // pickers render from. Config failure is fatal (nothing to edit); registry
  // failures degrade to hidden pickers (same as new-session modal).
  let cfg;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const [c] = await Promise.all([
      fetchJSON(NZ_CONTRACT.API.projects_config + '?name=' + encodeURIComponent(name), { timeoutMs: 10000, headers, credentials: 'same-origin' }),
      fetchCLIBackends(),
      fetchAccessProfiles(),
    ]);
    cfg = c || {};
  } catch (err) {
    if (err && err.status) showAPIError('加载项目设置', err.status, '');
    else showNetworkError('加载项目设置', err);
    return;
  }

  const accessProfilePicker = renderAccessProfilePicker(accessProfiles, { selectId: 'ps-access-profile', selectedId: cfg.access_profile || '' });
  const backendPicker = renderBackendPicker(cliBackends, { selectId: 'ps-backend', selectedId: cfg.backend || '' });

  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="项目设置：' + escAttr(name) + '">' +
      '<h3>项目设置 · ' + esc(name) + '</h3>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-display-name">显示名称</label>' +
        '<input id="ps-display-name" style="' + PICKER_SELECT_STYLE + '" maxlength="200" value="' + escAttr(cfg.display_name || '') + '" placeholder="' + escAttr(name) + '">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-emoji">Emoji</label>' +
        '<input id="ps-emoji" style="' + PICKER_SELECT_STYLE + '" maxlength="16" value="' + escAttr(cfg.emoji || '') + '" placeholder="🗂">' +
      '</div>' +
      accessProfilePicker +
      '<div style="margin:-6px 0 12px"><button type="button" class="linklike" data-action="ps-new-profile" style="background:none;border:none;color:var(--nz-accent);font-size:12px;cursor:pointer;padding:0">+ 新建访问档…</button></div>' +
      backendPicker +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-planner-model">Planner model（留空则继承）</label>' +
        '<input id="ps-planner-model" style="' + PICKER_SELECT_STYLE + '" maxlength="256" value="' + escAttr(cfg.planner_model || '') + '" placeholder="' + escAttr(accessProfileDefaultModel(cfg.access_profile) || '（继承默认）') + '">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-planner-prompt">Planner prompt（可选，单行）</label>' +
        '<textarea id="ps-planner-prompt" rows="3" style="' + PICKER_SELECT_STYLE + ';resize:vertical" maxlength="8192" placeholder="附加系统提示…">' + esc(cfg.planner_prompt || '') + '</textarea>' +
      '</div>' +
      '<div id="ps-preview" style="font-size:12px;color:var(--nz-text-mute);margin-bottom:12px;padding:8px;background:var(--nz-bg-0);border-radius:4px"></div>' +
      '<div id="ps-error" style="display:none;color:var(--nz-danger,#e5484d);font-size:12px;margin-bottom:8px"></div>' +
      '<div class="modal-btns">' +
        '<button type="button" data-action="modal-close">取消</button>' +
        '<button type="button" class="primary" data-action="ps-save" data-name="' + escAttr(name) + '">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);

  // Live "effective link" preview: shows how the next session under this
  // project will resolve (profile → resolved model). Non-sensitive only — no
  // base-URL/token, just the profile label + model (§8.1 联动预览 / §8.4).
  const updatePreview = () => {
    const apEl = document.getElementById('ps-access-profile');
    const apID = apEl ? apEl.value : (cfg.access_profile || '');
    const pmEl = document.getElementById('ps-planner-model');
    const model = (pmEl && pmEl.value.trim()) || accessProfileDefaultModel(apID) || '（继承默认）';
    const info = accessProfileChipInfo(apID);
    const label = info ? info.label : '全局默认';
    const box = document.getElementById('ps-preview');
    if (box) box.textContent = '生效链路：' + label + ' → ' + model;
  };
  const apSel = document.getElementById('ps-access-profile');
  if (apSel) apSel.addEventListener('change', updatePreview);
  const pmInput = document.getElementById('ps-planner-model');
  if (pmInput) pmInput.addEventListener('input', updatePreview);
  updatePreview();

  const saveBtn = overlay.querySelector('[data-action="ps-save"]');
  if (saveBtn) saveBtn.addEventListener('click', () => saveProjectSettings(name, cfg, overlay));

  // "+ 新建访问档" opens the create form; on success it refreshes the registry
  // and re-selects the new profile in this drawer's picker.
  const newBtn = overlay.querySelector('[data-action="ps-new-profile"]');
  if (newBtn) newBtn.addEventListener('click', () => {
    openCreateAccessProfile((newID) => {
      const sel = document.getElementById('ps-access-profile');
      if (sel) {
        // The picker was rendered before the new profile existed; add + select
        // it so the drawer immediately reflects the creation without a reopen.
        if (![...sel.options].some(o => o.value === newID)) {
          const opt = document.createElement('option');
          opt.value = newID;
          opt.textContent = accessProfileChipInfo(newID)?.label || newID;
          sel.appendChild(opt);
        }
        sel.value = newID;
        updatePreview();
      }
    });
  });
}

// ACCESS_PROFILE_TEMPLATES pre-fill the create form for the two common cases so
// the operator doesn't have to know env-var names (RFC P1-d). Values are the
// literal overlay keys the server accepts; the token goes to a *_FILE.
const ACCESS_PROFILE_TEMPLATES = {
  '1p': {
    label: '个人 Anthropic（1P 直连）',
    display_name: '个人 Anthropic',
    // Reference a design token rather than an inline hex literal (RNEW-UX-015
    // ratchet). The operator can edit the colour later by hand-editing config.
    chip_color: 'var(--nz-accent)',
    env: { CLAUDE_CODE_USE_BEDROCK: '0', ANTHROPIC_BASE_URL: 'https://api.anthropic.com' },
    token_env_key: 'ANTHROPIC_AUTH_TOKEN_FILE',
    token_hint: '粘贴 1P Anthropic 的 auth token（写入 0600 文件，不进 config）',
    default_model: 'claude-fable-5',
  },
  'bedrock': {
    label: '公司 Bedrock（经本机 proxy）',
    display_name: '公司 Bedrock',
    chip_color: 'var(--nz-purple)',
    env: { CLAUDE_CODE_USE_BEDROCK: '1', CLAUDE_CODE_SKIP_BEDROCK_AUTH: '1', ANTHROPIC_BEDROCK_BASE_URL: 'http://127.0.0.1:8889', AWS_REGION: 'us-west-2' },
    token_env_key: '',
    token_hint: '',
    default_model: 'claude-opus-4-8',
  },
};

// openCreateAccessProfile renders the guided create-profile form (RFC P1-d).
// A template pre-fills env keys + colour so the operator only supplies an id,
// a display name, and (for 1P) a token. On submit it POSTs /api/access-profiles;
// on success it refreshes the cached registry and calls onCreated(id). The
// token textarea value is sent once and never read back (write-only secret).
function openCreateAccessProfile(onCreated) {
  const tplOptions = Object.keys(ACCESS_PROFILE_TEMPLATES)
    .map(k => '<option value="' + escAttr(k) + '">' + esc(ACCESS_PROFILE_TEMPLATES[k].label) + '</option>').join('');
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="新建访问档">' +
      '<h3>新建访问档</h3>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-template">模板</label>' +
        '<span class="picker-select-wrap"><select id="cap-template" style="' + PICKER_SELECT_ONLY_STYLE + '">' + tplOptions + '</select></span>' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-id">档 ID（英数字 . _ -，唯一）</label>' +
        '<input id="cap-id" style="' + PICKER_SELECT_STYLE + '" maxlength="64" placeholder="1p-fable">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-display">显示名称</label>' +
        '<input id="cap-display" style="' + PICKER_SELECT_STYLE + '" maxlength="200">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-model">默认 model（可选）</label>' +
        '<input id="cap-model" style="' + PICKER_SELECT_STYLE + '" maxlength="256">' +
      '</div>' +
      '<div id="cap-token-wrap" style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-token">Token（写入 0600 文件，仅此一次可见）</label>' +
        '<textarea id="cap-token" rows="2" style="' + PICKER_SELECT_STYLE + ';resize:vertical" placeholder="" autocomplete="off"></textarea>' +
        '<div id="cap-token-hint" style="font-size:11px;color:var(--nz-text-mute);margin-top:4px"></div>' +
      '</div>' +
      '<div id="cap-error" style="display:none;color:var(--nz-danger,#e5484d);font-size:12px;margin-bottom:8px"></div>' +
      '<div class="modal-btns">' +
        '<button type="button" data-action="modal-close">取消</button>' +
        '<button type="button" class="primary" data-action="cap-create">创建</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);

  const tplSel = overlay.querySelector('#cap-template');
  const applyTemplate = () => {
    const tpl = ACCESS_PROFILE_TEMPLATES[tplSel.value];
    if (!tpl) return;
    const dEl = overlay.querySelector('#cap-display');
    if (dEl && !dEl.value) dEl.value = tpl.display_name || '';
    const mEl = overlay.querySelector('#cap-model');
    if (mEl && !mEl.value) mEl.value = tpl.default_model || '';
    // Token field only relevant when the template references a *_FILE key.
    const wrap = overlay.querySelector('#cap-token-wrap');
    const hint = overlay.querySelector('#cap-token-hint');
    if (wrap) wrap.style.display = tpl.token_env_key ? '' : 'none';
    if (hint) hint.textContent = tpl.token_hint || '';
  };
  tplSel.addEventListener('change', applyTemplate);
  applyTemplate();


  overlay.querySelector('[data-action="cap-create"]').addEventListener('click', async () => {
    const tpl = ACCESS_PROFILE_TEMPLATES[tplSel.value] || {};
    const id = (overlay.querySelector('#cap-id').value || '').trim();
    const errBox = overlay.querySelector('#cap-error');
    const showErr = (m) => { if (errBox) { errBox.textContent = m; errBox.style.display = 'block'; } };
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(id)) {
      showErr('档 ID 无效（英数字 . _ -，1-64 位，不能以 - 开头）');
      return;
    }
    const body = {
      id: id,
      display_name: (overlay.querySelector('#cap-display').value || '').trim(),
      chip_color: tpl.chip_color || '',
      default_model: (overlay.querySelector('#cap-model').value || '').trim(),
      env: Object.assign({}, tpl.env || {}),
    };
    if (tpl.token_env_key) {
      const tok = (overlay.querySelector('#cap-token').value || '').trim();
      if (!tok) { showErr('该模板需要 token'); return; }
      body.token_env_key = tpl.token_env_key;
      body.token_content = tok;
    }
    try {
      const headers = { 'Content-Type': 'application/json' };
      const t = getToken();
      if (t) headers['Authorization'] = 'Bearer ' + t;
      await fetchJSON(NZ_CONTRACT.API.access_profiles, {
        timeoutMs: 10000, method: 'POST', headers, credentials: 'same-origin',
        body: JSON.stringify(body),
      });
    } catch (err) {
      if (err && err.status === 409) showErr('该档 ID 已存在');
      else if (err && err.status === 400) showErr('配置无效：请检查各字段');
      else if (err && err.status) showAPIError('创建访问档', err.status, '');
      else showNetworkError('创建访问档', err);
      return;
    }
    overlay.remove();
    showToast('访问档已创建 · ' + id, 'success');
    // Force a registry refresh (bypass the 60s cache) so the new profile is
    // visible immediately to pickers/chips.
    accessProfilesFetchedAt = 0;
    await fetchAccessProfiles();
    if (typeof onCreated === 'function') onCreated(id);
  });
}

// accessProfileDefaultModel resolves a profile id to its default_model from the
// cached registry, or "" when unknown / global default. Used only for the
// planner-model placeholder + preview — never a value the form submits.
function accessProfileDefaultModel(profileID) {
  if (!profileID || !accessProfiles || !Array.isArray(accessProfiles.profiles)) return '';
  const e = accessProfiles.profiles.find(p => p && p.id === profileID);
  return (e && e.default_model) ? e.default_model : '';
}

// saveProjectSettings collects the settings-modal fields, merges them onto the
// loaded config (preserving fields the drawer doesn't edit — chat_bindings,
// git_sync, created_at, …), and PUTs. On success it refreshes the sidebar +
// the access-profile chip source; on validation failure it shows the server's
// generic reason inline.
async function saveProjectSettings(name, baseCfg, overlay) {
  const val = (id) => { const el = document.getElementById(id); return el ? el.value : undefined; };
  // Start from the loaded config so unedited fields (chat_bindings, git_sync,
  // memory_file, created_at) round-trip untouched.
  const cfg = Object.assign({}, baseCfg);
  cfg.display_name = (val('ps-display-name') || '').trim();
  cfg.emoji = (val('ps-emoji') || '').trim();
  cfg.access_profile = val('ps-access-profile') || '';
  cfg.backend = val('ps-backend') || '';
  cfg.planner_model = (val('ps-planner-model') || '').trim();
  cfg.planner_prompt = (val('ps-planner-prompt') || '').trim();

  const errBox = overlay.querySelector('#ps-error');
  const showErr = (msg) => { if (errBox) { errBox.textContent = msg; errBox.style.display = 'block'; } };

  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON(NZ_CONTRACT.API.projects_config + '?name=' + encodeURIComponent(name), {
      timeoutMs: 10000, method: 'PUT', headers, credentials: 'same-origin',
      body: JSON.stringify(cfg),
    });
  } catch (err) {
    if (err && err.status === 400) showErr('配置无效：请检查各字段（未知 backend / 访问档、超长 prompt 等）');
    else if (err && err.status) showAPIError('保存项目设置', err.status, '');
    else showNetworkError('保存项目设置', err);
    return;
  }
  overlay.remove();
  showToast('项目设置已保存 · ' + name, 'success');
  // Access-profile binding change affects the next session's chip; refresh both
  // the profile registry and the sidebar so chips repaint.
  fetchSessions();
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
      '<span class="hp-subtitle">侧栏为当前活跃会话，此处为全部历史会话</span>' +
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
    // the query is empty keep the compact "(47)" form. Decide on the same
    // trimmed query filterHistoryEntries matches on (#2431: whitespace-only
    // input is not a filter, so no "(N / N)").
    countEl.textContent = (query || '').trim()
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
  // Group by the SAME key the list is sorted on (retired_at || last_active,
  // see the merged.sort above). Grouping by last_active alone while sorting
  // by retired_at interleaved days and repeated day headers.
  let currentDay = '';
  itemsEl.innerHTML = filtered.map(s => {
    let dayHeader = '';
    const groupTs = s.retired_at || s.last_active;
    if (groupTs) {
      const d = new Date(groupTs);
      const dayStr = historyDayLabel(d);
      if (dayStr !== currentDay) {
        currentDay = dayStr;
        dayHeader = '<div class="hp-day-header">' + esc(dayStr) + '</div>';
      }
    }
    const ago = s.last_active ? timeAgo(s.last_active) : '';
    const abs = s.last_active ? formatAbsTime(s.last_active) : '';
    return dayHeader +
      '<div class="history-popover-item" data-sid="' + escAttr(s.session_id) + '" data-action="history-resume">' +
      (s.prompt ? '<div class="hp-prompt" title="' + escAttr(s.prompt) + '">' + esc(s.prompt) + '</div>' : '<div class="hp-prompt" style="color:var(--nz-text-dim)">未命名</div>') +
      '<div class="hp-meta">' +
        (s.project ? '<span class="hp-project">' + esc(s.project) + '</span><span class="hp-dot">&middot;</span>' : '') +
        (ago ? '<span' + (abs ? ' title="' + escAttr(abs) + '"' : '') + '>' + ago + '</span>' : '') +
      '</div>' +
      '</div>';
  }).join('');
}

function sessionTypeTag(cliName, entrypoint) {
  var label;
  if (cliName === 'kiro') { label = 'Kiro CLI'; }
  else if (cliName === 'codex') { label = 'Codex CLI'; }
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
  const filePickBtn = document.querySelector('button[data-action="file-picker"]');
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

function cliIcon(name) {
  // Kiro official ghost-style mark (sourced from https://kiro.dev/icon.svg).
  // Inlined here so the asset works offline + survives CSP. Compressed to
  // the essential shapes: rounded purple bg + white ghost body + 2 black
  // eyes. The original 1200×1200 is recoordinatized for the 16×16 viewbox
  // sidebar / header sc-cli-icon slot. UI Round 5 R5-1.
  if (name === 'kiro') return '<svg class="sc-cli-icon" viewBox="0 0 1200 1200" fill="none" xmlns="http://www.w3.org/2000/svg"><rect width="1200" height="1200" rx="260" fill="#9046FF"/><path d="M398.554 818.914C316.315 1001.03 491.477 1046.74 620.672 940.156C658.687 1059.66 801.052 970.473 852.234 877.795C964.787 673.567 919.318 465.357 907.64 422.374C827.637 129.443 427.623 128.946 358.8 423.865C342.651 475.544 342.402 534.18 333.458 595.051C328.986 625.86 325.507 645.488 313.83 677.785C306.873 696.424 297.68 712.819 282.773 740.645C259.915 783.881 269.604 867.113 387.87 823.883L399.051 818.914H398.554Z" fill="white"/><ellipse cx="636" cy="487" rx="40" ry="63" fill="black"/><ellipse cx="771" cy="487" rx="40" ry="63" fill="black"/></svg>';
  // codex: official OpenAI logomark (Simple Icons, MIT/brand path, 24×24).
  // Inlined for offline + CSP. Single path filled with the OpenAI brand green
  // (#10a37f, matches profile_codex.go ChipColor) so the codex session is
  // recognizable at a glance in the sidebar / header sc-cli-icon slot.
  if (name === 'codex') return '<svg class="sc-cli-icon" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="#10a37f" d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/></svg>';
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
  // Per-card node badge: the sidebar now lists every connected node's
  // sessions together (the node picker moved into the New Session modal), so
  // each non-local card is tagged with its connection to disambiguate. Local
  // sessions stay unmarked — they're the default and the common case. The
  // hue is derived from the node id (nodeColor) so it matches the palette's
  // .cp-node badge and the modal node picker.
  const nodeBadge = (isMultiNode() && sNode !== 'local')
    ? '<span class="sc-node" style="background:' + nodeColor(sNode) + '" title="' + escAttr(getNodeDisplayName(sNode)) + '">' + esc(getNodeDisplayName(sNode)) + '</span>'
    : '';

  const dismissBtn = '<button type="button" class="btn-close btn-dismiss" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" data-action="session-dismiss" title="移除" aria-label="移除会话">' + ICONS.close + '</button>';

  const typeTag = s.source === 'terminal' ? sessionTypeTag(s.cli_name, s.entrypoint) : '';
  const agentCount = s.subagents ? s.subagents.length : 0;
  const agentBadge = agentCount > 0 ? '<span class="sc-agents">' + ICONS.robot + '\u00D7' + agentCount + '</span>' : '';
  // R110-P3 IM origin: show a small chip for sessions sourced from feishu /
  // slack / discord / weixin so operators can eyeball which cards are real
  // IM threads vs dashboard-local conversations. originBadgeHtml returns ''
  // for non-IM prefixes so the meta line stays clean for those.
  const originBadge = originBadgeHtml(s.key);
  // UI Round 5 R5-2: backend chip removed from session cards. The cli icon
  // (cliIcon, kiro-ghost vs claude-logomark) already disambiguates backend
  // visually, so the chip was redundant. backendChipHtml() helper kept for
  // doctor panel where listing backends needs an explicit text label.
  //
  // Access-profile chip IS shown (RFC project-access-profile §8.3): unlike
  // backend it has no icon, and it carries a billing/account dimension an
  // operator must be able to eyeball ("is this card on the personal 1P or the
  // company Bedrock account?"). Empty for single-auth mode / global default so
  // deployments that don't use profiles see no change.
  const accessProfileChip = accessProfileChipHtml(s.access_profile);
  // ui-polish-light-theme D4: the cli icon rides inside the meta line at
  // 18px instead of a dedicated 36px column — at 36px every card led with
  // a saturated brand mark that outweighed its own title, while the icon
  // only carries one low-entropy bit (which backend). Title owns line 1.
  const metaHtml = icon +
    '<span class="sc-dot ' + dotCls + '"></span>' +
    '<span>' + esc(displayState) + '</span>' +
    nodeBadge +
    originBadge +
    accessProfileChip +
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

  return '<div class="' + cls + '" role="listitem" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" tabindex="0" aria-label="' + escAttr(prompt + ' · ' + displayState) + '" data-action="session-select" data-action-keydown="session-card-key">' +
    dismissBtn +
    '<div class="sc-body">' +
      '<div class="sc-header">' +
        '<div class="sc-prompt" title="' + escAttr(prompt) + '">' + esc(prompt) + '</div>' +
        unreadBadge +
        (ago ? '<span class="sc-time"' + (absTime ? ' title="' + escAttr(absTime) + '"' : '') + ' data-ts="' + s.last_active + '">' + ago + '</span>' : '') +
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
    const r = await fetch(NZ_CONTRACT.API.sessions_resume, {
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
      entries = await fetchJSON(NZ_CONTRACT.API.discovered_preview + '?session_id=' + encodeURIComponent(sessionId) + cwdParam, { headers, timeoutMs: 5000 });
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
// is reconcileSelectedNode(), which setState already invokes on every WS
// state change via updateStatusBar(). The 1s tick therefore had no
// user-visible effect and was a periodic no-op repaint. Issue #434.

function updateStatusBar() {
  const container = document.getElementById('sidebar-status');
  // #sidebar-status 节点已在"底部让位给 session 列表"的迭代中删除。没节点就
  // 早退，但仍要 reconcileSelectedNode()——选中的远程节点若已下线，需把
  // selectedNode 拨回 local，否则 dispatch / 头部会指向一个消失的连接。
  if (!container) { reconcileSelectedNode(); return; }
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
    ? '<button type="button" class="status-reconnect" data-action="ws-reconnect" title="立即重连" aria-label="立即重连">重连</button>'
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
  // A remote flipping offline must also pull selectedNode back to local if it
  // was pointing at that now-gone node, without waiting for the next poll.
  reconcileSelectedNode();
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
        '<button type="button" class="primary" data-action="cheatsheet-dismiss">好的</button>' +
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
  if (nzViews.agent) {
    nzViews.agent.onSessionSwitch(key, node);
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
  if (isDiscoveredKey(key)) {
    const d = findDiscovered(parseDiscoveredPid(key), node);
    if (d) {
      // #2431: same as the managed path below — leave assets/cron/settings
      // first, or the preview panel is written into a hidden #main while the
      // previous session has already been unsubscribed.
      if (activeView !== 'chat') setActivityView('chat');
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
  _lastAppliedMainState = null; // #2431: new session → first poll must reconcile
  // Opening a card counts as "reading" it — clear the chat-style unread chip
  // before the DOM toggle below so the next render reflects a zeroed state.
  const selSid = sid(key, node);
  if (sessionUnread[selSid]) {
    delete sessionUnread[selSid];
  }
  // Opening a session on another node retargets dispatch (selectedNode drives
  // the dispatch node + main header). The sidebar no longer filters by node,
  // so there is no list to re-render — just persist the new target.
  if (prevNode !== selectedNode) {
    try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  }
  lastEventTime = 0;
  lastRenderedEventTime = 0;
  oldestFetchedEventTime = 0;
  _autoPageBackCount = 0; // reset the blank-page recovery budget per session
  // Invalidate any in-flight "load earlier" page of the previous session and
  // free the flag so the new session can page back immediately.
  _earlierGen++;
  _earlierLoading = false;
  mobileEnterChat();
  stopPreviewPolling();
  const activeCard = setActiveSessionCard(key, node);
  if (activeCard) updateCardUnreadChip(activeCard, 0);
  renderMainShell();
  fetchSessionRuns(key, node); // populate the run-history timeline (best-effort)
  fetchGitState(key, node); // populate the branch / worktree chip (best-effort)
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

// ===== Session tuning popover =====
// Per-session model/effort switching from the header chips.
// docs/rfc/dashboard-model-effort-control.md §4.1. Control lives where the
// state is displayed: clicking the model label / effort tag opens a picker;
// the choice POSTs /api/sessions/override and the server decides the apply
// path (rpc / respawn / deferred — F9 split is server-side, the frontend
// only renders the returned applied_via).

const TUNING_EFFORT_TIERS = ['low', 'medium', 'high', 'xhigh', 'max'];
let tuningPopoverCloseHandler = null;

// #1980 PR-2: the header tuning chips' document-level listener merged into
// the global nz.actions registry (keys tuning-model / tuning-effort) — the
// chips are rebuilt on repaint, so delegation stays the right shape.

function dismissTuningPopover() {
  const el = document.getElementById('tuning-popover');
  if (el) el.remove();
  if (tuningPopoverCloseHandler) {
    document.removeEventListener('click', tuningPopoverCloseHandler);
    tuningPopoverCloseHandler = null;
  }
}

// tuningToast is a minimal self-dismissed notice — the dashboard has no
// global toast helper, and the F7 rejection text (CLI-supplied, sanitized
// server-side) needs a visible surface that outlives the popover.
function tuningToast(msg, isError) {
  let t = document.getElementById('tuning-toast');
  if (t) t.remove();
  t = document.createElement('div');
  t.id = 'tuning-toast';
  t.style.cssText = 'position:fixed;top:16px;left:50%;transform:translateX(-50%);' +
    'max-width:70%;padding:10px 16px;border-radius:10px;z-index:var(--nz-z-toast);font-size:13px;' +
    'background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);' +
    'border:1px solid ' + (isError ? 'var(--nz-danger, #d33)' : 'var(--nz-border)') + ';' +
    'color:var(--nz-text)';
  t.textContent = msg; // textContent — CLI-origin text must never hit innerHTML
  document.body.appendChild(t);
  setTimeout(() => t.remove(), isError ? 8000 : 4000);
}

// tuningModelsForSession resolves the popover's model choices from the
// cached /api/cli/backends payload (BackendInfo.models: agent-reported for
// kiro, cli.backends[].models fallback for claude). Empty list → the
// popover shows its manual-input row only.
function tuningModelsForSession(s) {
  const backendID = (s && s.backend) || sessionBackends[selectedKey] ||
    (cliBackends && cliBackends.default) || '';
  if (!cliBackends || !Array.isArray(cliBackends.backends)) return { models: [], backendID };
  const entry = cliBackends.backends.find(b => b && b.id === backendID) ||
    cliBackends.backends.find(b => b && b.id === (cliBackends.default || ''));
  return {
    models: (entry && Array.isArray(entry.models)) ? entry.models : [],
    backendID: entry ? entry.id : backendID,
    // BackendInfo.protocol ("acp" | "stream-json") decides the empty-manifest
    // hint: only ACP backends ever report a list after their first session.
    protocol: entry ? (entry.protocol || '') : '',
  };
}

function openTuningPopover(kind) {
  dismissTuningPopover();
  if (!selectedKey) return;
  // NG4: override API is local-only in this slice; remote sessions get an
  // explanation instead of a dead control (mirrors git chip's local-only).
  if ((selectedNode || 'local') !== 'local') {
    tuningToast('远程节点会话暂不支持切换模型/档位', false);
    return;
  }
  const s = sessionsData[sid(selectedKey, selectedNode)] ||
    // Not spawned yet: show the parked pick as current so a re-open marks it.
    (sessionPendingTuning[selectedKey] || {});
  const running = s.state === 'running';
  const rows = [];
  const current = kind === 'model' ? (s.model || '') : (s.effort || '');

  if (kind === 'model') {
    const { models, protocol } = tuningModelsForSession(s);
    for (const m of models) {
      const active = m.id === current || (current && current.indexOf(m.id) !== -1);
      rows.push({ value: m.id, label: m.id, desc: m.description || '', active });
    }
    if (models.length === 0) {
      // ACP backends (kiro) report their manifest on the first session;
      // stream-json backends (claude) never do — telling a claude operator to
      // wait would be a lie, so point at the config knob instead.
      rows.push({ header: true, label: protocol === 'acp'
        ? '清单在该 backend 首次会话后可用；可手动输入：'
        : '该 backend 不上报模型清单；可在 config.yaml 的 cli.backends[].models 配置候选，或手动输入：' });
    }
    rows.push({ input: true });
    rows.push({ value: '', label: '恢复默认（配置链）', reset: true });
  } else {
    for (const tier of TUNING_EFFORT_TIERS) {
      rows.push({ value: tier, label: tier, active: tier === current });
    }
    rows.push({ value: '', label: '恢复默认（配置链）', reset: true });
  }

  const anchor = document.getElementById(kind === 'model' ? 'header-model' : 'header-effort');
  if (!anchor) return;
  const pop = document.createElement('div');
  pop.id = 'tuning-popover';
  pop.style.cssText = 'position:fixed;min-width:220px;max-width:320px;max-height:340px;' +
    'overflow-y:auto;background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);' +
    'border:1px solid var(--nz-border);border-radius:10px;padding:6px 0;z-index:120;' +
    'font-size:13px;scrollbar-width:thin';
  const rect = anchor.getBoundingClientRect();
  pop.style.left = Math.max(8, Math.min(rect.left, window.innerWidth - 340)) + 'px';
  pop.style.top = (rect.bottom + 6) + 'px';

  const hint = kind === 'effort'
    ? 'ⓘ 将重启 CLI 进程并恢复上下文' + (running ? '（会中断当前回合）' : '')
    : 'ⓘ 生效时机以返回的应用路径为准';
  let html = '<div style="padding:6px 12px;color:var(--nz-text-mute);font-size:12px;border-bottom:1px solid var(--nz-bg-2)">' +
    (kind === 'model' ? '切换模型' : '切换 effort 档位') + '</div>';
  for (const rowSpec of rows) {
    if (rowSpec.header) {
      html += '<div style="padding:6px 12px;color:var(--nz-text-faint);font-size:12px">' + esc(rowSpec.label) + '</div>';
      continue;
    }
    if (rowSpec.input) {
      html += '<div style="padding:6px 12px"><input id="tuning-manual-input" type="text" placeholder="model id…" ' +
        'style="width:100%;box-sizing:border-box;background:var(--nz-bg-2);border:1px solid var(--nz-border);' +
        'border-radius:6px;padding:5px 8px;color:var(--nz-text);font-size:12px"></div>';
      continue;
    }
    const mark = rowSpec.active ? '● ' : (rowSpec.reset ? '↺ ' : '○ ');
    html += '<div class="tuning-opt" data-value="' + escAttr(rowSpec.value) + '"' +
      ' style="padding:7px 12px;cursor:pointer;color:var(--nz-text);' +
      (rowSpec.active ? 'font-weight:600;color:var(--nz-accent);' : '') +
      (rowSpec.reset ? 'border-top:1px solid var(--nz-bg-2);color:var(--nz-text-mute);' : '') + '"' +
      (rowSpec.desc ? ' title="' + escAttr(rowSpec.desc) + '"' : '') + '>' +
      mark + esc(rowSpec.label) + '</div>';
  }
  html += '<div style="padding:6px 12px;color:var(--nz-text-faint);font-size:11px;border-top:1px solid var(--nz-bg-2)">' + esc(hint) + '</div>';
  pop.innerHTML = html;
  document.body.appendChild(pop);

  pop.querySelectorAll('.tuning-opt').forEach(item => {
    item.onmouseenter = () => item.style.background = 'var(--nz-hover-bg)';
    item.onmouseleave = () => item.style.background = '';
    item.addEventListener('click', () => {
      const v = item.dataset.value;
      dismissTuningPopover();
      // Respawn-family switches interrupt a running turn — confirm first
      // (§4.1 运行中防护; the server decides the actual path, we only warn
      // for the case that ALWAYS respawns: effort changes).
      if (kind === 'effort' && running &&
          !confirm('会话正在运行：切换档位将中断当前回合并重启 CLI 进程（上下文保留）。继续？')) {
        return;
      }
      postTuningOverride(kind, v);
    });
  });
  const manual = pop.querySelector('#tuning-manual-input');
  if (manual) {
    manual.addEventListener('click', (e) => e.stopPropagation());
    manual.onkeydown = (e) => {
      if (e.key === 'Enter') {
        const v = manual.value.trim();
        dismissTuningPopover();
        if (v) postTuningOverride('model', v);
      }
    };
  }
  setTimeout(() => {
    tuningPopoverCloseHandler = (e) => {
      if (!pop.contains(e.target)) dismissTuningPopover();
    };
    document.addEventListener('click', tuningPopoverCloseHandler);
  }, 0);
}

// postTuningOverride sends the switch and renders the outcome. Pending
// visual: the source chip dims until the next sessions poll repaints it
// with the server-confirmed value (no optimistic promotion — §4.1 三态;
// a rollback is visible because the poll simply keeps the old value).
async function postTuningOverride(kind, value) {
  const key = selectedKey;
  const chip = document.getElementById(kind === 'model' ? 'header-model' : 'header-effort');
  if (chip) chip.style.opacity = '0.45';
  const restore = () => { if (chip) chip.style.opacity = ''; };
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const body = { key };
    body[kind] = value;
    const resp = await fetch(NZ_CONTRACT.API.sessions_override, {
      method: 'POST', headers, body: JSON.stringify(body),
    });
    if (!resp.ok) {
      const text = (await resp.text()).trim();
      restore();
      // 409 = CLI rejection (F7/F15): surface the CLI's own text verbatim.
      tuningToast(resp.status === 409 ? ('切换被 CLI 拒绝：' + text) : ('切换失败：' + text), true);
      return;
    }
    const data = await resp.json();
    const via = data.applied_via || '';
    const label = kind === 'model' ? '模型' : '档位';
    // No server row for this key = the session has not spawned yet; the pick
    // was parked server-side. Mirror it so the chips show it until promotion.
    const isPending = !sessionsData[sid(key, selectedNode)];
    if (isPending) {
      const prev = sessionPendingTuning[key] || {};
      const next = Object.assign({}, prev);
      next[kind] = value;
      sessionPendingTuning[key] = next;
      tuningToast(label + (value ? '已记录，发送首条消息时生效' : '已恢复默认'), false);
    } else if (via === 'rpc') {
      tuningToast(label + '已切换（对下一轮生效）', false);
    } else if (via === 'respawn') {
      tuningToast(label + '已记录，CLI 进程将重启并恢复上下文（下条消息生效）', false);
    } else {
      tuningToast(label + '已记录，将于下次会话进程启动时生效', false);
    }
    // Pull fresh state now rather than waiting out the poll interval; the
    // repaint clears the pending dim with the server-confirmed value.
    setTimeout(() => { restore(); fetchSessions(); }, 800);
  } catch (e) {
    restore();
    tuningToast('切换请求失败：网络错误', true);
  }
}

// repaintGitChip re-renders the chip for the currently selected session from
// cache. Called at the end of renderMainShell so a header rebuild triggered by
// something unrelated (rename, model update) doesn't drop the chip.
function repaintGitChip() {
  if (!selectedKey) { setHeaderGitChip(''); return; }
  setHeaderGitChip(gitChipHtml(gitStateCache[sid(selectedKey, selectedNode)]));
}

async function fetchGitState(key, node) {
  node = node || 'local';
  // Git state is a local-node concern: a remote session's workspace lives on
  // that node's filesystem, so resolving it here would describe the wrong
  // tree. Clear the chip so a remote session doesn't inherit the previously
  // selected local session's branch.
  if (!key || node !== 'local') { setHeaderGitChip(''); return; }
  const cacheKey = sid(key, node);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const resp = await fetch(NZ_CONTRACT.API.sessions_git + '?key=' + encodeURIComponent(key), { headers });
    // The cache entry is per-session so dropping it is always right; the
    // header chip is only cleared when this session is still the selected one.
    if (!resp.ok) { delete gitStateCache[cacheKey]; if (selectedKey !== key || selectedNode !== node) return; setHeaderGitChip(''); return; }
    const data = await resp.json();
    gitStateCache[cacheKey] = data;
    // Guard against a stale response landing after the user switched sessions.
    if (selectedKey !== key || selectedNode !== node) return;
    setHeaderGitChip(gitChipHtml(data));
  } catch (_) {
    delete gitStateCache[cacheKey];
    if (selectedKey !== key || selectedNode !== node) return;
    setHeaderGitChip('');
  }
}

// invalidateGitState drops the cached payload for a session and re-resolves it.
// Called after /cd (the session's workspace moved, so the branch may differ)
// and on session dismissal so a recycled key cannot inherit a stale branch.
function invalidateGitState(key, node) {
  if (!key) return;
  delete gitStateCache[sid(key, node || 'local')];
  if (key === selectedKey) fetchGitState(key, node || 'local');
}

// removeSidebarCard drops a session card from the DOM without waiting for
// the next renderSidebar. It MUST also reset _lastSidebarHtml: renderSidebar
// skips `list.innerHTML = html` when the rebuilt string equals the cache, so
// a DOM-only removal would leave the cache describing a card that is no
// longer mounted and the next (identical) render would never bring it back
// — e.g. after a failed DELETE whose .finally re-fetches the list.
function removeSidebarCard(key) {
  // Escape like setActiveSessionCard: discovered keys embed the node name, so
  // a `"` or `\` would otherwise make querySelector throw mid-takeover/dismiss.
  const card = document.querySelector('.session-card[data-key="' + (window.CSS && CSS.escape ? CSS.escape(key) : key) + '"]');
  if (card) card.remove();
  _lastSidebarHtml = null;
}

// dismissSession removes a session from the sidebar. The × button deletes
// immediately with no confirmation — per operator preference, the friction
// isn't worth it. Accidental deletes are recoverable by re-entering the
// prompt (pending) or reopening the CLI (remote/discovered).
async function dismissSession(key, node, opts) {
  node = node || 'local';
  delete sessionDrafts[key];
  delete sessionScrollPos[sid(key, node)];
  // Drop the cached git state so a later key reuse can't inherit this
  // session's branch chip before its own fetch resolves.
  delete gitStateCache[sid(key, node)];
  // sessionBackends is normally consumed on first sendMessage. A dismiss
  // before any send leaves the entry behind; clear it defensively so a
  // subsequent re-create with the same key (unlikely but possible if the
  // ms timestamp collides on rapid double-create) doesn't inherit a
  // stale backend pick.
  delete sessionBackends[key];
  delete sessionAccessProfiles[key];

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
    removeSidebarCard(key);
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
  if (isDiscoveredKey(key)) {
    const d = findDiscovered(parseDiscoveredPid(key), node);
    if (!d) { showToast('未找到该外部会话', 'warning'); return; }
    try {
      const headers = {'Content-Type': 'application/json'};
      const token = getToken();
      if (token) headers['Authorization'] = 'Bearer ' + token;
      try {
        await fetchJSON(NZ_CONTRACT.API.discovered_close, {
          timeoutMs: 10000,
          method: 'POST', headers,
          body: JSON.stringify({pid: d.pid, session_id: d.session_id || '', cwd: d.cwd || '', proc_start_time: d.proc_start_time || 0, node: node || ''})
        });
      } catch (err) {
        if (err && err.status) showAPIError('关闭外部会话', err.status, err.message || '');
        else showNetworkError('关闭外部会话', err);
        return;
      }
      dropDiscovered(d.pid, d.node);
      if (pendingDiscovered && sameDiscovered(pendingDiscovered, d.pid, d.node)) {
        pendingDiscovered = null;
        stopPreviewPolling();
        document.getElementById('main').innerHTML = mainEmptyHtml();
        wireQuickAskInput();
      }
      removeSidebarCard(key);
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
  removeSidebarCard(key);

  const headers = {'Content-Type': 'application/json'};
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const body = {key: key};
  if (node && node !== 'local') body.node = node;
  // Fire-and-forget: do NOT await — the UI is already updated. On failure we
  // re-sync from the server so a genuinely-undeleted session reappears.
  fetchJSON(NZ_CONTRACT.API.sessions, {timeoutMs: 10000, method: 'DELETE', headers, body: JSON.stringify(body)})
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
    await fetchJSON(NZ_CONTRACT.API.sessions_label, {
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
  // Header-only repaint: a full renderMainShell would rebuild #events-scroll
  // empty with nothing refetching the conversation (see renderMainHeader).
  renderMainHeader();
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

// Export pager bounds (#2430). A bare `/api/sessions/events?key=` only returns
// the in-memory ring (server default 500), so a long session's export silently
// dropped its early history while the toast claimed "已导出 N 条". The pager
// walks backward with the same `before=` cursor loadEarlierEvents uses (which
// falls through to the on-disk JSONL when the ring is exhausted).
// EXPORT_PAGE_LIMIT mirrors the server's maxEventsPageLimit; EXPORT_MAX_PAGES
// is the hard stop (20k events) so a runaway session can't hang the tab.
const EXPORT_PAGE_LIMIT = 500;
const EXPORT_MAX_PAGES = 40;
let _exportInFlight = false;

// exportEventKey identifies an entry across overlapping pages: the backend's
// uuid when present, else (time,type,detail) for pre-uuid synthetic entries.
function exportEventKey(e) {
  if (e && e.uuid) return 'u:' + e.uuid;
  return 'k:' + ((e && e.time) || 0) + '|' + ((e && e.type) || '') + '|' + ((e && e.detail) || '');
}

// fetchAllSessionEvents returns { events, truncated } (or { status } on a
// non-2xx first page). `truncated` is set whenever the export is known or
// suspected to be incomplete — page cap hit, a later page failed or was
// malformed, a full page yielded nothing new, or a remote node (whose relay
// ignores before/limit and so can only ever serve the ring) returned a
// ring-sized slice — so the caller must warn rather than claim a full export.
//
// Cursor: `before = oldest + 1`, NOT `before = oldest`. Both the ring
// (EntriesBefore) and the disk sources filter strictly `Time < before`, so a
// same-millisecond sibling group (one CLI frame's blocks) split by the ring
// edge or a 500-entry page edge would lose its older members for good under a
// strict cursor. Re-admitting the watermark millisecond and dropping what we
// already hold by exportEventKey keeps every sibling; progress is measured by
// "new entries after dedup", not by the cursor moving.
async function fetchAllSessionEvents(key, node, headers) {
  const remote = !!(node && node !== 'local');
  const base = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(key) +
    (remote ? '&node=' + encodeURIComponent(node) : '');
  const r = await fetch(base, { headers });
  if (!r.ok) return { status: r.status };
  let events = await r.json();
  if (!Array.isArray(events)) events = [];
  if (remote) return { events, truncated: events.length >= EXPORT_PAGE_LIMIT };
  if (events.length === 0) return { events, truncated: false };

  const seen = new Set(events.map(exportEventKey));
  let truncated = false;
  let oldest = (events[0] && events[0].time) || 0;
  for (let pages = 0; oldest > 0; pages++) {
    if (pages >= EXPORT_MAX_PAGES) { truncated = true; break; }
    const pr = await fetch(base + '&before=' + (oldest + 1) + '&limit=' + EXPORT_PAGE_LIMIT, { headers });
    if (!pr.ok) { truncated = true; break; }
    const page = await pr.json();
    if (!Array.isArray(page)) { truncated = true; break; }
    if (page.length === 0) break;
    const fresh = page.filter(e => {
      const k = exportEventKey(e);
      if (seen.has(k)) return false;
      seen.add(k);
      return true;
    });
    if (fresh.length === 0) {
      // A full page of entries we already hold can't be told apart from a
      // same-ms flood wider than one page — stop and warn. A short page of
      // known entries just means the history is exhausted.
      if (page.length >= EXPORT_PAGE_LIMIT) truncated = true;
      break;
    }
    events = fresh.concat(events);
    const pageOldest = (fresh[0] && fresh[0].time) || 0;
    if (!pageOldest) break; // untimed head reached; nothing older to cursor on
    oldest = pageOldest;
  }
  return { events, truncated };
}

async function downloadSessionMarkdown() {
  if (!selectedKey) return;
  if (_exportInFlight) return;
  _exportInFlight = true;
  // Capture identity up front: the pager may take several round trips and
  // the operator can switch sessions meanwhile — the export still belongs to
  // the session whose button was clicked.
  const key = selectedKey;
  const node = selectedNode;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const res = await fetchAllSessionEvents(key, node, headers);
    if (res.status) {
      showAPIError('导出会话', res.status, '');
      return;
    }
    const events = res.events;
    const truncated = res.truncated;
    if (!Array.isArray(events) || events.length === 0) {
      showToast('会话无可导出内容', 'warning');
      return;
    }
    const s = sessionsData[sid(key, node)] || {};
    const keyParts = (key || '').split(':');
    const title = s.user_label || s.summary || s.last_prompt ||
      keyTailDisplay(keyParts) || key || '';
    const md = formatSessionMarkdown({
      title: title,
      key: key,
      node: node,
      cli: s.cli_name ? (s.cli_name + (s.cli_version ? ' v' + s.cli_version : '')) : '',
      workspace: s.workspace || sessionWorkspaces[key] || '',
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
    if (truncated) {
      showToast('已导出 ' + events.length + ' 条事件（历史过长，更早的事件已截断）', 'warning', 5000);
    } else {
      showToast('已导出 ' + events.length + ' 条事件', 'success', 2000);
    }
  } catch (e) {
    showNetworkError('导出会话', e);
  } finally {
    _exportInFlight = false;
  }
}

// mainHeaderHtml builds the .main-header block (title, rename/export buttons,
// detail line with the empty #header-git / #header-effort / #header-runstats
// mounts) for session snapshot `s`. Shared by renderMainShell (full shell) and
// renderMainHeader (header-only repaint) so the two can never drift.
function mainHeaderHtml(s) {
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
  // ui-polish-light-theme D5: the version string is debug info an operator
  // needs rarely — keep it in the hover title, show just the backend name.
  // (The settings 关于 section lists versions permanently.)
  const cliLabel = headerCLILabelHtml(effCLIName, effCLIVersion);
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
  const rawModel = s.model ||
    (!sessionsData[sid(selectedKey, selectedNode)] && sessionPendingTuning[selectedKey]
      ? (sessionPendingTuning[selectedKey].model || '') : '');
  const compactModel = rawModel
    .replace(/^(global|us|eu|apac)\.anthropic\./, '') // strip Bedrock inference-profile prefix
    .replace(/-(\d+)-(\d+)/, '-$1.$2')          // 4-7 → 4.7 (matches kiro list)
    .replace(/\[(\d+m)\]$/i, ' $1');            // [1m] → " 1m"
  const modelLabel = rawModel
    ? '<span class="model-label" id="header-model" data-action="tuning-model" style="cursor:pointer" title="' + escAttr(rawModel + ' — 点击切换模型') + '">· ' + esc(compactModel) + '</span>'
    : '<span class="model-label model-label-unset" id="header-model" data-action="tuning-model" style="cursor:pointer" title="model 未在 system/init 上报；可能仍在 spawn 中 — 点击可指定模型">· (模型未配置)</span>';
  const headerOriginBadge = originBadgeHtml(selectedKey);
  // UI Round 5 R5-2: header backend chip removed. The "kiro v2.3.0" /
  // "claude-code 2.1.143" cliLabel already names the backend; the
  // surrounding chip was a duplicate signal that competed for attention
  // with cost / turn-timer.
  const headerBackendChip = '';
  // session-run-metrics header cleanup: the per-session cost chip was removed.
  // The figure came from the CLI's self-reported total_cost_usd, which is
  // computed against Anthropic list pricing — under Bedrock that diverges
  // systematically from the actual AWS bill (often reading $0), so it misled
  // more than it informed. The header now surfaces the run-history overview
  // (N 轮 · 均 X · 最长 X) instead, injected asynchronously into
  // #header-runstats by renderSessionRunsPanel.
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
  const canRename = selectedKey && !isDiscoveredKey(selectedKey);
  const renameBtn = canRename
    ? '<button type="button" class="btn-rename" data-action="session-rename" title="重命名会话" aria-label="重命名会话">' + ICONS.edit + '</button>'
    : '';
  // UX P2 Markdown export: any session that has an addressable key can be
  // exported — no dependency on managed status because the /api/sessions/events
  // endpoint serves both managed and discovered keys uniformly. The button
  // shares the .btn-rename hover-reveal treatment so the header stays calm
  // by default.
  const downloadBtn = selectedKey
    ? '<button type="button" class="btn-rename btn-download" data-action="session-download-md" title="导出会话为 Markdown" aria-label="导出会话为 Markdown">' + ICONS.download + '</button>'
    : '';

  return '<div class="main-header">' +
      '<button type="button" class="btn-mobile-back" data-action="mobile-back" title="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868" aria-label="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868">' + ICONS.back + '</button>' +
      '<div class="main-header-content">' +
      '<h2>' + esc(displayName) + renameBtn + downloadBtn + '</h2>' +
      '<div class="detail">' +
        '<span class="detail-left">' + cliLabel + modelLabel + '</span>' +
        headerBackendChip +
        headerOriginBadge +
        // Git branch / worktree chip. Built empty here and filled
        // asynchronously by renderGitChip once /api/sessions/git resolves;
        // stays empty (collapses via :empty) for non-repo workspaces and
        // remote-node sessions.
        '<span class="detail-git" id="header-git"></span>' +
        ctxBarHtml +
        // kiro thinking-effort tier. Built empty and filled by
        // setHeaderEffortChip (called below and from fetchSessions) so a tier
        // change lands without waiting for a header rebuild; collapses via
        // :empty for backends that report no effort.
        '<span class="detail-effort" id="header-effort"></span>' +
        // Spawn-gate warnings (#2532). Built empty and filled by
        // setHeaderSpawnDiagChip (same lifecycle as the effort chip);
        // collapses via :empty when every configured input took effect.
        '<span class="detail-spawndiag" id="header-spawndiag"></span>' +
        // Overlay drift (#2543): live argv vs a fresh spawn under current
        // config. Same lifecycle as the spawn-diag chip; :empty collapses.
        '<span class="detail-overlaydrift" id="header-overlaydrift"></span>' +
        turnTimerHtml +
        // Run-history overview ("N 轮 · 均 X · 最长 X"). Built empty here and
        // filled asynchronously by renderSessionRunsPanel once /api/sessions/runs
        // resolves; stays empty (collapses) for sessions with no recorded runs.
        '<span class="detail-runstats" id="header-runstats"></span>' +
      '</div>' +
      '</div>' +
    '</div>';
}

// renderMainHeader repaints ONLY the header of the current session's shell.
// renameSession used to call renderMainShell, which rebuilds #events-scroll
// empty — and nothing on the rename path refetches history (the WS stays
// subscribed with its cursor advanced; the poll cursor lastEventTime is not
// reset), so the conversation vanished until the session was re-selected.
// Falls back to a full rebuild when no shell is mounted yet.
function renderMainHeader() {
  const main = document.getElementById('main');
  const header = main ? main.querySelector(':scope > .main-header') : null;
  if (!header || !document.getElementById('events-scroll')) { renderMainShell(); return; }
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};
  header.outerHTML = mainHeaderHtml(s);
  // The mounts inside the header were just emptied — repaint from cache /
  // refetch exactly as renderMainShell's tail does.
  repaintGitChip();
  setHeaderEffortChip();
  setHeaderSpawnDiagChip();
  setHeaderOverlayDriftChip();
  fetchSessionRuns(selectedKey, selectedNode);
}

// #2437: the cli label carries a fixed id so updateHeaderCLI can refresh it
// in place. It used to rewrite .detail-left wholesale, which wiped the
// sibling #header-model span (the tuning popover anchor) on every poll that
// passed fetchSessions' version short-circuit. The span is always emitted
// (empty when no backend name is known yet) so a later poll has a target.
function headerCLILabelHtml(name, version) {
  const title = (name && version) ? ' title="' + escAttr(name + ' v' + version) + '"' : '';
  return '<span id="header-cli"' + title + '>' + esc(name || '') + '</span>';
}

function renderMainShell() {
  const main = document.getElementById('main');
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};

  main.innerHTML =
    mainHeaderHtml(s) +
    // cron-panel-consolidation RFC §4.2: cron timeline used to mount here
    // (#cron-timeline-panel placeholder above the events scroll). It now
    // lives entirely inside the 定时任务 panel's per-job drawer; mainShell
    // is reserved for human conversation surfaces.
    // session-run-metrics RFC §8.2: the run-history timeline is a NEW sibling
    // node here (not the relocated cron one). Hidden until renderSessionRunsPanel
    // populates it; :empty/[hidden] keeps it out of layout when a session has
    // no recorded runs.
    '<details class="session-runs-panel" id="session-runs-panel" hidden></details>' +
    '<div class="events" id="events-scroll" role="log" aria-live="polite" aria-relevant="additions">' + (s.state === 'running' ? '<div class="empty-state loading-indicator">\u6b63\u5728\u52a0\u8f7d\u4e8b\u4ef6\u2026</div>' : '') + '</div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button type="button" data-action="nav-msg" data-dir="prev" id="nav-prev" title="\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2191)" aria-label="\u8df3\u5230\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navUp + '</button>' +
      '<span class="nav-counter" id="nav-counter" data-action="nav-show-list" title="\u70b9\u51fb\u67e5\u770b\u5168\u90e8\u7528\u6237\u6d88\u606f"></span>' +
      '<button type="button" data-action="nav-msg" data-dir="next" id="nav-next" title="\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2193)" aria-label="\u8df3\u5230\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navDown + '</button>' +
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
        '<button type="button" class="btn-icon" data-action="file-picker" title="上传图片或 PDF" aria-label="上传图片或 PDF">' + ICONS.attach + '</button>' +
        '<button type="button" class="btn-icon btn-mic" id="btn-mic" data-action="input-mode-toggle" title="' + (voiceInputMode ? '\u5207\u6362\u952e\u76d8' : '\u5207\u6362\u8bed\u97f3') + '" aria-label="' + (voiceInputMode ? '\u5207\u6362\u5230\u952e\u76d8\u8f93\u5165' : '\u5207\u6362\u5230\u8bed\u97f3\u8f93\u5165') + '">' + (voiceInputMode ? ICONS.keyboard : ICONS.mic) + '</button>' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message..." data-action-keydown="msg-input-key" data-action-compositionend="msg-input-compend"></div>' +
        '<button type="button" class="btn-hold-talk" id="btn-hold-talk" title="\u6309\u4f4f\u8bf4\u8bdd\u6539\u5f55\u97f3" aria-label="\u6309\u4f4f\u8bf4\u8bdd\u5f00\u59cb\u5f55\u97f3">\u6309\u4f4f\u8bf4\u8bdd</button>' +
        '<button type="button" class="btn-icon btn-send" id="btn-send" data-action="msg-send" title="发送" aria-label="发送消息">' + ICONS.send + '</button>' +
        '<button type="button" class="btn-icon btn-stop" id="btn-stop" data-action="session-interrupt" title="停止" aria-label="停止当前回合">' + ICONS.stop + '</button>' +
      '</div>' +
      '<div class="input-hints">Enter send &middot; Shift+Enter newline &middot; Esc interrupt</div>' +
      '<input type="file" id="file-input" accept="image/*,application/pdf" multiple style="display:none" data-action-change="file-input-change">' +
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
  // The header was just rebuilt from scratch, which emptied #header-git.
  // Repaint from cache so a rebuild driven by something unrelated (rename,
  // model arriving) doesn't blank the branch chip until the next fetch.
  repaintGitChip();
  // Same rationale as repaintGitChip: the rebuild emptied #header-effort.
  setHeaderEffortChip();
  setHeaderSpawnDiagChip();
  setHeaderOverlayDriftChip();
}

// _fetchEventsInFlight gates concurrent HTTP polls of `/api/sessions/events`.
// The 1 s `setInterval` driver and the on-demand `full` fetch (session
// switch / WS fallback) can otherwise pile up when the network lags or the
// server is slow: the second request completes first, `appendEvents`
// re-orders events, and the first response is then applied on top. The
// simpler in-flight flag (mirroring `_earlierLoading` on
// `loadEarlierEvents`) skips overlapping polls — a missed tick is cheap
// because the next tick will pick up any accumulated events via `after=`
// anyway.
//
// A `full` fetch must NOT be coalesced (#2430): it is the session-switch
// render. Dropping it left the new session to the next tick, which ran with
// lastEventTime=0 and no `limit` → the server's legacy default branch handed
// back the whole ring, appendEvents grafted ≤500 bubbles in one shot, and
// neither "load earlier" nor the saved scroll position was restored. Instead
// a full fetch bumps _fetchEventsGen so the tail still in flight becomes
// stale: it can neither append into the new render nor release the in-flight
// flag the full fetch now owns.
let _fetchEventsInFlight = false;
let _fetchEventsGen = 0;
async function fetchEvents(full) {
  if (!selectedKey) return;
  if (!full && _fetchEventsInFlight) return;
  // Capture session identity at dispatch time so a mid-flight switch doesn't
  // apply stale events to the new session's DOM. `selectedKey` can flip
  // synchronously from `pickSession`/`dismiss` callbacks while `await`
  // suspends us; applying `appendEvents` after that point would graft the
  // prior session's tail into the newly-opened session's scroller.
  const dispatchKey = selectedKey;
  const dispatchNode = selectedNode;
  if (full) _fetchEventsGen++;
  const gen = _fetchEventsGen;
  const stale = () => selectedKey !== dispatchKey || selectedNode !== dispatchNode || gen !== _fetchEventsGen;
  _fetchEventsInFlight = true;
  try {
    let url = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(dispatchKey);
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
    // The initial (`full`) fetch reads the X-Events-Has-More header so it can
    // mount "load earlier" off the server's truncation decision rather than the
    // brittle len>=INITIAL_HISTORY_LIMIT guess. _eventsHeaders captures the raw
    // Response headers for that one call; incremental polls don't need them.
    let hasMoreHeader = null;
    try {
      const onResp = full ? (resp) => {
        // null when the header is absent (legacy server / remote-node relay) —
        // leave hasMoreHeader null so renderEvents falls back to the length
        // heuristic rather than treating "absent" as an authoritative false.
        const v = resp && resp.headers ? resp.headers.get('X-Events-Has-More') : null;
        if (v != null) hasMoreHeader = (v === '1' || v === 'true');
      } : null;
      events = await fetchJSON(url, { headers, timeoutMs: 5000, onResponse: onResp });
    } catch (err) {
      if (err.status) return; // HTTP non-2xx — mirror legacy !r.ok early-return
      throw err;              // timeout / network — surface via outer catch
    }
    if (!events || events.length === 0) return;
    // Drop stale responses whose selection has since moved, or that a newer
    // `full` fetch has superseded. Clearing `lastEventTime` is the caller's
    // job at switch time, so we don't touch it here.
    if (stale()) return;

    if (full) {
      // Pass the server's authoritative hasMore when the header was present;
      // null means "fall back to the length heuristic" (legacy / remote node).
      renderEvents(events, hasMoreHeader);
    } else {
      appendEvents(events);
    }

    const last = events[events.length - 1];
    if (last && last.time > lastEventTime) lastEventTime = last.time;
  } catch (e) {
    console.error('fetch events:', e);
  } finally {
    // Only the newest generation owns the flag (mirrors loadEarlierEvents /
    // _earlierGen): a superseded tail must not free it under the full fetch.
    if (gen === _fetchEventsGen) _fetchEventsInFlight = false;
  }
}

// loadEarlierEvents fetches up to EARLIER_PAGE_LIMIT events older than the
// currently-oldest rendered bubble. Prepends the rendered output to the top
// of the events pane and preserves scroll position so the user's view doesn't
// jump when new content is injected above.
//
// Idempotent: calls bail out while a prior fetch is in flight.
let _earlierLoading = false;
// _earlierGen is bumped by selectSession so a stale loadEarlierEvents (still
// awaiting the previous session's page) can neither prepend into the new
// session's scroller nor clear the new session's in-flight flag.
let _earlierGen = 0;

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

  // Capture session identity at dispatch time (mirrors fetchEvents): the
  // operator can switch sessions while we await, and prepending the old
  // session's page into the new session's scroller grafts two histories.
  const key = selectedKey;
  const node = selectedNode;
  const gen = _earlierGen;
  const stale = () => selectedKey !== key || selectedNode !== node || gen !== _earlierGen;
  _earlierLoading = true;
  updateEarlierButton('loading');
  try {
    let url = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(key) +
              '&before=' + oldestTime + '&limit=' + EARLIER_PAGE_LIMIT;
    if (node && node !== 'local') url += '&node=' + encodeURIComponent(node);
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(url, { headers });
    if (stale()) return;
    if (!r.ok) { updateEarlierButton('error'); return; }
    const events = await r.json();
    if (stale()) return;
    if (!Array.isArray(events) || events.length === 0) {
      updateEarlierButton('done');
      return;
    }
    prependEvents(events);
    // If we got a full page there may be more; otherwise mark done.
    updateEarlierButton(events.length >= EARLIER_PAGE_LIMIT ? 'ready' : 'done');
  } catch (e) {
    console.error('load earlier events:', e);
    if (!stale()) updateEarlierButton('error');
  } finally {
    // Release the flag unless selectSession has already reset it for a newer
    // session (it bumps _earlierGen) — otherwise a second page-back could run
    // concurrently. Keyed on the generation only, NOT the full stale(): paths
    // that flip selectedKey without selectSession (pending-session create,
    // dismiss / discovered preview → selectedKey=null) never reset the flag,
    // so a stale() check here would leave it stuck true until the next select.
    if (gen === _earlierGen) _earlierLoading = false;
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

  // The DOM's leading divider was emitted for prevTime=0 ("always divide
  // before the first visible bubble"). Once older bubbles sit above it, it is
  // only legitimate when the gap to the newest prepended bubble is a real
  // divider gap — otherwise the pagination seam shows two stacked dividers
  // (#2430).
  const oldLeadDivider = leadingTimeDivider(el);
  const frag = document.createElement('div');
  frag.innerHTML = html;
  const newestPrependedTime = lastDividerTime(frag);
  // Move children one-by-one to preserve DOM structure; innerHTML replace
  // would wipe the existing event bubbles. Anchor on the pre-insert first
  // child once: inserting each child before a moving el.firstChild reversed
  // the prepended page (newest-first) under the seam.
  const anchor = el.firstChild;
  while (frag.firstChild) {
    el.insertBefore(frag.firstChild, anchor);
  }
  if (oldLeadDivider && newestPrependedTime) {
    const leadT = Number(oldLeadDivider.getAttribute('data-time') || 0);
    if (leadT && leadT - newestPrependedTime < EVENT_DIVIDER_GAP_MS) oldLeadDivider.remove();
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

// renderEvents replaces the whole events pane on the initial / full-fetch path.
// hasMore (when not null) is the server's authoritative "older history exists"
// signal from the X-Events-Has-More header; null means the header was absent
// (legacy server or remote node) and we fall back to the length heuristic.
function renderEvents(events, hasMore) {
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
  // Poll-fallback twin of onHistory's pre-render hydrate: rebuild the
  // answered-set so replayed AskUserQuestion cards render locked (#2430).
  hydrateAskAnsweredFromHistory(events);
  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  // Decide whether "load earlier" will mount BEFORE rendering the all-internal
  // placeholder, so its copy never promises a button that won't appear. Mount
  // off the server's hasMore flag when present — it knows the slice was
  // truncated by visible-bubble count, catching the case the old length
  // heuristic missed (more visible bubbles than DefaultVisibleTarget but fewer
  // total events than INITIAL_HISTORY_LIMIT). Fall back to the length heuristic
  // only when the header was absent (hasMore === null).
  const showEarlier = (hasMore === true) ||
    (hasMore == null && events.length >= INITIAL_HISTORY_LIMIT);
  if (html) {
    el.innerHTML = html;
  } else if (events.length === 0) {
    el.innerHTML = '<div class="empty-state">暂无事件</div>';
  } else {
    // The server returned events but every one was filtered out by
    // INTERNAL_EVENT_TYPES — typically a parallel agent team where the
    // visible tail of the log is all tool_use / task_progress. Render a
    // neutral placeholder so the panel isn't a blank void. Only invite the
    // user to "click below" when the button will actually mount; otherwise the
    // whole remembered history is internal activity with nothing older to page
    // to, so promise nothing.
    el.innerHTML = showEarlier
      ? '<div class="empty-state">该会话最近仅有 agent 活动，点击下方加载更早的消息</div>'
      : '<div class="empty-state">该会话仅有 agent 活动，暂无对话消息</div>';
  }
  if (events.length > 0) {
    const last = events[events.length - 1];
    if (last.time) lastRenderedEventTime = last.time;
    const first = events[0];
    if (first.time && (oldestFetchedEventTime === 0 || first.time < oldestFetchedEventTime)) {
      oldestFetchedEventTime = first.time;
    }
  }
  if (showEarlier) {
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
  // An ask_question followed by a user event inside this same batch must
  // render already locked (mirrors onHistory's pre-render hydrate).
  hydrateAskAnsweredFromHistory(events);
  // Force-bottom when a "user" event arrives: either the local operator just
  // hit send, or a teammate posted through the IM channel — in both cases the
  // message must be visible, even if the viewport was scrolled up.
  let sawUser = false;
  events.forEach(e => {
    if (isInternalEvent(e)) return;
    // Deduplicate: drop strictly-older events. Same-ms events are legitimate
    // siblings (thinking + text from one frame, two text blocks) and are only
    // dropped when their uuid is already on screen — same rule as onHistory.
    if (e.time && e.time < lastRenderedEventTime) return;
    if (e.time && e.time === lastRenderedEventTime && eventAlreadyRendered(el, e.uuid)) return;
    if (e.type === 'user') {
      // Same rules as the WS paths (onEvent / onHistory): a user bubble whose
      // uuid is already on screen is a replay, and the first arrival of the
      // real user event replaces the optimistic bubble the send rendered.
      // Without this a send that left over WS and was echoed by the poll
      // (socket dropped in between) painted the message twice (#2430).
      if (eventAlreadyRendered(el, e.uuid)) {
        if (e.time && e.time > lastRenderedEventTime) lastRenderedEventTime = e.time;
        return;
      }
      const opt = el.querySelector('.optimistic-msg');
      if (opt) opt.remove();
      // Lock cards already on screen before this user bubble is appended.
      lockRenderedAskCards(el);
    }
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

// lockRenderedAskCards applies hydrateAskAnsweredFromHistory's rule to the
// live DOM: a `user` event landing incrementally (WS onEvent, onHistory
// backfill, poll appendEvents) means every AskUserQuestion card already on
// screen was answered on some surface (Feishu, the input box, another tab), so
// it must lock now — not only after a reload replays history. Without this the
// stale card stayed submittable and pushed an out-of-date answer into the
// next turn (#2430). Idempotent: cards onAskSubmit already locked are skipped
// via the existing .ask-status marker.
function lockRenderedAskCards(scrollEl) {
  if (!scrollEl) return;
  scrollEl.querySelectorAll('.event.ask_question[data-tool-use-id]').forEach(card => {
    const tuid = card.getAttribute('data-tool-use-id') || '';
    if (!tuid) return;
    _askAnswered.add(tuid);
    card.querySelectorAll('button').forEach(b => { b.disabled = true; });
    const content = card.querySelector('.event-content');
    if (!content) return;
    const status = content.querySelector('.ask-status');
    if (!status) {
      const div = document.createElement('div');
      div.className = 'ask-status';
      div.textContent = '已回答';
      content.appendChild(div);
    } else if (status.textContent.indexOf('发送失败') === 0) {
      // onAskSubmit's failure rollback left the card actionable; a user event
      // from another surface has since answered it, so the failure copy is
      // stale — replace it rather than leave a locked card saying "failed".
      status.textContent = '已回答';
    }
  });
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
        ' data-action="ask-option-toggle">' +
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
    ' data-action="ask-submit">提交全部回答</button>';
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
    const scratchKey = getActiveScratchKey
      ? getActiveScratchKey()
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
  const r = await fetch(NZ_CONTRACT.API.sessions_send, { method: 'POST', headers, body: JSON.stringify(payload) });
  if (!r.ok) {
    const raw = await r.text().catch(() => '');
    throw new Error('send failed: ' + r.status + ' ' + raw.slice(0, 200));
  }
}

// LEAKED_TOOLCALL_RE anchors the start of a tool-call block that an LLM
// emitted as *prose* instead of a structured tool_use content block. The
// claude/anthropic harness expresses a real tool call as a dedicated
// content block (type:"tool_use") which naozhi surfaces as its own
// tool_use event and filters out of the main transcript (isInternalEvent).
// But the model occasionally regresses and writes the call syntax —
//   call
//   <invoke name="Bash">
//   <parameter name="command">…</parameter>
//   </invoke>
// — verbatim into an assistant *text* block. That text flows through the
// pipeline untouched (process_event_format.go stores it as type:"text")
// and lands in the bubble as a literal wall of XML, which is noise to the
// operator (the call was never executed — it's a malformed turn).
//
// The anchor REQUIRES a `call` or `<function_calls>` marker alone on its
// own line immediately preceding `<invoke name="`. This is deliberately
// strict: a bare `<invoke …>` must NOT trip the detector, because operators
// legitimately quote tool-call syntax inside backticks when discussing it
// (e.g. this very bug report). Validated against 667 real text/user events:
// 9 genuine leaks caught, 0 false positives on quoted-syntax discussions.
const LEAKED_TOOLCALL_RE = /(?:^|\n)[ \t]*(?:call|<function_calls>)[ \t]*\n[ \t]*<invoke name="/;

// stripLeakedToolCalls splits an assistant text body into the real prose
// that precedes a leaked tool-call block and the leaked block itself.
// Returns null when no leak is detected (the overwhelmingly common case —
// keep this cheap so every text bubble can call it). On a hit it returns
// { prose, leaked } where `prose` is the body with the leaked region
// removed (trailing whitespace trimmed) and `leaked` is the raw XML for the
// fold-away <details>. The region runs from the anchor's `call` /
// `<function_calls>` line to the final `</invoke>` (plus an optional
// trailing `</function_calls>`), so multiple chained <invoke> blocks under
// one marker collapse into a single fold.
function stripLeakedToolCalls(text) {
  if (!text || text.indexOf('</invoke>') === -1) return null;
  const m = LEAKED_TOOLCALL_RE.exec(text);
  if (!m) return null;
  // Anchor start = the `call` / `<function_calls>` line, not the regex's
  // leading \n. m.index points at the char before that line when the
  // alternation matched `\n`; step over it so the marker stays in `leaked`.
  let start = m.index;
  if (text[start] === '\n') start += 1;
  // The leaked region ends at the LAST </invoke> — a single prose turn that
  // leaks more than one call writes them consecutively, and everything from
  // the marker to the last close tag is the malformed payload.
  let end = text.lastIndexOf('</invoke>') + '</invoke>'.length;
  const tail = text.slice(end);
  const fc = /^\s*<\/function_calls>/.exec(tail);
  if (fc) end += fc[0].length;
  return { prose: text.slice(0, start).replace(/\s+$/, ''), leaked: text.slice(start, end) };
}

// eventHtml renders one EventEntry bubble.
// opts.includeInternal=true keeps tool_use / task_* / agent / result
// events that the parent view hides (banner handles them there). The sub-agent
// internal view (agent_view.js) needs them — a team member's work is almost
// entirely tool_use; filtering those out leaves the panel looking
// empty even when the jsonl transcript is full of content. RFC v4 §3.6.7 /
// §3.6.1 contract: parent and agent views share the bubble renderer but
// differ on the filter policy.
function eventHtml(e, opts) {
  if (e && e.type === 'thinking') return '';
  const includeInternal = !!(opts && opts.includeInternal);
  if (!includeInternal && isInternalEvent(e)) return '';
  // AskUserQuestion interactive card: dedicated renderer with option buttons.
  // The matching tool_use entry is already filtered out via INTERNAL_EVENT_TYPES,
  // so the card stands alone in the transcript.
  if (e.type === 'ask_question') return renderAskQuestionCard(e);
  // Filter out Claude Code system XML injected as user messages
  const raw = e.detail || e.summary || '';
  if (e.type === 'user' && /^<(task-notification|system-reminder|local-command|command-name|available-deferred-tools)[\s>]/.test(raw)) return '';
  // CLI-synthesised interrupt marker: SIGINT-aborted turn, not user intent.
  if (e.type === 'user' && (raw === '[Request interrupted by user]' || raw === '[Request interrupted by user for tool use]')) return '';
  const icons = {init:ICONS.gear,system:ICONS.gear,user:ICONS.user,text:ICONS.spark,todo:ICONS.todo};
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
    // Guard against a leaked tool-call block (the model wrote <invoke …>
    // syntax into a text turn instead of emitting a structured tool_use).
    // Render the real prose normally and fold the malformed XML behind a
    // collapsed warning so the bubble stays readable; esc() keeps the raw
    // payload inert (it is displayed as text, never parsed as HTML).
    const leak = stripLeakedToolCalls(cleanRaw);
    if (leak) {
      content = renderMd(leak.prose || e.type) +
        '<details class="leaked-toolcall"><summary class="leaked-toolcall-summary">' +
        '⚠ 模型输出了未执行的工具调用（已折叠）</summary>' +
        '<pre class="leaked-toolcall-body">' + esc(leak.leaked) + '</pre></details>';
    } else {
      content = renderMd(cleanRaw || e.type);
    }
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
      var toolURL = NZ_CONTRACT.API.sessions_tool_result + '?key=' + encodeURIComponent(selectedKey) +
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
  // Fallback to thumb on load failure: the lightbox's loadWithFallback
  // covers both HTTP 404 (attachment GC'd) and Content-Type mismatch
  // (it checks naturalWidth===0 after onload). See the lightbox IIFE's
  // loadWithFallback comment for rationale. RFC §3.6.3.
  let imgHtml = '';
  if (e.images && e.images.length > 0) {
    const paths = e.image_paths || [];
    const cacheBust = e.time ? ('&v=' + e.time) : '';
    imgHtml = '<div class="event-images">' + e.images.map((src, i) => {
      const p = paths[i] || '';
      let full = src;
      if (p && selectedKey) {
        full = NZ_CONTRACT.API.sessions_attachment + '?key=' + encodeURIComponent(selectedKey) +
          '&path=' + encodeURIComponent(p) + cacheBust;
      }
      // No inline onclick: a document-level delegated listener in the
      // lightbox IIFE handles clicks on .event-images img[data-full] and
      // opens the whole group (RFC lightbox-gallery-nav §3). Delegation
      // survives the poll-driven innerHTML re-renders that destroy and
      // recreate these nodes.
      return '<img src="' + escAttr(src) + '" loading="lazy" ' +
        'data-full="' + escAttr(full) + '" ' +
        'data-thumb="' + escAttr(src) + '">';
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
    ? '<button class="event-copy-btn hover-only" type="button" data-raw="' + escAttr(cleanRaw) + '" data-action="event-copy" title="复制" aria-label="复制消息">复制</button>'
    : '';
  const askBtn = isLong && e.type === 'text'
    ? '<button class="event-ask-btn hover-only" type="button" data-raw="' + escAttr(cleanRaw) + '" data-msg-time="' + (e.time || 0) + '" data-action="ask-aside" title="基于此内容追问">' + ICONS.preview + ' 追问</button>'
    : '';

  const timeAttr = e.time ? ' data-time="' + e.time + '" title="' + escAttr(formatTimeFull(e.time)) + '"' : '';
  // data-uuid carries the backend's authoritative entry identity (crypto/rand
  // hex from internal/cli/uuid.go, round-tripped via the event-log entry). It
  // is the idempotency key for user-bubble dedup: a process restart re-subscribe
  // replays history, and without a stable per-event identity the same user
  // message renders twice (optimistic bubble already consumed, time cursor
  // didn't advance). See docs/rfc/dashboard-event-uuid-idempotent-render.md.
  // Attribute is omitted (not empty) when uuid is absent so "no uuid" events
  // (some CLI-synthesised entries) stay distinguishable and never collide.
  const uuidAttr = e.uuid ? ' data-uuid="' + escAttr(e.uuid) + '"' : '';
  return '<div class="event ' + esc(e.type||'') + '"' + timeAttr + uuidAttr + '>' +
    '<span class="event-icon">' + icon + '</span>' +
    '<div class="event-content">' + content + imgHtml + copyBtn + askBtn + '</div></div>';
}

// eventAlreadyRendered reports whether a .event with this uuid is already in
// the given scroll container. DOM is the single source of truth for render
// dedup — no parallel JS Set — so trimEventsScroll() eviction and full
// innerHTML rebuilds keep the dedup set automatically consistent (an element
// trimmed from the DOM stops matching, exactly as intended). Empty/absent
// uuid never matches (returns false) so uuid-less events are never swallowed.
// CSS.escape guards the attribute selector even though uuids are hex — keeps
// the "all selector inputs are escaped" invariant if the uuid source ever
// changes shape. See docs/rfc/dashboard-event-uuid-idempotent-render.md.
function eventAlreadyRendered(scrollEl, uuid) {
  if (!scrollEl || !uuid) return false;
  const sel = (typeof CSS !== 'undefined' && CSS.escape) ? CSS.escape(uuid) : uuid;
  return !!scrollEl.querySelector('.event[data-uuid="' + sel + '"]');
}

// Expose the bubble renderer for agent_view.js (RFC v4 agent-team-ui §3.6).
// The sub-agent transcript panel must use the same layout as the parent view —
// tool_result folding, markdown, image thumbnails, copy/ask buttons — so one
// eventHtml is the source of truth (agent_view imports it; a past revision
// referenced a non-existent stub and silently lost the entire bubble UI).

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

// leadingTimeDivider returns the scroller's first time divider when it
// precedes every rendered bubble (the divider renderEventsWithDividers emits
// for prevTime=0); null when a bubble comes first or nothing is rendered.
function leadingTimeDivider(el) {
  if (!el) return null;
  for (const c of el.children) {
    if (!c.classList) continue;
    if (c.classList.contains('event-time-divider')) return c;
    if (c.classList.contains('event')) return null;
  }
  return null;
}

// removeOptimisticMsg drops the optimistic user bubble a send rendered. With
// a send id (the WS send_ack echoes the `id` the send frame carried) only that
// send's bubble goes — a busy/error ack for the second of two in-flight sends
// must not eat the first one's bubble (#2430). Without an id (legacy servers,
// HTTP-path send_error) fall back to the oldest bubble on screen.
function removeOptimisticMsg(sendId) {
  const root = document.getElementById('events-scroll') || document;
  let opt;
  if (sendId) {
    const sel = (typeof CSS !== 'undefined' && CSS.escape) ? CSS.escape(sendId) : sendId;
    opt = root.querySelector('.optimistic-msg[data-send-id="' + sel + '"]');
  } else {
    opt = root.querySelector('.optimistic-msg');
  }
  if (opt) opt.remove();
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

function getMsgValue(el) { return (el ? el.innerText : '').trim(); }
function setMsgValue(el, v) { if (el) el.innerText = v; }
function clearMsg(el) { if (el) el.textContent = ''; }

// validateComposerForSend runs the synchronous pre-send checks that depend on
// the live composer (text-level backend feature gates, byte cap, in-flight
// uploads). Toasts and returns false when the send must abort. sendMessage
// calls it twice: once before closing the reentrancy gate and again after
// `await awaitPendingOrients()` — the composer stays editable during that wait,
// so text and attachments captured before the await can be stale (#2405).
function validateComposerForSend(text) {
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
    return false;
  }

  // Multi-Backend RFC §8.3 D13 — `@-mention` embedded context only
  // works when the backend reads file paths from inside the prompt
  // (claude does; kiro doesn't). Strip-and-warn would silently change
  // the prompt; better to abort + toast so the operator can paste the
  // absolute path or content explicitly.
  if (text && /(?:^|\s)@[\w./-]/.test(text) && !featureForCurrent('embedded_context')) {
    showToast('当前后端不支持 @ 文件 mention，请粘贴绝对路径或文件内容', 'warning');
    return false;
  }

  // Per-field byte cap matches server maxWSSendTextBytes (1 MB). Reject
  // up-front so oversize pastes don't round-trip and return a silent
  // send_ack error that the optimistic bubble would have already printed.
  const byteLen = new Blob([text]).size;
  if (byteLen > 1024 * 1024) {
    showToast('消息过长 (' + Math.ceil(byteLen / 1024) + ' KB > 1024 KB 上限)', 'warning');
    return false;
  }

  // Block send while any attachment is still uploading or errored —
  // we only reference file_ids on the server, so partial uploads would
  // silently drop images. User can retry or remove the bad one.
  if (pendingFiles.some(f => f.status === 'uploading')) {
    showToast('图片上传中，请稍候…', 'warning');
    return false;
  }
  return true;
}

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
      const r = await fetch(NZ_CONTRACT.API.discovered_takeover, {
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
      dropDiscovered(pd.pid, pd.node);
      // Remove the discovered card from sidebar
      removeSidebarCard(discoveredKey(pd.pid, pd.node));
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
  if (!validateComposerForSend(text)) return;

  // #2405 — close the reentrancy gate BEFORE the first await. sendComposerTurn
  // starts with `await awaitPendingOrients()`, which can block for up to
  // ORIENT_MAX_WAIT_MS while the composer stays editable. With the gate set
  // only after that await, every Enter pressed during the wait spawned another
  // sendMessage that captured the same text; once orient settled, waiter #1
  // sent text+file_ids and cleared the composer while #2..N each fired a
  // text-only ghost over WS. The `.sending` class is the visible cue that the
  // click registered. The finally is the ONLY reset — every exit path of the
  // gated half goes through it.
  sending = true;
  const btn = document.getElementById('btn-send');
  if (btn) btn.classList.add('sending');
  try {
    await sendComposerTurn(selectedKey, selectedNode);
  } finally {
    sending = false;
    if (btn) btn.classList.remove('sending');
  }
}

// sendComposerTurn is the gated half of sendMessage: the caller holds the
// `sending` gate for its whole duration. targetKey/targetNode are the session
// the operator hit send on; a switch during the orient wait aborts the send
// rather than redirecting the captured text to the newly selected session.
async function sendComposerTurn(targetKey, targetNode) {
  // Auto-orient runs as a fire-and-forget vision side-call after upload
  // (maybeAutoOrient). If the user hits send within its ~12s window, the
  // server would TakeAll the upload BEFORE the rotation's in-place Replace
  // lands — sending the original sideways image. Transparently wait for any
  // in-flight orient to settle (hard-capped at ORIENT_MAX_WAIT_MS) so the
  // rotated bytes are in the store before we consume the file_ids. Silent by
  // design: no toast, the user already clicked send and the rotation is
  // best-effort.
  await awaitPendingOrients();
  if (!selectedKey || selectedKey !== targetKey || selectedNode !== targetNode) return;
  // The composer stayed editable during the wait: re-read the text and re-run
  // the synchronous checks. Without the uploading re-check an id-less upload
  // dropped mid-wait was filtered out of fileIDs and then deleted by
  // clearPendingFiles() — the attachment vanished without a trace.
  const input = document.getElementById('msg-input');
  const text = getMsgValue(input);
  if (!text && pendingFiles.length === 0) return;
  if (!validateComposerForSend(text)) return;
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

  // Flip the send→stop button + running banner BEFORE the network round trip,
  // not after — a resumed session has no CLI process yet, so the first send
  // triggers a subprocess spawn that can take several hundred ms. Leaving the
  // green send button visible during that window makes the click feel ignored
  // and invites double-sends. onSendAck/rollbackOptimisticRunning undo this on
  // busy/error/reset; the 20s safety timer in markSessionOptimisticRunning
  // prevents a stuck banner if the server never responds.
  markSessionOptimisticRunning(selectedKey, selectedNode);

  // WS path: preferred for TEXT-ONLY sends. Sends carrying file_ids MUST go
  // over HTTP instead: the uploadStore owner for a WS send is the one frozen
  // at WebSocket upgrade time (wsDeriveUploadOwner → setUploadOwner, never
  // refreshed in no-token mode), while /api/sessions/upload derives its owner
  // from the CURRENT nz_anon cookie. The nz_anon label expires after
  // anonCookieMaxAgeSeconds (1h) with no sliding renewal on old servers, so a
  // long-lived dashboard tab ends up with upload-owner ≠ WS-owner and every
  // file-bearing WS send fails TakeAll with "file not found or expired".
  // HTTP sends carry the same cookie the upload just used (or freshly
  // minted), so the two owners can never diverge. Token-mode deployments are
  // owner-stable either way; routing on file presence keeps them on the same
  // path for consistency.
  if (wsm.isConnected() && fileIDs.length === 0) {
    const id = 'r' + (++wsm.sendCounter);
    const sendMsg = { type: 'send', key: selectedKey, text: text, id: id };
    // No file_ids here by construction — file-bearing sends take the HTTP
    // path above so the uploadStore owner matches the upload's cookie.
    if (selectedNode && selectedNode !== 'local') sendMsg.node = selectedNode;
    if (sessionWorkspaces[selectedKey]) sendMsg.workspace = sessionWorkspaces[selectedKey];
    if (sessionBackends[selectedKey]) sendMsg.backend = sessionBackends[selectedKey];
    if (sessionAccessProfiles[selectedKey]) sendMsg.access_profile = sessionAccessProfiles[selectedKey];
    if (wsm.send(sendMsg)) {
      // Workspace/backend/access profile are consumed once on session spawn;
      // forget them only now that the frame is out. A failed wsm.send falls
      // through to the HTTP path below, which must still see them.
      if (sendMsg.workspace) {
        delete sessionWorkspaces[selectedKey];
        delete sessionNodes[selectedKey];
      }
      delete sessionBackends[selectedKey];
      delete sessionAccessProfiles[selectedKey];
      // Optimistic render: show user message immediately without waiting
      // for the CLI to echo it back as a "user" event.
      renderOptimisticUserMsg(text, id);
      if (input) clearMsg(input);
      delete sessionDrafts[selectedKey];
      clearPendingFiles();
      if (text) sessionLastSent[sid(selectedKey, selectedNode)] = text;
      // Confirmed send: the workspace/node/backend were consumed above (and
      // deleted from the in-memory maps), so rewrite the durable blob without
      // this key. Only on the success path — a failed wsm.send falls through to
      // HTTP below and must keep the entry for that retry.
      persistPending();
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
    if (sessionAccessProfiles[selectedKey]) {
      payload.access_profile = sessionAccessProfiles[selectedKey];
      delete sessionAccessProfiles[selectedKey];
    }

    // Mark this tab as the originator BEFORE the request leaves (text or
    // image-only alike): a send_error for this turn can arrive over the WS
    // any time after the server has the request.
    const sentSid = sid(selectedKey, selectedNode);
    httpSendPending.add(sentSid);
    const r = await fetch(NZ_CONTRACT.API.sessions_send, {method:'POST', headers, body: JSON.stringify(payload)});

    if (!r.ok) {
      httpSendPending.delete(sentSid); // rejected synchronously — no async frame will follow
      if (input) setMsgValue(input, text);
      rollbackOptimisticRunning(selectedKey, selectedNode);
      // Some error paths still write text/plain; fall back to text() so we
      // always surface the real message instead of a generic "send failed".
      const raw = await r.text().catch(() => '');
      let detail = '', filesConsumed = false;
      try {
        const j = JSON.parse(raw);
        if (j && j.error) detail = j.error;
        if (j && j.files_consumed) filesConsumed = true;
      } catch (_) { if (raw) detail = raw; }
      // files_consumed: the server already took the pre-uploaded attachments
      // out of the uploadStore before rejecting (post-TakeAll 4xx/5xx), so the
      // chips we still hold reference dead ids — a retry would fail with
      // "file not found or expired". Drop them and ask for a re-attach.
      // Pre-TakeAll rejections (and 401/403/429 from the middleware/limiter)
      // never set the flag, so the user's unsent attachments stay put.
      if (filesConsumed) {
        clearPendingFiles();
        showToast('附件已失效，请重新添加后再发送', 'warning');
      }
      if (r.status === 401 || r.status === 403) {
        showAuthModal();
        return;
      }
      if (r.status === 429) {
        // The server names the limiter that fired (send vs upload rate limit);
        // there is no queue-full 429 on this path, so never invent one.
        showToast(detail || '请求过于频繁，请稍后重试', 'warning');
        return;
      }
      showAPIError('发送消息', r.status, detail);
      return;
    }

    // /clear and /new return status:"reset" — no CLI turn to run, so don't
    // flip to 'running'. Every other success ('accepted'/'queued') should
    // show the banner immediately. Read the body once (before clearing the
    // input) so we can branch on status without reviving the stale text.
    // Record the sent text BEFORE awaiting the body (interrupt re-fill source;
    // also a secondary onSendError gate).
    if (text) sessionLastSent[sentSid] = text;
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
      delete sessionLastSent[sentSid]; // no turn ran, nothing to re-fill on interrupt
      httpSendPending.delete(sentSid);
    } else {
      // Optimistic running flip already applied above — keep it.
      // Optimistic bubble parity with the WS path — but ONLY while WS is
      // connected: the live event stream (onHistory/onEvent) is what removes
      // .optimistic-msg when the real "user" event arrives. The WS-down
      // fallback keeps its legacy no-bubble behaviour (appendEvents also
      // replaces the bubble now, but the poll echo lags up to a tick).
      if (wsm.isConnected()) renderOptimisticUserMsg(text);
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
    httpSendPending.delete(sid(selectedKey, selectedNode));
    if (input) setMsgValue(input, text);
    rollbackOptimisticRunning(selectedKey, selectedNode);
    showNetworkError('发送消息', e);
  }
}

// renderOptimisticUserMsg appends the just-sent text as an optimistic user
// bubble at the bottom of the events scroller. The real "user" event pushed
// by the server (onHistory/onEvent) removes the `.optimistic-msg` element
// when it arrives. Shared by the WS send path and the HTTP send path used
// for file-bearing sends (owner-divergence fix) — both run under a live WS
// subscription, which is what guarantees the removal side fires. No-op when
// text is empty (image-only sends have no text to echo; the thumbnails
// arrive with the real user event). `sendId` (WS path) is stamped on the
// bubble so a busy/error send_ack can roll back exactly this send.
function renderOptimisticUserMsg(text, sendId) {
  const el = document.getElementById('events-scroll');
  if (!el || !text) return;
  const now = Date.now();
  const html = eventHtml({type: 'user', detail: text, time: now});
  if (!html) return;
  const prevT = lastDividerTime(el);
  if (prevT === 0 || now - prevT >= EVENT_DIVIDER_GAP_MS) {
    el.insertAdjacentHTML('beforeend', timeDividerHtml(now));
  }
  el.insertAdjacentHTML('beforeend', html);
  el.lastElementChild.classList.add('optimistic-msg');
  if (sendId) el.lastElementChild.setAttribute('data-send-id', sendId);
  // Always force-bottom after a send: the user just posted something and
  // expects to see it, even if they had scrolled up to browse earlier
  // history. stickEventsBottom handles async layout changes from input-area
  // collapse and lazy images.
  stickEventsBottom();
  navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
  navUpdatePill();
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

// patchSidebarCardState updates the sidebar card's status dot + label text in
// place so an optimistic running flip is reflected on the left list at the
// same instant as the main conversation, instead of lagging behind the
// server's session_state push (which can be several hundred ms when a CLI
// subprocess has to spawn) or the 5s sessions poll. Mirrors the DOM-patch
// branch in onSessionState. No-op if the card isn't currently rendered.
function patchSidebarCardState(key, node, state) {
  const msgNode = node || 'local';
  const displayState = state === 'dead' ? 'ready' : state;
  let card = null;
  document.querySelectorAll('.session-card').forEach(c => {
    if (c.dataset.key === key && (c.dataset.node || 'local') === msgNode) card = c;
  });
  if (!card) return;
  const dot = card.querySelector('.sc-dot');
  if (dot) {
    dot.className = 'sc-dot ' + (displayState === 'running' ? 'dot-running' : (displayState === 'ready' ? 'dot-ready' : 'dot-new'));
  }
  const meta = card.querySelector('.sc-meta');
  if (meta) {
    const stateSpan = meta.querySelectorAll('span')[1]; // [0]=dot, [1]=state text
    if (stateSpan && !stateSpan.classList.contains('sc-node')) stateSpan.textContent = displayState;
  }
}

function markSessionOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = sid(key, node || 'local');
  const sd = sessionsData[sKey];
  if (sd && sd.state === 'running') return; // server already said running
  // A just-created session has no sessionsData entry until the next list
  // fetch. Still flip the button/banner below (#2405: the missing feedback on
  // a new session's first send invited Enter mashing); fetchSessions keeps the
  // flag-forced 'running' once the entry lands, onSessionState clears it.
  if (sd) {
    sessionOptimisticPrevState[sKey] = sd.state;
    sd.state = 'running';
  }
  sessionOptimisticRunning[sKey] = true;
  // Sidebar parity: flip the card's dot/label to running right now so the
  // left list never looks idle while the main banner already says working.
  patchSidebarCardState(key, node, 'running');
  if (_optimisticRunningTimers[sKey]) clearTimeout(_optimisticRunningTimers[sKey]);
  _optimisticRunningTimers[sKey] = setTimeout(() => {
    delete _optimisticRunningTimers[sKey];
    // Only rollback if still optimistic (no real running state arrived).
    if (sessionOptimisticRunning[sKey]) {
      rollbackOptimisticRunning(key, node);
    }
  }, 20000);
  if (key === selectedKey && (node || 'local') === selectedNode) {
    // justSent makes the banner's first line read "已发送，正在处理…" until the
    // first real event (tool/thinking/output) arrives, so the operator gets a
    // distinct "received, starting up" signal during CLI spawn rather than a
    // generic static "处理中…".
    turnState.justSent = true;
    startTurnTimer();
    updateSendButton('running');
  }
}

function rollbackOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = sid(key, node || 'local');
  if (!sessionOptimisticRunning[sKey]) return;
  delete sessionOptimisticRunning[sKey];
  delete sessionOptimisticPrevState[sKey];
  if (_optimisticRunningTimers[sKey]) {
    clearTimeout(_optimisticRunningTimers[sKey]);
    delete _optimisticRunningTimers[sKey];
  }
  const sd = sessionsData[sKey];
  if (sd && sd.state === 'running') {
    sd.state = 'ready';
    patchSidebarCardState(key, node, 'ready');
  }
  // The flip may have been applied without a sessionsData entry (new session's
  // first send) — restore the button either way.
  if (key === selectedKey && (node || 'local') === selectedNode) updateSendButton('ready');
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
  popover.style.cssText = 'position:absolute;right:44px;bottom:0;width:' + maxW + 'px;max-height:300px;overflow-y:auto;background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);border:1px solid var(--nz-border);border-radius:10px;padding:6px 0;z-index:11;font-size:13px;scrollbar-width:thin;scrollbar-color:var(--nz-border) transparent';
  popover.innerHTML = items.join('');
  pill.appendChild(popover);
  navPopoverOpen = true;
  popover.querySelectorAll('.nav-list-item').forEach(item => {
    item.style.cssText += 'padding:8px 12px;cursor:pointer;color:var(--nz-text);transition:background .1s;border-bottom:1px solid var(--nz-bg-2);overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
    item.onmouseenter = () => item.style.background = 'var(--nz-hover-bg)';
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
    handleFiles(imageFiles);
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
  // voice-overlay (R20260610-UI-3): the recording overlay had no Esc handler,
  // so a stuck recording could only be dismissed by clicking it. Mirror the
  // click escape-hatch (see #voice-overlay click listener) on Esc for parity
  // with every other overlay.
  if (escCloseVoiceOverlay()) closed = true;
  if (activePopover) { closeHistoryPopover(); closed = true; }
  if (document.getElementById('nav-list-popover')) { navDismissPopover(); closed = true; }
  // §16 inline-expand 回归 + cron-panel-consolidation RFC §6.4: Esc 关 cron 的
  // 行内展开 / drawer。优先级（行展开先于 drawer）与关闭逻辑都收在 cron_view.js
  // 的 cronEscClose 里，dashboard.js 仅经委托——绝不跨脚本裸引用 cron 内部状态
  // （cronExpandedRunId / cronDetailJobId），否则 cron_view.js 未加载时这里会抛
  // `cronExpandedRunId is not defined`（dashboard-cron-view-extraction §2.6 B1）。
  // nz.views.cron 缺席（cron_view.js 没加载）时优雅降级，不影响其它 Esc 分支。
  // 独立 if（非 else if）：忠实保留迁移前语义——cron 分支独立于上方 popover 分支，
  // 即便同一次 Esc 已关掉 history/nav-list popover，仍会继续关 cron 展开/drawer。
  if (nzViews.cron && nzViews.cron.escClose()) { closed = true; }
  if (closed) e.preventDefault();
});

// §16 inline-expand 回归: ↑↓ 切上一条 / 下一条 run 的全局快捷键已随 cron 状态一并
// 迁入 cron_view.js（B1 修复）——handler 与它读的 cronExpandedRunId / navigateExpandedRun
// 同处一个 <script>，绑定必然就绪；cron_view.js 缺席则该快捷键自然不注册，不再
// 拖垮 dashboard.js。Cmd/Ctrl+Up/Down 的会话切换仍在下方（有 metaKey 守卫，错开）。

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

// Turn watchdog: while the selected session is "running", periodically pull
// the authoritative REST snapshot so the banner self-heals if a terminal WS
// signal (the 'result' event and/or the 'ready' session_state broadcast) is
// dropped on a still-open connection. Without this the "处理中..." banner stays
// stuck until the operator switches sessions or reconnects — the bug this fixes.
// fetchSessions reconciles via updateMainState (see the relaxed gate in
// fetchSessions); the watchdog just supplies the missing tick, since the
// session poll is stopped while WS is connected.
let _turnWatchdogTimer = null;
const TURN_WATCHDOG_INTERVAL_MS = 15000;
function startTurnWatchdog() {
  if (_turnWatchdogTimer) return;
  _turnWatchdogTimer = setInterval(() => {
    // Self-heal: if the selected session was cleared without routing through
    // updateSendButton (dismissSession nulls selectedKey + swaps to the empty
    // shell in three branches), the fetchSessions reconcile is gated on
    // `if (selectedKey)` and would never stop us — so retire the watchdog here
    // instead of polling /api/sessions forever for the page lifetime.
    if (!selectedKey) { stopTurnWatchdog(); return; }
    debouncedFetchSessions();
  }, TURN_WATCHDOG_INTERVAL_MS);
}
function stopTurnWatchdog() {
  if (_turnWatchdogTimer) { clearInterval(_turnWatchdogTimer); _turnWatchdogTimer = null; }
}

function updateSendButton(state) {
  if (selectedKey) _lastAppliedMainState = { key: sid(selectedKey, selectedNode), state: state };
  const banner = document.getElementById('running-banner');
  const sendBtn = document.getElementById('btn-send');
  const stopBtn = document.getElementById('btn-stop');
  const inVoiceMode = document.getElementById('input-area')?.classList.contains('voice-mode');
  if (state === 'running') {
    if (banner) banner.style.display = '';
    if (sendBtn) sendBtn.style.display = 'none';
    if (stopBtn) stopBtn.style.display = 'flex';
    if (nzViews.agent) nzViews.agent.initFromSession();
    refreshBanner();
    startTurnWatchdog();
  } else {
    stopTurnWatchdog();
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
//
// node (optional) selects which node's manifest to fetch for the node-aware
// new-session picker. Omitted / 'local' returns the local manifest and keeps
// the global cliBackends cache (which every chip / feature-gate consumer
// reads) warm. A remote node id appends ?node=<id> so the primary proxies to
// that node (picker node-aware fix); the remote result is cached per node in
// cliBackendsByNode and does NOT touch the global cliBackends / feature gates.
async function fetchCLIBackends(node) {
  const isLocal = !node || node === 'local';
  if (isLocal) {
    if (cliBackends && Date.now() - cliBackendsFetchedAt < 60000) {
      return cliBackends;
    }
  } else {
    const hit = cliBackendsByNode[node];
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
      cliBackends = manifest;
      cliBackendsFetchedAt = Date.now();
      // Multi-Backend RFC §8.3 D9-D15: re-apply feature gates whenever the
      // LOCAL backends manifest lands. The boot path fires fetchCLIBackends()
      // in parallel with fetchSessions, so the very first renderMainShell may
      // have run with cliBackends==null — call gates here so the input
      // controls update once the manifest is available. Remote-node fetches
      // must NOT drive feature gates (the input controls operate on the
      // locally-selected session), so this stays inside the isLocal branch.
      applyFeatureGates();
    } else if (manifest) {
      cliBackendsByNode[node] = { data: manifest, at: Date.now() };
    } else {
      // A null / malformed remote manifest must not be pinned for 60s —
      // drop any stale entry so the next open refetches (#2429).
      delete cliBackendsByNode[node];
    }
    return manifest;
  } catch (e) {
    if (!isLocal) delete cliBackendsByNode[node];
    return null;
  }
}

// renderBackendFetchFailed is the picker-slot fallback when a REMOTE node's
// backends manifest could not be fetched: a one-line notice plus a retry
// button (wired by refreshBackendPicker) instead of silently showing no
// picker as if the node had a single backend.
function renderBackendFetchFailed(node) {
  return '<span class="cp-backend-fail">' + esc(getNodeDisplayName(node)) + ' 后端清单获取失败 ' +
    '<button type="button" class="settings-syslink-btn cp-backend-retry">重试</button></span>';
}

// fetchAccessProfiles caches /api/access-profiles for 60s (same policy as
// fetchCLIBackends). Returns {profiles:[...], default} or null on error / when
// no profiles are configured (single-auth deployments — the picker/chip then
// stay hidden). RFC project-access-profile §8.
async function fetchAccessProfiles() {
  if (accessProfiles && Date.now() - accessProfilesFetchedAt < 60000) {
    return accessProfiles;
  }
  try {
    const data = await fetchJSON(NZ_CONTRACT.API.access_profiles, {credentials: 'same-origin'});
    accessProfiles = data && Array.isArray(data.profiles) ? data : null;
    accessProfilesFetchedAt = Date.now();
    return accessProfiles;
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
  if (!accessProfiles || !Array.isArray(accessProfiles.profiles)) return null;
  if (accessProfiles.profiles.length <= 1) return null; // single-auth mode
  if (!profileID) return null; // global default → no chip (matches "no overlay")
  const entry = accessProfiles.profiles.find(p => p && p.id === profileID);
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

// renderNodePicker returns an HTML fragment for a connection (node) <select>,
// or an empty string when only the local node is connected (no meaningful
// choice to offer). It took over the modal slot the agent picker used to
// occupy: the sidebar node selector was removed, so choosing which connection
// a new session lives on now happens here, at creation time. Mirrors
// renderBackendPicker's shape — single <select id="new-node"> consumed by
// getSelectedNode() at submit time — and pre-selects the current selectedNode
// so the picker matches whatever the operator was last working on.
//
// 'local' is always pinned first (defensive: even if the server's nodes
// payload omits it). Remotes follow, ordered by display name.
function renderNodePicker() {
  if (!isMultiNode()) return '';
  const ids = Object.keys(nodesData);
  if (ids.indexOf('local') === -1) ids.unshift('local');
  const sorted = ids.slice().sort((a, b) => {
    if (a === 'local' && b !== 'local') return -1;
    if (b === 'local' && a !== 'local') return 1;
    return getNodeDisplayName(a).localeCompare(getNodeDisplayName(b));
  });
  const current = selectedNode || 'local';
  const options = sorted.map(id => {
    const selected = id === current ? ' selected' : '';
    const status = getNodeStatus(id);
    const label = getNodeDisplayName(id) + ' · ' + statusLabelForNode(status);
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
// current selectedNode (then 'local') when the picker isn't rendered — i.e.
// single-connection setups where there is nothing to choose.
function getSelectedNode() {
  const el = document.getElementById('new-node');
  const v = el && el.value ? el.value : '';
  return v || selectedNode || 'local';
}

// wireNodePicker attaches a change listener to the modal's #new-node <select>
// (added imperatively to stay clear of CSP's inline-handler ban). Picking a
// connection updates the global selectedNode + persists it, then runs the
// caller's onChange so a palette can re-filter its project list to the newly
// chosen node. No-op when the picker isn't present (single-node setups).
function wireNodePicker(onChange) {
  const el = document.getElementById('new-node');
  if (!el) return;
  el.addEventListener('change', function () {
    selectedNode = el.value || 'local';
    try { localStorage.setItem('nz_selectedNode', selectedNode); } catch (_) { /* noop */ }
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
  //
  // Fetch the backend manifest for the CURRENTLY-SELECTED node so the picker
  // pre-selects that node's default backend, not the primary's (picker
  // node-aware fix). A node switch inside the modal re-fetches + repaints the
  // picker via refreshBackendPicker below.
  Promise.all([fetchCLIBackends(selectedNode), fetchAccessProfiles()]).then(([backendsData, profilesData]) => {
    // defaultWorkspace 来自 local stats，远程节点没有对应的 client 端字段，
    // 因此选中 remote 时不预填路径，让用户显式输入远程上的工作目录。
    const isLocal = (selectedNode || 'local') === 'local';
    const ws = isLocal ? (defaultWorkspace || '') : '';
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
          const nowLocal = (selectedNode || 'local') === 'local';
          const ph = nowLocal ? (defaultWorkspace || '') : '';
          wsEl.placeholder = ph;
          if (!wsEl.value) wsEl.value = ph;
        }
        refreshBackendPicker('new-backend-slot');
      });
      // First-open path: a failed REMOTE manifest arrives here as null and
      // renderBackendPicker(null) painted an empty slot. Route through
      // refreshBackendPicker so the retry notice shows on open too (#2429).
      if (!backendsData && (selectedNode || 'local') !== 'local') refreshBackendPicker('new-backend-slot');
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
  const reqNode = selectedNode;
  fetchCLIBackends(reqNode).then(backendsData => {
    // The modal may have closed while the fetch was in flight.
    if (!document.getElementById(slotId)) return;
    // Drop a stale response whose node no longer matches the selection.
    if (selectedNode !== reqNode) return;
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
  const accessProfilePicker = renderAccessProfilePicker(profilesData || accessProfiles);
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
  // scoped to selectedNode (nodeFilteredProjects), so a node switch must
  // repaint it against the current query. The backend picker also repaints
  // against the newly-selected node's manifest (picker node-aware fix).
  wireNodePicker(function () {
    renderPaletteList(state, input.value);
    refreshBackendPicker('cp-backend-slot');
  });
  // First-open path: see the no-projects modal above — a null remote
  // manifest must surface the retry notice, not an empty slot (#2429).
  if (!backendsData && (selectedNode || 'local') !== 'local') refreshBackendPicker('cp-backend-slot');
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
  // Palette is scoped to selectedNode: switching the node selector retargets
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
    //           entries (dir_mtime 0) sort last, then original projectsData
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
      // Final stable tiebreak: original projectsData order (input index).
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
  return el;
}

// quickRowHint is the 「快速新建」 subtitle for the selected node. Only the
// LOCAL default workspace is known client-side (stats.default_workspace);
// a remote node resolves its own default on dispatch, so name the node
// instead of echoing the local path (#2429).
function quickRowHint(node) {
  if (!node || node === 'local') return defaultWorkspace ? shortPath(defaultWorkspace) : '';
  return getNodeDisplayName(node) + ' · 默认工作区';
}

function buildQuickRow(idx) {
  const hint = quickRowHint(selectedNode || 'local');
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
  // selectedNode (wireNodePicker keeps it current), so renderNodePicker below
  // pre-selects it without an explicit hand-off.
  const preselectedBackend = getSelectedBackend();
  const preselectedProfile = getSelectedAccessProfile();
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (overlay) overlay.remove();
  // 选中 remote 节点时不用 local 的 defaultWorkspace 占位符，避免误导用户。
  const isLocal = (selectedNode || 'local') === 'local';
  const ws = isLocal ? (defaultWorkspace || '') : '';
  const prefill = initialValue && (initialValue.startsWith('/') || initialValue.startsWith('~')) ? initialValue : '';
  // Re-render the access-profile + backend + connection pickers inside the
  // modal and pre-select the palette's choices, so switching to Custom
  // Workspace doesn't drop any of them. The backend picker is emitted from
  // the per-node cache (cliBackendsByNode) for the currently-selected node,
  // falling back to the local cliBackends for the boot window before the
  // per-node fetch resolves; refreshBackendPicker below repaints it against
  // the authoritative manifest and on every node switch (picker node-aware
  // fix).
  const accessProfilePicker = renderAccessProfilePicker(accessProfiles, { selectedId: preselectedProfile });
  const seedBackends = (selectedNode && selectedNode !== 'local' && cliBackendsByNode[selectedNode])
    ? cliBackendsByNode[selectedNode].data
    : cliBackends;
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
      const nowLocal = (selectedNode || 'local') === 'local';
      wsEl.placeholder = nowLocal ? (defaultWorkspace || '') : '';
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
  sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + sessionCounter;
  // Continue → backend-provided stable key (precise continuation); new →
  // fresh timestamp key (independent parallel session). resolveSessionKey
  // also falls back to a timestamp key when no stableKey is available.
  const key = resolveSessionKey(mode, stableKey, projectName, agent, ts);

  sessionWorkspaces[key] = projectPath;
  if (nodeId && nodeId !== 'local') sessionNodes[key] = nodeId;
  if (backend) sessionBackends[key] = backend;
  if (accessProfile) sessionAccessProfiles[key] = accessProfile;
  // Durably persist the pending workspace and eagerly bind it server-side so a
  // reload-before-first-send (the proven cwd-fallback trigger) no longer drops
  // the workspace. This is the primary fix path (project palette open).
  persistPending();
  eagerBindWorkspace(key, projectPath, nodeId);

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = nodeId || 'local';
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  lastEventTime = 0;
  mobileEnterChat();
  setActiveSessionCard(key, selectedNode);
  renderMainShell();
  navRebuild();
  lastVersion = 0;
  debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

function doCreateSession() {
  const workspace = document.getElementById('new-workspace').value.trim();
  const backend = getSelectedBackend();
  const accessProfile = getSelectedAccessProfile();
  const agent = getSelectedAgent();
  // Read the connection picker directly so the choice lands even if the
  // change listener never fired (e.g. the operator never re-opened the
  // select). getSelectedNode falls back to selectedNode / 'local'.
  const targetNode = getSelectedNode();
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'session') : 'session';
  document.querySelector('.modal-overlay').remove();

  sessionCounter++;
  const now = new Date();
  const ts = localDateStamp(now) + '-' + sessionCounter;
  // R110-P3 key schema (see buildDashboardSessionKey godoc): 4 segments
  // with agentID as the terminal segment so buildSessionOpts picks up the
  // right AgentOpts entry.
  const key = buildDashboardSessionKey(ts, folderName, agent);

  if (workspace) sessionWorkspaces[key] = workspace;
  if (backend) sessionBackends[key] = backend;
  if (accessProfile) sessionAccessProfiles[key] = accessProfile;
  if (targetNode !== 'local') sessionNodes[key] = targetNode;
  // Persist + eager-bind so the custom workspace survives a reload-before-send.
  persistPending();
  if (workspace) eagerBindWorkspace(key, workspace, targetNode);

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = targetNode;
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
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
  const ts = localDateStamp(now) + '-' + sessionCounter;
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

// renderSettingsView paints the standalone settings top-level view into
// #settings-main. Currently a single section: theme (tri-state, reusing
// applyTheme/THEME_ORDER/THEME_LABELS). Theme buttons are wired via event
// delegation — no inline onclick (the HTML inline-handler cap is 0).
// Re-rendered on each theme click to refresh the active state.
function renderSettingsView() {
  const root = document.getElementById('settings-main');
  if (!root) return;
  const cur = getCurrentTheme();
  const themeBtns = THEME_ORDER.map(function (t) {
    return '<button type="button" class="settings-theme-opt' + (t === cur ? ' active' : '') +
      '" data-theme="' + esc(t) + '" aria-pressed="' + (t === cur ? 'true' : 'false') + '">' +
      esc(THEME_LABELS[t]) + '</button>';
  }).join('');
  // 关于 section (ui-polish-light-theme D3/D8): build identity moved here
  // from the Home health strip — version tags answer "what am I running",
  // a settings question, not a "问点什么" one. Sourced from the same
  // /api/sessions stats snapshot; rows render only when the field is
  // present so a cold view before the first poll stays clean.
  const s = lastStatsSnapshot || {};
  const aboutRows = [];
  if (s.version_tag) aboutRows.push(['naozhi', s.version_tag]);
  if (s.cli_name) aboutRows.push([s.cli_name, s.cli_version || '—']);
  if (cliBackends && Array.isArray(cliBackends.backends) && cliBackends.backends.length > 0) {
    aboutRows.push(['Backends', cliBackends.backends.map(function (b) {
      return (b && b.id) || '?';
    }).join(' · ')]);
  }
  const aboutHtml = aboutRows.length === 0 ? '' :
    '<section class="settings-sec"><h2>关于</h2>' +
      aboutRows.map(function (r) {
        return '<div class="settings-about-line"><span class="settings-about-key">' + esc(r[0]) + '</span><span>' + esc(r[1]) + '</span></div>';
      }).join('') +
    '</section>';
  // 系统任务 entry (ui-polish-light-theme D9): the mobile tab bar drops the
  // 系统 tab (6 tabs was over budget for 390px) — this row is its replacement
  // entry point. Hidden on desktop via CSS (.settings-syslink) where the rail
  // tab remains.
  const sysLinkHtml =
    '<section class="settings-sec settings-syslink"><h2>系统任务</h2>' +
      '<button type="button" class="settings-syslink-btn" id="settings-open-system">查看内置后台守护运行状态 ›</button>' +
    '</section>';
  root.innerHTML =
    '<div class="settings-head"><h1>设置</h1></div>' +
    '<div class="settings-body">' +
      '<section class="settings-sec"><h2>主题</h2>' +
        '<div class="settings-theme" id="settings-theme-group" role="group" aria-label="主题">' + themeBtns + '</div>' +
      '</section>' +
      aboutHtml +
      sysLinkHtml +
    '</div>';
  const grp = document.getElementById('settings-theme-group');
  if (grp) grp.addEventListener('click', function (e) {
    const b = e.target.closest('.settings-theme-opt');
    if (!b) return;
    applyTheme(b.dataset.theme, true); // persist=true: user-initiated → save to server
    renderSettingsView(); // refresh active state
  });
  const sysBtn = document.getElementById('settings-open-system');
  if (sysBtn) sysBtn.addEventListener('click', function () { setActivityView('system'); });
}

/* ===== WebSocket Connection Manager ===== */

const WS_STATES = { OFF: 'off', CONNECTING: 'connecting', AUTH: 'authenticating', CONNECTED: 'connected', DISCONNECTED: 'disconnected' };

// emitCron dispatches a cron-view command over nz.bus (#2557 PR-E1):
// dashboard's WS core no longer calls cron_view functions through the
// window bridge — cron subscribes to these events at module init.
// EventTarget dispatch is synchronous, so ordering semantics match the
// old direct calls; if cron_view ever failed to load, dispatch is a no-op
// (same resilience the old typeof guards bought).

// Late-bound intra-module hooks (#2557 PR-E3): these used to be IIFE
// self-exports on window; they are module-scope lets now, assigned when the
// owning IIFE runs and read at event time (never at load time).
let openLightboxGroup = null;
let openLightboxFromThumb = null;
let getActiveScratchKey = null;
let closeScratchDrawer = null;
let askAside = null;

const emitCron = (type, detail) => nzBus.dispatchEvent(new CustomEvent(type, { detail }));

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
          emitCron('cron:live-status', this.cronLive.status);
          break;
        }
        // Server confirmed subscription — apply authoritative state
        this.subscribedKey = this._pendingSubscribeKey || msg.key;
        // 非 pending 时以帧自带的 node 为准，不退到 'local'：relay 重建远端订阅
        // (remoteDropped) 或 reconnect 后，远端 subscribed 经 relay 扇出给该 key
        // 下所有 tab（relay 每帧注入 node，reverseconn 也带 Node）。非 pending 的
        // tab 若被改写成 'local'，之后 subscription_timeout 处理要求 node 匹配就
        // 不再清簿记 → 不重订阅，原 bug 复现。
        this.subscribedNode = this._pendingSubscribeNode || msg.node || 'local';
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
      case 'unsubscribed':
        // Server ack for an explicit unsubscribe (wshub_subscribe.go, three
        // emit sites incl. the relayed remote ack). wsm.unsubscribe() already
        // cleared subscribedKey/Node synchronously and a relayed ack may name
        // a key this tab no longer tracks — nothing to reconcile. Listed so
        // the frame is a documented no-op rather than an unhandled type.
        break;
      case 'error':
        // cron-live RFC §2.2: 错误命中 cron live pending → 单独清理
        if (msg.key && this.cronLive.pendingJobId && msg.key === ('cron:' + this.cronLive.pendingJobId)) {
          this.cronLive.pendingJobId = null;
          this.cronLive.subscribedKey = null;
          this.cronLive.status = 'stopped';
          emitCron('cron:live-status', 'stopped');
          break;
        }
        // PurgeNodeSubscriptions broadcast: error{node, "node disconnected"}
        // reaches every tab regardless of what it is subscribed to. Drop only
        // the bookkeeping that points at the dead node, snap selectedNode back
        // to local via the existing reconcile path, and re-fetch so the
        // sidebar reflects the node's sessions going away.
        if (!msg.key && msg.node && msg.error === 'node disconnected') {
          if (this.subscribedNode === msg.node) {
            this.subscribedKey = null;
            this.subscribedNode = null;
          }
          if (this._pendingSubscribeNode === msg.node) {
            this._pendingSubscribeKey = null;
            this._pendingSubscribeNode = null;
          }
          // The selected session lived on the dead node: no pushes can reach
          // it any more and its key means nothing under the `local` node
          // reconcileSelectedNode snaps to, so deselect it (the backend's
          // "deselect stale sessions" contract) before selectedNode moves.
          // Ownership comes from the session store, NOT from selectedNode:
          // that global is the dispatch target and wireNodePicker rewrites
          // it the moment the new-session picker changes node, so a local
          // session with the picker on n1 must survive n1 going away.
          // Pending (never-sent) sessions are only a draft target — they stay
          // selected and are neither cleared nor deleted here.
          if (selectedKey && sessionWorkspaces[selectedKey] === undefined &&
              (sessionsData[sid(selectedKey, msg.node)] || sessionNodes[selectedKey] === msg.node)) {
            deselectNodeSession(msg.node);
          }
          nodesData = Object.fromEntries(Object.entries(nodesData).filter(([id]) => id !== msg.node));
          reconcileSelectedNode();
          lastVersion = 0;
          debouncedFetchSessions();
          break;
        }
        // Subscribe failed (e.g. session not found yet) — reset pending, but
        // only when the frame is about THIS subscribe: a keyed error for a
        // different key (or an agent_subscribe validation error, which also
        // arrives as a bare `error`) must not wipe an unrelated in-flight
        // subscribe. Keyless frames without a node are the legacy shape of a
        // subscribe rejection and still clear pending.
        if (!msg.key || msg.key === this._pendingSubscribeKey) {
          this._pendingSubscribeKey = null;
          this._pendingSubscribeNode = null;
        }
        break;
      case 'history':
        if (nzState.isCronLiveKey && nzState.isCronLiveKey(msg.key)) { this.onCronLiveHistory(msg); break; }
        this.onHistory(msg);
        break;
      case 'event':
        if (nzState.isCronLiveKey && nzState.isCronLiveKey(msg.key)) { this.onCronLiveEvent(msg); break; }
        this.onEvent(msg);
        break;
      case 'send_ack':
        this.onSendAck(msg);
        break;
      case 'send_error':
        this.onSendError(msg);
        break;
      case 'interrupt_ack':
        this.onInterruptAck(msg);
        break;
      case 'session_state':
        if (nzState.isCronLiveKey && nzState.isCronLiveKey(msg.key)) { this.onCronLiveSessionState(msg); break; }
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
        emitCron('cron:run-started', msg);
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
        emitCron('cron:run-ended', msg);
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
        if (msg && msg.job_id) emitCron('cron:timeline-refresh-head', msg.job_id);
        break;
      case 'daemon_run_started':
      case 'daemon_run_ended':
        // System-daemon (sysession) run boundary. fetchSystemDaemons is the
        // only path that updates the 系统 rail attention badge; without this
        // case it ran solely at boot / on panel open / on the 5s poll while
        // the system view is active, so a daemon failing in the background
        // never lit the badge until the operator happened to open the view.
        //
        // This is a hub-wide broadcast (sysession ticks ~every 30s → a few
        // frames per minute per tab). Honour the RNEW-UX-014 hidden-tab
        // suspension: skip the fetch while hidden; startPollers re-fetches
        // once on the visibilitychange back to visible so the badge catches up.
        if (document.hidden) break;
        fetchSystemDaemons().then(() => {
          if (activeView === 'system') renderSystemView();
        }).catch(() => {});
        break;
      case 'pong':
        break;
      // RFC v4 agent-team-ui §3.5.2 — drill-in flow. All four handlers
      // live in agent_view.js so new agent-view functionality doesn't
      // mean touching this dispatch table.
      case 'agent_event':
        if (nzViews.agent) nzViews.agent.onAgentEvent(msg);
        break;
      case 'agent_meta':
        if (nzViews.agent) nzViews.agent.onAgentMeta(msg);
        break;
      case 'agent_done':
        if (nzViews.agent) nzViews.agent.onAgentDone(msg);
        break;
      case 'agent_subscribe_rejected':
        if (nzViews.agent) nzViews.agent.onAgentSubscribeRejected(msg);
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
    emitCron('cron:live-status', 'pending');
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
      // cron_view is an ES module (D3 PR-C1): its cronJobs binding is reached
      // through the nz.state accessor it registers, not a bare global.
      const job = Array.isArray(nzState.cronJobs)
        ? nzState.cronJobs.find(j => j && j.id === jobId)
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
    } else {
      emitCron('cron:live-ensure-subscription');
    }
  },

  onHistory(msg) {
    if (msg.key !== selectedKey || (msg.node || 'local') !== selectedNode) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const events = msg.events || [];
    // 初始帧判别以服务端的 initial 标记为准（ServerMsg.Initial），不看到达
    // 顺序，也不看 'subscribed' ack 是否已回来 —— 两者都不可靠：
    //   * reverseconn 首订阅路径的 history 由本地 FetchEvents goroutine 发出，
    //     ack 却要经远端 readLoop 绕回来，该调用点明确写了两者顺序不定；
    //   * 被顶替订阅的 eventPushLoop 是独立 goroutine，unsub() 不排空在途的
    //     backfill，它的帧可以落在新 ack 的前面或后面。
    // 旧逻辑"第一个到达的 history 帧就是初始帧"因此会把零星几条增量事件当整页
    // 渲染，并把 lastRenderedEventTime 推到最新，随后真正的初始帧走增量路径被
    // 时间戳守卫整批丢弃 —— 即"运行中重复点击会话后消息丢失/顺序错乱"。
    // backfill 帧不带 initial 标记，所以无论何时落地都只走增量 append，
    // _initialSubscribe 留给真正的初始帧消费。
    const isInitial = this._initialSubscribe && msg.initial === true;
    // 只有真正消费了初始帧才清标记（旧代码无条件清，是上述丢帧的直接原因）。
    if (isInitial) this._initialSubscribe = false;

    // Rebuild the answered-set from history BEFORE rendering so card
    // re-renders show the correct locked state. The Set is in-memory so
    // a page reload or session switch would otherwise make an already-
    // answered card re-actionable and invite duplicate answers to CC.
    hydrateAskAnsweredFromHistory(events);

    const display = processEventsForDisplay(events);

    if (isInitial) {
      // Full render replaces everything — remove any optimistic messages
      const html = renderEventsWithDividers(display, 0);
      // Decide "load earlier" mounting BEFORE the all-internal placeholder so
      // its copy never invites a click on a button that won't appear. Mount off
      // the server's has_more flag \u2014 it knows the frame was truncated by
      // visible-bubble count (DefaultVisibleTarget), catching the case the old
      // length heuristic missed: more visible bubbles than the target but fewer
      // total events than INITIAL_HISTORY_LIMIT. Fall back to the length
      // heuristic only for older servers / relayed nodes that don't set
      // has_more (the field is absent \u2192 undefined).
      const wsHasMore = (typeof msg.has_more === 'boolean') ? msg.has_more : null;
      const showEarlier = (wsHasMore === true) ||
        (wsHasMore == null && events.length >= INITIAL_HISTORY_LIMIT);
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
        // looking broken. Invite "click below" only when the button will
        // mount; otherwise the whole history is internal activity with nothing
        // older to reach, so promise nothing.
        el.innerHTML = showEarlier
          ? '<div class="empty-state">\u8be5\u4f1a\u8bdd\u6700\u8fd1\u4ec5\u6709 agent \u6d3b\u52a8\uff0c\u70b9\u51fb\u4e0b\u65b9\u52a0\u8f7d\u66f4\u65e9\u7684\u6d88\u606f</div>'
          : '<div class="empty-state">\u8be5\u4f1a\u8bdd\u4ec5\u6709 agent \u6d3b\u52a8\uff0c\u6682\u65e0\u5bf9\u8bdd\u6d88\u606f</div>';
      }
      // Reset dedup tracker on full render and anchor the pagination
      // cursor to the earliest event we received, independent of DOM
      // contents so loadEarlierEvents still works after a fully-filtered
      // page.
      //
      // 水位无条件重置：整页替换后 lastRenderedEventTime 只能描述"这一页渲染了
      // 什么"。空 Initial 帧（running 会话刚起进程，completeSubscribe 的空帧臂）
      // 也必须把水位归零 —— 否则被顶替订阅的 stale 增量帧先到把水位推高、空
      // Initial 帧把面板重置成加载占位符但水位没动，新 pushLoop 推同批事件时
      // 全部撞上 `e.time <= lastRenderedEventTime` 被整批丢弃。
      lastRenderedEventTime = events.length ? (events[events.length - 1].time || 0) : 0;
      if (events.length > 0) {
        const first = events[0];
        if (first.time && (oldestFetchedEventTime === 0 || first.time < oldestFetchedEventTime)) {
          oldestFetchedEventTime = first.time;
        }
      }
      if (showEarlier) {
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
        // Strictly-older events were rendered by an earlier frame. Same-ms
        // events must NOT be dropped on time alone: one CLI frame's blocks
        // (thinking + text, process_event_format.go) and ACP's trailing
        // thinking/text/result share one millisecond, and eventHtml(thinking)
        // renders nothing while the cursor below still advances — so `<=`
        // swallowed the text bubble that followed. Same-ms replays (the backend
        // re-admits the watermark ms, #2402) are dropped by uuid instead: a
        // same-time same-uuid pair is always the same entry (RFC dashboard-
        // event-uuid-idempotent-render §3). Newer-time events keep their
        // append behaviour untouched, so streaming text is never frozen.
        if (e.time && e.time < lastRenderedEventTime) return;
        if (e.time && e.time === lastRenderedEventTime && eventAlreadyRendered(el, e.uuid)) return;
        if (e.type === 'user') {
          // uuid idempotency for user bubbles: the time-cursor guard above
          // misses the bug case (onEvent rendered the real user event but a
          // restart re-subscribe replays it before the cursor advanced), so
          // dedup on the authoritative uuid as the backstop. User-only by
          // design — streaming text re-emits the same uuid (RFC §3).
          if (eventAlreadyRendered(el, e.uuid)) {
            if (e.time && e.time > lastRenderedEventTime) lastRenderedEventTime = e.time;
            return;
          }
          const opt = el.querySelector('.optimistic-msg');
          if (opt) opt.remove();
          sawUser = true;
          lockRenderedAskCards(el);
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
        paintTurnElapsed();
        turnState.timerId = setInterval(paintTurnElapsed, 1000);
      }
      for (let i = turnStart; i < events.length; i++) {
        applyEventToTurnState(events[i]);
      }
    } else {
      // Incremental: accumulate additively, reset only on turn boundaries
      for (let i = 0; i < events.length; i++) {
        const ev = events[i];
        if (ev.type === 'user') {
          resetTurnStateForUserEcho();
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
            // ev.cost is the CLI's per-incarnation cumulative total, which
            // RESETS on resume. The authoritative session total is the
            // monotonic delta-sum the server ships as total_cost on the next
            // snapshot poll; never let this optimistic bump regress below it
            // (post-resume ev.cost is lower than the carried-over total).
            if (sessionsData[sKey] && ev.cost > (sessionsData[sKey].total_cost || 0)) {
              sessionsData[sKey].total_cost = ev.cost;
            }
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
  },

  onEvent(msg) {
    if (msg.key !== selectedKey || (msg.node || 'local') !== selectedNode) return;
    // Cron timed_out / failed 终态后丢弃后续 ghost 事件（CLI 子进程
    // 在 deadline 命中后还会再吐 result，但 cron run 已记录为终态，
    // 继续追加只会让用户看到"超时但还在工作"的分裂视觉）。
    if (nzState.isCronSessionFrozen && nzState.isCronSessionFrozen(msg.key)) return;
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
      resetTurnStateForUserEcho();
      // A user message after an AskUserQuestion means it was answered on some
      // surface — lock the card before the bubble lands (#2430).
      lockRenderedAskCards(document.getElementById('events-scroll'));
    } else if (ev.type === 'result') {
      if (ev.cost) {
        const sKey = sid(selectedKey, selectedNode);
        // total_cost still feeds the Home/recent-sessions aggregate; the header
        // cost chip itself was removed (see renderMainShell).
        //
        // ev.cost is the CLI's per-incarnation cumulative total, which RESETS
        // on resume. The authoritative session total is the monotonic
        // delta-sum the server ships as total_cost; never let this optimistic
        // bump regress below it (post-resume ev.cost is lower than the carried
        // total). In-process the two agree (deltas sum to the cumulative).
        if (sessionsData[sKey] && ev.cost > (sessionsData[sKey].total_cost || 0)) {
          sessionsData[sKey].total_cost = ev.cost;
        }
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
    if (nzViews.agent && nzViews.agent.activeTaskID()) return;
    const html = eventHtml(ev);
    if (!html) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const empty = el.querySelector('.empty-state');
    if (empty) empty.remove();
    const isUser = ev.type === 'user';
    if (isUser) {
      // uuid idempotency (user bubbles only): a duplicate push or a
      // post-restart re-subscribe history replay must not paint the same user
      // message twice. The real bubble appended below carries data-uuid
      // (eventHtml), so once it is on screen any later replay of the same uuid
      // is caught here; advance the event-time cursor so the time-gated
      // onHistory path stays consistent, then bail before re-appending.
      // Scope is user-only by design: streaming text re-emits the same uuid
      // many times (RFC §3 — 586 dup uuids measured), so text/tool events must
      // keep their existing append behaviour and never dedup by uuid here.
      if (eventAlreadyRendered(el, ev.uuid)) {
        const t = ev.time || 0;
        if (t && t > lastRenderedEventTime) lastRenderedEventTime = t;
        return;
      }
      // First arrival of the real user event: drop the optimistic placeholder.
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
    // Advance the event-time cursor on first render too, matching appendEvents
    // (3159) and onHistory (10745). Without this, a synthetic CLI history entry
    // with no uuid (pre-uuid events) leaves the cursor un-advanced after the WS
    // push path renders it, so the later time-gated onHistory replay
    // (e.time <= lastRenderedEventTime) admits it again and the uuid fallback
    // never fires → duplicate bubble (#2063).
    if (evT && evT > lastRenderedEventTime) lastRenderedEventTime = evT;
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
      removeOptimisticMsg(msg.id);
      rollbackOptimisticRunning(msg.key || selectedKey, msg.node || selectedNode);
      // send 从未真正进入 turn，别把它当成「当前 turn 的输入」残留 —— 否则
      // 下次中断会把这条从未送达的文本回填上来。
      delete sessionLastSent[sid(msg.key || selectedKey, msg.node || selectedNode)];
    } else if (msg.status === 'error') {
      // The WS send_ack error is an in-band message, not an HTTP status,
      // but treat the server-supplied `error` string the same way as an
      // HTTP 500 body: truncate + prefix with "发送消息失败：".
      showAPIError('发送消息', 500, msg.error || '');
      // Remove this send's optimistic message on send failure
      removeOptimisticMsg(msg.id);
      rollbackOptimisticRunning(msg.key || selectedKey, msg.node || selectedNode);
      delete sessionLastSent[sid(msg.key || selectedKey, msg.node || selectedNode)];
    }
  },

  // onInterruptAck surfaces a failed / no-op interrupt. interruptSession()
  // toasts "已发送中断" optimistically the moment the frame leaves the socket,
  // so a status:"error" ack (unknown node / server shutting down / remote RPC
  // failure / internal error) or "not_running" (no live process to interrupt)
  // must be reported or the operator believes the interrupt landed.
  onInterruptAck(msg) {
    if (!msg || msg.status === 'ok') return;
    if (msg.status === 'not_running') {
      showToast('会话未在运行，无需中断', 'warning');
      return;
    }
    if (msg.status === 'error') {
      showAPIError('中断会话', 500, msg.error || '');
    }
  },

  // send_error: the HTTP send path (every file-bearing send, plus the WS-down
  // fallback) has no per-request back-channel after its 202, so the server
  // fans asynchronous failures (spawn error, passthrough send failure, remote
  // node send failure) out to every subscriber of the key as this frame.
  //
  // Gate on httpSendPending / sessionLastSent: the frame reaches every tab
  // watching the key, but only the tab that actually sent the failed message
  // owns an optimistic bubble / running flip for it. A second operator's tab
  // (or this tab after it sent nothing) must ignore the frame entirely —
  // otherwise it would tear down its own legitimate optimistic state and
  // toast about a message it never sent. httpSendPending is the primary gate
  // (set for every HTTP send, image-only included); sessionLastSent is the
  // text-only secondary. When we did send: reuse the send_ack error recovery
  // (toast, drop the optimistic bubble if any, roll back running) for the
  // on-screen key, or just undo the running flip for a key we sent to and
  // then navigated away from.
  onSendError(msg) {
    if (!msg || !msg.key) return;
    const node = msg.node || 'local';
    const sKey = sid(msg.key, node);
    if (!sessionLastSent[sKey] && !httpSendPending.has(sKey)) return;
    httpSendPending.delete(sKey);
    if (msg.key === selectedKey && node === (selectedNode || 'local')) {
      this.onSendAck({ status: 'error', key: msg.key, node: msg.node, error: msg.error });
      return;
    }
    rollbackOptimisticRunning(msg.key, node);
    delete sessionLastSent[sKey];
  },

  onSessionState(msg) {
    const msgNode = msg.node || 'local';
    const sKey = sid(msg.key, msgNode);
    // Real state arrived — the optimistic flip has served its purpose, regardless
    // of whether the server says running/ready/dead. Clear the flag so future
    // turns don't short-circuit the running→ready rollback logic. Capture it
    // FIRST: the wasDead computation below needs to know whether prev.state
    // is a real server-reported 'running' or just the pre-send optimistic flip.
    const wasOptimisticRunning = !!sessionOptimisticRunning[sKey];
    const optimisticPrevState = sessionOptimisticPrevState[sKey];
    delete sessionOptimisticRunning[sKey];
    delete sessionOptimisticPrevState[sKey];
    if (_optimisticRunningTimers[sKey]) {
      clearTimeout(_optimisticRunningTimers[sKey]);
      delete _optimisticRunningTimers[sKey];
    }
    // 服务端 resubscribeEvents 60s 超时后已经丢弃了本连接对该 key 的订阅
    // (wshub_eventpush.go)，此后这条订阅不会再有任何事件帧。必须同步清掉
    // 本地订阅簿记 —— 否则客户端永远"以为自己订阅着"：下一次 running 广播
    // 到达时 needSub 的 case 1 (subscribedKey mismatch) 不成立、case 3
    // (_subscriptionSuspended) 也不成立，整轮 turn 的事件全部推空，
    // dashboard 静止直到手动重新点击会话（bug: 出结果后不自动更新）。
    // 清掉之后，下一次 running 广播经 case 1 重新订阅，拿到完整初始帧。
    if (msg.reason === 'subscription_timeout' &&
        this.subscribedKey === msg.key &&
        (this.subscribedNode || 'local') === msgNode) {
      this.subscribedKey = null;
      this.subscribedNode = null;
      this._subscriptionSuspended = false;
      this.lastEventTimeWs = 0;
    }
    const prev = sessionsData[sKey] || {};
    const prevState = prev.state;   // capture before mutation
    // wasDead 判定必须穿透乐观 running 翻转：markSessionOptimisticRunning 在
    // 网络往返之前就把 sessionsData.state 写成 'running'，所以对所有
    // dashboard 本页发起的 send，服务端真正的 running 广播到达时 prevState
    // 恒为 'running' —— 若直接读它，dead→running 的重订阅 (case 2) 对本页发送
    // 永远是死代码，恰好漏掉"进程被回收后从本页发消息"这个最常见的失联场景。
    // 用翻转前记录的真实状态还原判据。
    const effectivePrevState = wasOptimisticRunning ? optimisticPrevState : prevState;
    // 判据是 state==='dead' 本身，而不是 death_reason 是否非空。二者不等价：
    // death_reason 由 mapSendError 在 no_output_timeout / total_timeout 时写入
    // (internal/session/managed_send.go)，进程未必被回收，会话随后回到 ready
    // 却留着这个陈旧标记。按 death_reason 判会让此后每一次普通发送都命中
    // case 2，强制 lastEventTimeWs=0 全量重订阅 —— 而全量重渲染
    // (el.innerHTML = html) 会抹掉刚发出、服务端还没回显的 .optimistic-msg
    // 气泡，正是 case 3 旁边那句注释警告过的危害。sessionsData.state 保留后端
    // 真实状态（UI 层才把 dead 显示成 ready，见下方 displayState），所以
    // 'dead' 是可靠且精确的判据，对齐 case 2 注释本身的表述
    // ("subscribed but process was dead → revived")。
    const wasDead = effectivePrevState === 'dead';
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
    // The HTTP send reached a terminal state (or never became a turn): the
    // originator mark is no longer needed — drop it so it cannot linger and
    // let a much later send_error for someone else's send slip through.
    if (msg.state === 'ready' || msg.state === 'dead') httpSendPending.delete(sKey);
    // 一轮对话里 agent 很可能切了分支（git checkout / 新建 worktree 分支）。
    // 这不改 workspace 路径，所以 workspace-diff 那条失效路径不会触发，chip
    // 会一直停在选中会话那一刻的分支上。turn 边界是重新解析的自然时机：
    // 频率低（每轮一次而非定时轮询），且恰好覆盖"agent 干完活"这个分支最可能
    // 已变的时刻。invalidateGitState 内部只在该会话仍被选中时才真正发请求。
    if (turnCompleted) invalidateGitState(msg.key, msgNode);
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
      emitCron('cron:live-status', 'stopped');
    }
  },

  // cron-live RFC §5: 首批 history 帧到达。EventEntriesSince(after) 后端无条数
  // 上限（After>0 时 Limit 被忽略），前端必须自己截尾到 CRON_LIVE_MAX_EVENTS。
  onCronLiveHistory(msg) {
    if (nzState.isCronSessionFrozen && nzState.isCronSessionFrozen(msg.key)) return;
    const incoming = msg.events || [];
    if (incoming.length === 0) return;
    const lastTime = this.cronLive.lastEventTimeMs;
    // Same-ms siblings pass the time gate; same-ms replays are dropped by uuid
    // against the buffered array (mirrors onHistory's same-ms rule).
    const seen = new Set((this.cronLive.events || []).map(e => e.uuid).filter(Boolean));
    const newOnes = incoming.filter(e => !e.time || e.time > lastTime ||
      (e.time === lastTime && !(e.uuid && seen.has(e.uuid))));
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
    emitCron('cron:live-repaint');
  },

  onCronLiveEvent(msg) {
    if (nzState.isCronSessionFrozen && nzState.isCronSessionFrozen(msg.key)) return;
    const ev = msg.event;
    if (!ev) return;
    if (ev.time && ev.time < this.cronLive.lastEventTimeMs) return;
    if (ev.time && ev.time === this.cronLive.lastEventTimeMs && ev.uuid &&
        (this.cronLive.events || []).some(e => e.uuid === ev.uuid)) return;
    this.cronLive.events = this.cronLive.events || [];
    this.cronLive.events.push(ev);
    if (this.cronLive.events.length > CRON_LIVE_MAX_EVENTS) {
      this.cronLive.events.shift();
      this.cronLive.truncatedCount = (this.cronLive.truncatedCount || 0) + 1;
    }
    if (ev.time) this.cronLive.lastEventTimeMs = ev.time;
    this.cronLive.status = 'live';
    emitCron('cron:live-event', ev);
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
    // no-op since #sidebar-status DOM was deleted; selectedNode reconciliation
    // is already driven by this updateStatusBar() call.)
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
      // #2431: a hidden tab has had its pollers suspended by stopPollers;
      // re-arming here would undo that. startPollers re-arms on return.
      if (!document.hidden) discoveredPollTimer = setInterval(scanDiscovered, 30000);
      // Pull fresh node/session state immediately to clear stale data
      debouncedFetchSessions();
    } else if (s === WS_STATES.DISCONNECTED) {
      // RNEW-UX-010 — announce only on real transitions from connected, so
      // initial cold boot (OFF→CONNECTING→DISCONNECTED retry) stays silent.
      if (prev === WS_STATES.CONNECTED) announce('连接已断开，正在重试');
      // WS lost: start fallback polling — unless the tab is hidden (#2431):
      // stopPollers already suspended everything and startPollers re-arms on
      // visibilitychange from the then-current WS state.
      const visible = !document.hidden;
      if (visible && !sessionPollTimer) sessionPollTimer = setInterval(fetchSessions, 5000);
      if (discoveredPollTimer) { clearInterval(discoveredPollTimer); discoveredPollTimer = null; }
      if (visible) discoveredPollTimer = setInterval(scanDiscovered, 5000);
      if (selectedKey && !eventTimer) {
        lastEventTime = this.lastEventTimeWs;
        if (visible) eventTimer = setInterval(() => fetchEvents(false), 1000);
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

function updateHeaderCLI() {
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};
  // #2437: only touch the #header-cli span painted by headerCLILabelHtml.
  // Never rewrite the whole left container — #header-model lives next door.
  const el = document.getElementById('header-cli');
  if (!el) return;
  // Fallback chain mirrors renderMainShell — see backendDisplayName godoc
  // for why pending sessions need the sessionBackends lookup before the
  // global defaultCLIName fallback.
  const name = s.cli_name || backendDisplayName(sessionBackends[selectedKey]) || defaultCLIName;
  const version = s.cli_version || backendDisplayVersion(sessionBackends[selectedKey]) || defaultCLIVersion;
  // Same display rule as renderMainShell (D5): version lives in the hover
  // title only, the text is just the backend name.
  const text = name || '';
  const title = (name && version) ? name + ' v' + version : '';
  if (el.textContent !== text) el.textContent = text;
  if (title !== (el.getAttribute('title') || '')) {
    if (title) el.setAttribute('title', title); else el.removeAttribute('title');
  }
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
  // Invalidate any in-flight previewDiscovered(): every caller of this
  // function (selectSession, the createSession paths, a newer preview) is
  // moving the operator off the discovered panel, so a preview fetch that
  // resolves afterwards must neither paint into the now-managed
  // #events-scroll nor re-arm previewTimer.
  _previewGen++;
}

/* ===== Discovery & Takeover ===== */

// Discovered-card identity is (pid, node): pids repeat across nodes, so every
// key/lookup/removal goes through these helpers (#2431). Key shape is
// '_discovered:<pid>:<node>' — pid stays in slot 1 for parseDiscoveredPid.
function discoveredKey(pid, node) {
  return '_discovered:' + pid + ':' + (node || 'local');
}
function isDiscoveredKey(key) {
  return typeof key === 'string' && key.startsWith('_discovered:');
}
function parseDiscoveredPid(key) {
  return parseInt(key.split(':')[1], 10);
}
function sameDiscovered(d, pid, node) {
  return d.pid === pid && (d.node || 'local') === (node || 'local');
}
function findDiscovered(pid, node) {
  return discoveredItems.find(d => sameDiscovered(d, pid, node)) || null;
}
function dropDiscovered(pid, node) {
  discoveredItems = discoveredItems.filter(d => !sameDiscovered(d, pid, node));
}

async function scanDiscovered() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — /api/discovered walks the filesystem, so a
    // stalled disk shouldn't wedge the scan button forever.
    const data = await fetchJSON(NZ_CONTRACT.API.discovered, { headers, timeoutMs: 10000 });
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

async function previewDiscovered(sessionId, cwd, pid, procStartTime, node, cliName, entrypoint) {
  // Generation guard: two rapid clicks on different discovered cards both
  // pass the synchronous prologue, then the first call's awaited fetch used to
  // resolve into the SECOND card's #events-scroll and arm a second
  // setInterval without clearing the first (previewTimer was simply
  // overwritten → leaked interval appending the wrong session's events).
  // stopPreviewPolling() bumps _previewGen, so capture AFTER calling it.
  stopPreviewPolling();
  const gen = _previewGen;
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
  setActiveSessionCard(discoveredKey(pid, node), node || 'local');

  const base = cwd.split('/').pop() || cwd;
  const main = document.getElementById('main');
  main.innerHTML =
    '<div class="main-header">' +
      '<button type="button" class="btn-mobile-back" data-action="mobile-back" title="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868" aria-label="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868">' + ICONS.back + '</button>' +
      '<div class="main-header-content">' +
        '<h2>' + esc(base) + '</h2>' +
        '<div class="detail">' +
          sessionTypeTag(cliName || 'cli', entrypoint || '') +
        '</div>' +
      '</div>' +
    '</div>' +
    '<div class="events" id="events-scroll"><div class="empty-state">加载中…</div></div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button type="button" data-action="nav-msg" data-dir="prev" id="nav-prev" title="\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2191)" aria-label="\u8df3\u5230\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navUp + '</button>' +
      '<span class="nav-counter" id="nav-counter" data-action="nav-show-list" title="\u70b9\u51fb\u67e5\u770b\u5168\u90e8\u7528\u6237\u6d88\u606f"></span>' +
      '<button type="button" data-action="nav-msg" data-dir="next" id="nav-next" title="\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2193)" aria-label="\u8df3\u5230\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navDown + '</button>' +
    '</div>' +
    '<div class="input-area" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message to take over..." data-action-keydown="msg-input-key" data-action-compositionend="msg-input-compend"></div>' +
        '<button type="button" class="btn-icon btn-send" id="btn-send" data-action="msg-send" title="发送" aria-label="发送消息">' + ICONS.send + '</button>' +
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
      events = await fetchJSON(NZ_CONTRACT.API.discovered_preview + '?session_id=' + encodeURIComponent(sessionId) + nodeParam + cwdParam, { headers, timeoutMs: 10000 });
    } catch (err) {
      if (gen !== _previewGen) return;
      const errText = err.message || '';
      const el0 = document.getElementById('events-scroll');
      if (el0) el0.innerHTML = '<div class="empty-state">' + esc(errText || '预览失败') + '</div>';
      if (err.status) showAPIError('预览会话', err.status, errText);
      return;
    }
    // A newer previewDiscovered(), selectSession() or createSession() (all of
    // which run stopPreviewPolling → _previewGen++) may have superseded this
    // call while the fetch was in flight. The managed-session panel reuses the
    // #events-scroll id, so an element check alone is not enough — never
    // paint into someone else's panel.
    if (gen !== _previewGen) return;
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
    // previewTimer is provably null here: it is only ever armed below, after
    // this generation check, and any older generation's interval was cleared
    // by the stopPreviewPolling() in our own prologue. Do NOT call
    // stopPreviewPolling() at this point — it would bump _previewGen and
    // invalidate this very call.
    previewEventCount = events.length;
    const capturedSid = sessionId;
    // #1770: guard against overlapping ticks. Each tick re-fetches the full
    // preview event list; on a slow link a fetch can outlast the 2s interval,
    // so without this flag consecutive ticks pile up concurrent requests.
    // Mirrors _fetchEventsInFlight on the main events poll.
    let previewInFlight = false;
    previewTimer = setInterval(async () => {
      // A newer previewDiscovered() already cleared this interval in its
      // prologue; the check is defence-in-depth against a tick that was
      // queued before clearInterval landed.
      if (gen !== _previewGen) return;
      if (previewInFlight) return;
      previewInFlight = true;
      try {
        const headers2 = {};
        const t2 = getToken();
        if (t2) headers2['Authorization'] = 'Bearer ' + t2;
        const r2 = await fetch(NZ_CONTRACT.API.discovered_preview + '?session_id=' + encodeURIComponent(capturedSid) + nodeParam + cwdParam, { headers: headers2 });
        if (!r2.ok) return;
        const all = await r2.json();
        if (gen !== _previewGen) return;
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

/* ===== Cron Tab =====
   The cron (定时任务) view was extracted to static/cron_view.js (PR-1,
   RFC dashboard-cron-view-extraction). It loads as a plain <script defer>
   after this file, so its top-level functions / state stay in the shared
   global scope. dashboard.js's WebSocket core still calls cron functions
   (setCronLiveStatus / isCronLiveKey / cronApplyRunStarted / …) and cron
   calls back into dashboard.js globals — both work because everything is
   global. Shared helpers appendEventsToContainer() / authHeaders() that
   happened to live in this region moved with it and remain global. */

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
        '<button type="button" data-action="onboarding-dismiss">稍后再说</button>' +
        '<button type="button" class="primary" data-action="onboarding-create">立即创建会话</button>' +
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
// RFC project-access-profile §8.3: fire at boot so the session-card chip has
// profile metadata (label/colour) on the first renderSidebar. Failure /
// single-auth deployments still work — the chip helpers return '' when
// accessProfiles is null.
fetchAccessProfiles();
fetchSessions().then(maybeShowOnboarding);
sessionPollTimer = setInterval(fetchSessions, 5000);
scanDiscovered();
discoveredPollTimer = setInterval(scanDiscovered, 30000);
// fetchCronJobs() bootstrap moved to cron_view.js tail (PR-1).
fetchSystemDaemons().catch(function () {}); // prime the 系统 rail badge
startSidebarTimeTick();
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
    stopSidebarTimeTick();
    // #1770: also pause the WS keep-alive ping while the tab is hidden. The
    // 30s app-level ping wakes the mobile radio every 30s for nothing —
    // connection liveness is independently maintained by the server's
    // protocol-level Ping/Pong (writePump, wsPingPeriod≈54s), so dropping the
    // app ping loses no liveness detection. Re-armed in startPollers on resume.
    if (wsm && wsm.pingTimer) wsm.cleanup();
  };
  const startPollers = () => {
    if (!sessionPollTimer) {
      // #2431: a half-open socket (lid close / network switch) still reports
      // CONNECTED, so no frames arrive and no fallback poll is armed below;
      // the version gate would then short-circuit this one-shot fetch too.
      // Zero lastVersion so returning to the tab always repaints once.
      lastVersion = 0;
      fetchSessions(); // immediate refresh on resume so UI is not stale
      // #2431: the 5 s sessions poll is a WS-outage fallback. Over a live
      // socket session_state pushes already drive the sidebar; arming the
      // interval here made it run alongside WS until the next reconnect.
      if (!(wsm && wsm.state === WS_STATES.CONNECTED)) {
        sessionPollTimer = setInterval(fetchSessions, 5000);
      }
    }
    // Same rationale as the turn-boundary refresh in onSessionState: the branch
    // can change without the workspace path changing, and an operator switching
    // branches in their own terminal produces no turn at all. Re-resolving when
    // the tab regains focus catches that case without a dedicated poller.
    if (selectedKey) invalidateGitState(selectedKey, selectedNode);
    if (!discoveredPollTimer) {
      discoveredPollTimer = setInterval(scanDiscovered, 30000);
    }
    // Relative-time labels drifted while hidden; startSidebarTimeTick
    // refreshes them once immediately before re-arming the 60s tick.
    startSidebarTimeTick();
    // daemon_run_* WS frames are ignored while hidden (see wsm.onMessage), so
    // re-sync the 系统 rail badge once on return.
    fetchSystemDaemons().catch(function () {});
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
      showToast('页面遇到异常，可能需要刷新：' + msg, 'warning', 4000);
    } catch (_) { /* last-resort: never throw from the error handler */ }
  }
  window.addEventListener('error', handle, true);
  window.addEventListener('unhandledrejection', handle);
})();

initMobile();
initViewportTracking();
initSwipeDelete();
initSwipeBack();
(function(){
  var ov=document.createElement('div');ov.className='lightbox-overlay';
  ov.setAttribute('role','dialog');ov.setAttribute('aria-modal','true');ov.setAttribute('aria-label','Image preview');
  // tabindex=-1 lets openLightboxGroup move focus onto the overlay, so the
  // ←/→/r/+/- shortcuts work even when the click left focus in the chat
  // textarea (the keydown handler skips editable elements by design).
  ov.setAttribute('tabindex','-1');
  // Toolbar buttons sit absolute top-right; rotation buttons trigger 90° steps,
  // zoom buttons give pointer-only users (touch device + mouse without wheel)
  // a clickable zoom entry. The overlay's click-to-close handler ignores events
  // that didn't target the overlay itself, so toolbar clicks won't propagate up
  // and dismiss the modal. The prev/next rails + counter belong to the gallery
  // group model (RFC lightbox-gallery-nav): hidden via .lb-single when the
  // group holds one image.
  ov.innerHTML='<div class="lb-toolbar">'
    +'<button type="button" class="lb-tool-btn" data-lb-action="zoom-out" aria-label="Zoom out" title="缩小 (-)">' + ICONS.zoomOut + '</button>'
    +'<button type="button" class="lb-tool-btn" data-lb-action="zoom-in" aria-label="Zoom in" title="放大 (+)">' + ICONS.zoomIn + '</button>'
    +'<button type="button" class="lb-tool-btn" data-lb-action="rotate-left" aria-label="Rotate left" title="Rotate left (R)">' + ICONS.rotateLeft + '</button>'
    +'<button type="button" class="lb-tool-btn" data-lb-action="rotate-right" aria-label="Rotate right" title="Rotate right (Shift+R)">' + ICONS.rotateRight + '</button>'
    +'</div>'
    +'<button type="button" class="lb-nav lb-nav-prev" data-lb-action="prev" aria-label="上一张" title="上一张 (←)">' + ICONS.galleryPrev + '</button>'
    +'<button type="button" class="lb-nav lb-nav-next" data-lb-action="next" aria-label="下一张" title="下一张 (→)">' + ICONS.galleryNext + '</button>'
    +'<div class="lb-counter" aria-live="polite"></div>'
    +'<img alt=""><div class="lb-zoom-hint"></div>';
  document.body.appendChild(ov);
  var img=ov.querySelector('img'),hint=ov.querySelector('.lb-zoom-hint');
  var counter=ov.querySelector('.lb-counter'),prevBtn=ov.querySelector('.lb-nav-prev'),nextBtn=ov.querySelector('.lb-nav-next');
  var scale=1,panX=0,panY=0,rotation=0,dragging=false,lx=0,ly=0,ht=null,rotateAnimTimer=null;
  // Gallery group state: items is a click-time snapshot of {full, thumb} URL
  // strings (no DOM references), so the poll-driven innerHTML re-renders that
  // destroy the thumbnails cannot invalidate an open lightbox.
  var items=[],idx=0,lastFocus=null;
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
  function close(){
    ov.classList.remove('active');reset();
    // Return focus to wherever the user was before the lightbox grabbed it,
    // but only if focus is still inside the overlay — if the user already
    // clicked elsewhere, stealing focus back would be hostile.
    if(lastFocus&&ov.contains(document.activeElement)){try{lastFocus.focus()}catch(_){/* detached node */}}
    lastFocus=null;
  }
  function zoomBy(f){scale=Math.min(Math.max(scale*f,.5),10);apply();showHint()}
  // ── Gallery group navigation (RFC lightbox-gallery-nav §3) ──
  // preloaded dedupes warm-up requests across fast paging within one open
  // gallery; the Image objects themselves are throwaway — the browser HTTP
  // cache holds the bytes. Reset per openLightboxGroup: attachment URLs carry
  // a ?v=<time> cache-buster, so a session-lifetime map would only grow.
  var preloaded={};
  function preload(i){
    if(i<0||i>=items.length)return;
    var u=items[i].full;
    if(!u||preloaded[u])return;
    preloaded[u]=1;
    var im=new Image();
    // Swallow 404s (GC-expired attachments): a failed warm-up must not
    // surface through the RNEW-UX-002 global error handler as a toast.
    im.onerror=function(){};
    im.src=u;
  }
  function updateNav(){
    var multi=items.length>1;
    ov.classList.toggle('lb-single',!multi);
    if(!multi)return;
    counter.textContent=(idx+1)+' / '+items.length;
    // aria-disabled (not the disabled attribute) keeps boundary buttons in
    // the tab order so screen-reader users can perceive the edge.
    prevBtn.setAttribute('aria-disabled',idx<=0?'true':'false');
    nextBtn.setAttribute('aria-disabled',idx>=items.length-1?'true':'false');
  }
  // loadWithFallback(item) loads item.full and silently degrades to
  // item.thumb when the original is gone. The attachment-GC-expired path
  // (RFC §3.6.3): the on-disk original at /api/sessions/attachment?... is
  // gone, but the embedded thumbnail data URI was persisted alongside it
  // and renders identically (though at 600px). Without the fallback the
  // user would see a broken-image glyph.
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
  function loadWithFallback(item){
    var src=item.full,fallback=item.thumb;
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
  }
  function show(i,dir){
    if(i<0||i>=items.length)return;
    idx=i;
    // Zoom/pan/rotation reset on every page turn — carrying the previous
    // image's pan could push the next one fully off-screen.
    reset();
    loadWithFallback(items[idx]);
    updateNav();
    preload(idx+(dir||1));
  }
  function nav(dir){show(idx+dir,dir)}
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
    // A horizontal swipe can land a browser-synthesized click anywhere on the
    // overlay — backdrop (would close), or a nav rail at left/right where the
    // finger lifted (would double-navigate). Swallow ANY click arriving within
    // the synthesis window after a nav swipe. Time-based (not a boolean flag)
    // because preventDefault in touchend suppresses the synthetic click on
    // most engines: a leftover boolean would eat the NEXT legitimate click,
    // while a timestamp simply expires.
    if(Date.now()-swipeAt<500){swipeAt=0;return}
    var btn=e.target&&e.target.closest&&e.target.closest('[data-lb-action]');
    if(btn){
      // Toolbar/nav click — handle action and don't fall through to backdrop close.
      // prev/next respect aria-disabled (ARIA 1.2 §6.6.3): pointer-events:none
      // blocks mouse, but the buttons stay focusable, so Enter would otherwise
      // activate a control that announces itself as disabled.
      var action=btn.getAttribute('data-lb-action');
      var dis=btn.getAttribute('aria-disabled')==='true';
      if(action==='rotate-left')rotateBy(-90);
      else if(action==='rotate-right')rotateBy(90);
      else if(action==='zoom-in')zoomBy(1.2);
      else if(action==='zoom-out')zoomBy(1/1.2);
      else if(action==='prev'&&!dis)nav(-1);
      else if(action==='next'&&!dis)nav(1);
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
  // Touch: pinch zoom + drag pan + double-tap + horizontal swipe nav.
  // Gesture arbitration (RFC lightbox-gallery-nav §3):
  //   - swipeScale is captured at touchSTART. A pinch leaves `scale` at an
  //     arbitrary float when the second finger lifts, so reading it at
  //     touchend would misclassify a pinch-then-swipe as a pan.
  //   - `pinched` marks any gesture that ever had two fingers down; such a
  //     gesture can never end as a navigation swipe.
  //   - scale < 1.05 (tolerance, not ===1): swipe navigates. Above it the
  //     single finger pans the zoomed image — mutually exclusive paths.
  var iDist=0,iScale=1,lastTap=0,sx=0,sy=0,swipeScale=1,pinched=false,swipeAt=0;
  function t2d(t){return Math.hypot(t[1].clientX-t[0].clientX,t[1].clientY-t[0].clientY)}
  img.addEventListener('touchstart',function(e){if(e.touches.length===2){e.preventDefault();pinched=true;iDist=t2d(e.touches);iScale=scale}else if(e.touches.length===1){pinched=false;sx=e.touches[0].clientX;sy=e.touches[0].clientY;swipeScale=scale;if(scale>1){lx=sx;ly=sy;dragging=true}}},{passive:false});
  img.addEventListener('touchmove',function(e){if(e.touches.length===2&&iDist){e.preventDefault();scale=Math.min(Math.max(iScale*(t2d(e.touches)/iDist),.5),10);apply();showHint()}else if(e.touches.length===1&&dragging){e.preventDefault();panX+=e.touches[0].clientX-lx;panY+=e.touches[0].clientY-ly;lx=e.touches[0].clientX;ly=e.touches[0].clientY;apply()}},{passive:false});
  img.addEventListener('touchend',function(e){
    if(e.touches.length<2)iDist=0;
    if(e.touches.length!==0)return;
    dragging=false;
    if(e.changedTouches.length!==1)return;
    var t=e.changedTouches[0],dx=t.clientX-sx,dy=t.clientY-sy;
    if(!pinched&&items.length>1&&swipeScale<1.05&&Math.abs(dx)>50&&Math.abs(dx)>Math.abs(dy)*1.2){
      e.preventDefault();
      nav(dx<0?1:-1);
      // A nav swipe must not double as the first/second tap of a double-tap
      // zoom (rapid successive swipes land within the 300ms window), nor may
      // its synthesized click reach the overlay click handler (backdrop close
      // or a nav rail under the lifted finger).
      lastTap=0;
      swipeAt=Date.now();
      return;
    }
    var now=Date.now();
    if(now-lastTap<300){e.preventDefault();if(scale>1.05)reset();else scale=2.5;apply();showHint()}
    lastTap=now;
  },{passive:false});
  // openLightboxGroup(list, start) opens a gallery over `list`, an array of
  // {full, thumb} URL-string snapshots, starting at index `start`. Single
  // lightbox instance: calling while already open simply replaces the group.
  openLightboxGroup=function(list,start){
    items=(list&&list.length)?list:[];
    if(!items.length)return;
    preloaded={};
    var wasOpen=ov.classList.contains('active');
    show(Math.min(Math.max(start||0,0),items.length-1));
    ov.classList.add('active');
    if(!wasOpen){
      // Move focus onto the overlay so ←/→/Esc work immediately; remember
      // where it came from so close() can restore it.
      lastFocus=document.activeElement;
      try{ov.focus()}catch(_){/* focus can throw on detached roots */}
    }
  };
  // openLightboxFromThumb(el) collects every sibling thumbnail inside the
  // clicked .event-images container into a gallery group. dataset reads copy
  // plain strings — no DOM references survive into `items`, so poll-driven
  // innerHTML re-renders can't invalidate an open lightbox.
  openLightboxFromThumb=function(el){
    var box=el.closest&&el.closest('.event-images');
    var imgs=box?Array.prototype.slice.call(box.querySelectorAll('img[data-full]')):[el];
    var list=imgs.map(function(i){return{full:i.dataset.full||i.src,thumb:i.dataset.thumb||i.src}});
    // indexOf can only miss if `el` somehow lacks data-full (not rendered by
    // eventHtml); degrade to the first image rather than refusing to open.
    openLightboxGroup(list,Math.max(0,imgs.indexOf(el)));
  };
  // Compatibility shell: the historical single-image entry point. Kept so
  // any caller outside eventHtml (or user bookmarklets) keeps working.
  // Delegated thumbnail click handler — registered once on document, so it
  // survives the chat transcript's innerHTML re-renders (eventHtml emits the
  // thumbnails without inline onclick; RFC lightbox-gallery-nav §3).
  document.addEventListener('click',function(e){
    var t=e.target&&e.target.closest&&e.target.closest('.event-images img[data-full]');
    if(t)openLightboxFromThumb(t);
  });
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
    // Tab containment: aria-modal alone isn't honored by every AT, and a
    // sighted keyboard user could Tab into the chat behind the overlay.
    // nz_util's trapFocus is unsuitable here — its Escape branch removes the
    // overlay element, but this lightbox is a persistent singleton.
    if(e.key==='Tab'){
      var nodes=Array.prototype.filter.call(ov.querySelectorAll('button'),function(n){return n.offsetParent!==null});
      if(!nodes.length){e.preventDefault();return}
      var first=nodes[0],last=nodes[nodes.length-1],cur=document.activeElement;
      if(e.shiftKey&&(cur===first||cur===ov)){e.preventDefault();last.focus()}
      else if(!e.shiftKey&&cur===last){e.preventDefault();first.focus()}
      else if(!ov.contains(cur)){e.preventDefault();first.focus()}
      return;
    }
    if(e.key==='ArrowLeft'){e.preventDefault();nav(-1);return}
    if(e.key==='ArrowRight'){e.preventDefault();nav(1);return}
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

  function showDrawer() {
    drawer.classList.add('visible');
    // Dock as a right-hand split on desktop (no-op on phone overlay).
    if (nzSplitEnter) nzSplitEnter();
    // Opened last → stack on top of the preview pane if both are docked.
    if (nzSplitBringToFront) nzSplitBringToFront('scratch');
  }
  function hideDrawer() {
    drawer.classList.remove('visible');
    if (nzSplitExit) nzSplitExit();
    // Re-expand the sidebar if openScratch auto-collapsed it (no-op if the
    // preview drawer is still open or the user collapsed it themselves).
    restoreSidebarAfterDrawer();
  }

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
      await fetch(NZ_CONTRACT.API.scratch_id.replace('{id}', encodeURIComponent(id)), {
        method: 'DELETE', headers: authHeaders(),
      });
    } catch (_) { /* best effort */ }
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

  // @contract-begin scratchAdmitEvent
  // scratchAdmitEvent is the same-ms replay gate for the drawer's HTTP poll.
  // HandleEvents ?after= re-admits the watermark millisecond (#2456, so a
  // same-ms sibling is never lost), which means every idle tick replays the
  // entries AT st.lastEventTime. st.seenAtWM holds the uuids already
  // processed (rendered OR echo-dropped) at that ms: the local optimistic
  // user bubble carries no data-uuid and matchesPendingEcho consumes its
  // entry on first sight, so a DOM lookup alone would re-render the echoed
  // user event on the next tick. Returns false when e must be skipped;
  // otherwise records it and advances the watermark. uuid-less (pre-uuid)
  // events at the watermark are admitted — never swallow what we can't
  // identify (losing history is worse than a duplicate bubble).
  function scratchAdmitEvent(st, e) {
    const t = (e && typeof e.time === 'number') ? e.time : 0;
    if (t && t < st.lastEventTime) return false;
    if (!st.seenAtWM) st.seenAtWM = new Set();
    if (t && t === st.lastEventTime && e.uuid && st.seenAtWM.has(e.uuid)) return false;
    if (t > st.lastEventTime) {
      st.lastEventTime = t;
      st.seenAtWM.clear();
    }
    if (t && t === st.lastEventTime && e.uuid) st.seenAtWM.add(e.uuid);
    return true;
  }
  // @contract-end scratchAdmitEvent

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
      // Same-ms replay / strictly-older guard; also owns the watermark.
      if (!scratchAdmitEvent(state, e)) continue;
      // Drop server-echoed user messages that we already rendered locally.
      if (matchesPendingEcho(e)) continue;
      // Reuse the main event renderer so aside bubbles match the transcript
      // style (markdown, code blocks, etc.) without duplicating logic.
      const h = eventHtml(e);
      if (!h) continue;
      const t = e.time || 0;
      // Insert a divider when the gap between adjacent visible bubbles
      // exceeds EVENT_DIVIDER_GAP_MS — matches the main-window grammar.
      if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)
      ) {
        elMsgs.insertAdjacentHTML('beforeend', timeDividerHtml(t));
      }
      const tmp = document.createElement('div');
      tmp.innerHTML = h;
      while (tmp.firstChild) elMsgs.appendChild(tmp.firstChild);
      if (t) prevT = t;
      if (e.type === 'user') sawUser = true;
    }
    // Hide any "↗ 追问" buttons inside the aside itself — stacking is disabled.
    for (const btn of elMsgs.querySelectorAll('.event-ask-btn')) btn.remove();
    // Apply WeChat-style avatar grouping in the aside too (it reuses eventHtml
    // and the same .nz-grouped CSS, but lives outside the #events-scroll
    // observer, so tag it explicitly).
    regroupAvatars(elMsgs);
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
      let url = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(state.key);
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
      const r = await fetch(NZ_CONTRACT.API.scratch_open, {
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
        showAPIError('打开追问', r.status, txt);
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
        seenAtWM: new Set(), // uuids processed AT lastEventTime (scratchAdmitEvent)
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
      collapseSidebarForDrawer();
      setTimeout(() => elInput.focus(), 60);
      startPolling();
    } catch (e) {
      console.error('open scratch', e);
      showNetworkError('打开追问', e);
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
      const r = await fetch(NZ_CONTRACT.API.sessions_send, {
        method: 'POST',
        headers: authHeaders({'Content-Type': 'application/json'}),
        body: JSON.stringify({key: state.key, text}),
      });
      if (!r.ok) {
        const txt = await r.text().catch(() => '');
        showAPIError('发送消息', r.status, txt);
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
      showNetworkError('发送消息', e);
      elLoading.classList.remove('visible');
    } finally {
      sending = false;
      elSend.disabled = false;
      elInput.focus();
    }
  }

  async function promoteScratch() {
    if (!state) {
      showToast('追问会话已关闭，无法保存');
      return;
    }
    const id = state.scratchId;
    try {
      const r = await fetch(NZ_CONTRACT.API.scratch_id_promote.replace('{id}', encodeURIComponent(id)), {
        method: 'POST', headers: authHeaders(),
      });
      if (!r.ok) {
        const txt = await r.text().catch(() => '');
        showAPIError('保存为正式会话', r.status, txt);
        return;
      }
      const data = await r.json();
      state = null;   // scratch was detached server-side; skip the DELETE in closeScratch
      stopPolling();
      hideDrawer();
      clearMessages();
      elSave.classList.remove('visible');
      elInput.value = '';
      showToast('已保存为正式会话');
      // Refresh sidebar and try to select the new key.
      try {
        if (typeof lastVersion !== 'undefined') lastVersion = 0;
        await fetchSessions();
        if (data.key) selectSession(data.key, 'local');
      } catch (_) {}
    } catch (e) {
      console.error('promote scratch', e);
      showNetworkError('保存为正式会话', e);
    }
  }

  // Expose the active scratch router-key so shared renderers (e.g. the
  // AskUserQuestion submit handler) can route answers to the scratch CLI
  // instead of the parent session whose `selectedKey` is what `onAskSubmit`
  // would otherwise read. Returns '' when no scratch is open.
  getActiveScratchKey = function() {
    return (state && state.key) ? state.key : '';
  };

  // Exposed so the view-router (setActivityView) can tear the 追问 drawer down
  // when leaving the chat view — the drawer is position:fixed and would
  // otherwise float over assets/cron/settings. closeScratch handles the
  // no-op-when-closed case internally.
  closeScratchDrawer = function() { closeScratch(true); };

  // Expose the global used by the ↗ button in eventHtml.
  askAside = function(btn) {
    if (!btn) return;
    const raw = btn.getAttribute('data-raw') || '';
    const msgTime = Number(btn.getAttribute('data-msg-time') || 0);
    if (!raw || raw.length < 1) return;
    if (!selectedKey) {
      showToast('请先选择会话');
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
      const data = await fetchJSON(NZ_CONTRACT.API.memory_slug.replace('{slug}', encodeURIComponent(slug)), {
        headers: authHeaders(),
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



// ─── module exports (#2557 PR-E2) ───────────────────────────────────────────
// The view modules import these instead of dereferencing the window bridge.
// dashboard is the dependency root: it imports only nz_util, so the graph
// stays acyclic and module execution order matches the historical tag order.
// (let bindings like lastEventTime export as live views — reassignment here
// is visible to importers, unlike a window-property copy.)
// Wire the markdown renderers' dashboard-side helpers (#2558 D4). Runs in
// dashboard's module body, before any render call.
configureUtilities({ allSessionsCache, cliBackends, getToken, lastStatsSnapshot, renderSystemView, wsm });
configureFileRefs({ AVATAR_GROUP_GAP_MS, ICONS, collapseSidebarForDrawer, getToken, isInternalEvent, loadKatex, loadMermaid, matchProject, nzSplitBringToFront, nzSplitEnter, nzSplitExit, renderRich, restoreSidebarAfterDrawer, runPendingAsync });
configureRunningBanner({ ICONS, getMsgValue, getToken, setMsgValue, showNetworkError, sid, wsm });
configureSystemView({ formatAbsTime, getMsgValue, mainEmptyHtml, refreshCostSummary, renderServiceOverviewHtml, setActivityView, timeAgo, wireQuickAskInput });
configureSplitView({ lsGet, lsRemove, lsSet, stickEventsBottom });
configureSelfUpdate({ confirmDialog, markSessionOptimisticRunning });
configureSessionHeader({ fetchSessions, formatAbsTime, getToken, renderMainShell, sid });
configureComposerFiles({ ICONS, featureForCurrent, formatFileSize, getToken, sendMessage, showAuthModal });
configureMobileNav({ ICONS, confirmDialog, dismissSession, lsGet, lsSet, nzAnyDrawerOpen, nzSplitExit, renameSession, renderMainHeader, selectSession });
configureVoice({ ICONS, getMsgValue, getToken, sendMessage, setMsgValue, sid, updateSendButton });
configureRenderMd({
  FILE_REF_HAS_EXT,
  decodeEscEntities,
  fencedPathList,
  fileRefCode,
  isFileRefCandidate,
  safeUrl,
  splitPathLine,
});

export {
  authHeaders,
  eventHtml,
  fetchCLIBackends,
  fetchEvents,
  getToken,
  isInternalEvent,
  lastDividerTime,
  lastEventTime,
  lsGet,
  lsSet,
  renderBackendPicker,
  renderEventsWithDividers,
  sessionScrollPos,
  setActivityView,
  showAuthModal,
  wsm,
};

// ─── data-action registry (#1980 PR-2, docs/rfc/csp-data-action.md) ────────
// Every handler the dashboard's generated HTML wires via data-action(-<type>)
// attributes, plus the absorbed project-header / tuning-chip / modal-close
// dispatch maps. Parameters ride data-* attributes; keys are code literals.
function thumbIdxOf(el) {
  const t = el.closest('[data-idx]');
  return t ? parseInt(t.dataset.idx, 10) : -1;
}
const dashActivate = (fn) => (el, e) => {
  if (e.type === 'keydown') {
    if (e.key !== 'Enter') return;
  }
  fn(el, e);
};
registerActions({
  'session-new': () => createNewSession(),
  'history-resume': (el) => { resumeRecentSession(el.dataset.sid); closeHistoryPopover(); },
  'session-dismiss': (el) => dismissSession(el.dataset.key, el.dataset.node),
  'session-select': (el) => selectSession(el.dataset.key, el.dataset.node),
  'session-card-key': (el, e) => sessionCardKey(e),
  'ws-reconnect': () => reconnectNow(),
  'cheatsheet-dismiss': () => dismissCheatsheet(),
  'session-rename': () => renameSession(),
  'session-download-md': () => downloadSessionMarkdown(),
  'mobile-back': () => mobileBack(),
  'nav-msg': (el) => navMsg(el.dataset.dir),
  'nav-show-list': () => navShowList(),
  'file-picker': () => openFilePicker(),
  'input-mode-toggle': () => toggleInputMode(),
  'msg-input-key': (el, e) => handleKey(e),
  'msg-input-compend': () => { lastCompositionEnd = Date.now(); },
  'msg-send': () => sendMessage(),
  'session-interrupt': () => interruptSession(),
  'file-input-change': (el) => handleFiles(el.files),
  'ask-option-toggle': (el) => onAskOptionToggle(el),
  'ask-submit': (el) => onAskSubmit(el),
  'event-copy': (el) => copyEventContent(el),
  'ask-aside': (el) => askAside(el),
  'upload-retry': (el) => retryUpload(thumbIdxOf(el)),
  'file-remove': (el) => removeFile(thumbIdxOf(el)),
  'thumb-dragstart': (el, e) => onThumbDragStart(e, thumbIdxOf(el)),
  'thumb-dragover': (el, e) => onThumbDragOver(e),
  'thumb-dragleave': (el, e) => onThumbDragLeave(e),
  'thumb-drop': (el, e) => onThumbDrop(e, thumbIdxOf(el)),
  'thumb-dragend': () => onThumbDragEnd(),
  'thumb-key': (el, e) => onThumbKeyDown(e, thumbIdxOf(el)),
  'token-input-key': dashActivate(() => saveToken()),
  'auth-dismiss': () => dismissAuthModal(),
  'token-save': () => saveToken(),
  'create-session-key': dashActivate(() => doCreateSession()),
  'modal-close': (el) => { const o = el.closest('.modal-overlay'); if (o) o.remove(); },
  'session-create': () => doCreateSession(),
  'code-copy': (el) => copyCodeBlock(el),
  'onboarding-dismiss': () => dismissOnboarding(),
  'onboarding-create': () => { dismissOnboarding(); createNewSession(); },
  // absorbed: project-header buttons (ex SIDEBAR_PROJECT_ACTIONS)
  'project-collapse': (el) => toggleProjectCollapsed(el.dataset.key),
  'project-favorite': (el) => toggleFavorite(el.dataset.name, el.dataset.node),
  'project-github': (el) => showGitRemote(el.dataset.url),
  'project-settings': (el) => openProjectSettings(el.dataset.name),
  // absorbed: header tuning chips
  'tuning-model': () => openTuningPopover('model'),
  'tuning-effort': () => openTuningPopover('effort'),
});

// ─── D3 ES-module bridge (RFC docs/rfc/dashboard-es-modules.md §3) ─────────
// nz.state accessors: migrated modules (agent_view, cron_view) reach
// dashboard's reassignable top-level bindings through these accessors — a
// classic script's let never lands on window, and a copied value would go
// stale on reassignment. Setters exist only for the names cron_view
// legitimately writes today (activeView / eventTimer / selectedKey); keep
// the rest getter-only so a new cross-file write is a reviewed decision.
Object.defineProperties(nzState, {
  activeView: { get: function () { return activeView; }, set: function (v) { activeView = v; } },
  defaultWorkspace: { get: function () { return defaultWorkspace; } },
  eventTimer: { get: function () { return eventTimer; }, set: function (v) { eventTimer = v; } },
  lastEventTime: { get: function () { return lastEventTime; }, set: function (v) { lastEventTime = v; } },
  navUserEls: { get: function () { return navUserEls; } },
  nodesData: { get: function () { return nodesData; } },
  pendingFiles: { get: function () { return pendingFiles; }, set: function (v) { pendingFiles = v; } },
  projectsData: { get: function () { return projectsData; } },
  selectedKey: { get: function () { return selectedKey; }, set: function (v) { selectedKey = v; } },
  selectedNode: { get: function () { return selectedNode; }, set: function (v) { selectedNode = v; } },
  sending: { get: function () { return sending; } },
  sessionDrafts: { get: function () { return sessionDrafts; } },
  sessionLastSent: { get: function () { return sessionLastSent; } },
  sessionPendingTuning: { get: function () { return sessionPendingTuning; } },
  sessionScrollPos: { get: function () { return sessionScrollPos; } },
  sessionsData: { get: function () { return sessionsData; } },
  turnState: { get: function () { return turnState; } },
});
// ─── nz.test: Playwright instrumentation surface (#2557 PR-E3) ─────────────
// The e2e suite probes these bindings (page.evaluate). Reassignable lets are
// exposed as accessors so a probe read always sees the live binding and a
// probe write lands back on it; functions/consts ride plain properties.
// Production code must never read nz.test — the mock server mirrors it onto
// window (test/e2e e2e-shim) for the suite's legacy bare-identifier probes.
// This list may only shrink as tests migrate to first-class assertions.
Object.defineProperties(nzTest, {
  _lastSidebarData: { get: function () { return _lastSidebarData; }, set: function (v) { _lastSidebarData = v; } },
  _lastSidebarHtml: { get: function () { return _lastSidebarHtml; }, set: function (v) { _lastSidebarHtml = v; } },
  activeView: { get: function () { return activeView; }, set: function (v) { activeView = v; } },
  discoveredItems: { get: function () { return discoveredItems; }, set: function (v) { discoveredItems = v; } },
  discoveredPollTimer: { get: function () { return discoveredPollTimer; }, set: function (v) { discoveredPollTimer = v; } },
  katexReady: { get: function () { return katexReady; }, set: function (v) { katexReady = v; } },
  lastEventTime: { get: function () { return lastEventTime; }, set: function (v) { lastEventTime = v; } },
  lastRenderedEventTime: { get: function () { return lastRenderedEventTime; }, set: function (v) { lastRenderedEventTime = v; } },
  lastVersion: { get: function () { return lastVersion; }, set: function (v) { lastVersion = v; } },
  navPopoverOpen: { get: function () { return navPopoverOpen; }, set: function (v) { navPopoverOpen = v; } },
  pendingFiles: { get: function () { return pendingFiles; }, set: function (v) { pendingFiles = v; } },
  projectsData: { get: function () { return projectsData; }, set: function (v) { projectsData = v; } },
  selectedKey: { get: function () { return selectedKey; }, set: function (v) { selectedKey = v; } },
  selectedNode: { get: function () { return selectedNode; }, set: function (v) { selectedNode = v; } },
  sending: { get: function () { return sending; }, set: function (v) { sending = v; } },
  sessionPollTimer: { get: function () { return sessionPollTimer; }, set: function (v) { sessionPollTimer = v; } },
  sessionsData: { get: function () { return sessionsData; }, set: function (v) { sessionsData = v; } },
  turnState: { get: function () { return turnState; }, set: function (v) { turnState = v; } },
});
Object.assign(nzTest, {
  BLOCK_SPLIT_RE: BLOCK_SPLIT_RE,
  isFileRefCandidate: isFileRefCandidate,
  splitPathLine: splitPathLine,
  LIST_ITEM_RE: LIST_ITEM_RE,
  LIST_SHAPE_RE: LIST_SHAPE_RE,
  MAX_LIST_DEPTH: MAX_LIST_DEPTH,
  MAX_LIVE_DOM_EVENTS: MAX_LIVE_DOM_EVENTS,
  WS_STATES: WS_STATES,
  _askAnswered: _askAnswered,
  _mdCache: _mdCache,
  appendEvents: appendEvents,
  applyFeatureGates: applyFeatureGates,
  clearPendingFiles: clearPendingFiles,
  closeHistoryPopover: closeHistoryPopover,
  createNewSession: createNewSession,
  debouncedFetchSessions: debouncedFetchSessions,
  dismissSession: dismissSession,
  doCreateInProject: doCreateInProject,
  eventAlreadyRendered: eventAlreadyRendered,
  eventHtml: eventHtml,
  fetchEvents: fetchEvents,
  fetchSessions: fetchSessions,
  getMsgValue: getMsgValue,
  getNodeStatus: getNodeStatus,
  getSelectedNode: getSelectedNode,
  highlight: highlight,
  interruptSession: interruptSession,
  isMathDisplay: isMathDisplay,
  isMathInline: isMathInline,
  katexPending: katexPending,
  markSessionOptimisticRunning: markSessionOptimisticRunning,
  maybeShowOnboarding: maybeShowOnboarding,
  navDismissPopover: navDismissPopover,
  openProjectPalette: openProjectPalette,
  parseListItem: parseListItem,
  pickPaletteCustom: pickPaletteCustom,
  promptDialog: promptDialog,
  reconcileSelectedNode: reconcileSelectedNode,
  removeSidebarCard: removeSidebarCard,
  renameSession: renameSession,
  renderEvents: renderEvents,
  renderKatex: renderKatex,
  renderMainShell: renderMainShell,
  renderMd: renderMd,
  renderNodePicker: renderNodePicker,
  renderOptimisticUserMsg: renderOptimisticUserMsg,
  renderSidebar: renderSidebar,
  renderTable: renderTable,
  restorePending: restorePending,
  scanDiscovered: scanDiscovered,
  scrollSlackPx: scrollSlackPx,
  selectSession: selectSession,
  sendMessage: sendMessage,
  sessionCardKey: sessionCardKey,
  sessionDrafts: sessionDrafts,
  sessionNodes: sessionNodes,
  sessionWorkspaces: sessionWorkspaces,
  setActivityView: setActivityView,
  setMsgValue: setMsgValue,
  showGitRemote: showGitRemote,
  showToast: showToast,
  sid: sid,
  toggleHistory: toggleHistory,
  toggleProjectCollapsed: toggleProjectCollapsed,
  trimEventsScroll: trimEventsScroll,
  updateHeaderCLI: updateHeaderCLI,
  updateStatusBar: updateStatusBar,
  wireNodePicker: wireNodePicker,
  wsm: wsm,
});
