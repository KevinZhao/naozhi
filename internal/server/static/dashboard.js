import { NZ_CONTRACT } from './contract.js';
import { authHeaders, getToken, lsGet, lsRemove, lsSet } from './platform.js';
import { registerShell } from './shell.js';
import { sessionStream } from './session_stream.js';
import { WS_STATES, wsm } from './ws_manager.js';
import { composer, hooks, perSession, selection, serverInfo, sessionList, timers, transcript, ui } from './state.js';
import { esc, escAttr, fetchJSON, showToast, trapFocus, nzBus, nzViews, registerActions, sessionExitChipHtml } from './nz_util.js';
import { eventHtml } from './event_render.js';
import { onAskOptionToggle, onAskSubmit } from './ask_card.js';
import { eventIdentityKey, fetchEvents, hasMoreHeader, renderEvents } from './event_stream.js';
import { renderMd, runPendingAsync } from './render_md.js';
import { fetchSessionRuns, setHeaderEffortChip, setHeaderOverlayDriftChip, setHeaderPRChip, setHeaderSpawnDiagChip } from './session_header.js';
import {
  handleFiles,
  onThumbDragEnd,
  onThumbDragLeave,
  onThumbDragOver,
  onThumbDragStart,
  onThumbDrop,
  onThumbKeyDown,
  openFilePicker,
  removeFile,
  retryUpload,
} from './composer_files.js';
import {
  collapseSidebarForDrawer,
  initMobile,
  initSidebarCollapsed,
  initSwipeBack,
  initSwipeDelete,
  initViewportTracking,
  mobileBack,
  restoreSidebarAfterDrawer,
  toggleSidebarCollapsed,
} from './mobile_nav.js';
import { escCloseVoiceOverlay, toggleInputMode, voiceMouseDown, voiceTouchStart } from './voice.js';
import {
  initSplitWidth,
  splitDock,
} from './split_view.js';
import {
  fetchSystemDaemons,
  openSystemPanel,
  renderSystemView,
  stopSystemPoll,
} from './system_view.js';
import { interruptSession, resetTurnState, saveScrollPos, updateSendButton } from './running_banner.js';
import { closeFilePreview, regroupAvatars, startFileRefObserver } from './file_refs.js';
import {
  applyFeatureGates,
  closeHistoryPopover,
  confirmDialog,
  copyCodeBlock,
  copyEventContent,
  dismissAuthModal,
  formatAbsTime,
  getMsgValue,
  historyDayLabel,
  isMobile,
  mobileEnterChat,
  reconnectNow,
  setActiveSessionCard,
  setMsgValue,
  showAPIError,
  showNetworkError,
  startSidebarTimeTick,
  stopPreviewPolling,
  stopSidebarTimeTick,
  timeAgo,
  timeDividerHtml,
} from './utilities.js';
import {
  previewDiscovered,
  scanDiscovered,
} from './discovery.js';
import {
  dismissSession,
  fetchGitState,
  invalidateGitState,
  openTuningPopover,
  renameSession,
  repaintGitChip,
} from './tuning.js';
import { navDismissPopover, navMsg, navRebuild, navShowList } from './msg_nav.js';
import {
  openProjectSettings,
  showGitRemote,
  toggleFavorite,
  toggleProjectCollapsed,
} from './sidebar_project.js';
import { backendDisplayName, backendDisplayVersion, createNewSession, doCreateSession, keyTailDisplay, saveToken, startWSAuthRetryCountdown } from './auth_modal.js';
import { fetchAccessProfiles, fetchCLIBackends } from './backend_catalog.js';
import {
  handleKey,
  sendMessage,
} from './send_message.js';
import { collectWorkspaceSessionIDs, fetchSessions, onSessionsApplied, originBadgeHtml, refreshHeaderExitChip, restorePending, updateCardUnreadChip, updateMainState } from './session_list.js';
import { findDiscovered, isDiscoveredKey, matchProject, parseDiscoveredPid, sid } from './session_ident.js';
import { ICONS } from './icons.js';
// Service worker registration
if('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(()=>{});

// Collapsed project sections: Set of "node:name" keys. Persisted in
// localStorage so a user's fold state survives reloads. Toggled via the
// chevron button in the project section-header; the renderer skips emitting
// cards/empty-CTA for groups whose key is in this set.
sessionList.collapsedProjects = (function() {
  try { return new Set(JSON.parse(localStorage.getItem('nz_collapsedProjects') || '[]')); }
  catch(_) { return new Set(); }
})();
// selectedNode doubles as (a) the node the currently-selected session lives on
// and (b) the "view" filter applied to the sidebar session list when multiple
// nodes are connected. Persisted to localStorage so a reload keeps the user on
// the node they were browsing; validated against nodesData on every fetch so a
// removed/offline remote falls back to 'local'.
selection.node = (function() {
  try { return localStorage.getItem('nz_selectedNode') || 'local'; }
  catch(_) { return 'local'; }
})();

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
      if (ui.activeView === 'settings') renderSettingsView(); // refresh active pill if open
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
  if (view === ui.activeView) return;
  const prev = ui.activeView;
  ui.activeView = view;
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
  // reserved. Both close paths run splitDock.exit, clearing nz-split-open.
  if (prev === 'chat' && view !== 'chat') {
    closeFilePreview();
    if (hooks.closeScratchDrawer) hooks.closeScratchDrawer();
  }
  // Enter the target view.
  if (view === 'assets') { if (nzViews.asset) nzViews.asset.show(); }
  else if (view === 'files') { if (nzViews.files) nzViews.files.show(); }
  else if (view === 'cron') { nzBus.dispatchEvent(new CustomEvent('cron:open-panel')); }
  else if (view === 'system') { openSystemPanel(); }
  else if (view === 'settings') { renderSettingsView(); }
}

// reconcileMainStateAfterPoll brings the open session's banner and send/stop
// buttons to the REST state when a session_state push was missed. With the
// socket down REST is the only source and always wins. Over a live socket only
// the finished direction applies (no optimistic running window is open: a
// terminal 'result' or 'ready' push was dropped), as running is the push side's
// job; a poll sent before the key's latest push keeps the pushed state
// (mergeBackendSessions), so only a later poll heals a dropped push.
function reconcileMainStateAfterPoll(wsConnected) {
  if (!selection.key) return;
  const sKey = sid(selection.key, selection.node);
  const sd = sessionList.sessionsData[sKey];
  if (sd && (!wsConnected || (sd.state !== 'running' && !perSession.optimisticRunning[sKey]))) {
    // updateSendButton is not idempotent ('running' re-seeds agent rows,
    // 'ready' resets turn state and scroll) and this runs every 5 s under
    // fallback: re-apply only what differs from the last applied state (#2431).
    // The exit chip is idempotent and the poll may bring the death_detail a
    // 'dead' push left out, so it repaints either way.
    const applied = selection.lastAppliedMainState;
    if (!(applied && applied.key === sKey && applied.state === sd.state)) {
      updateMainState(sd.state);
    } else {
      refreshHeaderExitChip(sd.state);
    }
  } else if (sd && wsConnected && sd.state === 'running') {
    // A dropped 'running' push: the sidebar paints running from REST while
    // the banner, which only the push flips to running, stays idle. Heal it
    // only while the banner is fully hidden, so a banner that is correctly
    // showing (live activity, background agents) never flickers.
    const banner = document.getElementById('running-banner');
    if (banner && banner.classList.contains('nz-hidden')) {
      updateMainState('running');
    }
  }
}
onSessionsApplied(reconcileMainStateAfterPoll);
onSessionsApplied(() => { if (selection.key) updateHeaderCLI(); });

// --- History Popover ---


document.addEventListener('click', function(e) {
  if (ui.activePopover && !ui.activePopover.contains(e.target) && !e.target.closest('#btn-history')) {
    closeHistoryPopover();
  }
});

function toggleHistory() {
  if (ui.activePopover) { closeHistoryPopover(); return; }

  // Show all filesystem history sessions, deduplicated against workspace.
  // Includes prev_session_ids so that earlier links in a resumed-chain
  // session don't appear twice (once in the sidebar, once in history).
  const workspaceIDs = collectWorkspaceSessionIDs(sessionList.allSessionsCache);
  const merged = sessionList.historySessionsData
    .filter(r => !workspaceIDs.has(r.session_id))
    .map(r => ({
      key: '_history:' + r.session_id, node: 'local', source: 'recent',
      session_id: r.session_id, last_active: r.last_active || 0,
      retired_at: r.retired_at || 0,
      prompt: r.last_prompt || r.summary || '',
      project: r.project || matchProject(r.workspace), tool: '',
    }));
  // Newest first by when the session left the sidebar (retired_at, unix ms);
  // one this process never saw retire sorts by its transcript's last write.
  merged.sort((a, b) => (b.retired_at || b.last_active) - (a.retired_at || a.last_active));

  const popover = document.createElement('div');
  popover.className = isMobile() ? 'history-sheet' : 'history-popover';
  // The header's count chip and filter input re-render the items through
  // applyHistoryFilter(merged, query): an O(N) scan of ≤200 entries per key.
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
  ui.activePopoverBackdrop = backdrop;
  document.body.appendChild(backdrop);

  ui.activePopover = popover;
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
      (s.prompt ? '<div class="hp-prompt" title="' + escAttr(s.prompt) + '">' + esc(s.prompt) + '</div>' : '<div class="hp-prompt nz-dim">未命名</div>') +
      '<div class="hp-meta">' +
        (s.project ? '<span class="hp-project">' + esc(s.project) + '</span><span class="hp-dot">&middot;</span>' : '') +
        (ago ? '<span' + (abs ? ' title="' + escAttr(abs) + '"' : '') + '>' + ago + '</span>' : '') +
      '</div>' +
      '</div>';
  }).join('');
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
  const found = sessionList.historySessionsData.find(r => r.session_id === sessionId);
  resumeRecentById(sessionId, found ? found.workspace : '', found ? (found.last_prompt || found.summary || '') : '');
}

async function resumeRecentById(sessionId, workspace, lastPrompt) {
  // Guard: if already resuming this session, find the managed key and select it
  for (const s of sessionList.allSessionsCache) {
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
    sessionList.lastVersion = 0;
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
    if (selection.key !== expectedKey) return; // user navigated away
    if (!entries || entries.length === 0) return;
    renderEvents(entries);
  } catch (e) {
    console.error('previewRecentSession:', e);
  }
}

// CHEATSHEET_ENTRIES is the single source of truth for the shortcut modal;
// `keys` render as <kbd> chips joined by "+". Rows promise working UX only.
// '斜杠命令' lists exactly what turn.Parse handles in sessionSend (send.go);
// the IM-only section mirrors internal/dispatch/commands.go.
// TestCheatsheetDashboardSlashCommandsAreRecognised pins the split.
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
  { keys: ['/new'], desc: '重置当前会话（不带参数；/new <agent> 在这里会当普通消息发送）' },
  { keys: ['/clear'], desc: '重置当前会话（同 /new）' },
  { keys: ['/urgent'], desc: '/urgent <消息> 立即中断当前回复并优先发送（需后端支持抢占）' },
  { section: '斜杠命令（仅 IM 平台）' },
  { keys: ['/new <agent>'], desc: '重置指定 agent 的对话（如 /new review 对应 code-reviewer）' },
  { keys: ['/cd'], desc: '切换工作目录（/cd <path>；受 session.cwd 的 allowed_root 限制）' },
  { keys: ['/pwd'], desc: '显示当前工作目录' },
  { keys: ['/project'], desc: '绑定会话到项目（/project <name> 或 /project off 解绑）' },
  { keys: ['/cron'], desc: '定时任务：/cron add "<schedule>" <prompt> · /cron list · /cron del <id>' },
  { keys: ['/help'], desc: '显示可用命令' },
  { keys: ['/stop'], desc: '中断当前回复（保留排队消息）；dashboard 上用双击 Esc' },
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

// Global Esc: close the voice overlay, the history popover, the nav list and
// cron's inline expand / drawer when no modal or input has focus. Four independent ifs, not else-if: one Esc closes every one
// that is open. cron goes through nzViews.cron, absent when cron_view.js is not
// loaded (dashboard-cron-view-extraction §2.6 B1); its own priority (expanded
// row before drawer) lives in cron_view.js's cronEscClose.
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Escape') return;
  // Overlays with their own Esc trapFocus handling take precedence.
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
  let closed = false;
  if (escCloseVoiceOverlay()) closed = true;
  if (ui.activePopover) { closeHistoryPopover(); closed = true; }
  if (document.getElementById('nav-list-popover')) { navDismissPopover(); closed = true; }
  if (nzViews.cron && nzViews.cron.escClose()) { closed = true; }
  if (closed) e.preventDefault();
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
  if (selection.key) {
    const inp = document.getElementById('msg-input');
    const draft = getMsgValue(inp);
    if (draft) perSession.drafts[selection.key] = draft;
    else delete perSession.drafts[selection.key];
    // 同时快照当前会话的滚动位置，回来时恢复
    saveScrollPos(selection.key, selection.node);
  }
  if (isDiscoveredKey(key)) {
    const d = findDiscovered(parseDiscoveredPid(key), node);
    if (d) {
      // #2431: same as the managed path below — leave assets/cron/settings
      // first, or the preview panel is written into a hidden #main while the
      // previous session has already been unsubscribed.
      if (ui.activeView !== 'chat') setActivityView('chat');
      previewDiscovered(d.session_id, d.cwd, d.pid, d.proc_start_time || 0, d.node || '', d.type_label || '');
      return;
    }
  }
  selection.pendingDiscovered = null;
  // Picking a session returns to the chat view from any other top-level view
  // (assets / cron / settings). This restores the chat sidebar+main, hides the
  // other view's panels, and flips activeView back to 'chat' so renderMainShell
  // (which writes #main) is visible and any in-flight cron repaint is suppressed.
  if (ui.activeView !== 'chat') setActivityView('chat');
  const prevKey = selection.key;
  const prevNode = selection.node;
  selection.key = key;
  selection.node = node;
  selection.lastAppliedMainState = null; // #2431: new session → first poll must reconcile
  // Opening a card counts as "reading" it — clear the chat-style unread chip
  // before the DOM toggle below so the next render reflects a zeroed state.
  const selSid = sid(key, node);
  if (perSession.unread[selSid]) {
    delete perSession.unread[selSid];
  }
  // Opening a session on another node retargets dispatch (selectedNode drives
  // the dispatch node + main header). The sidebar no longer filters by node,
  // so there is no list to re-render — just persist the new target.
  if (prevNode !== selection.node) {
    try { localStorage.setItem('nz_selectedNode', selection.node); } catch(_) {}
  }
  transcript.lastEventTime = 0;
  transcript.lastRenderedEventTime = 0;
  transcript.oldestFetchedEventTime = 0;
  transcript.autoPageBackCount = 0; // reset the blank-page recovery budget per session
  // Invalidate any in-flight "load earlier" page of the previous session and
  // free the flag so the new session can page back immediately.
  transcript.earlierGen++;
  transcript.earlierLoading = false;
  mobileEnterChat();
  stopPreviewPolling();
  const activeCard = setActiveSessionCard(key, node);
  if (activeCard) updateCardUnreadChip(activeCard, 0);
  renderMainShell();
  fetchSessionRuns(key, node); // populate the run-history timeline (best-effort)
  fetchGitState(key, node); // populate the branch / worktree chip (best-effort)
  navRebuild(); // clear stale nav state before async events arrive
  const draftInput = document.getElementById('msg-input');
  if (draftInput && perSession.drafts[key]) {
    setMsgValue(draftInput, perSession.drafts[key]);
  }

  const changed = prevKey !== key || prevNode !== node;
  if (wsm.isConnected()) {
    if (changed) sessionStream.unsubscribe();
    sessionStream.lastEventTimeWs = 0;
    sessionStream.subscribe(key, node);
    if (timers.events) { clearInterval(timers.events); timers.events = null; }
  } else {
    fetchEvents(true);
    if (timers.events) clearInterval(timers.events);
    timers.events = setInterval(() => fetchEvents(false), 1000);
  }
}

// --- Markdown export (UX P2) ---

// Kinds the export drops (clievent kindTable's MarkdownIgnore column says why).
const MARKDOWN_EXPORT_IGNORE = new Set(NZ_CONTRACT.ENUMS.EVENT_TYPE_MD_IGNORE);

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
    } else if (e.type === 'system') {
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

// fetchAllSessionEvents returns { events, truncated } (or { status } on a
// non-2xx first page). `truncated` is set whenever the export is known or
// suspected to be incomplete — page cap hit, a later page failed or was
// malformed, or a page with nothing new said has-more — so the caller must
// warn rather than claim a full export. A remote session pages the same way:
// its node serves the `before=` pages through the relay.
//
// Cursor: `before = oldest + 1`, NOT `before = oldest`. Both the ring
// (EntriesBefore) and the disk sources filter strictly `Time < before`, so a
// same-millisecond sibling group (one CLI frame's blocks) split by the ring
// edge or a 500-entry page edge would lose its older members for good under a
// strict cursor. Re-admitting the watermark millisecond and dropping what we
// already hold by eventIdentityKey keeps every sibling; progress is measured by
// "new entries after dedup", not by the cursor moving.
async function fetchAllSessionEvents(key, node, headers) {
  const remote = !!(node && node !== 'local');
  const base = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(key) +
    (remote ? '&node=' + encodeURIComponent(node) : '');
  const r = await fetch(base, { headers });
  if (!r.ok) return { status: r.status };
  let events = await r.json();
  if (!Array.isArray(events)) events = [];
  if (events.length === 0) return { events, truncated: false };

  const seen = new Set(events.map(eventIdentityKey));
  let truncated = false;
  let oldest = (events[0] && events[0].time) || 0;
  for (let pages = 0; oldest > 0; pages++) {
    if (pages >= EXPORT_MAX_PAGES) { truncated = true; break; }
    const pr = await fetch(base + '&before=' + (oldest + 1) + '&limit=' + EXPORT_PAGE_LIMIT, { headers });
    if (!pr.ok) { truncated = true; break; }
    const page = await pr.json();
    if (!Array.isArray(page)) { truncated = true; break; }
    const hm = hasMoreHeader(pr);
    const fresh = page.filter(e => {
      const k = eventIdentityKey(e);
      if (seen.has(k)) return false;
      seen.add(k);
      return true;
    });
    if (fresh.length === 0) {
      // has-more=1 here is a degraded read or a same-ms flood wider than a
      // page; an older server sends no header, so only a full page is suspect.
      truncated = hm === null ? page.length >= EXPORT_PAGE_LIMIT : hm;
      break;
    }
    events = fresh.concat(events);
    const pageOldest = (fresh[0] && fresh[0].time) || 0;
    if (!pageOldest) break; // untimed head reached; nothing older to cursor on
    if (hm === false) break;
    oldest = pageOldest;
  }
  return { events, truncated };
}

async function downloadSessionMarkdown() {
  if (!selection.key) return;
  if (transcript.exportInFlight) return;
  transcript.exportInFlight = true;
  // Capture identity up front: the pager may take several round trips and
  // the operator can switch sessions meanwhile — the export still belongs to
  // the session whose button was clicked.
  const key = selection.key;
  const node = selection.node;
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
    const s = sessionList.sessionsData[sid(key, node)] || {};
    const keyParts = (key || '').split(':');
    const title = s.user_label || s.summary || s.last_prompt ||
      keyTailDisplay(keyParts) || key || '';
    const md = formatSessionMarkdown({
      title: title,
      key: key,
      node: node,
      cli: s.cli_name ? (s.cli_name + (s.cli_version ? ' v' + s.cli_version : '')) : '',
      workspace: s.workspace || perSession.workspaces[key] || '',
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
    transcript.exportInFlight = false;
  }
}

// mainHeaderHtml builds the .main-header block (title, rename/export buttons,
// detail line with the empty #header-git / #header-effort / #header-runstats
// mounts) for session snapshot `s`. Shared by renderMainShell (full shell) and
// renderMainHeader (header-only repaint) so the two can never drift.
function mainHeaderHtml(/** @type {SessionSnapshot} */ s) {
  const keyParts = (selection.key || '').split(':');
  const agentIsGeneric = !s.agent || s.agent === 'general';
  // Primary title: user_label (operator-set rename) > summary > latest prompt
  // > agent name > key tail.
  const displayName = s.user_label || s.summary || s.last_prompt || (agentIsGeneric ? '' : s.agent) || keyTailDisplay(keyParts) || selection.key || '';

  // Detail line: left = CLI name + version, middle = backend chip (multi-
  // backend mode only) + IM origin chip (only for real IM threads —
  // feishu/slack/discord/weixin), right = cost (formatted per session's
  // cost_unit). originBadgeHtml / backendChipHtml return '' when the
  // session/deployment doesn't warrant a chip so the layout stays clean.
  const effCLIName = s.cli_name || backendDisplayName(perSession.backends[selection.key]) || serverInfo.defaultCLIName;
  const effCLIVersion = s.cli_version || backendDisplayVersion(perSession.backends[selection.key]) || serverInfo.defaultCLIVersion;
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
    (!sessionList.sessionsData[sid(selection.key, selection.node)] && perSession.pendingTuning[selection.key]
      ? (perSession.pendingTuning[selection.key].model || '') : '');
  const compactModel = rawModel
    .replace(/^(global|us|eu|apac)\.anthropic\./, '') // strip Bedrock inference-profile prefix
    .replace(/-(\d+)-(\d+)/, '-$1.$2')          // 4-7 → 4.7 (matches kiro list)
    .replace(/\[(\d+m)\]$/i, ' $1');            // [1m] → " 1m"
  const modelLabel = rawModel
    ? '<span class="model-label nz-clickable" id="header-model" data-action="tuning-model" title="' + escAttr(rawModel + ' — 点击切换模型') + '">· ' + esc(compactModel) + '</span>'
    : '<span class="model-label model-label-unset nz-clickable" id="header-model" data-action="tuning-model" title="model 未在 system/init 上报；可能仍在 spawn 中 — 点击可指定模型">· (模型未配置)</span>';
  const headerOriginBadge = originBadgeHtml(selection.key);
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
  const canRename = selection.key && !isDiscoveredKey(selection.key);
  const renameBtn = canRename
    ? '<button type="button" class="btn-rename" data-action="session-rename" title="重命名会话" aria-label="重命名会话">' + ICONS.edit + '</button>'
    : '';
  // UX P2 Markdown export: any session that has an addressable key can be
  // exported — no dependency on managed status because the /api/sessions/events
  // endpoint serves both managed and discovered keys uniformly. The button
  // shares the .btn-rename hover-reveal treatment so the header stays calm
  // by default.
  const downloadBtn = selection.key
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
        '<span class="detail-exit" id="header-exit">' + sessionExitChipHtml(s.state, s.death_reason, s.death_detail, s.startup_failure) + '</span>' +
        // Git branch / worktree chip. Built empty here and filled
        // asynchronously by renderGitChip once /api/sessions/git resolves;
        // stays empty (collapses via :empty) for non-repo workspaces and
        // remote-node sessions.
        '<span class="detail-git" id="header-git"></span><span class="detail-pr" id="header-pr"></span>' +
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
  const s = sessionList.sessionsData[sid(selection.key, selection.node)] || {};
  header.outerHTML = mainHeaderHtml(s);
  // The mounts inside the header were just emptied — repaint from cache /
  // refetch exactly as renderMainShell's tail does.
  repaintGitChip();
  setHeaderEffortChip();
  setHeaderSpawnDiagChip();
  setHeaderOverlayDriftChip();
  setHeaderPRChip();
  fetchSessionRuns(selection.key, selection.node);
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
  const s = sessionList.sessionsData[sid(selection.key, selection.node)] || {};

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
    '<div class="events" id="events-scroll" role="log" aria-live="off">' + (s.state === 'running' ? '<div class="empty-state loading-indicator">\u6b63\u5728\u52a0\u8f7d\u4e8b\u4ef6\u2026</div>' : '') + '</div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button type="button" data-action="nav-msg" data-dir="prev" id="nav-prev" title="\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2191)" aria-label="\u8df3\u5230\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navUp + '</button>' +
      '<span class="nav-counter" id="nav-counter" data-action="nav-show-list" title="\u70b9\u51fb\u67e5\u770b\u5168\u90e8\u7528\u6237\u6d88\u606f"></span>' +
      '<button type="button" data-action="nav-msg" data-dir="next" id="nav-next" title="\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2193)" aria-label="\u8df3\u5230\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navDown + '</button>' +
    '</div>' +
    '<div class="running-banner nz-hidden" id="running-banner">' +
      '<div class="rb-tool-row">' +
        '<span class="running-status"><span class="running-dot" aria-hidden="true"></span><span id="tool-activity">处理中...</span></span>' +
        '<span class="rb-elapsed" id="rb-elapsed" aria-hidden="true"></span>' +
      '</div>' +
      '<div class="rb-thinking-summary nz-hidden" id="rb-thinking-summary"></div>' +
      '<div class="rb-agents" id="rb-agents"></div>' +
      '<div class="rb-stats nz-hidden" id="rb-stats"></div>' +
    '</div>' +
    '<div class="input-area' + (composer.voiceInputMode ? ' voice-mode' : '') + '" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<button type="button" class="btn-icon" data-action="file-picker" title="上传图片或 PDF" aria-label="上传图片或 PDF">' + ICONS.attach + '</button>' +
        '<button type="button" class="btn-icon btn-mic" id="btn-mic" data-action="input-mode-toggle" title="' + (composer.voiceInputMode ? '\u5207\u6362\u952e\u76d8' : '\u5207\u6362\u8bed\u97f3') + '" aria-label="' + (composer.voiceInputMode ? '\u5207\u6362\u5230\u952e\u76d8\u8f93\u5165' : '\u5207\u6362\u5230\u8bed\u97f3\u8f93\u5165') + '">' + (composer.voiceInputMode ? ICONS.keyboard : ICONS.mic) + '</button>' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message..." data-action-keydown="msg-input-key" data-action-compositionend="msg-input-compend"></div>' +
        '<button type="button" class="btn-hold-talk" id="btn-hold-talk" title="\u6309\u4f4f\u8bf4\u8bdd\u6539\u5f55\u97f3" aria-label="\u6309\u4f4f\u8bf4\u8bdd\u5f00\u59cb\u5f55\u97f3">\u6309\u4f4f\u8bf4\u8bdd</button>' +
        '<button type="button" class="btn-icon btn-send" id="btn-send" data-action="msg-send" title="发送" aria-label="发送消息">' + ICONS.send + '</button>' +
        '<button type="button" class="btn-icon btn-stop" id="btn-stop" data-action="session-interrupt" title="停止" aria-label="停止当前回合">' + ICONS.stop + '</button>' +
      '</div>' +
      '<div class="input-hints">Enter send &middot; Shift+Enter newline &middot; Esc interrupt</div>' +
      '<input type="file" id="file-input" accept="image/*,application/pdf" multiple class="nz-hidden" data-action-change="file-input-change">' +
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
  setHeaderPRChip();
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
  const s = serverInfo.lastStatsSnapshot || {};
  const aboutRows = [];
  if (s.version_tag) aboutRows.push(['naozhi', s.version_tag]);
  if (s.cli_name) aboutRows.push([s.cli_name, s.cli_version || '—']);
  if (serverInfo.cliBackends && Array.isArray(serverInfo.cliBackends.backends) && serverInfo.cliBackends.backends.length > 0) {
    aboutRows.push(['Backends', serverInfo.cliBackends.backends.map(function (b) {
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

/* ===== WS receive: auth failures and system-daemon runs (the session frames register in event_stream.js and session_list.js) ===== */

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
wsm.onAuthFail((msg) => {
  const raw = (msg.error || '').toString();
  if (raw.toLowerCase().includes('too many')) {
    let retryAfter = parseInt(msg.retry_after, 10);
    if (!Number.isFinite(retryAfter) || retryAfter <= 0) retryAfter = 60;
    startWSAuthRetryCountdown(retryAfter);
  } else {
    showAPIError('WebSocket 鉴权', 401, raw || '令牌无效');
  }
});
// System-daemon run boundary. fetchSystemDaemons is the only path that
// updates the 系统 rail attention badge; without this a daemon failing in
// the background never lit the badge until the operator opened the view.
// Honour the RNEW-UX-014 hidden-tab suspension.
const daemonRun = () => {
  if (document.hidden) return;
  fetchSystemDaemons().then(() => {
    if (ui.activeView === 'system') renderSystemView();
  }).catch(() => {});
};
const sysRun = (msg) => msg.subsystem === 'sysession';
wsm.on(NZ_CONTRACT.WS.run_started, daemonRun, sysRun);
wsm.on(NZ_CONTRACT.WS.run_ended, daemonRun, sysRun);

/* ===== WS Helper Functions ===== */

function updateHeaderCLI() {
  const s = sessionList.sessionsData[sid(selection.key, selection.node)] || {};
  // #2437: only touch the #header-cli span painted by headerCLILabelHtml.
  // Never rewrite the whole left container — #header-model lives next door.
  const el = document.getElementById('header-cli');
  if (!el) return;
  // Fallback chain mirrors renderMainShell — see backendDisplayName godoc
  // for why pending sessions need the sessionBackends lookup before the
  // global defaultCLIName fallback.
  const name = s.cli_name || backendDisplayName(perSession.backends[selection.key]) || serverInfo.defaultCLIName;
  const version = s.cli_version || backendDisplayVersion(perSession.backends[selection.key]) || serverInfo.defaultCLIVersion;
  // Same display rule as renderMainShell (D5): version lives in the hover
  // title only, the text is just the backend name.
  const text = name || '';
  const title = (name && version) ? name + ' v' + version : '';
  if (el.textContent !== text) el.textContent = text;
  if (title !== (el.getAttribute('title') || '')) {
    if (title) el.setAttribute('title', title); else el.removeAttribute('title');
  }
}

/* ===== Cron Tab =====
   The cron (定时任务) view lives in cron_view.js and the modules it imports
   (cron_live.js owns the live stream). They import this file, never the
   reverse; dashboard reaches them through nz.bus ('cron:open-panel'), hooks
   and the wsm.on / wsm.onReady registrations they make at load. */

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
  const hasSessions = (Object.keys(sessionList.sessionsData || {}).length > 0) ||
    (Object.keys(perSession.workspaces || {}).length > 0);
  const hasProjects = (sessionList.projectsData && sessionList.projectsData.length > 0);
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

// Fill dashboard's shell slots before the bootstrap below: the pollers and
// the discovered-session scan it starts may call back up at once.
registerShell({ renderMainHeader, renderMainShell, selectSession, setActivityView });
initSplitWidth();
initSidebarCollapsed();

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
timers.sessionPoll = setInterval(fetchSessions, 5000);
scanDiscovered();
timers.discoveredPoll = setInterval(scanDiscovered, 30000);
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
    if (timers.sessionPoll) { clearInterval(timers.sessionPoll); timers.sessionPoll = null; }
    if (timers.discoveredPoll) { clearInterval(timers.discoveredPoll); timers.discoveredPoll = null; }
    if (timers.events) { clearInterval(timers.events); timers.events = null; }
    stopSidebarTimeTick();
    // #1770: also pause the WS keep-alive ping while the tab is hidden. The
    // 30s app-level ping wakes the mobile radio every 30s for nothing —
    // connection liveness is independently maintained by the server's
    // protocol-level Ping/Pong (writePump, wsPingPeriod≈54s), so dropping the
    // app ping loses no liveness detection. Re-armed in startPollers on resume.
    if (wsm.pingTimer) wsm.cleanup();
  };
  const startPollers = () => {
    if (!timers.sessionPoll) {
      // #2431: a half-open socket (lid close / network switch) still reports
      // CONNECTED, so no frames arrive and no fallback poll is armed below;
      // the version gate would then short-circuit this one-shot fetch too.
      // Zero lastVersion so returning to the tab always repaints once.
      sessionList.lastVersion = 0;
      fetchSessions(); // immediate refresh on resume so UI is not stale
      // #2431: the 5 s sessions poll is a WS-outage fallback. Over a live
      // socket session_state pushes already drive the sidebar; arming the
      // interval here made it run alongside WS until the next reconnect.
      if (!(wsm.state === WS_STATES.CONNECTED)) {
        timers.sessionPoll = setInterval(fetchSessions, 5000);
      }
    }
    // Same rationale as the turn-boundary refresh in onSessionState: the branch
    // can change without the workspace path changing, and an operator switching
    // branches in their own terminal produces no turn at all. Re-resolving when
    // the tab regains focus catches that case without a dedicated poller.
    if (selection.key) invalidateGitState(selection.key, selection.node);
    if (!timers.discoveredPoll) {
      timers.discoveredPoll = setInterval(scanDiscovered, 30000);
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
    if (!timers.events && selection.key && wsm.state !== WS_STATES.CONNECTED) {
      fetchEvents(false);
      timers.events = setInterval(() => fetchEvents(false), 1000);
    }
    // #1770: re-arm the WS ping we paused in stopPollers, but only when the
    // socket is actually live — a dropped/offline socket has no ping to keep
    // and will re-arm via auth_ok on reconnect.
    if (!wsm.pingTimer && wsm.conn && wsm.conn.readyState === WebSocket.OPEN) {
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
  hooks.openLightboxGroup=function(list,start){
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
  hooks.openLightboxFromThumb=function(el){
    var box=el.closest&&el.closest('.event-images');
    var imgs=box?Array.prototype.slice.call(box.querySelectorAll('img[data-full]')):[el];
    var list=imgs.map(function(i){return{full:i.dataset.full||i.src,thumb:i.dataset.thumb||i.src}});
    // indexOf can only miss if `el` somehow lacks data-full (not rendered by
    // eventHtml); degrade to the first image rather than refusing to open.
    hooks.openLightboxGroup(list,Math.max(0,imgs.indexOf(el)));
  };
  // Compatibility shell: the historical single-image entry point. Kept so
  // any caller outside eventHtml (or user bookmarklets) keeps working.
  // Delegated thumbnail click handler — registered once on document, so it
  // survives the chat transcript's innerHTML re-renders (eventHtml emits the
  // thumbnails without inline onclick; RFC lightbox-gallery-nav §3).
  document.addEventListener('click',function(e){
    var t=e.target&&e.target.closest&&e.target.closest('.event-images img[data-full]');
    if(t)hooks.openLightboxFromThumb(t);
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
    splitDock.enter();
    // Opened last → stack on top of the preview pane if both are docked.
    splitDock.bringToFront('scratch');
  }
  function hideDrawer() {
    drawer.classList.remove('visible');
    splitDock.exit();
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
        if (typeof sessionList.lastVersion !== 'undefined') sessionList.lastVersion = 0;
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
  hooks.getActiveScratchKey = function() {
    return (state && state.key) ? state.key : '';
  };

  // Exposed so the view-router (setActivityView) can tear the 追问 drawer down
  // when leaving the chat view — the drawer is position:fixed and would
  // otherwise float over assets/cron/settings. closeScratch handles the
  // no-op-when-closed case internally.
  hooks.closeScratchDrawer = function() { closeScratch(true); };

  // Expose the global used by the ↗ button in eventHtml.
  hooks.askAside = function(btn) {
    if (!btn) return;
    const raw = btn.getAttribute('data-raw') || '';
    const msgTime = Number(btn.getAttribute('data-msg-time') || 0);
    if (!raw || raw.length < 1) return;
    if (!selection.key) {
      showToast('请先选择会话');
      return;
    }
    // Derive agentId from the current session key (4th segment) so the
    // server can inherit the matching agent registration.
    const parts = String(selection.key).split(':');
    const agentId = parts.length >= 4 ? parts[3] : 'general';
    openScratch(raw, agentId, selection.key, msgTime);
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
    runPendingAsync();
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




export { setActivityView };

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
  'msg-input-compend': () => { composer.lastCompositionEnd = Date.now(); },
  'msg-send': () => sendMessage(),
  'session-interrupt': () => interruptSession(),
  'file-input-change': (el) => handleFiles(el.files),
  'ask-option-toggle': (el) => onAskOptionToggle(el),
  'ask-submit': (el) => onAskSubmit(el),
  'event-copy': (el) => copyEventContent(el),
  'ask-aside': (el) => hooks.askAside(el),
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

// Read by the e2e suite through test/e2e/e2e-shim.js.
export { maybeShowOnboarding, renderMainShell, selectSession, sessionCardKey, toggleHistory, updateHeaderCLI };
