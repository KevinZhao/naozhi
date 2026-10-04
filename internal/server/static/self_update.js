// self_update.js — the dashboard's self-update chip and its version-skew
// reload. It owns its poll timer and apply state machine, exposes nothing and
// self-registers its bootstrap. Like every module dashboard.js imports, it
// must not import dashboard.js back (the cycle puts dashboard's consts in TDZ).
import { NZ_CONTRACT } from './contract.js';
import { showToast } from './nz_util.js';
import { sid } from './session_ident.js';
import { composer, perSession, selection, sessionList, ui } from './state.js';
import { confirmDialog } from './utilities.js';
import { wsm } from './ws_manager.js';

// --- Self-Update Chip ---
//
// Surfaces GET /api/system/update in the sidebar header. See
// docs/rfc/dashboard-update-notice.md.
//
// The one thing worth knowing before touching this: "a newer version exists"
// and "a newer version is already on disk" are DIFFERENT states needing
// opposite actions, and the server tells us which via `action` — we never
// compare version strings here. Under the default update mode the steady state
// is `restart` (the background checker downloads within seconds of finding a
// release), so that is the common path, not the rare one.
//
// Clicking it applies the update after a confirmation — install+restart, or a
// restart alone when the bytes are already staged. When the deployment cannot
// apply it itself (no write permission, no managed service, or
// update.dashboard_install=false) the click explains what to run instead.

let updateState = null;
let updateTimer = null;
// updateApplying is set the moment an apply is accepted (202) and keeps the
// chip busy until the server's `phase` catches up, or while the dying process
// no longer answers the poll.
let updateApplying = false;
let updateApplyTimer = null;

// UPDATE_APPLY_MAX_MS bounds how long the local flag may outlive the server's
// account. Several apply outcomes leave `phase` untouched (a failed release
// lookup writes only check_error; ErrNothingToDo / ErrInstallInProgress write
// nothing), so no terminal condition below fires and, without a deadline, the
// chip would say "正在应用新版本" for the page's life. Generous because a slow
// GitHub lookup precedes PhaseInstalling; expiring early is cheap (a repeat
// apply is a no-op), expiring late leaves a stale-but-honest label.
const UPDATE_APPLY_MAX_MS = 90000;

// UPDATE_POLL_MS is deliberately slow. Version state changes on a 6h server
// cadence, so a minute of staleness is invisible, and this must not become
// another per-second poll. UPDATE_POLL_BUSY_MS applies only while an apply is
// in flight, when the operator IS watching.
const UPDATE_POLL_MS = 60000;
const UPDATE_POLL_BUSY_MS = 3000;
let updatePollMs = UPDATE_POLL_MS;

function setUpdatePoll(ms) {
  if (updateTimer && ms === updatePollMs) return;
  updatePollMs = ms;
  if (updateTimer) clearInterval(updateTimer);
  updateTimer = setInterval(fetchUpdateStatus, ms);
}

// setUpdateApplying is the only writer of updateApplying, so the deadline can
// never be left running by a path that cleared the flag some other way.
function setUpdateApplying(on) {
  updateApplying = on;
  if (updateApplyTimer) {
    clearTimeout(updateApplyTimer);
    updateApplyTimer = null;
  }
  if (!on) return;
  updateApplyTimer = setTimeout(() => {
    updateApplyTimer = null;
    updateApplying = false;
    // Fall back to the server's account, no toast: the usual cause is an apply
    // that never started, which check_error / last_error in the detail explain.
    renderUpdateChip();
    fetchUpdateStatus();
  }, UPDATE_APPLY_MAX_MS);
}

async function fetchUpdateStatus() {
  try {
    const r = await fetch(NZ_CONTRACT.API.system_update);
    if (!r.ok) return;
    updateState = await r.json();
    // An apply has landed somewhere terminal — succeeded (nothing left to do),
    // failed, or installed-but-not-restarted. Either way the server's own state
    // is now more accurate than our local "in flight" flag.
    if (updateApplying && (updateState.action === 'none' ||
        updateState.phase === 'failed' || updateState.phase === 'staged')) {
      setUpdateApplying(false);
    }
    const busy = updateApplying ||
      updateState.phase === 'installing' || updateState.phase === 'restarting';
    setUpdatePoll(busy ? UPDATE_POLL_BUSY_MS : UPDATE_POLL_MS);
    renderUpdateChip();
  } catch (e) {
    // Silent: background information, the chip keeps its state. During a
    // restart this is the expected path, so updateApplying stays set, bounded
    // by UPDATE_APPLY_MAX_MS rather than by this poll.
  }
}

// updateChipView maps a status payload to the chip's presentation. Pure, so a
// contract test can cover every branch without a DOM. `applying` outranks
// `phase`: it covers the window before the server reports the accepted apply,
// and the restart, when the server does not answer at all.
function updateChipView(st, applying) {
  if (!st || !st.action || st.action === 'none') return { show: false };
  const target = st.staged || st.latest || '';
  if (applying) {
    return { show: true, busy: true, tag: target, title: '正在应用新版本，服务即将重启…' };
  }
  if (st.phase === 'installing') {
    return { show: true, busy: true, tag: target, title: '正在下载新版本…' };
  }
  if (st.phase === 'restarting') {
    return { show: true, busy: true, tag: target, title: '正在重启以应用新版本…' };
  }
  if (st.phase === 'failed') {
    return { show: true, failed: true, tag: target, title: '上次升级失败，点击查看原因' };
  }
  if (st.action === 'restart') {
    // The staged case: bytes are already on disk, verified. A restart is the
    // only remaining step — the icon is a restart glyph, not a download one.
    return { show: true, restart: true, tag: target, title: target + ' 已就绪，点击重启生效' };
  }
  return { show: true, tag: target, title: '有新版本 ' + target + ' 可安装' };
}

// updateCanApply reports whether the chip should offer to do the work rather
// than explain it. Both flags matter: can_apply is the deployment's ability
// (writable path / manageable service), install_enabled is its permission
// (update.dashboard_install).
function updateCanApply(st) {
  return !!(st && st.action && st.action !== 'none' && st.can_apply && st.install_enabled);
}

function renderUpdateChip() {
  const btn = document.getElementById('btn-update');
  if (!btn) return;
  const view = updateChipView(updateState, updateApplying);
  if (!view.show) {
    btn.hidden = true;
    return;
  }
  btn.hidden = false;
  btn.classList.toggle('is-busy', !!view.busy);
  btn.classList.toggle('is-failed', !!view.failed);
  btn.title = view.title;
  btn.setAttribute('aria-label', view.title);
  const tag = document.getElementById('update-tag');
  if (tag) tag.textContent = view.tag;
  // Swap the glyph: an up-arrow reads as "fetch this", a circular arrow as
  // "restart to apply". Getting these backwards would tell the operator to
  // expect a download when none is needed.
  const svg = btn.querySelector('svg');
  if (svg) {
    svg.innerHTML = view.restart
      ? '<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/>'
      : '<path d="M12 19V5"/><path d="M5 12l7-7 7 7"/>';
  }
}

// updateChipDetail builds the explanatory body shown when the chip is clicked.
// Pure for the same reason as updateChipView.
function updateChipDetail(st) {
  const lines = [];
  lines.push('当前版本：' + (st.current || '未知'));
  if (st.action === 'restart') {
    lines.push('已就绪：' + (st.staged || '') + '（已下载并校验，落盘完成）');
    lines.push('');
    lines.push('只需重启本节点服务即可生效，无需再次下载。');
  } else if (st.action === 'install') {
    lines.push('最新版本：' + (st.latest || ''));
    lines.push('');
    lines.push('需要下载并安装后重启本节点服务。');
  }
  if (st.running_sessions > 0) {
    // Stated as reassurance, not warning: shim keeps the CLI subprocesses
    // alive across a naozhi restart, so in-flight conversations survive.
    lines.push('当前有 ' + st.running_sessions + ' 个会话正在运行；CLI 子进程由 shim 保持存活，对话不会中断。');
  }
  if (!st.can_apply && st.blocked_reason) {
    lines.push('');
    lines.push('⚠️ ' + st.blocked_reason);
  }
  if (st.last_error) {
    lines.push('');
    lines.push('上次错误：' + st.last_error);
  }
  if (st.check_error) {
    lines.push('');
    lines.push('版本检查失败：' + st.check_error);
  }
  if (st.install_enabled === false) {
    lines.push('');
    lines.push('⚠️ 本实例已关闭 dashboard 一键升级（update.dashboard_install: false）。');
  }
  if (updateCanApply(st)) {
    // We are about to offer to do it, so what belongs here is the way OUT:
    // if the new build does not come up, the dashboard carrying this advice is
    // the thing that is gone.
    if (st.rollback_hint) {
      lines.push('');
      lines.push('若新版本无法启动，回滚：');
      lines.push('  ' + st.rollback_hint);
    }
    return lines.join('\n');
  }
  // Cannot apply from here — hand over the exact command. Restart and install
  // need different ones: re-running `naozhi upgrade` over a staged binary
  // overwrites the backup. The server supplies it (`manual_command`) because it
  // depends on the server's OS and launchd label, not this browser's. Empty
  // means nothing to paste (blocked_reason already says to restart by hand).
  if (st.manual_command) {
    lines.push('');
    lines.push(st.action === 'restart' ? '手动生效：' : '手动升级：');
    lines.push('  ' + st.manual_command);
  }
  return lines.join('\n');
}

// updateApplyPrompt builds the confirmation copy. Its branches must stay
// distinguishable: the operator agrees either to "download this" or to "restart
// to apply what is already downloaded". "本节点" is load bearing (RFC NG2): only
// the process serving this dashboard upgrades, not every node.
function updateApplyPrompt(st) {
  const isRestart = st.action === 'restart';
  if (isRestart) {
    return {
      title: '重启以应用新版本',
      message: (st.staged || '') + ' 已下载并校验完成，重启本节点服务即可生效（无需再次下载）',
      confirmText: '立即重启生效',
    };
  }
  if (st.restart_supported === false) {
    // Writable install dir but no managed service to restart. Installing still
    // helps (it is what `naozhi upgrade` does); promising a restart would not.
    return {
      title: '下载并安装新版本',
      message: '将下载并校验 ' + (st.latest || '') + ' 并替换 binary；本节点未检测到受管服务，安装后需手动重启进程才会生效',
      confirmText: '仅下载安装',
    };
  }
  return {
    title: '下载并安装新版本',
    message: '将下载并校验 ' + (st.latest || '') + '，替换 binary 后重启本节点服务',
    confirmText: '立即安装并重启',
  };
}

async function onUpdateChipClick() {
  if (!updateState) return;
  const st = updateState;
  // Busy: an apply is already in flight. Re-confirming would either 409 or, in
  // the staged state, be the repeat install that destroys the backup.
  if (updateApplying || st.phase === 'installing' || st.phase === 'restarting') {
    showToast('升级正在进行中…');
    return;
  }
  if (!updateCanApply(st)) {
    const titleMap = { restart: '新版本已就绪', install: '有新版本可用' };
    await confirmDialog({
      title: titleMap[st.action] || '版本状态',
      message: st.action === 'restart'
        ? (st.staged || '') + ' 已下载完成，重启后生效'
        : '最新版本 ' + (st.latest || '') + ' 可安装',
      detail: updateChipDetail(st),
      confirmText: '知道了',
      cancelText: '关闭',
      variant: 'primary',
    });
    return;
  }
  const prompt = updateApplyPrompt(st);
  const ok = await confirmDialog({
    title: prompt.title,
    message: prompt.message,
    detail: updateChipDetail(st),
    // Restarting the gateway is disruptive enough to warrant the danger
    // treatment plus a speed bump, even though sessions survive it (F10).
    confirmText: st.phase === 'failed' ? '重试' : prompt.confirmText,
    cancelText: '取消',
    variant: 'danger',
    countdownSecs: 3,
  });
  if (!ok) return;
  await applyUpdate(st.action);
}

// applyUpdate POSTs the apply. confirm_action echoes back the action we showed
// the operator: if the background checker changed the situation between render
// and click, the server rejects with 409 rather than doing something else.
async function applyUpdate(action) {
  const btn = document.getElementById('btn-update');
  if (btn) btn.classList.add('is-busy');
  try {
    const headers = { 'Content-Type': 'application/json' };
    const r = await fetch(NZ_CONTRACT.API.system_update_apply, {
      method: 'POST', headers, credentials: 'same-origin',
      body: JSON.stringify({ confirm_action: action }),
    });
    if (r.status === 202) {
      setUpdateApplying(true);
      renderUpdateChip();
      setUpdatePoll(UPDATE_POLL_BUSY_MS);
      showToast(action === 'restart' ? '正在重启以应用新版本…' : '正在下载并安装新版本…');
      return;
    }
    const raw = (await r.text().catch(() => '')).trim();
    showToast('升级未启动：' + (raw.slice(0, 200) || ('HTTP ' + r.status)));
    // A 409 means our view of the state was stale — re-read it immediately so
    // the chip stops offering the operation that was just refused.
    fetchUpdateStatus();
  } catch (e) {
    showToast('升级请求失败：' + (e && e.message ? e.message : e));
  } finally {
    // On the 202 path renderUpdateChip has already set the busy class from
    // state; this only clears the optimistic one on the error paths.
    if (btn && !updateApplying) btn.classList.remove('is-busy');
  }
}

function initUpdateChip() {
  const btn = document.getElementById('btn-update');
  if (!btn) return;
  btn.addEventListener('click', onUpdateChipClick);
  fetchUpdateStatus();
  setUpdatePoll(UPDATE_POLL_MS);
}

document.addEventListener('DOMContentLoaded', initUpdateChip);

// --- Asset-version skew ---
//
// nz-asset-version names the assets this page booted with, auth_ok those served
// now. On a difference a banner with no close control offers the reload and an
// idle tab reloads itself. Sessions survive it, so idle guards only unsent input
// (memory-only drafts of other sessions too), an open surface and a watched turn.
const SKEW_IDLE_MS = 60000;
// SKEW_BUSY matches every surface a reload would close under the operator.
const SKEW_BUSY = '.modal-overlay, .cmd-palette-overlay, .lightbox-overlay.active, .voice-overlay.show, ' +
  '#fv-drawer.fv-open, #aside-drawer.visible, #cron-detail-pane.is-open';
const skew = { server: '', lastInputAt: 0 };

function skewIdle() {
  const texts = [...document.querySelectorAll('#msg-input, textarea')].map((el) => el.value ?? el.innerText);
  if (texts.concat(Object.values(perSession.drafts)).some((t) => (t || '').trim()) || composer.sending) return false;
  if (composer.pendingFiles.length || ui.activePopover || document.querySelector(SKEW_BUSY)) return false;
  if (document.hidden) return true;
  const sd = sessionList.sessionsData[sid(selection.key, selection.node)];
  return Date.now() - skew.lastInputAt >= SKEW_IDLE_MS && !(sd && sd.state === 'running');
}

// maybeSkewReload reloads at most once per server version, so a page still
// skewed after it (two builds behind one address) keeps the banner, not a loop.
function maybeSkewReload() {
  if (!skew.server || !skewIdle()) return;
  try {
    if (sessionStorage.getItem('nz-asset-reload') === skew.server) return;
    sessionStorage.setItem('nz-asset-reload', skew.server);
  } catch (e) { return; }
  location.reload();
}

wsm.onReady((msg) => {
  const meta = document.querySelector('meta[name="nz-asset-version"]');
  if (!meta || !msg.asset_version || msg.asset_version === meta.content) return;
  if (!skew.server) {
    skew.lastInputAt = Date.now();
    const bar = document.getElementById('asset-skew-banner');
    bar.hidden = false;
    bar.addEventListener('click', () => location.reload());
    const touched = () => { skew.lastInputAt = Date.now(); };
    for (const t of ['keydown', 'pointerdown', 'wheel']) document.addEventListener(t, touched, { capture: true, passive: true });
    document.addEventListener('visibilitychange', maybeSkewReload);
    setInterval(maybeSkewReload, 30000);
  }
  skew.server = msg.asset_version;
  maybeSkewReload();
});
