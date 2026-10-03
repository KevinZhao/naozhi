// session_list.js — the sidebar's session list: the /api/sessions poll and its
// merge with pending (not yet sent) sessions, the cards and badges, the status
// bar, the node helpers, and the WS state, subscription and session_state frames.
import { NZ_CONTRACT } from './contract.js';
import { getToken, lsGet } from './platform.js';
import { sessionStream } from './session_stream.js';
import { WS_STATES, wsm } from './ws_manager.js';
import { perSession, selection, serverInfo, sessionList, timers, transcript } from './state.js';
import { esc, escAttr, fetchJSON, patchCardExitChip, reconcileChildren, sessionExitChipHtml } from './nz_util.js';
import { setHeaderEffortChip, setHeaderOverlayDriftChip, setHeaderSpawnDiagChip } from './session_header.js';
import { deselectNodeSession, reconcileSelectedNode } from './system_view.js';
import { turnState, updateSendButton } from './running_banner.js';
import { PENDING_LS_KEY, announce, formatAbsTime, persistPending, renderRecentSessionsPanel, setActiveSessionCard, showAuthModal, timeAgo } from './utilities.js';
import { scanDiscovered } from './discovery.js';
import { invalidateGitState } from './tuning.js';
import { sectionHeaderFallbackHtml, sectionHeaderHtml } from './sidebar_project.js';
import { accessProfileChipHtml, backendDisplayName, backendDisplayVersion } from './auth_modal.js';
import { _optimisticRunningTimers } from './send_message.js';
import { fetchEvents } from './event_stream.js';
import { discoveredKey, getNodeDisplayName, isMultiNode, matchProject, nodeColor, sessionTypeTag, sid } from './session_ident.js';
import { ICONS } from './icons.js';

// collectWorkspaceSessionIDs returns the set of Claude session UUIDs that the
// sidebar already represents — current session_id PLUS any prev_session_ids
// from auto-chain history. Used to deduplicate the history popover/badge so
// links in an active chain aren't surfaced twice (once in workspace, once in
// history). Skips empty strings defensively in case the API ever returns
// nulls inside prev_session_ids.
export function collectWorkspaceSessionIDs(sessions) {
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


// restorePending rehydrates the in-memory pending maps from localStorage at
// boot, BEFORE the first fetchSessions/send. Idempotent via _pendingRestored
// (multiple DOMContentLoaded listeners exist). Every entry is shape-validated:
// a hand-edited blob cannot inject a non-string key or a non-absolute ws path
// (defense in depth — the server still re-validates the workspace on send).
export function restorePending() {
  if (selection.pendingRestored) return;
  selection.pendingRestored = true;
  const saved = lsGet(PENDING_LS_KEY, {});
  if (!saved || typeof saved !== 'object') return;
  for (const [k, v] of Object.entries(saved)) {
    if (typeof k !== 'string' || !k) continue;
    if (!v || typeof v !== 'object' || typeof v.ws !== 'string' || !v.ws) continue;
    if (v.ws[0] !== '/' && v.ws[0] !== '~') continue; // reject relative / junk
    perSession.workspaces[k] = v.ws;
    if (v.node && v.node !== 'local') perSession.nodes[k] = v.node;
    if (v.backend) perSession.backends[k] = v.backend;
    if (typeof v.access_profile === 'string' && v.access_profile) perSession.accessProfiles[k] = v.access_profile;
  }
}

// fetchSessionsPayload GETs /api/sessions. null is a failed poll the caller
// reports as false (an auth failure, after prompting for a token, or an HTTP
// error); a network error throws. The 8 s timeout releases a hung response
// before the next 5 s poll tick (RNEW-UX-003).
async function fetchSessionsPayload() {
  const headers = {};
  const t = getToken();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  try {
    return await fetchJSON(NZ_CONTRACT.API.sessions, { headers, timeoutMs: 8000 });
  } catch (err) {
    if (err.status === 401 || err.status === 403) {
      showAuthModal({ auto: true }); // background poll: respects de-dupe + cooldown
      return null;
    }
    if (err.status) return null;
    throw err;
  }
}

// sessionsUnchanged reports whether the poll carries nothing new, and
// otherwise records it as the last one seen. stats.version changes on session
// add/remove/rename/reset; nodes and history have no version and compare as
// JSON. Process state flips (running↔ready, last_response) never advance the
// version, which over a live socket the session_state push covers; under
// WS-fallback polling REST is the only state source, so the short-circuit
// would freeze the sidebar and it applies only while connected (#2431).
// renderSidebar is idempotent, so the 5 s fallback repaint is the intended
// cost.
function sessionsUnchanged(data, wsConnected) {
  const version = (data.stats && data.stats.version) || 0;
  const nodesHash = JSON.stringify(data.nodes || {});
  const historyHash = JSON.stringify(data.history_sessions || []);
  if (wsConnected && version === sessionList.lastVersion && version > 0 && nodesHash === sessionList.lastNodesJSON && historyHash === sessionList.lastHistoryJSON) return true;
  sessionList.lastVersion = version;
  sessionList.lastNodesJSON = nodesHash;
  sessionList.lastHistoryJSON = historyHash;
  return false;
}

function applySessionsStats(data) {
  if (data.nodes) sessionList.nodesData = data.nodes;
  if (data.stats.default_workspace) serverInfo.defaultWorkspace = data.stats.default_workspace;
  if (data.stats.projects) sessionList.projectsData = data.stats.projects;
  if (data.stats.cli_name) serverInfo.defaultCLIName = data.stats.cli_name;
  if (data.stats.cli_version) serverInfo.defaultCLIVersion = data.stats.cli_version;
  // The Home panel's health strip reads uptime / watchdog / active-count off
  // the full stats object without a second fetch.
  serverInfo.lastStatsSnapshot = data.stats;
  sessionList.historySessionsData = data.history_sessions || [];
}

// mergeBackendSessions folds the polled sessions into sessionsData, adds each
// key to backendKeys and returns the list the sidebar paints from.
function mergeBackendSessions(polled, backendKeys) {
  // A session the operator just dismissed stays out until its DELETE
  // resolves, or a lagging poll re-adds the card; a failed delete clears the
  // key and the session reappears on the next poll.
  const live = sessionList.optimisticDeleteKeys.size > 0
    ? polled.filter(s => !sessionList.optimisticDeleteKeys.has(sid(s.key, s.node || 'local')))
    : polled;
  return live.map(s => {
    const n = s.node || 'local';
    const sKey = sid(s.key, n);
    // Keep the optimistic 'running' flip while the REST snapshot still lags
    // the send, or the banner hides until the session_state push catches up.
    // It lands in the returned copy too: a re-render from the cached payload
    // (project collapse, sidebar search) would otherwise paint the card idle
    // while the banner shows running (#2431).
    if (perSession.optimisticRunning[sKey] && s.state !== 'running') {
      s = Object.assign({}, s, { state: 'running' });
    }
    // A new workspace (/cd, or the first snapshot after spawn resolved the
    // real cwd) can be another repo, another worktree or no repo at all.
    const prevWS = sessionList.sessionsData[sKey] && sessionList.sessionsData[sKey].workspace;
    if (prevWS !== undefined && prevWS !== s.workspace) {
      invalidateGitState(s.key, n);
    }
    sessionList.sessionsData[sKey] = s;
    backendKeys.add(s.key);
    return s;
  });
}

// reconcilePending forgets the pending sessions the backend now lists and
// persists once: the durable blob must drop them, or a reload re-injects a
// ghost card, and removePendingSession per key would re-serialize the whole
// blob once per key.
function reconcilePending(backendKeys) {
  let reconciledAny = false;
  for (const key of Object.keys(perSession.workspaces)) {
    if (!backendKeys.has(key)) continue;
    delete perSession.workspaces[key];
    delete perSession.nodes[key];
    delete perSession.backends[key];
    delete perSession.accessProfiles[key];
    delete perSession.pendingTuning[key];
    reconciledAny = true;
  }
  if (reconciledAny) persistPending();
}

// appendPendingCards adds a card for each pending session the backend does
// not list yet. After reconcilePending no such key is left; the check keeps
// the backend copy the only card if the two calls are ever reordered.
function appendPendingCards(sessions, backendKeys) {
  for (const key of Object.keys(perSession.workspaces)) {
    if (!backendKeys.has(key)) sessions.push(pendingCardFor(key));
  }
}

// pendingCardFor is the sidebar row of a session this browser created but the
// backend does not list yet.
function pendingCardFor(key) {
  const parts = key.split(':');
  // The agent chip shows the palette pick off the key tail; a legacy
  // 3-segment key degrades to "general".
  const pendingAgent = parts.length >= 4 && parts[3] ? parts[3] : 'general';
  // The CLI brand (sidebar icon, chat header) follows the backend pick before
  // the first message spawns the wrapper; a kiro pick must not show the
  // claude logomark. With one backend there is no picker and no pick, and the
  // lone backend is defaultCLIName. See backendDisplayName godoc.
  const pendingBackend = perSession.backends[key] || '';
  const pendingCLIName = backendDisplayName(pendingBackend) || serverInfo.defaultCLIName;
  // On the default backend the live version (from system/init, refreshed
  // every poll) beats the manifest, which is cached up to 60 s and would flash
  // the pre-upgrade version; another backend has no live source, and the
  // default's version would mislabel it. R20260613-pending-version.
  const isDefaultBackend = !pendingBackend || (serverInfo.cliBackends !== null && pendingBackend === serverInfo.cliBackends.default);
  const pendingCLIVersion = isDefaultBackend
    ? (serverInfo.defaultCLIVersion || backendDisplayVersion(pendingBackend))
    : (backendDisplayVersion(pendingBackend) || serverInfo.defaultCLIVersion);
  // Mirror the server's project/project_fallback shape, so an unregistered
  // workspace groups under its basename from the first paint (#2431).
  const pendingWS = perSession.workspaces[key];
  const pendingProject = matchProject(pendingWS);
  const pendingFallback = pendingProject ? '' : workspaceFallbackName(pendingWS);
  return {
    key: key,
    state: 'new',
    platform: parts[0] || 'dashboard',
    agent: pendingAgent,
    workspace: pendingWS,
    // "now" sorts the card to the bottom (oldest first) at once; 0 would sort
    // it to the top until the real session, stamped about the same, snaps it
    // down.
    created_at: Date.now(),
    last_active: 0,
    last_prompt: '',
    last_response: '',
    node: perSession.nodes[key] || 'local',
    project: pendingProject || pendingFallback,
    project_fallback: !!pendingFallback,
    backend: pendingBackend,
    access_profile: perSession.accessProfiles[key] || '',
    cli_name: pendingCLIName,
    cli_version: pendingCLIVersion,
  };
}

// onSessionsApplied registers fn(wsConnected) to run, in registration order,
// at the end of every poll that applied a new payload, after the sidebar
// repaint. dashboard.js hooks the main-state reconcile and the header's CLI
// label here, so this module does not import it back.
const sessionsAppliedHooks = [];
export function onSessionsApplied(fn) {
  sessionsAppliedHooks.push(fn);
}

export async function fetchSessions() {
  try {
    let data = await fetchSessionsPayload();
    if (!data) return false;
    // The effort tier, spawn diagnosis and overlay drift change at turn
    // boundaries, which do not advance stats.version, and renderMainShell does
    // not run on turn completion: they repaint here, before the short-circuit.
    // docs/rfc/kiro-effort-visibility.md §5.1 / R1b
    if (selection.key) setHeaderEffortChip(data.sessions);
    if (selection.key) setHeaderSpawnDiagChip(data.sessions);
    if (selection.key) setHeaderOverlayDriftChip(data.sessions);
    const wsConnected = wsm.state === WS_STATES.CONNECTED;
    if (sessionsUnchanged(data, wsConnected)) return;
    applySessionsStats(data);
    const backendKeys = new Set();
    data = Object.assign({}, data, { sessions: mergeBackendSessions(data.sessions || [], backendKeys) });
    reconcilePending(backendKeys);
    appendPendingCards(data.sessions, backendKeys);
    renderSidebar(data);
    // The sidebar search re-renders from this payload rather than polling the
    // server on every keystroke.
    sessionList.lastSidebarData = data;
    for (const fn of sessionsAppliedHooks) fn(wsConnected);
    return true;
  } catch (e) {
    console.error('fetchSessions:', e);
    return false;
  }
}

export function debouncedFetchSessions() {
  return new Promise(resolve => {
    timers.fetchDebounceResolvers.push(resolve);
    if (timers.fetchDebounce) clearTimeout(timers.fetchDebounce);
    timers.fetchDebounce = setTimeout(() => {
      timers.fetchDebounce = null;
      const resolvers = timers.fetchDebounceResolvers;
      timers.fetchDebounceResolvers = [];
      fetchSessions().then(() => resolvers.forEach(r => r()));
    }, 300);
  });
}

// sidebarRowKey is a sidebar row's reconcile identity: a card by its node and
// session key, a project header by its group key; other rows (the 未分组
// header, the empty-state CTA) match by position.
function sidebarRowKey(el) {
  if (el.classList.contains('session-card')) return 'c:' + (el.dataset.node || 'local') + '\n' + (el.dataset.key || '');
  if (el.classList.contains('section-header')) {
    const btn = el.querySelector('[data-action="project-collapse"]');
    if (btn) return 'h:' + btn.dataset.key;
  }
  return null;
}

export function renderSidebar(data) {
  const st = data.stats;
  updateStatusBar();
  if (st.default_workspace) serverInfo.defaultWorkspace = st.default_workspace;
  if (st.projects) sessionList.projectsData = st.projects;

  const list = document.getElementById('session-list');
  // R110-P2 empty-state CTA: keeps the "no sessions" text E2E asserts and adds
  // the header `+` button's action for first-time users.
  const html = sidebarHtml(buildSidebarItems(data)) || '<div class="no-sessions">no sessions<br><button type="button" class="no-sessions-cta" data-action="session-new">+ 开启你的第一个会话</button></div>';
  // Keyed reconcile: only the rows whose markup changed are replaced, so a
  // poll with nothing new touches no DOM, in-place patches (state dot, unread
  // chip, a removed card) compare as they stand, and kept nodes keep the
  // list's scroll position.
  if (reconcileChildren(list, html, sidebarRowKey) && selection.key) {
    // A replaced card drops the cached active-card ref; re-resolve it so
    // selector switches stay O(1) on the next click.
    setActiveSessionCard(selection.key, selection.node);
  }

  // The history button's count is an archive size, not an unread count, and
  // stays hidden; the popover header shows it (ui-polish-light-theme D10).
  const hBadge = document.getElementById('history-badge');
  if (hBadge) hBadge.style.display = 'none';

  // The Home panel's 最近会话 list mirrors every repaint (R110-P1).
  renderRecentSessionsPanel();
}

// buildSidebarItems is every row the sidebar lists: managed sessions and
// discovered terminal sessions from every connected node (each card carries
// a node badge), oldest first. It sorts in place the list it caches as
// allSessionsCache, whose order msg_nav walks. Cron stubs never reach it: the
// server filters them (cron-panel-consolidation RFC §4.2).
function buildSidebarItems(data) {
  const items = (data.sessions || []).map(s => {
    if (!s.source) s.source = 'managed';
    return s;
  });
  sessionList.discoveredItems.forEach(d => {
    items.push({
      key: discoveredKey(d.pid, d.node),
      state: d.state || 'ready',
      cli_name: d.cli_name || 'cli',
      type_label: d.type_label || '',
      last_active: d.last_active || d.started_at,
      last_prompt: d.last_prompt || d.summary || '',
      workspace: d.cwd,
      project: d.project || matchProject(d.cwd),
      node: d.node || 'local',
      source: 'terminal',
      _discovered: d,
    });
  });
  sessionList.allSessionsCache = items;
  // Stable order: a row never moves on activity or state change. created_at
  // is server-stamped (unix ms); a pre-feature payload falls back to
  // last_active.
  items.sort((a, b) => {
    const aC = a.created_at || a.last_active || 0;
    const bC = b.created_at || b.last_active || 0;
    if (aC !== bC) return aC - bC;
    return (a.key || '').localeCompare(b.key || '');
  });
  return items;
}

// groupSidebarItems buckets the rows by project, keyed by (node, name) so a
// remote and a local project of the same name stay apart. A fallback group
// (named after the workspace basename, no registered project) adds the path
// to its key, so /a/tmp and /b/tmp do not collapse into one mislabeled group.
// Every favorite project gets a group, empty or not, so its header always
// renders.
function groupSidebarItems(items) {
  const groups = {};
  const ungrouped = [];
  items.forEach(s => {
    const pn = s.project || '';
    if (!pn) { ungrouped.push(s); return; }
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
  });
  sessionList.projectsData.forEach(p => {
    if (!p.favorite) return;
    const pNode = p.node || 'local';
    const k = pNode + ':' + p.name;
    if (!groups[k]) groups[k] = {name: p.name, node: pNode, items: []};
  });
  return { groups, ungrouped };
}

// sortedGroupKeys orders the groups { tier, created, name }: favorites on top,
// fallback groups at the bottom, then by the project's server-stamped
// created_at. A group without one (a fallback group, or a pre-feature server)
// is anchored by its earliest session, so ad-hoc sessions land in the order
// their workspace was first opened; with no stamp at all, name decides.
function sortedGroupKeys(groups, projIndex) {
  const sortKeys = {};
  const groupKeys = Object.keys(groups);
  groupKeys.forEach(k => {
    const g = groups[k];
    const p = projIndex[k];
    let created = (p && p.created_at) ? p.created_at : 0;
    if (!created) {
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
  return groupKeys.sort((a, b) => {
    const ka = sortKeys[a], kb = sortKeys[b];
    if (ka.tier !== kb.tier) return ka.tier - kb.tier;
    if (ka.created !== kb.created) return ka.created - kb.created;
    return ka.name.localeCompare(kb.name);
  });
}

// sidebarHtml renders the rows grouped under their project headers, the
// sessions with no project at all last under 未分组, and a collapsed project as
// its header alone. An empty favorite group renders its header and no row:
// the top-right `+` button is the create affordance.
function sidebarHtml(items) {
  // Indexed by (node, name) to reach a project's favorite/github flags.
  const projIndex = {};
  sessionList.projectsData.forEach(p => {
    projIndex[(p.node || 'local') + ':' + p.name] = p;
  });
  const { groups, ungrouped } = groupSidebarItems(items);
  const groupKeys = sortedGroupKeys(groups, projIndex);
  if (groupKeys.length === 0) return items.map(sessionCardHtml).join('');
  let html = '';
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
    if (sessionList.collapsedProjects.has(k)) return;
    if (g.items.length > 0) {
      html += g.items.map(sessionCardHtml).join('');
    }
  });
  if (ungrouped.length > 0) {
    html += '<div class="section-header"><span class="sh-name">未分组</span></div>';
    html += ungrouped.map(sessionCardHtml).join('');
  }
  return html;
}

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
export function originBadgeHtml(key) {
  const info = originBadgeInfo(key);
  if (!info) return '';
  return '<span class="sc-origin kind-' + esc(info.kind) + '" title="' + escAttr(info.label) + '">' + esc(info.label) + '</span>';
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

function sessionCardHtml(/** @type {SessionSnapshot} */ s) {
  const sNode = s.node || 'local';
  const isActive = selection.key === s.key && selection.node === sNode;
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
  const unreadCount = perSession.unread[sid(s.key, sNode)] || 0;
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
    ? '<span class="sc-node" data-nz-bg="' + escAttr(nodeColor(sNode)) + '" title="' + escAttr(getNodeDisplayName(sNode)) + '">' + esc(getNodeDisplayName(sNode)) + '</span>'
    : '';

  const dismissBtn = '<button type="button" class="btn-close btn-dismiss" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" data-action="session-dismiss" title="移除" aria-label="移除会话">' + ICONS.close + '</button>';

  const typeTag = s.source === 'terminal' ? sessionTypeTag(s.type_label) : '';
  const agentCount = (isActive && turnState.agents.length) || (s.subagents ? s.subagents.length : 0);
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
    '<span>' + esc(displayState) + '</span>' + sessionExitChipHtml(s.state, s.death_reason) +
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
export function updateCardUnreadChip(card, count) {
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

export function updateStatusBar() {
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
  const currentIsLocal = !multi || selection.node === 'local';

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
    const nd = sessionList.nodesData[selection.node] || {};
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

/* ===== Node Selector ===== */
//
// The node selector replaces the per-card .sc-node badge + per-node rows in
// .sidebar-status when more than one node is connected. Clicking the trigger
// opens a dropdown of all nodes; clicking a node switches the sidebar filter.
// Single-node setups (local only, or one remote only) hide the whole thing —
// there is nothing to choose between.

// getNodeStatus returns a normalized status key (ok/connecting/offline/
// unreachable/error) for a node. 'local' tracks the WS state machine; remotes
// read from the server-side node health snapshot. Falls back to 'offline' when
// the server has no record — safer than pretending the node is reachable.
export function getNodeStatus(id) {
  if (!id || id === 'local') {
    if (wsm.state === WS_STATES.CONNECTED) return 'ok';
    if (wsm.state === WS_STATES.CONNECTING || wsm.state === WS_STATES.AUTH) return 'connecting';
    return 'offline';
  }
  const nd = sessionList.nodesData[id];
  if (!nd) return 'offline';
  return nd.status || 'offline';
}

/* ===== WebSocket state: status bar, fallback pollers, announcements ===== */

// wsStateChanged follows wsm (ws_manager.js): every transition repaints the
// status bar; CONNECTED stops the REST fallback pollers, DISCONNECTED arms them.
function wsStateChanged(s, prev) {
  updateStatusBar();
  if (s === WS_STATES.CONNECTED) {
    // RNEW-UX-010 — sighted users see the dot flip; AT users get the
    // transition announced politely. Only announce when it's a real
    // transition (prev !== CONNECTED) to avoid re-announcing on no-op
    // state refreshes.
    if (prev !== WS_STATES.CONNECTED) announce(wsm._everConnected ? '已重新连接' : '已连接');
    // WS connected: stop session polling, rely on push
    if (timers.sessionPoll) { clearInterval(timers.sessionPoll); timers.sessionPoll = null; }
    // Reduce discovered scan frequency
    if (timers.discoveredPoll) { clearInterval(timers.discoveredPoll); timers.discoveredPoll = null; }
    // #2431: a hidden tab has had its pollers suspended by stopPollers;
    // re-arming here would undo that. startPollers re-arms on return.
    if (!document.hidden) timers.discoveredPoll = setInterval(scanDiscovered, 30000);
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
    if (visible && !timers.sessionPoll) timers.sessionPoll = setInterval(fetchSessions, 5000);
    if (timers.discoveredPoll) { clearInterval(timers.discoveredPoll); timers.discoveredPoll = null; }
    if (visible) timers.discoveredPoll = setInterval(scanDiscovered, 5000);
    if (selection.key && !timers.events) {
      transcript.lastEventTime = sessionStream.lastEventTimeWs;
      if (visible) timers.events = setInterval(() => fetchEvents(false), 1000);
    }
  }
}
wsm.onStateChange(wsStateChanged);

// onSessionState applies a pushed process state to the session: the optimistic
// flip it settles, its bookkeeping, its card, the header and the subscription.
function onSessionState(msg) {
  const msgNode = msg.node || 'local';
  const sKey = sid(msg.key, msgNode);
  // Real state arrived — the optimistic flip has served its purpose, regardless
  // of whether the server says running/ready/dead. Clear the flag so future
  // turns don't short-circuit the running→ready rollback logic. Capture it
  // FIRST: the wasDead computation below needs to know whether prev.state
  // is a real server-reported 'running' or just the pre-send optimistic flip.
  const wasOptimisticRunning = !!perSession.optimisticRunning[sKey];
  const optimisticPrevState = perSession.optimisticPrevState[sKey];
  delete perSession.optimisticRunning[sKey];
  delete perSession.optimisticPrevState[sKey];
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
      sessionStream.subscribedKey === msg.key &&
      (sessionStream.subscribedNode || 'local') === msgNode) {
    sessionStream.subscribedKey = null;
    sessionStream.subscribedNode = null;
    sessionStream._subscriptionSuspended = false;
    sessionStream.lastEventTimeWs = 0;
  }
  const prev = sessionList.sessionsData[sKey] || {};
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
  settleTurnBoundary(msg, msgNode, sKey, prevState);
  if (sessionList.sessionsData[sKey]) {
    sessionList.sessionsData[sKey].state = msg.state;
    if (msg.reason) {
      sessionList.sessionsData[sKey].death_reason = msg.reason;
    } else if (msg.state === 'running') {
      // Process revived: clear stale death_reason
      delete sessionList.sessionsData[sKey].death_reason;
    }
  }
  paintSessionCardState(msg, msgNode, sKey);
  if (msg.key === selection.key && msgNode === selection.node) updateMainState(msg.state);
  resubscribeOnRunning(msg, msgNode, wasDead);
  // State changed: force next fetchSessions to re-render sidebar.
  // storeGen doesn't increment on process state transitions (only session
  // mutations), so the version cache would otherwise skip the re-render.
  sessionList.lastVersion = 0;
  if (msg.reason) debouncedFetchSessions();
}

// settleTurnBoundary is what a session_state owes the turn it ends: the unread
// count, the sent-text cache, the HTTP send mark and the git chip.
function settleTurnBoundary(msg, msgNode, sKey, prevState) {
  // Chat-style unread: a running→ready (or dead) transition means the model
  // just produced a reply. Bump the unread counter unless the operator is
  // already looking at that card — in which case they're reading it live.
  const turnCompleted = prevState === 'running' && (msg.state === 'ready' || msg.state === 'dead');
  const isActive = msg.key === selection.key && msgNode === selection.node;
  if (turnCompleted && !isActive) {
    perSession.unread[sKey] = (perSession.unread[sKey] || 0) + 1;
  }
  // Turn 自然跑完后清掉上一次发出的文本缓存，否则下一轮刚进 running
  // 就中断会把陈旧文本回填上来。中断路径不会走到这里被清掉，因为
  // interruptSession 会先消费 lastSent 再发中断。
  if (turnCompleted) delete perSession.lastSent[sKey];
  // The HTTP send reached a terminal state (or never became a turn): the
  // originator mark is no longer needed — drop it so it cannot linger and
  // let a much later send_error for someone else's send slip through.
  if (msg.state === 'ready' || msg.state === 'dead') perSession.httpSendPending.delete(sKey);
  // 一轮对话里 agent 很可能切了分支（git checkout / 新建 worktree 分支）。
  // 这不改 workspace 路径，所以 workspace-diff 那条失效路径不会触发，chip
  // 会一直停在选中会话那一刻的分支上。turn 边界是重新解析的自然时机：
  // 频率低（每轮一次而非定时轮询），且恰好覆盖"agent 干完活"这个分支最可能
  // 已变的时刻。invalidateGitState 内部只在该会话仍被选中时才真正发请求。
  if (turnCompleted) invalidateGitState(msg.key, msgNode);
}

// paintSessionCardState patches the session's sidebar card in place: badge,
// dot, state text, exit chip and unread chip.
function paintSessionCardState(msg, msgNode, sKey) {
  let card = null;
  document.querySelectorAll('.session-card').forEach(c => {
    if (c.dataset.key === msg.key && (c.dataset.node || 'local') === msgNode) card = c;
  });
  if (!card) return;
  // Surface dead sessions as "ready" in the UI — the backend state is
  // retained on sessionsData so the resubscribe logic below still fires
  // when a dead→running transition occurs.
  const displayState = msg.state === 'dead' ? 'ready' : msg.state;
  const badge = card.querySelector('.badge');
  if (badge) { badge.className = 'badge ' + displayState; badge.textContent = displayState; }
  // Sidebar cards carry .sc-dot (dot-running/dot-ready/dot-new) and the state
  // text, not .badge; patch both so the card follows the push immediately.
  const dot = card.querySelector('.sc-dot');
  if (dot) {
    dot.className = 'sc-dot ' + (displayState === 'running' ? 'dot-running' : (displayState === 'ready' ? 'dot-ready' : 'dot-new'));
  }
  const meta = card.querySelector('.sc-meta');
  if (meta) {
    const stateSpan = meta.querySelectorAll('span')[1]; // [0]=dot, [1]=state text
    if (stateSpan && !stateSpan.classList.contains('sc-node')) stateSpan.textContent = displayState;
  }
  patchCardExitChip(card, msg.state, msg.reason);
  // Sync the unread chip in place. fetchSessions re-renders from template
  // and reads sessionUnread directly; this path keeps the bubble fresh
  // between polls (WS state arrives faster than the sessions poll tick).
  updateCardUnreadChip(card, perSession.unread[sKey] || 0);
}

// resubscribeOnRunning re-subscribes the session on screen when it turns
// "running" and needs a live event stream. Covers: (1) not subscribed yet (new session, subscribedKey mismatch)
//         (2) subscribed but process was dead → revived
//         (3) subscribed without eventPushLoop (no-process subscribe → process available)
//            — detected by the "suspended" reason the server sends for no-process subscribes.
// Case 3 must NOT fire on normal ready→running transitions for already-subscribed
// sessions — that would cause full re-render and wipe the optimistic user message.
function resubscribeOnRunning(msg, msgNode, wasDead) {
  if (msg.key === selection.key && msgNode === selection.node && msg.state === 'running') {
    const needSub = (
      (sessionStream.subscribedKey !== msg.key && sessionStream._pendingSubscribeKey !== msg.key) || // case 1: not subscribed and no pending subscribe
      (wasDead && !msg.reason) ||                                   // case 2
      (sessionStream.subscribedKey === msg.key && sessionStream._subscriptionSuspended) // case 3
    );
    if (needSub) {
      sessionStream.lastEventTimeWs = 0;
      sessionStream.subscribe(msg.key, selection.node);
    }
  }
}

/* ===== Session frames: subscription acks, errors, state pushes, list updates ===== */

wsm.on(NZ_CONTRACT.WS.subscribed, (msg) => {
  // Server confirmed subscription — apply authoritative state
  sessionStream.subscribedKey = sessionStream._pendingSubscribeKey || msg.key;
  // 非 pending 时以帧自带的 node 为准，不退到 'local'：relay 重建远端订阅
  // (remoteDropped) 或 reconnect 后，远端 subscribed 经 relay 扇出给该 key
  // 下所有 tab（relay 每帧注入 node，reverseconn 也带 Node）。非 pending 的
  // tab 若被改写成 'local'，之后 subscription_timeout 处理要求 node 匹配就
  // 不再清簿记 → 不重订阅，原 bug 复现。
  sessionStream.subscribedNode = sessionStream._pendingSubscribeNode || msg.node || 'local';
  sessionStream._pendingSubscribeKey = null;
  sessionStream._pendingSubscribeNode = null;
  // Track whether the server started an eventPushLoop for this subscription.
  // "suspended" means the session had no process — no live events will arrive
  // until the process starts, at which point onSessionState triggers re-subscribe.
  sessionStream._subscriptionSuspended = (msg.reason === 'suspended');
  if (msg.state && msg.key === selection.key && sessionStream.subscribedNode === selection.node) {
    const subSKey = sid(msg.key, sessionStream.subscribedNode);
    if (sessionList.sessionsData[subSKey]) {
      sessionList.sessionsData[subSKey].state = msg.state;
      updateMainState(msg.state);
    }
  }
});
// Server ack for an explicit unsubscribe (wshub_subscribe.go, three
// emit sites incl. the relayed remote ack). sessionStream.unsubscribe() already
// cleared subscribedKey/Node synchronously and a relayed ack may name
// a key this tab no longer tracks — nothing to reconcile. Registered so
// the frame is a documented no-op rather than an unhandled type.
wsm.on(NZ_CONTRACT.WS.unsubscribed, () => {});
wsm.on(NZ_CONTRACT.WS.error, (msg) => {
  // PurgeNodeSubscriptions broadcast: error{node, "node disconnected"}
  // reaches every tab regardless of what it is subscribed to. Drop only
  // the bookkeeping that points at the dead node, snap selectedNode back
  // to local via the existing reconcile path, and re-fetch so the
  // sidebar reflects the node's sessions going away.
  if (!msg.key && msg.node && msg.error === 'node disconnected') {
    if (sessionStream.subscribedNode === msg.node) {
      sessionStream.subscribedKey = null;
      sessionStream.subscribedNode = null;
    }
    if (sessionStream._pendingSubscribeNode === msg.node) {
      sessionStream._pendingSubscribeKey = null;
      sessionStream._pendingSubscribeNode = null;
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
    if (selection.key && perSession.workspaces[selection.key] === undefined &&
        (sessionList.sessionsData[sid(selection.key, msg.node)] || perSession.nodes[selection.key] === msg.node)) {
      deselectNodeSession(msg.node);
    }
    sessionList.nodesData = Object.fromEntries(Object.entries(sessionList.nodesData).filter(([id]) => id !== msg.node));
    reconcileSelectedNode();
    sessionList.lastVersion = 0;
    debouncedFetchSessions();
    return;
  }
  // Subscribe failed (e.g. session not found yet) — reset pending, but
  // only when the frame is about THIS subscribe: a keyed error for a
  // different key (or an agent_subscribe validation error, which also
  // arrives as a bare `error`) must not wipe an unrelated in-flight
  // subscribe. Keyless frames without a node are the legacy shape of a
  // subscribe rejection and still clear pending.
  if (!msg.key || msg.key === sessionStream._pendingSubscribeKey) {
    sessionStream._pendingSubscribeKey = null;
    sessionStream._pendingSubscribeNode = null;
  }
});
wsm.on(NZ_CONTRACT.WS.session_state, (msg) => onSessionState(msg));
wsm.on(NZ_CONTRACT.WS.sessions_update, () => {
  // RNEW-UX-010 — snapshot pre-update session-key set so we can spot
  // a newly-added key after the fetch completes. Comparing sizes is
  // not enough (delete+create at the same tick would net to zero).
  const prevSessKeys = new Set(Object.keys(sessionList.sessionsData || {}));
  debouncedFetchSessions().then(() => {
    // Auto-subscribe to newly created session if we don't have an active
    // subscription. _pendingSubscribeKey is intentionally not checked:
    // a no-process subscribe returns "subscribed" + persisted history but
    // no live eventPushLoop, so subscribedKey may not be set while the
    // pending flag was already cleared. This ensures recovery.
    if (selection.key && !sessionStream.subscribedKey && sessionList.sessionsData[sid(selection.key, selection.node)]) {
      sessionStream.subscribe(selection.key, selection.node);
    }
    const added = Object.keys(sessionList.sessionsData || {}).filter(k => !prevSessKeys.has(k));
    if (added.length > 0) announce('新会话已创建');
  });
});

export function updateMainState(state) {
  const ia = document.getElementById('input-area');
  if (ia) ia.classList.toggle('disabled', false);
  updateSendButton(state);
  // The header's exit chip reads the session's own death_reason: the reason a
  // caller has in hand may be a subscription status ('suspended'), not a death.
  const exitEl = document.getElementById('header-exit');
  if (exitEl) {
    const sd = sessionList.sessionsData[sid(selection.key, selection.node)];
    const html = sessionExitChipHtml(state, sd ? sd.death_reason : '');
    if (exitEl.innerHTML !== html) exitEl.innerHTML = html;
  }
}
