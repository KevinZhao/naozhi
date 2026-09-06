// self_update.js — the dashboard's self-update chip (#2558 D4-2).
//
// Verbatim move out of dashboard.js (`git diff --color-moved` shows a pure
// move; only the import/wiring lines are new). The chip owns its own poll
// timer + apply state machine and exposes nothing back to dashboard — the
// chip self-registers its DOMContentLoaded bootstrap.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back (that cycle puts dashboard's own consts in TDZ). The two dashboard
// helpers this chip calls are injected once via configureSelfUpdate().
import { showToast } from './nz_util.js';

const deps = {
  confirmDialog: null,
  markSessionOptimisticRunning: null,
};
export function configureSelfUpdate(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('self_update dep missing: ' + k);
    deps[k] = impl[k];
  }
}

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
// updateApplying is set the moment an apply is accepted (202) and is what keeps
// the chip in its busy state across the gap where the server's `phase` has not
// caught up yet — or where the process is already dying and the next poll never
// answers. Without it the chip would flick back to "restart to apply" for the
// last second of its own life.
let updateApplying = false;
let updateApplyTimer = null;

// UPDATE_APPLY_MAX_MS bounds how long the local flag may outlive the server's
// own account of what is happening. It exists because several apply outcomes
// deliberately leave `phase` untouched: a release lookup that fails writes only
// check_error (selfupdate.Status.noteCheck), and ErrNothingToDo /
// ErrInstallInProgress write nothing at all. In those cases action stays
// 'install', phase stays 'available', and none of the terminal conditions below
// ever fire — so without a deadline the chip would sit on "正在应用新版本" for
// the rest of the page's life, at the 3s busy cadence, while nothing was being
// applied. Same shape as the safety timer in deps.markSessionOptimisticRunning.
//
// Generous on purpose: a slow GitHub lookup happens BEFORE doInstall writes
// PhaseInstalling, so this window has to comfortably contain it. Expiring early
// costs little (the chip returns to the server's truth and a second click is
// safe — TryLock plus the staged short-circuits make a repeat apply a no-op),
// expiring late leaves a stale-but-honest "in flight" label.
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
    // Fall back to whatever the server says rather than guessing an outcome —
    // and no toast: the common reason we are here is an apply that never got
    // off the ground, which the re-read will describe accurately (check_error /
    // last_error land in the chip's detail).
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
    // Silent: a failed version poll must never produce a toast. It is
    // background information and the chip simply keeps its previous state.
    // During a restart this is the EXPECTED path — the server we are polling is
    // being replaced — so updateApplying is deliberately left set, bounded by
    // the UPDATE_APPLY_MAX_MS deadline rather than by this poll.
  }
}

// updateChipView maps a status payload to the chip's presentation. Pure, so a
// contract test can cover every branch without a DOM.
//
// `applying` (our local flag) outranks `phase` on purpose: it is true in the
// window where we know an apply was accepted but the server has not said so
// yet, and it stays true when the server stops answering at all because it is
// restarting.
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
  // need different ones: telling someone to re-run `naozhi upgrade` when the
  // binary is already staged is what makes them overwrite the backup.
  //
  // The command itself comes from the server (`manual_command`): it depends on
  // the SERVER's OS and its real launchd label, neither of which this browser
  // can know — the browser's platform APIs describe the operator's laptop, not
  // the node being upgraded. Empty means there is nothing to paste (a staged
  // binary with no managed service: blocked_reason already says to restart by
  // hand).
  if (st.manual_command) {
    lines.push('');
    lines.push(st.action === 'restart' ? '手动生效：' : '手动升级：');
    lines.push('  ' + st.manual_command);
  }
  return lines.join('\n');
}

// updateApplyPrompt builds the confirmation copy. Pure and exported to the
// contract test because the two branches must stay distinguishable: the whole
// point of the feature is that the operator is told which of "download this" and
// "restart to apply what is already downloaded" they are agreeing to.
//
// "本节点" is load bearing (RFC NG2): in a multi-node deployment this upgrades
// only the process serving this dashboard, and copy that said "服务" alone would
// read as all of them.
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
    // Writable install dir but nothing we can restart (naozhi started by hand,
    // not by systemd/launchd). Installing still helps — it is what `naozhi
    // upgrade` would do — but promising a restart we cannot perform would leave
    // the operator believing the new version is live when it is not.
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
    await deps.confirmDialog({
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
  const ok = await deps.confirmDialog({
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

