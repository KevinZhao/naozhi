// system_view.js — the 系统 view (sysession daemons).
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Shared state is read from the state.js objects; the one
// call back into dashboard (setActivityView) goes through shell.js.
import { NZ_CONTRACT } from './contract.js';
import { perSession, selection, sessionList, ui } from './state.js';
import { esc, fetchJSON, formatBytes, formatDurationShort, showToast } from './nz_util.js';
import { wireQuickAskInput } from './auth_modal.js';
import { shell } from './shell.js';
import { cachedCostSummary, costCardTitle, formatAbsTime, formatHomeCost, getMsgValue, mainEmptyHtml, refreshCostSummary, renderServiceOverviewHtml, timeAgo } from './utilities.js';

// ===== System view (sysession daemons) =====
//
// Read-only mirror of the 自动化 (cron) view for naozhi's built-in background
// daemons (自动化改名, attachment-gc). The backend already exposes
// everything we need at GET /api/system/daemons (a DaemonStatus[] array; empty
// when sysession is disabled), so this view is pure presentation:
//   - one card per daemon: 状态点 + 名称 + 启用 pill + 描述
//   - last-run summary: 状态 / 触发方式 / 用时 / 多久之前
//   - per-tick stats (examined/acted/skipped_*) as compact chips
//   - consecutive-failure warnings when a daemon is unhealthy
//   - its 30-day ledger cost (session key sys:<name>) once it has entries
// No create/edit/delete: these are naozhi-owned, configured via YAML+restart
// (RFC system-session §9.2). The view polls at 5s while active and stops on
// leave (stopSystemPoll), matching the cron view's poll-while-visible model.
let systemDaemons = [];
let systemPollTimer = null;

function openSystemPanel() {
  if (ui.activeView !== 'system') { shell.setActivityView('system'); return; }
  renderSystemView();              // paint from cache (instant)
  fetchSystemDaemons().then(renderSystemView).catch(function () {});
  startSystemPoll();
}

function startSystemPoll() {
  if (systemPollTimer) return;
  // 5s cadence: daemons tick on the order of 30s, so a faster poll only burns
  // requests. Skip the fetch when the tab is hidden to avoid background churn.
  systemPollTimer = setInterval(function () {
    if (document.hidden || ui.activeView !== 'system') return;
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
// nanoseconds) as a compact human string via the ms formatter.
function systemTickLabel(ns) {
  if (!ns || ns <= 0) return '—';
  return formatDurationShort(ns / 1e6);
}

// systemStatLabel maps a flattened TickReport stat key (flattenTickReport in
// manager.go) to a Chinese label. Keys MUST match what the daemons emit:
// AutoTitler's bumpSkip(...) reasons as skipped_*, attachment-gc's gcCount*
// Counts keys verbatim (pinned by TestDashboardJS_SystemStatLabels*).
// Unknown reasons keep their raw suffix so a new skip-bucket still shows up.
// A live (non-dry-run) tick's would_reap_* counted that tick's deletions.
const SYSTEM_STAT_LABELS = {
  examined: '检查',
  acted: '执行',
  skipped_reserved_namespace: '跳过·保留命名空间',
  skipped_group_chat: '跳过·群聊',
  skipped_origin_user: '跳过·用户已命名',
  skipped_min_first_turns: '跳过·轮次不足',
  skipped_no_new_turns: '跳过·无新增对话',
  skipped_min_rename_interval: '跳过·命名间隔未到',
  skipped_restored_auto_title: '跳过·沿用已有自动标题',
  dry_run: '演练模式',
  would_reap_legacy_no_meta: '可回收·无meta旧文件',
  would_reap_meta_no_refs: '可回收·无引用(高风险)',
  would_reap_refs_expired: '可回收·引用过期',
  would_reap_bytes: '可回收体积',
};
function systemStatLabel(key, live) {
  const label = SYSTEM_STAT_LABELS[key];
  if (label) return live && key.indexOf('would_reap_') === 0 ? label.replace('可回收', '已回收') : label;
  if (key.indexOf('skipped_') === 0) return '跳过·' + key.slice(8);
  return key;
}
// systemStatValue renders a stat: *_bytes as a size, dry_run as a yes flag.
function systemStatValue(key, v) {
  if (key.endsWith('_bytes')) return formatBytes(v) || '0 B';
  return key === 'dry_run' ? (v ? '是' : '否') : String(v || 0);
}

// refreshSystemCosts asks the ledger for the overview total and each daemon's
// own (booked under sys:<name>); a fresh snapshot repaints the view.
function refreshSystemCosts() {
  for (const k of [''].concat(systemDaemons.map((d) => 'sys:' + d.name))) {
    refreshCostSummary(k).then((updated) => updated && ui.activeView === 'system' && renderSystemView()).catch(() => {});
  }
}

// daemonCostHtml is the card's 30-day cost row; '' until the ledger holds an
// entry for the daemon.
function daemonCostHtml(name) {
  const c = cachedCostSummary('sys:' + name);
  if (!c || c.entries === 0) return '';
  return '<span class="sys-cost" title="' + esc(costCardTitle(c, '仅 ' + name)) + '">近 30 天花费 <b>' + esc(formatHomeCost(c.usd)) + '</b></span>';
}

function renderSystemView() {
  const root = document.getElementById('system-main');
  if (!root) return;
  refreshSystemCosts();
  const cards = systemDaemons.map(function (d) {
    const lr = d.last_run;
    const st = systemStateMeta(lr && lr.state);
    // Dot reflects the most salient state: unhealthy > running-but-fine.
    const dotCls = daemonNeedsAttention(d) ? 'fail' : (d.enabled ? (lr ? st.cls : 'ok') : 'off');
    const enabledPill = d.enabled
      ? '<span class="sys-pill on">已启用</span>'
      : '<span class="sys-pill off">已停用</span>';
    let metaRows = '<span>周期 <b>' + esc(systemTickLabel(d.tick)) + '</b></span>';
    metaRows += '<span>累计运行 <b>' + (d.runs_total || 0) + '</b> 次</span>';
    if (d.process_started_at) {
      const started = Date.parse(d.process_started_at);
      if (!isNaN(started)) {
        metaRows += '<span>启动于 <b title="' + esc(formatAbsTime(started)) + '">' + esc(timeAgo(started)) + '</b></span>';
      }
    }
    metaRows += daemonCostHtml(d.name);
    let lastRunBlock = '<div class="sys-meta"><span>尚未运行</span></div>';
    let statsBlock = '';
    if (lr) {
      const ended = lr.ended_at ? Date.parse(lr.ended_at) : NaN;
      const whenTxt = !isNaN(ended) ? timeAgo(ended) : '—';
      const whenTitle = !isNaN(ended) ? formatAbsTime(ended) : '';
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
        return '<span class="sys-stat">' + esc(systemStatLabel(k, !stats.dry_run)) + ' <b>' + esc(systemStatValue(k, stats[k])) + '</b></span>';
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
      renderServiceOverviewHtml() +
      '<div class="system-intro">naozhi 内置的后台守护进程。它们由系统自动调度，只读展示运行状态，配置通过 YAML 调整后重启生效。</div>' +
      body +
    '</div>';
}

// deselectNodeSession clears the main pane after the node hosting the
// selected session disconnected (PurgeNodeSubscriptions → error{node, "node
// disconnected"}). Mirrors dismissSession's deselect: the draft is kept for
// when the node comes back, the caller already dropped the WS bookkeeping,
// and the sidebar refetch removes the node's cards.
function deselectNodeSession(nodeID) {
  const inp = document.getElementById('msg-input');
  const draft = inp ? getMsgValue(inp) : '';
  if (draft) perSession.drafts[selection.key] = draft;
  selection.key = null;
  const main = document.getElementById('main');
  if (main) {
    main.innerHTML = mainEmptyHtml();
    wireQuickAskInput();
  }
  showToast('节点 ' + nodeID + ' 已断开，已退出该节点上的会话', 'warning');
}

// reconcileSelectedNode snaps a persisted `selection.node` back to 'local'
// when that node has disappeared: it still drives dispatch targeting and the
// main header. One entry point for the poll / switch / create call sites.
function reconcileSelectedNode() {
  if (selection.node && selection.node !== 'local' && !sessionList.nodesData[selection.node]) {
    selection.node = 'local';
    try { localStorage.setItem('nz_selectedNode', selection.node); } catch(_) {}
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
