import { esc, escAttr, fetchJSON, showToast, trapFocus, nzState, nzBus, nzViews, nzTest, registerActions   } from './nz_util.js';
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
import {
  configureDiscovery,
  discoveredKey,
  dropDiscovered,
  findDiscovered,
  isDiscoveredKey,
  parseDiscoveredPid,
  previewDiscovered,
  sameDiscovered,
  scanDiscovered,
} from './discovery.js';
import {
  configureTuning,
  dismissSession,
  fetchGitState,
  invalidateGitState,
  openTuningPopover,
  removeSidebarCard,
  renameSession,
  repaintGitChip,
} from './tuning.js';
import {
  configureMsgNav,
  navDismissPopover,
  navMsg,
  navPopoverOpen,
  navRebuild,
  navShowList,
  navUpdatePill,
  updateSendButton,
} from './msg_nav.js';
import {
  CLAWD_SVG,
  ICONS,
  configureSidebarProject,
  openProjectSettings,
  sectionHeaderFallbackHtml,
  sectionHeaderHtml,
  showGitRemote,
  toggleFavorite,
  toggleProjectCollapsed,
} from './sidebar_project.js';
import {
  PICKER_SELECT_ONLY_STYLE,
  PICKER_SELECT_STYLE,
  accessProfileChipHtml,
  accessProfileChipInfo,
  backendDisplayName,
  backendDisplayVersion,
  configureAuthModal,
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
} from './auth_modal.js';
import {
  _optimisticRunningTimers,
  clearPendingFiles,
  configureSendMessage,
  getMsgValue,
  handleKey,
  markSessionOptimisticRunning,
  renderOptimisticUserMsg,
  rollbackOptimisticRunning,
  sendMessage,
  setMsgValue,
} from './send_message.js';
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
  const oldIdx = nzState.navIdx;
  nzState.navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
  nzState.navIdx = oldIdx >= 0 && oldIdx < nzState.navUserEls.length ? oldIdx : -1;
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
      nzState.navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
      if (nzState.navIdx >= 0 && nzState.navIdx < nzState.navUserEls.length) { /* preserve */ } else nzState.navIdx = -1;
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
      nzState.navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
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
// Wire the extracted modules BEFORE the bootstrap sequence below. Some of
// them run at load time (discovery's scanDiscovered, the pollers) and read
// their injected deps immediately: with the configure block placed after the
// bootstrap, deps.getToken was still undefined and the discovered-session
// scan failed silently (#2558 D4-6 — six e2e specs, no console error beyond
// one warn line).
// ─── module exports (#2557 PR-E2) ───────────────────────────────────────────
// The view modules import these instead of dereferencing the window bridge.
// dashboard is the dependency root: it imports only nz_util, so the graph
// stays acyclic and module execution order matches the historical tag order.
// (let bindings like lastEventTime export as live views — reassignment here
// is visible to importers, unlike a window-property copy.)
// Wire the markdown renderers' dashboard-side helpers (#2558 D4). Runs in
// dashboard's module body, before any render call.
configureSendMessage({ EVENT_DIVIDER_GAP_MS, awaitPendingOrients, discoveredKey, dropDiscovered, eventHtml, featureForCurrent, fetchEvents, fetchSessions, getToken, httpSendPending, interruptSession, lastDividerTime, navUpdatePill, persistPending, removeSidebarCard, renderFilePreviews, selectSession, sessionAccessProfiles, sessionBackends, sessionNodes, sessionOptimisticPrevState, sessionOptimisticRunning, sessionWorkspaces, showAPIError, showAuthModal, showNetworkError, sid, startTurnTimer, stickEventsBottom, timeDividerHtml, updateSendButton, wsm });
configureAuthModal({ applyFeatureGates, cliBackendsByNode, debouncedFetchSessions, eagerBindWorkspace, fetchSessions, getNodeDisplayName, getNodeStatus, isMultiNode, mobileEnterChat, navRebuild, nodeColor, persistPending, projectDisplayLabel, projectDisplayPrefix, renderMainShell, sendMessage, sessionAccessProfiles, sessionBackends, sessionNodes, sessionWorkspaces, setActiveSessionCard, setMsgValue, shortPath, showNetworkError, statusLabelForNode, stopPreviewPolling, updateStatusBar, wsm });
configureSidebarProject({ PICKER_SELECT_ONLY_STYLE, PICKER_SELECT_STYLE, accessProfileChipInfo, debouncedFetchSessions, fetchAccessProfiles, fetchCLIBackends, fetchSessions, getToken, projectDisplayLabel, projectDisplayPrefix, renderAccessProfilePicker, renderBackendPicker, renderSidebar, showAPIError, showNetworkError });
configureMsgNav({ closeHistoryPopover, createNewSession, debouncedFetchSessions, escCloseVoiceOverlay, handleFiles, refreshBanner, resetTurnState, selectSession, sid });
configureTuning({ debouncedFetchSessions, dropDiscovered, fetchSessions, findDiscovered, getToken, gitChipHtml, gitStateCache, isDiscoveredKey, mainEmptyHtml, parseDiscoveredPid, promptDialog, removePendingSession, renderMainHeader, sameDiscovered, sessionAccessProfiles, sessionBackends, sessionWorkspaces, setHeaderGitChip, showAPIError, showNetworkError, sid, stopPreviewPolling, wireQuickAskInput, wsm });
configureDiscovery({ EVENT_DIVIDER_GAP_MS, ICONS, debouncedFetchSessions, eventHtml, getToken, isInternalEvent, lastDividerTime, mobileEnterChat, navRebuild, navUpdatePill, processEventsForDisplay, renderEventsWithDividers, sessionTypeTag, setActiveSessionCard, showAPIError, showNetworkError, stickEventsBottom, stopPreviewPolling, timeDividerHtml, wsm });
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




export {
  authHeaders,
  eventHtml,
  fetchEvents,
  getToken,
  isInternalEvent,
  lastDividerTime,
  lastEventTime,
  lsGet,
  lsSet,
  renderEventsWithDividers,
  sessionScrollPos,
  setActivityView,
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
// nz.state accessors: the extracted modules reach
// dashboard's reassignable top-level bindings through these accessors — a
// classic script's let never lands on window, and a copied value would go
// stale on reassignment. Setters exist only for the names cron_view
// legitimately writes today (activeView / eventTimer / selectedKey); keep
// the rest getter-only so a new cross-file write is a reviewed decision.
Object.defineProperties(nzState, {
  accessProfiles: { get: function () { return accessProfiles; }, set: function (v) { accessProfiles = v; } },
  accessProfilesFetchedAt: { get: function () { return accessProfilesFetchedAt; }, set: function (v) { accessProfilesFetchedAt = v; } },
  activePopover: { get: function () { return activePopover; }, set: function (v) { activePopover = v; } },
  activeView: { get: function () { return activeView; }, set: function (v) { activeView = v; } },
  allSessionsCache: { get: function () { return allSessionsCache; }, set: function (v) { allSessionsCache = v; } },
  _autoPageBackCount: { get: function () { return _autoPageBackCount; }, set: function (v) { _autoPageBackCount = v; } },
  cliBackends: { get: function () { return cliBackends; }, set: function (v) { cliBackends = v; } },
  cliBackendsFetchedAt: { get: function () { return cliBackendsFetchedAt; }, set: function (v) { cliBackendsFetchedAt = v; } },
  collapsedProjects: { get: function () { return collapsedProjects; }, set: function (v) { collapsedProjects = v; } },
  defaultCLIName: { get: function () { return defaultCLIName; }, set: function (v) { defaultCLIName = v; } },
  defaultCLIVersion: { get: function () { return defaultCLIVersion; }, set: function (v) { defaultCLIVersion = v; } },
  defaultWorkspace: { get: function () { return defaultWorkspace; } },
  discoveredItems: { get: function () { return discoveredItems; }, set: function (v) { discoveredItems = v; } },
  _earlierGen: { get: function () { return _earlierGen; }, set: function (v) { _earlierGen = v; } },
  _earlierLoading: { get: function () { return _earlierLoading; }, set: function (v) { _earlierLoading = v; } },
  eventTimer: { get: function () { return eventTimer; }, set: function (v) { eventTimer = v; } },
  getActiveScratchKey: { get: function () { return getActiveScratchKey; }, set: function (v) { getActiveScratchKey = v; } },
  historySessionsData: { get: function () { return historySessionsData; }, set: function (v) { historySessionsData = v; } },
  _lastAppliedMainState: { get: function () { return _lastAppliedMainState; }, set: function (v) { _lastAppliedMainState = v; } },
  lastCompositionEnd: { get: function () { return lastCompositionEnd; }, set: function (v) { lastCompositionEnd = v; } },
  lastDiscoveredJSON: { get: function () { return lastDiscoveredJSON; }, set: function (v) { lastDiscoveredJSON = v; } },
  lastEventTime: { get: function () { return lastEventTime; }, set: function (v) { lastEventTime = v; } },
  lastRenderedEventTime: { get: function () { return lastRenderedEventTime; }, set: function (v) { lastRenderedEventTime = v; } },
  _lastSidebarData: { get: function () { return _lastSidebarData; }, set: function (v) { _lastSidebarData = v; } },
  _lastSidebarHtml: { get: function () { return _lastSidebarHtml; }, set: function (v) { _lastSidebarHtml = v; } },
  lastStatsSnapshot: { get: function () { return lastStatsSnapshot; }, set: function (v) { lastStatsSnapshot = v; } },
  lastVersion: { get: function () { return lastVersion; }, set: function (v) { lastVersion = v; } },
  navPopoverOpen: { get: function () { return navPopoverOpen; }, set: function (v) { navPopoverOpen = v; } },
  nodesData: { get: function () { return nodesData; } },
  oldestFetchedEventTime: { get: function () { return oldestFetchedEventTime; }, set: function (v) { oldestFetchedEventTime = v; } },
  _optimisticDeleteKeys: { get: function () { return _optimisticDeleteKeys; }, set: function (v) { _optimisticDeleteKeys = v; } },
  pendingDiscovered: { get: function () { return pendingDiscovered; }, set: function (v) { pendingDiscovered = v; } },
  pendingFiles: { get: function () { return pendingFiles; }, set: function (v) { pendingFiles = v; } },
  previewEventCount: { get: function () { return previewEventCount; }, set: function (v) { previewEventCount = v; } },
  _previewGen: { get: function () { return _previewGen; }, set: function (v) { _previewGen = v; } },
  previewTimer: { get: function () { return previewTimer; }, set: function (v) { previewTimer = v; } },
  projectsData: { get: function () { return projectsData; } },
  selectedKey: { get: function () { return selectedKey; }, set: function (v) { selectedKey = v; } },
  selectedNode: { get: function () { return selectedNode; }, set: function (v) { selectedNode = v; } },
  sending: { get: function () { return sending; }, set: function (v) { sending = v; } },
  sessionCounter: { get: function () { return sessionCounter; }, set: function (v) { sessionCounter = v; } },
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
