// Service worker registration
if('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(()=>{});

let selectedKey = null;
let eventTimer = null;
let lastEventTime = 0;
let lastRenderedEventTime = 0;
let lastCompositionEnd = 0;
let sessionsData = {};
let allSessionsCache = [];
let sessionFirstSeen = (function() { try { return JSON.parse(localStorage.getItem('nz_firstSeen') || '{}'); } catch(_) { return {}; } })();
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
let sessionPollTimer = null;
let discoveredPollTimer = null;
let discoveredItems = []; // discovered sessions, merged into sidebar
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
let historySessionsData = []; // from API history_sessions (all filesystem sessions)

function getToken() { return ''; }
function setToken(t) { /* token stored in HttpOnly cookie only */ }

function removePendingSession(key) {
  delete sessionWorkspaces[key];
  delete sessionNodes[key];
}

async function fetchSessions() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/sessions', { headers });
    if (r.status === 401 || r.status === 403) {
      if (!document.querySelector('.modal-overlay')) showAuthModal();
      // Explicit falsy return lets maybeShowOnboarding know auth is still
      // unresolved and skip its overlay, avoiding stacking onboarding on
      // top of the auth modal.
      return false;
    }
    if (!r.ok) return false;
    const data = await r.json();
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
    (data.sessions || []).forEach(s => {
      const n = s.node || 'local';
      sessionsData[sid(s.key, n)] = s;
      backendKeys.add(s.key);
    });

    // Remove pending sessions that now exist in backend
    for (const key of Object.keys(sessionWorkspaces)) {
      if (backendKeys.has(key)) {
        delete sessionWorkspaces[key];
        delete sessionNodes[key];
      }
    }

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
          data.sessions.push({
            key: key,
            state: 'new',
            platform: parts[0] || 'dashboard',
            agent: pendingAgent,
            workspace: sessionWorkspaces[key],
            last_active: 0,
            last_prompt: '',
            node: sessionNodes[key] || 'local',
            project: matchProject(sessionWorkspaces[key]),
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
  // updateStatusBar() call above (which tail-calls updateNodeSelector so a
  // 1s status tick keeps the trigger dot live during disconnect); session
  // counts in the dropdown therefore lag by at most one poll cycle, which
  // only matters while the dropdown is open — an acceptable trade for not
  // paying two full dropdown repaints per poll.

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

  // Stamp first-seen time for each session (stable sort anchor).
  // Once recorded, position never changes regardless of activity.
  let fsChanged = false;
  allItems.forEach(s => {
    const id = (s.node || 'local') + ':' + s.key;
    if (!sessionFirstSeen[id]) { sessionFirstSeen[id] = s.last_active || Date.now(); fsChanged = true; }
  });
  // Prune entries for sessions that no longer exist
  const activeIds = new Set(allItems.map(s => (s.node || 'local') + ':' + s.key));
  for (const k of Object.keys(sessionFirstSeen)) {
    if (!activeIds.has(k)) { delete sessionFirstSeen[k]; fsChanged = true; }
  }
  if (fsChanged) { try { localStorage.setItem('nz_firstSeen', JSON.stringify(sessionFirstSeen)); } catch(_) {} }

  // Sort: running first (still active), then by first-seen desc (newest on top, position stable)
  allItems.sort((a, b) => {
    const aRun = a.state === 'running' ? 0 : 1;
    const bRun = b.state === 'running' ? 0 : 1;
    if (aRun !== bRun) return aRun - bRun;
    const aFS = sessionFirstSeen[(a.node || 'local') + ':' + a.key] || 0;
    const bFS = sessionFirstSeen[(b.node || 'local') + ':' + b.key] || 0;
    return bFS - aFS;
  });

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
    const matched = filterSessionsByQuery(allItems, filterQuery);
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
    allItems.forEach(s => {
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
    if (groupKeys.length > 0) {
      // Pre-compute per-group sort keys once — avoids repeated map lookups
      // inside the sort comparator (fav flag, max firstSeen, display name).
      // This keeps comparator at O(1) scalar comparisons rather than
      // O(M + map-lookup) per call. Also sidesteps the Math.max(...spread)
      // call-stack limit that would eventually RangeError at huge session
      // counts.
      const sortKeys = {};
      groupKeys.forEach(k => {
        const g = groups[k];
        const p = projIndex[k];
        let m = 0;
        for (const s of g.items) {
          const fs = sessionFirstSeen[(s.node || 'local') + ':' + s.key] || 0;
          if (fs > m) m = fs;
        }
        // Tier order: favorite projects → regular projects → fallback
        // (workspace-basename) groups. Fallback groups represent ad-hoc
        // quick-session folders that were never registered as projects, so
        // they should always sink below real project sections.
        const tier = g.fallback ? 2 : ((p && p.favorite) ? 0 : 1);
        sortKeys[k] = { tier, first: m, name: g.name };
      });
      groupKeys.sort((a, b) => {
        const ka = sortKeys[a], kb = sortKeys[b];
        if (ka.tier !== kb.tier) return ka.tier - kb.tier;
        if (ka.first !== kb.first) return kb.first - ka.first;
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
        // the header's `sh-new` `+` button already provides the create
        // affordance, so a duplicate "New session in X" CTA would just add
        // visual noise.
      });
      if (ungrouped.length > 0) {
        // Final catch-all: sessions with no project name AND no workspace
        // (rare — usually transient takeover/planner edge cases). The old
        // "Other" label predated the workspace-basename fallback; keep a
        // bucket but label it clearly so it isn't mistaken for a real group.
        html += '<div class="section-header"><span class="sh-name">未分组</span></div>';
        html += ungrouped.map(sessionCardHtml).join('');
      }
    } else {
      html = allItems.map(sessionCardHtml).join('');
    }
  }

  // R110-P2 empty-state CTA: keep the legacy "no sessions" text (E2E asserts
  // it via toContain) but add a visible call-to-action so first-time users
  // aren't left staring at a dead sidebar. createNewSession is the same handler
  // the header `+` button invokes. NOT emitted in filter mode — the
  // filter-specific empty state ('没有匹配的会话') already covers that path.
  if (!html && !filterActive) html = '<div class="no-sessions">no sessions<br><button type="button" class="no-sessions-cta" onclick="createNewSession()">+ 开启你的第一个会话</button></div>';
  list.innerHTML = html;
  // Restore scroll on the next frame so the browser finishes layout first;
  // synchronous assignment after innerHTML can visibly jump on slow devices.
  requestAnimationFrame(() => {
    list.scrollTop = scrollTop;
  });

  // Update history badge (filesystem history sessions, deduplicated against workspace)
  const hBadge = document.getElementById('history-badge');
  if (hBadge) {
    const workspaceIDs = new Set(allSessionsCache.filter(s => s.session_id).map(s => s.session_id));
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
function initSidebarSearch() {
  const input = document.getElementById('sidebar-search-input');
  if (input) {
    input.addEventListener('input', () => {
      // Re-render locally against the cached /api/sessions payload so
      // rapid typing doesn't DoS the server with per-keystroke requests.
      // When no data has landed yet (first load), fall through to a
      // debounced fetch as a degraded bootstrap.
      if (_lastSidebarData) {
        renderSidebar(_lastSidebarData);
      } else {
        debouncedFetchSessions();
      }
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
  const collapseBtn = '<button type="button" class="' + cCls + '" data-key="' + escAttr(ck) + '" title="' + cTitle + ' ' + escAttr(p.name) + '" aria-label="' + cTitle + ' ' + escAttr(p.name) + '" aria-expanded="' + (collapsed ? 'false' : 'true') + '" onclick="event.stopPropagation();toggleProjectCollapsed(this.dataset.key)">' + CHEVRON_SVG + '</button>';
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
  const collapseBtn = '<button type="button" class="' + cCls + '" data-key="' + escAttr(ck) + '" title="' + cTitle + ' ' + escAttr(p.name) + '" aria-label="' + cTitle + ' ' + escAttr(p.name) + '" aria-expanded="' + (collapsed ? 'false' : 'true') + '" onclick="event.stopPropagation();toggleProjectCollapsed(this.dataset.key)">' + CHEVRON_SVG + '</button>';
  const countBadge = collapsed && count > 0 ? '<span class="sh-count">' + count + '</span>' : '';

  // No longer pass `data-fav` — the handler derives current state from the
  // authoritative `projectsData` at click time, avoiding a stale DOM attribute
  // that could cause a fast second click (before re-render) to send a
  // redundant or wrong-polarity toggle.
  const starBtn = '<button type="button" class="' + starCls + '" data-name="' + escAttr(p.name) + '" data-node="' + escAttr(node) + '" title="' + starTitle + '" aria-label="' + starTitle + ' ' + escAttr(p.name) + '" onclick="event.stopPropagation();toggleFavorite(this.dataset.name,this.dataset.node)">' + STAR_SVG + '</button>';

  let ghBtn = '';
  if (p.github) {
    const url = p.git_remote_url || '';
    // R110-P2 tooltip clarity: the old "GitHub: <url>" left the CTA implicit
    // — click-to-open was only discoverable by trial. Lead with the verb
    // "在 GitHub 打开仓库" so the affordance is explicit; append the URL so
    // operators can still eyeball the remote for the common case where
    // they're verifying the repo match before clicking.
    ghBtn = '<button type="button" class="sh-btn github-on" data-url="' + escAttr(url) + '" title="在 GitHub 打开仓库：' + escAttr(url) + '" aria-label="在 GitHub 打开仓库 ' + escAttr(p.name) + '" onclick="event.stopPropagation();showGitRemote(this.dataset.url)">' + GITHUB_SVG + '</button>';
  }

  // Compact `+` icon in every section header so creation is always one click
  // away — also serves as the only per-project create affordance for empty
  // favorite groups (the old full-width "New session in X" CTA under the
  // header was removed as redundant).
  const newBtn = '<button type="button" class="sh-btn sh-new" data-name="' + escAttr(p.name) + '" data-node="' + escAttr(node) + '" title="在 ' + escAttr(p.name) + ' 中新建会话" aria-label="在 ' + escAttr(p.name) + ' 中新建会话" onclick="event.stopPropagation();newSessionInProject(this.dataset.name,this.dataset.node)"><svg viewBox="0 0 24 24" aria-hidden="true"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg></button>';

  const collapsedCls = collapsed ? ' is-collapsed' : '';
  return '<div class="section-header' + collapsedCls + '" role="group" aria-label="' + escAttr(p.name) + '">' +
    collapseBtn + starBtn +
    '<span class="sh-name" title="' + escAttr(p.name) + '">' + esc(p.name) + '</span>' +
    countBadge +
    ghBtn +
    newBtn +
    '</div>';
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
    const r = await fetch('/api/projects/favorite?' + qs, { method: 'POST', headers });
    if (!r.ok) {
      showAPIError(next ? '收藏项目' : '取消收藏', r.status, '');
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
  } catch (e) {
    showNetworkError(next ? '收藏项目' : '取消收藏', e);
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
  const safe = /^(https?:\/\/|git:\/\/)/i.test(url);
  if (safe) {
    window.open(url, '_blank', 'noopener,noreferrer');
    return;
  }
  // Fallback: surface the URL but truncated to keep credentials embedded in
  // ssh URLs from being broadcast via the toast surface.
  const shown = url.length > 80 ? url.slice(0, 77) + '…' : url;
  showToast('GitHub remote: ' + shown);
}

function newSessionInProject(name, node) {
  const p = projectsData.find(x => x.name === name && (x.node || 'local') === (node || 'local'));
  if (!p) return;
  doCreateInProject(p.path, p.name, p.node || 'local');
}

// --- History Popover ---

let activePopover = null;

function closeHistoryPopover() {
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
  const workspaceIDs = new Set(
    allSessionsCache.filter(s => s.session_id).map(s => s.session_id)
  );
  const merged = historySessionsData
    .filter(r => !workspaceIDs.has(r.session_id))
    .map(r => ({
      key: '_history:' + r.session_id, node: 'local', source: 'recent',
      session_id: r.session_id, last_active: r.last_active || 0,
      prompt: r.last_prompt || r.summary || '',
      project: r.project || matchProject(r.workspace), tool: '',
    }));
  merged.sort((a, b) => b.last_active - a.last_active);

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
      (s.prompt ? '<div class="hp-prompt" title="' + escAttr(s.prompt) + '">' + esc(s.prompt) + '</div>' : '<div class="hp-prompt" style="color:#6e7681">未命名</div>') +
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
// dashboard.html `.sc-origin.kind-*`. Non-IM prefixes (dashboard, local,
// cron, scratch_*, planner) intentionally do NOT appear here — originBadgeInfo
// returns null for them so those sessions don't grow a misleading "外部
// 来源" chip.
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

function cliIcon(name) {
  if (name === 'kiro') return '<svg class="sc-cli-icon" viewBox="0 0 16 16" fill="none"><path d="M8 1L14 5.5V10.5L8 15L2 10.5V5.5L8 1Z" fill="#f97316" opacity="0.85"/><path d="M6 5.5V10.5M6 8H9.5L6 5.5M6 8L9.5 10.5" stroke="#fff" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  // Default: official Claude logomark (from claude.ai/favicon.svg)
  return '<svg class="sc-cli-icon" viewBox="0 0 248 248" fill="none"><path d="M52.4285 162.873L98.7844 136.879L99.5485 134.602L98.7844 133.334H96.4921L88.7237 132.862L62.2346 132.153L39.3113 131.207L17.0249 130.026L11.4214 128.844L6.2 121.873L6.7094 118.447L11.4214 115.257L18.171 115.847L33.0711 116.911L55.485 118.447L71.6586 119.392L95.728 121.873H99.5485L100.058 120.337L98.7844 119.392L97.7656 118.447L74.5877 102.732L49.4995 86.1905L36.3823 76.62L29.3779 71.7757L25.8121 67.2858L24.2839 57.3608L30.6515 50.2716L39.3113 50.8623L41.4763 51.4531L50.2636 58.1879L68.9842 72.7209L93.4357 90.6804L97.0015 93.6343L98.4374 92.6652L98.6571 91.9801L97.0015 89.2625L83.757 65.2772L69.621 40.8192L63.2534 30.6579L61.5978 24.632C60.9565 22.1032 60.579 20.0111 60.579 17.4246L67.8381 7.49965L71.9133 6.19995L81.7193 7.49965L85.7946 11.0443L91.9074 24.9865L101.714 46.8451L116.996 76.62L121.453 85.4816L123.873 93.6343L124.764 96.1155H126.292V94.6976L127.566 77.9197L129.858 57.3608L132.15 30.8942L132.915 23.4505L136.608 14.4708L143.994 9.62643L149.725 12.344L154.437 19.0788L153.8 23.4505L150.998 41.6463L145.522 70.1215L141.957 89.2625H143.994L146.414 86.7813L156.093 74.0206L172.266 53.698L179.398 45.6635L187.803 36.802L193.152 32.5484H203.34L210.726 43.6549L207.415 55.1159L196.972 68.3492L188.312 79.5739L175.896 96.2095L168.191 109.585L168.882 110.689L170.738 110.53L198.755 104.504L213.91 101.787L231.994 98.7149L240.144 102.496L241.036 106.395L237.852 114.311L218.495 119.037L195.826 123.645L162.07 131.592L161.696 131.893L162.137 132.547L177.36 133.925L183.855 134.279H199.774L229.447 136.524L237.215 141.605L241.8 147.867L241.036 152.711L229.065 158.737L213.019 154.956L175.45 145.977L162.587 142.787H160.805V143.85L171.502 154.366L191.242 172.089L215.82 195.011L217.094 200.682L213.91 205.172L210.599 204.699L188.949 188.394L180.544 181.069L161.696 165.118H160.422V166.772L164.752 173.152L187.803 207.771L188.949 218.405L187.294 221.832L181.308 223.959L174.813 222.777L161.187 203.754L147.305 182.486L136.098 163.345L134.745 164.2L128.075 235.42L125.019 239.082L117.887 241.8L111.902 237.31L108.718 229.984L111.902 215.452L115.722 196.547L118.779 181.541L121.58 162.873L123.291 156.636L123.14 156.219L121.773 156.449L107.699 175.752L86.304 204.699L69.3663 222.777L65.291 224.431L58.2867 220.768L58.9235 214.27L62.8713 208.48L86.304 178.705L100.44 160.155L109.551 149.507L109.462 147.967L108.959 147.924L46.6977 188.512L35.6182 189.93L30.7788 185.44L31.4156 178.115L33.7079 175.752L52.4285 162.873Z" fill="#D97757"/></svg>';
}

function sessionCardHtml(s) {
  const sNode = s.node || 'local';
  const isActive = selectedKey === s.key && selectedNode === sNode;
  const isNew = s.state === 'new';
  const isCron = typeof s.key === 'string' && s.key.indexOf('cron:') === 0;
  const cls = 'session-card' + (isActive ? ' active' : '') + (isNew ? ' new-card' : '') + (isCron ? ' cron-card' : '');

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

  const cronBadge = isCron ? '<span class="sc-cron" title="Scheduled cron task">\u23F0 cron</span>' : '';
  const typeTag = s.source === 'terminal' ? sessionTypeTag(s.cli_name, s.entrypoint) : '';
  const agentCount = s.subagents ? s.subagents.length : 0;
  const agentBadge = agentCount > 0 ? '<span class="sc-agents">\u{1F916}\u00D7' + agentCount + '</span>' : '';
  // R110-P3 IM origin: show a small chip for sessions sourced from feishu /
  // slack / discord / weixin so operators can eyeball which cards are real
  // IM threads vs dashboard-local conversations. originBadgeHtml returns ''
  // for non-IM prefixes so the meta line stays clean for those.
  const originBadge = originBadgeHtml(s.key);
  const metaHtml = '<span class="sc-dot ' + dotCls + '"></span>' +
    '<span>' + esc(displayState) + '</span>' +
    nodeBadge +
    originBadge +
    cronBadge +
    typeTag +
    agentBadge;

  return '<div class="' + cls + '" role="listitem" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" tabindex="0" aria-label="' + escAttr(prompt + ' · ' + displayState) + '" onclick="selectSession(this.dataset.key,this.dataset.node)" onkeydown="sessionCardKey(event)">' +
    dismissBtn +
    icon +
    '<div class="sc-body">' +
      '<div class="sc-header">' +
        '<div class="sc-prompt" title="' + escAttr(prompt) + '">' + esc(prompt) + '</div>' +
        unreadBadge +
        (ago ? '<span class="sc-time"' + (absTime ? ' title="' + escAttr(absTime) + '"' : '') + '>' + ago + '</span>' : '') +
      '</div>' +
      '<div class="sc-meta">' + metaHtml + '</div>' +
    '</div>' +
  '</div>';
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
    previewRecentSession(key, sessionId);
  } catch (e) {
    showNetworkError('恢复会话', e);
  }
}

async function previewRecentSession(expectedKey, sessionId) {
  try {
    const headers = {};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch('/api/discovered/preview?session_id=' + encodeURIComponent(sessionId), { headers });
    if (!r.ok) return;
    if (selectedKey !== expectedKey) return; // user navigated away
    const entries = await r.json();
    if (!entries || entries.length === 0) return;
    renderEvents(entries);
  } catch (e) {
    console.error('previewRecentSession:', e);
  }
}

const STATUS_LABELS = { off: 'offline', connecting: 'connecting...', authenticating: 'authenticating...', connected: 'connected', disconnected: 'HTTP fallback', disconnected_retry: 'reconnecting...' };
const REMOTE_LABELS = { ok: 'connected', error: 'error', offline: 'offline', unreachable: 'unreachable' };
const VALID_DOT_CLASSES = { ok: 'ok', error: 'error', offline: 'offline', connecting: 'connecting', off: 'off', connected: 'connected', disconnected: 'disconnected', authenticating: 'authenticating' };

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

// _statusTickTimer drives the 1s repaint loop while WS is disconnected so
// the "已断开 N 秒" label advances without waiting for the next state
// transition. Started by _updateStatusTick when state != CONNECTED; stopped
// when state returns to CONNECTED. Repaint cost is O(N) where N = count of
// sidebar-status rows (<=1 + node count); negligible.
let _statusTickTimer = null;
function _updateStatusTick(state) {
  if (state === WS_STATES.CONNECTED) {
    if (_statusTickTimer) { clearInterval(_statusTickTimer); _statusTickTimer = null; }
    return;
  }
  if (!_statusTickTimer) {
    _statusTickTimer = setInterval(updateStatusBar, 1000);
  }
}

function updateStatusBar() {
  const container = document.getElementById('sidebar-status');
  // #sidebar-status 节点已在"底部让位给 session 列表"的迭代中删除。没节点就
  // 早退，但 updateNodeSelector 必须照常跑——它驱动顶部多节点下拉的显隐，
  // 跟 sidebar-status 是两码事，否则 multi-node 切换框会一起消失。
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
  mobileEnterChat();
  stopPreviewPolling();
  document.querySelectorAll('.session-card').forEach(el => {
    const nowActive = el.dataset.key === key && (el.dataset.node || 'local') === node;
    el.classList.toggle('active', nowActive);
    if (nowActive) updateCardUnreadChip(el, 0);
  });
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
      const r = await fetch('/api/discovered/close', {
        method: 'POST', headers,
        body: JSON.stringify({pid: d.pid, session_id: d.session_id || '', cwd: d.cwd || '', proc_start_time: d.proc_start_time || 0, node: node || ''})
      });
      if (!r.ok) {
        const text = await r.text().catch(() => '');
        showAPIError('关闭外部会话', r.status, text);
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

  try {
    const headers = {'Content-Type': 'application/json'};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const body = {key: key};
    if (node && node !== 'local') body.node = node;
    const r = await fetch('/api/sessions', {method: 'DELETE', headers, body: JSON.stringify(body)});
    if (!r.ok && r.status !== 404) {
      const text = await r.text().catch(() => '');
      showAPIError('删除会话', r.status, text);
      return;
    }
    delete sessionsData[sid(key, node)];
    if (selectedKey === key) {
      selectedKey = null;
      if (wsm.subscribedKey === key) wsm.unsubscribe();
      document.getElementById('main').innerHTML = mainEmptyHtml();
      wireQuickAskInput();
    }
    lastVersion = 0;
    debouncedFetchSessions();
  } catch (e) { showNetworkError('删除会话', e); }
}

// Operator-facing rename flow. Prompts for a new display label; empty input
// clears any prior label and falls back to the summary/last_prompt display
// chain. Uses PATCH /api/sessions/label so the mutation round-trips through
// the server and persists across reloads.
async function renameSession() {
  if (!selectedKey) return;
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};
  const current = s.user_label || '';
  const input = window.prompt('重命名会话（留空恢复默认标题，最多 128 字节）', current);
  if (input === null) return; // user cancelled
  const next = input.trim();
  if (next === current) return;
  try {
    const headers = {'Content-Type': 'application/json'};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const body = {key: selectedKey, label: next};
    if (selectedNode && selectedNode !== 'local') body.node = selectedNode;
    const r = await fetch('/api/sessions/label', {
      method: 'PATCH', headers,
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      const text = await r.text().catch(() => '');
      showAPIError('重命名', r.status, text);
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
  } catch (e) {
    showNetworkError('重命名', e);
  }
}

// --- Markdown export (UX P2) ---

// MARKDOWN_EXPORT_IGNORE captures event types that carry no user-visible
// content in the dashboard render path (tool_use + internal agent
// bookkeeping + the result envelope duplicated by streaming `text`
// events). The export pipeline drops them to keep the emitted document
// aligned with what the operator actually read in the UI.
const MARKDOWN_EXPORT_IGNORE = new Set(['tool_use', 'result', 'agent', 'task_start', 'task_progress', 'task_done', 'thinking']);

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

  // Detail line: left = CLI name + version, middle = IM origin chip (only
  // for real IM threads — feishu/slack/discord/weixin), right = cost
  // (hidden for kiro). originBadgeHtml returns '' for non-IM keys so the
  // chip is invisible on dashboard/cron/scratch/planner sessions.
  const effCLIName = s.cli_name || defaultCLIName;
  const effCLIVersion = s.cli_version || defaultCLIVersion;
  const cliLabel = effCLIName ? esc(effCLIName) + (effCLIVersion ? ' v' + esc(effCLIVersion) : '') : '';
  const headerOriginBadge = originBadgeHtml(selectedKey);
  const showCost = effCLIName !== 'kiro';
  const cost = s.total_cost || 0;
  const costText = '$' + (cost < 0.01 && cost > 0 ? cost.toFixed(4) : cost.toFixed(2));
  const costClass = 'detail-cost' + (cost >= 1 ? ' high-cost' : cost > 0 ? ' has-cost' : '');
  // R110-P3 cost tooltip: compute the same detail the live updater
  // (updateHeaderCost) writes, so the very first render isn't missing
  // hover content until a subsequent event refresh lands.
  const costTooltip = formatHeaderCostTooltip(s, selectedKey, selectedNode);
  const costTitleAttr = costTooltip ? ' title="' + escAttr(costTooltip) + '"' : '';

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
        '<span class="detail-left">' + cliLabel + '</span>' +
        headerOriginBadge +
        (showCost ? '<span class="' + costClass + '" id="header-cost"' + costTitleAttr + '>' + costText + '</span>' : '') +
      '</div>' +
      '</div>' +
    '</div>' +
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
        '<button class="btn-icon" onclick="openFilePicker()" title="上传图片" aria-label="上传图片">&#x1f4ce;</button>' +
        '<button class="btn-icon btn-mic" id="btn-mic" onclick="toggleInputMode()" title="' + (voiceInputMode ? '\u5207\u6362\u952e\u76d8' : '\u5207\u6362\u8bed\u97f3') + '" aria-label="' + (voiceInputMode ? '\u5207\u6362\u5230\u952e\u76d8\u8f93\u5165' : '\u5207\u6362\u5230\u8bed\u97f3\u8f93\u5165') + '">' + (voiceInputMode ? '&#x2328;' : '&#x1f3a4;') + '</button>' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message..." onkeydown="handleKey(event)" oncompositionend="lastCompositionEnd=Date.now()"></div>' +
        '<button class="btn-hold-talk" id="btn-hold-talk" title="\u6309\u4f4f\u8bf4\u8bdd\u6539\u5f55\u97f3" aria-label="\u6309\u4f4f\u8bf4\u8bdd\u5f00\u59cb\u5f55\u97f3">\u6309\u4f4f\u8bf4\u8bdd</button>' +
        '<button class="btn-icon btn-send" id="btn-send" onclick="sendMessage()" title="发送" aria-label="发送消息">&#x27a4;</button>' +
        '<button class="btn-icon btn-stop" id="btn-stop" onclick="interruptSession()" title="stop" aria-label="Stop current turn">&#x25A0;</button>' +
      '</div>' +
      '<div class="input-hints">Enter send &middot; Shift+Enter newline &middot; Esc interrupt</div>' +
      '<input type="file" id="file-input" accept="image/*" multiple style="display:none" onchange="handleFiles(this.files)">' +
    '</div>';

  // Enable drag-drop
  const ia = document.getElementById('input-area');
  ia.addEventListener('dragover', e => { e.preventDefault(); ia.style.borderColor='#58a6ff'; });
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
    const r = await fetch(url, { headers });
    if (!r.ok) return;
    const events = await r.json();
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

  // Remove "load earlier" button so we can place new events first; it'll be
  // re-added after.
  const btn = document.getElementById('earlier-events-btn');
  if (btn) btn.remove();

  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);

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

  // runMermaid / runKatex only iterate their `pending` dictionaries (new IDs
  // emitted by the freshly-rendered bubbles above), so they are already
  // incremental — no DOM scan is needed.
  runMermaid();
  runKatex();
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
    btn.style.cssText = 'display:block;margin:8px auto;padding:6px 14px;background:#21262d;border:1px solid #30363d;color:#c9d1d9;border-radius:6px;cursor:pointer;font-size:12px';
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
  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  el.innerHTML = html || (events.length === 0 ? '<div class="empty-state">暂无事件</div>' : '');
  // Track the latest rendered event time for deduplication
  if (events.length > 0) {
    const last = events[events.length - 1];
    if (last.time) lastRenderedEventTime = last.time;
  }
  // If we got a full initial page there's probably more history available;
  // show the "Load earlier" affordance. Empty state keeps no button.
  if (events.length >= INITIAL_HISTORY_LIMIT) {
    ensureEarlierButton();
  }
  runMermaid();
  runKatex();
  navRebuild();
  // 若有上次切走时保存的滚动位置且不在底部，恢复它；否则照旧贴底。
  if (!restoreScrollPos(selectedKey, selectedNode)) {
    stickEventsBottom();
  }
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
  if (sawUser) stickEventsBottom();
  else if (wasBottom) el.scrollTop = el.scrollHeight;
  runMermaid();
  runKatex();
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
function isInternalEvent(e) { return e && INTERNAL_EVENT_TYPES.has(e.type); }

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

function eventHtml(e) {
  if (isInternalEvent(e) || e.type === 'thinking') return '';
  // Filter out Claude Code system XML injected as user messages
  const raw = e.detail || e.summary || '';
  if (e.type === 'user' && /^<(task-notification|system-reminder|local-command|command-name|available-deferred-tools)[\s>]/.test(raw)) return '';
  const icons = {init:'\u2699',system:'\u2699',user:'\u{1f464}',text:'\u2726',todo:'\u2630'};
  const icon = icons[e.type] || '';

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
  } else {
    content = esc(e.detail || e.summary || e.type);
  }

  // Render image thumbnails for user messages
  let imgHtml = '';
  if (e.images && e.images.length > 0) {
    imgHtml = '<div class="event-images">' + e.images.map(src =>
      '<img src="' + escAttr(src) + '" loading="lazy" onclick="openLightbox(this.src)">'
    ).join('') + '</div>';
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

// Walk a list of events and produce an HTML string with time dividers inserted
// whenever the gap between adjacent VISIBLE (non-null) bubbles exceeds
// EVENT_DIVIDER_GAP_MS. `prevTime` seeds the comparison against whatever is
// already rendered in the DOM (0 = always emit a leading divider for the first
// visible event).
function renderEventsWithDividers(events, prevTime) {
  let out = '';
  let lastTime = prevTime || 0;
  for (const e of events) {
    const h = eventHtml(e);
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
  const totalBytes = pendingFiles.reduce((n, f) => n + (f.normalizedSize || f.file.size || 0), 0);
  const batchCap = 9 * 1024 * 1024;
  if (totalBytes > batchCap) {
    showToast('图片总大小 ' + Math.ceil(totalBytes / 1024 / 1024) + ' MB 超过 9 MB 上限，请分批发送或减少图片', 'warning');
    return;
  }
  const fileIDs = pendingFiles.map(f => f.id).filter(Boolean);

  sending = true;
  const btn = document.getElementById('btn-send');
  if (btn) btn.classList.add('sending');

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
      showAuthModal();
      return;
    }
    if (r.status === 429) {
      if (input) setMsgValue(input, text);
      showToast('消息队列已满，请稍后重试', 'warning');
      return;
    }
    if (!r.ok) {
      if (input) setMsgValue(input, text);
      // Some error paths still write text/plain; fall back to text() so we
      // always surface the real message instead of a generic "send failed".
      const raw = await r.text().catch(() => '');
      let detail = '';
      try { const j = JSON.parse(raw); if (j && j.error) detail = j.error; } catch (_) { if (raw) detail = raw; }
      showAPIError('发送消息', r.status, detail);
      return;
    }

    // Clear input only after confirmed success
    if (input) clearMsg(input);
    delete sessionDrafts[selectedKey];
    clearPendingFiles();

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
  Notebook: '编辑 Notebook', WebFetch: '抓取'
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

function renderAgentRows() {
  var agents = turnState.agents;
  if (agents.length === 0) return '';

  // Separate solo subagents from team members
  var solos = [];
  var teams = {}; // teamName -> [agent, ...]
  for (var i = 0; i < agents.length; i++) {
    var a = agents[i];
    if (a.teamName) {
      if (!teams[a.teamName]) teams[a.teamName] = [];
      teams[a.teamName].push(a);
    } else {
      solos.push(a);
    }
  }

  var html = '';
  // Solo subagents
  for (var j = 0; j < solos.length; j++) {
    html += agentRowHtml(solos[j]);
  }
  // Team groups
  var teamNames = Object.keys(teams);
  for (var k = 0; k < teamNames.length; k++) {
    var tn = teamNames[k];
    var members = teams[tn];
    html += '<div class="rb-team-header"><span class="team-icon">\u25c6</span>' + esc(tn) + '<span class="team-count">' + members.length + ' agents</span></div>';
    for (var m = 0; m < members.length; m++) {
      html += agentRowHtml(members[m]);
    }
  }
  return html;
}

function agentRowHtml(a) {
  var isDone = a.status === 'completed' || a.status === 'error';
  var cls = 'rb-agent-row' + (isDone ? ' done' : '');
  var label = a.name || a.description || 'agent';
  var parts = '<div class="' + cls + '"><span class="sa-dot"></span>';
  if (a.background) parts += '<span class="sa-bg">[bg]</span>';
  parts += '<span class="sa-name">' + esc(label) + '</span>';
  // Detail: lastTool or description
  var detail = '';
  if (a.lastTool) detail = a.lastTool;
  else if (a.description && a.name) detail = a.description;
  if (detail) parts += '<span class="sa-detail">\u00b7 ' + esc(detail) + '</span>';
  // Stats
  var stat = '';
  if (a.toolUses > 0) stat += a.toolUses + ' calls';
  if (a.durationMs > 0) stat += (stat ? ' \u00b7 ' : '') + fmtDuration(a.durationMs);
  if (isDone) stat += (stat ? ' \u00b7 ' : '') + '\u2713';
  if (stat) parts += '<span class="sa-stat">\u00b7 ' + stat + '</span>';
  parts += '</div>';
  return parts;
}

function findAgentByToolUseId(tuid) {
  for (var i = 0; i < turnState.agents.length; i++) {
    if (turnState.agents[i].toolUseId === tuid) return turnState.agents[i];
  }
  return null;
}

function findAgentByTaskId(tid) {
  for (var i = 0; i < turnState.agents.length; i++) {
    if (turnState.agents[i].taskId === tid) return turnState.agents[i];
  }
  return null;
}

function initAgentsFromSession() {
  const sd = sessionsData[sid(selectedKey, selectedNode || 'local')];
  if (sd && sd.subagents && sd.subagents.length > 0) {
    turnState.agents = sd.subagents.map(function(sa) {
      return {
        toolUseId: '', taskId: '', name: sa.name, teamName: '',
        description: sa.activity || '', background: !!sa.background,
        lastTool: '', toolUses: 0, totalTokens: 0, durationMs: 0, status: 'running'
      };
    });
  }
}

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
  }
}

function interruptSession() {
  if (!selectedKey) return;
  const sd = sessionsData[sid(selectedKey, selectedNode || 'local')];
  if (!sd || sd.state !== 'running') return;
  const targetNode = selectedNode && selectedNode !== 'local' ? selectedNode : '';
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
    const active = i === navIdx ? ' style="color:#58a6ff;font-weight:600"' : '';
    return '<div class="nav-list-item" data-idx="' + i + '"' + active + '>' +
      '<span style="color:#484f58;margin-right:6px">' + (i+1) + '.</span>' + esc(summary) + '</div>';
  });
  const pill = document.getElementById('nav-pill');
  const popover = document.createElement('div');
  popover.id = 'nav-list-popover';
  const maxW = Math.min(280, (document.getElementById('main')?.offsetWidth || 280) - 70);
  popover.style.cssText = 'position:absolute;right:44px;bottom:0;width:' + maxW + 'px;max-height:300px;overflow-y:auto;background:rgba(22,27,34,.95);backdrop-filter:blur(8px);border:1px solid #30363d;border-radius:10px;padding:6px 0;z-index:11;font-size:13px;scrollbar-width:thin;scrollbar-color:#30363d transparent';
  popover.innerHTML = items.join('');
  pill.appendChild(popover);
  popover.querySelectorAll('.nav-list-item').forEach(item => {
    item.style.cssText += 'padding:8px 12px;cursor:pointer;color:#c9d1d9;transition:background .1s;border-bottom:1px solid #21262d;overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
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
      navDismissPopover();
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
  if (closed) e.preventDefault();
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

function openFilePicker() { document.getElementById('file-input').click(); }

// Downscale any image to JPEG with max edge 1600 and quality 0.8.
// Rationale: the CLI writes user messages as one NDJSON line to the shim,
// which is capped at 12 MB per line; base64 inflates bytes by ~1.33×, so a
// 20-image batch must stay under ~9 MB raw to fit. 1600 / q0.8 typically
// yields 150–400 KB per JPEG (vs 500 KB–1.2 MB at 2048 / q0.85) while still
// above the 1568 px knee where Anthropic's vision models stop gaining
// accuracy. HEIC is also handled here — createImageBitmap decodes it on
// Safari 17+ and we re-encode to JPEG.
// Falls back to the original file if decoding fails so the server's
// content-type check still produces a real error message.
async function normalizeImage(file) {
  const MAX_EDGE = 1600;
  try {
    const bmp = await createImageBitmap(file);
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

function handleFiles(fileList) {
  const toUpload = [];
  // Relax the source-file ceiling to 40 MB: iPhone HEIC/JPEG straight from
  // Photos is often ~6–12 MB, and browsers deliver HEIC as-is. We downscale
  // before upload, so the 10 MB server ceiling applies to the re-encoded JPEG.
  for (const raw of fileList) {
    if (!raw.type.startsWith('image/')) continue;
    if (raw.size > 40 * 1024 * 1024) { showToast('文件过大（上限 40MB）', 'warning'); continue; }
    if (pendingFiles.length >= 20) { showToast('最多上传 20 个文件', 'warning'); break; }
    const entry = {
      file: raw,
      blobUrl: URL.createObjectURL(raw),
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
    const file = await normalizeImage(entry.file);
    // Track the *normalized* byte size so sendMessage can pre-check the
    // batch against the 12 MB CLI-stdin ceiling. Raw file.size would
    // overstate HEIC/huge photos that we downscale before upload.
    entry.normalizedSize = file.size;
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
    entry.status = 'ready';
  } catch (e) {
    entry.status = 'error';
    entry.error = e.message || 'upload failed';
  }
  renderFilePreviews();
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
    return '<div class="file-thumb ' + entry.status + '"' +
      ' data-idx="' + i + '"' +
      (draggable ? ' draggable="true" tabindex="0" role="button" aria-label="\u56fe\u7247 ' + (i + 1) + '\uff0c\u62d6\u52a8\u6216\u7528\u5de6\u53f3\u65b9\u5411\u952e\u6392\u5e8f"' : '') +
      (draggable ? ' ondragstart="onThumbDragStart(event,' + i + ')"' : '') +
      (draggable ? ' ondragover="onThumbDragOver(event)"' : '') +
      (draggable ? ' ondragleave="onThumbDragLeave(event)"' : '') +
      (draggable ? ' ondrop="onThumbDrop(event,' + i + ')"' : '') +
      (draggable ? ' ondragend="onThumbDragEnd()"' : '') +
      (draggable ? ' onkeydown="onThumbKeyDown(event,' + i + ')"' : '') +
      '>' +
      '<img src="' + entry.blobUrl + '" draggable="false">' +
      overlay +
      '<button class="remove" onclick="removeFile(' + i + ')" title="\u79fb\u9664\u56fe\u7247" aria-label="\u79fb\u9664\u56fe\u7247">\u00d7</button>' +
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
      // R110-P3 brand lockup: `>_` mark + `脑汁 Naozhi` wordmark anchors the
      // login screen so operators recognize they're on the right service.
      // Mirrors the `>_` glyph used in the empty state; pure text (no image
      // asset) keeps the static bundle tiny.
      '<div class="auth-brand">' +
        '<div class="ab-mark" aria-hidden="true">&gt;_</div>' +
        '<div class="ab-wordmark">' +
          '<span class="ab-name">脑汁 Naozhi</span>' +
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
    const r = await fetch('/api/cli/backends', {credentials: 'same-origin'});
    if (!r.ok) return null;
    const data = await r.json();
    cliBackends = data && Array.isArray(data.backends) ? data : null;
    cliBackendsFetchedAt = Date.now();
    return cliBackends;
  } catch (e) {
    return null;
  }
}

// renderBackendPicker returns an HTML fragment for a backend <select>, or
// an empty string when only one backend is enabled. The selected value is
// surfaced via document.getElementById('new-backend').value at submit time.
function renderBackendPicker(backendsData) {
  if (!backendsData || !Array.isArray(backendsData.backends)) return '';
  const list = backendsData.backends;
  if (list.length <= 1) return '';
  const defaultID = backendsData.default || (list[0] && list[0].id) || '';
  const options = list.map(b => {
    const selected = b.id === defaultID ? ' selected' : '';
    const label = (b.display_name || b.id) + (b.version ? ' ' + b.version : '') + (b.available === false ? ' (unavailable)' : '');
    const disabled = b.available === false ? ' disabled' : '';
    return '<option value="' + escAttr(b.id) + '"' + selected + disabled + '>' + esc(label) + '</option>';
  }).join('');
  return '<div style="margin-bottom:12px">' +
    '<label style="font-size:12px;color:#8b949e;display:block;margin-bottom:4px" for="new-backend">CLI backend</label>' +
    '<select id="new-backend" style="width:100%;padding:6px 8px;background:#0d1117;color:#c9d1d9;border:1px solid #30363d;border-radius:4px">' +
    options +
    '</select>' +
    '</div>';
}

function getSelectedBackend() {
  const el = document.getElementById('new-backend');
  return el && el.value ? el.value : '';
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
    '<label style="font-size:12px;color:#8b949e;display:block;margin-bottom:4px" for="new-agent">Agent</label>' +
    '<select id="new-agent" style="width:100%;padding:6px 8px;background:#0d1117;color:#c9d1d9;border:1px solid #30363d;border-radius:4px">' +
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

function createNewSession() {
  // Fetch backends upfront so the picker (if any) is ready when the modal
  // renders. Failure falls back to the single-backend UI — cli.backends
  // returns {} on older naozhi which fetchCLIBackends maps to null.
  fetchCLIBackends().then(backendsData => {
    const ws = defaultWorkspace || '';
    const backendPicker = renderBackendPicker(backendsData);

    if (!projectsData.length) {
      const overlay = document.createElement('div');
      overlay.className = 'modal-overlay';
      overlay.innerHTML =
        '<div class="modal" role="dialog" aria-modal="true" aria-label="新建会话">' +
          '<h3>New Session</h3>' +
          backendPicker +
          renderAgentPicker() +
          '<div style="margin-bottom:12px">' +
            '<label style="font-size:12px;color:#8b949e;display:block;margin-bottom:4px" for="new-workspace">工作目录</label>' +
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
  projectsData.forEach(p => {
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

  const items = scored.map(s => ({type: 'project', data: s}));
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
    if (it.type === 'project') {
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
  el.innerHTML =
    icon +
    '<div class="cp-main">' +
      '<div class="cp-name">' + highlight(p.name, s.nameRanges) + '</div>' +
      '<div class="cp-path">' + highlight(shortPath(p.path), s.pathRanges) + '</div>' +
    '</div>' + nodeBadge;
  el.addEventListener('click', () => pickPaletteProject(p));
  el.addEventListener('mouseenter', () => setActiveIdx(idx));
  return el;
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
    '<div class="cp-main"><div class="cp-name" style="color:#8b949e">' + label + '</div></div>';
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
    if (item.type === 'project') pickPaletteProject(item.data.project);
    else pickPaletteCustom(input.value.trim());
  }
}

function pickPaletteProject(p) {
  const backend = getSelectedBackend();
  const agent = getSelectedAgent();
  doCreateInProject(p.path, p.name, p.node || 'local', backend, agent);
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
  const ws = defaultWorkspace || '';
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
        '<label style="font-size:12px;color:#8b949e;display:block;margin-bottom:4px" for="new-workspace">工作目录路径</label>' +
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

function doCreateInProject(projectPath, projectName, nodeId, backend, agent) {
  // Read the backend/agent from the still-mounted overlay BEFORE removing it,
  // so callers that omit the explicit argument still get the user's pick.
  if (backend === undefined) backend = getSelectedBackend();
  if (agent === undefined) agent = getSelectedAgent();
  const overlay = document.querySelector('.modal-overlay, .cmd-palette-overlay');
  if (overlay) overlay.remove();
  sessionCounter++;
  const now = new Date();
  const ts = now.toISOString().slice(0,10) + '-' +
    now.toTimeString().slice(0,8).replace(/:/g, '') + '-' + sessionCounter;
  // R110-P3 key schema: 4 segments (platform:chatType:chatID:agentID).
  // Merges ts + projectName into the chatID segment so parts[3] is the
  // agentID (buildSessionOpts reads parts[3]). See buildDashboardSessionKey
  // godoc for the contract.
  const key = buildDashboardSessionKey(ts, projectName, agent);

  sessionWorkspaces[key] = projectPath;
  if (nodeId && nodeId !== 'local') sessionNodes[key] = nodeId;
  if (backend) sessionBackends[key] = backend;

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
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
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

  if (workspace) sessionWorkspaces[key] = workspace;
  if (backend) sessionBackends[key] = backend;

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = 'local';
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  if (typeof updateNodeSelector === 'function') updateNodeSelector();
  lastEventTime = 0;
  mobileEnterChat();
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
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

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = 'local';
  try { localStorage.setItem('nz_selectedNode', selectedNode); } catch(_) {}
  if (typeof updateNodeSelector === 'function') updateNodeSelector();
  lastEventTime = 0;
  mobileEnterChat();
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
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
    '<form class="quick-ask-form" onsubmit="event.preventDefault();submitQuickAsk(event)">' +
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
//
// Input shape tolerant: missing last_active / total_cost on a session
// contributes zero / is skipped rather than NaN-poisoning the totals.
function computeHomeStats(items, nowMs) {
  const arr = Array.isArray(items) ? items : [];
  const now = typeof nowMs === 'number' ? nowMs : Date.now();
  const d = new Date(now);
  const dayStart = new Date(d.getFullYear(), d.getMonth(), d.getDate(), 0, 0, 0, 0).getTime();
  let todayActive = 0;
  let totalCost = 0;
  for (const s of arr) {
    if (!s) continue;
    if (typeof s.last_active === 'number' && s.last_active >= dayStart) todayActive++;
    if (typeof s.total_cost === 'number' && isFinite(s.total_cost)) totalCost += s.total_cost;
  }
  return { todayActive: todayActive, totalCost: totalCost };
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
  // Line 2: CLI identity. Helpful when operators have multiple naozhi
  // deployments on different CLI versions.
  if (stats.cli_name) {
    let cli = stats.cli_name;
    if (stats.cli_version) cli += ' ' + stats.cli_version;
    lines.push({ text: cli, kind: 'info' });
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
  // R110-P1 Home stats strip (Round 147): today-active + total cost. Rendered
  // above the list so operators see a cumulative signal before scanning the
  // session rows. Prompts and tokens need backend aggregation — omitted here.
  const stats = computeHomeStats(items, Date.now());
  const statsHtml =
    '<div class="recent-panel-stats" role="group" aria-label="今日概览">' +
      '<div class="recent-stat">' +
        '<div class="recent-stat-value">' + stats.todayActive + '</div>' +
        '<div class="recent-stat-label">今日活跃会话</div>' +
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
  host.innerHTML =
    '<div class="recent-panel">' +
      '<div class="recent-panel-title">最近会话</div>' +
      statsHtml +
      '<div class="recent-panel-list" role="list">' + rows + '</div>' +
      healthHtml +
    '</div>';
}

function showToast(msg, type, duration) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast show' + (type ? ' ' + type : '');
  clearTimeout(el._tid);
  el._tid = setTimeout(() => { el.className = 'toast'; }, duration || 3000);
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
  if (status === 401 || status === 403) {
    return '鉴权失败，请重新登录' + withTail;
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
  const codeEl = wrap && wrap.querySelector('code');
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

/* Focus trap: confine Tab within an overlay, restore focus on dismissal.
   Called after an overlay is appended to the DOM. Returns nothing — the
   overlay's MutationObserver tears down listeners when it's removed. */
function trapFocus(overlay) {
  if (!overlay || overlay._trapped) return;
  overlay._trapped = true;
  const prevActive = document.activeElement;
  const FOCUSABLE = 'button, [href], input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"]), [contenteditable="true"]';
  const onKey = (e) => {
    if (e.key === 'Escape') {
      // Let inner handlers pre-empt; otherwise dismiss the overlay.
      if (!e.defaultPrevented) { overlay.remove(); }
      return;
    }
    if (e.key !== 'Tab') return;
    const nodes = [...overlay.querySelectorAll(FOCUSABLE)].filter(el => !el.disabled && el.offsetParent !== null);
    if (nodes.length === 0) { e.preventDefault(); return; }
    const first = nodes[0], last = nodes[nodes.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  overlay.addEventListener('keydown', onKey);
  const obs = new MutationObserver(() => {
    if (!document.body.contains(overlay)) {
      overlay.removeEventListener('keydown', onKey);
      obs.disconnect();
      if (prevActive && prevActive.focus) { try { prevActive.focus(); } catch(_) {} }
    }
  });
  obs.observe(document.body, { childList: true, subtree: false });
}

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

    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay confirm-overlay';
    overlay.innerHTML =
      '<div class="modal confirm-dialog" role="alertdialog" aria-modal="true" aria-labelledby="confirm-title">' +
        '<h3 id="confirm-title">' + esc(title) + '</h3>' +
        (message ? '<p>' + esc(message) + '</p>' : '') +
        (detail ? '<p class="confirm-detail"><code>' + esc(detail) + '</code></p>' : '') +
        '<div class="modal-btns">' +
          '<button type="button" class="confirm-cancel">' + esc(cancelText) + '</button>' +
          '<button type="button" class="' + confirmClass + ' confirm-ok">' + esc(confirmText) + '</button>' +
        '</div>' +
      '</div>';

    let settled = false;
    const finish = (ok) => {
      if (settled) return;
      settled = true;
      overlay.remove();
      resolve(!!ok);
    };

    overlay.querySelector('.confirm-cancel').addEventListener('click', () => finish(false));
    overlay.querySelector('.confirm-ok').addEventListener('click', () => finish(true));
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

const _escEl = document.createElement('div');
function esc(s) {
  if (!s) return '';
  _escEl.textContent = s;
  return _escEl.innerHTML;
}
// Escape for HTML attribute context. We don't know whether the caller used
// single- or double-quoted attributes, so we escape both to be safe.
function escAttr(s) {
  return esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function escJs(s) {
  if (!s) return '';
  return String(s).replace(/\\/g,'\\\\').replace(/'/g,"\\'").replace(/"/g,'\\"').replace(/\n/g,'\\n').replace(/\r/g,'\\r').replace(/</g,'\\u003c').replace(/>/g,'\\u003e');
}
// URL schemes that are safe to embed in <a href>. Anything else (including
// javascript:, data:, vbscript:, file:, about:) gets rewritten to '#'.
function safeUrl(u) {
  if (!u) return '#';
  const trimmed = String(u).trim();
  if (/^(https?:|mailto:|\/|#|\?)/i.test(trimmed)) return trimmed;
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

/* Lightweight Markdown renderer for text/result events.
   Plain messages (no fenced code, math, or mermaid) are memoized since event
   renders run repeatedly — every WS push triggers a full-list re-render for
   the initial history, plus nav rebuilds, plus preview polls. */
const _mdCache = new Map();
const _MD_CACHE_MAX = 500;

function renderMd(s) {
  if (!s) return '';
  // Only cache when the input has no constructs that mint unique DOM ids
  // (mermaid-N / ktx-N), otherwise cached HTML would collide across messages.
  const cacheable = s.length < 20000 && !/```|\$|\\\[|\\\(/.test(s);
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

// Path candidate regex: must contain at least one `/` (filters out bare
// filenames like "README") and accept any non-whitespace, non-colon char in
// segments so Unicode filenames (Chinese, Japanese, …) are not silently
// dropped. Optional leading `./`, `../`, or `/` (absolute paths are resolved
// to project-relative form by resolveProjectForAbsPath before the server
// call — server still rejects absolute paths for defence in depth).
// Optional :line (e.g. src/foo.go:42) or :line-line (foo.go:10-20) suffix
// works because `:` is excluded from segment chars, so it anchors the suffix.
// Rejects spaces (breaks on prose) and leading URL schemes.
const FILE_REF_RE = /^(?:\.\.?\/|\/)?(?!https?:)[^\s:]+(?:\/[^\s:]+)+(?::\d+(?:-\d+)?)?$/;

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
  const codeEls = eventEl.querySelectorAll('.event-content code, .event-content .md-code');
  codeEls.forEach(code => {
    if (code.dataset.frScanned === '1') return;
    code.dataset.frScanned = '1';
    const text = (code.textContent || '').trim();
    if (!text || text.length > 512) return; // absurdly long paths skip
    if (!FILE_REF_RE.test(text)) return;
    // Skip when nested inside <a> (authored link target).
    if (code.closest('a')) return;
    // Skip fenced code blocks (<pre><code>): those are content, not refs.
    if (code.closest('pre')) return;
    const { path, line } = splitPathLine(text);

    // Decide which project owns this path. Absolute paths get looked up
    // against projectsData on the active node; relative paths ride on the
    // session's active project (the historical behaviour).
    let projName, projNode, serverPath;
    if (path.startsWith('/')) {
      const hit = resolveProjectForAbsPath(path, activeNode);
      if (!hit) return; // abs path outside any known project on this node
      projName = hit.name;
      projNode = hit.node;
      serverPath = hit.relPath;
    } else {
      if (!activeProj) return; // relative path needs an active project
      projName = activeProj.name;
      projNode = activeProj.node;
      serverPath = path.replace(/^\.\//, ''); // strip leading ./ for server
    }

    const wrap = document.createElement('span');
    wrap.className = 'file-ref fr-candidate';
    wrap.dataset.path = serverPath;       // what we send to the server
    wrap.dataset.displayPath = path;      // what the user typed / saw
    wrap.dataset.line = line;
    wrap.dataset.project = projName;
    wrap.dataset.node = projNode;
    code.parentNode.insertBefore(wrap, code);
    wrap.appendChild(code);
    // Queue for existence check; button DOM is injected once we know the
    // file exists.
    queueFileRefCheck(wrap);
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
    const r = await fetch('/api/projects/files/exists', {
      method: 'POST', headers,
      body: JSON.stringify({ project: batch.project, node: batch.node, paths })
    });
    if (!r.ok) return;
    const data = await r.json();
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
      body.innerHTML = '<div class="fv-binary">Binary file — click <strong>download</strong> to save.<span class="fv-mime">' + esc(data.mime || '') + '</span></div>';
      return;
    }
    const parts = [];
    if (data.truncated) {
      parts.push('<div class="fv-truncated">file truncated at ' + formatFileSize(1024 * 1024) + ' (total ' + formatFileSize(data.size || 0) + ') — download for full content</div>');
    }
    const lang = inferLang(path, data.mime || '');
    // Markdown: render via existing renderer; others: raw <pre><code> with line gutter.
    if (lang === 'markdown') {
      parts.push('<div class="fv-md">' + renderMd(data.content || '') + '</div>');
    } else {
      const raw = data.content || '';
      const lines = raw.split('\n');
      const gutter = lines.map((_, i) => String(i + 1)).join('\n');
      parts.push('<pre class="fv-lined"><span class="fv-gutter" aria-hidden="true">' + gutter + '</span><code class="fv-code">' + esc(raw) + '</code></pre>');
    }
    body.innerHTML = parts.join('');
    if (line) scrollToPreviewLine(body, parseInt(line, 10));
  } catch (e) {
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
  if (mime === 'text/markdown') return 'markdown';
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

function renderMdUncached(s) {
  // Split by fenced code blocks and display math blocks
  const parts = s.split(/(```[\s\S]*?```|\$\$[\s\S]*?\$\$|\\\[[\s\S]*?\\\])/g);
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
    // Process line by line for block elements. Accumulate into a chunks array
    // + single join() at the end rather than `html +=` per line: V8 reallocates
    // the underlying string on every concat past the small-string threshold,
    // which is O(n^2) over line count. A 200-line response rendered ~50 times
    // per history replay was the dominant cost in the text-event path.
    const lines = part.split('\n');
    const chunks = [];
    let inList = '';
    for (let i = 0; i < lines.length; i++) {
      let line = lines[i];
      // Headings
      const hm = line.match(/^(#{1,4})\s+(.+)$/);
      if (hm) {
        if (inList) { chunks.push('</' + inList + '>'); inList = ''; }
        const level = hm[1].length;
        chunks.push('<strong class="md-h' + level + '">' + inlineMd(hm[2]) + '</strong>\n');
        continue;
      }
      // Unordered list
      if (/^\s*[-*]\s+/.test(line)) {
        if (inList === 'ol') { chunks.push('</ol>'); inList = ''; }
        if (!inList) { chunks.push('<ul class="md-ul">'); inList = 'ul'; }
        chunks.push('<li>' + inlineMd(line.replace(/^\s*[-*]\s+/, '')) + '</li>');
        continue;
      }
      // Ordered list
      if (/^\s*\d+\.\s+/.test(line)) {
        if (inList === 'ul') { chunks.push('</ul>'); inList = ''; }
        if (!inList) { chunks.push('<ol class="md-ol">'); inList = 'ol'; }
        chunks.push('<li>' + inlineMd(line.replace(/^\s*\d+\.\s+/, '')) + '</li>');
        continue;
      }
      if (line === '') {
        if (inList) {
          // Look ahead: if next non-blank line continues the list, keep it open
          let peek = i + 1;
          while (peek < lines.length && lines[peek] === '') peek++;
          if (peek < lines.length) {
            let nl = lines[peek];
            if ((inList === 'ol' && /^\s*\d+\.\s+/.test(nl)) ||
                (inList === 'ul' && /^\s*[-*]\s+/.test(nl))) {
              continue;
            }
          }
          chunks.push('</' + inList + '>'); inList = '';
        }
        chunks.push('<div class="md-blank"></div>');
        continue;
      }
      if (inList) { chunks.push('</' + inList + '>'); inList = ''; }
      if (/^\|.+\|$/.test(line.trim())) {
        let tbl = [line];
        while (i + 1 < lines.length && /^\|.+\|$/.test(lines[i + 1].trim())) { tbl.push(lines[++i]); }
        chunks.push(renderTable(tbl));
        continue;
      }
      chunks.push(inlineMd(line) + '<br>');
    }
    if (inList) chunks.push('</' + inList + '>');
    return chunks.join('');
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
    // `$...$`: require non-alphanumeric outside + LaTeX-ish char inside,
    // else prose like "每月$650$USD" gets rendered as italic math.
    s = s.replace(/(?<![A-Za-z0-9])\$([^\s\$][^\$\n]*?[^\s\$]|[^\s\$])\$(?![A-Za-z0-9])/g, function(match, tex) {
      if (!/[\\^_{}]/.test(tex)) return match;
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
  // Use function-form replacements to prevent JS's special $-sequences
  // ($&, $', $`, $n) from expanding inside the replacement string. Those
  // sequences survive esc() (they aren't HTML entities) and would let an
  // attacker-controlled LLM snippet splice unescaped characters into the
  // emitted HTML by embedding `$&` inside a backtick/bold region.
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
    if (safe === '#') return text;
    return '<a href="' + escAttr(safe) + '" class="md-link" target="_blank" rel="noopener noreferrer">' + text + '</a>';
  });
  // Auto-link bare URLs not already inside an <a> tag
  s = s.replace(/(^|[^"'>])(https?:\/\/[^\s<)}\]]+)/g, function(_, prefix, url) {
    var clean = url.replace(/[.,;:!?)]+$/, '');
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
      return '<code class="md-code">' + codeTokens[+idx] + '</code>';
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
  const cells = l => l.trim().replace(/\\\|/g, PIPE)
    .replace(/^\||\|$/g, '')
    .split('|')
    .map(c => c.trim().split(PIPE).join('|'));
  let h = '<table class="md-table"><thead><tr>' + cells(lines[0]).map(c => '<th>' + inlineMd(c) + '</th>').join('') + '</tr></thead><tbody>';
  for (let i = 2; i < lines.length; i++) h += '<tr>' + cells(lines[i]).map(c => '<td>' + inlineMd(c) + '</td>').join('') + '</tr>';
  return '<div class="md-table-wrap">' + h + '</tbody></table></div>';
}

function processEventsForDisplay(events) {
  return events.filter(e => !isInternalEvent(e));
}

function sid(key, node) { return key + '\t' + (node || 'local'); }

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
    const delay = Math.max(this.backoff, authGap);
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
        // Subscribe failed (e.g. session not found yet) — reset pending
        this._pendingSubscribeKey = null;
        this._pendingSubscribeNode = null;
        break;
      case 'history':
        this.onHistory(msg);
        break;
      case 'event':
        this.onEvent(msg);
        break;
      case 'send_ack':
        this.onSendAck(msg);
        break;
      case 'interrupt_ack':
        break;
      case 'session_state':
        this.onSessionState(msg);
        break;
      case 'sessions_update':
        debouncedFetchSessions().then(() => {
          // Auto-subscribe to newly created session if we don't have an active
          // subscription. _pendingSubscribeKey is intentionally not checked:
          // a no-process subscribe returns "subscribed" + persisted history but
          // no live eventPushLoop, so subscribedKey may not be set while the
          // pending flag was already cleared. This ensures recovery.
          if (selectedKey && !wsm.subscribedKey && sessionsData[sid(selectedKey, selectedNode)]) {
            wsm.subscribe(selectedKey, selectedNode);
          }
        });
        break;
      case 'cron_result':
        fetchCronJobs().then(() => renderCronPanel());
        break;
      case 'pong':
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

  /* -- WS event handlers -- */

  onConnected() {
    if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
    if (selectedKey) {
      if (lastEventTime > 0 && this.lastEventTimeWs === 0) {
        this.lastEventTimeWs = lastEventTime;
      }
      this.subscribe(selectedKey, selectedNode);
    }
  },

  onHistory(msg) {
    if (msg.key !== selectedKey || (msg.node || 'local') !== selectedNode) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const events = msg.events || [];
    const isInitial = this._initialSubscribe;
    this._initialSubscribe = false;

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
        el.innerHTML = '';
      }
      // Reset dedup tracker on full render
      if (events.length > 0) { const last = events[events.length - 1]; if (last.time) lastRenderedEventTime = last.time; }
      // Mount "load earlier" affordance when the server returned a full page
      // (more history likely exists). Skipped for short histories so we don't
      // surface a useless button.
      if (events.length >= INITIAL_HISTORY_LIMIT) {
        ensureEarlierButton();
      }
      runMermaid();
      runKatex();
      navRebuild();
      // 若有上次切走时保存的滚动位置且不在底部，恢复它；否则照旧贴底。
      if (!restoreScrollPos(selectedKey, selectedNode)) {
        stickEventsBottom();
      }
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
      if (sawUser) stickEventsBottom();
      else if (wasBottom) el.scrollTop = el.scrollHeight;
      runMermaid();
  runKatex();
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
    const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
    const prevT = lastDividerTime(el);
    const evT = ev.time || 0;
    if (evT && (prevT === 0 || evT - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(evT));
    }
    el.insertAdjacentHTML('beforeend', html);
    // User events always force-bottom; AI output only sticks when already at bottom.
    if (isUser) stickEventsBottom();
    else if (wasBottom) el.scrollTop = el.scrollHeight;
    runMermaid();
  runKatex();
    if (ev.type === 'user') {
      navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
      navUpdatePill();
    }
  },

  onSendAck(msg) {
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
    } else if (msg.status === 'error') {
      // The WS send_ack error is an in-band message, not an HTTP status,
      // but treat the server-supplied `error` string the same way as an
      // HTTP 500 body: truncate + prefix with "发送消息失败：".
      showAPIError('发送消息', 500, msg.error || '');
      // Remove optimistic message on send failure
      const opt = document.querySelector('.optimistic-msg');
      if (opt) opt.remove();
    }
  },

  onSessionState(msg) {
    const msgNode = msg.node || 'local';
    const sKey = sid(msg.key, msgNode);
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
    _updateStatusTick(s);
    if (s === WS_STATES.CONNECTED) {
      // No reconnect toast: the sidebar status row already conveys the
      // transition (amber "connecting..." dot → green "connected" dot,
      // and .status-outage drops off) which is the user-visible signal.
      // The previous top-of-screen toast was redundant and on mobile
      // covered the header. _everConnected stays on the wsm struct because
      // future consumers may still want to differentiate first-handshake
      // from reconnect (e.g. fresh session poll vs. no-op).
      this._everConnected = true;
      // WS connected: stop session polling, rely on push
      if (sessionPollTimer) { clearInterval(sessionPollTimer); sessionPollTimer = null; }
      // Reduce discovered scan frequency
      if (discoveredPollTimer) { clearInterval(discoveredPollTimer); discoveredPollTimer = null; }
      discoveredPollTimer = setInterval(scanDiscovered, 30000);
      // Pull fresh node/session state immediately to clear stale data
      debouncedFetchSessions();
    } else if (s === WS_STATES.DISCONNECTED) {
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
  el.textContent = '$' + (cost < 0.01 && cost > 0 ? cost.toFixed(4) : cost.toFixed(2));
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
  if (cost > 0) lines.push('累计花费: $' + cost.toFixed(4));
  // firstSeen is stored per-(node,key) in localStorage by renderSidebar.
  // Read it here so the tooltip shows when THIS dashboard first saw the
  // session — matches operator mental model for "how long has this been
  // running" better than any server-side timestamp we currently surface.
  const seenKey = (selNode || 'local') + ':' + (selKey || '');
  const first = typeof sessionFirstSeen === 'object' && sessionFirstSeen
    ? sessionFirstSeen[seenKey]
    : 0;
  if (first > 0) {
    lines.push('首次打开: ' + formatAbsTime(first));
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
  const name = s.cli_name || defaultCLIName;
  const version = s.cli_version || defaultCLIVersion;
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
    const r = await fetch('/api/discovered', { headers });
    discoveredItems = (await r.json()) || [];
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
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
  mobileEnterChat();

  // Highlight the discovered card
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
  const card = document.querySelector('.session-card[data-key="_discovered:' + pid + '"]');
  if (card) card.classList.add('active');

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
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/discovered/preview?session_id=' + encodeURIComponent(sessionId) + nodeParam, { headers });
    if (!r.ok) {
      const errText = await r.text().catch(() => '');
      const el0 = document.getElementById('events-scroll');
      if (el0) el0.innerHTML = '<div class="empty-state">' + esc(errText || '预览失败') + '</div>';
      showAPIError('预览会话', r.status, errText);
      return;
    }
    const events = await r.json();
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
    previewTimer = setInterval(async () => {
      try {
        const headers2 = {};
        const t2 = getToken();
        if (t2) headers2['Authorization'] = 'Bearer ' + t2;
        const r2 = await fetch('/api/discovered/preview?session_id=' + encodeURIComponent(capturedSid) + nodeParam, { headers: headers2 });
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
      } catch (_) {}
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
  let card = null, startX = 0, startY = 0, tracking = false;
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
    }
    if (dx >= 0) return;
    card.classList.add('swiping');
    card.style.transform = 'translateX(' + dx + 'px)';
    card.style.background = 'rgba(218,54,51,' + Math.min(0.35, -dx / card.offsetWidth * 0.6) + ')';
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
// Timezone the backend uses to evaluate cron schedules. Surfaced in the
// modal and job list so users aren't guessing whether "0 9 * * *" means
// 9am local or 9am UTC.
let cronTimezoneLabel = '';
// Configured default IM target for cron completion notifications, or null
// when the server has no default configured. Used to render helpful copy
// alongside the notify toggle in create/edit modals.
let cronNotifyDefault = null;
// R110-P2 cron filter state — module-level so renderCronList can read the
// live values each paint without a closure. Mirrors the sidebar-search
// approach (cronFilterQuery is the substring, cronFilterStatus is one of
// 'all' | 'active' | 'attention'). 'attention' matches paused-or-last_error,
// aligning with the header cron-badge's attention definition so the filter
// "what needs my eyeballs" dovetails with the top-level signal.
let cronFilterQuery = '';
let cronFilterStatus = 'all';

// Pads an integer to two digits (e.g. 7 -> "07"). Used for HH/MM rendering.
function pad2(n) { return (n < 10 ? '0' : '') + n; }

// parseCronToFreq inspects a schedule expression and, when it matches one of
// our canonical frequency shapes, returns a descriptor the frequency picker
// can restore. Returning null means "we don't recognize this — fall back to
// the raw expression editor." This is intentionally narrow: we only recognize
// the exact shapes buildFreqSchedule emits, so round-tripping is lossless.
function parseCronToFreq(expr) {
  if (!expr) return null;
  const s = expr.trim();
  let m = s.match(/^@every\s+(\d+)(m|h)$/i);
  if (m) {
    const n = parseInt(m[1], 10);
    const unit = m[2].toLowerCase();
    if (unit === 'm' && n < 5) return null;
    return { mode: 'interval', n, unit };
  }
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
    const days = parseDowField(dow);
    if (days) return { mode: 'weekly', dows: days, time: hhmm };
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
// is invalid (e.g. interval <5min, no weekday selected).
function buildFreqSchedule(desc) {
  if (!desc) return { err: '请选择频率' };
  if (desc.mode === 'interval') {
    const n = parseInt(desc.n, 10);
    if (!Number.isFinite(n) || n < 1) return { err: '间隔必须是正整数' };
    if (desc.unit === 'm' && n < 5) return { err: '最短间隔为 5 分钟' };
    if (desc.unit !== 'm' && desc.unit !== 'h') return { err: '单位无效' };
    return { expr: '@every ' + n + desc.unit };
  }
  if (desc.mode === 'daily') {
    const t = parseHHMM(desc.time);
    if (!t) return { err: '时间格式无效' };
    return { expr: t.m + ' ' + t.h + ' * * *' };
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
    // parseCronToFreq only recognizes shapes the frequency-picker round-trips
    // (editor contract). Some common hand-written shapes — notably the
    // `*/N` step-value form — don't fit that shape but ARE worth
    // humanizing for the display-only label. Try the label-only hook
    // before giving up and echoing the raw expression.
    const step = humanizeCronStepValue(expr);
    if (step) return step;
    return expr;
  }
  if (d.mode === 'interval') {
    return d.unit === 'h' ? ('每 ' + d.n + ' 小时') : ('每 ' + d.n + ' 分钟');
  }
  if (d.mode === 'daily') return '每天 ' + d.time;
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

// buildFreqPickerHtml renders the frequency tab UI. initial is an optional
// descriptor to pre-fill (used by edit modal); when absent we default to
// "every 1 hour" which matches the most common ask.
function buildFreqPickerHtml(initial) {
  const d = initial || { mode: 'interval', n: 1, unit: 'h' };
  const tab = (mode, label) =>
    '<button type="button" class="freq-tab' + (d.mode === mode ? ' active' : '') + '" data-mode="' + mode + '" onclick="freqSelectTab(this, \'' + mode + '\')">' + esc(label) + '</button>';

  const iv = d.mode === 'interval' ? d : { n: 1, unit: 'h' };
  const intervalHtml =
    '<div class="freq-body" data-mode="interval" style="' + (d.mode === 'interval' ? '' : 'display:none') + '">' +
      '<div class="freq-row">' +
        '<span class="freq-label">每隔</span>' +
        '<input class="freq-num" id="freq-iv-n" type="number" min="1" max="59" value="' + esc(String(iv.n)) + '" oninput="freqUpdate()">' +
        '<select class="freq-select" id="freq-iv-unit" onchange="freqUpdate()">' +
          '<option value="m"' + (iv.unit === 'm' ? ' selected' : '') + '>分钟</option>' +
          '<option value="h"' + (iv.unit === 'h' ? ' selected' : '') + '>小时</option>' +
        '</select>' +
      '</div>' +
      '<div class="freq-preset-row">' +
        '<button type="button" class="freq-preset" onclick="freqIntervalPreset(5,\'m\')">每 5 分钟</button>' +
        '<button type="button" class="freq-preset" onclick="freqIntervalPreset(15,\'m\')">每 15 分钟</button>' +
        '<button type="button" class="freq-preset" onclick="freqIntervalPreset(30,\'m\')">每 30 分钟</button>' +
        '<button type="button" class="freq-preset" onclick="freqIntervalPreset(1,\'h\')">每小时</button>' +
        '<button type="button" class="freq-preset" onclick="freqIntervalPreset(6,\'h\')">每 6 小时</button>' +
      '</div>' +
    '</div>';

  const da = d.mode === 'daily' ? d : { time: '09:00' };
  const dailyHtml =
    '<div class="freq-body" data-mode="daily" style="' + (d.mode === 'daily' ? '' : 'display:none') + '">' +
      '<div class="freq-row">' +
        '<span class="freq-label">每天</span>' +
        '<input class="freq-time" id="freq-daily-time" type="time" value="' + esc(da.time) + '" onchange="freqUpdate()" oninput="freqUpdate()">' +
      '</div>' +
      '<div class="freq-preset-row">' +
        '<button type="button" class="freq-preset" onclick="freqDailyPreset(\'09:00\')">早上 9 点</button>' +
        '<button type="button" class="freq-preset" onclick="freqDailyPreset(\'12:00\')">中午 12 点</button>' +
        '<button type="button" class="freq-preset" onclick="freqDailyPreset(\'18:00\')">傍晚 6 点</button>' +
        '<button type="button" class="freq-preset" onclick="freqDailyPreset(\'22:00\')">晚上 10 点</button>' +
      '</div>' +
    '</div>';

  const wk = d.mode === 'weekly' ? d : { dows: [1, 2, 3, 4, 5], time: '09:00' };
  const selectedDows = new Set(wk.dows || []);
  const dowBtns = DOW_LABELS.map(x =>
    '<button type="button" class="freq-dow' + (selectedDows.has(x.i) ? ' on' : '') +
    '" data-dow="' + x.i + '" onclick="freqToggleDow(this)">' + esc(x.label) + '</button>'
  ).join('');
  const weeklyHtml =
    '<div class="freq-body" data-mode="weekly" style="' + (d.mode === 'weekly' ? '' : 'display:none') + '">' +
      '<div class="freq-row"><span class="freq-label">星期</span><div class="freq-dows" id="freq-weekly-dows">' + dowBtns + '</div></div>' +
      '<div class="freq-row">' +
        '<span class="freq-label">时间</span>' +
        '<input class="freq-time" id="freq-weekly-time" type="time" value="' + esc(wk.time) + '" onchange="freqUpdate()" oninput="freqUpdate()">' +
      '</div>' +
      '<div class="freq-preset-row">' +
        '<button type="button" class="freq-preset" onclick="freqWeeklyPreset([1,2,3,4,5],\'09:00\')">工作日 9 点</button>' +
        '<button type="button" class="freq-preset" onclick="freqWeeklyPreset([0,6],\'10:00\')">周末 10 点</button>' +
        '<button type="button" class="freq-preset" onclick="freqWeeklyPreset([1],\'09:00\')">每周一 9 点</button>' +
      '</div>' +
    '</div>';

  const mo = d.mode === 'monthly' ? d : { day: 1, time: '09:00' };
  let dayOpts = '';
  for (let i = 1; i <= 31; i++) {
    dayOpts += '<option value="' + i + '"' + (mo.day === i ? ' selected' : '') + '>' + i + '</option>';
  }
  const monthlyHtml =
    '<div class="freq-body" data-mode="monthly" style="' + (d.mode === 'monthly' ? '' : 'display:none') + '">' +
      '<div class="freq-row">' +
        '<span class="freq-label">每月</span>' +
        '<select class="freq-select" id="freq-monthly-day" onchange="freqUpdate()">' + dayOpts + '</select>' +
        '<span class="freq-label">日</span>' +
        '<input class="freq-time" id="freq-monthly-time" type="time" value="' + esc(mo.time) + '" onchange="freqUpdate()" oninput="freqUpdate()">' +
      '</div>' +
      '<div class="cron-tz-hint" style="margin-top:6px">如果选择 29、30、31 日，当月没有这一天时会跳过。</div>' +
    '</div>';

  return '<div class="freq-tabs" role="tablist">' +
      tab('interval', '间隔') + tab('daily', '每天') + tab('weekly', '每周') + tab('monthly', '每月') +
    '</div>' +
    intervalHtml + dailyHtml + weeklyHtml + monthlyHtml;
}

// freqCurrentDescriptor reads the picker state back into a descriptor object.
// Returns null when the picker is absent.
function freqCurrentDescriptor() {
  const active = document.querySelector('.freq-tabs .freq-tab.active');
  if (!active) return null;
  const mode = active.getAttribute('data-mode');
  if (mode === 'interval') {
    const n = parseInt(document.getElementById('freq-iv-n').value, 10);
    const unit = document.getElementById('freq-iv-unit').value;
    return { mode, n, unit };
  }
  if (mode === 'daily') {
    return { mode, time: document.getElementById('freq-daily-time').value };
  }
  if (mode === 'weekly') {
    const btns = document.querySelectorAll('#freq-weekly-dows .freq-dow.on');
    const dows = [...btns].map(b => parseInt(b.getAttribute('data-dow'), 10)).sort((a, b) => a - b);
    return { mode, dows, time: document.getElementById('freq-weekly-time').value };
  }
  if (mode === 'monthly') {
    return {
      mode,
      day: parseInt(document.getElementById('freq-monthly-day').value, 10),
      time: document.getElementById('freq-monthly-time').value,
    };
  }
  return null;
}

function freqSelectTab(btn, mode) {
  document.querySelectorAll('.freq-tabs .freq-tab').forEach(b => b.classList.remove('active'));
  btn.classList.add('active');
  document.querySelectorAll('.freq-body').forEach(el => {
    el.style.display = el.getAttribute('data-mode') === mode ? '' : 'none';
  });
  freqUpdate();
}
function freqToggleDow(btn) { btn.classList.toggle('on'); freqUpdate(); }
function freqIntervalPreset(n, unit) {
  document.getElementById('freq-iv-n').value = n;
  document.getElementById('freq-iv-unit').value = unit;
  freqUpdate();
}
function freqDailyPreset(t) {
  document.getElementById('freq-daily-time').value = t;
  freqUpdate();
}
function freqWeeklyPreset(dows, t) {
  const set = new Set(dows);
  document.querySelectorAll('#freq-weekly-dows .freq-dow').forEach(b => {
    const i = parseInt(b.getAttribute('data-dow'), 10);
    b.classList.toggle('on', set.has(i));
  });
  document.getElementById('freq-weekly-time').value = t;
  freqUpdate();
}

// freqUpdate refreshes overlay._cronSchedule and the multi-run preview.
// Advanced raw-cron input takes priority when non-empty; otherwise the
// picker's descriptor feeds buildFreqSchedule.
function freqUpdate() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  const advanced = document.getElementById('freq-advanced-input');
  if (advanced && advanced.value.trim()) {
    overlay._cronSchedule = advanced.value.trim();
    previewFreqSchedule(overlay._cronSchedule);
    return;
  }
  const desc = freqCurrentDescriptor();
  const { expr, err } = buildFreqSchedule(desc);
  if (err) {
    overlay._cronSchedule = '';
    renderFreqPreview({ valid: false, error: err });
    return;
  }
  overlay._cronSchedule = expr;
  previewFreqSchedule(expr);
}

let _freqPreviewTimer = null;
function previewFreqSchedule(expr) {
  clearTimeout(_freqPreviewTimer);
  _freqPreviewTimer = setTimeout(() => doPreviewFreq(expr), 200);
}
async function doPreviewFreq(expr) {
  if (!expr) { renderFreqPreview({ valid: false, error: '' }); return; }
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/preview?count=5&schedule=' + encodeURIComponent(expr), { headers });
    const data = await r.json();
    renderFreqPreview(data);
  } catch (e) {
    renderFreqPreview({ valid: false, error: 'preview error' });
  }
}

function renderFreqPreview(data) {
  const box = document.getElementById('freq-preview');
  if (!box) return;
  if (!data || !data.valid) {
    box.className = 'freq-preview err';
    box.innerHTML = '<div class="freq-preview-title">schedule 无效</div>' +
      '<div style="color:var(--nz-red)">' + esc(data && data.error || '请完成频率设置') + '</div>';
    return;
  }
  const runs = data.next_runs || (data.next_run ? [data.next_run] : []);
  box.className = 'freq-preview';
  let list = '';
  for (let i = 0; i < runs.length; i++) {
    const ts = runs[i];
    const d = new Date(ts);
    const pretty = d.toLocaleString(undefined, {
      month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false,
    });
    const rel = i === 0 ? timeAgo(ts, true) : '';
    list += '<li><span>' + esc(pretty) + '</span>' + (rel ? '<span class="fp-rel">' + esc(rel) + '</span>' : '') + '</li>';
  }
  const tz = data.timezone_label || data.timezone || cronTimezoneLabel;
  box.innerHTML =
    '<div class="freq-preview-title">接下来会在这些时间运行</div>' +
    '<ul class="freq-preview-list">' + list + '</ul>' +
    (tz ? '<div class="freq-preview-tz">时区：' + esc(tz) + '</div>' : '');
}

// Toggle the advanced (raw cron expression) disclosure. Closing clears the
// advanced input so the picker resumes control.
function freqToggleAdvanced(btn) {
  const body = document.getElementById('freq-advanced-body');
  const open = body.style.display !== 'none';
  body.style.display = open ? 'none' : '';
  btn.classList.toggle('open', !open);
  if (!open) {
    const input = document.getElementById('freq-advanced-input');
    if (input) input.focus();
  } else {
    const input = document.getElementById('freq-advanced-input');
    if (input) input.value = '';
    freqUpdate();
  }
}

// buildCronWorkspaceBody renders only the workspace picker body (the outer
// field label is provided by the two-column grid wrapper). Retained IDs:
// #cron-ws-list, #cron-ws-custom-toggle, #cron-ws-custom-form, #cron-workdir
// — collectors and toggle helpers depend on those names.
function buildCronWorkspaceBody() {
  let html = '';
  if (projectsData.length > 0) {
    html += '<ul class="proj-pick" id="cron-ws-list" role="listbox" aria-label="工作目录">' +
      projectsData.map(p =>
        '<li role="option" data-path="' + escAttr(p.path) + '" onclick="cronSelectWorkspace(this, \'' + escJs(p.path) + '\')">' +
          '<div class="pp-name">' + esc(p.name) + '</div>' +
          '<div class="pp-path">' + esc(shortPath(p.path)) + '</div>' +
        '</li>'
      ).join('') +
      '<li id="cron-ws-custom-toggle" role="option" onclick="toggleCronWsCustom()">' +
        '<div class="pp-custom"><span class="pp-custom-icon">+</span> 自定义路径</div>' +
      '</li>' +
      '</ul>';
  }
  html += '<div id="cron-ws-custom-form" style="' + (projectsData.length > 0 ? 'display:none;' : '') + 'margin-top:6px">' +
    '<input id="cron-workdir" placeholder="' + escAttr(defaultWorkspace || '/home/user/project') + '" aria-label="工作目录路径">' +
    '</div>';
  return html;
}

// buildScheduleSection renders the frequency picker + advanced disclosure +
// preview panel used by create and edit modals. initialRawExpr is only set
// when an existing job's schedule doesn't match any picker shape; we surface
// it via the advanced input so the user can still edit without losing the
// original expression.
function buildScheduleSection(initialDesc, initialRawExpr) {
  const pickerHtml = buildFreqPickerHtml(initialDesc);
  const advancedOpen = !!initialRawExpr;
  const rawValue = initialRawExpr ? escAttr(initialRawExpr) : '';
  // The outer .cf-label ("什么时候") from renderCronModalBody provides the
  // section heading in the two-column modal, so we don't duplicate it here.
  return pickerHtml +
    '<button type="button" class="freq-advanced-toggle' + (advancedOpen ? ' open' : '') + '" onclick="freqToggleAdvanced(this)">' +
      '<span class="chev">&#9656;</span>' +
      '<span>我要写 cron 表达式</span>' +
    '</button>' +
    '<div class="freq-advanced-body" id="freq-advanced-body" style="' + (advancedOpen ? '' : 'display:none') + '">' +
      '<input id="freq-advanced-input" type="text" placeholder="@every 30m or 0 9 * * 1-5" value="' + rawValue + '" oninput="freqUpdate()">' +
      '<div class="cron-tz-hint" style="margin-top:4px">留空则使用上面的频率选择器；填写后覆盖选择器。</div>' +
    '</div>' +
    '<div class="freq-preview" id="freq-preview">' +
      '<div class="freq-preview-title">接下来会在这些时间运行</div>' +
      '<div style="color:var(--nz-text-faint)">...</div>' +
    '</div>';
}

function createNewCronJob() {
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
  return '<div class="modal-body">' +
      '<div class="cron-modal-grid">' +
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
    if (workDir) body.work_dir = workDir;
    const notifyVals = collectCronNotifyValues();
    if (notifyVals.notify !== null) body.notify = notifyVals.notify;
    if (notifyVals.notify_platform !== null) body.notify_platform = notifyVals.notify_platform;
    if (notifyVals.notify_chat_id !== null) body.notify_chat_id = notifyVals.notify_chat_id;
    const freshCtx = collectCronContextValue();
    if (freshCtx === true) body.fresh_context = true;
    const r = await fetch('/api/cron', {method: 'POST', headers, body: JSON.stringify(body)});
    if (!r.ok) {
      const errText = await r.text().catch(() => '');
      showAPIError('创建定时任务', r.status, errText);
      return;
    }
    const data = await r.json();
    if (overlay) overlay.remove();
    showToast('定时任务已创建', 'success');
    fetchCronJobs();
    if (data.id) {
      const key = 'cron:' + data.id;
      sessionWorkspaces[key] = workDir || defaultWorkspace || '/tmp';
      lastVersion = 0;
      await fetchSessions();
      selectSession(key, 'local');
    }
  } catch (e) { showNetworkError('创建定时任务', e); }
}

function openCronPanel() {
  // Deselect managed session via selectedKey only — selectedNode is the
  // sidebar filter now (see previewDiscovered comment) and must survive
  // opening the cron panel so the user comes back to the right node list.
  selectedKey = null;
  if (wsm.subscribedKey) wsm.unsubscribe();
  if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
  mobileEnterChat();
  // Paint immediately from the cache primed at page load (line ~5982) so the
  // click feels instant. If the cache is empty we still render the panel —
  // renderCronPanel handles the zero-job "empty state" branch. A background
  // refresh reconciles with the server and re-renders if anything changed.
  renderCronPanel();
  fetchCronJobs().then(() => renderCronPanel());
}

// filterCronJobs is the pure match step for the R110-P2 cron panel filter.
// Extracted so unit tests exercise the predicate without driving DOM. Match
// surface for the substring arm: prompt, work_dir, schedule, id (all
// case-insensitive). Status arm is one of 'all' | 'active' | 'attention',
// where 'attention' == paused OR last_error (dovetails with the header
// cron-badge's attention count so both surfaces speak the same predicate).
function filterCronJobs(jobs, query, status) {
  const q = (query || '').trim().toLowerCase();
  const s = status || 'all';
  return (Array.isArray(jobs) ? jobs : []).filter(j => {
    if (!j) return false;
    if (s === 'active' && j.paused) return false;
    if (s === 'attention' && !(j.paused || j.last_error)) return false;
    if (!q) return true;
    const fields = [j.prompt, j.work_dir, j.schedule, j.id];
    for (const f of fields) {
      if (typeof f === 'string' && f.toLowerCase().indexOf(q) !== -1) return true;
    }
    return false;
  });
}

// cronJobCardHtml renders a single cron card. Extracted from the legacy
// renderCronPanel map() body so renderCronList can iterate over a filtered
// slice without duplicating the (non-trivial) markup. Pure w.r.t. inputs.
function cronJobCardHtml(j) {
  const status = j.paused ? '<span class="badge paused">paused</span>' : '<span class="badge running">active</span>';
  const nextStr = j.next_run ? timeAgo(j.next_run, true) : '';
  const lastStr = j.last_run_at ? timeAgo(j.last_run_at) : '';
  const nextAbs = j.next_run ? formatAbsTime(j.next_run) : '';
  const lastAbs = j.last_run_at ? formatAbsTime(j.last_run_at) : '';
  const wdStr = j.work_dir ? '<span class="cc-ws" title="' + escAttr(j.work_dir) + '">' + esc(shortPath(j.work_dir)) + '</span>' : '';
  let notifyStr = '';
  if (j.notify === true) {
    const tgt = (j.notify_platform && j.notify_chat_id)
      ? j.notify_platform + ':' + j.notify_chat_id
      : (cronNotifyDefault ? cronNotifyDefault.platform + ':' + cronNotifyDefault.chat_id : 'default');
    notifyStr = '<span class="cc-notify on" title="IM 通知 → ' + escAttr(tgt) + '">&#128276; notify</span>';
  } else if (j.notify === false) {
    notifyStr = '<span class="cc-notify off" title="IM 通知已关闭">&#128277; silent</span>';
  }
  const freshStr = j.fresh_context
    ? '<span class="cc-notify on" title="每次运行前重置会话">&#128260; fresh</span>'
    : '';
  let result = '';
  if (j.last_error) {
    result = '<div class="cc-result err"><span class="cc-icon">\u2716</span><span class="cc-text">' + esc(j.last_error) + '</span></div>';
  } else if (j.last_result) {
    result = '<div class="cc-result ok"><span class="cc-icon">\u2714</span><span class="cc-text">' + esc(j.last_result) + '</span></div>';
  }
  const promptBlock = j.prompt
    ? '<div class="cc-prompt">' + esc(j.prompt) + '</div>'
    : '<div class="cc-prompt placeholder">未设置 prompt（点右侧 edit 按钮配置）</div>';
  const toggleBtn = j.paused
    ? '<button type="button" class="cc-btn" onclick="cronResume(\'' + escJs(j.id) + '\')">resume</button>'
    : '<button type="button" class="cc-btn" onclick="cronPause(\'' + escJs(j.id) + '\')">pause</button>';
  // Run Now button — hidden for paused jobs because the backend rejects
  // TriggerNow with 409 ErrJobPaused, so the click would only produce a
  // localized "状态冲突" toast without advancing the operator. For active
  // jobs the backend's jobRunningGuard + SkipIfStillRunning chain already
  // protects against overlap; we don't need a frontend disable state.
  const runBtn = j.paused
    ? ''
    : '<button type="button" class="cc-btn" onclick="cronTriggerNow(\'' + escJs(j.id) + '\')" title="立即执行一次" aria-label="立即执行一次">run</button>';
  const human = humanizeCron(j.schedule);
  const showRaw = human !== j.schedule;
  return '<div class="cron-card" role="button" tabindex="0" onclick="openCronSession(\'' + escJs(j.id) + '\')" onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();openCronSession(\'' + escJs(j.id) + '\')}">' +
    promptBlock +
    '<div class="cc-human">' + esc(human) + '</div>' +
    (showRaw ? '<div class="cc-expr">' + esc(j.schedule) + '</div>' : '') +
    '<div class="cc-meta">' + status + wdStr + notifyStr + freshStr +
      (lastStr ? '<span' + (lastAbs ? ' title="last run: ' + escAttr(lastAbs) + '"' : '') + '>ran ' + lastStr + '</span>' : '') +
      (nextStr ? '<span' + (nextAbs ? ' title="next run: ' + escAttr(nextAbs) + '"' : '') + '>next ' + nextStr + '</span>' : '') +
    '</div>' +
    result +
    '<div class="cc-actions" onclick="event.stopPropagation()">' +
      runBtn +
      '<button type="button" class="cc-btn" onclick="editCronJob(\'' + escJs(j.id) + '\')">edit</button>' +
      toggleBtn +
      '<button type="button" class="cc-btn danger" onclick="cronDelete(\'' + escJs(j.id) + '\')">delete</button>' +
    '</div>' +
  '</div>';
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
        '<div class="cron-empty-hint">No cron jobs yet</div>' +
        '<div class="cron-empty-sub">让 naozhi 按计划自动在某个工作目录下运行 prompt</div>' +
        '<button type="button" class="cron-empty-cta" onclick="createNewCronJob()">Create your first cron job</button>' +
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
  const sorted = [...matched].sort((a, b) => b.created_at - a.created_at);
  host.innerHTML = sorted.map(cronJobCardHtml).join('');
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

// setCronStatusFilter toggles between the three status modes. Re-applies
// aria-pressed + active class on the chip row so the current mode is
// visible + SR-accessible, then repaints the list.
function setCronStatusFilter(status) {
  if (status !== 'all' && status !== 'active' && status !== 'attention') return;
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

function renderCronPanel() {
  // Guard against an async race: fetchCronJobs().then(renderCronPanel) fires
  // after the user may have already clicked a cron card and opened a session.
  // Re-rendering the cron list would clobber renderMainShell's DOM and make
  // the chat history "disappear". Only paint when no session is selected.
  if (selectedKey) return;
  const main = document.getElementById('main');
  // Shell-preserving repaint: when the cron panel is already mounted (user
  // is just typing in the search box or toggling a chip), we only want to
  // repaint the list. Rebuilding the shell would wipe the input value and
  // steal focus. Detect by probing for the list host element.
  if (document.getElementById('cron-list-items')) {
    renderCronList();
    return;
  }
  const tzBanner = cronTimezoneLabel
    ? '<div class="cron-tz-banner" title="Schedules are evaluated in this timezone">timezone: ' + esc(cronTimezoneLabel) + '</div>'
    : '';
  const chipActive = s => cronFilterStatus === s ? ' active' : '';
  const chipPressed = s => cronFilterStatus === s ? 'true' : 'false';
  let html =
    '<div class="main-header">' +
      '<button class="btn-mobile-back" onclick="mobileBack()" title="back" aria-label="Back to sidebar">&#8592;</button>' +
      '<div class="main-header-content"><h2>Cron Jobs</h2></div>' +
    '</div>' +
    '<div class="cron-detail">' +
      '<div class="cron-detail-body">' +
        '<div class="cron-list-head">' +
          '<h3>Cron Jobs</h3>' +
          '<button type="button" class="cron-new-btn" onclick="createNewCronJob()" aria-label="Create new cron job">' +
            '<svg viewBox="0 0 24 24" aria-hidden="true"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>' +
            ' New' +
          '</button>' +
        '</div>' +
        '<div class="cron-filter-bar">' +
          '<div class="cron-search-row">' +
            '<input type="text" id="cron-search-input" class="cron-search-input" placeholder="搜索 prompt、目录、cron 表达式..." autocomplete="off" spellcheck="false" aria-label="搜索定时任务" value="' + escAttr(cronFilterQuery) + '" oninput="onCronSearchInput()" />' +
            '<button type="button" class="cron-search-clear" onclick="clearCronSearch()" title="清空搜索" aria-label="清空搜索">&times;</button>' +
          '</div>' +
          '<div class="cron-status-chips" role="group" aria-label="按状态筛选">' +
            '<button type="button" class="cron-status-chip' + chipActive('all') + '" data-status="all" aria-pressed="' + chipPressed('all') + '" onclick="setCronStatusFilter(\'all\')">全部</button>' +
            '<button type="button" class="cron-status-chip' + chipActive('active') + '" data-status="active" aria-pressed="' + chipPressed('active') + '" onclick="setCronStatusFilter(\'active\')">运行中</button>' +
            '<button type="button" class="cron-status-chip' + chipActive('attention') + '" data-status="attention" aria-pressed="' + chipPressed('attention') + '" onclick="setCronStatusFilter(\'attention\')">需关注</button>' +
          '</div>' +
        '</div>' +
        tzBanner +
        '<div id="cron-list-items"></div>' +
      '</div>' +
    '</div>';
  main.innerHTML = html;
  // Paint list now that the shell is mounted; subsequent keystrokes / chip
  // flips route through renderCronList directly without touching the shell.
  renderCronList();
}

function openCronSession(cronId) {
  const key = 'cron:' + cronId;
  // Ensure the session appears in the sidebar (may be pending if never sent)
  if (!sessionsData[sid(key, 'local')] && !sessionWorkspaces[key]) {
    sessionWorkspaces[key] = defaultWorkspace || '/tmp';
    lastVersion = 0;
    debouncedFetchSessions();
  }
  selectSession(key, 'local');
}

async function fetchCronJobs() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron', { headers });
    if (!r.ok) return;
    const data = await r.json();
    cronJobs = data.jobs || [];
    cronTimezoneLabel = data.timezone_label || data.timezone || '';
    cronNotifyDefault = data.notify_default || null;
    const cronBadge = document.getElementById('cron-badge');
    if (cronBadge) {
      // Badge surfaces jobs needing attention (paused or last run errored),
      // not the raw total — avoids a persistent red dot on healthy setups.
      const attention = cronJobs.filter(j => j.paused || j.last_error).length;
      cronBadge.textContent = attention;
      cronBadge.style.display = attention > 0 ? '' : 'none';
      // Attention badge is semantically an alert (paused / errored jobs), so
      // opt into the red .is-alert variant defined in dashboard.html Track D.
      // History badge stays neutral grey because it is a cumulative count, not
      // an unread/failure signal.
      cronBadge.classList.toggle('is-alert', attention > 0);
    }
  } catch (e) { console.error('fetch cron:', e); }
}

// cronTriggerNow calls POST /api/cron/trigger to kick off a job immediately
// without waiting for the next scheduled tick. Useful when the operator
// wants to verify a prompt edit or rerun after a transient failure.
//
// Contract notes:
//   - Backend rejects paused jobs with 409 ErrJobPaused; the button is
//     hidden for paused jobs (cronJobCardHtml), so 409 here usually means a
//     pause landed between render and click — surface it via showAPIError
//     rather than a custom message.
//   - Backend's jobRunningGuard + SkipIfStillRunning chain serializes
//     against the scheduled-tick path, so a double-click at worst sees one
//     execution plus an "already running, skipping overlap" Info log. No
//     frontend debounce is needed.
//   - Success response is {status:"triggered"}; the eventual run completion
//     arrives via the existing cron_update WS event (last_run_at / last_result)
//     which already repaints the card.
async function cronTriggerNow(id) {
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/trigger', { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('立即执行定时任务', r.status, raw);
      return;
    }
    showToast('已触发执行', 'success', 2000);
  } catch (e) { showNetworkError('立即执行定时任务', e); }
}

async function cronPause(id) {
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/pause', { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('暂停定时任务', r.status, raw);
      return;
    }
    fetchCronJobs().then(() => renderCronPanel());
  } catch (e) { showNetworkError('暂停定时任务', e); }
}

async function cronResume(id) {
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/resume', { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('恢复定时任务', r.status, raw);
      return;
    }
    fetchCronJobs().then(() => renderCronPanel());
  } catch (e) { showNetworkError('恢复定时任务', e); }
}

async function cronDelete(id) {
  const job = (Array.isArray(cronJobs) ? cronJobs.find(j => j.id === id) : null) || {};
  const promptPreview = (job.prompt || '').slice(0, 200);
  const ok = await confirmDialog({
    title: '删除定时任务？',
    message: '任务将被永久删除，下次不再触发。',
    detail: promptPreview ? promptPreview : ('id: ' + id),
    confirmText: '删除',
    variant: 'danger',
  });
  if (!ok) return;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron?id=' + encodeURIComponent(id), { method: 'DELETE', headers });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('删除定时任务', r.status, raw);
      return;
    }
    fetchCronJobs().then(() => renderCronPanel());
  } catch (e) { showNetworkError('删除定时任务', e); }
}

// Edit an existing cron job. Opens a modal pre-populated with the current
// schedule, prompt, and work_dir. The frequency picker tries to restore the
// job's schedule via parseCronToFreq — when it can't (e.g. user wrote a
// custom expression by hand), we surface the raw expression in the advanced
// disclosure so it can still be edited without loss.
function editCronJob(id) {
  const job = cronJobs.find(j => j.id === id);
  if (!job) { showToast('未找到该任务', 'warning'); return; }

  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  const notifyInitial = job.notify === true ? 'on' : (job.notify === false ? 'off' : '');
  const hasOverride = !!(job.notify_platform && job.notify_chat_id);
  const notifyHtml = buildCronNotifyToggleHtml(notifyInitial, hasOverride, job.notify_platform, job.notify_chat_id);
  const contextHtml = buildCronContextToggleHtml(!!job.fresh_context);

  // Round-trip attempt: if the saved expression matches a picker shape we
  // restore the picker; otherwise open the advanced disclosure with the raw
  // expression so power users don't lose their handcrafted schedule.
  const initialDesc = parseCronToFreq(job.schedule);
  const initialRaw = initialDesc ? '' : (job.schedule || '');
  const scheduleHtml = buildScheduleSection(initialDesc || { mode: 'interval', n: 1, unit: 'h' }, initialRaw);

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
        promptId: 'edit-cron-prompt',
        promptPlaceholder: '这个任务要做什么？',
      }) +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">取消</button>' +
        '<button type="button" class="primary" onclick="doEditCronJob(\'' + escJs(id) + '\')">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  fillCronPrompt('edit-cron-prompt', job.prompt);

  overlay.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') overlay.remove();
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      doEditCronJob(id);
    }
  });

  // Seed overlay._cronSchedule so "save without edits" still sends the
  // correct schedule, then trigger the first preview render.
  //
  // Do NOT seed overlay._cronWorkDir from job.work_dir — the submit path
  // uses overlay._cronWorkDir only as a fallback when the input is empty,
  // which is exactly the user-cleared-workdir case. Seeding the current
  // value would prevent clearing. Picker clicks (cronSelectWorkspace)
  // explicitly set _cronWorkDir; the input's pre-filled value covers the
  // unchanged-workdir case.
  overlay._cronSchedule = job.schedule || '';
  overlay._cronWorkDir = '';
  freqUpdate();
}

// buildEditCronWorkspaceBody is the edit-mode counterpart to
// buildCronWorkspaceBody — renders the same picker but with the current
// job.work_dir pre-selected (if it matches a known project) or visible in
// the custom input. Shares the input#cron-workdir / #edit-cron-workdir
// contract via the explicit inputId parameter.
function buildEditCronWorkspaceBody(currentDir) {
  const hasProjects = projectsData.length > 0;
  const matched = hasProjects && currentDir ? projectsData.find(p => p.path === currentDir) : null;
  const showCustom = !matched && currentDir;
  let html = '';
  if (hasProjects) {
    html += '<ul class="proj-pick" id="cron-ws-list" role="listbox" aria-label="工作目录">' +
      projectsData.map(p => {
        const selected = matched && matched.path === p.path;
        return '<li role="option" data-path="' + escAttr(p.path) + '"' +
          (selected ? ' class="selected" aria-selected="true"' : '') +
          ' onclick="cronSelectWorkspace(this, \'' + escJs(p.path) + '\')">' +
            '<div class="pp-name">' + esc(p.name) + '</div>' +
            '<div class="pp-path">' + esc(shortPath(p.path)) + '</div>' +
          '</li>';
      }).join('') +
      '<li id="cron-ws-custom-toggle" role="option"' + (showCustom ? ' style="display:none"' : '') +
      ' onclick="toggleCronWsCustom()">' +
        '<div class="pp-custom"><span class="pp-custom-icon">+</span> 自定义路径</div>' +
      '</li>' +
      '</ul>';
  }
  html += '<div id="cron-ws-custom-form" style="' + (hasProjects && !showCustom ? 'display:none;' : '') + 'margin-top:6px">' +
    '<input id="edit-cron-workdir" placeholder="' + escAttr(defaultWorkspace || '/home/user/project') + '" value="' + escAttr(currentDir || '') + '" aria-label="工作目录路径">' +
    '</div>';
  return html;
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
    fetchCronJobs().then(() => renderCronPanel());
  } catch (e) {
    showNetworkError('保存定时任务', e);
  }
}

/* ===== Sidebar resizer (desktop only) ===== */
(function(){
  const resizer = document.getElementById('resizer');
  const sidebar = document.querySelector('.sidebar');
  const LS_KEY = 'naozhi_sidebar_w';
  const saved = parseFloat(localStorage.getItem(LS_KEY));
  if (saved >= 200) sidebar.style.width = saved + 'px';

  let startX, startW;
  resizer.addEventListener('mousedown', function(e) {
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
    localStorage.setItem(LS_KEY, Math.round(sidebar.getBoundingClientRect().width));
  }
  resizer.addEventListener('dblclick', function() {
    sidebar.style.width = '360px';
    localStorage.removeItem(LS_KEY);
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
      '<h3>欢迎使用 脑汁 Dashboard</h3>' +
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

fetchSessions().then(maybeShowOnboarding);
sessionPollTimer = setInterval(fetchSessions, 5000);
scanDiscovered();
discoveredPollTimer = setInterval(scanDiscovered, 30000);
fetchCronJobs(); // load initial cron state for badge
wsm.connect();
initMobile();
initViewportTracking();
initSwipeDelete();
initSwipeBack();
initSidebarSearch();
(function(){
  var ov=document.createElement('div');ov.className='lightbox-overlay';
  ov.setAttribute('role','dialog');ov.setAttribute('aria-modal','true');ov.setAttribute('aria-label','Image preview');
  ov.innerHTML='<img alt=""><div class="lb-zoom-hint"></div>';document.body.appendChild(ov);
  var img=ov.querySelector('img'),hint=ov.querySelector('.lb-zoom-hint');
  var scale=1,panX=0,panY=0,dragging=false,lx=0,ly=0,ht=null;
  function showHint(){hint.textContent=Math.round(scale*100)+'%';hint.classList.add('visible');clearTimeout(ht);ht=setTimeout(function(){hint.classList.remove('visible')},1200)}
  function apply(){img.style.transform=(scale===1&&!panX&&!panY)?'':'translate('+panX+'px,'+panY+'px) scale('+scale+')';ov.classList.toggle('zoomed',scale>1)}
  function reset(){scale=1;panX=0;panY=0;dragging=false;img.style.transform='';ov.classList.remove('zoomed','dragging');hint.classList.remove('visible');clearTimeout(ht)}
  function close(){ov.classList.remove('active');reset()}
  ov.addEventListener('click',function(e){if(e.target===ov)close()});
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
  window.openLightbox=function(src){reset();img.src=src;ov.classList.add('active')};
  document.addEventListener('keydown',function(e){if(!ov.classList.contains('active'))return;if(e.key==='Escape')close();else if(e.key==='+'||e.key==='='){scale=Math.min(scale*1.2,10);apply();showHint()}else if(e.key==='-'){scale=Math.max(scale/1.2,.5);apply();showHint()}else if(e.key==='0'){reset();apply();showHint()}});
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
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
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
      if (!r.ok) return;
      const evs = await r.json();
      if (Array.isArray(evs) && evs.length > 0) {
        renderNewEvents(evs);
        // Hide the "thinking…" indicator once the first bubble arrives.
        if (evs.some(e => e.type === 'text' || e.type === 'result')) {
          elLoading.classList.remove('visible');
        }
      }
    } catch (_) { /* swallow */ }
  }

  function startPolling() {
    stopPolling();
    pollTimer = setInterval(pollOnce, 1000);
  }

  async function openScratch(quote, agentId, sourceKey, sourceMsgTime) {
    // Confirm replacement if an aside is already open. Replacement is
    // non-destructive (the previous scratch is still reachable via history)
    // so we use 'primary' variant instead of 'danger'.
    if (state) {
      const ok = (typeof confirmDialog === 'function')
        ? await confirmDialog({
            title: '替换当前追问窗口？',
            message: '当前未保存为正式会话的追问内容将被关闭。',
            confirmText: '替换',
            variant: 'primary',
          })
        : confirm('当前追问窗口将被替换，继续？');
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
