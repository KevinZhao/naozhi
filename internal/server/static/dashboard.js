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
let pendingFiles = []; // {file, id, status: 'uploading'|'ready'|'error'}
let sending = false;
let selectedNode = 'local';
let nodesData = {};
let lastVersion = 0;
let lastNodesJSON = '';
let lastHistoryJSON = '';
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
let localWsInfo = { name: '', sys: '' };
const sessionWorkspaces = {};
const sessionNodes = {};
const sessionDrafts = {}; // key -> draft text, preserved across session switches
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
      return;
    }
    if (!r.ok) return;
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
          data.sessions.push({
            key: key,
            state: 'new',
            platform: parts[0] || 'dashboard',
            agent: 'general',
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
  } catch (e) {
    console.error('fetchSessions:', e);
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
  localWsInfo = { name: st.workspace_name || st.workspace_id || '', sys: '' };
  if (st.system) {
    const sys = st.system;
    let memStr = sys.memory_mb > 0 ? (sys.memory_mb >= 1024 ? (sys.memory_mb / 1024).toFixed(1) + 'G' : sys.memory_mb + 'M') : '';
    const ipStr = sys.ips && sys.ips.length > 0 ? sys.ips.join(', ') : '';
    localWsInfo.sys = sys.os + '/' + sys.arch + ' \u00b7 ' + sys.cpus + 'C' + (memStr ? '/' + memStr : '') + (ipStr ? ' \u00b7 ' + ipStr : '');
  }
  updateStatusBar();
  if (st.agents) availableAgents = st.agents;
  if (st.default_workspace) defaultWorkspace = st.default_workspace;
  if (st.projects) projectsData = st.projects;

  const list = document.getElementById('session-list');
  const scrollTop = list.scrollTop;

  // Merge discovered into sessions — tag them as source=terminal
  const allItems = (data.sessions || []).map(s => {
    if (!s.source) s.source = 'managed';
    return s;
  });
  discoveredItems.forEach(d => {
    allItems.push({
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

  // Workspace sidebar: managed + discovered sessions.
  allSessionsCache = allItems;

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

  // Always group by project when any session has a project
  const projects = new Set(allItems.map(s => s.project).filter(Boolean));

  let html;
  if (projects.size > 0) {
    // Group by project, sort groups by earliest first-seen in group (stable)
    const groups = {};
    const ungrouped = [];
    allItems.forEach(s => {
      const p = s.project || '';
      if (p) {
        if (!groups[p]) groups[p] = [];
        groups[p].push(s);
      } else {
        ungrouped.push(s);
      }
    });
    const sortedProjects = Object.keys(groups).sort((a, b) => {
      const aFirst = Math.max(...groups[a].map(s => sessionFirstSeen[(s.node || 'local') + ':' + s.key] || 0));
      const bFirst = Math.max(...groups[b].map(s => sessionFirstSeen[(s.node || 'local') + ':' + s.key] || 0));
      return bFirst - aFirst;
    });
    html = '';
    sortedProjects.forEach(p => {
      html += '<div class="section-header">' + esc(p) + '</div>';
      html += groups[p].map(sessionCardHtml).join('');
    });
    if (ungrouped.length > 0) {
      html += '<div class="section-header">Other</div>';
      html += ungrouped.map(sessionCardHtml).join('');
    }
  } else {
    html = allItems.map(sessionCardHtml).join('');
  }

  if (!html) html = '<div class="no-sessions">no sessions</div>';
  list.innerHTML = html;
  list.scrollTop = scrollTop;

  // Update history badge (filesystem history sessions, deduplicated against workspace)
  const hBadge = document.getElementById('history-badge');
  if (hBadge) {
    const workspaceIDs = new Set(allSessionsCache.filter(s => s.session_id).map(s => s.session_id));
    const historyCount = historySessionsData.filter(r => !workspaceIDs.has(r.session_id)).length;
    hBadge.textContent = historyCount > 0 ? historyCount : '';
    hBadge.style.display = historyCount > 0 ? '' : 'none';
  }
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

  let itemsHtml;
  if (merged.length === 0) {
    itemsHtml = '<div class="history-popover-empty">no history</div>';
  } else {
    // Group by day.
    let currentDay = '';
    itemsHtml = merged.map(s => {
      let dayHeader = '';
      if (s.last_active) {
        const d = new Date(s.last_active);
        const dayStr = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', weekday: 'short' });
        if (dayStr !== currentDay) {
          currentDay = dayStr;
          dayHeader = '<div class="hp-day-header">' + esc(dayStr) + '</div>';
        }
      }
      const ago = s.last_active ? timeAgo(s.last_active) : '';
      const onclick = 'resumeRecentSession(this.dataset.sid);closeHistoryPopover()';
      return dayHeader +
        '<div class="history-popover-item" data-sid="' + escAttr(s.session_id) + '" onclick="' + onclick + '">' +
        (s.prompt ? '<div class="hp-prompt" title="' + escAttr(s.prompt) + '">' + esc(s.prompt) + '</div>' : '<div class="hp-prompt" style="color:#6e7681">(no prompt)</div>') +
        '<div class="hp-meta">' +
          (s.project ? '<span class="hp-project">' + esc(s.project) + '</span><span class="hp-dot">&middot;</span>' : '') +
          (ago ? '<span>' + ago + '</span>' : '') +
        '</div>' +
        '</div>';
    }).join('');
  }

  const popover = document.createElement('div');
  popover.className = isMobile() ? 'history-sheet' : 'history-popover';
  popover.innerHTML = '<div class="history-popover-header">History (' + merged.length + ')</div>' + itemsHtml;
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

function cliIcon(name) {
  if (name === 'kiro') return '<svg class="sc-cli-icon" viewBox="0 0 16 16" fill="none"><path d="M8 1L14 5.5V10.5L8 15L2 10.5V5.5L8 1Z" fill="#f97316" opacity="0.85"/><path d="M6 5.5V10.5M6 8H9.5L6 5.5M6 8L9.5 10.5" stroke="#fff" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  // Default: official Claude logomark (from claude.ai/favicon.svg)
  return '<svg class="sc-cli-icon" viewBox="0 0 248 248" fill="none"><path d="M52.4285 162.873L98.7844 136.879L99.5485 134.602L98.7844 133.334H96.4921L88.7237 132.862L62.2346 132.153L39.3113 131.207L17.0249 130.026L11.4214 128.844L6.2 121.873L6.7094 118.447L11.4214 115.257L18.171 115.847L33.0711 116.911L55.485 118.447L71.6586 119.392L95.728 121.873H99.5485L100.058 120.337L98.7844 119.392L97.7656 118.447L74.5877 102.732L49.4995 86.1905L36.3823 76.62L29.3779 71.7757L25.8121 67.2858L24.2839 57.3608L30.6515 50.2716L39.3113 50.8623L41.4763 51.4531L50.2636 58.1879L68.9842 72.7209L93.4357 90.6804L97.0015 93.6343L98.4374 92.6652L98.6571 91.9801L97.0015 89.2625L83.757 65.2772L69.621 40.8192L63.2534 30.6579L61.5978 24.632C60.9565 22.1032 60.579 20.0111 60.579 17.4246L67.8381 7.49965L71.9133 6.19995L81.7193 7.49965L85.7946 11.0443L91.9074 24.9865L101.714 46.8451L116.996 76.62L121.453 85.4816L123.873 93.6343L124.764 96.1155H126.292V94.6976L127.566 77.9197L129.858 57.3608L132.15 30.8942L132.915 23.4505L136.608 14.4708L143.994 9.62643L149.725 12.344L154.437 19.0788L153.8 23.4505L150.998 41.6463L145.522 70.1215L141.957 89.2625H143.994L146.414 86.7813L156.093 74.0206L172.266 53.698L179.398 45.6635L187.803 36.802L193.152 32.5484H203.34L210.726 43.6549L207.415 55.1159L196.972 68.3492L188.312 79.5739L175.896 96.2095L168.191 109.585L168.882 110.689L170.738 110.53L198.755 104.504L213.91 101.787L231.994 98.7149L240.144 102.496L241.036 106.395L237.852 114.311L218.495 119.037L195.826 123.645L162.07 131.592L161.696 131.893L162.137 132.547L177.36 133.925L183.855 134.279H199.774L229.447 136.524L237.215 141.605L241.8 147.867L241.036 152.711L229.065 158.737L213.019 154.956L175.45 145.977L162.587 142.787H160.805V143.85L171.502 154.366L191.242 172.089L215.82 195.011L217.094 200.682L213.91 205.172L210.599 204.699L188.949 188.394L180.544 181.069L161.696 165.118H160.422V166.772L164.752 173.152L187.803 207.771L188.949 218.405L187.294 221.832L181.308 223.959L174.813 222.777L161.187 203.754L147.305 182.486L136.098 163.345L134.745 164.2L128.075 235.42L125.019 239.082L117.887 241.8L111.902 237.31L108.718 229.984L111.902 215.452L115.722 196.547L118.779 181.541L121.58 162.873L123.291 156.636L123.14 156.219L121.773 156.449L107.699 175.752L86.304 204.699L69.3663 222.777L65.291 224.431L58.2867 220.768L58.9235 214.27L62.8713 208.48L86.304 178.705L100.44 160.155L109.551 149.507L109.462 147.967L108.959 147.924L46.6977 188.512L35.6182 189.93L30.7788 185.44L31.4156 178.115L33.7079 175.752L52.4285 162.873Z" fill="#D97757"/></svg>';
}

function sessionCardHtml(s) {
  const sNode = s.node || 'local';
  const isActive = selectedKey === s.key && selectedNode === sNode;
  const isNew = s.state === 'new';
  const cls = 'session-card' + (isActive ? ' active' : '') + (isNew ? ' new-card' : '');

  // Line 1: prompt
  const prompt = s.summary || s.last_prompt || (isNew ? '(new session)' : '(no prompt)');
  const icon = cliIcon(s.cli_name || 'cli');

  // Line 2: status dot + meta
  const dotCls = s.state === 'running' ? 'dot-running' : (s.state === 'ready' ? 'dot-ready' : 'dot-new');
  const ago = s.last_active ? timeAgo(s.last_active) : '';
  const nodeBadge = isMultiNode() && sNode !== 'local'
    ? '<span class="sc-node" style="background:' + nodeColor(sNode) + '">' + esc(sNode) + '</span>' : '';

  const dismissBtn = '<button type="button" class="btn-dismiss" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" onclick="event.stopPropagation();dismissSession(this.dataset.key,this.dataset.node)" title="remove" aria-label="Remove session">&times;</button>';

  const typeTag = s.source === 'terminal' ? sessionTypeTag(s.cli_name, s.entrypoint) : '';
  const agentCount = s.subagents ? s.subagents.length : 0;
  const agentBadge = agentCount > 0 ? '<span class="sc-agents">\u{1F916}\u00D7' + agentCount + '</span>' : '';
  const metaHtml = '<span class="sc-dot ' + dotCls + '"></span>' +
    '<span>' + esc(s.state) + '</span>' +
    nodeBadge +
    typeTag +
    agentBadge;

  return '<div class="' + cls + '" role="listitem" data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" tabindex="0" aria-label="' + escAttr(prompt + ' · ' + s.state) + '" onclick="selectSession(this.dataset.key,this.dataset.node)" onkeydown="sessionCardKey(event)">' +
    dismissBtn +
    icon +
    '<div class="sc-body">' +
      '<div class="sc-header">' +
        '<div class="sc-prompt" title="' + escAttr(prompt) + '">' + esc(prompt) + '</div>' +
        (ago ? '<span class="sc-time">' + ago + '</span>' : '') +
      '</div>' +
      '<div class="sc-meta">' + metaHtml + '</div>' +
    '</div>' +
  '</div>';
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
    if (!r.ok) { showToast('resume failed'); return; }
    const data = await r.json();
    const key = data.key;
    if (!key) return;

    // Force sidebar refresh to pick up the dismissed entry
    lastVersion = 0;
    await fetchSessions();

    selectSession(key, 'local');
    previewRecentSession(key, sessionId);
  } catch (e) {
    showToast('resume error: ' + e.message);
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

function updateStatusBar() {
  const container = document.getElementById('sidebar-status');
  if (!container) return;
  const wsUp = wsm.state === WS_STATES.CONNECTED;

  // Local node row (always first)
  const localName = localWsInfo.name || 'workspace';
  // Distinguish short reconnect vs stable polling mode
  const statusKey = (wsm.state === WS_STATES.DISCONNECTED && wsm.backoff > 8000) ? 'disconnected' : (wsm.state === WS_STATES.DISCONNECTED ? 'disconnected_retry' : wsm.state);
  const localLabel = localName + ' \u00b7 ' + (STATUS_LABELS[statusKey] || wsm.state);
  const dotKey = statusKey === 'disconnected' ? 'connecting' : wsm.state; // HTTP fallback = yellow dot
  const localSys = localWsInfo.sys || '';

  let html = '<div class="status-row">' +
    '<span class="status-dot ' + (VALID_DOT_CLASSES[dotKey] || 'off') + '"></span>' +
    '<div class="status-info">' +
      '<div class="status-ws">' + esc(localLabel) + '</div>' +
      (localSys ? '<div class="status-sys">' + esc(localSys) + '</div>' : '') +
    '</div></div>';

  // Remote node rows (from last known nodesData)
  const nodeIds = Object.keys(nodesData).filter(id => id !== 'local').sort();
  for (const id of nodeIds) {
    const nd = nodesData[id];
    const name = (nd.display_name || id);
    // Remote status comes from the server's last node health snapshot (via
    // /api/sessions polling or WS push), so it stays meaningful even while
    // the local WS briefly reconnects. Only flip to "unreachable" when we
    // have no recent snapshot at all.
    const status = nd.status || (wsUp ? 'offline' : 'unreachable');
    const dotCls = VALID_DOT_CLASSES[status] || 'offline';
    const label = REMOTE_LABELS[status] || status;
    const addr = nd.remote_addr || '';

    html += '<div class="status-row">' +
      '<span class="status-dot ' + dotCls + '"></span>' +
      '<div class="status-info">' +
        '<div class="status-ws">' + esc(name) + ' \u00b7 ' + esc(label) + '</div>' +
        (addr ? '<div class="status-sys">' + esc(addr) + '</div>' : '') +
      '</div></div>';
  }

  container.innerHTML = html;
}

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
  lastEventTime = 0;
  lastRenderedEventTime = 0;
  mobileEnterChat();
  stopPreviewPolling();
  document.querySelectorAll('.session-card').forEach(el => {
    el.classList.toggle('active', el.dataset.key === key && (el.dataset.node || 'local') === node);
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

async function dismissSession(key, node) {
  node = node || 'local';
  delete sessionDrafts[key];

  // If it's a pending (never-sent) session, just remove from localStorage
  if (sessionWorkspaces[key] !== undefined) {
    removePendingSession(key);
    delete sessionsData[sid(key, node)];
    if (selectedKey === key) {
      selectedKey = null;
      document.getElementById('main').innerHTML = '<div class="empty-state">select a session</div>';
    }
    lastVersion = 0;
    debouncedFetchSessions();
    return;
  }

  // Discovered session — kill external process via /api/discovered/close
  if (key.startsWith('_discovered:')) {
    const pid = parseInt(key.split(':')[1]);
    const d = discoveredItems.find(x => x.pid === pid);
    if (!d) { showToast('discovered session not found'); return; }
    try {
      const headers = {'Content-Type': 'application/json'};
      const token = getToken();
      if (token) headers['Authorization'] = 'Bearer ' + token;
      const r = await fetch('/api/discovered/close', {
        method: 'POST', headers,
        body: JSON.stringify({pid: d.pid, session_id: d.session_id || '', cwd: d.cwd || '', proc_start_time: d.proc_start_time || 0, node: node || ''})
      });
      if (!r.ok) {
        const text = await r.text().catch(() => '' + r.status);
        showToast('close failed: ' + text);
        return;
      }
      discoveredItems = discoveredItems.filter(x => x.pid !== pid);
      if (pendingDiscovered && pendingDiscovered.pid === pid) {
        pendingDiscovered = null;
        stopPreviewPolling();
        document.getElementById('main').innerHTML = '<div class="empty-state">select a session</div>';
      }
      const card = document.querySelector('.session-card[data-key="' + key + '"]');
      if (card) card.remove();
      lastVersion = 0;
      debouncedFetchSessions();
    } catch (e) { showToast('close error: ' + e.message); }
    return;
  }

  try {
    const headers = {'Content-Type': 'application/json'};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch('/api/sessions', {method: 'DELETE', headers, body: JSON.stringify({key: key})});
    if (!r.ok && r.status !== 404) {
      const text = await r.text().catch(() => r.status);
      showToast('remove failed: ' + text);
      return;
    }
    delete sessionsData[sid(key, node)];
    if (selectedKey === key) {
      selectedKey = null;
      if (wsm.subscribedKey === key) wsm.unsubscribe();
      document.getElementById('main').innerHTML = '<div class="empty-state">select a session</div>';
    }
    lastVersion = 0;
    debouncedFetchSessions();
  } catch (e) { showToast('remove error: ' + e.message); }
}

function renderMainShell() {
  const main = document.getElementById('main');
  const s = sessionsData[sid(selectedKey, selectedNode)] || {};

  const keyParts = (selectedKey || '').split(':');
  const agentIsGeneric = !s.agent || s.agent === 'general';
  // Primary title: user's latest prompt. Fallback to agent name or key tail.
  const displayName = s.summary || s.last_prompt || (agentIsGeneric ? '' : s.agent) || keyParts[keyParts.length - 1] || selectedKey || '';

  // Detail line: left = CLI name + version, right = cost (hidden for kiro)
  const effCLIName = s.cli_name || defaultCLIName;
  const effCLIVersion = s.cli_version || defaultCLIVersion;
  const cliLabel = effCLIName ? esc(effCLIName) + (effCLIVersion ? ' v' + esc(effCLIVersion) : '') : '';
  const showCost = effCLIName !== 'kiro';
  const cost = s.total_cost || 0;
  const costText = '$' + (cost < 0.01 && cost > 0 ? cost.toFixed(4) : cost.toFixed(2));
  const costClass = 'detail-cost' + (cost >= 1 ? ' high-cost' : cost > 0 ? ' has-cost' : '');

  main.innerHTML =
    '<div class="main-header">' +
      '<button class="btn-mobile-back" onclick="mobileBack()" title="back">&#8592;</button>' +
      '<div class="main-header-content">' +
      '<h2>' + esc(displayName) + '</h2>' +
      '<div class="detail">' +
        '<span class="detail-left">' + cliLabel + '</span>' +
        (showCost ? '<span class="' + costClass + '" id="header-cost">' + costText + '</span>' : '') +
      '</div>' +
      '</div>' +
    '</div>' +
    '<div class="events" id="events-scroll" role="log" aria-live="polite" aria-relevant="additions">' + (s.state === 'running' ? '<div class="empty-state loading-indicator">loading events\u2026</div>' : '') + '</div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button onclick="navMsg(\'prev\')" id="nav-prev" title="previous user message (Alt+\u2191)">&#x25B2;</button>' +
      '<span class="nav-counter" id="nav-counter" onclick="navShowList()" title="click to list all"></span>' +
      '<button onclick="navMsg(\'next\')" id="nav-next" title="next user message (Alt+\u2193)">&#x25BC;</button>' +
    '</div>' +
    '<div class="running-banner" id="running-banner" style="display:none" role="status" aria-live="polite">' +
      '<div class="rb-tool-row">' +
        '<span class="running-status"><span class="running-dot" aria-hidden="true"></span><span id="tool-activity">Working...</span></span>' +
        '<span class="rb-elapsed" id="rb-elapsed"></span>' +
      '</div>' +
      '<div class="rb-thinking-summary" id="rb-thinking-summary" style="display:none"></div>' +
      '<div class="rb-agents" id="rb-agents"></div>' +
      '<div class="rb-stats" id="rb-stats" style="display:none"></div>' +
    '</div>' +
    '<div class="input-area' + (voiceInputMode ? ' voice-mode' : '') + '" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<button class="btn-icon" onclick="openFilePicker()" title="upload image">&#x1f4ce;</button>' +
        '<button class="btn-icon btn-mic" id="btn-mic" onclick="toggleInputMode()" title="' + (voiceInputMode ? '\u5207\u6362\u952e\u76d8' : '\u5207\u6362\u8bed\u97f3') + '">' + (voiceInputMode ? '&#x2328;' : '&#x1f3a4;') + '</button>' +
        '<div id="msg-input" contenteditable="true" role="textbox" data-placeholder="send a message..." onkeydown="handleKey(event)" oncompositionend="lastCompositionEnd=Date.now()"></div>' +
        '<button class="btn-hold-talk" id="btn-hold-talk">\u6309\u4f4f\u8bf4\u8bdd</button>' +
        '<button class="btn-icon btn-send" id="btn-send" onclick="sendMessage()" title="send">&#x27a4;</button>' +
        '<button class="btn-icon btn-stop" id="btn-stop" onclick="interruptSession()" title="stop">&#x25A0;</button>' +
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
  // Double-tap events feed → focus input (mobile)
  let lastTapMs = 0;
  document.getElementById('events-scroll').addEventListener('touchend', e => {
    if (!isMobile() || e.target.closest('a,button,code,pre')) return;
    const now = Date.now();
    if (now - lastTapMs < 300) { document.getElementById('msg-input')?.focus(); lastTapMs = 0; }
    else lastTapMs = now;
  }, {passive:true});
}

async function fetchEvents(full) {
  if (!selectedKey) return;
  try {
    let url = '/api/sessions/events?key=' + encodeURIComponent(selectedKey);
    if (selectedNode && selectedNode !== 'local') url += '&node=' + encodeURIComponent(selectedNode);
    if (!full && lastEventTime > 0) url += '&after=' + lastEventTime;

    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(url, { headers });
    if (!r.ok) return;
    const events = await r.json();
    if (!events || events.length === 0) return;

    if (full) {
      renderEvents(events);
    } else {
      appendEvents(events);
    }

    const last = events[events.length - 1];
    if (last && last.time > lastEventTime) lastEventTime = last.time;
  } catch (e) { console.error('fetch events:', e); }
}

function renderEvents(events) {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  const display = processEventsForDisplay(events);
  const html = display.map(eventHtml).filter(Boolean).join('');
  el.innerHTML = html || (events.length === 0 ? '<div class="empty-state">no events yet</div>' : '');
  el.scrollTop = el.scrollHeight;
  // Track the latest rendered event time for deduplication
  if (events.length > 0) {
    const last = events[events.length - 1];
    if (last.time) lastRenderedEventTime = last.time;
  }
  runMermaid();
  runKatex();
  navRebuild();
}

function appendEvents(events) {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  const empty = el.querySelector('.empty-state');
  if (empty) empty.remove();
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  events.forEach(e => {
    if (isInternalEvent(e)) return;
    // Deduplicate: skip events at or before the last rendered time
    if (e.time && e.time <= lastRenderedEventTime) return;
    const h = eventHtml(e); if (h) el.insertAdjacentHTML('beforeend', h);
    if (e.time && e.time > lastRenderedEventTime) lastRenderedEventTime = e.time;
  });
  if (wasBottom) el.scrollTop = el.scrollHeight;
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
const INTERNAL_EVENT_TYPES = new Set(['tool_use','result','agent','task_start','task_progress','task_done']);
function isInternalEvent(e) { return e && INTERNAL_EVENT_TYPES.has(e.type); }

function eventHtml(e) {
  if (isInternalEvent(e) || e.type === 'thinking') return '';
  // Filter out Claude Code system XML injected as user messages
  const raw = e.detail || e.summary || '';
  if (e.type === 'user' && /^<(task-notification|system-reminder|local-command|command-name|available-deferred-tools)[\s>]/.test(raw)) return '';
  const icons = {init:'\u2699',system:'\u2699',user:'\u{1f464}',text:'\u2726'};
  const icon = icons[e.type] || '';

  let content = '';
  if (e.type === 'system') {
    content = esc(e.summary || e.type);
  } else if (e.type === 'text' || e.type === 'user') {
    let raw = e.detail || e.summary || e.type;
    // Strip redundant "[+N image(s)]" suffix when thumbnails are present
    if (e.images && e.images.length > 0) raw = raw.replace(/ \[\+\d+ image\(s\)\]$/, '');
    content = renderMd(raw);
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

  // Copy button for long AI text messages (>200 chars raw) — inside content, at bottom
  const rawText = e.detail || e.summary || '';
  const rawLen = rawText.length;
  const copyBtn = (e.type === 'text' && rawLen > 200)
    ? '<button class="event-copy-btn" data-raw="' + escAttr(rawText) + '" onclick="copyEventContent(this)">copy</button>'
    : '';

  return '<div class="event ' + esc(e.type||'') + '">' +
    '<span class="event-icon">' + icon + '</span>' +
    '<div class="event-content">' + content + imgHtml + copyBtn + '</div></div>';
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
      showToast('press Esc again to interrupt', 'warning', 1000);
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
    if (input) input.dataset.placeholder = 'taking over session...';
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
        const errText = await r.text();
        showToast('takeover failed: ' + errText);
        if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
        sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      const data = await r.json();
      if (!data.key) {
        showToast('takeover failed: no session key returned');
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
        showToast('takeover timed out — session not ready');
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
      showToast('takeover error: ' + e.message);
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

  // Block send while any attachment is still uploading or errored —
  // we only reference file_ids on the server, so partial uploads would
  // silently drop images. User can retry or remove the bad one.
  if (pendingFiles.some(f => f.status === 'uploading')) {
    showToast('images still uploading...');
    return;
  }
  const failed = pendingFiles.filter(f => f.status === 'error');
  if (failed.length > 0) {
    showToast('upload failed: ' + (failed[0].error || 'unknown') + ' (remove or retry)');
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
    if (wsm.send(sendMsg)) {
      // Optimistic render: show user message immediately without waiting
      // for the CLI to echo it back as a "user" event.
      const el = document.getElementById('events-scroll');
      if (el && text) {
        const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
        const html = eventHtml({type: 'user', detail: text, time: Date.now()});
        if (html) {
          el.insertAdjacentHTML('beforeend', html);
          el.lastElementChild.classList.add('optimistic-msg');
          if (wasBottom) el.scrollTop = el.scrollHeight;
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

    const r = await fetch('/api/sessions/send', {method:'POST', headers, body: JSON.stringify(payload)});

    if (r.status === 401 || r.status === 403) {
      if (input) setMsgValue(input, text);
      showAuthModal();
      return;
    }
    if (r.status === 429) {
      if (input) setMsgValue(input, text);
      showToast('message queue full, please wait');
      return;
    }
    if (!r.ok) {
      if (input) setMsgValue(input, text);
      // Some error paths still write text/plain; fall back to text() so we
      // always surface the real message instead of a generic "send failed".
      const raw = await r.text().catch(() => '');
      let msg = 'send failed: ' + r.status;
      try { const j = JSON.parse(raw); if (j && j.error) msg = j.error; } catch (_) { if (raw) msg = raw; }
      showToast(msg);
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
    showToast('send error: ' + e.message);
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

const toolVerbs = {
  Read: 'Reading', Edit: 'Editing', Write: 'Writing', Bash: 'Running',
  Grep: 'Searching', Glob: 'Finding files', Agent: 'Agent',
  Notebook: 'Editing notebook', WebFetch: 'Fetching'
};

function toolVerb(tool, summary) {
  const verb = toolVerbs[tool] || ('Using ' + tool);
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
      actEl.textContent = 'Thinking...';
    } else if (turnState.isWriting) {
      actEl.textContent = 'Writing...';
    } else {
      actEl.textContent = 'Working...';
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
  if (wsm.isConnected()) {
    wsm.send({ type: 'interrupt', key: selectedKey, id: 'int' + Date.now() });
    showToast('interrupt sent', 'warning');
  } else {
    // HTTP fallback when WebSocket is disconnected
    const headers = {'Content-Type': 'application/json'};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    fetch('/api/sessions/interrupt', {
      method: 'POST',
      headers,
      body: JSON.stringify({ key: selectedKey })
    }).then(r => r.json()).then(d => {
      showToast(d.status === 'ok' ? 'interrupt sent' : 'session not running', 'warning');
    }).catch(() => showToast('interrupt failed', 'error'));
  }
}

function scrollEventsToBottom() {
  const el = document.getElementById('events-scroll');
  if (el) el.scrollTop = el.scrollHeight;
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

function navMsg(dir) {
  if (navUserEls.length === 0) return;
  if (dir === 'prev') {
    if (navIdx <= 0) navIdx = navUserEls.length - 1;
    else navIdx--;
  } else {
    if (navIdx < 0 || navIdx >= navUserEls.length - 1) navIdx = 0;
    else navIdx++;
  }
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
    el.addEventListener('scroll', () => {
      const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 50;
      if (atBottom && navIdx >= 0) {
        navIdx = -1;
        navUpdatePill();
      }
      navDismissPopover();
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

// Get sessions in the same project group as the current selection (sidebar order)
function currentProjectSessions() {
  if (!allSessionsCache || allSessionsCache.length === 0) return [];
  const cur = allSessionsCache.find(s => s.key === selectedKey && (s.node || 'local') === selectedNode);
  const proj = cur ? (cur.project || '') : '';
  return allSessionsCache.filter(s => (s.project || '') === proj);
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
    if (loadingEl) loadingEl.innerHTML = 'no events yet';
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

// Downscale any image to JPEG with max edge 2048 and quality 0.85.
// Rationale: the CLI writes user messages as one NDJSON line to the shim,
// which is capped at 16 MB per line; two 10 MB photos base64-encoded alone
// blow past that and silently break the pipe. 2048 is also the knee where
// Anthropic's vision models stop benefiting from extra resolution, so we
// lose nothing by shrinking. HEIC is also handled here — createImageBitmap
// decodes it on Safari 17+ and we re-encode to JPEG.
// Falls back to the original file if decoding fails so the server's
// content-type check still produces a real error message.
async function normalizeImage(file) {
  const MAX_EDGE = 2048;
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
    const blob = await new Promise(res => canvas.toBlob(res, 'image/jpeg', 0.85));
    if (!blob) return file;
    return new File([blob], (file.name || 'image').replace(/\.[^.]+$/, '') + '.jpg', { type: 'image/jpeg' });
  } catch (_) {
    return file;
  }
}

function handleFiles(fileList) {
  const toUpload = [];
  // Relax the source-file ceiling to 40 MB: iPhone HEIC/JPEG straight from
  // Photos is often ~6–12 MB, and browsers deliver HEIC as-is. We downscale
  // before upload, so the 10 MB server ceiling applies to the re-encoded JPEG.
  for (const raw of fileList) {
    if (!raw.type.startsWith('image/')) continue;
    if (raw.size > 40 * 1024 * 1024) { showToast('file too large (max 40MB)'); continue; }
    if (pendingFiles.length >= 10) { showToast('max 10 files'); break; }
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
  toUpload.forEach(uploadEntry);
}

async function uploadEntry(entry) {
  entry.status = 'uploading';
  entry.error = '';
  renderFilePreviews();
  try {
    const file = await normalizeImage(entry.file);
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
  if (entry && entry.status === 'error') uploadEntry(entry);
}

function removeFile(idx) {
  const [removed] = pendingFiles.splice(idx, 1);
  if (removed && removed.blobUrl) URL.revokeObjectURL(removed.blobUrl);
  renderFilePreviews();
}

function renderFilePreviews() {
  const el = document.getElementById('file-preview');
  if (!el) return;
  el.innerHTML = pendingFiles.map((entry, i) => {
    const overlay =
      entry.status === 'uploading' ? '<div class="upload-status uploading"></div>' :
      entry.status === 'error' ? '<div class="upload-status error" title="' + escAttr(entry.error || 'upload failed') + '" onclick="retryUpload(' + i + ')">\u21bb</div>' :
      '';
    return '<div class="file-thumb ' + entry.status + '">' +
      '<img src="' + entry.blobUrl + '">' +
      overlay +
      '<button class="remove" onclick="removeFile(' + i + ')">\u00d7</button>' +
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
    showToast(err.message === 'not supported' ? '\u6d4f\u89c8\u5668\u4e0d\u652f\u6301\u5f55\u97f3' : '\u9ea6\u514b\u98ce\u6743\u9650\u88ab\u62d2\u7edd');
    console.warn('mic error:', err);
  });
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
  fetch('/api/transcribe', {
    method: 'POST',
    headers: headers,
    credentials: 'same-origin',
    body: fd
  }).then(r => {
    if (!r.ok) return r.text().then(t => { throw new Error('HTTP ' + r.status + ': ' + t); });
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
      showToast('\u672a\u68c0\u6d4b\u5230\u8bed\u97f3', '', 5000);
    }
  }).catch(err => {
    hideVoiceOverlay();
    showToast('\u8f6c\u5199\u5931\u8d25: ' + err.message, '', 5000);
  });
}

// --- Auth modal ---

function showAuthModal() {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="Dashboard API token">' +
      '<h3>Dashboard API Token</h3>' +
      '<input id="token-input" type="password" placeholder="enter dashboard token..." onkeydown="if(event.key===\'Enter\'){saveToken()}">' +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">cancel</button>' +
        '<button type="button" class="primary" onclick="saveToken()">save</button>' +
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
    } else {
      document.getElementById('token-input').value = '';
      document.getElementById('token-input').placeholder = 'invalid token — try again';
    }
  } catch(e) {
    showToast('network error', 'error');
  }
}

function createNewSession() {
  const ws = defaultWorkspace || '';

  if (!projectsData.length) {
    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    overlay.innerHTML =
      '<div class="modal" role="dialog" aria-modal="true" aria-label="New session">' +
        '<h3>New Session</h3>' +
        '<div style="margin-bottom:12px">' +
          '<label style="font-size:12px;color:#8b949e;display:block;margin-bottom:4px" for="new-workspace">Workspace</label>' +
          '<input id="new-workspace" placeholder="' + escAttr(ws) + '" value="' + escAttr(ws) + '" onkeydown="if(event.key===\'Enter\'){doCreateSession()}">' +
        '</div>' +
        '<div class="modal-btns">' +
          '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">cancel</button>' +
          '<button type="button" class="primary" onclick="doCreateSession()">create</button>' +
        '</div>' +
      '</div>';
    document.body.appendChild(overlay);
    trapFocus(overlay);
    setTimeout(() => document.getElementById('new-workspace').focus(), 100);
    return;
  }

  openProjectPalette();
}

function openProjectPalette() {
  const overlay = document.createElement('div');
  overlay.className = 'cmd-palette-overlay';
  overlay.innerHTML =
    '<div class="cmd-palette" role="dialog" aria-label="New session">' +
      '<div class="cmd-palette-header">' +
        '<input id="cp-input" type="text" autocomplete="off" spellcheck="false" placeholder="Search projects or type a path…">' +
      '</div>' +
      '<div id="cp-list" class="cmd-palette-list" role="listbox"></div>' +
      '<div class="cmd-palette-footer">' +
        '<span><kbd>↑</kbd><kbd>↓</kbd> navigate</span>' +
        '<span><kbd>Enter</kbd> open</span>' +
        '<span><kbd>Esc</kbd> close</span>' +
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
  if (q) scored.sort((a, b) => b.score - a.score);

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
  el.className = 'cmd-palette-item';
  el.dataset.idx = String(idx);
  const nodeId = p.node || 'local';
  const nodeBadge = nodeId !== 'local'
    ? '<span class="cp-node" style="background:' + nodeColor(nodeId) + '">' + esc(nodeId) + '</span>'
    : '';
  el.innerHTML =
    '<span class="cp-icon">▸</span>' +
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
    ? 'Open custom workspace: <span style="color:#79c0ff">' + esc(query) + '</span>'
    : 'Open custom workspace…';
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
  doCreateInProject(p.path, p.name, p.node || 'local');
}

function pickPaletteCustom(initialValue) {
  const overlay = document.querySelector('.cmd-palette-overlay');
  if (overlay) overlay.remove();
  const ws = defaultWorkspace || '';
  const prefill = initialValue && (initialValue.startsWith('/') || initialValue.startsWith('~')) ? initialValue : '';
  const modal = document.createElement('div');
  modal.className = 'modal-overlay';
  modal.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="Custom workspace">' +
      '<h3>Custom Workspace</h3>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:#8b949e;display:block;margin-bottom:4px" for="new-workspace">Workspace path</label>' +
        '<input id="new-workspace" placeholder="' + escAttr(ws) + '" value="' + escAttr(prefill) + '" onkeydown="if(event.key===\'Enter\'){doCreateSession()}">' +
      '</div>' +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">cancel</button>' +
        '<button type="button" class="primary" onclick="doCreateSession()">create</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(modal);
  trapFocus(modal);
  setTimeout(() => {
    const el = document.getElementById('new-workspace');
    if (el) { el.focus(); el.select(); }
  }, 50);
}

function doCreateInProject(projectPath, projectName, nodeId) {
  const overlay = document.querySelector('.modal-overlay, .cmd-palette-overlay');
  if (overlay) overlay.remove();
  sessionCounter++;
  const now = new Date();
  const ts = now.toISOString().slice(0,10) + '-' +
    now.toTimeString().slice(0,8).replace(/:/g, '') + '-' + sessionCounter;
  const key = 'dashboard:direct:' + ts + ':' + projectName;

  sessionWorkspaces[key] = projectPath;
  if (nodeId && nodeId !== 'local') sessionNodes[key] = nodeId;

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = nodeId || 'local';
  lastEventTime = 0;
  mobileEnterChat();
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
  renderMainShell();
  navRebuild();
  lastVersion = 0;
  debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}

function doCreateSession() {
  const workspace = document.getElementById('new-workspace').value.trim();
  const folderName = workspace ? (workspace.replace(/\/+$/, '').split('/').pop() || 'session') : 'session';
  document.querySelector('.modal-overlay').remove();

  sessionCounter++;
  const now = new Date();
  const ts = now.toISOString().slice(0,10) + '-' +
    now.toTimeString().slice(0,8).replace(/:/g, '') + '-' + sessionCounter;
  const key = 'dashboard:direct:' + ts + ':' + folderName;

  if (workspace) sessionWorkspaces[key] = workspace;

  stopPreviewPolling();
  wsm.unsubscribe();
  selectedKey = key;
  selectedNode = 'local';
  lastEventTime = 0;
  mobileEnterChat();
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
  renderMainShell();
  navRebuild();
  lastVersion = 0;
  debouncedFetchSessions();
  setTimeout(() => { const input = document.getElementById('msg-input'); if (input) input.focus(); }, 100);
}


// --- Utilities ---

function showToast(msg, type, duration) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast show' + (type ? ' ' + type : '');
  clearTimeout(el._tid);
  el._tid = setTimeout(() => { el.className = 'toast'; }, duration || 3000);
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
    navigator.clipboard.writeText(text).then(() => showToast('copied', 'success')).catch(() => { fallbackCopy(text); showToast('copied', 'success'); });
  } else {
    fallbackCopy(text);
    showToast('copied', 'success');
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
  const code = btn.closest('.md-code-wrap').querySelector('code').textContent;
  copyWithFeedback(btn, code);
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
      return '<div class="md-code-wrap"><pre class="md-pre"><code' + langAttr + '>' + esc(code) + '</code></pre><button class="md-copy-btn" onclick="copyCodeBlock(this)">copy</button></div>';
    }
    if (part.startsWith('$$') && part.endsWith('$$')) {
      return '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>';
    }
    if (part.startsWith('\\[') && part.endsWith('\\]')) {
      return '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>';
    }
    // Process line by line for block elements
    const lines = part.split('\n');
    let html = '';
    let inList = '';
    for (let i = 0; i < lines.length; i++) {
      let line = lines[i];
      // Headings
      const hm = line.match(/^(#{1,4})\s+(.+)$/);
      if (hm) {
        if (inList) { html += '</' + inList + '>'; inList = ''; }
        const level = hm[1].length;
        html += '<strong class="md-h' + level + '">' + inlineMd(hm[2]) + '</strong>\n';
        continue;
      }
      // Unordered list
      if (/^\s*[-*]\s+/.test(line)) {
        if (inList === 'ol') { html += '</ol>'; inList = ''; }
        if (!inList) { html += '<ul class="md-ul">'; inList = 'ul'; }
        html += '<li>' + inlineMd(line.replace(/^\s*[-*]\s+/, '')) + '</li>';
        continue;
      }
      // Ordered list
      if (/^\s*\d+\.\s+/.test(line)) {
        if (inList === 'ul') { html += '</ul>'; inList = ''; }
        if (!inList) { html += '<ol class="md-ol">'; inList = 'ol'; }
        html += '<li>' + inlineMd(line.replace(/^\s*\d+\.\s+/, '')) + '</li>';
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
          html += '</' + inList + '>'; inList = '';
        }
        html += '<div class="md-blank"></div>';
        continue;
      }
      if (inList) { html += '</' + inList + '>'; inList = ''; }
      if (/^\|.+\|$/.test(line.trim())) {
        let tbl = [line];
        while (i + 1 < lines.length && /^\|.+\|$/.test(lines[i + 1].trim())) { tbl.push(lines[++i]); }
        html += renderTable(tbl);
        continue;
      }
      html += inlineMd(line) + '<br>';
    }
    if (inList) html += '</' + inList + '>';
    return html;
  }).join('');
}

/* Inline markdown: bold, italic, code, links, math */
function inlineMd(s) {
  // Extract inline math before HTML escaping. Use \x00 delimiters to avoid collisions with user content.
  const mathTokens = [];
  s = s.replace(/\$([^\$\n]+?)\$/g, function(_, tex) {
    const idx = mathTokens.length;
    mathTokens.push(renderKatex(tex, false));
    return '\x00KTX' + idx + '\x00';
  });
  s = s.replace(/\\\((.+?)\\\)/g, function(_, tex) {
    const idx = mathTokens.length;
    mathTokens.push(renderKatex(tex, false));
    return '\x00KTX' + idx + '\x00';
  });
  s = esc(s);
  s = s.replace(/`([^`]+)`/g, '<code class="md-code">$1</code>');
  s = s.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/\*(.+?)\*/g, '<em>$1</em>');
  s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, function(_, text, url) {
    const safe = safeUrl(url);
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
  return s;
}

function renderTable(lines) {
  if (lines.length < 2) return lines.map(l => inlineMd(l) + '\n').join('');
  if (!/^\|[\s\-:]+(\|[\s\-:]+)+\|$/.test(lines[1].trim())) return lines.map(l => inlineMd(l) + '\n').join('');
  const cells = l => l.trim().replace(/^\||\|$/g, '').split('|').map(c => c.trim());
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

  connect() {
    if (this.conn && (this.conn.readyState === WebSocket.OPEN || this.conn.readyState === WebSocket.CONNECTING)) return;

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
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, this.backoff);
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
        showToast('WS auth failed: ' + (msg.error || 'invalid token'));
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
    if (this.lastEventTimeWs > 0) msg.after = this.lastEventTimeWs;
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
      const html = display.map(eventHtml).filter(Boolean).join('');
      // Only show "no events yet" when the server returned zero events and the session
      // is idle. For running sessions, show "loading events..." since eventPushLoop will
      // deliver events shortly (fixes blank-then-"no events yet" flash on click).
      if (html) {
        el.innerHTML = html;
      } else if (events.length === 0) {
        const sd = sessionsData[sid(selectedKey, selectedNode)];
        el.innerHTML = (sd && sd.state === 'running')
          ? '<div class="empty-state loading-indicator">loading events\u2026</div>'
          : '<div class="empty-state">no events yet</div>';
      } else {
        el.innerHTML = '';
      }
      el.scrollTop = el.scrollHeight;
      // Reset dedup tracker on full render
      if (events.length > 0) { const last = events[events.length - 1]; if (last.time) lastRenderedEventTime = last.time; }
      runMermaid();
  runKatex();
      navRebuild();
    } else {
      const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
      // Remove stale "no events yet" before processing incremental events
      const emptyEl = el.querySelector('.empty-state');
      if (emptyEl) emptyEl.remove();
      display.forEach(e => {
        // Deduplicate: skip events at or before the last rendered time
        if (e.time && e.time <= lastRenderedEventTime) return;
        // When the real "user" event arrives, remove the optimistic version
        if (e.type === 'user') {
          const opt = el.querySelector('.optimistic-msg');
          if (opt) opt.remove();
        }
        const h = eventHtml(e);
        if (h) {
          el.insertAdjacentHTML('beforeend', h);
        }
        if (e.time && e.time > lastRenderedEventTime) lastRenderedEventTime = e.time;
      });
      if (wasBottom) el.scrollTop = el.scrollHeight;
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
    if (ev.type === 'user') {
      const opt = el.querySelector('.optimistic-msg');
      if (opt) opt.remove();
    }
    const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
    el.insertAdjacentHTML('beforeend', html);
    if (wasBottom) el.scrollTop = el.scrollHeight;
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
        showToast('消息已排队，待当前回复完成后处理');
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
    } else if (msg.status === 'error') {
      showToast('send error: ' + (msg.error || 'unknown'), 'error');
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
      const badge = card.querySelector('.badge');
      if (badge) { badge.className = 'badge ' + msg.state; badge.textContent = msg.state; }
      card.classList.toggle('dead-card', msg.state === 'ready' && !!sessionsData[sKey]?.death_reason);
      // Update sidebar dot and state text to reflect new state immediately.
      // sessionCardHtml renders .sc-dot with dot-running/dot-ready/dot-new,
      // but onSessionState previously only patched .badge (which doesn't exist
      // in sidebar cards), leaving the dot stale.
      const dot = card.querySelector('.sc-dot');
      if (dot) {
        dot.className = 'sc-dot ' + (msg.state === 'running' ? 'dot-running' : (msg.state === 'ready' ? 'dot-ready' : 'dot-new'));
      }
      const meta = card.querySelector('.sc-meta');
      if (meta) {
        const stateSpan = meta.querySelectorAll('span')[1]; // [0]=dot, [1]=state text
        if (stateSpan && !stateSpan.classList.contains('sc-node')) stateSpan.textContent = msg.state;
      }
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
    this.state = s;
    updateStatusBar();
    if (s === WS_STATES.CONNECTED) {
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
  // Deselect any managed session
  selectedKey = null;
  selectedNode = null;
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
      '<button class="btn-mobile-back" onclick="mobileBack()" title="back">&#8592;</button>' +
      '<div class="main-header-content">' +
        '<h2>' + esc(base) + '</h2>' +
        '<div class="detail">' +
          sessionTypeTag(cliName || 'cli', entrypoint || '') +
        '</div>' +
      '</div>' +
    '</div>' +
    '<div class="events" id="events-scroll"><div class="empty-state">loading...</div></div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button onclick="navMsg(\'prev\')" id="nav-prev" title="previous user message (Alt+\u2191)">&#x25B2;</button>' +
      '<span class="nav-counter" id="nav-counter" onclick="navShowList()" title="click to list all"></span>' +
      '<button onclick="navMsg(\'next\')" id="nav-next" title="next user message (Alt+\u2193)">&#x25BC;</button>' +
    '</div>' +
    '<div class="input-area" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<div id="msg-input" contenteditable="true" role="textbox" data-placeholder="send a message to take over..." onkeydown="handleKey(event)" oncompositionend="lastCompositionEnd=Date.now()"></div>' +
        '<button class="btn-icon btn-send" id="btn-send" onclick="sendMessage()" title="send">&#x27a4;</button>' +
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
      if (el0) el0.innerHTML = '<div class="empty-state">' + esc(errText || 'preview failed') + '</div>';
      showToast(errText || 'preview failed');
      return;
    }
    const events = await r.json();
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const display = processEventsForDisplay(events);
    if (events.length === 0) {
      el.innerHTML = '<div class="empty-state">no conversation history</div>';
    } else {
      el.innerHTML = display.map(eventHtml).filter(Boolean).join('');
      el.scrollTop = el.scrollHeight;
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
        fresh.forEach(e => {
          if (isInternalEvent(e)) return;
          const h = eventHtml(e); if (h) el2.insertAdjacentHTML('beforeend', h);
        });
        if (wasBottom) el2.scrollTop = el2.scrollHeight;
        navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
        navUpdatePill();
      } catch (_) {}
    }, 2000);
  } catch (e) {
    showToast('preview error: ' + e.message);
  }
}

function handleTakeoverClick(el) {
  takeover(el, Number(el.dataset.pid), el.dataset.sessionId, el.dataset.cwd, Number(el.dataset.pst), el.dataset.node || '');
}

async function takeover(btn, pid, sessionId, cwd, procStartTime, node) {
  btn.classList.add('taking');
  btn.textContent = 'taking over...';
  try {
    const headers = {'Content-Type': 'application/json'};
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch('/api/discovered/takeover', {
      method: 'POST', headers,
      body: JSON.stringify({pid: pid, session_id: sessionId, cwd: cwd, proc_start_time: procStartTime || 0, node: node || ''})
    });
    if (!r.ok) {
      const text = await r.text();
      showToast('takeover failed: ' + text);
      btn.classList.remove('taking');
      btn.textContent = 'takeover';
      return;
    }
    const data = await r.json();
    showToast('session taken over', 'success');
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
    showToast('takeover error: ' + e.message);
    btn.classList.remove('taking');
    btn.textContent = 'takeover';
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

function initSwipeDelete() {
  const list = document.getElementById('session-list');
  if (!list) return;
  let card = null, startX = 0, startY = 0, tracking = false;
  list.addEventListener('touchstart', e => {
    if (e.touches.length !== 1) { card = null; return; }
    const c = e.target.closest('.session-card[data-key]');
    if (!c) return;
    card = c; startX = e.touches[0].clientX; startY = e.touches[0].clientY; tracking = false;
  }, {passive:true});
  list.addEventListener('touchmove', e => {
    if (!card) return;
    const dx = e.touches[0].clientX - startX;
    const dy = e.touches[0].clientY - startY;
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
    if (!card || !tracking) { card = null; tracking = false; return; }
    const dx = e.changedTouches[0].clientX - startX;
    const c = card; card = null; tracking = false;
    c.classList.remove('swiping');
    if (dx < -c.offsetWidth * 0.4) {
      c.style.transition = 'transform .2s ease, opacity .2s ease';
      c.style.transform = 'translateX(-100%)';
      c.style.opacity = '0';
      setTimeout(() => dismissSession(c.dataset.key, c.dataset.node || 'local'), 180);
    } else {
      c.style.transition = 'transform .2s ease, background .2s ease';
      c.style.transform = '';
      c.style.background = '';
      setTimeout(() => { c.style.transition = ''; }, 200);
    }
  }, {passive:true});
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

function createNewCronJob() {
  const presets = [
    { label: 'Every 30 min', value: '@every 30m' },
    { label: 'Every hour', value: '@every 1h' },
    { label: 'Daily 9:00', value: '0 9 * * *' },
    { label: 'Weekdays 9:00', value: '0 9 * * 1-5' },
    { label: 'Every Monday 9:00', value: '0 9 * * 1' },
  ];
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  let scheduleHtml =
    '<ul class="proj-pick" id="cron-schedule-list" role="listbox" aria-label="Schedule presets">' +
    presets.map(p =>
      '<li role="option" data-value="' + escAttr(p.value) + '" onclick="cronSelectSchedule(this, \'' + escJs(p.value) + '\')">' +
        '<div class="pp-name">' + esc(p.label) + '</div>' +
        '<div class="pp-path">' + esc(p.value) + '</div>' +
      '</li>'
    ).join('') +
    '<li id="cron-custom-toggle" role="option" onclick="toggleCronCustom()">' +
      '<div class="pp-custom"><span class="pp-custom-icon">&#9881;</span> Custom expression</div>' +
    '</li>' +
    '</ul>' +
    '<div id="cron-custom-form" style="display:none;margin-top:8px">' +
      '<input id="cron-schedule" placeholder="@every 30m or 0 9 * * 1-5" aria-label="Custom cron expression">' +
      '<div id="cron-preview-hint" class="cron-preview-hint"></div>' +
    '</div>';

  let wsHtml = '<div style="margin-top:12px"><div class="modal-section-label">Workspace (optional)</div>';
  if (projectsData.length > 0) {
    wsHtml += '<ul class="proj-pick" id="cron-ws-list" role="listbox" aria-label="Workspace">' +
      projectsData.map(p =>
        '<li role="option" data-path="' + escAttr(p.path) + '" onclick="cronSelectWorkspace(this, \'' + escJs(p.path) + '\')">' +
          '<div class="pp-name">' + esc(p.name) + '</div>' +
          '<div class="pp-path">' + esc(shortPath(p.path)) + '</div>' +
        '</li>'
      ).join('') +
      '<li id="cron-ws-custom-toggle" role="option" onclick="toggleCronWsCustom()">' +
        '<div class="pp-custom"><span class="pp-custom-icon">+</span> Custom path</div>' +
      '</li>' +
      '</ul>';
  }
  wsHtml += '<div id="cron-ws-custom-form" style="' + (projectsData.length > 0 ? 'display:none;' : '') + 'margin-top:4px">' +
    '<input id="cron-workdir" placeholder="' + escAttr(defaultWorkspace || '/home/user/project') + '" aria-label="Workspace path">' +
    '</div></div>';

  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="New cron job">' +
      '<h3>New Cron Job</h3>' +
      '<div class="modal-body">' +
        '<div style="margin-bottom:12px">' +
          '<div class="modal-section-label">Prompt</div>' +
          '<textarea id="cron-prompt" placeholder="what should this job do?" style="min-height:72px;max-height:160px" aria-label="Prompt"></textarea>' +
        '</div>' +
        '<div class="modal-section-label">Schedule</div>' +
        scheduleHtml + wsHtml +
      '</div>' +
      '<div class="modal-btns">' +
        '<button type="button" onclick="this.closest(\'.modal-overlay\').remove()">cancel</button>' +
        '<button type="button" class="primary" onclick="doCreateCronJob()">create</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  overlay.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') overlay.remove();
  });
  overlay._cronSchedule = '';
  overlay._cronWorkDir = '';
}

function cronSelectSchedule(el, value) {
  const overlay = el.closest('.modal-overlay');
  overlay._cronSchedule = value;
  document.querySelectorAll('#cron-schedule-list li').forEach(li => {
    li.classList.remove('selected');
    li.setAttribute('aria-selected', 'false');
  });
  el.classList.add('selected');
  el.setAttribute('aria-selected', 'true');
  // Hide custom form and clear its state when preset selected
  const customForm = document.getElementById('cron-custom-form');
  if (customForm) customForm.style.display = 'none';
  const customInput = document.getElementById('cron-schedule');
  if (customInput) customInput.value = '';
  const hint = document.getElementById('cron-preview-hint');
  if (hint) { hint.textContent = ''; hint.className = 'cron-preview-hint'; }
  const toggle = document.getElementById('cron-custom-toggle');
  if (toggle) toggle.style.display = '';
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
  if (customForm) customForm.style.display = 'none';
  const toggle = document.getElementById('cron-ws-custom-toggle');
  if (toggle) toggle.style.display = '';
}

function toggleCronWsCustom() {
  const form = document.getElementById('cron-ws-custom-form');
  const toggle = document.getElementById('cron-ws-custom-toggle');
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
    document.getElementById('cron-workdir').focus();
  } else {
    form.style.display = 'none';
    if (toggle) toggle.style.display = '';
  }
}

function toggleCronCustom() {
  const form = document.getElementById('cron-custom-form');
  const toggle = document.getElementById('cron-custom-toggle');
  if (form.style.display === 'none') {
    form.style.display = '';
    toggle.style.display = 'none';
    // Clear preset selection
    const overlay = form.closest('.modal-overlay');
    if (overlay) overlay._cronSchedule = '';
    document.querySelectorAll('#cron-schedule-list li').forEach(li => {
      li.classList.remove('selected');
      li.setAttribute('aria-selected', 'false');
    });
    const input = document.getElementById('cron-schedule');
    input.focus();
    if (!input._cronPreviewBound) {
      let previewTimer;
      input.addEventListener('input', function() {
        clearTimeout(previewTimer);
        previewTimer = setTimeout(() => previewCronSchedule(input.value.trim()), 300);
      });
      input._cronPreviewBound = true;
    }
  } else {
    form.style.display = 'none';
    toggle.style.display = '';
  }
}

async function previewCronSchedule(schedule) {
  const hint = document.getElementById('cron-preview-hint');
  if (!hint) return;
  if (!schedule) { hint.textContent = ''; hint.className = 'cron-preview-hint'; return; }
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/preview?schedule=' + encodeURIComponent(schedule), { headers });
    const data = await r.json();
    if (data.valid) {
      hint.className = 'cron-preview-hint ok';
      hint.textContent = 'next run: ' + timeAgo(data.next_run, true);
    } else {
      hint.className = 'cron-preview-hint err';
      hint.textContent = data.error || 'invalid schedule';
    }
  } catch (e) {
    hint.className = 'cron-preview-hint err';
    hint.textContent = 'preview error';
  }
}

async function doCreateCronJob() {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  // Resolve schedule: preset selection or custom input
  let schedule = overlay._cronSchedule || '';
  const customInput = document.getElementById('cron-schedule');
  if (customInput && customInput.value.trim()) schedule = customInput.value.trim();
  if (!schedule) { showToast('schedule is required', 'warning'); return; }
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
    const r = await fetch('/api/cron', {method: 'POST', headers, body: JSON.stringify(body)});
    if (!r.ok) { showToast('create failed: ' + await r.text()); return; }
    const data = await r.json();
    if (overlay) overlay.remove();
    showToast('cron job created', 'success');
    fetchCronJobs();
    if (data.id) {
      const key = 'cron:' + data.id;
      sessionWorkspaces[key] = workDir || defaultWorkspace || '/tmp';
      lastVersion = 0;
      await fetchSessions();
      selectSession(key, 'local');
    }
  } catch (e) { showToast('error: ' + e.message); }
}

function openCronPanel() {
  selectedKey = null; selectedNode = null;
  if (wsm.subscribedKey) wsm.unsubscribe();
  if (eventTimer) { clearInterval(eventTimer); eventTimer = null; }
  document.querySelectorAll('.session-card').forEach(el => el.classList.remove('active'));
  mobileEnterChat();
  fetchCronJobs().then(() => renderCronPanel());
}

function renderCronPanel() {
  const main = document.getElementById('main');
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
        '</div>';
  if (cronJobs.length === 0) {
    html +=
      '<div class="cron-empty">' +
        '<div class="cron-empty-icon" aria-hidden="true">&#9201;</div>' +
        '<div class="cron-empty-hint">No cron jobs yet</div>' +
        '<button type="button" class="cron-empty-cta" onclick="createNewCronJob()">Create your first cron job</button>' +
      '</div>';
  } else {
    const sorted = [...cronJobs].sort((a, b) => b.created_at - a.created_at);
    html += sorted.map(j => {
      const status = j.paused ? '<span class="badge paused">paused</span>' : '<span class="badge running">active</span>';
      const nextStr = j.next_run ? timeAgo(j.next_run, true) : '';
      const lastStr = j.last_run_at ? timeAgo(j.last_run_at) : '';
      const wdStr = j.work_dir ? '<span class="cc-ws" title="' + escAttr(j.work_dir) + '">' + esc(shortPath(j.work_dir)) + '</span>' : '';
      let result = '';
      if (j.last_error) {
        result = '<div class="cc-result err"><span class="cc-icon">\u2716</span><span class="cc-text">' + esc(j.last_error) + '</span></div>';
      } else if (j.last_result) {
        result = '<div class="cc-result ok"><span class="cc-icon">\u2714</span><span class="cc-text">' + esc(j.last_result) + '</span></div>';
      }
      const promptBlock = j.prompt
        ? '<div class="cc-prompt">' + esc(j.prompt) + '</div>'
        : '<div class="cc-prompt placeholder">(no prompt — tap to set)</div>';
      const toggleBtn = j.paused
        ? '<button type="button" class="cc-btn" onclick="cronResume(\'' + escJs(j.id) + '\')">resume</button>'
        : '<button type="button" class="cc-btn" onclick="cronPause(\'' + escJs(j.id) + '\')">pause</button>';
      return '<div class="cron-card" role="button" tabindex="0" onclick="openCronSession(\'' + escJs(j.id) + '\')" onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();openCronSession(\'' + escJs(j.id) + '\')}">' +
        promptBlock +
        '<div class="cc-schedule">' + esc(j.schedule) + '</div>' +
        '<div class="cc-meta">' + status + wdStr +
          (lastStr ? '<span>ran ' + lastStr + '</span>' : '') +
          (nextStr ? '<span>next ' + nextStr + '</span>' : '') +
        '</div>' +
        result +
        '<div class="cc-actions" onclick="event.stopPropagation()">' +
          toggleBtn +
          '<button type="button" class="cc-btn danger" onclick="cronDelete(\'' + escJs(j.id) + '\')">delete</button>' +
        '</div>' +
      '</div>';
    }).join('');
  }
  html += '</div></div>';
  main.innerHTML = html;
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
    const cronBadge = document.getElementById('cron-badge');
    if (cronBadge) {
      // Badge surfaces jobs needing attention (paused or last run errored),
      // not the raw total — avoids a persistent red dot on healthy setups.
      const attention = cronJobs.filter(j => j.paused || j.last_error).length;
      cronBadge.textContent = attention;
      cronBadge.style.display = attention > 0 ? '' : 'none';
    }
  } catch (e) { console.error('fetch cron:', e); }
}

async function cronPause(id) {
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/pause', { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) { showToast('pause failed'); return; }
    fetchCronJobs().then(() => renderCronPanel());
  } catch (e) { showToast('error: ' + e.message); }
}

async function cronResume(id) {
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron/resume', { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) { showToast('resume failed'); return; }
    fetchCronJobs().then(() => renderCronPanel());
  } catch (e) { showToast('error: ' + e.message); }
}

async function cronDelete(id) {
  if (!confirm('Delete cron job ' + id + '?')) return;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch('/api/cron?id=' + encodeURIComponent(id), { method: 'DELETE', headers });
    if (!r.ok) { showToast('delete failed'); return; }
    fetchCronJobs().then(() => renderCronPanel());
  } catch (e) { showToast('error: ' + e.message); }
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

/* ===== Initialization ===== */

fetchSessions();
sessionPollTimer = setInterval(fetchSessions, 5000);
scanDiscovered();
discoveredPollTimer = setInterval(scanDiscovered, 30000);
fetchCronJobs(); // load initial cron state for badge
wsm.connect();
initMobile();
initViewportTracking();
initSwipeDelete();
initSwipeBack();
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
