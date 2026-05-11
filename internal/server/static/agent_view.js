// agent_view.js — RFC v4 agent-team-ui frontend module.
//
// Phase 2.5: moved the 5 banner helpers out of dashboard.js.
// Phase 3: click-to-drill-in + WS agent_subscribe + tool_result folding +
//          auto-collapse + breadcrumb + HTTP poll fallback.
//
// Loaded AFTER dashboard.js via <script> tag ordering (dashboard.html).
// Depends on globals already defined by dashboard.js: esc, escAttr,
// fmtDuration, sessionsData, sid, selectedKey, selectedNode, turnState,
// sessionScrollPos, wsm, showToast, eventsUrl, eventHtml,
// renderEventsWithDividers, toolVerb.

(function () {
  'use strict';

  // ─── Phase 2.5 banner helpers ─────────────────────────────────────────

  function renderAgentRows() {
    var agents = turnState.agents;
    if (agents.length === 0) return '';

    // Separate solo subagents from team members.
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
    // Solo subagents.
    for (var j = 0; j < solos.length; j++) {
      html += agentRowHtml(solos[j]);
    }
    // Team groups.
    var teamNames = Object.keys(teams);
    for (var k = 0; k < teamNames.length; k++) {
      var tn = teamNames[k];
      var members = teams[tn];
      html += '<div class="rb-team-header"><span class="team-icon">◆</span>' +
        esc(tn) + '<span class="team-count">' + members.length + ' agents</span></div>';
      for (var m = 0; m < members.length; m++) {
        html += agentRowHtml(members[m]);
      }
    }
    return html;
  }

  function agentRowHtml(a) {
    var isDone = a.status === 'completed' || a.status === 'error';
    var cls = 'rb-agent-row' + (isDone ? ' done' : '');
    // Phase 3: mark the currently-selected agent row when we have an active
    // drill-in view so the banner highlights which internal stream the main
    // event area is showing.
    if (state.activeTaskID && a.taskId && a.taskId === state.activeTaskID) {
      cls += ' active';
    }
    var label = a.name || a.description || 'agent';
    // Phase 3: clickable when we have a task_id. Pre-task_start agents
    // ("spawned" status without TaskID) have no internal view yet, so skip
    // the onclick to avoid a dead tap.
    var clickable = !!a.taskId;
    var onclick = clickable
      ? ' onclick="window.AgentView.switchTo(\'' + escAttr(a.taskId) + '\')"'
      : '';
    var parts = '<div class="' + cls + '" data-task="' + escAttr(a.taskId || '') + '"' +
      (clickable ? ' role="button" tabindex="0"' : '') + onclick + '>';
    parts += '<span class="sa-dot"></span>';
    if (a.background) parts += '<span class="sa-bg">[bg]</span>';
    parts += '<span class="sa-name">' + esc(label) + '</span>';
    // Detail: lastTool or description.
    var detail = '';
    if (a.lastTool) detail = a.lastTool;
    else if (a.description && a.name) detail = a.description;
    if (detail) parts += '<span class="sa-detail">· ' + esc(detail) + '</span>';
    // Stats.
    var stat = '';
    if (a.toolUses > 0) stat += a.toolUses + ' calls';
    if (a.durationMs > 0) stat += (stat ? ' · ' : '') + fmtDuration(a.durationMs);
    if (isDone) stat += (stat ? ' · ' : '') + '✓';
    if (stat) parts += '<span class="sa-stat">· ' + stat + '</span>';
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
    var sd = sessionsData[sid(selectedKey, selectedNode || 'local')];
    if (sd && sd.subagents && sd.subagents.length > 0) {
      turnState.agents = sd.subagents.map(function (sa) {
        return {
          toolUseId: '', taskId: '', name: sa.name, teamName: '',
          description: sa.activity || '', background: !!sa.background,
          lastTool: '', toolUses: 0, totalTokens: 0, durationMs: 0, status: 'running'
        };
      });
    }
  }

  // ─── Phase 3: drill-in state ──────────────────────────────────────────
  //
  // `state` is the agent-view specific state kept independent of turnState
  // so turn-boundary resets (result / user events) don't nuke an open
  // drill-in view. The activeTaskID persists across turns until the user
  // Esc's back or switches sessions.

  var state = {
    activeKey: '',     // session key the active view belongs to
    activeTaskID: '',  // empty = parent view
    activeAgentName: '',
    activeTeamName: '',
    activeStatus: '',  // mirrors the tailer status for the breadcrumb stat
    switchSeq: 0,      // monotonic — lets async Resolve polls abandon stale switches
    retries: 0,        // 202 retry counter (bounded, §3.6.4)
    pollTimer: null,   // HTTP fallback interval ID
    pollAfterMS: 0,    // HTTP poll watermark
  };

  var MAX_SWITCH_RETRIES = 20; // §R14: 5 s retry ceiling

  // switchTo(taskID) — drill into a specific agent's internal transcript.
  // Called by:
  //   - banner row onclick (AgentView.switchTo)
  //   - WS agent_subscribe_rejected{reason:"capacity"} fallback path
  //   - Esc (via switchTo(null))
  function switchTo(taskID) {
    var seq = ++state.switchSeq;
    if (!taskID) {
      state.retries = 0;
    }
    state.activeKey = selectedKey || '';
    state.activeTaskID = taskID || '';
    // Opening a drill-in cancels any auto-collapse-in-progress on the banner
    // so the user's explicit click isn't immediately undone.
    if (turnState.collapsedByAuto !== undefined) {
      turnState.collapsedByAuto = false;
    }
    stopHttpPoll();
    refreshBanner();

    var el = document.getElementById('events-scroll');
    if (!el) return;

    if (!taskID) {
      hideBreadcrumb();
      // Back to parent view: re-render the ring-buffered parent events.
      unsubscribeCurrent();
      el.innerHTML = '';
      if (typeof window.fetchEvents === 'function') {
        window.lastEventTime = 0;
        window.fetchEvents(true);
      }
      return;
    }

    // Found the row? Populate the breadcrumb with the best info we have.
    var row = findAgentByTaskId(taskID);
    if (row) {
      state.activeAgentName = row.name || '';
      state.activeTeamName = row.teamName || '';
      state.activeStatus = row.status || '';
    }
    showBreadcrumb();
    el.innerHTML = '<div class="empty-state loading-indicator">正在加载 agent 内部事件…</div>';

    // Fetch the initial event slice via HTTP; WS subscribe runs in parallel
    // and catches up afterwards. The HTTP path handles 202/404/tombstone.
    var dispatchKey = selectedKey;
    var node = selectedNode || 'local';
    fetchAgentEventsInitial(taskID, dispatchKey, node, seq);
  }

  function fetchAgentEventsInitial(taskID, dispatchKey, node, seq) {
    var qs = '?key=' + encodeURIComponent(dispatchKey) +
      '&node=' + encodeURIComponent(node) +
      '&task_id=' + encodeURIComponent(taskID) +
      '&limit=200';
    fetch('/api/sessions/agent_events' + qs, { credentials: 'same-origin' })
      .then(function (r) {
        // Stale switch? Drop.
        if (seq !== state.switchSeq || selectedKey !== dispatchKey) return;
        if (r.status === 202) {
          state.retries++;
          if (state.retries >= MAX_SWITCH_RETRIES) {
            showToast('该 agent 暂无内部记录', 'warning');
            switchTo(null);
            return;
          }
          // Tell the user we're polling so 5s of "正在加载…" doesn't feel
          // like the UI hung. Without this feedback, historical agent rows
          // (from sessions that predate the linker fix) look identical to
          // a truly loading view for the full retry window.
          if (state.retries >= 4) {
            var elLoad = document.getElementById('events-scroll');
            if (elLoad) {
              var loadDiv = elLoad.querySelector('.loading-indicator');
              if (loadDiv) {
                loadDiv.textContent = '等待 agent 内部事件… (' +
                  state.retries + '/' + MAX_SWITCH_RETRIES + ')';
              }
            }
          }
          setTimeout(function () {
            if (seq !== state.switchSeq) return;
            fetchAgentEventsInitial(taskID, dispatchKey, node, seq);
          }, 250);
          return;
        }
        if (r.status === 404) {
          showToast('该 agent 暂无内部记录', 'warning');
          switchTo(null);
          return;
        }
        if (!r.ok) {
          showToast('加载失败 (' + r.status + ')', 'error');
          switchTo(null);
          return;
        }
        return r.json();
      })
      .then(function (events) {
        if (!events || seq !== state.switchSeq) return;
        renderAgentEvents(events, /*reset=*/ true);
        subscribeCurrent(taskID);
      })
      .catch(function (err) {
        if (seq !== state.switchSeq) return;
        console.warn('agent_events fetch failed', err);
        switchTo(null);
      });
  }

  function renderAgentEvents(events, reset) {
    var el = document.getElementById('events-scroll');
    if (!el) return;
    if (reset) el.innerHTML = '';
    if (!events || events.length === 0) {
      if (reset && el.innerHTML === '') {
        el.innerHTML = '<div class="empty-state">该 agent 暂无内部事件</div>';
      }
      return;
    }
    // Delegate to dashboard.js's shared renderer so the sub-agent panel and
    // parent view stay visually identical (markdown, tool_result folding,
    // image thumbnails, time dividers). window.renderEventsWithDividers /
    // window.eventHtml are exported by dashboard.js right next to their
    // definitions; fall back to a plain-text stub only if both are somehow
    // missing (unexpected — contract is enforced by dashboard.html script
    // ordering).
    // includeInternal=true keeps tool_use / thinking / task_* bubbles that
    // the parent view hides — for a sub-agent panel those ARE the content.
    var renderOpts = { includeInternal: true };
    var renderAll = typeof window.renderEventsWithDividers === 'function'
      ? window.renderEventsWithDividers : null;
    var renderOne = typeof window.eventHtml === 'function'
      ? window.eventHtml : null;
    if (renderAll) {
      el.insertAdjacentHTML('beforeend', renderAll(events, 0, renderOpts));
    } else if (renderOne) {
      for (var i = 0; i < events.length; i++) {
        var html = renderOne(events[i], renderOpts);
        if (html) el.insertAdjacentHTML('beforeend', html);
      }
    } else {
      for (var j = 0; j < events.length; j++) {
        var ev = events[j];
        if (!ev) continue;
        var div = document.createElement('div');
        div.className = 'event event-' + (ev.type || 'unknown');
        div.textContent = '[' + (ev.type || '?') + '] ' + (ev.summary || '');
        el.appendChild(div);
      }
    }
    // Track scroll position for sessionScrollPos restore on next switch.
    var k = sid(selectedKey, selectedNode || 'local') + '|' + state.activeTaskID;
    var pos = sessionScrollPos[k];
    if (pos && typeof pos.scrollTop === 'number') {
      el.scrollTop = pos.scrollTop;
    } else {
      el.scrollTop = 0;
    }
  }

  function appendAgentEvent(ev) {
    var el = document.getElementById('events-scroll');
    if (!el || !ev) return;
    var renderOne = typeof window.eventHtml === 'function'
      ? window.eventHtml : null;
    if (renderOne) {
      var html = renderOne(ev, { includeInternal: true });
      if (!html) return;
      el.insertAdjacentHTML('beforeend', html);
    } else {
      var div = document.createElement('div');
      div.className = 'event event-' + (ev.type || 'unknown');
      div.textContent = '[' + (ev.type || '?') + '] ' + (ev.summary || '');
      el.appendChild(div);
    }
    var nearBottom = (el.scrollHeight - el.scrollTop - el.clientHeight) < 80;
    if (nearBottom) el.scrollTop = el.scrollHeight;
  }

  // ─── WS subscribe / unsubscribe ────────────────────────────────────

  function subscribeCurrent(taskID) {
    if (!taskID) return;
    var msg = {
      type: 'agent_subscribe',
      key: selectedKey,
      node: selectedNode || 'local',
      task_id: taskID,
    };
    if (wsm && typeof wsm.send === 'function') {
      wsm.send(msg);
    }
  }

  function unsubscribeCurrent() {
    var taskID = state.activeTaskID;
    if (!taskID) return;
    var msg = {
      type: 'agent_unsubscribe',
      key: state.activeKey || selectedKey,
      node: selectedNode || 'local',
      task_id: taskID,
    };
    if (wsm && typeof wsm.send === 'function') {
      wsm.send(msg);
    }
  }

  // ─── Breadcrumb ────────────────────────────────────────────────────

  function ensureBreadcrumbEl() {
    var el = document.getElementById('agent-breadcrumb');
    if (el) return el;
    var host = document.getElementById('running-banner');
    if (!host) return null;
    el = document.createElement('div');
    el.className = 'agent-breadcrumb';
    el.id = 'agent-breadcrumb';
    el.style.display = 'none';
    el.innerHTML =
      '<button class="bc-back" type="button" aria-label="返回父会话" ' +
      'onclick="window.AgentView.switchTo(null)">← 返回父会话</button>' +
      '<span class="bc-tag">agent</span>' +
      '<span class="bc-name" id="bc-agent-name"></span>' +
      '<span class="bc-team" id="bc-agent-team"></span>' +
      '<span class="bc-stat" id="bc-agent-stat"></span>';
    // Insert at the top of the events scroll container so it sits just
    // above the agent events, matching the RFC §3.6.5 mockup.
    var scroll = document.getElementById('events-scroll');
    if (scroll && scroll.parentNode) {
      scroll.parentNode.insertBefore(el, scroll);
    } else {
      host.appendChild(el);
    }
    return el;
  }

  function showBreadcrumb() {
    var el = ensureBreadcrumbEl();
    if (!el) return;
    el.style.display = '';
    var nm = document.getElementById('bc-agent-name');
    var tm = document.getElementById('bc-agent-team');
    var st = document.getElementById('bc-agent-stat');
    if (nm) nm.textContent = state.activeAgentName || '';
    if (tm) tm.textContent = state.activeTeamName || '';
    if (st) st.textContent = state.activeStatus || '';
  }

  function hideBreadcrumb() {
    var el = document.getElementById('agent-breadcrumb');
    if (el) el.style.display = 'none';
  }

  function refreshBreadcrumbStat(patch) {
    if (!state.activeTaskID) return;
    var st = document.getElementById('bc-agent-stat');
    if (!st) return;
    var pieces = [];
    if (patch && patch.tool_uses > 0) pieces.push(patch.tool_uses + ' calls');
    if (patch && patch.duration_ms > 0) pieces.push(fmtDuration(patch.duration_ms));
    if (pieces.length > 0) st.textContent = pieces.join(' · ');
  }

  // ─── WS message dispatch entrypoints ───────────────────────────────
  // Called by dashboard.js's onMessage switch for the four "agent_*"
  // message types. Keeping them here means new message types don't
  // force churn in dashboard.js.

  function onAgentEvent(msg) {
    if (!msg || !msg.event) return;
    if (msg.task_id !== state.activeTaskID) return;
    appendAgentEvent(msg.event);
  }

  function onAgentMeta(msg) {
    if (!msg || !msg.task_id) return;
    // Apply meta to the banner row so stat line stays fresh.
    var row = findAgentByTaskId(msg.task_id);
    if (row && msg.meta) {
      if (msg.meta.last_tool) row.lastTool = msg.meta.last_tool;
      if (msg.meta.tool_uses > 0) row.toolUses = msg.meta.tool_uses;
      if (msg.meta.duration_ms > 0) row.durationMs = msg.meta.duration_ms;
      refreshBanner();
    }
    if (msg.task_id === state.activeTaskID) {
      refreshBreadcrumbStat(msg.meta);
    }
  }

  function onAgentDone(msg) {
    if (!msg || !msg.task_id) return;
    var row = findAgentByTaskId(msg.task_id);
    if (row) {
      row.status = msg.status || 'completed';
      refreshBanner();
    }
    if (msg.task_id === state.activeTaskID) {
      state.activeStatus = msg.status || 'completed';
      var st = document.getElementById('bc-agent-stat');
      if (st) {
        var cur = st.textContent || '';
        st.textContent = cur ? cur + ' · ' + (msg.status || '完成')
          : (msg.status || '完成');
      }
    }
  }

  function onAgentSubscribeRejected(msg) {
    if (!msg) return;
    if (msg.task_id !== state.activeTaskID) return;
    switch (msg.reason) {
      case 'capacity':
        showToast('服务器繁忙，改用 HTTP 轮询 (3 秒/次)', 'info');
        startHttpPoll(msg.task_id);
        break;
      case 'remote_not_supported':
        showToast('跨节点 agent 视图暂不支持', 'warning');
        switchTo(null);
        break;
      case 'session_not_found':
        showToast('会话已关闭', 'warning');
        switchTo(null);
        break;
      case 'tombstone':
      case 'no_linker':
      case 'closed':
        showToast('该 agent 暂无内部记录', 'warning');
        switchTo(null);
        break;
      case 'pending':
        // Linker hasn't seen this task_id yet; the HTTP 202 path already
        // handles the retry loop for us — no extra action needed.
        break;
      default:
        showToast('agent 订阅被拒绝 (' + msg.reason + ')', 'warning');
        switchTo(null);
    }
  }

  // ─── HTTP poll fallback (capacity rejected path) ───────────────────

  function startHttpPoll(taskID) {
    stopHttpPoll();
    state.pollAfterMS = 0;
    state.pollTimer = setInterval(function () {
      if (taskID !== state.activeTaskID) {
        stopHttpPoll();
        return;
      }
      var qs = '?key=' + encodeURIComponent(selectedKey) +
        '&node=' + encodeURIComponent(selectedNode || 'local') +
        '&task_id=' + encodeURIComponent(taskID) +
        '&after=' + state.pollAfterMS +
        '&limit=200';
      fetch('/api/sessions/agent_events' + qs, { credentials: 'same-origin' })
        .then(function (r) {
          if (taskID !== state.activeTaskID) return;
          // 202 = tailer still spinning up; no body to parse, wait next tick.
          if (r.status === 202) return;
          if (!r.ok) return;
          return r.json();
        })
        .then(function (events) {
          if (!events || !events.length) return;
          for (var i = 0; i < events.length; i++) {
            appendAgentEvent(events[i]);
          }
          // Advance the watermark only when the server gave us a real
          // timestamp. time===0 means the event predates the field; treating
          // it as "newest" would pin after=0 forever and cause duplicate
          // renders on every 3s tick.
          var lastTime = events[events.length - 1].time;
          if (typeof lastTime === 'number' && lastTime > state.pollAfterMS) {
            state.pollAfterMS = lastTime;
          }
        })
        .catch(function () { /* swallow; will retry */ });
    }, 3000);
  }

  function stopHttpPoll() {
    if (state.pollTimer) {
      clearInterval(state.pollTimer);
      state.pollTimer = null;
    }
  }

  // ─── Esc key — back to parent ──────────────────────────────────────

  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (!state.activeTaskID) return;
    // Don't hijack Esc if the user is focused in a text input (the
    // dashboard's interrupt shortcut owns Esc there).
    var t = e.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' ||
      t.isContentEditable)) {
      return;
    }
    e.preventDefault();
    switchTo(null);
  });

  // ─── Session change hook ───────────────────────────────────────────
  // When the user picks a different session, any open drill-in on the
  // previous session must close — otherwise agent_event messages for the
  // now-inactive taskID would leak into the new session's event list.
  //
  // Called by dashboard.js's selectSession *before* it mutates selectedKey
  // (so saveScrollPos still keys off the old session). We therefore accept
  // the target key/node as arguments rather than comparing against the now-
  // stale global. When onSessionSwitch is called with no args (legacy call
  // sites), fall back to unconditional cleanup — any call site that did not
  // pass a target is by construction changing sessions.
  function onSessionSwitch(targetKey, targetNode) {
    if (!state.activeTaskID) return;
    var tKey = targetKey == null ? null : targetKey;
    var tNode = targetNode == null ? null : targetNode;
    if (tKey !== null) {
      var curNode = state.activeKey ? (selectedNode || 'local') : '';
      if (tKey === state.activeKey &&
          (tNode === null || tNode === (curNode || 'local'))) {
        // Same session re-click — keep drill-in alive.
        return;
      }
    }
    unsubscribeCurrent();
    stopHttpPoll();
    state.activeTaskID = '';
    state.activeKey = '';
    state.activeAgentName = '';
    state.activeTeamName = '';
    state.activeStatus = '';
    hideBreadcrumb();
  }

  // ─── Exports ───────────────────────────────────────────────────────

  // Window globals so dashboard.js keeps its existing bare-name calls.
  window.renderAgentRows = renderAgentRows;
  window.agentRowHtml = agentRowHtml;
  window.findAgentByToolUseId = findAgentByToolUseId;
  window.findAgentByTaskId = findAgentByTaskId;
  window.initAgentsFromSession = initAgentsFromSession;

  // AgentView namespace — Phase 3 callers should use these.
  window.AgentView = {
    renderAgentRows: renderAgentRows,
    agentRowHtml: agentRowHtml,
    findByToolUseId: findAgentByToolUseId,
    findByTaskId: findAgentByTaskId,
    initFromSession: initAgentsFromSession,

    // Phase 3 API.
    switchTo: switchTo,
    onAgentEvent: onAgentEvent,
    onAgentMeta: onAgentMeta,
    onAgentDone: onAgentDone,
    onAgentSubscribeRejected: onAgentSubscribeRejected,
    onSessionSwitch: onSessionSwitch,
    activeTaskID: function () { return state.activeTaskID; },
  };
})();
