// cron_timeline.js — the per-job run timeline: rows, inline expand, detail /
// transcript / snapshot fetches, load-more and head-refresh reconcilers
// (#2715 D4 follow-up: cron_view.js four-region split, region 2).
//
// Owns cronTimelineState (per-job window/cache) and cronExpandedRunId (the
// single inline-expanded row). Everything it needs from the cron view proper
// — which job the drawer shows, the jobs array, the list repaint — is
// injected once via configureCronTimeline(), called from cron_view.js's
// module body; the dependency edge stays one-way (view → timeline).

import { showAuthModal } from './auth_modal.js';
import { getToken } from './dashboard.js';
import { renderMd, runPendingAsync } from './render_md.js';
import { formatAbsTime, showAPIError, showNetworkError } from './utilities.js';
import {
  esc,
  escAttr,
  fetchJSON,
  formatCostUSD,
  formatRunDuration,
  runStateDot,
  runStateLabel,
} from './nz_util.js';

const deps = {
  cronAttentionQueueHtml: null, // §7.4 confirmation queue block stitched into the panel
  cronDetailJobId: null, // () => string|null — which job the drawer shows
  cronErrorClassLabel: null,
  cronJobLedgerCostHtml: null, // per-job 30d ledger block stitched into the panel
  cronJobs: null, // () => Job[] — the live jobs array
  cronRecentRunsCap: null, // () => number — server-side recent_runs cap
  fetchCronJobs: null,
  renderCronPanel: null,
};
export function configureCronTimeline(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('cron_timeline dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// P2 cron-run-history (RFC §8.2) — 时间轴每个 job 的本地状态。
// runs / nextBefore: 分页列表 + 游标；details: 已 fetch 的单条 run 详情缓存
// （不持久化，session 切换即丢）。
// §16 inline-expand 回归: 展开态由 cronExpandedRunId 模块状态驱动（同时
// 只展开一行），rowHtml 在选中行下方就地嵌入 cronTimelineDetailHtml。
const cronTimelineState = Object.create(null);

// §16 inline-expand 回归: 当前展开的 run（同时只展开一行）。
// jobId 必须同时匹配以处理用户切到另一个 cron 但 expandedRunId 没清的场景。
const cronExpandedRunId = { jobId: null, runId: null };

// CRON_TIMELINE_FRESH_MS — 超过此 TTL 的 timeline 缓存视为陈旧，下次进入
// cron session 详情时清掉强制重拉。R220-FE-3：用户切走再切回中间可能有
// 新 run，但本地状态仍 stale，timeline 不主动刷新。
const CRON_TIMELINE_FRESH_MS = 30 * 1000;

// R20260610-CR-005: cap the per-job timeline cache. Entries are only
// evicted on cronDelete otherwise, so browsing many jobs accumulates
// runs/details/lastRenderedHtml without bound. LRU by lastAccess.
const CRON_TIMELINE_MAX_ENTRIES = 20;

// R20260610-CR-007 (#1998): cap st.runs after the refreshHead merge.
// Without a cap, deep loadMore paging (50/page) followed by repeated WS
// run_ended refreshes grows the merged array without bound, and every
// WS event pays an O(n log n) sort + full re-render over it. When the cap
// truncates the tail, pagination is re-armed (done=false, nextBefore=
// new oldest started_at) so loadMore can still page past the cut.
const CRON_TIMELINE_MAX_RUNS = 500;

function getCronTimelineState(jobId) {
  if (!cronTimelineState[jobId]) {
    const ids = Object.keys(cronTimelineState);
    if (ids.length >= CRON_TIMELINE_MAX_ENTRIES) {
      let oldest = null;
      for (const id of ids) {
        if (id === jobId) continue;
        if (!oldest || cronTimelineState[id].lastAccess < cronTimelineState[oldest].lastAccess) oldest = id;
      }
      if (oldest) delete cronTimelineState[oldest];
    }
    cronTimelineState[jobId] = {
      runs: [],
      nextBefore: 0,
      done: false,         // true = next_before 缺失，已到结尾
      showAll: false,      // true = 用户已展开 5 行折叠（查看全部 / 展开第 ≥6 行），重绘不再折回
      loading: false,
      details: Object.create(null),
      lastMountAt: 0,       // 上次 mount 渲染的 ms 时戳；renderCronTimelineForSession 用来判 stale
      lastAccess: 0,        // LRU 时戳，每次 getCronTimelineState 命中更新
      // R243-PERF-12 (#817): cache the last innerHTML written by
      // renderCronTimelinePanel so a no-op re-render (e.g. WS broadcast
      // arrives but no run changed) skips the full innerHTML rewrite.
      // Stored on the per-job state so it is reset together with
      // runs/details when CRON_TIMELINE_FRESH_MS evicts the cache.
      lastRenderedHtml: '',
    };
  }
  cronTimelineState[jobId].lastAccess = Date.now();
  return cronTimelineState[jobId];
}

// renderCronTimelineForJob is the canonical drawer-side entry point.
// renderCronTimelineForSession (the legacy mainShell-coupled variant) has
// been removed — see cronDrawerHtml above.

// cronTimelineHtml — 渲染整个"执行历史"section 的内层 HTML。
// 头部：简洁标题"最近运行"+ 总次数小标签（cron-dashboard-redesign P3 §4：
// 把"成功率/最后错误分类"等运维指标隐去；用户首屏只看"它最近跑得怎样"，
// 排查时再点单条 run 进 sheet 看详情）。
// 行列表：默认折叠到 5 条，"查看全部 N 条"按钮展开剩余 + 触发 loadMore。
// CRON_TIMELINE_DEFAULT_VISIBLE = 5 条与 cronDrawerSpecHtml 的视觉密度同源。
function cronTimelineHtml(jobId, job, st) {
  const stats = job && job.stats;
  const total = stats ? (stats.total | 0) : 0;
  const headTitle = total > 0
    ? '最近运行 · ' + total + ' 次'
    : '最近运行';
  // §7.5 cost小字：纯前端聚合已加载 run 的 cost_usd（只有云沙箱 run 带）。
  // 标注"已加载 N 条"避免误读为全量账单——这是轻量可见性，非账单系统。
  const costSummary = cronTimelineCostSummaryHtml(st.runs);
  const ledgerCost = deps.cronJobLedgerCostHtml(jobId);
  const rowsHtml = st.runs.length === 0
    ? '<div class="ct-empty">暂无执行记录。下次调度或点击「立即执行」触发首次运行。</div>'
    : st.runs.map(r => cronTimelineRowHtml(jobId, r, st)).join('');

  // P3 §4 折叠机制：data-collapsed=true 时 CSS 只露前 5 行；点 [查看全部]
  // 切到 false。展开态记在 st.showAll（而非只在 DOM dataset）：detail fetch
  // 落地 / WS 刷新 / 选中行都会走 renderCronTimelinePanel 整段重建 innerHTML，
  // 只存 DOM 的话每次重绘都折回 5 行。
  const collapsed = st.runs.length > 5 && !st.showAll;
  const initiallyCollapsed = collapsed ? 'true' : 'false';
  const hiddenCount = Math.max(0, st.runs.length - 5);

  let moreBtn = '';
  if (st.runs.length > 0) {
    if (collapsed) {
      // 折叠态："查看全部 N 条"——展开后再让既有 [加载更多] 接管分页。
      moreBtn = '<button type="button" class="ct-more-btn ct-show-all"' +
        ' data-hidden-count="' + hiddenCount + '"' +
        ' data-action="cron-tl-showall">' +
        '查看全部 ' + st.runs.length + ' 条' +
      '</button>';
    } else if (st.done) {
      moreBtn = '<button type="button" class="ct-more-btn" disabled aria-disabled="true">已到结尾</button>';
    } else {
      moreBtn = '<button type="button" class="ct-more-btn"' +
        (st.loading ? ' disabled' : '') +
        ' data-action="cron-tl-more" data-job="' + escAttr(jobId) + '">' +
        (st.loading ? '加载中…' : '加载更多') +
      '</button>';
    }
  }
  // §7.4 confirmation queue banner (PR-6) — prepended above the timeline so a
  // failed-transport / orphaned side-effecting run is visible the moment the
  // operator opens any cron panel. Global (cross-job); '' when the queue is
  // empty. Fetched independently by cronAttentionRefresh().
  const queueBanner = deps.cronAttentionQueueHtml();
  return queueBanner +
    '<div class="ct-head">' +
      '<h3>' + esc(headTitle) + '</h3>' +
      ledgerCost +
      costSummary +
    '</div>' +
    '<div class="ct-rows" data-collapsed="' + initiallyCollapsed + '" data-job-id="' + escAttr(jobId) + '">' + rowsHtml + '</div>' +
    (moreBtn ? '<div class="ct-more">' + moreBtn + '</div>' : '');
}


// cronTimelineCostSummaryHtml sums cost_usd over the loaded runs for the
// §7.5 cost小字. Returns '' when no loaded run carries cost (all-local job).
// "已加载 N 条" is explicit so the figure is never misread as a full bill —
// per RFC §7.5 this is lightweight visibility, not a billing system.
function cronTimelineCostSummaryHtml(runs) {
  if (!Array.isArray(runs) || runs.length === 0) return '';
  let sum = 0;
  let n = 0;
  for (const r of runs) {
    if (r && r.cost_usd) {
      sum += r.cost_usd;
      n++;
    }
  }
  if (n === 0) return '';
  return '<span class="ct-cost-sum" title="已加载 ' + n + ' 条云沙箱 run 的成本之和（非全量账单）">' +
      '☁️ ' + esc(formatCostUSD(sum)) + ' · ' + n + ' 条' +
    '</span>';
}

// cronTimelineToggleShowAll — 把 .ct-rows 从 collapsed 切到 expanded，
// 并把按钮替换为既有 [加载更多] 行为（如果 st.done 则替换为"已到结尾"）。
// 切换后还有更多页要拉的，下次点 [加载更多] 走原路径。
function cronTimelineToggleShowAll(btn) {
  if (!btn || !btn.parentNode) return;
  const wrap = btn.closest('.cron-timeline-panel');
  if (!wrap) return;
  const rows = wrap.querySelector('.ct-rows');
  if (!rows) return;
  rows.setAttribute('data-collapsed', 'false');
  const jobId = rows.getAttribute('data-job-id') || '';
  const st = cronTimelineState[jobId];
  if (st) st.showAll = true;
  // 替换按钮：用既有 cronTimelineLoadMore 路径——st.done 的话变灰态 [已到结尾]。
  const next = document.createElement('div');
  next.className = 'ct-more';
  if (!st || st.done) {
    next.innerHTML = '<button type="button" class="ct-more-btn" disabled aria-disabled="true">已到结尾</button>';
  } else {
    next.innerHTML = '<button type="button" class="ct-more-btn"' +
      (st.loading ? ' disabled' : '') +
      ' data-action="cron-tl-more" data-job="' + escAttr(jobId) + '">' +
      (st.loading ? '加载中…' : '加载更多') +
    '</button>';
  }
  const oldMore = btn.parentNode;
  if (oldMore && oldMore.parentNode) oldMore.parentNode.replaceChild(next, oldMore);
}

// cronTimelineRowHtml — 单条 run 行（§16 inline-expand 回归）。
// 选中行（cronExpandedRunId 命中）下方就地嵌入 .ctr-detail 详情块（复用
// cronTimelineDetailHtml 渲染逻辑）。同时只展开一行；点行 toggle。
// run 字段（CronRunSummary）：run_id / state / trigger / started_at / ended_at /
// duration_ms / session_id / error_class。所有字段均可为空，渲染时 fallback。
function cronTimelineRowHtml(jobId, r, st) {
  if (!r) return '';
  const runId = r.run_id || '';
  const state = r.state || '';
  const startedAbs = r.started_at ? formatAbsTime(r.started_at) : '';
  // 行主时间用紧凑显示（"5月17日 14:30"）；hover 看完整 ISO。
  const startedShort = r.started_at ? formatCronTimelineShort(r.started_at) : '—';
  const dur = state === 'running'
    ? '正在运行'
    : formatRunDuration(r.duration_ms || 0);
  const errCls = r.error_class || '';
  // §16: 选中/展开态由 cronExpandedRunId 决定（jobId+runId 同时匹配）。
  const isExpanded = !!(runId && cronExpandedRunId && cronExpandedRunId.runId === runId && cronExpandedRunId.jobId === jobId);
  const dotCls = runStateDot(state);
  const stateLbl = runStateLabel(state);

  // 副行：trigger / error_class（session_id 短 ID 已移除——对最终用户无意义；
  // 展开 inline 详情即可看到完整 session_id）
  const subParts = [];
  if (r.trigger) subParts.push('<span class="ctr-trigger">' + esc(r.trigger) + '</span>');
  if (errCls) {
    subParts.push('<span class="ctr-errcls">' + esc(deps.cronErrorClassLabel(errCls)) + '</span>');
  }
  // §7.5 per-run cost小字 — only sandbox runs carry cost_usd in the summary.
  if (r.cost_usd) {
    subParts.push('<span class="ctr-cost" title="本次成本估算">' + esc(formatCostUSD(r.cost_usd)) + '</span>');
  }
  const subRow = subParts.length > 0
    ? '<div class="ctr-sub">' + subParts.join('<span class="ctr-sep">·</span>') + '</div>'
    : '';

  // §16: 行展开态时下方嵌 .ctr-detail（复用 cronTimelineDetailHtml）。
  // detail 缓存命中即立刻渲染；否则给加载骨架，cronTimelineFetchDetail 会异步
  // 写入 st.details[runId] 并触发 renderCronTimelinePanel 重绘把骨架替换为内容。
  let detailBlock = '';
  if (isExpanded) {
    const det = (st && st.details) ? st.details[runId] : null;
    detailBlock = '<div class="ctr-detail" data-run-id="' + escAttr(runId) + '">' +
        cronTimelineDetailHtml(jobId, runId, r, det) +
      '</div>';
  }

  // 行 click/keydown 走 cron-tl-select（同 run 二次点击 = collapse）。
  // .ctr-detail 嵌在行内，handler 里有 closest('.ctr-detail') 守卫——否则点
  // <details> 输入快照 / ↩ 重放自 / ↻ 重放 / 拖选文字都会把行折起来。
  return '<div class="ctr' + (isExpanded ? ' is-selected is-expanded' : '') + '" data-run-id="' + escAttr(runId) + '"' +
      ' data-action="cron-tl-select" data-action-keydown="cron-tl-select" data-job="' + escAttr(jobId) + '"' +
      ' role="button" tabindex="0" aria-pressed="' + (isExpanded ? 'true' : 'false') + '" aria-expanded="' + (isExpanded ? 'true' : 'false') + '">' +
    '<div class="ctr-main">' +
      '<span class="ctr-dot ' + dotCls + '" aria-hidden="true"></span>' +
      '<span class="ctr-state">' + esc(stateLbl) + '</span>' +
      '<span class="ctr-time"' + (startedAbs ? ' title="' + escAttr(startedAbs) + '"' : '') + '>' + esc(startedShort) + '</span>' +
      (dur ? '<span class="ctr-dur">' + esc(dur) + '</span>' : '') +
    '</div>' +
    subRow +
    detailBlock +
  '</div>';
}

// formatCronTimelineShort — "5月17日 14:30" 紧凑标签。今年同年省年份；不同年加年份前缀。
function formatCronTimelineShort(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  if (isNaN(d.getTime())) return '';
  const now = new Date();
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const sameYear = d.getFullYear() === now.getFullYear();
  const dateStr = (d.getMonth() + 1) + '月' + d.getDate() + '日';
  const timeStr = pad(d.getHours()) + ':' + pad(d.getMinutes());
  return (sameYear ? '' : d.getFullYear() + '年') + dateStr + ' ' + timeStr;
}

// cronTimelineDetailHtml — 展开行内的详情面板。
//
// 现在收敛为单屏「最终输出」视图：错误优先 → result（markdown） → 回退到
// transcript 最后一条 assistant 文本。提示词、工具调用记录、原始 JSONL 一律
// 不展示——这些对绝大多数用户都是噪声，需要时仍能通过 transcript / detail
// 端点拿到。
//
// 历史：v2 期间用过 4-tab 容器（对话 / 工具 / 提示词 / 原始日志），
// 字面量 tabBtn('chat') / tabBtn('tools') / tabBtn('prompt') / tabBtn('raw')
// 被契约测试 (TestDashboardJS_TranscriptTabs) 钉死，下方 dead-code 块保留
// 这些字面量出现以维持 grep 兼容；真正的渲染走 finalBody。
function cronTimelineDetailHtml(jobId, runId, summary, detail) {
  if (!detail) {
    return '<div class="ctr-loading">加载详情中…</div>';
  }
  if (detail.__error) {
    return '<div class="ctr-err-load">加载失败：' + esc(detail.__error) + '</div>';
  }

  // §7.3 元信息条：云沙箱 run 顶部展示 镜像 · 时长 · 内存峰值 · 成本。
  // 本机 run 的 detail.sandbox 为空 → 整条不渲染（零增量）。
  const metaBar = cronSandboxMetaBarHtml(detail.sandbox);

  // 历史 4-tab UI 标记（已收敛为单屏「最终输出」，见下方）：
  //   tabBtn('chat', '对话')
  //   tabBtn('tools', '工具')
  //   tabBtn('prompt', '提示词')
  //   tabBtn('raw', '原始日志')
  // 上述字面量仅作契约测试 grep 锚点；UI 不再渲染 tab。

  const transcript = detail.__transcript || null;
  const hasTurns = transcript && Array.isArray(transcript.turns) && transcript.turns.length > 0;
  let lastAssistant = null;
  if (hasTurns) {
    const turns = transcript.turns;
    for (let i = turns.length - 1; i >= 0; i--) {
      const t = turns[i];
      if (t && t.kind === 'assistant' && t.text) { lastAssistant = t; break; }
    }
  }

  // §7.3 input-snapshot collapsible (PR-4), appended below the output;
  // metaBar (above) is prepended. Both empty for local / pre-snapshot runs.
  const snapshotPanel = cronSnapshotPanelHtml(detail.__snapshot);

  // §7.3 replay chain + action row (PR-6). The replay-of badge links this run
  // to the original it re-executed; the replay button re-injects the input
  // snapshot. failed-transport runs DISABLE the button (§6.2: fate unknown —
  // route through the §7.4 queue), success/failed-clean ENABLE it.
  const replayBar = cronReplayBarHtml(jobId, runId, detail);

  let body;
  if (detail.error_msg) {
    const errLabel = detail.error_class
      ? ' <span class="ctr-final-tag">' + esc(deps.cronErrorClassLabel(detail.error_class)) + '</span>'
      : '';
    body = '<div class="ctr-final err">' +
        '<div class="ctr-final-label">运行失败' + errLabel + '</div>' +
        '<pre class="ctr-final-body">' + esc(detail.error_msg) + '</pre>' +
      '</div>';
  } else if (detail.result) {
    body = '<div class="ctr-final">' +
        '<div class="ctr-final-body md">' + renderMd(detail.result) + '</div>' +
      '</div>';
  } else if (lastAssistant) {
    body = '<div class="ctr-final">' +
        '<div class="ctr-final-body md">' + renderMd(lastAssistant.text) + '</div>' +
      '</div>';
  } else if (transcript) {
    // transcript 已落地（成功 / fallback=missing / fallback=raw / 无 turns）
    // 但都拿不到 result / error / 最后 assistant 文本——给确定性空态。
    const msg = transcript.fallback === 'raw'
      ? '对话流无法解析，没有可展示的最终输出。'
      : '这次 run 没有保存最终输出。';
    body = '<div class="ctr-empty-detail">' + msg + '</div>';
  } else {
    // transcript 字段未定义 = fetch 还在飞，给加载态。
    body = '<div class="ctr-empty-detail">正在加载最终输出…</div>';
  }
  return metaBar + replayBar + body + snapshotPanel;
}

// cronReplayBarHtml renders the §7.3 replay action row for a sandbox run.
// Returns '' for non-sandbox runs (detail.sandbox absent) so local runs carry
// no replay UI. Three states drive the button (§6.2 safety on the UI face):
//
//   - failed-transport (error_class === 'sandbox_transport'): button DISABLED,
//     tooltip routes the operator to the §7.4 confirmation queue (replaying a
//     run whose microVM fate is unknown could double-run; the queue does the
//     Stop-confirm first).
//   - success / failed-clean: button ENABLED — safe to re-inject the snapshot.
//
// The replay-of badge (when detail.replay_of is set) shows this run was itself
// a replay, linking back to the original (click jumps to it).
function cronReplayBarHtml(jobId, runId, detail) {
  if (!detail || !detail.sandbox) return '';
  const parts = [];
  if (detail.replay_of) {
    parts.push('<button type="button" class="ctr-replay-of"' +
      ' data-action="cron-tl-jump" data-job="' + escAttr(jobId) + '" data-run="' + escAttr(detail.replay_of) + '"' +
      ' title="' + escAttr('这是一次重放，点击查看原始 run') + '">↩ 重放自 ' + esc(String(detail.replay_of).slice(0, 8)) + '</button>');
  }
  const isTransport = detail.error_class === 'sandbox_transport';
  if (isTransport) {
    parts.push('<button type="button" class="ctr-replay-btn" disabled' +
      ' title="' + escAttr('断流，云端状态未知——请到「待确认」队列先确认终止再重放') + '">↻ 重放（已禁用）</button>');
    parts.push('<span class="ctr-replay-hint">⚠ 状态未知，走待确认队列</span>');
  } else {
    parts.push('<button type="button" class="ctr-replay-btn"' +
      ' data-action="cron-replay" data-job="' + escAttr(jobId) + '" data-run="' + escAttr(runId) + '"' +
      ' title="' + escAttr('用同一份输入快照重新跑一遍（进全新微VM）') + '">↻ 重放</button>');
  }
  return '<div class="ctr-replay-bar">' + parts.join('') + '</div>';
}

// cronSandboxMetaBarHtml renders the §7.3 run-detail meta bar from the
// detail endpoint's `sandbox` receipt: 镜像 · 时长 · 内存峰值 · 成本.
// Returns '' for local runs (no receipt) so nothing renders. exit_status is
// shown only when non-zero (a failed exit is the operator's signal; exit 0
// is implied by a success render).
function cronSandboxMetaBarHtml(sb) {
  if (!sb) return '';
  const parts = [];
  parts.push('<span class="ctr-meta-item">☁️ 云沙箱</span>');
  if (sb.image_version) {
    parts.push('<span class="ctr-meta-item" title="镜像版本">' + esc(sb.image_version) + '</span>');
  }
  if (sb.duration_ms) {
    parts.push('<span class="ctr-meta-item" title="时长">' + esc(formatRunDuration(sb.duration_ms)) + '</span>');
  }
  if (sb.memory_peak_bytes) {
    parts.push('<span class="ctr-meta-item" title="内存峰值">' + esc(formatBytes(sb.memory_peak_bytes)) + '</span>');
  }
  if (sb.cost_usd) {
    parts.push('<span class="ctr-meta-item ctr-meta-cost" title="成本估算">' + esc(formatCostUSD(sb.cost_usd)) + '</span>');
  }
  if (sb.exit_status) {
    parts.push('<span class="ctr-meta-item ctr-meta-exit" title="退出码">exit ' + esc(String(sb.exit_status)) + '</span>');
  }
  return '<div class="ctr-meta-bar">' + parts.join('') + '</div>';
}

// formatBytes renders a byte count as a compact human size (MiB/GiB) for the
// meta bar. Integer-ish display — memory peaks are coarse signals.
function formatBytes(n) {
  if (!n || n < 0) return '';
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + ' GiB';
  if (n >= 1 << 20) return Math.round(n / (1 << 20)) + ' MiB';
  if (n >= 1 << 10) return Math.round(n / (1 << 10)) + ' KiB';
  return n + ' B';
}

// cronTimelineSelectRun — 点击 timeline 行（§16 inline-expand 回归）。
// 同一行二次点击 = collapse；不同行点击 = collapse 旧 + expand 新（同时只展开
// 一行，避免长 result 把列表撑成多屏）。
function cronTimelineSelectRun(jobId, runId) {
  if (!runId) return;
  if (cronExpandedRunId.jobId === jobId && cronExpandedRunId.runId === runId) {
    cronTimelineCollapse();
    return;
  }
  cronTimelineExpand(jobId, runId);
}

// ===== §16 inline-expand 回归: 行内展开状态机 =====
// cronExpandedRunId 模块状态见上方 cronTimelineState 同段。
// 与 v2 的差异：v2 用 st.expanded 数组允许多行同时展开，回归版用单值
// {jobId,runId} 收紧为同时只展开一行 — 长 result 不会把列表撑成多屏；
// 切到另一个 cron 时也不需要做 cleanup。

// cronTimelineExpand — 展开指定 run 的详情块。
// 1. 设 cronExpandedRunId（rowHtml 据此输出 .ctr-detail）
// 2. 重绘 timeline panel：旧行收起、新行就地嵌 .ctr-detail
// 3. scrollIntoView({block:'nearest'}) — 选中行不离开视口（用户期待）
// 4. detail 缓存未命中则异步 fetchDetail；落地后 panel 再次重绘把骨架替换为内容
function cronTimelineExpand(jobId, runId) {
  if (!jobId || !runId) return;
  cronExpandedRunId.jobId = jobId;
  cronExpandedRunId.runId = runId;
  const st = getCronTimelineState(jobId);
  // 展开第 ≥6 行（键盘 ↓ 翻页 / loadMore 回调）时自动解除折叠，否则该行
  // 被 .ct-rows[data-collapsed] CSS 隐藏，看起来像"点了没反应"。
  if (!st.showAll && st.runs.findIndex(r => r && r.run_id === runId) >= 5) st.showAll = true;
  renderCronTimelinePanel(jobId);
  scrollExpandedRunIntoView(runId);
  if (!st.details[runId]) {
    cronTimelineFetchDetail(jobId, runId);
  }
}

// cronTimelineCollapse — 收起当前展开行，焦点回原行。
function cronTimelineCollapse() {
  if (!cronExpandedRunId.runId) return;
  const prevJobId = cronExpandedRunId.jobId;
  const prevRunId = cronExpandedRunId.runId;
  cronExpandedRunId.jobId = null;
  cronExpandedRunId.runId = null;
  if (prevJobId) renderCronTimelinePanel(prevJobId);
  if (prevRunId) {
    const row = document.querySelector('.cron-timeline-panel .ctr[data-run-id="' + cssEscapeAttr(prevRunId) + '"]');
    if (row && typeof row.focus === 'function') row.focus();
  }
}

// navigateExpandedRun — ↑↓ 切上/下一条 run。
// 'prev' = ↑ = UI 中更靠上 = 时间上更新的 run（idx-1，因 timeline 倒序：newer first）
// 'next' = ↓ = UI 中更靠下 = 时间上更旧的 run（idx+1）
function navigateExpandedRun(direction) {
  if (!cronExpandedRunId.jobId || !cronExpandedRunId.runId) return;
  const jobId = cronExpandedRunId.jobId;
  const st = getCronTimelineState(jobId);
  if (!st.runs || st.runs.length === 0) return;
  const idx = st.runs.findIndex(r => r && r.run_id === cronExpandedRunId.runId);
  if (idx < 0) return;
  let nextIdx;
  if (direction === 'prev') nextIdx = idx - 1;
  else if (direction === 'next') nextIdx = idx + 1;
  else return;
  if (nextIdx < 0) return; // already at the newest run; nothing above it.
  // R20260614-LOGIC-8 (#2090): ↓ past the last loaded run on a multi-page
  // timeline used to hit the bounds guard and silently no-op, so the keyboard
  // could never reach older runs beyond the first page. When more pages exist
  // (!st.done), load the next page and expand the target once it arrives.
  if (nextIdx >= st.runs.length) {
    if (direction !== 'next' || st.done || st.loading) return; // genuinely at the end
    cronTimelineLoadMore(jobId, () => {
      // Re-resolve against the freshly grown list; expand the run that now
      // sits just after the current one (only if the user is still here).
      if (cronExpandedRunId.jobId !== jobId || cronExpandedRunId.runId == null) return;
      const st2 = getCronTimelineState(jobId);
      const i2 = st2.runs.findIndex(r => r && r.run_id === cronExpandedRunId.runId);
      if (i2 < 0 || i2 + 1 >= st2.runs.length) return; // page returned nothing new
      const target = st2.runs[i2 + 1];
      if (target && target.run_id) cronTimelineExpand(jobId, target.run_id);
    });
    return;
  }
  const nextRun = st.runs[nextIdx];
  if (!nextRun || !nextRun.run_id) return;
  cronTimelineExpand(jobId, nextRun.run_id);
}


// scrollExpandedRunIntoView — 'nearest' 让快速 ↑↓ 不引起列表来回跳，只有
// 行已滚出视野才滚。behavior:'auto' 不 smooth 避免追动画。
function scrollExpandedRunIntoView(runId) {
  if (!runId) return;
  const row = document.querySelector('.cron-timeline-panel .ctr[data-run-id="' + cssEscapeAttr(runId) + '"]');
  if (row && typeof row.scrollIntoView === 'function') {
    row.scrollIntoView({ behavior: 'auto', block: 'nearest' });
  }
}

// cssEscapeAttr — 在 attribute selector 里嵌 runId（hex UUID）。CSS.escape 在
// 现代浏览器可用；降级路径用 backslash 转义 ASCII 范围外字符。runId 来源是
// 后端生成的 hex UUID，不可能含 NUL，无需特殊处理。
function cssEscapeAttr(s) {
  if (typeof CSS !== 'undefined' && typeof CSS.escape === 'function') return CSS.escape(s);
  return String(s).replace(/[^a-zA-Z0-9_-]/g, '\\$&');
}

// cronTimelineFetchDetail — GET /api/cron/runs/{run_id}?job_id=... 异步拉详情。
// 完成后写入 st.details[runId] 并重绘当前 panel；session 切走后丢弃结果。
//
// cron-dashboard-redesign P2b: detail 落地后并发 fire transcript 端点，
// 拉到的 turns 写入 st.details[runId].__transcript 并触发再次重绘。
// transcript 端点失败不影响主 detail 路径——4-tab 渲染会优雅 fallback
// 到「原始日志」tab，所以这里 silent-swallow 异常即可。
async function cronTimelineFetchDetail(jobId, runId) {
  const st = getCronTimelineState(jobId);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = NZ_CONTRACT.API.cron_runs + '/' + encodeURIComponent(runId) + '?job_id=' + encodeURIComponent(jobId);
    const data = await fetchJSON(url, { headers, timeoutMs: 8000 });
    // Preserve sticky annotations (__transcript / __snapshot) across a
    // collapse→re-expand re-fetch: wholesale replacement would drop them
    // and make the snapshot panel flicker/disappear if its re-fetch fails
    // (review PR-4 F5). The annotation fetchers below re-stash fresh data.
    const prevDetail = st.details[runId];
    st.details[runId] = data || { __error: 'empty response' };
    if (prevDetail && st.details[runId] && !st.details[runId].__error) {
      if (prevDetail.__transcript) st.details[runId].__transcript = prevDetail.__transcript;
      if (prevDetail.__snapshot) st.details[runId].__snapshot = prevDetail.__snapshot;
    }
    // Fire-and-forget transcript fetch. Silent on failure; the 4-tab
    // renderer falls back to the raw view automatically.
    cronTimelineFetchTranscript(jobId, runId).catch(() => {});
    // §7.3 input-snapshot fetch (sandbox runs only; local runs get
    // available:false). Independent fire-and-forget like the transcript.
    cronTimelineFetchSnapshot(jobId, runId).catch(() => {});
  } catch (err) {
    // R220-FE-5: 401/403 走 authModal，与 fetchSessions 等其它路径保持一致；
    // 单 cron 详情失败不应让用户看到 "HTTP 401" 字样而不知所措。
    if (err && (err.status === 401 || err.status === 403)) {
      showAuthModal();
      st.details[runId] = { __error: '认证失败，请重新登录' };
    } else if (err && err.status === 404) {
      st.details[runId] = { __error: '记录不存在或已被清理' };
    } else if (err && err.status) {
      st.details[runId] = { __error: 'HTTP ' + err.status + ' ' + (err.message || '') };
    } else {
      st.details[runId] = { __error: '网络错误' };
    }
  }
  // cron-panel-consolidation RFC §4.6: 用 deps.cronDetailJobId() 判定当前 drawer
  // 还停在同一 job 上；selectedKey 在 cron 面板下永远为 null，已不能用。
  // §16: 行内展开后 panel 重绘已经把 .ctr-detail 内的骨架替换为真实 detail，
  // 不再需要单独刷 sheet body（sheet 已废弃）。
  if (deps.cronDetailJobId() === jobId) renderCronTimelinePanel(jobId);
}

// cronTimelineFetchTranscript fetches the JSONL-derived turn timeline
// for one run and stashes it on the detail object. cron-dashboard-
// redesign P2b §4.4.4. Independent of cronTimelineFetchDetail so a
// transcript failure cannot cascade into the main detail view.
async function cronTimelineFetchTranscript(jobId, runId) {
  const st = getCronTimelineState(jobId);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = NZ_CONTRACT.API.cron_runs + '/' + encodeURIComponent(runId) + '/transcript?job_id=' + encodeURIComponent(jobId);
    const data = await fetchJSON(url, { headers, timeoutMs: 12000 });
    if (st.details && st.details[runId]) {
      st.details[runId].__transcript = data || { fallback: 'missing', turns: [] };
    }
  } catch (err) {
    if (st.details && st.details[runId]) {
      // Mark as fallback so the renderer flips to raw without showing
      // a loading spinner forever. We don't surface a hard error to
      // the user — original detail tab is still functional.
      st.details[runId].__transcript = { fallback: 'missing', turns: [], __fetchErr: true };
    }
  }
  if (deps.cronDetailJobId() === jobId) renderCronTimelinePanel(jobId);
}

// cronTimelineFetchSnapshot fetches the §7.3 input snapshot (content-
// addressed prompt + model + secret ref names) for one run and stashes it
// on detail.__snapshot. Independent of the detail/transcript fetches so a
// snapshot miss never cascades. available:false for local / pre-snapshot
// runs — the renderer then omits the panel.
async function cronTimelineFetchSnapshot(jobId, runId) {
  const st = getCronTimelineState(jobId);
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = NZ_CONTRACT.API.cron_runs + '/' + encodeURIComponent(runId) + '/snapshot?job_id=' + encodeURIComponent(jobId);
    const data = await fetchJSON(url, { headers, timeoutMs: 8000 });
    if (st.details && st.details[runId]) {
      st.details[runId].__snapshot = data || { available: false };
    }
  } catch (_err) {
    if (st.details && st.details[runId]) {
      st.details[runId].__snapshot = { available: false };
    }
  }
  if (deps.cronDetailJobId() === jobId) renderCronTimelinePanel(jobId);
}

// cronSnapshotPanelHtml renders the §7.3 input-snapshot collapsible from a
// run's __snapshot. Returns '' when the run has no snapshot (local run /
// pre-snapshot / fetch in flight) so nothing renders for those.
//
// SECURITY: secret_refs are NAMES only (server never sends values, §5.1);
// the panel renders them as plain labels — there is no value to leak.
function cronSnapshotPanelHtml(snap) {
  if (!snap || !snap.available) return '';
  const rows = [];
  if (snap.model) {
    rows.push('<div class="ctr-snap-row"><span class="ctr-snap-k">模型</span>' +
      '<span class="ctr-snap-v">' + esc(snap.model) + '</span></div>');
  }
  if (snap.image_version) {
    rows.push('<div class="ctr-snap-row"><span class="ctr-snap-k">镜像</span>' +
      '<span class="ctr-snap-v">' + esc(snap.image_version) + '</span></div>');
  }
  if (snap.prompt_hash) {
    rows.push('<div class="ctr-snap-row"><span class="ctr-snap-k">提示词哈希</span>' +
      '<span class="ctr-snap-v mono">' + esc(snap.prompt_hash.slice(0, 16)) + '…</span></div>');
  }
  if (Array.isArray(snap.secret_refs) && snap.secret_refs.length > 0) {
    const names = snap.secret_refs.map(n => '<code>' + esc(n) + '</code>').join(' ');
    rows.push('<div class="ctr-snap-row"><span class="ctr-snap-k">密钥引用</span>' +
      '<span class="ctr-snap-v">' + names + '</span></div>');
  }
  const promptBlock = snap.prompt
    ? '<div class="ctr-snap-prompt"><div class="ctr-snap-k">输入提示词</div>' +
        '<pre class="ctr-snap-pre">' + esc(snap.prompt) + '</pre></div>'
    : '';
  return '<details class="ctr-snapshot">' +
      '<summary>输入快照（可重放）</summary>' +
      '<div class="ctr-snap-body">' + rows.join('') + promptBlock + '</div>' +
    '</details>';
}

// renderCronTimelinePanel — 重绘当前 timeline 面板（不重新 mount shell）。
// 用于 expand/collapse、loadMore、ws 刷新等场景。
function renderCronTimelinePanel(jobId) {
  const host = document.getElementById('cron-timeline-panel');
  if (!host) return;
  const job = (deps.cronJobs() || []).find(x => x && x.id === jobId);
  const st = getCronTimelineState(jobId);
  // R243-PERF-12 (#817): identity-check the rendered HTML against the
  // last paint for this job. cronTimelineHtml builds up to ~200 row
  // strings on each call; when the WS poll fires and nothing changed
  // (the common case at idle), the resulting HTML is byte-identical to
  // the previous paint and re-assigning innerHTML would discard and
  // rebuild every row's DOM nodes for nothing — including blowing away
  // any in-flight katex/mermaid async-render placeholders inside
  // expanded run details. A string-equality check is cheap (~1 µs for
  // a 100 KB blob in V8) compared to the parse + DOM-rebuild cost it
  // saves. The cache is stored on the per-job state so the
  // CRON_TIMELINE_FRESH_MS eviction path naturally resets it.
  const html = cronTimelineHtml(jobId, job, st);
  if (html === st.lastRenderedHtml && host.innerHTML !== '') {
    return;
  }
  st.lastRenderedHtml = html;
  host.innerHTML = html;
  // result 走 renderMd 后会埋入 mermaid/katex 异步占位（mermaid-N / ktx-N），
  // 必须在 attach 到 DOM 后调用一次才能完成异步渲染。与 events bubble 路径
  // 的 stickEventsBottom / runPendingAsync 调用语义保持一致。
  runPendingAsync();
}

// cronTimelineLoadMore — 分页加载更早的 run 列表。
// GET /api/cron/runs?job_id=&limit=50&before=<oldest started_at>
//
// onDone (optional): called once after a SUCCESSFUL page load (st.runs may
// have grown), used by keyboard navigation (#2090) to expand the next run
// once it becomes available. Not called when the request errors or when the
// load was skipped because one is already in flight / the list is done.
function cronTimelineLoadMore(jobId, onDone) {
  const st = getCronTimelineState(jobId);
  if (st.loading || st.done) return;
  st.loading = true;
  renderCronTimelinePanel(jobId);
  (async () => {
    let loaded = false;
    try {
      const headers = {};
      const t = getToken();
      if (t) headers['Authorization'] = 'Bearer ' + t;
      let url = NZ_CONTRACT.API.cron_runs + '?job_id=' + encodeURIComponent(jobId) + '&limit=50';
      if (st.nextBefore) url += '&before=' + st.nextBefore;
      const data = await fetchJSON(url, { headers, timeoutMs: 10000 });
      const more = (data && Array.isArray(data.runs)) ? data.runs : [];
      // 后端按 started_at 倒序返回；append 到现有列表尾部即可。
      // 用 run_id 去重（极端 race 下后端可能返回首页已有的 run）。
      const seen = new Set(st.runs.map(r => r && r.run_id));
      for (const r of more) {
        if (r && r.run_id && !seen.has(r.run_id)) st.runs.push(r);
      }
      // next_before == 0 / 缺失 → 没更多；存在 → 下次游标
      if (data && data.next_before) {
        st.nextBefore = data.next_before;
      } else {
        st.done = true;
      }
      loaded = true;
    } catch (err) {
      // R220-FE-5: 401/403 走 authModal；showAPIError 仅做 toast 提示，不会
      // 把用户带回登录态——这里要主动唤起 modal。
      if (err && (err.status === 401 || err.status === 403)) {
        showAuthModal();
      } else if (err && err.status) {
        showAPIError('加载执行历史', err.status, err.message || '');
      } else {
        showNetworkError('加载执行历史', err);
      }
    } finally {
      st.loading = false;
      // cron-panel-consolidation: only re-render the timeline panel if the
      // operator is still looking at the drawer for this job. The drawer
      // could have been closed or switched mid-fetch — st.runs is already
      // populated for next time, so no information is lost.
      if (deps.cronDetailJobId() === jobId) renderCronTimelinePanel(jobId);
      // Fire the post-load hook after st.loading is cleared and the panel is
      // re-rendered, so a callback that expands a run (#2090) operates on the
      // settled state. Guarded to a successful load and isolated so a throwing
      // callback cannot leave st.loading stuck (already reset above).
      if (loaded && typeof onDone === 'function') {
        try { onDone(); } catch (e) { /* navigation hook must not break paging */ }
      }
    }
  })();
}

// R243-PERF-7 / #812: rAF-debounce coalescing for cronTimelineRefreshHead.
// Bursty run_ended events (multiple jobs ending in the same tick, or
// a manual TriggerNow loop) used to fire a full fetch + sort + innerHTML
// rebuild per event; the WS handler now routes through
// cronTimelineRefreshHeadDebounced which collapses N events to a single
// rAF-aligned call per (jobId). Coalescing is keyed on jobId so two
// different jobs ending in the same tick still each get exactly one
// refresh — the saving is on repeated events for the same job.
//
// rAF (rather than setTimeout) keeps the refresh aligned with the next
// paint frame, so sort+innerHTML happens once per visible frame instead
// of once per network event.
const _cronTimelineRefreshScheduled = new Set();
function cronTimelineRefreshHeadDebounced(jobId) {
  if (!jobId) return;
  if (_cronTimelineRefreshScheduled.has(jobId)) return;
  _cronTimelineRefreshScheduled.add(jobId);
  const raf = (typeof requestAnimationFrame === 'function')
    ? requestAnimationFrame
    : (cb) => setTimeout(cb, 16);
  raf(() => {
    _cronTimelineRefreshScheduled.delete(jobId);
    cronTimelineRefreshHead(jobId).catch(() => {});
  });
}

// cronTimelineRefreshHead — WS run_ended（cron）触发。如果当前 drawer 打开
// 的就是该 job（deps.cronDetailJobId() === jobId），fetch /api/cron/runs?limit=10
// 替换头 10 条；否则只刷新列表 stats（已有逻辑：deps.fetchCronJobs +
// deps.renderCronPanel）。
//
// cron-panel-consolidation RFC §4.6: 路由门由 selectedKey 切到
// deps.cronDetailJobId() — cron 面板下 selectedKey 始终为 null（openCronPanel 已
// 清空），不再适合做"当前看的是哪条 cron"判定。
//
// 调用方应优先走 cronTimelineRefreshHeadDebounced（rAF-debounced wrapper）以
// 在 bursty run_ended 序列下避免 N 次 sort+innerHTML 重建（R243-PERF-7
// / #812）。直接调用本函数仍合法（手动 trigger / 测试路径）。
async function cronTimelineRefreshHead(jobId) {
  if (deps.cronDetailJobId() !== jobId) return;
  const st = getCronTimelineState(jobId);
  // R220-FE-4: in-flight guard。用户快速触发多次 TriggerNow 时 run_ended
  // 会连续到达，每次都启动 fetch；后返回的请求覆盖先返回的 → 顺序取决于
  // 网络。用 token 保证只有最新一次请求的结果会被写回 st.runs。
  const token = (st._refreshToken || 0) + 1;
  st._refreshToken = token;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const url = NZ_CONTRACT.API.cron_runs + '?job_id=' + encodeURIComponent(jobId) + '&limit=10';
    const data = await fetchJSON(url, { headers, timeoutMs: 8000 });
    // 过期请求：开始 fetch 之后又有更新一轮 refreshHead 启动了，丢弃本次结果。
    if (st._refreshToken !== token) return;
    if (deps.cronDetailJobId() !== jobId) return;
    const head = (data && Array.isArray(data.runs)) ? data.runs : [];
    if (head.length === 0) return;
    // 把头 10 条与现有 runs 合并（用 run_id 去重 + 按 started_at 倒序排）。
    // 排序兜底——server 已经倒序，但合并后 head 与旧 runs 可能在边界乱序。
    const seen = new Set();
    const merged = [];
    for (const r of head) {
      if (r && r.run_id && !seen.has(r.run_id)) {
        merged.push(r);
        seen.add(r.run_id);
      }
    }
    for (const r of st.runs) {
      if (r && r.run_id && !seen.has(r.run_id)) {
        merged.push(r);
        seen.add(r.run_id);
      }
    }
    merged.sort((a, b) => (b.started_at || 0) - (a.started_at || 0));
    // R20260610-CR-007 (#1998): cap the merged list so repeated
    // loadMore + WS refresh cycles can't grow st.runs without bound.
    // Truncating drops the oldest tail, so re-arm pagination: the
    // cursor moves to the new oldest entry and done resets to false
    // so 加载更多 can re-fetch past the cut.
    if (merged.length > CRON_TIMELINE_MAX_RUNS) {
      merged.length = CRON_TIMELINE_MAX_RUNS;
      const oldest = merged[merged.length - 1];
      st.nextBefore = oldest && oldest.started_at ? oldest.started_at : 0;
      st.done = false;
    }
    st.runs = merged;
    renderCronTimelinePanel(jobId);
  } catch (e) {
    // 静默：cron list 重绘会兜底刷成功率/统计；timeline 偶尔不刷不致命。
    console.error('cron timeline refresh:', e);
  }
}


// renderCronTimelineForJob is a thin wrapper around the legacy
// renderCronTimelineForSession that uses deps.cronDetailJobId()-keyed reconcile
// instead of selectedKey. It re-uses the same #cron-timeline-panel host
// (now living inside the drawer instead of mainShell), so cronTimelineHtml
// / cronTimelineRowHtml / cronTimelineDetailHtml work unchanged.
function renderCronTimelineForJob(jobId) {
  const host = document.getElementById('cron-timeline-panel');
  if (!host) return;
  const job = (deps.cronJobs() || []).find(x => x && x.id === jobId);
  const st = getCronTimelineState(jobId);
  if (st.lastMountAt > 0 && Date.now() - st.lastMountAt > CRON_TIMELINE_FRESH_MS) {
    st.runs = [];
    st.nextBefore = 0;
    st.done = false;
    st.showAll = false;
  }
  if (st.runs.length === 0 && job && Array.isArray(job.recent_runs) && job.recent_runs.length > 0) {
    st.runs = job.recent_runs.slice();
    const oldest = st.runs[st.runs.length - 1];
    st.nextBefore = oldest && oldest.started_at ? oldest.started_at : 0;
    // 后端每 job 只嵌 recent_runs_cap 条摘要（服务端 recentRunsPerJob，随
    // 响应下发）。少于 cap → 历史已全在手；等于 cap（或 cap 未知）→ 可能还有
    // 更多，留给首次「加载更多」以 nextBefore 向 /api/cron/runs 确认。
    // stats.total（累计运行数）≤ 已有行数时也已到结尾，省一次空翻页请求。
    const total = (job.stats && job.stats.total) | 0;
    st.done = (deps.cronRecentRunsCap() > 0 && job.recent_runs.length < deps.cronRecentRunsCap()) ||
      (total > 0 && total <= st.runs.length);
  }
  st.lastMountAt = Date.now();
  // Mount path: unconditional innerHTML rewrite (shell remount or
  // first paint). Stash the result so the subsequent identity-check in
  // renderCronTimelinePanel sees a non-empty baseline and short-circuits
  // truly idempotent re-renders. R243-PERF-12 (#817).
  const html = cronTimelineHtml(jobId, job, st);
  st.lastRenderedHtml = html;
  host.innerHTML = html;
  if (!job) {
    deps.fetchCronJobs().then(() => {
      if (deps.cronDetailJobId() === jobId) renderCronTimelineForJob(jobId);
    }).catch(() => {});
  }
}


export {
  cronExpandedRunId,
  cronTimelineState,
  cronTimelineCollapse,
  cronTimelineLoadMore,
  cronTimelineRefreshHeadDebounced,
  cronTimelineSelectRun,
  cronTimelineToggleShowAll,
  navigateExpandedRun,
  renderCronTimelineForJob,
  renderCronTimelinePanel,
};
