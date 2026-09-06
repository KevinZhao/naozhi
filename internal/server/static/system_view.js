// system_view.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureSystemView(), called from dashboard's module body.
import { esc, fetchJSON, formatDurationShort, nzState, showToast } from './nz_util.js';

const deps = {
  formatAbsTime: null,
  getMsgValue: null,
  mainEmptyHtml: null,
  refreshCostSummary: null,
  renderServiceOverviewHtml: null,
  setActivityView: null,
  timeAgo: null,
  wireQuickAskInput: null,
};
export function configureSystemView(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('system_view dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// ===== System view (sysession daemons) =====
//
// Read-only mirror of the 自动化 (cron) view for naozhi's built-in background
// daemons — today just the AutoTitler (自动化改名). The backend already exposes
// everything we need at GET /api/system/daemons (a DaemonStatus[] array; empty
// when sysession is disabled), so this view is pure presentation:
//   - one card per daemon: 状态点 + 名称 + 启用 pill + 描述
//   - last-run summary: 状态 / 触发方式 / 用时 / 多久之前
//   - per-tick stats (examined/acted/skipped_*) as compact chips
//   - consecutive-failure warnings when a daemon is unhealthy
// No create/edit/delete: these are naozhi-owned, configured via YAML+restart
// (RFC system-session §9.2). The view polls at 5s while active and stops on
// leave (stopSystemPoll), matching the cron view's poll-while-visible model.
let systemDaemons = [];
let systemPollTimer = null;

function openSystemPanel() {
  if (nzState.activeView !== 'system') { deps.setActivityView('system'); return; }
  renderSystemView();              // paint from cache (instant)
  fetchSystemDaemons().then(renderSystemView).catch(function () {});
  startSystemPoll();
}

function startSystemPoll() {
  if (systemPollTimer) return;
  // 5s cadence: daemons tick on the order of 30s, so a faster poll only burns
  // requests. Skip the fetch when the tab is hidden to avoid background churn.
  systemPollTimer = setInterval(function () {
    if (document.hidden || nzState.activeView !== 'system') return;
    fetchSystemDaemons().then(renderSystemView).catch(function () {});
  }, 5000);
}

function stopSystemPoll() {
  if (systemPollTimer) { clearInterval(systemPollTimer); systemPollTimer = null; }
}

async function fetchSystemDaemons() {
  // 8s timeout mirrors the cron poll. The endpoint always returns a JSON array
  // (empty when sysession is off), so a non-array is treated as empty.
  const data = await fetchJSON(NZ_CONTRACT.API.system_daemons, { timeoutMs: 8000 });
  systemDaemons = Array.isArray(data) ? data : [];
  updateSystemBadge();
  return systemDaemons;
}

// A daemon needs attention when its last run failed or it has accumulated
// consecutive CLI/validation failures (the circuit-breaker inputs). Mirrors
// the cron attention badge so the rail surfaces problems without opening the
// view.
function daemonNeedsAttention(d) {
  if (!d) return false;
  if ((d.consecutive_cli_failures || 0) > 0) return true;
  if ((d.consecutive_validation_failures || 0) > 0) return true;
  const lr = d.last_run;
  return !!(lr && lr.state && lr.state !== 'succeeded');
}

function updateSystemBadge() {
  const n = systemDaemons.filter(daemonNeedsAttention).length;
  const badge = document.getElementById('abnav-system-badge');
  if (badge) badge.hidden = n === 0;
  // ui-polish-light-theme D9: on mobile the 系统 tab is hidden (6 tabs → 5)
  // and its entry point lives inside 设置 — mirror the attention badge onto
  // the 设置 tab so daemon trouble still pings through the rail. Desktop
  // keeps this badge hidden via CSS (the 系统 tab is visible there).
  const sBadge = document.getElementById('abnav-settings-badge');
  if (sBadge) sBadge.hidden = n === 0;
}

// systemStateMeta maps a last-run state into (dot class, Chinese label).
// Unknown states fall back to a neutral dot + the raw string so a forward-
// compat backend state never renders as blank.
function systemStateMeta(state) {
  switch (state) {
    case 'succeeded': return { cls: 'ok', label: '成功' };
    case 'failed': return { cls: 'fail', label: '失败' };
    case 'timed_out': return { cls: 'fail', label: '超时' };
    case 'canceled': return { cls: 'off', label: '已取消' };
    default: return { cls: 'off', label: state || '—' };
  }
}

// systemTickLabel renders a Go time.Duration (JSON-marshalled as integer
// nanoseconds) as a compact human string. Falls back to formatDurationShort
// once we're past sub-second, reusing the existing ms formatter.
function systemTickLabel(ns) {
  if (!ns || ns <= 0) return '—';
  return formatDurationShort(ns / 1e6);
}

// systemStatLabel maps a flattened TickReport stat key to a Chinese label.
// The skipped_* keys are "skipped_" + the daemon's Skipped-map reason
// (flattenTickReport in manager.go). Keys MUST match the reasons the daemon
// actually emits — AutoTitler's bumpSkip(...) calls in auto_titler.go produce
// reserved_namespace / group_chat / origin_user / min_first_turns /
// min_rename_interval / no_new_turns (pinned by
// TestDashboardJS_SystemStatLabelsMatchAutoTitlerSkipReasons).
// Unknown reasons keep their raw suffix so a new skip-bucket still shows up.
const SYSTEM_STAT_LABELS = {
  examined: '检查',
  acted: '执行',
  skipped_reserved_namespace: '跳过·保留命名空间',
  skipped_group_chat: '跳过·群聊',
  skipped_origin_user: '跳过·用户已命名',
  skipped_min_first_turns: '跳过·轮次不足',
  skipped_no_new_turns: '跳过·无新增对话',
  skipped_min_rename_interval: '跳过·命名间隔未到',
};
function systemStatLabel(key) {
  if (SYSTEM_STAT_LABELS[key]) return SYSTEM_STAT_LABELS[key];
  if (key.indexOf('skipped_') === 0) return '跳过·' + key.slice(8);
  return key;
}

function renderSystemView() {
  const root = document.getElementById('system-main');
  if (!root) return;
  deps.refreshCostSummary().catch(() => {});
  const cards = systemDaemons.map(function (d) {
    const lr = d.last_run;
    const st = systemStateMeta(lr && lr.state);
    // Dot reflects the most salient state: unhealthy > running-but-fine.
    const dotCls = daemonNeedsAttention(d) ? 'fail' : (d.enabled ? (lr ? st.cls : 'ok') : 'off');
    const enabledPill = d.enabled
      ? '<span class="sys-pill on">已启用</span>'
      : '<span class="sys-pill off">已停用</span>';
    let metaRows = '';
    metaRows += '<span>周期 <b>' + esc(systemTickLabel(d.tick)) + '</b></span>';
    metaRows += '<span>累计运行 <b>' + (d.runs_total || 0) + '</b> 次</span>';
    if (d.process_started_at) {
      const started = Date.parse(d.process_started_at);
      if (!isNaN(started)) {
        metaRows += '<span>启动于 <b title="' + esc(deps.formatAbsTime(started)) + '">' + esc(deps.timeAgo(started)) + '</b></span>';
      }
    }
    let lastRunBlock = '<div class="sys-meta"><span>尚未运行</span></div>';
    let statsBlock = '';
    if (lr) {
      const ended = lr.ended_at ? Date.parse(lr.ended_at) : NaN;
      const whenTxt = !isNaN(ended) ? deps.timeAgo(ended) : '—';
      const whenTitle = !isNaN(ended) ? deps.formatAbsTime(ended) : '';
      const triggerTxt = lr.trigger === 'manual' ? '手动' : '定时';
      lastRunBlock =
        '<div class="sys-meta">' +
          '<span>最近一次 <b>' + esc(st.label) + '</b></span>' +
          '<span>触发 <b>' + esc(triggerTxt) + '</b></span>' +
          '<span>用时 <b>' + esc(formatDurationShort(lr.duration_ms)) + '</b></span>' +
          '<span><b title="' + esc(whenTitle) + '">' + esc(whenTxt) + '</b></span>' +
        '</div>';
      const stats = lr.stats || {};
      const chips = Object.keys(stats).map(function (k) {
        return '<span class="sys-stat">' + esc(systemStatLabel(k)) + ' <b>' + (stats[k] || 0) + '</b></span>';
      });
      if (chips.length) statsBlock = '<div class="sys-stats-label">本次统计</div><div class="sys-stats">' + chips.join('') + '</div>';
    }
    let warnBlock = '';
    const cliF = d.consecutive_cli_failures || 0;
    const valF = d.consecutive_validation_failures || 0;
    if (cliF > 0 || valF > 0) {
      const parts = [];
      if (cliF > 0) parts.push('连续 CLI 失败 ' + cliF + ' 次');
      if (valF > 0) parts.push('连续校验失败 ' + valF + ' 次');
      warnBlock = '<div class="sys-warn">⚠ ' + esc(parts.join(' · ')) + '</div>';
    }
    return '<div class="sys-card">' +
        '<div class="sys-card-top">' +
          '<span class="sys-state-dot ' + dotCls + '"></span>' +
          '<span class="sys-name">' + esc(d.name || '—') + '</span>' +
          enabledPill +
        '</div>' +
        (d.description ? '<p class="sys-desc">' + esc(d.description) + '</p>' : '') +
        '<div class="sys-meta">' + metaRows + '</div>' +
        lastRunBlock +
        statsBlock +
        warnBlock +
      '</div>';
  }).join('');
  const body = cards ||
    '<div class="system-empty">暂无系统任务。<br>内置后台守护（如自动化改名）需在配置中启用 <code>sysession</code> 后重启生效。</div>';
  root.innerHTML =
    '<div class="system-head"><h1>系统任务</h1></div>' +
    '<div class="system-body">' +
      deps.renderServiceOverviewHtml() +
      '<div class="system-intro">naozhi 内置的后台守护进程。它们由系统自动调度，只读展示运行状态，配置通过 YAML 调整后重启生效。</div>' +
      body +
    '</div>';
}

// reconcileSelectedNode keeps `nzState.selectedNode` honest now that the sidebar node
// selector is gone (the node picker moved into the New Session modal). It no
// longer touches any DOM — the sidebar lists every node's sessions together —
// but `nzState.selectedNode` still drives dispatch targeting and the main header, so
// if the persisted selection points at a node that has since disappeared
// (remote removed server-side while the dashboard is open) we snap it back to
// 'local'. Kept as a single entry point so the many call sites (poll, session
// switch, session create) don't each need to re-derive the same guard.
// deselectNodeSession clears the main pane after the node hosting the
// selected session disconnected (PurgeNodeSubscriptions → error{node, "node
// disconnected"}). Mirrors dismissSession's deselect: the draft is kept for
// when the node comes back, the caller already dropped the WS bookkeeping,
// and the sidebar refetch removes the node's cards.
function deselectNodeSession(nodeID) {
  const inp = document.getElementById('msg-input');
  const draft = inp ? deps.getMsgValue(inp) : '';
  if (draft) nzState.sessionDrafts[nzState.selectedKey] = draft;
  nzState.selectedKey = null;
  const main = document.getElementById('main');
  if (main) {
    main.innerHTML = deps.mainEmptyHtml();
    deps.wireQuickAskInput();
  }
  showToast('节点 ' + nodeID + ' 已断开，已退出该节点上的会话', 'warning');
}

function reconcileSelectedNode() {
  if (nzState.selectedNode && nzState.selectedNode !== 'local' && !nzState.nodesData[nzState.selectedNode]) {
    nzState.selectedNode = 'local';
    try { localStorage.setItem('nz_selectedNode', nzState.selectedNode); } catch(_) {}
  }
}


export {
  deselectNodeSession,
  fetchSystemDaemons,
  openSystemPanel,
  reconcileSelectedNode,
  renderSystemView,
  stopSystemPoll,
};
