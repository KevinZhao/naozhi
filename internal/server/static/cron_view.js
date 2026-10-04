import { NZ_CONTRACT } from './contract.js';
import { selection, serverInfo, sessionList, timers, ui } from './state.js';
import { fetchCLIBackends, renderBackendPicker } from './backend_catalog.js';
import { setActivityView } from './dashboard.js';
import { authHeaders, getToken, lsGet, lsSet } from './platform.js';
import { sessionStream } from './session_stream.js';
import { wsm } from './ws_manager.js';
import {
  mobileBack,
} from './mobile_nav.js';
import {
  buildScheduleSection,
  freqMarkTouched,
  freqSelectMode,
  freqUpdate,
  humanizeCron,
  parseCronToFreq,
  cronTimezoneSuffix,
} from './cron_schedule.js';
import { cronAttentionConfirm, cronAttentionRefresh } from './cron_attention.js';
import { cronLive, ensureCronLiveSubscription, setCronLiveStatus } from './cron_live.js';
import { cronDrawerState, cronFrozenRuns, cronRefetchFullJob, cronRunClearedAtLocal, cronStore, cronTriggerCooldownClear, fetchCronJobs } from './cron_state.js';
import { cronErrorClassLabel, firstNonEmptyLine, formatAgoColloquial, formatRunningElapsed, formatWhenColloquial } from './cron_format.js';
import { registerShell } from './shell.js';
import {
  closeCronDetail,
  cronDrawerForgetJob,
  cronDrawerSpecPromptToggle,
  openCronDetail,
  renderCronDrawer,
} from './cron_drawer.js';
import {
  cronTriggerNow,
} from './cron_trigger.js';
import {
  cronExpandedRunId,
  cronTimelineState,
  cronTimelineCollapse,
  cronTimelineLoadMore,
  cronTimelineRefreshHeadDebounced,
  cronTimelineSelectRun,
  cronTimelineToggleShowAll,
  navigateExpandedRun,
  renderOpenCronTimeline,
} from './cron_timeline.js';
import {
  esc,
  escAttr,
  fetchJSON,
  nzBus,
  nzViews,
  reconcileChildren,
  registerActions,
  showToast,
  trapFocus,
  runStateDot,
  runStateLabel,
} from './nz_util.js';
import {
} from './render_md.js';
import {
  announce,
  confirmDialog,
  formatAbsTime,
  setActiveSessionCard,
  shortPath,
  showAPIError,
  showNetworkError,
} from './utilities.js';
// cron_view.js — Cron (定时任务) dashboard view.
//
// RFC docs/rfc/dashboard-cron-view-extraction.md (PR-1). Extracted verbatim
// from dashboard.js (the "===== Cron Tab =====" region, ~3960 lines): job
// list / drawer / create+edit forms / cron-expression parsing / run timeline
// + transcript / live event subscription / context menus.
//
// This module imports dashboard.js, so dashboard evaluates first: the
// cronSortOrder initializer below finds lsGet ready, and the wsm.on
// registrations and bootstrap fetchCronJobs() at the tail run after every
// cron function is defined. dashboard reaches back only through nz.bus and
// nzViews.cron; the cron modules it imports reach back only through the two
// shell slots it registers (openCronPanel, renderCronPanel).

/* ===== Cron Tab ===== */

// resize-fallback listener dedup flag (was a window property pre-#2557-E3).
let cronLayoutWindowListener = null;



// cron-panel-consolidation RFC §4.2: sidebar / mainShell are now reserved
// for human conversation surfaces and never paint cron-scheduler sessions.
// The previous `cronVisibleKeys` whitelist + markCronSessionVisible plumbing
// were UI bandages on top of /api/sessions returning cron stubs; PR2 moves
// that filter into the server (internal/server/dashboard_session.go), so
// the dashboard can simply assume no cron rows ever arrive. The single
// helper retained from the old block was `isCronSessionKey`, kept for the
// dismissSession safety check — moved to nz_util (#2557 PR-E1, imported
// above). New cron-detail visibility lives in `cronDrawerState.jobId`
// (PR4 / cronDrawerState.jobId state machine).
// R110-P2 cron filter state — module-level so renderCronList can read the
// live values each paint without a closure. Mirrors the sidebar-search
// approach (cronFilterQuery is the substring, cronFilterStatus is one of
// 'all' | 'active' | 'attention'). 'attention' matches paused-or-last_error,
// aligning with the header cron-badge's attention definition so the filter
// "what needs my eyeballs" dovetails with the top-level signal.
let cronFilterQuery = '';
let cronFilterStatus = 'all';

// cronSortComparators 定义四种排序模式的 compare 函数。
// - created_desc: 默认，最新创建在前（与旧版一致）
// - next_asc    : 按 next_run 升序——"接下来谁先跑"排在前；无 next_run 沉底
// - last_desc   : 最近跑过的排在前；从未跑过沉底
// - title_asc   : 按 title / prompt-fallback 字典序升序——便于按名字扫
const cronSortComparators = {
  created_desc: (a, b) => (b.created_at || 0) - (a.created_at || 0),
  next_asc: (a, b) => {
    const av = a.next_run || Number.POSITIVE_INFINITY;
    const bv = b.next_run || Number.POSITIVE_INFINITY;
    return av - bv;
  },
  last_desc: (a, b) => (b.last_run_at || 0) - (a.last_run_at || 0),
  title_asc: (a, b) => {
    const at = ((a.title || '').trim() || firstNonEmptyLine(a.prompt || '', 60)).toLowerCase();
    const bt = ((b.title || '').trim() || firstNonEmptyLine(b.prompt || '', 60)).toLowerCase();
    return at.localeCompare(bt);
  },
};

function cronSortComparatorsHasKey(k) {
  return Object.prototype.hasOwnProperty.call(cronSortComparators, k);
}

// cronSortOrder 控制 cron 面板列表的排序模式。保存在 localStorage 里，
// 切回页面保留用户偏好。四种模式见 cronSortComparators。cron-v2-polish §3.4。
// R202606-CR-1: this initializer must run AFTER cronSortComparators (and the
// cronSortComparatorsHasKey helper that closes over it) are defined. When the
// IIFE sat above those definitions, a saved localStorage preference made it
// call cronSortComparatorsHasKey while cronSortComparators was still in its
// TDZ, throwing "Cannot access 'cronSortComparators' before initialization"
// and crashing the whole cron panel on load. Keep it below this line.
let cronSortOrder = (function() {
  // RNEW-UX-004 demo: migrated to unified lsGet helper. Keyspace changed
  // from 'nz_cron_sort' to 'nz:cron_sort' — one-time loss of the saved
  // preference is acceptable (falls back to 'created_desc').
  const saved = lsGet('cron_sort', '');
  if (saved && cronSortComparatorsHasKey(saved)) return saved;
  return 'created_desc';
})();

// setCronSortOrder 切换排序模式，持久化到 localStorage 并重绘列表。
function setCronSortOrder(order) {
  if (!cronSortComparatorsHasKey(order)) return;
  cronSortOrder = order;
  lsSet('cron_sort', order); // RNEW-UX-004 demo: unified helper (see top-of-file lsSet)
  renderCronList();
}



// buildCronWorkspaceBody renders the workspace picker as a dropdown button +
// popover（v2 polish，参考 Claude Scheduled Tasks 的 "Work in a project ▾"
// 样式）。点击按钮展开列表；选中后 popover 折叠并把按钮文本改为所选 path。
//
// 保留 IDs 契约：#cron-ws-list, #cron-ws-custom-toggle, #cron-ws-custom-form,
// #cron-workdir 被 cronSelectWorkspace / toggleCronWsCustom / 提交 collector
// 读取；外壳改造但这些稳定锚点保持。aria-label="工作目录路径" 也是契约锁定
// 字符串（static_ux_contract_test 会 grep）。
function buildCronWorkspaceBody() {
  return buildCronWorkspaceBodyInternal({
    inputId: 'cron-workdir',
    selectedPath: '',
  });
}

function buildCronWorkspaceBodyInternal(opts) {
  const selected = opts.selectedPath || '';
  // Button label: 选中的项目名 > 选中的路径尾段 > 默认占位
  let label = '默认工作目录';
  if (selected) {
    const match = sessionList.projectsData.find(p => p.path === selected);
    label = match ? match.name : shortPath(selected);
  }
  // 下拉按钮，点击 toggle popover
  const buttonHtml =
    '<button type="button" class="ws-dropdown-btn" id="' + escAttr(opts.buttonId || 'cron-ws-dropdown') + '"' +
      ' aria-haspopup="listbox" aria-expanded="false" data-action="cron-ws-dropdown">' +
      '<span class="ws-dropdown-icon" aria-hidden="true">&#128193;</span>' +
      '<span class="ws-dropdown-label">' + esc(label) + '</span>' +
      '<span class="ws-dropdown-caret" aria-hidden="true">&#9662;</span>' +
    '</button>';
  // Popover 内容：项目列表 + "自定义路径" 触发条目
  let listItems = '';
  if (sessionList.projectsData.length > 0) {
    listItems = sessionList.projectsData.map(p => {
      const sel = selected && p.path === selected;
      return '<li role="option" data-path="' + escAttr(p.path) + '"' +
        (sel ? ' class="selected" aria-selected="true"' : ' aria-selected="false"') +
        ' data-action="cron-ws-select">' +
          '<div class="pp-name">' + esc(p.name) + '</div>' +
          '<div class="pp-path">' + esc(shortPath(p.path)) + '</div>' +
        '</li>';
    }).join('');
  }
  listItems +=
    '<li id="cron-ws-custom-toggle" role="option" data-action="cron-ws-custom">' +
      '<div class="pp-custom"><span class="pp-custom-icon">+</span> 自定义路径</div>' +
    '</li>';

  const popoverHtml =
    '<div class="ws-dropdown-popover" id="cron-ws-popover" role="listbox" aria-label="选择工作目录">' +
      '<ul class="proj-pick" id="cron-ws-list" role="listbox" aria-label="工作目录">' +
        listItems +
      '</ul>' +
      '<div id="cron-ws-custom-form" class="nz-cron-ws-custom' + (selected && !sessionList.projectsData.find(p => p.path === selected) ? '' : ' nz-hidden') + '">' +
        '<input id="' + escAttr(opts.inputId) + '" placeholder="' + escAttr(serverInfo.defaultWorkspace || '/home/user/project') + '"' +
          ' value="' + escAttr(selected && !sessionList.projectsData.find(p => p.path === selected) ? selected : '') + '"' +
          ' aria-label="工作目录路径">' +
      '</div>' +
    '</div>';

  return '<div class="ws-dropdown-wrap">' + buttonHtml + popoverHtml + '</div>';
}


function createNewCronJob() {
  // Sprint 6c: fetch backends upfront so the picker (if any) is ready when
  // the modal renders. Failure / single-backend deploys map to '' which
  // collapses the picker section — same pattern as createNewSession.
  fetchCLIBackends().then(backendsData => {
    const backendHtml = renderBackendPicker(backendsData, { selectId: 'cron-backend' });
    openCronCreateModal(backendHtml);
  }).catch(() => openCronCreateModal(''));
}

function openCronCreateModal(backendHtml) {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  // Default "每小时" matches the most common ask and gives users an
  // immediate, meaningful preview on open.
  const scheduleHtml = buildScheduleSection({ mode: 'hourly' }, '');
  const wsBody = buildCronWorkspaceBody();
  const notifyHtml = buildCronNotifyToggleHtml('', false);
  const contextHtml = buildCronContextToggleHtml(false);
  const placementHtml = buildCronPlacementHtml('', 'cron-placement');

  // Title + aria-label are inlined as literals (not passed through esc())
  // so the static UX contract test can grep the exact fragments in source.
  // See internal/server/static_ux_contract_test.go :: R154 cron-create.
  overlay.innerHTML =
    '<div class="modal cron-modal" role="dialog" aria-modal="true" aria-label="新建定时任务">' +
      '<div class="cm-header">' +
        '<h3>新建定时任务</h3>' +
        '<button type="button" class="cm-close" data-action="cron-modal-dismiss" aria-label="关闭">✕</button>' +
      '</div>' +
      renderCronModalBody({
        scheduleHtml, wsBody, notifyHtml, contextHtml,
        backendHtml, placementHtml,
        promptId: 'cron-prompt',
        promptPlaceholder: '例如：总结昨天的代码变更，push 到日报频道',
      }) +
      '<div class="modal-btns">' +
        '<button type="button" data-action="cron-modal-dismiss">取消</button>' +
        '<button type="button" class="primary" data-action="cron-create-save">创建</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  cronPlacementBindHint('cron-placement');
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
  // 创建流：默认 Daily 09:00 立即生效，"打开即保存"也能提交合法 schedule。
  // 与编辑流相反——编辑流必须不 touched，保留 job.schedule 原值直到用户
  // 主动改。
  overlay._cronScheduleTouched = true;
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
  // Title 字段跨两列独立一行，放在最上方——符合"先起名，再写提示词"的
  // 直觉顺序，与 Claude Scheduled Tasks UI 的 Name → Description → Prompt
  // 结构对齐。留空允许，UI 自动回退显示 Prompt 首行（JobTitleOrFallback）。
  // 关联：docs/rfc/cron-v2-polish.md §3.1 Increment A。
  const titleField =
    '<div class="cron-field cron-f-title">' +
      '<div class="cf-label">名称 <span class="nz-hint-faint">（可选）</span></div>' +
      '<input id="' + escAttr(opts.titleId || 'cron-title') + '" type="text" placeholder="' + escAttr(opts.titlePlaceholder || '例如：日报总结 · 周一早会准备') + '" maxlength="256" aria-label="任务名称">' +
    '</div>';
  // backendHtml 由 caller 提供（Sprint 6c）。仅在多 backend 模式下非空，
  // 单 backend 时 renderBackendPicker 返回空串、整段折叠。位置选 "其他设置"
  // 区与 notify / fresh-context 同列，因为 backend 是 "怎么跑" 的运行时
  // 设定，与 work_dir（"在哪里"）的资源/路径含义不同。
  const backendBlock = opts.backendHtml ? opts.backendHtml : '';
  // placement 选择器与 backend 同区（都是"怎么跑"的运行时设定，RFC §7.1）。
  const placementBlock = opts.placementHtml ? opts.placementHtml : '';
  return '<div class="modal-body">' +
      '<div class="cron-modal-grid">' +
        titleField +
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
            backendBlock +
            placementBlock +
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

// buildCronPlacementHtml renders the 运行位置 selector (agentcore-cloud-
// sandbox RFC §7.1): 本机 (default) / 云沙箱 ☁️. The sandbox option carries
// the Phase 1 fence hint (≤60min / no work_dir / remote-only MCP). The
// fence is enforced server-side (validateCronPlacement + ErrSandboxWorkDir);
// the client mirrors the work_dir conflict check at submit time so the
// user gets a precise toast instead of a 400 round-trip.
function buildCronPlacementHtml(initialPlacement, selectId) {
  const sandboxSel = initialPlacement === 'sandbox' ? ' selected' : '';
  const localSel = sandboxSel ? '' : ' selected';
  return '<div class="cron-placement-block nz-field">' +
      '<label class="nz-field-label" for="' + escAttr(selectId) + '">运行位置</label>' +
      '<select id="' + escAttr(selectId) + '" aria-label="运行位置" class="nz-input-block">' +
        '<option value=""' + localSel + '>本机</option>' +
        '<option value="sandbox"' + sandboxSel + '>云沙箱 ☁️</option>' +
      '</select>' +
      '<span class="ct-hint nz-cron-sandbox-hint' + (sandboxSel ? '' : ' nz-hidden') + '" id="' + escAttr(selectId) + '-hint">云沙箱为一次性隔离运行（跑完即焚）：限 60 分钟内任务，暂不支持工作目录与本地 MCP。</span>' +
    '</div>';
}

// collectCronPlacementValue returns the placement value, or null when the
// selector is absent.
function collectCronPlacementValue(selectId) {
  const el = document.getElementById(selectId);
  if (!el) return null;
  return el.value || '';
}

// cronPlacementBindHint toggles the fence hint under the selector when the
// user flips to 云沙箱. Called once after the modal mounts.
function cronPlacementBindHint(selectId) {
  const el = document.getElementById(selectId);
  const hint = document.getElementById(selectId + '-hint');
  if (!el || !hint) return;
  el.addEventListener('change', function() {
    // Class toggle, not style.display: the hint ships with .nz-hidden from
    // the renderer, so mixing the two would leave a stale inline value
    // winning over the class (#2559 D6-3).
    hint.classList.toggle('nz-hidden', el.value !== 'sandbox');
  });
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
  if (cronStore.notifyDefault && cronStore.notifyDefault.platform && cronStore.notifyDefault.chat_id) {
    defaultHint = '→ ' + esc(cronStore.notifyDefault.platform) + ' (' + esc(cronStore.notifyDefault.chat_id) + ')';
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
        ' data-touched="' + touched + '" data-action-change="cron-notify-on">' +
      '<span class="ct-main">完成后通知我' +
        '<span class="ct-hint" id="cron-notify-default-hint">' + defaultHint + '</span>' +
      '</span>' +
    '</label>' +
    '<label class="cron-toggle nz-tighten-top" id="cron-notify-override-toggle-wrap">' +
      '<input type="checkbox" id="cron-notify-override" ' + (hasOverride ? 'checked' : '') + ' data-action-change="cron-notify-override">' +
      '<span class="ct-main nz-hint">自定义此任务的通知目标</span>' +
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

// toggleCronWsDropdown 打开/关闭工作目录 popover。event.stopPropagation 防止
// 顶层 document 的 outside-click handler 立即把它再关掉。
function toggleCronWsDropdown(e) {
  if (e) { e.preventDefault(); e.stopPropagation(); }
  const pop = document.getElementById('cron-ws-popover');
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (!pop) return;
  const open = pop.classList.toggle('open');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) wireCronWsOutsideClick();
}

// 单例 outside-click 监听，capture 阶段判断点击是否在 popover 外部；
// 若是则关闭。只在 popover 打开期间挂载，关闭时自 remove。
function wireCronWsOutsideClick() {
  if (wireCronWsOutsideClick._on) return;
  const h = function(ev) {
    const pop = document.getElementById('cron-ws-popover');
    const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
    if (!pop || !pop.classList.contains('open')) {
      document.removeEventListener('mousedown', h, true);
      wireCronWsOutsideClick._on = false;
      return;
    }
    if (pop.contains(ev.target) || (btn && btn.contains(ev.target))) return;
    pop.classList.remove('open');
    if (btn) btn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('mousedown', h, true);
    wireCronWsOutsideClick._on = false;
  };
  document.addEventListener('mousedown', h, true);
  wireCronWsOutsideClick._on = true;
}

function cronSelectWorkspace(el, path) {
  const overlay = el.closest('.modal-overlay');
  if (!overlay) return;
  overlay._cronWorkDir = path;
  document.querySelectorAll('#cron-ws-list li').forEach(li => {
    li.classList.remove('selected');
    li.setAttribute('aria-selected', 'false');
  });
  el.classList.add('selected');
  el.setAttribute('aria-selected', 'true');
  const customForm = document.getElementById('cron-ws-custom-form');
  if (customForm) {
    customForm.classList.add('nz-hidden');
    // Clear the hidden custom input so the submit path (which falls back
    // to wdInput.value when non-empty) can't resurrect a stale path after
    // the user picked a different project. Matters in the edit modal,
    // where wdInput is pre-populated with the job's current work_dir.
    const input = customForm.querySelector('input');
    if (input) input.value = '';
  }
  const toggle = document.getElementById('cron-ws-custom-toggle');
  if (toggle) toggle.classList.remove('nz-hidden');
  // v2 polish: 选中即把 popover 折叠 + 把按钮文本更新为项目名
  updateCronWsDropdownLabel(path);
  closeCronWsPopover();
}

function updateCronWsDropdownLabel(path) {
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (!btn) return;
  const labelEl = btn.querySelector('.ws-dropdown-label');
  if (!labelEl) return;
  if (!path) { labelEl.textContent = '默认工作目录'; return; }
  const match = sessionList.projectsData.find(p => p.path === path);
  labelEl.textContent = match ? match.name : shortPath(path);
}

function closeCronWsPopover() {
  const pop = document.getElementById('cron-ws-popover');
  const btn = document.getElementById('cron-ws-dropdown') || document.getElementById('edit-cron-ws-dropdown');
  if (pop) pop.classList.remove('open');
  if (btn) btn.setAttribute('aria-expanded', 'false');
}

function toggleCronWsCustom() {
  const form = document.getElementById('cron-ws-custom-form');
  const toggle = document.getElementById('cron-ws-custom-toggle');
  if (!form) return;
  // .nz-hidden is the single source of truth for this form's visibility
  // (#2559 D6-3) — the renderer ships it and every toggle site flips the class.
  if (form.classList.contains('nz-hidden')) {
    form.classList.remove('nz-hidden');
    if (toggle) toggle.classList.add('nz-hidden');
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
    form.classList.add('nz-hidden');
    if (toggle) toggle.classList.remove('nz-hidden');
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
  // Resolve title（可选）
  const titleInput = document.getElementById('cron-title');
  const title = titleInput ? titleInput.value.trim() : '';
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
    if (title) body.title = title;
    if (workDir) body.work_dir = workDir;
    const notifyVals = collectCronNotifyValues();
    if (notifyVals.notify !== null) body.notify = notifyVals.notify;
    if (notifyVals.notify_platform !== null) body.notify_platform = notifyVals.notify_platform;
    if (notifyVals.notify_chat_id !== null) body.notify_chat_id = notifyVals.notify_chat_id;
    const freshCtx = collectCronContextValue();
    if (freshCtx === true) body.fresh_context = true;
    // Sprint 6c: pick up the cron-modal backend choice (if any). The picker
    // collapses entirely in single-backend deploys, so the element may be
    // absent — treat that as "router default" and omit the field, matching
    // the server's omitempty contract.
    const backendEl = document.getElementById('cron-backend');
    const backendVal = backendEl && backendEl.value ? backendEl.value : '';
    if (backendVal) body.backend = backendVal;
    // placement（RFC §7.1）：默认本机省略字段；云沙箱时前端先行围栏校验
    // （与服务端 validateCronPlacement 同语义），给出精确 toast。
    const placementVal = collectCronPlacementValue('cron-placement');
    if (placementVal === 'sandbox') {
      if (workDir) { showToast('云沙箱暂不支持工作目录：请清空"在哪里"或改用本机运行', 'warning'); return; }
      body.placement = 'sandbox';
    }
    let data;
    try {
      data = await fetchJSON(NZ_CONTRACT.API.cron, {timeoutMs: 10000, method: 'POST', headers, body: JSON.stringify(body)});
    } catch (err) {
      if (err && err.status) showAPIError('创建定时任务', err.status, err.message || '');
      else showNetworkError('创建定时任务', err);
      return;
    }
    if (!data) data = {};
    if (overlay) overlay.remove();
    showToast('定时任务已创建', 'success');
    fetchCronJobs();
    if (data.id) {
      // cron-panel-consolidation RFC §4.2: a freshly-created cron job no
      // longer pushes itself into the sidebar (cron stubs are filtered
      // server-side) nor takes over mainShell. Open the per-job drawer
      // directly so the operator sees the row they just configured.
      openCronDetail(data.id);
    }
  } catch (e) { showNetworkError('创建定时任务', e); }
}

function openCronPanel() {
  // Cron is its own top-level view now. Ensure it's active before painting.
  // setActivityView('cron') sets activeView='cron' first, then calls back
  // into openCronPanel — so the guard below is already satisfied on re-entry
  // and we don't recurse. Direct callers (legacy #btn-cron, openCronDetail)
  // route through here and get the view switch for free.
  if (ui.activeView !== 'cron') { setActivityView('cron'); return; }
  // Deselect managed session via selectedKey only — selectedNode is the
  // sidebar filter now (see previewDiscovered comment) and must survive
  // opening the cron panel so the user comes back to the right node list.
  selection.key = null;
  if (sessionStream.subscribedKey) sessionStream.unsubscribe();
  if (timers.events) { clearInterval(timers.events); timers.events = null; }
  setActiveSessionCard(null);
  // NOTE: no mobileEnterChat() here. Cron is a standalone view, not a chat
  // session — entering chat view would hide the bottom tab bar (the only nav
  // surface on mobile) and strand the user. The cron view is full-screen via
  // body.nz-view-cron CSS and the tab bar stays visible.
  // Paint immediately from the cache primed at page load (line ~5982) so the
  // click feels instant. If the cache is empty we still render the panel —
  // renderCronPanel handles the zero-job "empty state" branch. A background
  // refresh reconciles with the server and re-renders if anything changed.
  renderCronPanel();
  fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
}

// filterCronJobs is the pure match step for the R110-P2 cron panel filter.
// Extracted so unit tests exercise the predicate without driving DOM. Match
// surface for the substring arm: title, prompt, work_dir, schedule, id (all
// case-insensitive). title 放在最前，匹配优先 —— 人们搜索 cron 时最先想到
// 的就是自己给任务起的那个名字。
//
// Status arm:
//   - 'all'        全部
//   - 'active'     非 paused（保留旧语义；与 attentionCount 互斥的"运行中"
//                  入口，filterBar chip 上仍叫"运行中"以兼容 e2e）
//   - 'attention'  paused || last_error || missed（与 cronBadge 同源）
function filterCronJobs(jobs, query, status) {
  const q = (query || '').trim().toLowerCase();
  const s = status || 'all';
  return (Array.isArray(jobs) ? jobs : []).filter(j => {
    if (!j) return false;
    if (s === 'active' && j.paused) return false;
    // cron-v2-polish §3.3: attention 扩展为 paused || last_error || missed，
    // 与 fetchCronJobs 里的 cronBadge 计数同源，避免两处判断漂移。
    if (s === 'attention' && !(j.paused || j.last_error || j.missed)) return false;
    if (!q) return true;
    const fields = [j.title, j.prompt, j.work_dir, j.schedule, j.id];
    for (const f of fields) {
      if (typeof f === 'string' && f.toLowerCase().indexOf(q) !== -1) return true;
    }
    return false;
  });
}

// Cron ⋯ menu — single-active-menu model.
//
// Only one menu may be open at a time. A single module-level `cronMenuOnDoc`
// captures the outside-click handler so repeated toggles can't accumulate
// listeners (prior design spawned one per open, only removed on outside
// click — rapid open/close leaked them).
//
// Item actions use data-action dispatch instead of onclick string
// interpolation so the job id can't escape its quote boundary on any path.
let cronMenuOpenId = null;
let cronMenuOnDoc = null;
let cronMenuOnScroll = null;

function closeCronMenus() {
  document.querySelectorAll('.cj-menu').forEach(el => {
    if (el.parentNode) el.parentNode.removeChild(el);
  });
  cronMenuOpenId = null;
  if (cronMenuOnDoc) {
    document.removeEventListener('click', cronMenuOnDoc, true);
    cronMenuOnDoc = null;
  }
  if (cronMenuOnScroll) {
    window.removeEventListener('scroll', cronMenuOnScroll, true);
    window.removeEventListener('resize', cronMenuOnScroll);
    cronMenuOnScroll = null;
  }
}

// positionCronMenu places the menu near the anchor's bottom-right. If there
// isn't enough room below (viewport-bottom), flips above. Called after the
// menu is attached to the DOM so measurements are real.
function positionCronMenu(menu, anchor) {
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const anchorRect = anchor.getBoundingClientRect();
  const menuRect = menu.getBoundingClientRect();
  const margin = 6;
  // Right-align to anchor.
  let left = Math.min(anchorRect.right - menuRect.width, vw - menuRect.width - margin);
  left = Math.max(margin, left);
  // Prefer below; flip above if not enough room.
  const spaceBelow = vh - anchorRect.bottom;
  const spaceAbove = anchorRect.top;
  let top;
  if (spaceBelow >= menuRect.height + margin || spaceBelow >= spaceAbove) {
    top = anchorRect.bottom + 4;
  } else {
    top = anchorRect.top - menuRect.height - 4;
  }
  top = Math.max(margin, Math.min(top, vh - menuRect.height - margin));
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
}

// Dispatch table for menu actions. Keys must match the data-action values
// emitted in toggleCronMenu so a rename on one side is a caller-site break.
const CRON_MENU_ACTIONS = {
  'run': (id) => cronTriggerNow(id),
  'open': (id) => openCronDetail(id),
  'edit': (id) => editCronJob(id),
  'pause': (id) => cronPause(id),
  'resume': (id) => cronResume(id),
  'delete': (id) => cronDelete(id),
};

function handleCronMenuClick(ev) {
  const btn = ev.target.closest('.cj-menu-item');
  if (!btn) return;
  ev.stopPropagation();
  const action = btn.getAttribute('data-menu-action');
  const id = btn.getAttribute('data-id');
  closeCronMenus();
  const fn = CRON_MENU_ACTIONS[action];
  if (fn && id) fn(id);
}

function toggleCronMenu(id) {
  const sel = '.cj-row[data-cron-id="' + id.split('"').join('\\"') + '"]';
  const row = document.querySelector(sel);
  if (!row) return;
  // Toggle off if this row's menu is already open.
  if (cronMenuOpenId === id) {
    closeCronMenus();
    return;
  }
  // Close any other open menu before opening this one.
  closeCronMenus();
  const j = (cronStore.jobs || []).find(x => x && x.id === id);
  if (!j) return;
  const items = [];
  if (!j.paused) items.push({ label: '立即运行', action: 'run' });
  items.push({ label: '打开最近会话', action: 'open' });
  items.push({ label: '编辑', action: 'edit' });
  items.push({ label: j.paused ? '恢复' : '暂停', action: j.paused ? 'resume' : 'pause' });
  items.push({ sep: true });
  items.push({ label: '删除', action: 'delete', danger: true });
  const menu = document.createElement('div');
  menu.className = 'cj-menu open';
  menu.innerHTML = items.map(it => {
    if (it.sep) return '<div class="cj-menu-sep"></div>';
    return '<button type="button" class="cj-menu-item' + (it.danger ? ' danger' : '') +
      '" data-menu-action="' + escAttr(it.action) +
      '" data-id="' + escAttr(id) + '">' +
      esc(it.label) + '</button>';
  }).join('');
  menu.addEventListener('click', handleCronMenuClick);
  // Attach to <body> rather than the row so position:fixed escapes the
  // .cron-detail-body overflow clipping box. The menu anchors visually to
  // the ⋯ button via positionCronMenu.
  document.body.appendChild(menu);
  const anchor = row.querySelector('.cj-menu-btn') || row;
  positionCronMenu(menu, anchor);
  cronMenuOpenId = id;
  // Close on outside click / scroll / resize. setTimeout defers the doc
  // handler past the current click so the same event that opened doesn't
  // immediately close.
  setTimeout(() => {
    cronMenuOnDoc = (e) => {
      if (!menu.contains(e.target)) closeCronMenus();
    };
    document.addEventListener('click', cronMenuOnDoc, true);
  }, 0);
  cronMenuOnScroll = () => closeCronMenus();
  window.addEventListener('scroll', cronMenuOnScroll, true);
  window.addEventListener('resize', cronMenuOnScroll);
}

// cronApplyRunStarted optimistically patches the in-memory cronStore.jobs row so
// the "运行中" badge renders without a list refetch. P0 cron-run-history
// (RFC §7.2 / §8.1) — the run-ended event triggers the authoritative
// refetch a few seconds later. Tolerates an unknown job_id (we may receive
// a started event for a freshly created job before the local list pulls
// it; in that case the next fetchCronJobs reconciles).
function cronApplyRunStarted(msg) {
  if (!msg || !msg.job_id) return;
  const list = Array.isArray(cronStore.jobs) ? cronStore.jobs : [];
  const j = list.find(x => x && x.id === msg.job_id);
  if (!j) {
    // Optimistic miss: fallback to a refetch so the UI catches up.
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
    return;
  }
  j.current_run = {
    run_id: msg.run_id,
    started_at: msg.started_at,
    phase: 'queued',
    trigger: msg.trigger || '',
    session_id: msg.session_id || '',
    // applied_at_local 只在本地存在（不来自 wire）：fetchCronJobs 用它判断
    // 一个 list 响应是否比这个乐观补丁更旧 —— 见那里的 stale-clobber 注释。
    applied_at_local: Date.now(),
  };
  // 一个新的 run 起步意味着上一次 timed_out 的"冻结"窗口已过去，
  // 清掉 frozen 标记让用户能再次看到实时事件。否则 hourly 任务下一次
  // 跑时前端仍然会丢事件，看起来像 bug。
  cronFrozenRuns.delete(msg.job_id);
  // Round 2 R-4: WS-confirmed run start clears the optimistic cooldown
  // lock — the disabled-running label takes over from this point. If the
  // started event came from a non-manual trigger (scheduled/catchup), the
  // cooldown lock was never set so this is a no-op.
  cronTriggerCooldownClear(msg.job_id);
  // cron-live RFC §3: 跨 run 复位前一轮的 cron live 状态。两种场景：
  //   - fresh 模式：scheduler_run.go:318 Reset(key) 销毁旧 stub，后端
  //     resubscribeEvents 接管；前端容器里旧 run 的事件应当被新 run 替换。
  //   - persistent 模式：同 stub 跨 run 持续，但用户视角下 Run #2 是新 run，
  //     不该把 Run #1 的事件混在容器里。
  // 即使 ensureCronLiveSubscription 短路（jobId 已订）也要清。runStartedAt
  // 更新成新 run 的 started_at，让 cron_live 的 onReady / onCronLiveSessionState
  // 的 re-sub 路径用正确的 after= 阈值。
  if (cronLive.jobId === msg.job_id) {
    cronLive.events = [];
    cronLive.lastEventTimeMs = 0;
    cronLive.truncatedCount = 0;
    cronLive.runStartedAt = msg.started_at || Date.now();
    cronLive.status = 'pending';
    setCronLiveStatus('pending');
  }
  renderCronPanel();
  // cron-live RFC §3: drawer 已开 + 该 job 起跑 → 触发订阅。renderCronPanel
  // 已先调到 renderCronDrawer，drawer 内的 #cron-live-events 容器此时已就位。
  ensureCronLiveSubscription();
}

// cronApplyRunEnded patches the local row before the authoritative
// fetchCronJobs lands. We clear current_run so the running-badge stops
// flashing immediately; the subsequent refetch fills in last_error_class
// / counters / last_run_at.
function cronApplyRunEnded(msg) {
  if (!msg || !msg.job_id) return;
  const list = Array.isArray(cronStore.jobs) ? cronStore.jobs : [];
  const j = list.find(x => x && x.id === msg.job_id);
  if (!j) return;
  j.current_run = null;
  cronRunClearedAtLocal.set(msg.job_id, { at: Date.now(), runId: msg.run_id || '' });
  // Provisional last_error_class / last_run_at so the row repaints with
  // the new state before fetchCronJobs returns. Backend remains source
  // of truth for the persisted snapshot.
  if (msg.state && msg.state !== 'succeeded' && msg.state !== 'skipped') {
    j.last_error_class = msg.error_class || '';
    // 任何非 succeeded/skipped 终态都冻结：timed_out / failed / canceled。
    // 新的 run_started（cron）会清除冻结。
    cronFrozenRuns.add(msg.job_id);
  } else if (msg.state === 'succeeded') {
    j.last_error_class = '';
    j.last_error = '';
    cronFrozenRuns.delete(msg.job_id);
  } else if (msg.state === 'skipped') {
    cronFrozenRuns.delete(msg.job_id);
  }
  if (msg.ended_at) j.last_run_at = msg.ended_at;
  // cron-live RFC §3 / §6: 任务进入终态（任意 state），cron live 订阅保留供
  // 操作员回看，但 status 切到 'stopped' 让用户清楚区分"直播中"vs"已结束"。
  // unsub 仅在 closeCronDetail / 切换 jobId 时发生。
  if (cronLive.jobId === msg.job_id) {
    cronLive.status = 'stopped';
    setCronLiveStatus('stopped');
  }
  renderCronPanel();
}

// Polling timer that re-renders cron rows so "运行中 Xs" advances each
// second while at least one job is running. Idle when no jobs are running.
let cronRunningTickTimer = null;

// R243-PERF-8 / #813: per-tick scoped text update.
//
// Pre-fix behaviour: ensureCronRunningTick fired renderCronPanel() every
// second, which rebuilt the *entire* cron list innerHTML (N rows × full
// HTML strings + onclick wiring) just to advance the elapsed-time text
// on the running rows. With 50 jobs and any one of them running, that
// was 50× full-row reflow per second — a measurable jank hot path on
// modest hardware.
//
// Post-fix: the tick walks only `.cj-row.is-running` elements and
// updates the two text nodes that carry the elapsed label
// (`.cj-when.running` desktop, `.cj-when-inline.is-running` mobile).
// Everything else — schedule chip, stats badge, action buttons — is
// untouched, so the layout never reflows past the elapsed label.
//
// Fallback: if no running rows are mounted (the panel was closed between
// the timer firing and this callback) we DON'T fall back to a full
// renderCronPanel — the timer's own three-condition guard above already
// catches that case and clears the interval, so the no-op is correct.
//
// Job churn (a row finishes / a new row starts running) is handled by
// the WS run_started / run_ended (cron) fan-out which calls
// cronApplyRunStarted / cronApplyRunEnded → renderCronPanel; that path
// already updates row classes (`is-running` on/off) and is the right
// place to add/remove rows. The 1Hz tick is therefore *only* responsible
// for advancing the elapsed text on rows that are already classed
// is-running, which is exactly the scope of this targeted update.
function cronRunningTickPaintScoped() {
  const host = document.getElementById('cron-list-items');
  if (!host) return;
  const rows = host.querySelectorAll('.cj-row.is-running');
  if (!rows.length) return;
  // Build a quick lookup so we don't O(N) scan cronStore.jobs once per row.
  const byId = new Map();
  if (Array.isArray(cronStore.jobs)) {
    for (const j of cronStore.jobs) {
      if (j && j.id) byId.set(j.id, j);
    }
  }
  for (const row of rows) {
    const id = row.getAttribute('data-cron-id');
    if (!id) continue;
    const job = byId.get(id);
    if (!job || !job.current_run || !job.current_run.started_at) continue;
    const label = formatRunningElapsed(job.current_run.started_at);
    // Desktop column.
    const whenEl = row.querySelector('.cj-when.running');
    if (whenEl && whenEl.textContent !== label) whenEl.textContent = label;
    // Mobile inline span (cj-when-inline). Class set carries imminent /
    // paused but for running rows we just rewrite text.
    const inlineEl = row.querySelector('.cj-when-inline');
    if (inlineEl && inlineEl.textContent !== label) inlineEl.textContent = label;
  }
}

function ensureCronRunningTick() {
  const anyRunning = Array.isArray(cronStore.jobs) && cronStore.jobs.some(j => j && j.current_run);
  // Stop conditions（任何一个成立即清掉 timer）：
  //   - 无 running job
  //   - 当前选中了某个 session（renderCronPanel 第一行就 return,timer 等于在浪费 CPU）
  //   - cron-list-items DOM 已不在文档（用户切到非 cron 视图）
  // R220-FE-1: 修复 timer 永不停的内存/CPU 泄漏。
  const cronListMounted = !!document.getElementById('cron-list-items');
  const shouldRun = anyRunning && !selection.key && cronListMounted;
  if (shouldRun && !cronRunningTickTimer) {
    cronRunningTickTimer = setInterval(() => {
      // Defensive: 同样的三条件检查在 tick 内也跑一遍——避免 selectSession 切换
      // 之后这一帧还在 schedule 但 DOM 已经换了。
      if (selection.key || !document.getElementById('cron-list-items')) {
        clearInterval(cronRunningTickTimer);
        cronRunningTickTimer = null;
        return;
      }
      // R243-PERF-8 / #813: scoped text update instead of full
      // renderCronPanel rebuild. See cronRunningTickPaintScoped godoc.
      try { cronRunningTickPaintScoped(); } catch (_) {}
    }, 1000);
  } else if (!shouldRun && cronRunningTickTimer) {
    clearInterval(cronRunningTickTimer);
    cronRunningTickTimer = null;
  }
}

// cronJobCardHtml renders a single cron row. v3 redesign: high-density row
// replaces the v2 card (see docs/TODO.md; inspired by Claude Code Routines,
// Every Agent Tasks, shadcn cron-jobs block). Structure:
//
//   ● title                 每天 04:00  ...  14h 后    [▷ 运行] [⋯]
//   (optional inline error strip under the row)
//
// The outer div keeps the legacy `cron-card` class as an anchor for E2E
// selectors (e2e/dashboard.test.js never asserts inner structure). The new
// visual class is `cj-row`.
function cronJobCardHtml(j) {
  const titleStr = (j.title || '').trim() || firstNonEmptyLine(j.prompt || '', 60);
  const hasTitle = !!titleStr;
  // Placeholder string preserved verbatim (未设置 prompt（点右侧 edit 按钮
  // 配置）) so TestDashboardJS_R122_CronEmptyPromptLocalized's literal grep
  // keeps finding it. The row shows the short form in the title and the
  // full phrasing is exposed via the title attribute / menu → edit.
  const emptyPromptHint = '未设置 prompt（点右侧 edit 按钮配置）';
  const displayTitle = hasTitle ? titleStr : '未设置 prompt';

  const isPaused = !!j.paused;
  const isError = !!j.last_error && !isPaused;
  const isMissed = !!j.missed && !isPaused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  const isActive = cronDrawerState.jobId === j.id;
  const rowClasses = ['cj-row'];
  if (isPaused) rowClasses.push('paused');
  if (isError) rowClasses.push('is-error');
  if (isMissed) rowClasses.push('is-missed');
  if (isRunning) rowClasses.push('is-running');
  if (isActive) rowClasses.push('is-active');

  const when = cronJobWhen(j, isRunning, isPaused);
  // P2 cron-run-history (RFC §8.1 / §8.4) — 成功率小徽章 + recent_runs hover tooltip。
  const statsBadge = cronStatsBadgeHtml(j);
  const subRow = cronJobSubRowHtml(j, when, isPaused, isMissed);

  // Error strip: inline one-line summary for non-paused rows with last_error.
  const errorStrip = isError
    ? '<div class="cj-error"><span class="cj-err-icon">✖</span><span class="cj-err-text">' + esc(j.last_error) + '</span></div>'
    : '';

  // Actions: ghost Run + ⋯ menu trigger. Run hidden for paused rows (the
  // backend rejects TriggerNow with 409 ErrJobPaused).
  const runBtn = j.paused
    ? ''
    : '<button type="button" class="cc-btn cj-run" data-action="cron-run-now" title="立即执行一次" aria-label="立即执行一次"><span aria-hidden="true">▷</span> 运行</button>';
  const menuBtn = '<button type="button" class="cj-menu-btn" data-action="cron-menu-toggle" aria-label="更多操作" aria-haspopup="true">⋯</button>';

  // Status dot is the only at-a-glance signal of a job's run state; give it a
  // text label so it doesn't rely on colour alone (a11y) and so hover/SR users
  // get the same information sighted users read from the colour.
  const dotLabel = isRunning ? '运行中'
    : isPaused ? '已暂停'
    : isError ? '上次运行出错'
    : isMissed ? '已错过计划运行'
    : '已启用';

  return '<div class="' + rowClasses.join(' ') + ' cron-card" data-cron-id="' + escAttr(j.id) + '" role="button" tabindex="0" ' +
    'data-action="cron-open" data-action-keydown="cron-open">' +
    '<span class="cj-dot" role="img" title="' + escAttr(dotLabel) + '" aria-label="' + escAttr('状态：' + dotLabel) + '"></span>' +
    '<div class="cj-main">' +
      '<div class="cj-title' + (hasTitle ? '' : ' placeholder') + '" title="' + escAttr(titleStr || emptyPromptHint) + '">' + esc(displayTitle) + '</div>' +
      subRow +
    '</div>' +
    when.col +
    statsBadge +
    '<div class="cj-actions">' + runBtn + menuBtn + '</div>' +
    errorStrip +
  '</div>';
}

// cronJobWhen is a row's when-column: running → "运行中 Xs"（实时计时）;
// paused → "已暂停"/"已自动暂停"; else colloquial relative time. Running takes precedence
// over paused (P0 cron-run-history, RFC §8.1): a TriggerNow on a paused job is
// rejected backend-side, so a run on a paused row is a real one.
function cronJobWhen(j, isRunning, isPaused) {
  const nextAbs = j.next_run ? formatAbsTime(j.next_run) : '';
  let label = '';
  let imminent = false;
  if (isRunning) {
    label = formatRunningElapsed(j.current_run.started_at);
  } else if (isPaused) {
    label = j.paused_reason === 'auto_failures' ? '已自动暂停' : '已暂停';
  } else if (j.next_run) {
    const w = formatWhenColloquial(j.next_run);
    label = w.label;
    imminent = w.imminent;
  }
  const title = isRunning
    ? ' title="run_id ' + escAttr(j.current_run.run_id || '') + (j.current_run.phase ? ' — phase ' + escAttr(j.current_run.phase) : '') + '"'
    : (nextAbs ? ' title="next run: ' + escAttr(nextAbs) + '"' : '');
  const classes = 'cj-when' +
    (imminent ? ' imminent' : '') +
    (isPaused && !isRunning ? ' paused' : '') +
    (isRunning ? ' running' : '');
  const col = label
    ? '<div class="' + classes + '"' + title + '>' + esc(label) + '</div>'
    : '<div class="cj-when"></div>';
  return { label, imminent, col };
}

// cronJobSubRowHtml is the row's sub-row: the clickable schedule chip (→ edit
// modal), icons only where a value differs from the default (notify off,
// fresh on, missed), the last-run chip, and the when label for narrow
// viewports, where CSS hides the .cj-when column.
function cronJobSubRowHtml(j, when, isPaused, isMissed) {
  const lastAbs = j.last_run_at ? formatAbsTime(j.last_run_at) : '';
  const agoStr = j.last_run_at ? formatAgoColloquial(j.last_run_at) : '';
  // role=button + tabindex=0 + Enter/Space handler so a keyboard user can
  // open the edit modal focused at the schedule field without mousing.
  const scheduleChip = '<span class="cj-schedule" role="button" tabindex="0"' +
    ' data-action="cron-edit" data-action-keydown="cron-edit"' +
    ' title="点击修改时间">' + esc(humanizeCron(j.schedule) + cronTimezoneSuffix()) + '</span>';
  // ☁️ placement 徽标（RFC §7.2）：第三个正交标识，排最前；
  // last_error_class 提供 terminal 着色（transport 红+⚠）。
  let iconGlyphs = cronPlacementBadgeHtml(j.placement || '', j.last_error_class || '');
  if (j.notify === false) {
    iconGlyphs += '<span class="cj-icon notify-off" title="IM 通知已关闭">&#128277;</span>';
  }
  if (j.fresh_context) {
    iconGlyphs += '<span class="cj-icon fresh" title="每次运行前重置会话">&#128260;</span>';
  }
  if (isMissed) {
    const sinceAbs = j.missed_since ? formatAbsTime(j.missed_since) : '';
    const tip = sinceAbs ? '上次应跑于 ' + sinceAbs + '；进程可能刚重启或休眠过' : '已错过至少一次调度';
    iconGlyphs += '<span class="cj-icon missed" title="' + escAttr(tip) + '">&#9888;</span>';
  }
  const lastRunChip = agoStr
    ? '<span class="cj-ago"' + (lastAbs ? ' title="last run: ' + escAttr(lastAbs) + '"' : '') + '>上次 ' + esc(agoStr) + '</span>'
    : '';
  const whenMobile = when.label
    ? '<span class="cj-when-inline' + (when.imminent ? ' imminent' : '') + (isPaused ? ' paused' : '') + '">' + esc(when.label) + '</span>'
    : '';
  return '<div class="cj-sub">' + scheduleChip + iconGlyphs + lastRunChip + whenMobile + '</div>';
}

// cronStatsBadgeHtml — P2 cron-run-history (RFC §8.1 / §8.4) 列表卡片成功率徽章。
// 数据源：j.stats（total/succeeded/failed/skipped/timed_out/canceled）+ j.recent_runs。
// 三档：100%=绿 N、80-99%=中性 99%、<80%=红 92% (118/120)。total=0 不渲染（噪声）。
// Tooltip：hover 出 5 个状态气泡（按 recent_runs 顺序，最旧→最新）+ trigger 信息。
function cronStatsBadgeHtml(j) {
  const stats = j && j.stats;
  if (!stats || !stats.total || stats.total <= 0) return '';
  const total = stats.total | 0;
  const ok = stats.succeeded | 0;
  // 成功率 = succeeded / total（skipped / canceled 不计为失败也不计为成功，
  // 但保留在分母里，与详情页"120 次"的口径一致；后续 P3 可拆开）。
  const rate = total > 0 ? Math.round((ok * 100) / total) : 0;
  let cls, label;
  // failed / timed_out 后端 omitempty：为 0 时字段缺失 → undefined，必须 || 0
  // 否则 ok 分支永不可达（健康任务一直显示 100% 而非绿色 N）。
  if (rate >= 100 && (stats.failed || 0) === 0 && (stats.timed_out || 0) === 0) {
    cls = 'ok'; label = String(total);
  } else if (rate >= 80) {
    cls = 'mid'; label = rate + '%';
  } else {
    cls = 'bad'; label = rate + '% (' + ok + '/' + total + ')';
  }
  // recent_runs 5 条状态气泡（最旧→最新，与时间轴方向相反——气泡是"最近趋势条"，
  // 习惯上从左到右走时间）。空数组 fallback 为单条空气泡占位。
  const recent = Array.isArray(j.recent_runs) ? j.recent_runs.slice(0, 5).reverse() : [];
  const dotsHtml = recent.length > 0
    ? recent.map(r => {
        const st = (r && r.state) || '';
        const tip = formatAbsTime((r && r.started_at) || 0) +
          (st ? ' · ' + cronStateLabel(st) : '') +
          (r && r.trigger ? ' · ' + r.trigger : '');
        return '<span class="cj-stats-dot ' + cronStateDotClass(st) + '" title="' + escAttr(tip) + '"></span>';
      }).join('')
    : '<span class="cj-stats-dot empty"></span>';
  // 用绝对定位的 .cj-stats-pop 做悬浮 tooltip；浏览器原生 title 在 hover dots
  // 时会被 dots 自身的 title 接管，主徽章再写一份避免双层 tooltip 闪烁。
  const summary = '总 ' + total + '· 成功 ' + ok + '· 失败 ' + (stats.failed | 0) +
    (stats.skipped ? '· 跳过 ' + stats.skipped : '') +
    (stats.timed_out ? '· 超时 ' + stats.timed_out : '');
  return '<div class="cj-stats ' + cls + '" tabindex="0" aria-label="' + escAttr('执行统计 ' + summary) + '">' +
    '<span class="cj-stats-label">' + esc(label) + '</span>' +
    '<div class="cj-stats-pop" role="tooltip">' +
      '<div class="cj-stats-pop-row">' + dotsHtml + '</div>' +
      '<div class="cj-stats-pop-meta">' + esc(summary) + '</div>' +
    '</div>' +
  '</div>';
}

// cronStateDotClass / cronStateLabel —— 单一状态色 / 文案表，详情页时间轴 +
// 列表 tooltip 共用，避免两处不一致。RFC §8.2 配色：
//   succeeded 绿 / failed 红 / skipped 灰 / timed_out 橙 / canceled 紫 / running 蓝脉动
// 词表本体在 nz_util.runStateDot / runStateLabel（#2540 统一 run 词表）；
// 这两个名字保留为薄委托，60+ 调用点与既有锚点不用动。
function cronStateDotClass(state) { return runStateDot(state); }
function cronStateLabel(state) { return runStateLabel(state); }

// cronPlacementBadgeHtml —— ☁️ 云沙箱徽标（RFC §7.2）。placement 是与
// backend pill / origin badge 正交的第三个标识：本机任务返回空串（零增量），
// 云沙箱任务渲染 ☁️ pill；terminal 三态由 error_class 着色：
//   success → 绿（默认 pill 色） / sandbox_failed → 黄 / sandbox_transport → 红+⚠
// （禁止一键重放的引导属于 Phase 3 确认队列，这里只做视觉信号。）
function cronPlacementBadgeHtml(placement, errorClass) {
  if (placement !== 'sandbox') return '';
  let cls = 'cj-placement-sandbox';
  let label = '☁️ 沙箱';
  if (errorClass === 'sandbox_transport') {
    cls += ' pl-transport';
    label = '☁️ 沙箱 ⚠';
  } else if (errorClass === 'sandbox_failed' || errorClass === 'sandbox_unavailable') {
    cls += ' pl-failed';
  }
  return '<span class="' + cls + '" title="' + escAttr('云沙箱运行（跑完即焚）' + (errorClass ? ' · ' + cronErrorClassLabel(errorClass) : '')) + '">' + label + '</span>';
}

// cronAttentionReplay triggers a replay from the queue card (§7.4 `确认未完成，
// 重放`). The server embeds the §6.2 rule-1 Stop-confirm; a 409 means the
// original microVM could not be confirmed dead — surface it and keep the card.
async function cronAttentionReplay(jobId, runId) {
  await cronReplayRunInner(jobId, runId, true);
}

// cronReplayRun is the §7.3 detail-view replay button handler (success /
// failed-clean runs). Thin wrapper over the shared inner so both entry points
// share the POST + error mapping.
async function cronReplayRun(jobId, runId) {
  await cronReplayRunInner(jobId, runId, false);
}

// cronReplayRunInner POSTs /replay and reconciles UI. fromQueue=true also
// refreshes the queue banner after a successful replay (the card disappears).
async function cronReplayRunInner(jobId, runId, fromQueue) {
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(NZ_CONTRACT.API.cron_runs + '/' + encodeURIComponent(runId) + '/replay',
      { method: 'POST', headers, body: JSON.stringify({ job_id: jobId }) });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('重放 run', r.status, raw);
      return;
    }
  } catch (e) {
    showAPIError('重放 run', 0, String(e));
    return;
  }
  if (fromQueue) {
    if (await cronAttentionRefresh()) renderOpenCronTimeline();
  }
  // Refresh the timeline so the new replay run shows up at the head.
  if (cronDrawerState.jobId === jobId) cronTimelineRefreshHeadDebounced(jobId);
}

// cronEscClose — 全局 Esc 的 cron 分支委托入口。
// dashboard.js 的 Global Esc handler 不再裸引用 cron 内部状态（cronExpandedRunId /
// cronDrawerState.jobId），而是经 `window.nzCronEscClose && window.nzCronEscClose()` 守卫调用，由本函数在 cron_view.js
// 内部决定关哪一层。返回 true 表示"消费了 Esc"（调用方据此 preventDefault）。
//
// 这是 dashboard-cron-view-extraction RFC §2.6 B1 的修复：cron 状态搬入 cron_view.js
// 后，留在 dashboard.js 的 handler 跨脚本引用这些符号——一旦 cron_view.js 未加载
// （部署期缓存撕裂）即 `ReferenceError: cronExpandedRunId is not defined`。把决策收进
// cron_view.js，dashboard.js 仅经可选链委托，cron_view.js 缺席时 Esc 优雅降级。
//
// 关闭优先级与原 dashboard.js 一致：行内展开（更靠前的二级状态）先于 drawer。
function cronEscClose() {
  if (cronExpandedRunId && cronExpandedRunId.runId) { cronTimelineCollapse(); return true; }
  if (cronDrawerState.jobId !== null) { closeCronDetail(); return true; }
  return false;
}

// renderCronList repaints only the items container. Called by the filter
// input / chip handlers so typing doesn't rebuild the shell and blow away
// input focus / value. Also called by renderCronPanel after it builds the
// shell on first paint.
function renderCronList() {
  const host = document.getElementById('cron-list-items');
  if (!host) return;
  const filterActive = cronFilterQuery !== '' || cronFilterStatus !== 'all';
  if (!filterActive && cronStore.jobs.length === 0) {
    host.innerHTML =
      '<div class="cron-empty">' +
        '<div class="cron-empty-icon" aria-hidden="true">&#9201;</div>' +
        '<div class="cron-empty-hint">还没有定时任务</div>' +
        '<div class="cron-empty-sub">按计划自动在某个工作目录下运行提示词</div>' +
        '<button type="button" class="cron-empty-cta" data-action="cron-new">创建第一个定时任务</button>' +
      '</div>';
    return;
  }
  const matched = filterCronJobs(cronStore.jobs, cronFilterQuery, cronFilterStatus);
  if (matched.length === 0) {
    host.innerHTML =
      '<div class="cron-filter-empty">' +
        '没有匹配的定时任务' +
        '<div class="cfe-hint">调整关键词或切换状态标签</div>' +
      '</div>';
    return;
  }
  const cmp = cronSortComparators[cronSortOrder] || cronSortComparators.created_desc;
  const sorted = [...matched].sort(cmp);
  // One .cj-list holds the rows (the v3 grouped border is the list's). Rows
  // are reconciled by data-cron-id: a row whose markup is unchanged keeps its
  // node, so a run frame for one job replaces only that row and focus stays.
  let list = host.firstElementChild;
  if (host.childElementCount !== 1 || !list.classList.contains('cj-list')) {
    host.replaceChildren(list = Object.assign(document.createElement('div'), { className: 'cj-list' }));
  }
  reconcileChildren(list, sorted.map(cronJobCardHtml).join(''), el => el.getAttribute('data-cron-id'));
  // Start/stop the 1Hz running-tick driver by whether any row is running.
  ensureCronRunningTick();
}

// onCronSearchInput is the input oninput handler. Reads the live value,
// writes it to module state, then repaints only the items container. Cheap
// and local: typing 50 chars triggers 50 O(N) filter passes on the in-memory
// cronStore.jobs array, no server round-trips.
function onCronSearchInput() {
  const input = document.getElementById('cron-search-input');
  cronFilterQuery = input ? (input.value || '').trim() : '';
  renderCronList();
}

// setCronStatusFilter toggles between the status modes. Re-applies
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

// cronMissedBannerHtml — cron-v2-polish §3.3: missed banner。Count 取自
// cronStore.jobs 本地缓存，与 attention 计数同源。点击切到 attention filter，与 header
// cron-badge 的红点导航保持一致的"点进去看哪些 job 需要关注"语义。
function cronMissedBannerHtml() {
  const missedCount = cronStore.jobs.filter(j => j.missed).length;
  if (missedCount === 0) return '';
  return '<div class="cron-missed-banner" role="alert" data-action="cron-filter" data-status="attention" title="进程重启或休眠期间错过的调度不会自动补跑">' +
      '<span class="cmb-icon">&#9888;</span>' +
      '<span class="cmb-text">有 ' + missedCount + ' 个任务曾错过调度 — 进程重启或休眠空窗期未补跑。点此查看。</span>' +
    '</div>';
}

// cronSummaryChipHtml is the title row's status summary. The two buckets are
// mutually exclusive — a paused / errored / missed job counts as "需关注" and
// is excluded from "运行中" — so the two counts never exceed cronStore.jobs.length.
// It stays hidden: a data-only fallback for tests that grep for
// "运行中 N · 需关注 N"; the overview chip strip is the visible UI.
function cronSummaryChipHtml(attentionCount) {
  const activeCount = cronStore.jobs.filter(j => !j.paused && !j.last_error && !j.missed).length;
  const summaryParts = [];
  if (activeCount > 0) summaryParts.push('运行中 ' + activeCount);
  if (attentionCount > 0) summaryParts.push('<span class="cj-summary-attn">需关注 ' + attentionCount + '</span>');
  return summaryParts.length > 0
    ? '<span class="cj-summary" hidden>· ' + summaryParts.join(' · ') + '</span>'
    : '';
}

// cronFilterBarHtml is the adaptive filter bar. The search row shows only
// when cronStore.jobs > 5 (search adds noise at small scale). The status chips row
// additionally shows whenever something 需关注 exists (the rail badge says
// "需关注 N" — the panel must offer the matching 需关注 chip) or a non-default
// filter is active (the missed-banner sets 'attention'; without chips a
// ≤5-job install had no visible way back to 全部).
function cronFilterBarHtml(attentionCount) {
  const showSearchRow = cronStore.jobs.length > 5;
  if (!showSearchRow && attentionCount === 0 && cronFilterStatus === 'all') return '';
  const chipActive = s => cronFilterStatus === s ? ' active' : '';
  const chipPressed = s => cronFilterStatus === s ? 'true' : 'false';
  // 单一 chip 模板：新增 需关注 chip 的同时不增加 inline onclick 字面量数
  // （CSP ratchet TestDashboardCSP_GeneratedHandlerSurfaceRatchet）。
  const statusChip = (status, label, extraCls) =>
    '<button type="button" class="cron-status-chip' + (extraCls ? ' ' + extraCls : '') + chipActive(status) + '" data-status="' + status + '" aria-pressed="' + chipPressed(status) + '" data-action="cron-filter">' + label + '</button>';
  const attentionChip = (attentionCount > 0 || cronFilterStatus === 'attention')
    ? statusChip('attention', '需关注 ' + attentionCount, 'attention')
    : '';
  const searchRow = showSearchRow
    ? '<div class="cron-search-row">' +
        '<input type="text" id="cron-search-input" class="cron-search-input" placeholder="搜索名称、提示词、目录..." autocomplete="off" spellcheck="false" aria-label="搜索定时任务" value="' + escAttr(cronFilterQuery) + '" data-action-input="cron-search" />' +
        '<button type="button" class="cron-search-clear" data-action="cron-search-clear" title="清空搜索" aria-label="清空搜索">&times;</button>' +
      '</div>'
    : '';
  return '<div class="cron-filter-bar">' +
      searchRow +
      '<div class="cron-status-chips" role="group" aria-label="按状态筛选">' +
        statusChip('all', '全部') +
        statusChip('active', '运行中') +
        attentionChip +
        // cron-v2-polish §3.4 Increment D: 排序 select 放 chips 行末尾
        '<select class="cron-sort-select" aria-label="排序方式" data-action-change="cron-sort">' +
          '<option value="created_desc"' + (cronSortOrder === 'created_desc' ? ' selected' : '') + '>最新创建</option>' +
          '<option value="next_asc"' + (cronSortOrder === 'next_asc' ? ' selected' : '') + '>接下来</option>' +
          '<option value="last_desc"' + (cronSortOrder === 'last_desc' ? ' selected' : '') + '>最近运行</option>' +
          '<option value="title_asc"' + (cronSortOrder === 'title_asc' ? ' selected' : '') + '>按名字</option>' +
        '</select>' +
      '</div>' +
    '</div>';
}

function renderCronPanel() {
  // Guard against an async race: fetchCronJobs().then(renderCronPanel) and the
  // WS run_ended handler fire after the user may have switched away from
  // the cron view. Painting then would be wasted (the container is hidden) or
  // could fight the active view. Only paint when cron is the active view.
  if (ui.activeView !== 'cron') return;
  const main = document.getElementById('cron-main');
  if (!main) return;
  // Shell-preserving repaint: when the cron panel is already mounted (user
  // is just typing in the search box or toggling a chip), we only want to
  // repaint the list + drawer. Rebuilding the shell would wipe the input
  // value and steal focus. Detect by probing for the list host element.
  if (document.getElementById('cron-list-items')) {
    renderCronList();
    renderCronDrawer();
    return;
  }
  const attentionCount = cronStore.jobs.filter(j => j.paused || j.last_error || j.missed).length;
  let html =
    '<div class="cron-detail">' +
      '<div class="cron-detail-body">' +
        '<div class="cron-list-pane" id="cron-list-pane">' +
          '<div class="cron-list-head">' +
            '<div class="cron-list-head-title">' +
              '<button class="btn-mobile-back" data-action="cron-mobile-back" title="返回会话列表" aria-label="返回会话列表">&#8592;</button>' +
              '<h3>定时任务' + cronSummaryChipHtml(attentionCount) + '</h3>' +
            '</div>' +
            '<button type="button" class="cron-new-btn" data-action="cron-new" aria-label="新建定时任务">' +
              '<svg viewBox="0 0 24 24" aria-hidden="true"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>' +
              ' 新建' +
            '</button>' +
          '</div>' +
          cronFilterBarHtml(attentionCount) +
          cronMissedBannerHtml() +
          '<div id="cron-list-items"></div>' +
        '</div>' +
        // cron-panel-consolidation RFC §4.1 / §4.2: the drawer pane is always
        // in the DOM but only shown (`.is-open`) when cronDrawerState.jobId is
        // non-null; renderCronDrawer below fills it.
        '<aside class="cron-detail-pane" id="cron-detail-pane" role="region" aria-label="任务详情"></aside>' +
      '</div>' +
    '</div>';
  main.innerHTML = html;
  // Paint list now that the shell is mounted; subsequent keystrokes / chip
  // flips route through renderCronList directly without touching the shell.
  renderCronList();
  renderCronDrawer();
  // cron-panel-consolidation-ui RFC §2 (Round 2 R-1) — wire the layout
  // observer once the shell is in the DOM. The CSS rules key off
  // `[data-cron-layout]` on `.cron-detail-body`; this writes that
  // attribute based on the *element's actual width*, not the viewport.
  setupCronLayoutObserver();
}

// setupCronLayoutObserver picks the right two-column / single-column
// layout for the cron panel based on the available main-column width
// rather than the viewport width. The previous implementation used a
// single `@media(max-width:720)` rule, which silently broke when a
// 1080p user widened their sidebar past ~360px: the viewport stays at
// 1920 ("wide"), but the main column drops below the 720 cutoff and
// the drawer ends up at <300px wide where the prompt + timeline can't
// share the row.
//
// We tier into 4 modes — wide/medium/narrow/single — keyed off the
// `.cron-detail-body` element width since that's the parent of both
// list-pane and drawer-pane. ResizeObserver fires whenever the user
// drags the sidebar resizer, opens devtools, rotates the device, or
// resizes the window, so the layout always reflects reality.
//
// Idempotent: stores the observer on the body element via a Symbol-
// keyed property so re-mounts (renderCronPanel after fetchCronJobs)
// don't pile up observers. Falls back to a one-shot resize listener
// in browsers without ResizeObserver (none we ship today, but cheap
// insurance).
function setupCronLayoutObserver() {
  const body = document.querySelector('.cron-detail-body');
  if (!body) return;
  const apply = (w) => {
    // The tier is gauged off the body, which is what the observer watches
    // and what the drawer does not resize. The tier sets the list pane's
    // width while a drawer is open (cron.css), so gauging off the list pane
    // fed the tier back into itself and made the width depend on whether the
    // window had been resized with the drawer open.
    let mode;
    if (w >= 1100) mode = 'wide';
    else if (w >= 820) mode = 'medium';
    else if (w >= 560) mode = 'narrow';
    else mode = 'single';
    if (body.dataset.cronLayout !== mode) body.dataset.cronLayout = mode;
  };
  // Initial paint can run before layout settles (especially when the
  // panel is opened from a header click while the user just
  // resized the sidebar). Use offsetWidth which forces a synchronous
  // layout — fine here because we only run on shell mount.
  apply(body.offsetWidth);
  if (typeof ResizeObserver !== 'function') {
    // Fallback — listen on window resize. Less precise (won't catch
    // sidebar drags) but better than a static breakpoint.
    if (!cronLayoutWindowListener) {
      cronLayoutWindowListener = () => {
        const el = document.querySelector('.cron-detail-body');
        if (el) apply(el.offsetWidth);
      };
      window.addEventListener('resize', cronLayoutWindowListener);
    }
    return;
  }
  // Re-binding the observer to a freshly-mounted DOM node is fine —
  // the previous observer's handle goes away when the old DOM does.
  // We still guard via _cronLayoutObs so the observer is idempotent
  // across same-shell repaints.
  if (body._cronLayoutObs) body._cronLayoutObs.disconnect();
  const obs = new ResizeObserver(entries => {
    for (const e of entries) {
      const w = (e.contentBoxSize && e.contentBoxSize[0])
        ? e.contentBoxSize[0].inlineSize
        : e.contentRect.width;
      apply(w);
    }
  });
  obs.observe(body);
  body._cronLayoutObs = obs;
}

async function cronPause(id) {
  // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController + 10s
  // timeout so a NAT-dropped TCP connection no longer hangs the pause
  // button silently. err.status carries the HTTP status for the existing
  // showAPIError surface; thrown errors without err.status are network
  // failures and route to showNetworkError.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON(NZ_CONTRACT.API.cron_pause, { method: 'POST', headers, body: JSON.stringify({ id }) });
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('暂停定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('暂停定时任务', e);
  }
}

async function cronResume(id) {
  // RNEW-UX-003 (#444): see cronPause godoc for fetchJSON migration rationale.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON(NZ_CONTRACT.API.cron_resume, { method: 'POST', headers, body: JSON.stringify({ id }) });
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('恢复定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('恢复定时任务', e);
  }
}

async function cronDelete(id) {
  // Round 2 R-12 (RFC §7.5): destructive flow rewrite.
  // - Statistic in title ("32 次执行记录") so user weighs the loss
  // - Mention JSONL preservation + how to find session_id without
  //   leaking the raw `claude --resume` command at the L1/L2 user
  // - Different copy when the job is currently running (CLI sub-
  //   process won't be killed; that's a surprising behaviour
  //   operators MUST know before confirming)
  // - 3 s countdown via confirmDialog's new countdownSecs option —
  //   long enough to catch fat-finger Enter, short enough not to
  //   annoy
  const job = (Array.isArray(cronStore.jobs) ? cronStore.jobs.find(j => j.id === id) : null) || {};
  const title = job.title || job.user_label || '';
  const runCount = (job.stats && (job.stats.total | 0)) || 0;
  const isRunning = !!(job.current_run);
  const promptPreview = (job.prompt || '').slice(0, 200);

  const headline = title ? '删除「' + title + '」？' : '删除定时任务？';
  let body;
  if (isRunning) {
    const elapsed = job.current_run.started_at
      ? formatRunningElapsed(job.current_run.started_at)
      : '';
    const elapsedHint = elapsed ? '（已运行 ' + elapsed + '）' : '';
    body = '⚠ 该任务正在执行' + elapsedHint + '。\n\n' +
      '删除后任务定义和' + (runCount > 0 ? ' ' + runCount + ' 条 ' : '')
      + '历史记录立即清除，但当前正在跑的这次执行将继续运行直到完成（CLI 子进程不会被强行 kill）。完成结果不会被记录到任何地方。';
  } else if (runCount > 0) {
    body = '此操作将永久删除该任务及其全部 ' + runCount + ' 次执行记录，不可撤销。\n\n' +
      'CLI 的对话历史 JSONL 文件保留在磁盘，需要时可在终端用 claude --resume 复活；session_id 在执行历史的「详情」里能找到。';
  } else {
    body = '此操作将永久删除该任务，不可撤销。该任务尚未执行过，不会有历史会话残留。';
  }

  const ok = await confirmDialog({
    title: headline,
    message: body,
    detail: promptPreview ? promptPreview : ('id: ' + id),
    confirmText: '删除',
    variant: 'danger',
    countdownSecs: 3,
  });
  if (!ok) return;
  // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController + 10s
  // timeout so a NAT-dropped TCP connection no longer hangs the destructive
  // delete flow silently. The button is already disabled by the modal
  // confirm flow above so a deterministic timeout is the right surface.
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON(NZ_CONTRACT.API.cron + '?id=' + encodeURIComponent(id), { method: 'DELETE', headers });
    // R220-FE-2: 释放该 job 在前端持有的 timeline 状态（runs / details / pagination
    // 游标 / fetched 标记），避免 cronTimelineState 累积已删除 job 的内存。
    if (cronTimelineState[id]) delete cronTimelineState[id];
    // cron-panel-consolidation RFC §4.5 state-machine row "G. 删除中":
    // close the drawer if it was showing the just-deleted job. The
    // subsequent renderCronPanel (via fetchCronJobs.then) will repaint
    // the empty drawer — calling closeCronDetail explicitly here keeps
    // the visual feedback synchronous (no flash of the deleted task).
    cronDrawerForgetJob(id);
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    if (e && e.status) { showAPIError('删除定时任务', e.status, (e.message || '').slice(0, 500)); return; }
    showNetworkError('删除定时任务', e);
  }
}

// Edit an existing cron job. Opens a modal pre-populated with the current
// schedule, prompt, and work_dir. The frequency picker tries to restore the
// job's schedule via parseCronToFreq — when it can't (e.g. user wrote a
// custom expression by hand), we surface the raw expression in the advanced
// disclosure so it can still be edited without loss.
function editCronJob(id) {
  // R236-SEC-08 (#494): the poll fetches with ?compact=1 so j.prompt
  // may be truncated to 256 bytes for any job whose full body exceeds
  // that. Refetch the full job before opening the editor — if we
  // skipped this, saving the modal would persist the truncated prompt
  // back to disk, silently destroying the user's data. The refetch
  // helper returns { ok, ... } so we can refuse to open the editor on
  // a fetch failure rather than handing the user a 256-byte preview
  // that Save would happily commit back to disk (R251-FOLLOWUP / #494).
  cronRefetchFullJob(id).then(res => {
    if (!res || !res.ok) {
      const reason = res && res.reason;
      if (reason === 'fetch') {
        showToast('无法获取完整 prompt，编辑暂停以防截断保存。请稍后重试。', 'warning');
      } else {
        showToast('未找到该任务', 'warning');
      }
      return;
    }
    const job = res.job;
    // Sprint 6c: round-trip the saved backend choice. fetchCLIBackends is
    // promise-based; we open the modal once it resolves so the picker (if
    // multi-backend deploy) can pre-select the persisted value. Single-
    // backend deploys collapse the picker — no UI difference for legacy
    // installs.
    fetchCLIBackends().then(backendsData => {
      const backendHtml = renderBackendPicker(backendsData, {
        selectId: 'edit-cron-backend',
        selectedId: job.backend || '',
      });
      openCronEditModal(id, job, backendHtml);
    }).catch(() => openCronEditModal(id, job, ''));
  });
}

function openCronEditModal(id, job, backendHtml) {
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  const notifyInitial = job.notify === true ? 'on' : (job.notify === false ? 'off' : '');
  const hasOverride = !!(job.notify_platform && job.notify_chat_id);
  const notifyHtml = buildCronNotifyToggleHtml(notifyInitial, hasOverride, job.notify_platform, job.notify_chat_id);
  const contextHtml = buildCronContextToggleHtml(!!job.fresh_context);
  const placementHtml = buildCronPlacementHtml(job.placement || '', 'edit-cron-placement');

  // Round-trip attempt: if the saved expression matches a v2 picker shape
  // we pre-fill the picker; otherwise render the default picker and add a
  // "当前频率：<human-readable>" hint above it so the user sees the real
  // schedule (UI shows Daily/09:00 default which is a visual lie without
  // the hint). _cronScheduleTouched stays false so "save without touching
  // the frequency controls" preserves the legacy expression intact.
  // 关联 Issue #1（Round ? review）。
  const initialDesc = parseCronToFreq(job.schedule);
  const scheduleHtml = buildScheduleSection(
    initialDesc || { mode: 'daily', time: '09:00' },
    initialDesc ? '' : (job.schedule || '')
  );

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
        '<button type="button" class="cm-close" data-action="cron-modal-dismiss" aria-label="关闭">✕</button>' +
      '</div>' +
      renderCronModalBody({
        scheduleHtml, wsBody, notifyHtml, contextHtml,
        backendHtml, placementHtml,
        promptId: 'edit-cron-prompt',
        promptPlaceholder: '这个任务要做什么？',
        titleId: 'edit-cron-title',
      }) +
      '<div class="modal-btns">' +
        '<button type="button" data-action="cron-modal-dismiss">取消</button>' +
        '<button type="button" class="primary" data-action="cron-edit-save" data-id="' + escAttr(id) + '">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  cronPlacementBindHint('edit-cron-placement');
  trapFocus(overlay);
  fillCronPrompt('edit-cron-prompt', job.prompt);
  // 回填 title。用 value 属性赋值避开 HTML 特殊字符在模板插值中的风险，
  // 与 fillCronPrompt 的 rationale 一致（参见 renderCronModalBody 注释）。
  const titleEl = document.getElementById('edit-cron-title');
  if (titleEl) titleEl.value = job.title || '';

  overlay.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') overlay.remove();
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      doEditCronJob(id);
    }
  });

  // Seed overlay._cronSchedule with the job's ACTUAL schedule (not whatever
  // the picker would render back from parseCronToFreq). Critical for legacy
  // shapes that don't round-trip: @every 30m / multi-dow weekly / hand-
  // crafted expressions all parseCronToFreq → null → default Daily picker.
  // Without the touched gate, an immediate freqUpdate() would overwrite
  // _cronSchedule to "0 9 * * *" and a "save without changes" click would
  // silently rewrite the user's 30-minute job to every day 9 AM. See
  // freqMarkTouched doc. 不要调 freqUpdate()——让 seed 保持到用户真的
  // 交互频率控件为止。
  //
  // Seed overlay._cronWorkDir to job.work_dir. 之前注释推理为「input 的
  // pre-fill 覆盖未改动场景」是错的：buildCronWorkspaceBodyInternal 仅在
  // selected 命中 projectsData 中某个项目时把 input.value 设为 ''（隐藏
  // custom form 分支），所以「项目命中」+「未触碰任何控件」直接保存会让
  // newWorkDir 解析为 ''，PATCH 把 work_dir 清空、任务回退到 router 默认
  // cwd（用户报告的 bug）。Picker 点选会经 cronSelectWorkspace 覆盖
  // _cronWorkDir；用户点「自定义路径」走 toggleCronWsCustom 也会清掉
  // _cronWorkDir 让 input 接管；要清空 work_dir 的用户需显式切到自定义
  // 路径并清空输入框。
  overlay._cronSchedule = job.schedule || '';
  overlay._cronScheduleTouched = false;
  overlay._cronWorkDir = job.work_dir || '';
}

// buildEditCronWorkspaceBody is the edit-mode counterpart. v2 polish 之后
// 与 create 共享 buildCronWorkspaceBodyInternal，只传不同的 inputId 和
// 已选中的 currentDir 用来回填按钮文本 + 自定义路径输入。
function buildEditCronWorkspaceBody(currentDir) {
  return buildCronWorkspaceBodyInternal({
    inputId: 'edit-cron-workdir',
    buttonId: 'edit-cron-ws-dropdown',
    selectedPath: currentDir || '',
  });
}

async function doEditCronJob(id) {
  const overlay = document.querySelector('.modal-overlay');
  if (!overlay) return;
  const job = cronStore.jobs.find(j => j.id === id);
  if (!job) { showToast('未找到该任务', 'warning'); return; }

  const newPrompt = document.getElementById('edit-cron-prompt')?.value || '';
  const newTitle = (document.getElementById('edit-cron-title')?.value || '').trim();
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
  if (newTitle !== (job.title || '')) body.title = newTitle;
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

  // Sprint 6c: backend pointer semantics — only PATCH when the user picked
  // a different backend than the one stored on the job. Element absent =
  // single-backend deploy (or fetch failed); skip the field entirely so
  // the legacy unset path on the server stays unchanged.
  const backendEl = document.getElementById('edit-cron-backend');
  if (backendEl) {
    const newBackend = backendEl.value || '';
    const origBackend = job.backend || '';
    if (newBackend !== origBackend) body.backend = newBackend;
  }

  // placement（RFC §7.1）：仅在变化时 PATCH；切到云沙箱时按 EFFECTIVE
  // work_dir（本次编辑后的值，否则 job 现值）做前端围栏，与服务端
  // UpdateJob 临界区的原子检查同语义。
  const newPlacement = collectCronPlacementValue('edit-cron-placement');
  if (newPlacement !== null) {
    const origPlacement = job.placement || '';
    if (newPlacement !== origPlacement) {
      if (newPlacement === 'sandbox') {
        const effWorkDir = ('work_dir' in body) ? body.work_dir : (job.work_dir || '');
        if (effWorkDir) { showToast('云沙箱暂不支持工作目录：请先清空"在哪里"', 'warning'); return; }
      }
      body.placement = newPlacement;
    }
  }

  if (Object.keys(body).length === 0) { overlay.remove(); return; }
  if (body.schedule === '') { showToast('频率不能为空', 'warning'); return; }

  try {
    const headers = Object.assign({ 'Content-Type': 'application/json' }, authHeaders());
    const r = await fetch(NZ_CONTRACT.API.cron + '?id=' + encodeURIComponent(id), {
      method: 'PATCH', headers, body: JSON.stringify(body),
    });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('保存定时任务', r.status, raw);
      return;
    }
    overlay.remove();
    showToast('定时任务已更新', 'success');
    fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  } catch (e) {
    showNetworkError('保存定时任务', e);
  }
}

registerShell({ openCronPanel, renderCronPanel });
// Expose the Esc-close delegate so dashboard.js's Global Esc handler can route
// the cron branch here without referencing cron-internal globals across the
// script boundary (dashboard-cron-view-extraction RFC §2.6 B1). The optional
// call (nz.views.cron && …escClose()) degrades gracefully if cron_view.js
// fails to load, instead of throwing `cronExpandedRunId is not defined`.
nzViews.cron = { escClose: cronEscClose };

// §16 inline-expand 回归: ↑↓ 切上一条 / 下一条 run（仅当某行展开时）。
// Moved here from dashboard.js (B1 fix): this handler reads cronExpandedRunId /
// navigateExpandedRun, both file-local to cron_view.js. Keeping the listener in
// the same script as its state means the binding is guaranteed to exist when
// the handler runs — and the shortcut simply never registers if cron_view.js
// is absent, rather than crashing dashboard.js.
// 与 Cmd/Ctrl+Up/Down 的会话切换错开（那个在 dashboard.js，有 metaKey 守卫）。
document.addEventListener('keydown', function(e) {
  if (!cronExpandedRunId || !cronExpandedRunId.runId) return;
  if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  // #2435: the expanded run survives leaving the cron view / opening a modal,
  // so gate on the live view + no overlay, or ↑↓ would preventDefault and
  // silently switch an invisible run. activeView is dashboard.js top-level
  // state (same pattern as renderCronPanel's guard above).
  if (ui.activeView !== 'cron') return;
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable) return;
  e.preventDefault();
  navigateExpandedRun(e.key === 'ArrowUp' ? 'prev' : 'next');
});

// Bootstrap moved from dashboard.js: load initial cron state for the sidebar
// badge. Runs after all cron functions above are defined.
fetchCronJobs();


// ─── data-action registry (#1980, docs/rfc/csp-data-action.md) ─────────────
// Every handler the cron view's generated HTML wires via data-action(-<type>)
// attributes. Parameters ride sibling data-* attributes (escAttr, always
// double-quoted); keys are code literals by contract.
//
// stopPropagation note: the pre-#1980 inline handlers on nested controls
// (schedule chip / run / menu inside a clickable row) used
// event.stopPropagation() to keep the row's open handler from firing. The
// dispatcher's closest()-single-dispatch makes that structural: a click on
// the button resolves to the button's action only, so the calls are dropped.
// Document-level closers (cron menu / dashboard popovers) now see these
// clicks — each has its own provenance guard (menu.contains, closest
// checks), audited per RFC.

// cronIdOf resolves a job id for row-scoped actions: explicit data-id on the
// element (drawer buttons) or the enclosing row's data-cron-id.
function cronIdOf(el) {
  if (el.dataset.id) return el.dataset.id;
  const row = el.closest('[data-cron-id]');
  return row ? row.dataset.cronId : '';
}
// activate() adapts a click action for keyboard activation when the same key
// is also wired as data-action-keydown: Enter/Space activates (with
// preventDefault to stop scrolling/submit), anything else falls through.
const activate = (fn) => (el, e) => {
  if (e.type === 'keydown') {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
  }
  fn(el, e);
};
registerActions({
  'cron-freq-update': () => { freqMarkTouched(); freqUpdate(); },
  'cron-freq-mode': (el) => freqSelectMode(el.value),
  'cron-ws-dropdown': (el, e) => toggleCronWsDropdown(e),
  'cron-ws-select': (el) => cronSelectWorkspace(el, el.dataset.path),
  'cron-ws-custom': () => toggleCronWsCustom(),
  'cron-modal-dismiss': (el) => { const o = el.closest('.modal-overlay'); if (o) o.remove(); },
  'cron-create-save': () => doCreateCronJob(),
  'cron-notify-on': (el) => cronNotifyOnChange(el),
  'cron-notify-override': (el) => cronNotifyOverrideToggle(el),
  'cron-edit': activate((el) => editCronJob(cronIdOf(el))),
  'cron-run-now': (el) => cronTriggerNow(cronIdOf(el)),
  'cron-menu-toggle': (el) => toggleCronMenu(cronIdOf(el)),
  'cron-open': activate((el) => openCronDetail(cronIdOf(el), el)),
  'cron-tl-showall': (el) => cronTimelineToggleShowAll(el),
  'cron-tl-more': (el) => cronTimelineLoadMore(el.dataset.job),
  'cron-att-confirm': (el) => cronAttentionConfirm(el.dataset.run).then((done) => done && renderOpenCronTimeline()),
  'cron-att-replay': (el) => cronAttentionReplay(el.dataset.job, el.dataset.run),
  'cron-tl-select': activate((el, e) => {
    // Guard: clicks inside the expanded .ctr-detail (input snapshot, replay
    // buttons, text selection) must not collapse the row.
    if (e.target instanceof Element && e.target.closest('.ctr-detail')) return;
    cronTimelineSelectRun(el.dataset.job, el.dataset.runId);
  }),
  'cron-tl-jump': (el) => cronTimelineSelectRun(el.dataset.job, el.dataset.run),
  'cron-replay': (el) => cronReplayRun(el.dataset.job, el.dataset.run),
  'cron-new': () => createNewCronJob(),
  'cron-detail-close': () => closeCronDetail(),
  'cron-resume': (el) => cronResume(cronIdOf(el)),
  'cron-pause': (el) => cronPause(cronIdOf(el)),
  'cron-delete': (el) => cronDelete(cronIdOf(el)),
  'cron-edit-save': (el) => doEditCronJob(el.dataset.id),
  'cron-spec-toggle': (el) => cronDrawerSpecPromptToggle(el),
  'cron-filter': (el) => setCronStatusFilter(el.dataset.status),
  'cron-search': () => onCronSearchInput(),
  'cron-search-clear': () => clearCronSearch(),
  'cron-sort': (el) => setCronSortOrder(el.value),
  'cron-mobile-back': () => mobileBack(),
});

// ─── WS run frames and nz.bus subscriptions ────────────────────────────────
// Run frames (#2540) from subsystem cron: the handlers key on job_id, so
// owner_id is projected onto it here, once, at the wire boundary.
const cronRun = (msg) => msg.subsystem === 'cron';
const cronMsgOf = (msg) => Object.assign({}, msg, { job_id: msg.owner_id });
wsm.on(NZ_CONTRACT.WS.run_started, (msg) => cronApplyRunStarted(cronMsgOf(msg)), cronRun);
wsm.on(NZ_CONTRACT.WS.run_ended, (msg) => {
  // Every terminal state lands here; only succeeded celebrates.
  if (msg.state === 'succeeded') announce('定时任务已完成');
  cronApplyRunEnded(cronMsgOf(msg));
  // The refetch hydrates counters / last_error_class over the optimistic
  // patch; the head refresh is rAF-debounced so a burst collapses per paint.
  fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  if (msg.owner_id) cronTimelineRefreshHeadDebounced(msg.owner_id);
}, cronRun);
// A reconnect with no live stream to resume re-checks whether the open drawer
// should start one; cron_live's own onReady (registered first) resumes one.
wsm.onReady(() => { if (!cronLive.jobId) ensureCronLiveSubscription(); });
// dashboard cannot import cron_view (#2557 PR-E1), so it opens the panel over nz.bus.
nzBus.addEventListener('cron:open-panel', () => openCronPanel());
