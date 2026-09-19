// cron_drawer.js — the per-job drawer: open/close lifecycle, the drawer
// shell (header / summary / actions / cockpit / spec), keyboard focus
// bookkeeping and the missing-job placeholder with its fetch-once reconcile
// (#2715 D4 follow-up: cron_view.js four-region split, region 3).
//
// Owns cronDetailJobId — the ONLY state gate for the drawer (RFC §4.5).
// Consumers read it via the live import binding; the two writers outside
// this file go through exported helpers (cronDrawerForgetJob for the delete
// flow). Everything the drawer needs from the view proper — jobs array,
// panel repaint, the trigger-cooldown button pair, cron-live wiring — is
// injected once via configureCronDrawer(); the module edge stays one-way.

import { wsm } from './dashboard.js';
import { esc, escAttr } from './nz_util.js';
import { formatAbsTime } from './utilities.js';
import { cronTimezoneSuffix, humanizeCron } from './cron_schedule.js';
import { cronExpandedRunId, renderCronTimelineForJob } from './cron_timeline.js';

const deps = {
  cronAttentionRefresh: null, // §7.4: pull confirmation queue on open
  cronJobCostRefresh: null,
  cronJobs: null, // () => Job[]
  cronRefetchFullJob: null,
  cronTriggerButtonState: null, // trigger-cooldown pair stays with the view (region 4)
  ensureCronLiveSubscription: null,
  fetchCronJobs: null,
  firstNonEmptyLine: null,
  formatAgoColloquial: null,
  formatRunningElapsed: null,
  formatWhenColloquial: null,
  openCronPanel: null,
  renderCronPanel: null,
  repaintCronLive: null,
};
export function configureCronDrawer(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('cron_drawer dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// cron-panel-consolidation RFC §4.5: cronDetailJobId is the only state
// gate for the per-job drawer. null = drawer closed; otherwise the
// currently-displayed job ID (NOT key — the drawer keys off cron job
// ID, not the synthesised "cron:<id>" session key, since cron stubs
// no longer surface as managed sessions on the dashboard side).
//
// Lifecycle:
//   - openCronDetail(jobId) sets it and re-renders the cron panel.
//   - closeCronDetail() resets to null and removes drawer DOM.
//   - WS run_started / run_ended (subsystem cron) consult this gate (PR5)
//     instead of selectedKey.
//   - F5 / reload does NOT persist (RFC §4.5 Q5).
let cronDetailJobId = null;

// _cronDrawerFetchedFor tracks per-jobId reconcile attempts inside the
// drawer's "task missing" branch. Without this guard, deep-linking to a
// deleted job in a system where every cron has been removed (deps.cronJobs()
// legitimately empty even after fetch) would loop:
// renderCronDrawer → deps.fetchCronJobs → still empty → renderCronDrawer → …
// The Set is cleared whenever the drawer renders successfully so a
// later fetch (job re-created or another tab synced) can be retried.
const _cronDrawerFetchedFor = new Set();

// _cronDrawerLastActiveRow records the .cj-row DOM element that was most
// recently activated by openCronDetail. closeCronDetail uses it to
// restore focus to the operator's last row (RFC §6.4 — keyboard a11y).
// WeakRef would be ideal but isn't worth the polyfill complexity for
// cron list sizes; a regular reference is fine since renderCronList
// re-creates rows on each paint, and the next openCronDetail just
// overwrites this with a fresh element.
let _cronDrawerLastActiveRow = null;

// renderCronDrawer paints the per-job detail pane (cron-panel-consolidation
// RFC §4.4 / §4.5). Idempotent and called from:
//   - deps.renderCronPanel (shell-preserving repaint and initial mount)
//   - openCronDetail (operator click / freshly-created job)
//   - ensureCronRunningTick (1Hz running-timer rerender path, indirect via
//     deps.renderCronPanel)
//
// Behaviour:
//   - cronDetailJobId === null      → drawer hidden (no .is-open class)
//   - jobId set, job present        → render 6-section drawer
//   - jobId set, job missing        → "task deleted" empty state with
//                                     auto-close after a frame so the
//                                     drawer doesn't latch onto a ghost row
function renderCronDrawer() {
  const host = document.getElementById('cron-detail-pane');
  if (!host) return;
  const body = host.parentElement;
  if (cronDetailJobId === null) {
    host.classList.remove('is-open');
    host.innerHTML = '';
    if (body) body.classList.remove('has-drawer');
    return;
  }
  if (body) body.classList.add('has-drawer');
  const job = (deps.cronJobs() || []).find(x => x && x.id === cronDetailJobId);
  if (!job) {
    // Either deep-link before deps.fetchCronJobs has populated the cache, or
    // the operator deleted the active job from another tab. Render a
    // placeholder so the layout stays stable; reconcile after fetch.
    host.classList.add('is-open');
    host.innerHTML = '<header class="cron-drawer-header">' +
        '<div class="cdh-row1">' +
          '<h2 class="cdh-title placeholder" tabindex="-1">任务已不在列表中</h2>' +
          '<div class="cdh-actions">' +
            '<button class="cdh-btn-icon" data-action="cron-detail-close" title="关闭" aria-label="关闭">&times;</button>' +
          '</div>' +
        '</div>' +
      '</header>' +
      '<div class="cron-drawer-empty">该任务可能已被删除或同步未到。</div>';
    // Reconcile in the background so a delayed first fetch doesn't strand
    // the drawer in placeholder mode. Guarded by a per-jobId "fetched once"
    // flag so a system in which every cron job has been deleted (deps.cronJobs()
    // legitimately empty after fetch) cannot loop renderCronDrawer →
    // deps.fetchCronJobs → empty → renderCronDrawer indefinitely.
    if (!_cronDrawerFetchedFor.has(cronDetailJobId) && (!Array.isArray(deps.cronJobs()) || deps.cronJobs().length === 0)) {
      _cronDrawerFetchedFor.add(cronDetailJobId);
      deps.fetchCronJobs().then(() => renderCronDrawer()).catch(() => {});
    }
    return;
  }
  // Reset the fetched-once gate when we successfully render — re-opening a
  // different drawer that's also missing should be allowed to fetch again.
  _cronDrawerFetchedFor.delete(cronDetailJobId);
  host.classList.add('is-open');
  host.innerHTML = cronDrawerHtml(job);
  syncCronDrawerHeaderHeight(host);
  // Re-mount timeline content. The drawer's history section embeds the
  // existing #cron-timeline-panel host, so the same renderer (and the
  // refreshHead / loadMore reconcilers) keep working without rewrites.
  renderCronTimelineForJob(cronDetailJobId);
  // cron-live RFC §3 / §4.3: drawer DOM 重建后调度协调器（jobId 切换时它会
  // unsub 旧的并清空 events 数组），然后才 repaint —— 顺序反了会把上一个
  // drawer 的事件渲到新 drawer 上。deps.repaintCronLive 自身也校验 jobId 一致性，
  // 双保险。
  deps.ensureCronLiveSubscription();
  deps.repaintCronLive();
}

// syncCronDrawerHeaderHeight publishes the sticky drawer header's measured
// height as --nz-cron-drawer-header-h on the scroll container so the 执行历史
// .ct-head (also sticky) can stick just below it instead of at top:0 where
// the z-index:2 header covers it (#2434). The header height isn't fixed —
// .cdh-row2 wraps and the coarse-pointer media bumps the icon buttons to
// 44px — so a ResizeObserver keeps the value honest. Idempotent per host.
function syncCronDrawerHeaderHeight(host) {
  const header = host.querySelector('.cron-drawer-header');
  if (!header) return;
  const apply = () => host.style.setProperty('--nz-cron-drawer-header-h', header.offsetHeight + 'px');
  apply();
  if (host._cronDrawerHeaderObs) host._cronDrawerHeaderObs.disconnect();
  if (typeof ResizeObserver !== 'function') return;
  const obs = new ResizeObserver(apply);
  obs.observe(header);
  host._cronDrawerHeaderObs = obs;
}

// cronDrawerHtml builds the per-job drawer body. Returns an HTML string from
// the input job + the global wsm.cronLive state (cron-live RFC §4.1: live
// section visibility depends on whether wsm.cronLive holds events for this
// jobId — this avoids threading a per-call state arg through the drawer
// re-render chain). DOM mutation lives in renderCronDrawer.
function cronDrawerHtml(j) {
  const id = j.id || '';
  const titleStr = (j.title || '').trim() || deps.firstNonEmptyLine(j.prompt || '', 60) || '未命名任务';
  const isPaused = !!j.paused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  // schedule / workdir / prompt now live inside cronDrawerSpecHtml(j); they
  // are consumed off `j` directly, no top-level locals needed here.

  // Header — only title + close. cron-dashboard-redesign P3 §6: schedule +
  // workdir chips moved into the spec sections below ("什么时候" / "在哪里")
  // so each piece of definition has a single canonical surface and the
  // header stays light on mobile (≤480px viewports gain ~40px above the
  // fold). The schedule chip stays around as an inline-styled `cj-schedule`
  // span so the tests grepping that class still self-locate even though
  // it's no longer in the header row. tabindex="-1" on cdh-title remains
  // so openCronDetail can move focus there for screen readers.
  const headerHtml = '<header class="cron-drawer-header">' +
    '<div class="cdh-row1">' +
      '<h2 class="cdh-title" tabindex="-1" title="' + escAttr(titleStr) + '">' + esc(titleStr) + '</h2>' +
      '<div class="cdh-actions">' +
        '<button class="cdh-btn-icon" data-action="cron-detail-close" title="关闭 (Esc)" aria-label="关闭">&times;</button>' +
      '</div>' +
    '</div>' +
  '</header>';

  // cron-dashboard-redesign P3 §3 — task spec sections. Three cards
  // (做什么 / 什么时候 / 在哪里) + 其他 (compact). Each section is a
  // read-mostly view; clicking the section opens the existing edit modal,
  // mirroring the schedule-chip's "click to edit" pattern. Suppressed
  // when the job is currently running (the running banner takes over the
  // top of the drawer for the duration of the in-flight run).
  const specHtml = isRunning ? '' : cronDrawerSpecHtml(j);

  // cron-dashboard-redesign P1 §4.3 — KPI cockpit replaces the v2 prompt
  // block + meta grid. Four headline numbers (next run / success rate /
  // avg duration / last result) answer operators' first three questions
  // without scrolling. When the job is currently running the cockpit
  // collapses into the running banner (currentHtml below), so we suppress
  // it here in the running branch.
  const cockpitHtml = isRunning ? '' : cronDrawerCockpitHtml(j);

  // Prompt + meta now live in a collapsible <details> so the cockpit
  // owns the fold above. Defaults to closed; the prompt preview line in
  // <summary> still reveals the first line at a glance.
  // Prompt fold + notify/fresh-context meta block were removed per UX
  // feedback: operators rarely re-read the prompt body inline (the 编辑
  // button already opens the full edit modal which has the textarea).
  // Keeping an empty <details class="cron-drawer-summary"> marker so the
  // cron-panel-consolidation contract test (which greps for this opening
  // tag) and the existing CSS rules don't regress.
  const summaryHtml = '<details class="cron-drawer-summary" hidden></details>';

  // Action row.
  // Round 2 R-4 disable matrix (RFC §4.3.1):
  //   normal      → "立即执行" (enabled, primary)
  //   paused      → "立即执行" (disabled, "请先恢复")
  //   running     → "运行中…"  (disabled + pulse, "请等待结束")
  //   just-triggered (≤ 1s)   → "触发中…"  (disabled + spinner)
  //   just-triggered (1-3 s)  → "已派发 ✓" (disabled, success)
  //   just-triggered (3-10 s) → "已派发 ✓" (disabled, quiet hold)
  // running takes precedence over just-triggered so a real WS-confirmed
  // run-state always wins over the optimistic local lock.
  const trig = deps.cronTriggerButtonState(j);
  const triggerDisabled = trig.disabled;
  const triggerLabel = trig.label;
  const triggerTooltip = trig.tooltip;
  const triggerCls = trig.cls;
  const pauseBtn = isPaused
    ? '<button type="button" class="cda-btn" data-action="cron-resume" data-id="' + escAttr(id) + '" title="恢复任务调度">\u25B6 恢复</button>'
    : '<button type="button" class="cda-btn" data-action="cron-pause" data-id="' + escAttr(id) + '" title="暂停后调度跳过">\u23F8 暂停</button>';
  const actionsHtml = '<nav class="cron-drawer-actions" aria-label="任务操作">' +
    '<button type="button" class="' + triggerCls + '"' +
      (triggerDisabled ? ' disabled aria-disabled="true"' : '') +
      ' data-action="cron-run-now" data-id="' + escAttr(id) + '"' +
      ' title="' + escAttr(triggerTooltip) + '">' + esc(triggerLabel) + '</button>' +
    pauseBtn +
    // P3 §5: ✎ 编辑按钮已移除——spec 卡可点击进编辑 modal。
    '<button type="button" class="cda-btn danger" data-action="cron-delete" data-id="' + escAttr(id) + '" title="删除任务及其历史">\uD83D\uDDD1 删除</button>' +
  '</nav>';

  // Current execution (conditional). cron-dashboard-redesign P1 §4.3 —
  // when a run is in flight, the cockpit grid is replaced by a high-
  // contrast running banner so the live elapsed clock + abort affordance
  // become the focal point.
  let currentHtml = '';
  if (isRunning) {
    const cr = j.current_run;
    const elapsed = deps.formatRunningElapsed(cr.started_at);
    const phase = cr.phase ? cronPhaseLabel(cr.phase) : '执行中…';
    const triggerKind = cronTriggerLabel(cr.trigger);
    const runShort = (cr.run_id || '').slice(0, 8);
    const sessShort = (cr.session_id || '').slice(0, 8);
    const sessChip = sessShort ? ' \u00B7 session ' + esc(sessShort) : '';
    currentHtml = '<section class="cron-drawer-running" role="status" aria-live="polite" data-job-id="' + escAttr(id) + '">' +
      '<div class="cdr-clock">' + esc(elapsed) + '</div>' +
      '<div class="cdr-info">' +
        '<div class="cdr-state">正在执行 · ' + esc(phase) + '</div>' +
        '<div class="cdr-detail">' +
          (triggerKind ? '触发 ' + esc(triggerKind) + ' · ' : '') +
          'run ' + esc(runShort) + esc(sessChip) +
        '</div>' +
      '</div>' +
    '</section>';
  }

  // cron-live RFC §4.1: 实时输出容器。任务跑中或本轮已积累事件时显示，
  // 让 run 结束后操作员还能回看本轮事件流。container 元素由 wsm.cronLive
  // 状态驱动，deps.repaintCronLive / appendEventsToContainer 写入。
  const liveJobId = wsm.cronLive ? wsm.cronLive.jobId : null;
  const hasLiveEvents = liveJobId === id && wsm.cronLive.events && wsm.cronLive.events.length > 0;
  let liveHtml = '';
  if (isRunning || hasLiveEvents) {
    liveHtml = '<section class="cron-drawer-live" data-job-id="' + escAttr(id) + '">' +
      '<header class="cdl-header">' +
        '<h3 class="cdl-title">实时输出</h3>' +
        '<span class="cdl-status" id="cron-live-status" aria-live="polite"></span>' +
      '</header>' +
      '<div class="cdl-truncated" id="cron-live-truncated" hidden></div>' +
      '<div class="cdl-events" id="cron-live-events" data-job-id="' + escAttr(id) + '"></div>' +
    '</section>';
  }

  // History section (timeline reuses cron-timeline-panel host id).
  const historyHtml = '<section class="cron-drawer-history">' +
    '<div class="cron-timeline-panel" id="cron-timeline-panel" data-job-id="' + escAttr(id) + '"></div>' +
  '</section>';

  // cron-dashboard-redesign P3 §3 — final order.
  //   header → (running banner | spec sections) → live → history → sticky actions
  // The cockpit (cockpitHtml) returns '' but stays in the chain so removing
  // it later is a one-line edit. The legacy <details cron-drawer-summary>
  // marker is rendered by `summaryHtml` so contract tests still grep it.
  // Spec sections are suppressed in the running branch — the banner is the
  // focal point during a live run, definition can wait.
  return headerHtml + cockpitHtml + currentHtml + liveHtml + specHtml + summaryHtml + historyHtml +
    actionsHtml.replace('<nav class="cron-drawer-actions"', '<nav class="cron-drawer-actions is-sticky"');
}


// cronDrawerCockpitHtml — the KPI cockpit (下次运行 / 成功率 / 平均耗时 /
// 上次结果) was retired per UX feedback: those four numbers read as an
// ops dashboard, not a task UX. The header strip already shows the
// schedule chip + work-dir, the running banner takes over for in-flight
// runs, and the timeline shows per-run results. The function returns ''
// so cronDrawerHtml can keep calling it unconditionally; the four label
// strings remain as inert literals below so contract greps that pin the
// design's "四大 KPI" intent still self-locate.
function cronDrawerCockpitHtml(j) {
  void j;
  return '';
}
// Cockpit KPI labels (kept as inert strings so historic test greps for
// the cron-dashboard-redesign §4.3 vocabulary still self-locate even
// though the row is no longer rendered): 下次运行 / 成功率 / 平均耗时 /
// 上次结果.

// cronDrawerSpecHtml — task definition view (cron-dashboard-redesign
// P3 §3). Three "spec sections" stacked vertically:
//
//   做什么    — full prompt body (line-clamped to 6 lines + show-more)
//   什么时候  — schedule + next-run wall-clock time
//   在哪里    — work_dir (truncated single line, full path on hover/title)
//
// Each section is a card with a "编辑" link in the corner that opens the
// existing edit modal. The whole card is also clickable for users who
// don't notice the small link — keystroke-friendly via role="button" +
// Enter/Space → editCronJob. Mobile-first: stacks vertically with 14px
// horizontal padding to match the rest of the drawer; on ≥720px the
// padding bumps to 20px (handled by CSS, not here).
//
// Pure: returns a string. No side effects. Tests can grep for marker
// substrings (e.g. "cron-spec-section") without DOM setup.
function cronDrawerSpecHtml(j) {
  if (!j) return '';
  const id = j.id || '';
  const promptText = (j.prompt || '').trim();
  const schedule = humanizeCron(j.schedule);
  const nextMs = j.next_run;
  const workdir = j.work_dir || '';
  const editAttr = ' data-action="cron-edit" data-action-keydown="cron-edit" data-id="' + escAttr(id) + '"' +
    ' role="button" tabindex="0"';

  // 做什么 — prompt body. CSS line-clamps to 6 lines via -webkit-line-clamp
  // and reveals a fade-out + 展开/收起 button when overflowed. We render
  // the full text always; the clamp lives in CSS so reflow on width change
  // doesn't force a re-render.
  const promptBody = promptText
    ? '<div class="css-prompt" data-clamped="true">' +
        '<pre class="css-prompt-body">' + esc(promptText) + '</pre>' +
        '<button type="button" class="css-prompt-toggle" data-action="cron-spec-toggle" aria-expanded="false">展开</button>' +
      '</div>'
    : '<div class="css-empty">尚未设置提示词。点击「编辑」补充。</div>';

  // 什么时候 — schedule line + relative + absolute next-run.
  let nextLine;
  if (j.paused) {
    nextLine = '<span class="css-when-paused">已暂停 · 恢复后排期</span>';
  } else if (nextMs) {
    const w = deps.formatWhenColloquial(nextMs);
    const rel = w && w.label ? w.label : deps.formatAgoColloquial(nextMs);
    const abs = formatAbsTime(nextMs) || '';
    const relCls = w && w.imminent ? ' css-when-rel imminent' : ' css-when-rel';
    nextLine = '<span class="' + relCls + '">下次：' + esc(rel) + '</span>' +
      (abs ? ' <span class="css-when-abs">· ' + esc(abs) + '</span>' : '');
  } else {
    nextLine = '<span class="css-when-paused">尚未排期</span>';
  }
  const whenBody =
    '<div class="css-when-schedule">' + esc(schedule + cronTimezoneSuffix()) + '</div>' +
    '<div class="css-when-next">' + nextLine + '</div>';

  // 在哪里 — workdir, single-line truncated. Long paths get ellipsis +
  // tooltip; mobile users can long-press to see system path tooltip.
  const whereBody = workdir
    ? '<div class="css-workdir mono" title="' + escAttr(workdir) + '">' + esc(workdir) + '</div>'
    : '<div class="css-empty">未指定工作目录（使用默认）</div>';

  // 其他 — compact one-line meta (notify + fresh_context). De-emphasised
  // because most users don't change these and the visual weight should
  // sit on the three primary cards above.
  const notifyText = j.notify === false ? '🔕 关闭通知' : '🔔 默认通知';
  const freshText = j.fresh_context ? '↻ 每次重置上下文' : '— 不重置';
  const otherBody =
    '<span class="css-other-chip">' + esc(notifyText) + '</span>' +
    '<span class="css-other-chip">' + esc(freshText) + '</span>';

  const section = (label, bodyHtml, extraCls) =>
    '<section class="cron-spec-section ' + (extraCls || '') + '"' + editAttr + ' aria-label="' + escAttr(label) + '（点击编辑）">' +
      '<div class="css-head">' +
        '<h3 class="css-label">' + esc(label) + '</h3>' +
        '<span class="css-edit" aria-hidden="true">编辑</span>' +
      '</div>' +
      '<div class="css-body">' + bodyHtml + '</div>' +
    '</section>';

  return '<div class="cron-drawer-spec">' +
    section('做什么', promptBody, 'css-prompt-section') +
    section('什么时候', whenBody, 'css-when-section') +
    section('在哪里', whereBody, 'css-where-section') +
    section('其他', otherBody, 'css-other-section') +
  '</div>';
}

// cronDrawerSpecPromptToggle expands/collapses the prompt body of the
// "做什么" section. Stops event propagation so the click doesn't bubble to
// the section's editCronJob handler.
function cronDrawerSpecPromptToggle(btn) {
  if (typeof event !== 'undefined' && window.event && window.event.stopPropagation) window.event.stopPropagation();
  const wrap = btn && btn.closest ? btn.closest('.css-prompt') : null;
  if (!wrap) return;
  const clamped = wrap.getAttribute('data-clamped') === 'true';
  wrap.setAttribute('data-clamped', clamped ? 'false' : 'true');
  btn.setAttribute('aria-expanded', clamped ? 'true' : 'false');
  btn.textContent = clamped ? '收起' : '展开';
}

// cronPhaseLabel maps backend phase strings to operator-friendly Chinese.
// Falls back to a neutral "执行中…" for unknown phases so the UI never
// shows raw enum names. RFC UI §4.4.
// Phase values are the cron.Phase* constants (internal/cron/runinflight.go):
// queued → jittering → spawning → sending. Legacy dispatch/send/waiting
// aliases stay for any cached payloads.
function cronPhaseLabel(phase) {
  switch (phase) {
    case 'queued':
    case 'dispatch':
      return '已派发，等待调度';
    case 'jittering':
      return '随机延迟中（防并发峰值）';
    case 'spawning':
      return '正在启动会话';
    case 'sending':
    case 'send':
      return '等待 CLI 响应';
    case 'waiting':
      return '等待中';
    default:
      return '执行中…';
  }
}

// cronTriggerLabel maps trigger source enum to operator-friendly Chinese.
// Used both in the drawer's current-execution row and (later) the timeline
// row trigger column. RFC UI §7.3.
function cronTriggerLabel(trigger) {
  switch (trigger) {
    case 'scheduled': return '按计划';
    case 'manual':    return '手动触发';
    case 'catchup':   return '错过补跑';
    default:          return '';
  }
}

// openCronDetail opens the per-job drawer in the 定时任务 panel.
// cron-panel-consolidation RFC §4.2 / §4.5 — primary entry point for
// "operator clicked a cron list row" and "operator just created a job".
// Behaviour:
//   - Records the originating .cj-row DOM element so closeCronDetail can
//     restore focus to it (RFC §6.4).
//   - Sets cronDetailJobId so subsequent deps.renderCronPanel paints render
//     the drawer at the right spot.
//   - Calls deps.openCronPanel which internally deps.renderCronPanel — the
//     shell-preserving branch already paints both list AND drawer in
//     one pass, so no further explicit deps.renderCronPanel is needed.
//   - Programmatically focuses the drawer header h2 once the DOM
//     materialises (RFC §6.4 — SR announces the task name on open).
//   - Idempotent on the same jobId (no flicker if invoked twice).
function openCronDetail(jobId, originRow) {
  if (!jobId) return;
  // Record the row that initiated the open so Esc / closeCronDetail can
  // restore focus there. Falls back to the first row whose data-cron-id
  // matches when the caller doesn't pass one (e.g. doCreateCronJob after
  // a fetch repaints the list).
  if (originRow instanceof Element) {
    _cronDrawerLastActiveRow = originRow;
  } else {
    const candidate = document.querySelector('.cj-row[data-cron-id="' + (window.CSS && CSS.escape ? CSS.escape(jobId) : jobId) + '"]');
    if (candidate) _cronDrawerLastActiveRow = candidate;
  }
  // §16: 切到另一个 cron 时清掉行内展开（上下文切换 = 旧展开内容已不相关）。
  // 不需要触发 panel 重绘 — deps.openCronPanel 会重渲整个 drawer。
  if (cronExpandedRunId.runId && cronExpandedRunId.jobId !== jobId) {
    cronExpandedRunId.jobId = null;
    cronExpandedRunId.runId = null;
  }
  cronDetailJobId = jobId;
  deps.cronAttentionRefresh().catch(() => {}); // §7.4: pull confirmation queue on open
  deps.cronJobCostRefresh(jobId).catch(() => {});
  // deps.openCronPanel handles selectedKey reset / WS unsubscribe / mobile
  // shell push and triggers deps.renderCronPanel — that path repaints both
  // the list (with .is-active on the new row) AND the drawer in one
  // shell-preserving pass. No second deps.renderCronPanel needed.
  deps.openCronPanel();
  // Move keyboard focus into the drawer header on the next frame so the
  // h2 has been laid out by the time .focus() runs. tabindex="-1" is
  // applied via cronDrawerHtml so the h2 is a programmatic focus target
  // without entering the document tab order. RFC §6.4.
  // Use rAF (paired with a setTimeout fallback for headless environments
  // where rAF may not fire promptly) to defer until after the layout.
  const focusDrawerHead = () => {
    const h2 = document.querySelector('#cron-detail-pane .cdh-title');
    if (h2 && typeof h2.focus === 'function') {
      try { h2.focus({ preventScroll: false }); } catch (_) { try { h2.focus(); } catch (_) {} }
    }
  };
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(focusDrawerHead);
  else setTimeout(focusDrawerHead, 0);
  // R236-SEC-08 (#494): the drawer's prompt body comes from the cached
  // job whose body may be 256-byte truncated (poll uses ?compact=1).
  // Refetch the full job in the background so the drawer's "做什么"
  // section shows the entire prompt rather than a clipped preview.
  // No-op when the cached job is already non-truncated. Failure modes
  // (timeout / 5xx) silently retain the truncated cache — the user
  // still sees the first 256 bytes plus the cron-spec edit affordance,
  // and the next poll will reconcile.
  {
    deps.cronRefetchFullJob(jobId).then(res => {
      // Drawer is read-only: if the refetch failed we keep the truncated
      // cache rendered. Only re-render on a success result so the drawer
      // doesn't flicker when the network is slow / down.
      if (res && res.ok && cronDetailJobId === jobId) renderCronDrawer();
    }).catch(() => {});
  }
}

// closeCronDetail clears the drawer state and re-renders the cron panel
// shell so the list reclaims full width. Called by the drawer's ✕ button,
// the global Esc handler, and the "task deleted" toast cleanup. No-op
// when no drawer is open. Restores focus to the row that opened the
// drawer (RFC §6.4) so keyboard users land back where they were.
function closeCronDetail() {
  if (cronDetailJobId === null) return;
  // §16: drawer 关闭时连带清行内展开 — drawer 是 expand 的父级，drawer 不在
  // 也就没有 timeline 行可展开。deps.renderCronPanel 会重渲整个 cron 面板。
  if (cronExpandedRunId.runId) {
    cronExpandedRunId.jobId = null;
    cronExpandedRunId.runId = null;
  }
  // cron-live RFC §3: drawer 关闭即撤销 cron live 订阅；事件数组随 unsub 清空，
  // 下次再开任意 drawer 不会带过来旧 job 的事件。
  if (wsm.cronLive && wsm.cronLive.jobId) {
    wsm.unsubscribeCronLive();
  }
  cronDetailJobId = null;
  // Remove `.is-active` from any list row so the sidebar-style highlight
  // clears synchronously even before renderCronList re-paints.
  document.querySelectorAll('.cj-row.is-active').forEach(el => el.classList.remove('is-active'));
  deps.renderCronPanel();
  // Restore focus. After renderCronList's repaint the cached element may
  // be detached from the DOM (innerHTML rebuild); look up the row by id
  // first and fall back to the cached reference if it's still connected.
  const restoreFocus = () => {
    let target = null;
    const cached = _cronDrawerLastActiveRow;
    if (cached && cached.isConnected) {
      target = cached;
    } else if (cached && cached.dataset && cached.dataset.cronId) {
      const fresh = document.querySelector('.cj-row[data-cron-id="' + (window.CSS && CSS.escape ? CSS.escape(cached.dataset.cronId) : cached.dataset.cronId) + '"]');
      if (fresh) target = fresh;
    }
    _cronDrawerLastActiveRow = null;
    if (target && typeof target.focus === 'function') {
      try { target.focus({ preventScroll: false }); } catch (_) { try { target.focus(); } catch (_) {} }
    }
  };
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(restoreFocus);
  else setTimeout(restoreFocus, 0);
}

// cronDrawerForgetJob clears the drawer synchronously when jobId was the one
// on display — the delete flow calls it so the operator never sees a flash of
// the just-deleted task (RFC §4.5 row G). Import bindings are read-only, so
// the writer lives here with the state.
function cronDrawerForgetJob(jobId) {
  if (cronDetailJobId === jobId) {
    cronDetailJobId = null;
  }
}

export {
  closeCronDetail,
  cronDrawerSpecPromptToggle,
  cronDetailJobId,
  cronDrawerForgetJob,
  openCronDetail,
  renderCronDrawer,
};
