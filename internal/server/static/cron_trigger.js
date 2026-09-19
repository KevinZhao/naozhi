// cron_trigger.js — the 立即执行 (TriggerNow) flow and its visual-feedback
// cooldown state machine (#2715 D4 follow-up: cron_view.js four-region
// split, region 4).
//
// Owns cronJustTriggered (per-jobId trigger timestamps), the 200ms cooldown
// tick that walks sending → sent → clear, and cronTriggerButtonState — the
// primary button's disable/label matrix the drawer consumes through its
// deps. The WS run_started handler (cron_view.js) clears the cooldown via
// the exported cronTriggerCooldownClear.

import { getToken } from './dashboard.js';
import { showToast } from './nz_util.js';
import { showAPIError, showNetworkError } from './utilities.js';
import { cronDetailJobId, renderCronDrawer } from './cron_drawer.js';

const deps = {
  cronJobs: null, // () => Job[]
  fetchCronJobs: null,
};
export function configureCronTrigger(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('cron_trigger dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// cronTriggerButtonState — the 立即执行 button's disable matrix (see the
// comment block in cronDrawerHtml). Pure; shared by the drawer render and
// cronDrawerRefreshTriggerBtn so the 200 ms cooldown tick can patch the
// button in place instead of rebuilding the whole drawer.
function cronTriggerButtonState(j) {
  const id = j.id || '';
  const isPaused = !!j.paused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  const cooldown = cronTriggerCooldownState(id);
  const st = { disabled: false, label: '\u25B7 立即执行', tooltip: '立即执行一次', cls: 'cda-btn primary' };
  if (isPaused) {
    st.disabled = true;
    st.tooltip = '已暂停。请先恢复任务。';
  } else if (isRunning) {
    st.disabled = true;
    st.label = '\u25B7 运行中…';
    st.tooltip = '上一次执行尚未完成，请等待结束。';
    st.cls += ' is-running';
  } else if (cooldown) {
    st.disabled = true;
    st.label = '\u25B7 ' + cooldown.label;
    st.cls += cooldown.phase === 'sending' ? ' is-sending' : ' is-sent';
    st.tooltip = '刚已触发一次，请稍候。';
  }
  return st;
}

// cronDrawerRefreshTriggerBtn — targeted repaint of the drawer's primary
// action button(s) for the open job. Called from the cooldown tick every
// 200 ms; touching only the button keeps text selection, <details> open
// state and scroll position inside the drawer intact (a full
// renderCronDrawer here used to wipe all three for 10 s after 立即执行).
function cronDrawerRefreshTriggerBtn() {
  if (cronDetailJobId === null) return;
  const job = (deps.cronJobs() || []).find(x => x && x.id === cronDetailJobId);
  if (!job) return;
  const btns = document.querySelectorAll('#cron-detail-pane .cron-drawer-actions .cda-btn.primary');
  if (!btns.length) return;
  const st = cronTriggerButtonState(job);
  for (const btn of btns) {
    if (btn.className !== st.cls) btn.className = st.cls;
    if (btn.textContent !== st.label) btn.textContent = st.label;
    if (btn.title !== st.tooltip) btn.title = st.tooltip;
    if (btn.disabled !== st.disabled) {
      btn.disabled = st.disabled;
      if (st.disabled) btn.setAttribute('aria-disabled', 'true');
      else btn.removeAttribute('aria-disabled');
    }
  }
}

// cronJustTriggered tracks the per-jobId trigger timestamp (ms).
// Used by cronTriggerCooldownState() to compute disable + label state for
// both the drawer's primary action and the list row's ghost Run button so
// they stay in sync. Cleared by cronTriggerCooldownClear() on WS
// cron_run_started (preferred) or after the 10 s floor elapses.
const cronJustTriggered = Object.create(null);
const CRON_TRIGGER_COOLDOWN_MS = 10 * 1000;

function cronTriggerCooldownState(id) {
  const t = cronJustTriggered[id];
  if (!t) return null;
  const dt = Date.now() - t;
  if (dt < 0 || dt >= CRON_TRIGGER_COOLDOWN_MS) {
    delete cronJustTriggered[id];
    return null;
  }
  // 0..1000 ms → spinner; 1000..3000 ms → ✓; 3000..10000 ms → quiet hold.
  if (dt < 1000) return { phase: 'sending', label: '触发中…' };
  if (dt < 3000) return { phase: 'sent',    label: '已派发 ✓' };
  return { phase: 'cooldown', label: '已派发 ✓' };
}

function cronTriggerCooldownClear(id) {
  if (cronJustTriggered[id]) delete cronJustTriggered[id];
}

// cronTriggerCooldownTickTimer drives label transitions (sending → sent →
// cooldown) and final unlock. Runs at 200 ms while any job is in cooldown
// to make the spinner→✓ transition feel snappy without burning CPU when
// the button is idle.
let cronTriggerCooldownTickTimer = null;
function ensureCronTriggerCooldownTick() {
  const anyHot = Object.keys(cronJustTriggered).length > 0;
  if (anyHot && !cronTriggerCooldownTickTimer) {
    cronTriggerCooldownTickTimer = setInterval(() => {
      // Sweep expired entries; the in-place button patch below then lets
      // the drawer's trigger button leave cooldown.
      const now = Date.now();
      for (const k of Object.keys(cronJustTriggered)) {
        if (now - cronJustTriggered[k] >= CRON_TRIGGER_COOLDOWN_MS) {
          delete cronJustTriggered[k];
        }
      }
      // Patch only the drawer's trigger button so the spinner→✓→idle
      // transitions happen even if no other event fires, without rebuilding
      // the drawer (which wiped text selection / <details> state every 200 ms).
      cronDrawerRefreshTriggerBtn();
      if (Object.keys(cronJustTriggered).length === 0) {
        clearInterval(cronTriggerCooldownTickTimer);
        cronTriggerCooldownTickTimer = null;
      }
    }, 200);
  }
}

async function cronTriggerNow(id) {
  // Reentrancy guard: if a cooldown is already in flight for this id, drop
  // the click silently — the disabled button state should have prevented it
  // already, but keyboard activation paths (Enter on a non-disabled button
  // in the same paint window) can still slip through.
  if (cronJustTriggered[id]) return;
  cronJustTriggered[id] = Date.now();
  ensureCronTriggerCooldownTick();
  // Repaint drawer immediately so the button flips to the spinner without
  // waiting for the 200 ms tick. List ghost Run isn't repainted per row
  // (would be expensive on 500-job dashboards) — its disabled-after-trigger
  // state is read at render time.
  if (cronDetailJobId === id) renderCronDrawer();
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(NZ_CONTRACT.API.cron_trigger, { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) {
      // Failure — clear the cooldown immediately so the user can retry.
      // The 10 s floor would be punishing on a transient 502.
      cronTriggerCooldownClear(id);
      if (cronDetailJobId === id) renderCronDrawer();
      const raw = await r.text().catch(() => '');
      showAPIError('立即执行定时任务', r.status, raw);
      return;
    }
    // Success — leave cooldown in place; the tick timer will transition
    // the label and finally clear it. deps.cronJobs() row state will be updated
    // by the WS cron_run_started event (which also clears the cooldown
    // via the dispatch handler — see ws msg case below).
    showToast('已派发执行', 'success', 1500);
  } catch (e) {
    cronTriggerCooldownClear(id);
    if (cronDetailJobId === id) renderCronDrawer();
    showNetworkError('立即执行定时任务', e);
  }
}


export {
  cronDrawerRefreshTriggerBtn,
  cronTriggerButtonState,
  cronTriggerCooldownClear,
  cronTriggerNow,
};
