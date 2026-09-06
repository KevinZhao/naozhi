// running_banner.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureRunningBanner(), called from dashboard's module body.
import { escAttr, nzState, nzTest, nzViews, showToast } from './nz_util.js';

const deps = {
  ICONS: null,
  getMsgValue: null,
  getToken: null,
  setMsgValue: null,
  showNetworkError: null,
  sid: null,
  wsm: null,
};
export function configureRunningBanner(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('running_banner dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// --- Running banner: tool activity + agent tracking ---

let turnState = {
  toolCount: 0, currentTool: null, agents: [], isThinking: false,
  thinkingSummary: '', toolCounts: {}, toolOrder: [], turnStartTime: 0, isWriting: false,
  timerId: null, justSent: false
};

// resetTurnState clears the per-turn banner state. opts.keepTimer preserves
// the elapsed anchor (turnStartTime/timerId) and the justSent flag: used when
// the server echoes back the user message this client just sent, so the
// timer started at send time survives the turn-boundary reset instead of
// being cleared and re-anchored at the first streamed event (#2435).
function resetTurnState(opts) {
  const keepTimer = !!(opts && opts.keepTimer);
  const kept = keepTimer
    ? { turnStartTime: turnState.turnStartTime, timerId: turnState.timerId, justSent: turnState.justSent }
    : { turnStartTime: 0, timerId: null, justSent: false };
  if (!keepTimer && turnState.timerId) clearInterval(turnState.timerId);
  turnState = {
    toolCount: 0, currentTool: null, agents: [], isThinking: false,
    thinkingSummary: '', toolCounts: {}, toolOrder: [], turnStartTime: kept.turnStartTime, isWriting: false,
    timerId: kept.timerId, justSent: kept.justSent
  };
  // #2435: blank the elapsed chip so the next banner does not open showing
  // the previous turn's final time until its first 1s tick lands.
  if (!keepTimer) {
    const elapsedEl = document.getElementById('rb-elapsed');
    if (elapsedEl) elapsedEl.textContent = '';
  }
  refreshBanner();
}

// resetTurnStateForUserEcho handles the turn boundary a `user` event marks.
// If this client sent that message (justSent is still up — no real turn event
// has arrived yet) the send-time timer is kept; a user turn started on another
// surface (IM, another tab) gets the full reset and anchors at its first event.
function resetTurnStateForUserEcho() {
  resetTurnState(turnState.justSent ? { keepTimer: true } : undefined);
}

// paintTurnElapsed renders turnState.turnStartTime → "m:ss" into #rb-elapsed.
// Shared by startTurnTimer and the history-rebuild path so both paint
// immediately instead of waiting for the first interval tick.
function paintTurnElapsed() {
  const el = document.getElementById('rb-elapsed');
  if (!el || !turnState.turnStartTime) return;
  const s = Math.max(0, Math.floor((Date.now() - turnState.turnStartTime) / 1000));
  el.textContent = Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}

// startTurnTimer anchors the elapsed chip. Called from
// markSessionOptimisticRunning at send time (#2435: the anchor used to be the
// first streamed event, so CLI spawn latency was silently excluded) and
// idempotently from every subsequent turn event.
function startTurnTimer() {
  if (turnState.turnStartTime) return;
  turnState.turnStartTime = Date.now();
  paintTurnElapsed();
  turnState.timerId = setInterval(paintTurnElapsed, 1000);
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

// toolSummaryLine derives the banner's one-line tool summary from a tool_use
// event's detail. FormatToolInput on the server prefixes the detail with the
// tool name ("Bash ls", "mcp__x__y: {...}"), and toolVerb already leads with
// the (localized) tool name, so the prefix is dropped here — otherwise MCP
// tools render as "使用 mcp__x__y mcp__x__y: {...}" (#2435).
function toolSummaryLine(tool, detail) {
  if (!detail) return '';
  let line = detail.split('\n')[0];
  if (tool && line.indexOf(tool) === 0) {
    line = line.slice(tool.length).replace(/^[:\s]+/, '');
  }
  return line.substring(0, 60);
}

function refreshBanner() {
  const actEl = document.getElementById('tool-activity');
  const thinkEl = document.getElementById('rb-thinking-summary');
  const agEl = document.getElementById('rb-agents');
  const statsEl = document.getElementById('rb-stats');

  // Line 1: current activity. justSent (set on the optimistic flip, cleared by
  // the first real event in updateTurnState) gives a distinct "received,
  // starting up" message during the CLI-spawn window so the operator knows the
  // send landed rather than staring at a generic static "处理中…".
  if (actEl) {
    if (turnState.currentTool) {
      actEl.textContent = toolVerb(turnState.currentTool.tool, turnState.currentTool.summary);
    } else if (turnState.isThinking) {
      actEl.textContent = '思考中...';
    } else if (turnState.isWriting) {
      actEl.textContent = '输出中...';
    } else if (turnState.justSent) {
      actEl.textContent = '已发送，正在处理…';
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
    agEl.innerHTML = nzViews.agent ? nzViews.agent.renderAgentRows() : '';
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
    const sKey = deps.sid(nzState.selectedKey, nzState.selectedNode);
    const sess = nzState.sessionsData[sKey];
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
  if (!nzState.selectedKey) return;
  var card = document.querySelector('.session-card[data-key="' + escAttr(nzState.selectedKey) + '"]');
  if (!card) return;
  var meta = card.querySelector('.sc-meta');
  if (!meta) return;
  var count = turnState.agents.length;
  var existing = meta.querySelector('.sc-agents');
  if (count > 0) {
    var html = deps.ICONS.robot + '\u00D7' + count;
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
  // Any real turn event ends the optimistic "已发送，正在处理…" window — the
  // CLI is now actively thinking/using-tools/writing, so let the normal
  // activity labels take over.
  turnState.justSent = false;
  switch (ev.type) {
    case 'tool_use':
      turnState.toolCount++;
      trackTool(ev.tool || ev.summary);
      turnState.currentTool = { tool: ev.tool || ev.summary, summary: toolSummaryLine(ev.tool || ev.summary, ev.detail) };
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
      var a1 = nzViews.agent && nzViews.agent.findByToolUseId(ev.tool_use_id);
      if (a1) {
        a1.taskId = ev.task_id;
        a1.status = 'running';
      }
      break;
    case 'task_progress':
      var a2 = nzViews.agent && (nzViews.agent.findByTaskId(ev.task_id) || nzViews.agent.findByToolUseId(ev.tool_use_id));
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
      var a3 = nzViews.agent && (nzViews.agent.findByTaskId(ev.task_id) || nzViews.agent.findByToolUseId(ev.tool_use_id));
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
      // Also clear the tool tallies so hasContent (refreshBanner) doesn't stay
      // truthy on a turn boundary — keeps this branch consistent with
      // resetTurnState, the other turn-reset path.
      turnState.toolOrder = [];
      turnState.toolCounts = {};
      turnState.toolCount = 0;
      updateSidebarAgentBadge();
      break;
  }
}

function interruptSession() {
  if (!nzState.selectedKey) return;
  const sd = nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode || 'local')];
  if (!sd || sd.state !== 'running') return;
  const targetNode = nzState.selectedNode && nzState.selectedNode !== 'local' ? nzState.selectedNode : '';
  // Claude Code 风格：中断时把刚发的那条用户文本回填到输入框方便改写。
  // 只在输入框当前为空时回填，避免覆盖用户已经开始输入的新内容；回填后
  // 把光标挪到末尾、聚焦、滚进视口。回填完成即消费掉 lastSent，防止同一条
  // 文本在后续多次中断里反复回填。
  const lastText = nzState.sessionLastSent[deps.sid(nzState.selectedKey, nzState.selectedNode)];
  if (lastText) {
    const input = document.getElementById('msg-input');
    if (input && !deps.getMsgValue(input)) {
      deps.setMsgValue(input, lastText);
      try {
        input.focus();
        const range = document.createRange();
        range.selectNodeContents(input);
        range.collapse(false);
        const sel = window.getSelection();
        if (sel) { sel.removeAllRanges(); sel.addRange(range); }
      } catch (_) {}
      nzState.sessionDrafts[nzState.selectedKey] = lastText;
      delete nzState.sessionLastSent[deps.sid(nzState.selectedKey, nzState.selectedNode)];
    }
  }
  if (deps.wsm.isConnected()) {
    const req = { type: 'interrupt', key: nzState.selectedKey, id: 'int' + Date.now() };
    if (targetNode) req.node = targetNode;
    deps.wsm.send(req);
    showToast('已发送中断', 'warning');
  } else {
    // HTTP fallback when WebSocket is disconnected
    const headers = {'Content-Type': 'application/json'};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const body = { key: nzState.selectedKey };
    if (targetNode) body.node = targetNode;
    fetch(NZ_CONTRACT.API.sessions_interrupt, {
      method: 'POST',
      headers,
      body: JSON.stringify(body)
    }).then(r => r.json()).then(d => {
      showToast(d.status === 'ok' ? '已发送中断' : '会话未在运行', 'warning');
    }).catch((e) => deps.showNetworkError('中断会话', e));
  }
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
  nzState.sessionScrollPos[deps.sid(key, node || 'local')] = { fromBottom, atBottom };
}

// restoreScrollPos: 如果有保存的位置且不是贴底，则恢复并返回 true；
// 否则（无记录 / 之前在底）返回 false，交由调用方走 stickEventsBottom 贴底。
function restoreScrollPos(key, node) {
  const el = document.getElementById('events-scroll');
  if (!el || !key) return false;
  const pos = nzState.sessionScrollPos[deps.sid(key, node || 'local')];
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


export {
  applyEventToTurnState,
  fmtDuration,
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
};

// nz.test surface for the Playwright specs (#2557 PR-E3 pattern).
Object.assign(nzTest, { fmtDuration });
